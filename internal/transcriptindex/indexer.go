package transcriptindex

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Indexer owns refresh decisions and projection mapping. It intentionally has no
// scheduling, logging, SQL, filesystem, or runtime-specific responsibilities.
type Indexer struct {
	catalog    Catalog
	repository Repository
	limits     Limits
	now        func() time.Time
}

func New(catalog Catalog, repository Repository) *Indexer {
	return &Indexer{
		catalog:    catalog,
		repository: repository,
		limits:     DefaultLimits(),
		now:        time.Now,
	}
}

func (i *Indexer) Plan(ctx context.Context, request RefreshRequest) (RefreshPlan, error) {
	if i == nil || i.catalog == nil || i.repository == nil {
		return RefreshPlan{}, NewLimitedError(Limitation{Kind: LimitationRepositoryUnavailable},
			errors.New("transcript index dependencies are unavailable"))
	}
	if request.Mode == "" {
		request.Mode = RefreshModeIncremental
	}
	if request.Mode != RefreshModeIncremental && request.Mode != RefreshModeForce {
		return RefreshPlan{}, NewLimitedError(Limitation{Kind: LimitationInvalidProjection},
			errors.New("invalid transcript refresh mode"))
	}
	limits := i.limits.normalized()
	discovery, err := i.catalog.Discover(ctx, limits)
	if err != nil {
		return RefreshPlan{}, wrapLimitedError(err, LimitationDiscoveryIncomplete, SessionKey{})
	}
	if discovery.AsOf.IsZero() {
		discovery.AsOf = i.now().UTC()
	}
	if !discovery.Complete && len(discovery.Limitations) == 0 {
		discovery.Limitations = append(discovery.Limitations,
			Limitation{Kind: LimitationDiscoveryIncomplete})
	}
	discovery = validatedDiscovery(discovery)
	states, err := i.repository.ProjectionStates(ctx)
	if err != nil {
		return RefreshPlan{}, wrapLimitedError(err, LimitationRepositoryUnavailable, SessionKey{})
	}
	if states == nil {
		states = map[SessionKey]ProjectionState{}
	}

	sessions := append([]SourceSession(nil), discovery.Sessions...)
	sort.SliceStable(sessions, func(left, right int) bool {
		if sessions[left].Modified.Equal(sessions[right].Modified) {
			if sessions[left].Key.Runtime == sessions[right].Key.Runtime {
				return sessions[left].Key.SessionID < sessions[right].Key.SessionID
			}
			return sessions[left].Key.Runtime < sessions[right].Key.Runtime
		}
		return sessions[left].Modified.After(sessions[right].Modified)
	})

	planned := make([]SourceSession, 0, len(sessions))
	skipped := 0
	for _, session := range sessions {
		if discoveryBlocksSession(discovery.Limitations, session.Key) {
			continue
		}
		state, exists := states[session.Key]
		if request.Mode != RefreshModeForce && exists && state.Generation == session.Generation &&
			state.Limitation == nil {
			skipped++
			continue
		}
		planned = append(planned, session)
	}
	remaining := 0
	if request.MaxSessions > 0 && len(planned) > request.MaxSessions {
		remaining = len(planned) - request.MaxSessions
		planned = planned[:request.MaxSessions]
	}

	coverage := buildCoverage(discovery, states, nil)
	return RefreshPlan{
		Discovery: discovery,
		Sessions:  planned,
		States:    copyProjectionStates(states),
		Coverage:  coverage,
		Stats: RefreshStats{
			Discovered: len(sessions),
			Planned:    len(planned),
			Skipped:    skipped,
			Remaining:  remaining,
		},
	}, nil
}

func (i *Indexer) Refresh(ctx context.Context, request RefreshRequest) (RefreshResult, error) {
	plan, err := i.Plan(ctx, request)
	if err != nil {
		return RefreshResult{Coverage: unavailableCoverage(indexerNow(i), err)}, err
	}
	limits := i.limits.normalized()
	states := copyProjectionStates(plan.States)
	limitations := make([]Limitation, 0)
	stats := plan.Stats
	recordFailure := func(session SourceSession, limitation Limitation) {
		stats.Failed++
		if ctx.Err() != nil {
			limitations = append(limitations, limitation)
			return
		}
		state, recordErr := i.repository.RecordTranscriptProjectionLimitation(
			ctx, session.Key, plan.States[session.Key], i.now().UTC(), limitation)
		if recordErr != nil {
			limitations = append(limitations, limitation)
			limitations = append(limitations,
				limitationForError(recordErr, LimitationRepositoryUnavailable, session.Key))
			return
		}
		if state.Key == (SessionKey{}) {
			state.Key = session.Key
		}
		// The repository owns optimistic concurrency. A stale failed attempt may
		// return a newer successful state unchanged; never overwrite that truth with
		// the limitation we merely attempted to record.
		states[session.Key] = state
		if state.Limitation != nil {
			limitations = append(limitations, *state.Limitation)
		}
	}

	for sessionIndex, session := range plan.Sessions {
		if err := ctx.Err(); err != nil {
			limitation := limitationForError(err, LimitationReadCancelled, session.Key)
			recordFailure(session, limitation)
			stats.Remaining += len(plan.Sessions) - sessionIndex - 1
			break
		}

		snapshot, readErr := i.catalog.Read(ctx, session, limits)
		if readErr != nil {
			recordFailure(session,
				limitationForError(readErr, LimitationSourceUnreadable, session.Key))
			continue
		}
		if limitation, invalid := validateSnapshotForSession(session, snapshot); invalid {
			recordFailure(session, limitation)
			continue
		}
		plannedState := plan.States[session.Key]
		replacement, mapErr := buildReplacement(snapshot, limits, i.now().UTC(),
			plannedState.Generation, plannedState.IndexedAt)
		if mapErr != nil {
			recordFailure(session,
				limitationForError(mapErr, LimitationInvalidProjection, session.Key))
			continue
		}
		generation, generationErr := i.catalog.Generation(ctx, session, limits)
		if generationErr != nil {
			recordFailure(session,
				limitationForError(generationErr, LimitationSourceUnreadable, session.Key))
			continue
		}
		if generation != snapshot.Generation {
			recordFailure(session, Limitation{Kind: LimitationSourceMutated,
				Runtime: session.Key.Runtime, SessionID: session.Key.SessionID})
			continue
		}

		replaced, replaceErr := i.repository.ReplaceTranscriptProjection(ctx, replacement)
		if replaceErr != nil {
			limitations = append(limitations,
				limitationForError(replaceErr, LimitationRepositoryUnavailable, session.Key))
			stats.Failed++
			continue
		}
		state := replaced.State
		if state.Key != session.Key || state.Generation == "" || state.IndexedAt.IsZero() {
			limitations = append(limitations, Limitation{Kind: LimitationInvalidProjection,
				Runtime: session.Key.Runtime, SessionID: session.Key.SessionID})
			stats.Failed++
			continue
		}
		state.Limitation = nil
		states[session.Key] = state
		if replaced.Replaced {
			stats.Updated++
		} else {
			stats.Skipped++
		}
	}

	// Absence is trustworthy only after complete discovery and a fully admitted,
	// limitation-free pass. Otherwise old-good orphan candidates stay untouched.
	if plan.Discovery.Complete && len(plan.Discovery.Limitations) == 0 &&
		stats.Remaining == 0 && len(limitations) == 0 {
		keys := make([]SessionKey, 0, len(plan.Discovery.Sessions))
		for _, session := range plan.Discovery.Sessions {
			keys = append(keys, session.Key)
		}
		removed, removeErr := i.repository.RemoveTranscriptOrphans(ctx, keys)
		if removeErr != nil {
			limitations = append(limitations,
				limitationForError(removeErr, LimitationRepositoryUnavailable, SessionKey{}))
			stats.Failed++
		} else {
			stats.Orphaned = removed
		}
	}

	coverage := buildCoverage(plan.Discovery, states, limitations)
	return RefreshResult{Coverage: coverage, Stats: stats}, nil
}

func validatedDiscovery(discovery Discovery) Discovery {
	counts := make(map[SessionKey]int, len(discovery.Sessions))
	for _, session := range discovery.Sessions {
		counts[session.Key]++
	}
	validated := make([]SourceSession, 0, len(discovery.Sessions))
	for _, session := range discovery.Sessions {
		invalid := session.Key.Runtime == "" || session.Key.SessionID == "" ||
			session.Generation == "" || len(session.Segments) == 0 || counts[session.Key] != 1
		if !invalid {
			validated = append(validated, session)
			continue
		}
		discovery.Complete = false
		discovery.Limitations = append(discovery.Limitations, Limitation{
			Kind: LimitationInvalidProjection, Runtime: session.Key.Runtime,
			SessionID: session.Key.SessionID,
		})
	}
	discovery.Sessions = validated
	discovery.Limitations = mergeLimitations(discovery.Limitations)
	return discovery
}

func discoveryBlocksSession(limitations []Limitation, key SessionKey) bool {
	for _, limitation := range limitations {
		if limitation.Runtime != "" && limitation.Runtime != key.Runtime {
			continue
		}
		if limitation.SessionID != "" && limitation.SessionID != key.SessionID {
			continue
		}
		return true
	}
	return false
}

func validateSnapshotForSession(session SourceSession, snapshot SourceSnapshot) (Limitation, bool) {
	if snapshot.Session.Key != session.Key {
		return Limitation{Kind: LimitationInvalidProjection, Runtime: session.Key.Runtime,
			SessionID: session.Key.SessionID}, true
	}
	if snapshot.Generation != session.Generation || snapshot.Session.Generation != session.Generation {
		return Limitation{Kind: LimitationSourceMutated, Runtime: session.Key.Runtime,
			SessionID: session.Key.SessionID}, true
	}
	if snapshot.SourceBytes != snapshot.Session.SourceBytes {
		return Limitation{Kind: LimitationSourceMutated, Runtime: session.Key.Runtime,
			SessionID: session.Key.SessionID}, true
	}
	return Limitation{}, false
}

func buildReplacement(snapshot SourceSnapshot, limits Limits, indexedAt time.Time,
	expectedGeneration string, expectedIndexedAt time.Time) (ProjectionReplacement, error) {
	key := snapshot.Session.Key
	if key.Runtime == "" || key.SessionID == "" || snapshot.Generation == "" ||
		snapshot.Session.Generation == "" || snapshot.Generation != snapshot.Session.Generation {
		return ProjectionReplacement{}, NewLimitedError(Limitation{
			Kind: LimitationInvalidProjection, Runtime: key.Runtime, SessionID: key.SessionID}, nil)
	}
	if snapshot.SourceBytes < 0 || snapshot.SourceBytes > limits.MaxSourceBytes {
		return ProjectionReplacement{}, NewLimitedError(Limitation{
			Kind: LimitationSourceTooLarge, Runtime: key.Runtime, SessionID: key.SessionID,
			ObservedBytes: snapshot.SourceBytes, LimitBytes: limits.MaxSourceBytes}, nil)
	}

	documents := make([]SearchDocument, 0, minInt(len(snapshot.Events)+1, limits.MaxDocuments+1))
	textBytes := int64(0)
	appendDocument := func(document SearchDocument) error {
		document.Text = sanitizeSearchText(document.Text)
		document.Order = len(documents)
		if len(documents)+1 > limits.MaxDocuments {
			return NewLimitedError(Limitation{Kind: LimitationDocumentLimit,
				Runtime: key.Runtime, SessionID: key.SessionID,
				ObservedCount: len(documents) + 1, LimitCount: limits.MaxDocuments}, nil)
		}
		candidateBytes := textBytes + int64(len(document.Text))
		if candidateBytes > limits.MaxTextBytes {
			return NewLimitedError(Limitation{Kind: LimitationTextLimit,
				Runtime: key.Runtime, SessionID: key.SessionID,
				ObservedBytes: candidateBytes, LimitBytes: limits.MaxTextBytes}, nil)
		}
		documents = append(documents, document)
		textBytes = candidateBytes
		return nil
	}

	if err := appendDocument(SearchDocument{
		Timestamp: snapshot.Session.Modified.UTC().Format(time.RFC3339Nano),
		Kind:      "title",
		Text:      snapshot.Session.Title,
		Lineage:   "title:" + key.Runtime + ":" + key.SessionID,
	}); err != nil {
		return ProjectionReplacement{}, err
	}
	for _, event := range snapshot.Events {
		text, keep := searchEventText(event)
		if !keep {
			continue
		}
		if err := appendDocument(SearchDocument{
			Timestamp: event.Timestamp,
			Kind:      event.Kind,
			Text:      text,
			Lineage:   event.Lineage,
		}); err != nil {
			return ProjectionReplacement{}, err
		}
	}

	return ProjectionReplacement{
		Session: SessionMetadata{
			Key: key, CatalogID: snapshot.Session.CatalogID, ResumeID: snapshot.Session.ResumeID,
			Path: snapshot.Session.Path, CWD: snapshot.Session.CWD,
			Project: snapshot.Session.Project, Title: sanitizeSearchText(snapshot.Session.Title),
			Modified: snapshot.Session.Modified, Turns: snapshot.Session.Turns,
		},
		ExpectedGeneration: expectedGeneration,
		ExpectedIndexedAt:  expectedIndexedAt,
		Generation:         snapshot.Generation,
		IndexedAt:          indexedAt,
		SourceCount:        len(snapshot.Session.Segments),
		SourceBytes:        snapshot.SourceBytes,
		TextBytes:          textBytes,
		Documents:          documents,
	}, nil
}

func searchEventText(event SourceEvent) (string, bool) {
	var text string
	switch event.Kind {
	case "tool_call":
		text = strings.TrimSpace(event.Name + " " + truncateSearchText(event.Text, 200))
	case "tool_result":
		text = truncateSearchText(event.Text, 200)
	case "user", "assistant":
		text = event.Text
	default:
		return "", false
	}
	text = sanitizeSearchText(text)
	return text, text != ""
}

func sanitizeSearchText(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
}

func truncateSearchText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + "…"
}

func buildCoverage(discovery Discovery, states map[SessionKey]ProjectionState,
	attemptLimitations []Limitation) Coverage {
	coverage := Coverage{
		State:              CoverageCatchingUp,
		DiscoveredSessions: len(discovery.Sessions),
		CoverageAsOf:       discovery.AsOf,
		Limitations:        mergeLimitations(discovery.Limitations, attemptLimitations),
		Sessions:           make([]SessionCoverage, 0, len(discovery.Sessions)),
	}
	bySession := make(map[SessionKey][]Limitation)
	for _, source := range discovery.Sessions {
		for _, limitation := range coverage.Limitations {
			if limitation.Runtime != "" && limitation.Runtime != source.Key.Runtime {
				continue
			}
			if limitation.SessionID != "" && limitation.SessionID != source.Key.SessionID {
				continue
			}
			bySession[source.Key] = append(bySession[source.Key], limitation)
		}
	}

	hasStale := false
	for _, source := range discovery.Sessions {
		state, exists := states[source.Key]
		session := SessionCoverage{
			Key: source.Key, SourceGeneration: source.Generation, Modified: source.Modified,
			Limitations: append([]Limitation(nil), bySession[source.Key]...),
		}
		if exists {
			session.IndexedGeneration = state.Generation
			session.IndexedAt = state.IndexedAt
			if state.IndexedAt.After(coverage.LastSuccess) {
				coverage.LastSuccess = state.IndexedAt
			}
			if state.Limitation != nil {
				session.Limitations = mergeLimitations(session.Limitations, []Limitation{*state.Limitation})
			}
		}
		switch {
		case len(session.Limitations) > 0:
			session.State = CoverageIncomplete
		case exists && state.Generation == source.Generation:
			session.State = CoverageCurrent
			coverage.IndexedSessions++
		case exists && state.Generation != "":
			session.State = CoverageStale
			hasStale = true
		default:
			session.State = CoverageCatchingUp
		}
		if session.State != CoverageCurrent && source.Modified.After(coverage.NewestUnindexedModified) {
			coverage.NewestUnindexedModified = source.Modified
		}
		coverage.Sessions = append(coverage.Sessions, session)
	}

	switch {
	case !discovery.Complete || len(coverage.Limitations) > 0:
		coverage.State = CoverageIncomplete
	case coverage.IndexedSessions == coverage.DiscoveredSessions:
		coverage.State = CoverageCurrent
	case hasStale:
		coverage.State = CoverageStale
	default:
		coverage.State = CoverageCatchingUp
	}
	return coverage
}

// ForSession returns the same semantic coverage model narrowed to one canonical session.
// If discovery did not contain that session, the global non-current evidence is retained.
func (c Coverage) ForSession(key SessionKey) Coverage {
	for _, session := range c.Sessions {
		if session.Key != key {
			continue
		}
		out := Coverage{
			State:              session.State,
			DiscoveredSessions: 1,
			CoverageAsOf:       c.CoverageAsOf,
			LastSuccess:        session.IndexedAt,
			Limitations:        append([]Limitation(nil), session.Limitations...),
			Sessions:           []SessionCoverage{session},
		}
		if session.State == CoverageCurrent {
			out.IndexedSessions = 1
		} else {
			out.NewestUnindexedModified = session.Modified
		}
		return out
	}
	return Coverage{
		State:                   c.State,
		CoverageAsOf:            c.CoverageAsOf,
		LastSuccess:             c.LastSuccess,
		NewestUnindexedModified: c.NewestUnindexedModified,
		Limitations:             append([]Limitation(nil), c.Limitations...),
	}
}

func mergeLimitations(groups ...[]Limitation) []Limitation {
	seen := map[Limitation]struct{}{}
	var out []Limitation
	for _, group := range groups {
		for _, limitation := range group {
			if _, exists := seen[limitation]; exists {
				continue
			}
			seen[limitation] = struct{}{}
			out = append(out, limitation)
		}
	}
	sort.SliceStable(out, func(left, right int) bool {
		if out[left].Runtime != out[right].Runtime {
			return out[left].Runtime < out[right].Runtime
		}
		if out[left].SessionID != out[right].SessionID {
			return out[left].SessionID < out[right].SessionID
		}
		return out[left].Kind < out[right].Kind
	})
	return out
}

func limitationForError(err error, fallback LimitationKind, key SessionKey) Limitation {
	if limitation, ok := LimitationFromError(err); ok {
		if limitation.Runtime == "" {
			limitation.Runtime = key.Runtime
		}
		if limitation.SessionID == "" {
			limitation.SessionID = key.SessionID
		}
		return limitation
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		fallback = LimitationReadCancelled
	}
	return Limitation{Kind: fallback, Runtime: key.Runtime, SessionID: key.SessionID}
}

func wrapLimitedError(err error, fallback LimitationKind, key SessionKey) error {
	if _, ok := LimitationFromError(err); ok {
		return err
	}
	return NewLimitedError(limitationForError(err, fallback, key), err)
}

func unavailableCoverage(now func() time.Time, err error) Coverage {
	limitation := limitationForError(err, LimitationRepositoryUnavailable, SessionKey{})
	return Coverage{
		State:        CoverageUnavailable,
		CoverageAsOf: now().UTC(),
		Limitations:  []Limitation{limitation},
	}
}

func indexerNow(indexer *Indexer) func() time.Time {
	if indexer != nil && indexer.now != nil {
		return indexer.now
	}
	return time.Now
}

func copyProjectionStates(states map[SessionKey]ProjectionState) map[SessionKey]ProjectionState {
	copy := make(map[SessionKey]ProjectionState, len(states))
	for key, state := range states {
		copy[key] = state
	}
	return copy
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// ProjectLabel preserves the established compact session-index project mapping. It is
// owned with projection metadata mapping so indexed and direct CLI rows cannot drift.
func ProjectLabel(cwd string) string {
	if cwd == "" {
		return "-"
	}
	parts := strings.Split(strings.TrimRight(cwd, "/"), "/")
	return parts[len(parts)-1]
}
