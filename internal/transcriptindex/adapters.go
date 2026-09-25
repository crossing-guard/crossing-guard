package transcriptindex

import (
	"context"
	"errors"
	"sort"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// HarvestCatalog is the only production adapter from registered runtime projection
// capabilities to the application Catalog port. Runtime support is discovered through
// the optional interface; adding a capable runtime requires no branch here.
type HarvestCatalog struct {
	runtimes []harvest.Runtime
	sources  map[string]harvest.TranscriptProjectionSource
}

func NewHarvestCatalog(runtimes []harvest.Runtime) *HarvestCatalog {
	catalog := &HarvestCatalog{
		runtimes: append([]harvest.Runtime(nil), runtimes...),
		sources:  make(map[string]harvest.TranscriptProjectionSource, len(runtimes)),
	}
	sort.Slice(catalog.runtimes, func(i, j int) bool {
		return catalog.runtimes[i].Name() < catalog.runtimes[j].Name()
	})
	for _, runtime := range catalog.runtimes {
		if source, ok := runtime.(harvest.TranscriptProjectionSource); ok {
			catalog.sources[runtime.Name()] = source
		}
	}
	return catalog
}

func NewRegisteredHarvestCatalog() *HarvestCatalog {
	return NewHarvestCatalog(harvest.Runtimes())
}

func (c *HarvestCatalog) Discover(ctx context.Context, limits Limits) (Discovery, error) {
	out := Discovery{Sessions: []SourceSession{}, Complete: true, AsOf: time.Now().UTC()}
	if c == nil {
		return out, NewLimitedError(Limitation{Kind: LimitationDiscoveryIncomplete},
			errors.New("harvest catalog is unavailable"))
	}
	readLimits := harvest.ProjectionReadLimits{MaxSourceBytes: limits.normalized().MaxSourceBytes}
	for _, runtime := range c.runtimes {
		if err := ctx.Err(); err != nil {
			return out, harvestAdapterError(err, LimitationReadCancelled, SessionKey{})
		}
		source := c.sources[runtime.Name()]
		if source == nil {
			out.Complete = false
			out.Limitations = append(out.Limitations, Limitation{
				Kind: LimitationUnsupportedAdapter, Runtime: runtime.Name(),
			})
			continue
		}
		discovery, err := source.DiscoverTranscriptProjections(ctx, readLimits)
		if err != nil {
			if ctx.Err() != nil {
				return out, harvestAdapterError(err, LimitationReadCancelled,
					SessionKey{Runtime: runtime.Name()})
			}
			out.Complete = false
			limitation := harvestLimitation(err, LimitationSourceUnreadable,
				SessionKey{Runtime: runtime.Name()})
			out.Limitations = append(out.Limitations, limitation)
			continue
		}
		if !discovery.Complete {
			out.Complete = false
		}
		if len(discovery.Limitations) > 0 {
			out.Complete = false
		}
		for _, limitation := range discovery.Limitations {
			out.Limitations = append(out.Limitations, mapHarvestLimitation(limitation,
				SessionKey{Runtime: runtime.Name()}))
		}
		for _, session := range discovery.Sessions {
			out.Sessions = append(out.Sessions, sourceSessionFromHarvest(session))
		}
	}
	return out, nil
}

func (c *HarvestCatalog) Read(ctx context.Context, session SourceSession,
	limits Limits) (SourceSnapshot, error) {
	native, source, err := c.nativeSession(session)
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot, err := source.ReadTranscriptProjection(ctx, native,
		harvest.ProjectionReadLimits{MaxSourceBytes: limits.normalized().MaxSourceBytes})
	if err != nil {
		return SourceSnapshot{}, harvestAdapterError(err, LimitationSourceUnreadable, session.Key)
	}
	out := SourceSnapshot{
		Session: sourceSessionFromHarvest(snapshot.Session), Generation: snapshot.Generation,
		SourceBytes: snapshot.SourceBytes, Events: make([]SourceEvent, 0, len(snapshot.Events)),
	}
	for _, event := range snapshot.Events {
		out.Events = append(out.Events, SourceEvent{
			Ordinal: event.Ordinal, Lineage: event.Lineage, SegmentID: event.SegmentID,
			Timestamp: event.Event.Ts, Kind: event.Event.Kind, Name: event.Event.Name,
			Text: event.Event.Text,
		})
	}
	return out, nil
}

func (c *HarvestCatalog) Generation(ctx context.Context, session SourceSession,
	limits Limits) (string, error) {
	native, source, err := c.nativeSession(session)
	if err != nil {
		return "", err
	}
	generation, err := source.TranscriptProjectionGeneration(ctx, native,
		harvest.ProjectionReadLimits{MaxSourceBytes: limits.normalized().MaxSourceBytes})
	if err != nil {
		return "", harvestAdapterError(err, LimitationSourceUnreadable, session.Key)
	}
	return generation, nil
}

func (c *HarvestCatalog) nativeSession(session SourceSession) (harvest.ProjectionSession,
	harvest.TranscriptProjectionSource, error) {
	if c == nil {
		return harvest.ProjectionSession{}, nil, NewLimitedError(Limitation{
			Kind: LimitationUnsupportedAdapter, Runtime: session.Key.Runtime,
			SessionID: session.Key.SessionID}, errors.New("harvest catalog is unavailable"))
	}
	source := c.sources[session.Key.Runtime]
	if source == nil {
		return harvest.ProjectionSession{}, nil, NewLimitedError(Limitation{
			Kind: LimitationUnsupportedAdapter, Runtime: session.Key.Runtime,
			SessionID: session.Key.SessionID}, errors.New("runtime projection adapter is unavailable"))
	}
	native, ok := session.catalogToken.(harvest.ProjectionSession)
	if !ok || native.Runtime != session.Key.Runtime || native.ID != session.Key.SessionID ||
		native.Generation != session.Generation {
		return harvest.ProjectionSession{}, nil, NewLimitedError(Limitation{
			Kind: LimitationInvalidProjection, Runtime: session.Key.Runtime,
			SessionID: session.Key.SessionID}, errors.New("catalog session token is invalid"))
	}
	return native, source, nil
}

func sourceSessionFromHarvest(session harvest.ProjectionSession) SourceSession {
	segments := make([]SourceSegment, 0, len(session.Segments))
	for _, segment := range session.Segments {
		segments = append(segments, SourceSegment{
			ID: segment.ID, SourceRef: segment.SourceRef, UpdateMarker: segment.UpdateMarker,
			Modified: segment.Modified, SourceBytes: segment.SourceBytes,
		})
	}
	path := session.Summary.Path
	if path == "" && len(session.Segments) > 0 {
		path = session.Segments[0].SourceRef
	}
	return SourceSession{
		Key:       SessionKey{Runtime: session.Runtime, SessionID: session.ID},
		CatalogID: session.Summary.ID, ResumeID: session.Summary.ResumeID,
		Title: session.Summary.Title, TitleSource: session.Summary.TitleSource,
		Path: path, CWD: session.Summary.Cwd, Project: ProjectLabel(session.Summary.Cwd),
		Modified: session.Modified, Turns: session.Summary.UserTurns,
		SourceBytes: session.SourceBytes, Generation: session.Generation,
		Segments: segments, catalogToken: session,
	}
}

func harvestAdapterError(err error, fallback LimitationKind, key SessionKey) error {
	return NewLimitedError(harvestLimitation(err, fallback, key), err)
}

func harvestLimitation(err error, fallback LimitationKind, key SessionKey) Limitation {
	var projectionErr *harvest.ProjectionError
	if errors.As(err, &projectionErr) {
		return mapHarvestLimitation(projectionErr.Limitation, key)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		fallback = LimitationReadCancelled
	}
	return Limitation{Kind: fallback, Runtime: key.Runtime, SessionID: key.SessionID}
}

func mapHarvestLimitation(source harvest.ProjectionLimitation, fallback SessionKey) Limitation {
	kinds := map[harvest.ProjectionLimitationKind]LimitationKind{
		harvest.ProjectionSourceTooLarge:   LimitationSourceTooLarge,
		harvest.ProjectionSourceUnreadable: LimitationSourceUnreadable,
		harvest.ProjectionSourceMutated:    LimitationSourceMutated,
		harvest.ProjectionReadCancelled:    LimitationReadCancelled,
	}
	kind := kinds[source.Kind]
	if kind == "" {
		kind = LimitationSourceUnreadable
	}
	runtime, sessionID := source.Runtime, source.SessionID
	if runtime == "" {
		runtime = fallback.Runtime
	}
	if sessionID == "" {
		sessionID = fallback.SessionID
	}
	return Limitation{Kind: kind, Runtime: runtime, SessionID: sessionID,
		ObservedBytes: source.ObservedBytes, LimitBytes: source.LimitBytes}
}

// StoreRepository is the only production adapter from store.Index to Repository.
// SQL and transaction boundaries remain entirely inside store.
type StoreRepository struct {
	index *store.Index
}

func NewStoreRepository(index *store.Index) *StoreRepository {
	return &StoreRepository{index: index}
}

func (r *StoreRepository) ProjectionStates(ctx context.Context) (map[SessionKey]ProjectionState, error) {
	if err := r.ready(ctx, SessionKey{}); err != nil {
		return nil, err
	}
	states, err := r.index.TranscriptProjectionStates()
	if err != nil {
		return nil, repositoryAdapterError(err, SessionKey{})
	}
	out := make(map[SessionKey]ProjectionState, len(states))
	for _, state := range states {
		mapped := projectionStateFromStore(state)
		out[mapped.Key] = mapped
	}
	return out, nil
}

func (r *StoreRepository) RecordTranscriptProjectionLimitation(ctx context.Context, key SessionKey,
	expected ProjectionState, attemptedAt time.Time, limitation Limitation) (ProjectionState, error) {
	if err := r.ready(ctx, key); err != nil {
		return ProjectionState{}, err
	}
	state, err := r.index.RecordTranscriptProjectionLimitation(
		store.TranscriptProjectionKey{Runtime: key.Runtime, SessionID: key.SessionID},
		expected.Generation, expected.IndexedAt, attemptedAt, limitationToStore(limitation))
	if err != nil {
		return ProjectionState{}, repositoryAdapterError(err, key)
	}
	return projectionStateFromStore(state), nil
}

func (r *StoreRepository) ReplaceTranscriptProjection(ctx context.Context,
	replacement ProjectionReplacement) (ReplaceResult, error) {
	key := replacement.Session.Key
	if err := r.ready(ctx, key); err != nil {
		return ReplaceResult{}, err
	}
	documents := make([]store.SearchDocument, 0, len(replacement.Documents))
	for _, document := range replacement.Documents {
		documents = append(documents, store.SearchDocument{
			Order: document.Order, Timestamp: document.Timestamp, Kind: document.Kind,
			Text: document.Text, Lineage: document.Lineage,
		})
	}
	result, err := r.index.ReplaceTranscriptProjection(store.TranscriptProjection{
		Session: store.SessionRow{Vendor: key.Runtime, ID: key.SessionID,
			CatalogID: replacement.Session.CatalogID, ResumeID: replacement.Session.ResumeID,
			Path: replacement.Session.Path, CWD: replacement.Session.CWD,
			Project: replacement.Session.Project, Title: replacement.Session.Title,
			Modified: replacement.Session.Modified.Unix(), Turns: replacement.Session.Turns},
		ExpectedGeneration: replacement.ExpectedGeneration,
		ExpectedIndexedAt:  replacement.ExpectedIndexedAt,
		Generation:         replacement.Generation, IndexedAt: replacement.IndexedAt,
		SourceCount: replacement.SourceCount, Documents: documents,
	})
	if err != nil {
		return ReplaceResult{}, repositoryAdapterError(err, key)
	}
	return ReplaceResult{State: projectionStateFromStore(result.State), Replaced: result.Replaced}, nil
}

func (r *StoreRepository) RemoveTranscriptOrphans(ctx context.Context, keep []SessionKey) (int, error) {
	if err := r.ready(ctx, SessionKey{}); err != nil {
		return 0, err
	}
	keys := make([]store.TranscriptProjectionKey, 0, len(keep))
	for _, key := range keep {
		keys = append(keys, store.TranscriptProjectionKey{Runtime: key.Runtime, SessionID: key.SessionID})
	}
	removed, err := r.index.RemoveTranscriptProjectionOrphans(keys)
	if err != nil {
		return 0, repositoryAdapterError(err, SessionKey{})
	}
	return removed, nil
}

func (r *StoreRepository) ready(ctx context.Context, key SessionKey) error {
	if err := ctx.Err(); err != nil {
		return NewLimitedError(Limitation{Kind: LimitationReadCancelled,
			Runtime: key.Runtime, SessionID: key.SessionID}, err)
	}
	if r == nil || r.index == nil {
		return NewLimitedError(Limitation{Kind: LimitationRepositoryUnavailable,
			Runtime: key.Runtime, SessionID: key.SessionID}, errors.New("store repository is unavailable"))
	}
	return nil
}

func repositoryAdapterError(err error, key SessionKey) error {
	kind := LimitationRepositoryUnavailable
	switch {
	case store.IsBusyError(err):
		kind = LimitationRepositoryBusy
	case errors.Is(err, store.ErrTranscriptProjectionChanged):
		kind = LimitationSourceMutated
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		kind = LimitationReadCancelled
	}
	return NewLimitedError(Limitation{Kind: kind, Runtime: key.Runtime,
		SessionID: key.SessionID}, err)
}

func projectionStateFromStore(state store.TranscriptProjectionState) ProjectionState {
	out := ProjectionState{
		Key:        SessionKey{Runtime: state.Key.Runtime, SessionID: state.Key.SessionID},
		Generation: state.Generation, IndexedAt: state.IndexedAt,
		SourceCount: state.SourceCount, DocumentCount: state.DocumentCount,
		LastAttemptedAt: state.LastAttemptedAt,
	}
	if state.Limitation != nil {
		limitation := limitationFromStore(*state.Limitation)
		out.Limitation = &limitation
	}
	return out
}

func limitationToStore(limitation Limitation) store.TranscriptProjectionLimitation {
	return store.TranscriptProjectionLimitation{
		Kind: string(limitation.Kind), Runtime: limitation.Runtime,
		SessionID: limitation.SessionID, ObservedBytes: limitation.ObservedBytes,
		LimitBytes: limitation.LimitBytes, ObservedCount: limitation.ObservedCount,
		LimitCount: limitation.LimitCount,
	}
}

func limitationFromStore(limitation store.TranscriptProjectionLimitation) Limitation {
	return Limitation{
		Kind: LimitationKind(limitation.Kind), Runtime: limitation.Runtime,
		SessionID: limitation.SessionID, ObservedBytes: limitation.ObservedBytes,
		LimitBytes: limitation.LimitBytes, ObservedCount: limitation.ObservedCount,
		LimitCount: limitation.LimitCount,
	}
}
