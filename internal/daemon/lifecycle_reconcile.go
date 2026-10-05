package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/collectionconfig"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

const (
	lifecycleReconcileSessionLimit  = 25
	lifecycleLiveResultWindow       = time.Minute
	lifecycleTranscriptActionSchema = "transcript-action-v1"
)

// transcriptGenerationDigest identifies a transcript's generation by its first
// bytes. The digest lives in harvest so the usage reader shares it
// (token-usage-analytics plan §3.4); this caller's window is unchanged.
func transcriptGenerationDigest(path string) (string, error) {
	return harvest.SourcePrefixDigest(path, harvest.SourcePrefixWindow)
}

func lifecycleParserIssues(sessionID, runtime, sourceRef, sourceSegment string,
	batch harvest.LifecycleBatch, observedAt int64) []store.CollectionIssue {
	issues := make([]store.CollectionIssue, 0, len(batch.OversizedRecords)+len(batch.EvictedCalls))
	for _, oversized := range batch.OversizedRecords {
		detail := fmt.Sprintf("oversized-record:%d:%s", oversized.RecordStart,
			oversized.SourceGeneration)
		issues = append(issues, store.CollectionIssue{IssueID: oversized.IssueID,
			SessionID: sessionID, Runtime: runtime, SourceRef: sourceRef,
			SourceSegmentID: sourceSegment, CollectorID: "harvest-lifecycle", Kind: "malformed",
			AffectedCount: 1, FirstSeen: observedAt, LastSeen: observedAt,
			DetailDigest: observation.DigestBytes([]byte(detail))})
	}
	for _, evicted := range batch.EvictedCalls {
		issues = append(issues, store.CollectionIssue{IssueID: evicted.OccurrenceID,
			SessionID: sessionID, Runtime: runtime, NativeCallID: evicted.NativeCallID,
			SourceRef: evicted.SourceRef, SourceSegmentID: evicted.SourceSegmentID,
			CollectorID: "harvest-lifecycle", Kind: "parser-state-evicted", AffectedCount: 1,
			FirstSeen: observedAt, LastSeen: observedAt,
			DetailDigest: observation.DigestBytes([]byte(evicted.Reason))})
	}
	return issues
}

func lifecycleCursorIssue(sessionID, runtime, sourceRef, sourceSegment, generation string,
	kind, occurrence string, affectedCount int, observedAt int64) store.CollectionIssue {
	if affectedCount < 1 {
		affectedCount = 1
	}
	identity := strings.Join([]string{runtime, sourceRef, sourceSegment, generation, occurrence}, "\x00")
	return store.CollectionIssue{
		IssueID: observationIssueID(kind, identity), SessionID: sessionID, Runtime: runtime,
		ObservationID: sourceSegment, SourceRef: sourceRef, SourceSegmentID: sourceSegment,
		CollectorID: "harvest-lifecycle", Kind: kind, AffectedCount: affectedCount,
		FirstSeen: observedAt, LastSeen: observedAt,
		DetailDigest: observation.DigestBytes([]byte(occurrence)),
	}
}

func lifecycleEffectToStore(effect harvest.LifecycleEffect, mode collectionconfig.Mode, retained *int) store.ResultEffect {
	beforeBytes, afterBytes := effect.BeforeBytes, effect.AfterBytes
	if beforeBytes == 0 {
		beforeBytes = len(effect.ReplacementBefore)
	}
	if afterBytes == 0 {
		afterBytes = len(effect.ReplacementAfter)
	}
	out := store.ResultEffect{Ordinal: effect.Ordinal, RawIdentity: effect.RawIdentity,
		Operation: effect.Operation, MoveTarget: effect.MoveTarget, EvidenceSource: effect.EvidenceSource,
		SourceField: effect.SourceField, Completeness: effect.Completeness, ReplaceAll: effect.ReplaceAll,
		ReplacementBeforeBytes: beforeBytes, ReplacementBeforeDigest: effect.BeforeDigest,
		ReplacementAfterBytes: afterBytes, ReplacementAfterDigest: effect.AfterDigest,
		ContentBytes: effect.ContentBytes, ContentDigest: effect.ContentDigest,
		DiffBytes: effect.DiffBytes, DiffDigest: effect.DiffDigest, DiffCompleteness: effect.DiffCompleteness}
	for _, body := range []struct {
		value []byte
		set   func([]byte)
	}{
		{effect.ReplacementBefore, func(v []byte) { out.ReplacementBeforePayload = v }},
		{effect.ReplacementAfter, func(v []byte) { out.ReplacementAfterPayload = v }},
		{effect.ContentPayload, func(v []byte) { out.ContentPayload = v }},
		{effect.DiffPayload, func(v []byte) { out.DiffPayload = v }},
	} {
		if len(body.value) == 0 {
			continue
		}
		if mode == collectionconfig.MetadataOnly || *retained+len(body.value) > observation.MaxRetainedInput {
			out.Completeness = "metadata-only"
			if out.DiffBytes > 0 {
				out.DiffCompleteness = "metadata-only"
			}
			continue
		}
		copyBody := append([]byte(nil), body.value...)
		body.set(copyBody)
		*retained += len(copyBody)
	}
	return out
}

func appendLifecycleBatch(g *Governor, sessionID, cwd string, batch harvest.LifecycleBatch) error {
	for _, action := range batch.Actions {
		payload, completeness := action.InputPayload, action.Completeness
		if len(payload) > observation.MaxRetainedInput {
			payload, completeness = nil, "metadata-only"
		}
		observedAt, _ := parseEventTime(action.ObservedAt)
		result, err := g.ObserveV1(Observation{SessionID: sessionID, Runtime: action.Runtime,
			Tool: action.Tool, Content: string(payload), Cwd: cwd, TS: observedAt,
			Origin: "transcript"}, ObservationEvidence{
			Delivery: store.EventDelivery{ObservationID: action.ObservationID,
				ObservationSchema: lifecycleTranscriptActionSchema, EnvelopeDigest: action.SourceDigest,
				CollectorID: "harvest-lifecycle", CollectorVersion: "1",
				NativeCallID: action.NativeCallID, NativeCallKind: action.NativeCallKind,
				ReceivedAt: time.Now().Unix(), DeliveryAttempts: 1, DeliveryMode: "transcript"},
			Input: store.EventInput{MediaType: action.MediaType, RawBytes: action.RawBytes,
				CapturedBytes: len(payload), Digest: action.InputDigest, Completeness: completeness,
				Payload: payload, SourceRef: action.SourceRef + "#" + action.SourceSequence}})
		if err != nil {
			return err
		}
		g.writeMu.Lock()
		err = g.ix.ReconcileResultsForEvent(result.EventID, time.Now().Unix())
		if err == nil {
			var exact []string
			exact, err = g.ix.ExactResultObservationIDsForEvent(result.EventID)
			if err == nil {
				for _, observationID := range exact {
					if err = g.ix.ResolveCollectionIssue(observationIssueID("result-unjoined",
						observationID), time.Now().Unix()); err != nil {
						break
					}
				}
			}
		}
		g.writeMu.Unlock()
		if err != nil {
			return err
		}
	}
	now := time.Now()
	for _, result := range batch.Results {
		completed, _ := parseEventTime(result.CompletedAt)
		retained := 0
		effects := make([]store.ResultEffect, 0, len(result.Effects))
		for _, effect := range result.Effects {
			effects = append(effects, lifecycleEffectToStore(effect, g.resultPayloadMode, &retained))
		}
		g.writeMu.Lock()
		id, duplicate, err := g.ix.AppendResultObservation(store.ResultObservation{
			ObservationID: result.ObservationID, SessionID: sessionID, SuppliedSessionID: result.SuppliedSessionID,
			Runtime: result.Runtime, Tool: result.Tool, NativeCallID: result.NativeCallID,
			NativeCallKind: result.NativeCallKind, SourceKind: result.SourceKind, SourceRef: result.SourceRef,
			SourceSequence: result.SourceSequence, SourceDigest: result.SourceDigest,
			SourceSegmentID: result.SourceSegmentID, CollectorID: "harvest-lifecycle",
			State: result.State, ErrorClass: result.ErrorClass, CompletedAt: completed,
			RawBytes: result.RawBytes, RawFieldBytes: result.RawBytes, DecodedBytes: result.DecodedBytes,
			RetainedBytes: 0, PayloadDigest: result.PayloadDigest, Completeness: result.Completeness,
			StdoutBytes: result.StdoutBytes, StdoutDigest: result.StdoutDigest,
			StderrBytes: result.StderrBytes, StderrDigest: result.StderrDigest,
			DeliveryAttempts: 1, DeliveryMode: "transcript", ReceivedAt: time.Now().Unix()}, effects)
		aliasAdded := false
		if err == nil {
			for _, alias := range result.NativeCallAliases {
				aliasDuplicate, aliasErr := g.ix.AppendResultNativeCallAlias(store.ResultNativeCallAlias{
					ResultID: id, NativeCallKind: alias.NativeCallKind, NativeCallID: alias.NativeCallID,
					Algorithm: alias.Algorithm})
				if aliasErr != nil {
					err = aliasErr
					break
				}
				aliasAdded = aliasAdded || !aliasDuplicate
			}
		}
		firstLogical := false
		if err == nil && (!duplicate || aliasAdded) {
			err = g.ix.RelateLogicalResults(id, time.Now().Unix())
		}
		if err == nil && !duplicate {
			firstLogical, err = g.ix.IsFirstLogicalCompletion(id)
		}
		var reconciliation store.ResultReconciliation
		if err == nil && (!duplicate || aliasAdded) {
			reconciliation, err = g.ix.ReconcileResultObservation(id, time.Now().Unix())
		}
		if err == nil && (!duplicate || aliasAdded) {
			err = syncResultActionIssue(g, id, result.ObservationID, sessionID, result.Runtime,
				"harvest-lifecycle", reconciliation, time.Now().Unix())
		}
		g.writeMu.Unlock()
		if err != nil {
			return err
		}
		if duplicate {
			continue
		}
		liveCompletion := completed > 0 && completed >= now.Add(-lifecycleLiveResultWindow).Unix() &&
			completed <= now.Add(lifecycleLiveResultWindow).Unix()
		if firstLogical && liveCompletion && result.State == "success" && len(result.Effects) > 0 {
			trigger := observation.ResultEnvelope{ObservationID: result.ObservationID, SessionID: sessionID,
				Runtime: result.Runtime, Cwd: cwd, State: result.State,
				Effects: make([]observation.ResultEffect, len(result.Effects))}
			if err := scheduleSettledCheckpoint(g, trigger, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func withLifecycleSlot(ctx context.Context, g *Governor, work func() error) error {
	select {
	case g.lifecycleSlot <- struct{}{}:
		defer func() { <-g.lifecycleSlot }()
		return work()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func reconcileLifecycleOnce(ctx context.Context, g *Governor) error {
	return withLifecycleSlot(ctx, g, func() error {
		sessions, err := g.ix.LiveSessionRuntimes(lifecycleReconcileSessionLimit)
		if err != nil {
			return err
		}
		return reconcileLifecycleSessions(ctx, g, sessions)
	})
}

func reconcileLifecycleSession(ctx context.Context, g *Governor, sessionID string) error {
	return withLifecycleSlot(ctx, g, func() error {
		sessions, err := g.ix.LiveSessionRuntimesForSession(sessionID, 20)
		if err != nil {
			return err
		}
		return reconcileLifecycleSessions(ctx, g, sessions)
	})
}

type lifecycleSource struct{ path, segment, cwd string }

func lifecycleSources(live store.LiveSessionRuntime) []lifecycleSource {
	if live.TranscriptPath != "" {
		return []lifecycleSource{{path: live.TranscriptPath,
			segment: strings.TrimSuffix(filepath.Base(live.TranscriptPath), filepath.Ext(live.TranscriptPath)),
			cwd:     live.WorkingDirectory}}
	}
	// Compatibility fallback for observations recorded before source_ref existed.
	// Current hooks always take the exact path above and never scan the corpus.
	sources := []lifecycleSource{}
	for _, summary := range harvest.FindAll(live.Runtime, live.SessionID) {
		sources = append(sources, lifecycleSource{path: summary.Path, segment: summary.ID, cwd: summary.Cwd})
	}
	return sources
}

func lifecycleReadRequest(live store.LiveSessionRuntime, source lifecycleSource, generation string,
	cursor store.TranscriptCursor, retainEffectBodies bool) harvest.LifecycleReadRequest {
	return harvest.LifecycleReadRequest{Path: source.path, Runtime: live.Runtime,
		SourceSegmentID: source.segment, SourceGeneration: generation,
		Offset: cursor.CommittedOffset, SourceLine: cursor.SourceLine, State: cursor.ParserState,
		MaxBytes: harvest.DefaultLifecycleReadBytes, MaxRecords: harvest.DefaultLifecycleReadRecords,
		RetainEffectBodies: retainEffectBodies}
}

func reconcileLifecycleSource(ctx context.Context, g *Governor, live store.LiveSessionRuntime,
	source lifecycleSource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if handled, err := reconcileLogicalLifecycleSource(ctx, g, live, source); handled {
		return err
	}
	info, err := os.Stat(source.path)
	if err != nil {
		return recordLifecycleIssue(g, live.SessionID, live.Runtime, source.path,
			"transcript-unavailable", err)
	}
	generation, err := transcriptGenerationDigest(source.path)
	if err != nil {
		return recordLifecycleIssue(g, live.SessionID, live.Runtime, source.path,
			"transcript-unavailable", err)
	}
	cursor, found, err := g.ix.TranscriptCursor(live.Runtime, source.path, source.segment)
	if err != nil {
		return err
	}
	actionCollectionVersion := harvest.LifecycleActionCollectionVersion(live.Runtime)
	if found && actionCollectionVersion > 0 && cursor.ActionParserVersion == 0 {
		return nil
	}
	if found && !cursor.RescanNeeded && cursor.CommittedOffset == info.Size() &&
		cursor.FileSize == info.Size() && cursor.FileMTime == info.ModTime().UnixNano() &&
		cursor.GenerationDigest == generation {
		return nil
	}
	cursorIssues := []store.CollectionIssue{}
	if found && (info.Size() < cursor.FileSize || cursor.GenerationDigest != generation) {
		now := time.Now().Unix()
		occurrence := fmt.Sprintf("previous-generation=%s;current-generation=%s;previous-size=%d;current-size=%d",
			cursor.GenerationDigest, generation, cursor.FileSize, info.Size())
		cursorIssues = append(cursorIssues, lifecycleCursorIssue(live.SessionID, live.Runtime,
			source.path, source.segment, generation, "transcript-changed", occurrence, 1, now))
		cursor.CommittedOffset, cursor.SourceLine = 0, 0
		cursor.SourceSequence, cursor.ParserState, cursor.ParserStateDigest = "", nil, ""
	}
	if cursor.CommittedOffset > info.Size() {
		cursor.CommittedOffset, cursor.SourceLine = 0, 0
		cursor.ParserState, cursor.ParserStateDigest = nil, ""
	}
	batch, supported, err := harvest.LifecycleIncremental(live.Runtime,
		lifecycleReadRequest(live, source, generation, cursor,
			g.resultPayloadMode != collectionconfig.MetadataOnly))
	if err != nil {
		return recordLifecycleIssue(g, live.SessionID, live.Runtime, source.path,
			"transcript-unavailable", err)
	}
	if !supported {
		return nil
	}
	if err := appendLifecycleBatch(g, live.SessionID, source.cwd, batch); err != nil {
		if errors.Is(err, ErrObservationCollision) && cursor.ActionParserVersion == -1 {
			g.writeMu.Lock()
			blockErr := g.ix.BlockTranscriptActionBackfill(live.Runtime, source.path, source.segment,
				time.Now().Unix())
			g.writeMu.Unlock()
			issueErr := recordLifecycleIssue(g, live.SessionID, live.Runtime, source.path,
				"collision", err)
			return errors.Join(blockErr, issueErr)
		}
		return err
	}
	g.writeMu.Lock()
	err = g.ix.ResolveCollectionIssue(observationIssueID("transcript-unavailable",
		live.Runtime+"\x00"+source.path), time.Now().Unix())
	g.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("resolve recovered lifecycle source: %w", err)
	}
	lastSequence := cursor.SourceSequence
	if len(batch.Results) > 0 {
		lastSequence = batch.Results[len(batch.Results)-1].SourceSequence
	}
	stateDigest := ""
	if len(batch.ParserState) > 0 {
		stateDigest = observation.DigestBytes(batch.ParserState)
	}
	actionParserVersion := cursor.ActionParserVersion
	if !found && actionCollectionVersion > 0 {
		actionParserVersion = -1
	}
	postInfo, statErr := os.Stat(source.path)
	if actionCollectionVersion > 0 && statErr == nil && !batch.Continuation &&
		batch.NextOffset == postInfo.Size() {
		actionParserVersion = actionCollectionVersion
	}
	needsMore := batch.Continuation || batch.NextOffset < info.Size()
	if actionCollectionVersion > 0 && actionParserVersion == -1 &&
		(statErr != nil || (postInfo != nil && batch.NextOffset < postInfo.Size())) {
		needsMore = true
	}
	committedAt := time.Now().Unix()
	if batch.MalformedRecords > 0 {
		occurrence := fmt.Sprintf("malformed-records=%d;range=%d:%d", batch.MalformedRecords,
			cursor.CommittedOffset, batch.NextOffset)
		cursorIssues = append(cursorIssues, lifecycleCursorIssue(live.SessionID, live.Runtime,
			source.path, source.segment, generation, "malformed", occurrence,
			batch.MalformedRecords, committedAt))
	}
	issues := append(cursorIssues, lifecycleParserIssues(live.SessionID, live.Runtime,
		source.path, source.segment,
		batch, committedAt)...)
	g.writeMu.Lock()
	err = g.ix.CommitTranscriptCursor(store.TranscriptCursor{Runtime: live.Runtime, SourceRef: source.path,
		SourceSegmentID: source.segment, SessionID: live.SessionID, WorkingDirectory: source.cwd,
		FileSize: info.Size(), FileMTime: info.ModTime().UnixNano(), GenerationDigest: generation,
		SourceSequence: lastSequence, CommittedOffset: batch.NextOffset,
		SourceLine: batch.NextSourceLine, ParserState: batch.ParserState,
		ParserStateDigest: stateDigest, RescanNeeded: needsMore, ContinuationNeeded: needsMore,
		ActionParserVersion: actionParserVersion, UpdatedAt: committedAt}, issues)
	g.writeMu.Unlock()
	if err == nil && g.lifecycle != nil {
		g.lifecycle.stats.bytesInspected.Add(batch.BytesInspected)
		g.lifecycle.stats.recordsDecoded.Add(int64(batch.RecordsDecoded))
	}
	return err
}

func reconcileLogicalLifecycleSource(ctx context.Context, g *Governor, live store.LiveSessionRuntime,
	source lifecycleSource) (bool, error) {
	cursor, found, err := g.ix.TranscriptCursor(live.Runtime, source.path, source.segment)
	if err != nil {
		return true, err
	}
	request := harvest.LogicalLifecycleReadRequest{Source: source.path, Segment: source.segment,
		SessionID: live.SessionID, Cursor: cursor.ParserState,
		MaxRecords:         harvest.DefaultLifecycleReadRecords,
		RetainEffectBodies: g.resultPayloadMode != collectionconfig.MetadataOnly}
	page, supported, err := harvest.LogicalLifecycle(live.Runtime, request)
	if !supported {
		return false, nil
	}
	if err != nil {
		return true, recordLifecycleIssue(g, live.SessionID, live.Runtime, source.path,
			"transcript-unavailable", err)
	}
	if found && cursor.GenerationDigest != "" && cursor.GenerationDigest != page.Generation {
		request.Cursor = nil
		page, _, err = harvest.LogicalLifecycle(live.Runtime, request)
		if err != nil {
			return true, err
		}
	}
	if found && !cursor.RescanNeeded && cursor.SourceSequence == page.UpdateMarker &&
		!page.Continuation {
		return true, nil
	}
	if err := appendLifecycleBatch(g, live.SessionID, source.cwd, page.Batch); err != nil {
		return true, err
	}
	stateDigest := ""
	if len(page.Cursor) > 0 {
		stateDigest = observation.DigestBytes(page.Cursor)
	}
	actionVersion := harvest.LifecycleActionCollectionVersion(live.Runtime)
	committedAt := time.Now().Unix()
	issues := lifecycleParserIssues(live.SessionID, live.Runtime, source.path, source.segment,
		page.Batch, committedAt)
	g.writeMu.Lock()
	err = g.ix.CommitTranscriptCursor(store.TranscriptCursor{Runtime: live.Runtime,
		SourceRef: source.path, SourceSegmentID: source.segment, SessionID: live.SessionID,
		WorkingDirectory: source.cwd, GenerationDigest: page.Generation,
		SourceSequence: page.UpdateMarker, CommittedOffset: page.Batch.NextOffset,
		SourceLine: page.Batch.NextSourceLine, ParserState: page.Cursor,
		ParserStateDigest: stateDigest, RescanNeeded: page.Continuation,
		ContinuationNeeded: page.Continuation, ActionParserVersion: actionVersion,
		UpdatedAt: committedAt}, issues)
	g.writeMu.Unlock()
	if err == nil && g.lifecycle != nil {
		g.lifecycle.stats.bytesInspected.Add(page.Batch.BytesInspected)
		g.lifecycle.stats.recordsDecoded.Add(int64(page.Batch.RecordsDecoded))
	}
	return true, err
}

func reconcileLifecycleSessions(ctx context.Context, g *Governor, sessions []store.LiveSessionRuntime) error {
	for _, live := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		sources := lifecycleSources(live)
		if len(sources) == 0 {
			if err := recordLifecycleIssue(g, live.SessionID, live.Runtime, live.SessionID,
				"transcript-unavailable", nil); err != nil {
				return err
			}
			continue
		}
		g.writeMu.Lock()
		err := g.ix.ResolveCollectionIssue(observationIssueID("transcript-unavailable",
			live.Runtime+"\x00"+live.SessionID), time.Now().Unix())
		g.writeMu.Unlock()
		if err != nil {
			return fmt.Errorf("resolve recovered lifecycle session: %w", err)
		}
		for _, source := range sources {
			if err := reconcileLifecycleSource(ctx, g, live, source); err != nil {
				return err
			}
		}
	}
	return nil
}

func recordLifecycleIssue(g *Governor, sessionID, runtime, source, kind string, cause error) error {
	now := time.Now().Unix()
	detail := ""
	if cause != nil {
		detail = observation.DigestBytes([]byte(cause.Error()))
	}
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	issueID := observationIssueID(kind, runtime+"\x00"+source)
	found, err := g.ix.CollectionIssueExists(issueID)
	if err != nil {
		return fmt.Errorf("check lifecycle %s issue: %w", kind, err)
	}
	if found {
		return nil
	}
	if err := g.ix.RecordCollectionIssue(store.CollectionIssue{
		IssueID: issueID, SessionID: sessionID,
		Runtime: runtime, ObservationID: source, CollectorID: "harvest-lifecycle",
		Kind: kind, AffectedCount: 1, FirstSeen: now, LastSeen: now, DetailDigest: detail,
	}); err != nil {
		return fmt.Errorf("record lifecycle %s issue: %w", kind, err)
	}
	return nil
}

func startLifecycleReconciliation(g *Governor) {
	coordinator := newLifecycleCoordinator(g)
	g.lifecycle = coordinator
	go coordinator.runWorker()
	go coordinator.runScheduler()
}

func finalizeMissingResults(g *Governor, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	err := reconcileLifecycleSession(ctx, g, sessionID)
	cancel()
	if err != nil {
		return fmt.Errorf("reconcile lifecycle before missing-result finalization: %w", err)
	}
	missing, err := g.ix.MissingResultsForSession(sessionID)
	if err != nil {
		return fmt.Errorf("select missing lifecycle results: %w", err)
	}
	now := time.Now().Unix()
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	for _, candidate := range missing {
		detail := observation.DigestBytes([]byte(candidate.NativeCallKind + "\x00" + candidate.NativeCallID))
		if err := g.ix.EnsureCollectionIssue(store.CollectionIssue{
			IssueID: observationIssueID("result-missing", candidate.ObservationID), SessionID: sessionID,
			Runtime: candidate.Runtime, ObservationID: candidate.ObservationID,
			CollectorID: "lifecycle-finalizer", Kind: "result-missing", AffectedCount: 1,
			FirstSeen: now, LastSeen: now, DetailDigest: detail}); err != nil {
			return fmt.Errorf("record missing lifecycle result: %w", err)
		}
	}
	return nil
}
