package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/collectionconfig"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

func resultTestGovernor(t *testing.T) *Governor {
	t.Helper()
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() {
		stopSettledCheckpoints(g)
		_ = ix.Close()
	})
	return g
}

func appendResultTestAttempt(t *testing.T, g *Governor, session, nativeID string, transcript ...string) int64 {
	t.Helper()
	got, err := g.ObserveV1(Observation{SessionID: session, Runtime: "claude", Tool: "Edit",
		Decision: "allow", TS: time.Now().Unix(), Origin: "live"}, ObservationEvidence{
		Delivery: store.EventDelivery{ObservationID: "obs_0123456789abcdef0123456789abcdef",
			ObservationSchema: observation.SchemaV1, EnvelopeDigest: "sha256-v1:attempt", CollectorID: observation.CollectorPreTool,
			NativeCallID: nativeID, NativeCallKind: "tool_use_id", QueuedAt: time.Now().Unix(),
			ReceivedAt: time.Now().Unix(), DeliveryAttempts: 1, DeliveryMode: "direct"},
		Input: store.EventInput{Completeness: "unavailable", SourceRef: firstString(transcript)}})
	if err != nil {
		t.Fatal(err)
	}
	return got.EventID
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func validResultEnvelope(session, nativeID string) observation.ResultEnvelope {
	body := []byte(`{"ok":true}`)
	return observation.ResultEnvelope{Schema: observation.ResultSchemaV1,
		ObservationID: "res_0123456789abcdef0123456789abcdef", CollectorID: observation.CollectorPostTool,
		Runtime: "claude", SessionID: session, HookEventName: "PostToolUse", NativeCallID: nativeID,
		NativeCallKind: "tool_use_id", Tool: "Edit", State: "success", CompletedAt: time.Now().Unix(),
		MediaType: observation.InputMediaTypeJSON, RawEnvelopeBytes: 200, RawFieldBytes: len(body),
		DecodedBytes: len(body), RetainedBytes: len(body), PayloadDigest: observation.DigestBytes(body),
		Completeness: "complete", Payload: body, Effects: []observation.ResultEffect{{Ordinal: 0,
			RawIdentity: "src/a.go", Operation: "update", EvidenceSource: "derived-input",
			SourceField: "tool_input.file_path", Completeness: "partial", BeforeBytes: 3,
			BeforeDigest: observation.DigestBytes([]byte("old")), BeforePayload: []byte("old"),
			AfterBytes: 3, AfterDigest: observation.DigestBytes([]byte("new")), AfterPayload: []byte("new"),
			DiffCompleteness: "unavailable"}}, QueuedAt: time.Now().Unix(), DeliveryAttempts: 1, DeliveryMode: "direct"}
}

func TestIngestResultLinksExactAndDaemonReenforcesCodeEffectsRetention(t *testing.T) {
	g := resultTestGovernor(t)
	eventID := appendResultTestAttempt(t, g, "session-1", "call-1")
	envelope := validResultEnvelope("session-1", "call-1")
	receipt, err := ingestResultV1(g, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.JoinClass != "exact" || receipt.ResultID == 0 {
		t.Fatalf("receipt = %+v", receipt)
	}
	reconciliation, err := g.ix.ReconcileResultObservation(receipt.ResultID, time.Now().Unix())
	if err != nil || len(reconciliation.Candidates) != 1 || reconciliation.Candidates[0].EventID != eventID {
		t.Fatalf("reconciliation = %+v err=%v", reconciliation, err)
	}
	results, err := g.ix.ResultsForSession("session-1", 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("results = %+v err=%v", results, err)
	}
	if results[0].RetainedBytes != 0 || results[0].Completeness != "metadata-only" || len(results[0].Effects) != 1 || string(results[0].Effects[0].ReplacementAfterPayload) != "new" {
		t.Fatalf("stored result = %+v", results[0])
	}
	stopSettledCheckpoints(g)
	duplicate, err := ingestResultV1(g, envelope)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate receipt=%+v err=%v", duplicate, err)
	}
	g.settled.Lock()
	rescheduled := len(g.settled.items)
	g.settled.Unlock()
	if rescheduled != 0 {
		t.Fatalf("duplicate direct result scheduled %d settled checkpoints", rescheduled)
	}
}

func TestIngestResultMetadataOnlyDropsAllBodiesButKeepsEffectFacts(t *testing.T) {
	g := resultTestGovernor(t)
	g.resultPayloadMode = collectionconfig.MetadataOnly
	appendResultTestAttempt(t, g, "session-1", "call-1")
	if _, err := ingestResultV1(g, validResultEnvelope("session-1", "call-1")); err != nil {
		t.Fatal(err)
	}
	results, err := g.ix.ResultsForSession("session-1", 10)
	if err != nil || len(results) != 1 || len(results[0].Effects) != 1 {
		t.Fatalf("results = %+v err=%v", results, err)
	}
	effect := results[0].Effects[0]
	if effect.ReplacementBeforePayload != nil || effect.ReplacementAfterPayload != nil || effect.ReplacementBeforeBytes != 3 || effect.ReplacementAfterDigest == "" {
		t.Fatalf("metadata-only effect = %+v", effect)
	}
}

func TestPathReconciliationRequiresMeasuredEffectBodyForNativeMatch(t *testing.T) {
	for _, test := range []struct {
		name           string
		retainEffect   bool
		classification string
	}{
		{name: "measured", retainEffect: true, classification: "native-effect-git-matched"},
		{name: "metadata only", retainEffect: false, classification: "action-reported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := v1TestRepo(t)
			g := resultTestGovernor(t)
			g.resultPayloadMode = collectionconfig.CompleteBounded
			action := testV1Envelope(t, repo)
			action.NativeCallID, action.NativeCallKind = "call-path", "tool_use_id"
			if _, err := ingestObservationV1(t.Context(), g, action); err != nil {
				t.Fatal(err)
			}
			content := []byte("package a\n// changed\n")
			if err := os.WriteFile(filepath.Join(repo, "a.go"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			result := validResultEnvelope(action.SessionID, "call-path")
			result.Effects = []observation.ResultEffect{{Ordinal: 0, RawIdentity: "a.go",
				Operation: "update", EvidenceSource: "runtime-result", SourceField: "changes",
				Completeness: "complete", ContentBytes: len(content),
				ContentDigest: observation.DigestBytes(content), DiffCompleteness: "unavailable"}}
			if test.retainEffect {
				result.Effects[0].ContentPayload = content
			}
			if _, err := ingestResultV1(g, result); err != nil {
				t.Fatal(err)
			}
			stopSettledCheckpoints(g)
			boundary := action
			boundary.ObservationID = "obs_boundary_0123456789abcdef01234567"
			checkpoint := capturePreBoundary(t.Context(), g, boundary, "pre-verification")
			if checkpoint.Status != "complete" {
				t.Fatalf("checkpoint=%+v", checkpoint)
			}
			facts, err := g.ix.PathReconciliationsForSession(action.SessionID, 20)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, fact := range facts {
				if fact.CurrentCheckpointID == checkpoint.ID && fact.Path == "a.go" {
					found = true
					if fact.Classification != test.classification {
						t.Fatalf("classification=%q want=%q fact=%+v", fact.Classification,
							test.classification, fact)
					}
				}
			}
			if !found {
				t.Fatalf("no reconciliation for checkpoint %d: %+v", checkpoint.ID, facts)
			}
		})
	}
}

func TestLifecycleReconciliationUsesExactObservedTranscriptPath(t *testing.T) {
	g := resultTestGovernor(t)
	transcript := filepath.Join(t.TempDir(), "session-exact.jsonl")
	body := `{"type":"assistant","sessionId":"session-exact","timestamp":"2026-08-23T10:00:00Z","message":{"content":[{"type":"tool_use","id":"toolu_exact","name":"Edit","input":{"file_path":"src/a.go","old_string":"old","new_string":"new"}}]}}
{"type":"user","sessionId":"session-exact","timestamp":"2026-08-23T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_exact","content":"Updated src/a.go","is_error":false}]}}
`
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	appendResultTestAttempt(t, g, "session-exact", "toolu_exact", transcript)
	if err := reconcileLifecycleOnce(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	results, err := g.ix.ResultsForSession("session-exact", 10)
	if err != nil || len(results) != 1 || len(results[0].Effects) != 1 {
		t.Fatalf("results = %+v err=%v", results, err)
	}
	if results[0].NativeCallID != "toolu_exact" || results[0].Effects[0].RawIdentity != "src/a.go" {
		t.Fatalf("targeted transcript result = %+v", results[0])
	}
	stats, err := g.ix.ResultStatsForSession("session-exact")
	if err != nil || stats.Exact != 1 {
		t.Fatalf("stats = %+v err=%v", stats, err)
	}
	cursor, found, err := g.ix.TranscriptCursor("claude", transcript, "session-exact")
	if err != nil || !found || cursor.CommittedOffset != int64(len(body)) || cursor.SourceLine != 2 || cursor.RescanNeeded {
		t.Fatalf("incremental cursor=%+v found=%v err=%v", cursor, found, err)
	}
	if err := reconcileLifecycleOnce(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	unchanged, err := g.ix.ResultsForSession("session-exact", 10)
	if err != nil || len(unchanged) != 1 {
		t.Fatalf("unchanged source created work: results=%+v err=%v", unchanged, err)
	}
	g.settled.Lock()
	settled := len(g.settled.items)
	g.settled.Unlock()
	if settled != 0 {
		t.Fatalf("historical transcript replay scheduled %d settled checkpoints", settled)
	}
}

func TestLifecycleCoordinatorScopesOversizedIssueByRuntimeAndSegment(t *testing.T) {
	g := resultTestGovernor(t)
	path := filepath.Join(t.TempDir(), "shared-segment.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	for written := int64(0); written <= harvest.MaxLifecycleRecordBytes; written += int64(len(chunk)) {
		if _, err := f.Write(chunk); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	sessions := []store.LiveSessionRuntime{
		{SessionID: "oversized-claude", Runtime: "claude", TranscriptPath: path},
		{SessionID: "oversized-codex", Runtime: "codex", TranscriptPath: path},
	}
	if err := reconcileLifecycleSessions(t.Context(), g, sessions); err != nil {
		t.Fatal(err)
	}
	claudeIssues, err := g.ix.CollectionIssuesForSession("oversized-claude", 10)
	if err != nil {
		t.Fatal(err)
	}
	codexIssues, err := g.ix.CollectionIssuesForSession("oversized-codex", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claudeIssues) != 1 || len(codexIssues) != 1 ||
		claudeIssues[0].IssueID == codexIssues[0].IssueID {
		t.Fatalf("runtime-scoped issues claude=%+v codex=%+v", claudeIssues, codexIssues)
	}
	if claudeIssues[0].Runtime != "claude" || codexIssues[0].Runtime != "codex" ||
		claudeIssues[0].SourceSegmentID != "shared-segment" ||
		codexIssues[0].SourceSegmentID != "shared-segment" {
		t.Fatalf("issue linkage claude=%+v codex=%+v", claudeIssues[0], codexIssues[0])
	}
}

func TestLifecycleUnjoinedIssueUsesDeliveredActionObservationBoundary(t *testing.T) {
	g := resultTestGovernor(t)
	transcript := filepath.Join(t.TempDir(), "historical.jsonl")
	body := `{"type":"assistant","sessionId":"historical","timestamp":"2026-08-23T10:00:00Z","message":{"content":[{"type":"tool_use","id":"toolu_historical","name":"Edit","input":{"file_path":"src/a.go","old_string":"old","new_string":"new"}}]}}
{"type":"user","sessionId":"historical","timestamp":"2026-08-23T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_historical","content":"Updated","is_error":false}]}}
`
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, supported, err := harvest.Lifecycle("claude", transcript)
	if err != nil || !supported || len(batch.Results) != 1 {
		t.Fatalf("historical batch=%+v supported=%v err=%v", batch, supported, err)
	}
	if err := appendLifecycleBatch(g, "historical", t.TempDir(), batch); err != nil {
		t.Fatal(err)
	}
	issues, err := g.ix.CollectionIssuesForSession("historical", 10)
	if err != nil || len(issues) != 0 {
		t.Fatalf("historical issues=%+v err=%v", issues, err)
	}
	stats, err := g.ix.ResultStatsForSession("historical")
	if err != nil || stats.NoBoundary != 1 || stats.ExpectedMissing != 0 || stats.Unjoined != 1 {
		t.Fatalf("historical stats=%+v err=%v", stats, err)
	}

	appendResultTestAttempt(t, g, "observed-gap", "toolu_observed")
	now := time.Now().UTC().Add(time.Second)
	gapTranscript := filepath.Join(t.TempDir(), "observed-gap.jsonl")
	gapBody := fmt.Sprintf(`{"type":"assistant","sessionId":"observed-gap","timestamp":%q,"message":{"content":[{"type":"tool_use","id":"toolu_missing","name":"Bash","input":{"command":"true"}}]}}
{"type":"user","sessionId":"observed-gap","timestamp":%q,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_missing","content":"","is_error":false}]}}
`, now.Add(-time.Second).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err := os.WriteFile(gapTranscript, []byte(gapBody), 0o600); err != nil {
		t.Fatal(err)
	}
	gapBatch, supported, err := harvest.Lifecycle("claude", gapTranscript)
	if err != nil || !supported || len(gapBatch.Results) != 1 {
		t.Fatalf("gap batch=%+v supported=%v err=%v", gapBatch, supported, err)
	}
	if err := appendLifecycleBatch(g, "observed-gap", t.TempDir(), gapBatch); err != nil {
		t.Fatal(err)
	}
	issues, err = g.ix.CollectionIssuesForSession("observed-gap", 10)
	if err != nil || len(issues) != 1 || issues[0].ResolvedAt != 0 {
		t.Fatalf("observed gap issues=%+v err=%v", issues, err)
	}
	stats, err = g.ix.ResultStatsForSession("observed-gap")
	if err != nil || stats.ExpectedMissing != 1 {
		t.Fatalf("observed gap stats=%+v err=%v", stats, err)
	}
}

func TestDirectAndTranscriptSourcesCreateOneLogicalCheckpointAdmission(t *testing.T) {
	g := resultTestGovernor(t)
	appendResultTestAttempt(t, g, "session-logical", "toolu-logical")
	direct := validResultEnvelope("session-logical", "toolu-logical")
	direct.Cwd = t.TempDir()
	if _, err := ingestResultV1(g, direct); err != nil {
		t.Fatal(err)
	}
	stopSettledCheckpoints(g)
	now := time.Now().UTC()
	transcript := filepath.Join(t.TempDir(), "session-logical.jsonl")
	body := fmt.Sprintf(`{"type":"assistant","sessionId":"session-logical","timestamp":%q,"message":{"content":[{"type":"tool_use","id":"toolu-logical","name":"Edit","input":{"file_path":"src/a.go","old_string":"old","new_string":"new"}}]}}
{"type":"user","sessionId":"session-logical","timestamp":%q,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu-logical","content":"Updated","is_error":false}]}}
`, now.Add(-time.Second).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, supported, err := harvest.Lifecycle("claude", transcript)
	if err != nil || !supported || len(batch.Results) != 1 {
		t.Fatalf("batch=%+v supported=%v err=%v", batch, supported, err)
	}
	if err := appendLifecycleBatch(g, "session-logical", direct.Cwd, batch); err != nil {
		t.Fatal(err)
	}
	g.settled.Lock()
	rescheduled := len(g.settled.items)
	g.settled.Unlock()
	if rescheduled != 0 {
		t.Fatalf("second primary source scheduled %d logical checkpoints", rescheduled)
	}
	stats, err := g.ix.ResultStatsForSession("session-logical")
	if err != nil || stats.SourceObserved != 2 || stats.LogicalCompletions != 1 {
		t.Fatalf("logical stats=%+v err=%v", stats, err)
	}
}

func TestSettledCheckpointCoalescingRetainsEveryTrigger(t *testing.T) {
	g := resultTestGovernor(t)
	cwd := t.TempDir()
	for index, call := range []string{"call-a", "call-b"} {
		envelope := validResultEnvelope("session-triggers", call)
		envelope.ObservationID = fmt.Sprintf("res_%032x", index+1)
		envelope.Cwd = cwd
		if _, err := ingestResultV1(g, envelope); err != nil {
			t.Fatal(err)
		}
	}
	g.settled.Lock()
	if len(g.settled.items) != 1 {
		count := len(g.settled.items)
		g.settled.Unlock()
		t.Fatalf("settled items=%d want 1", count)
	}
	var checkpointID int64
	for _, state := range g.settled.items {
		checkpointID = state.checkpoint.ID
	}
	g.settled.Unlock()
	count, err := g.ix.SessionCheckpointTriggerCount(checkpointID)
	if err != nil || count != 2 {
		t.Fatalf("checkpoint triggers=%d err=%v", count, err)
	}
}

func TestLifecycleQueueOverflowLeavesDurableRescan(t *testing.T) {
	g := resultTestGovernor(t)
	coordinator := newLifecycleCoordinator(g)
	dir := t.TempDir()
	for index := 0; index <= lifecycleQueueCapacity; index++ {
		if err := coordinator.enqueue(store.LiveSessionRuntime{SessionID: fmt.Sprintf("s-%d", index),
			Runtime: "codex", TranscriptPath: filepath.Join(dir, fmt.Sprintf("r-%d.jsonl", index))}); err != nil {
			t.Fatal(err)
		}
	}
	rescans, err := g.ix.TranscriptRescanSources(10)
	if err != nil || len(rescans) != 1 || rescans[0].SessionID != fmt.Sprintf("s-%d", lifecycleQueueCapacity) {
		t.Fatalf("durable overflow rescans=%+v err=%v", rescans, err)
	}
}

func TestLifecycleQueueOverflowReturnsRescanFailureAndRetries(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.sqlite")
	writable, err := store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenRO(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(readOnly, nil)
	coordinator := newLifecycleCoordinator(g)
	dir := t.TempDir()
	for index := 0; index < lifecycleQueueCapacity; index++ {
		if err := coordinator.enqueue(store.LiveSessionRuntime{SessionID: fmt.Sprintf("queued-%d", index),
			Runtime: "codex", TranscriptPath: filepath.Join(dir, fmt.Sprintf("q-%d.jsonl", index))}); err != nil {
			t.Fatal(err)
		}
	}
	overflow := store.LiveSessionRuntime{SessionID: "overflow", Runtime: "codex",
		TranscriptPath: filepath.Join(dir, "overflow.jsonl")}
	if err := coordinator.hint(overflow); err == nil {
		t.Fatal("read-only rescan admission unexpectedly succeeded")
	}
	if coordinator.stats.failed.Load() != 1 || coordinator.stats.overflow.Load() != 1 {
		t.Fatalf("failed=%d overflow=%d", coordinator.stats.failed.Load(),
			coordinator.stats.overflow.Load())
	}
	if delayed, ok := coordinator.delayed[lifecycleWorkKey(overflow)]; !ok || delayed.work.SessionID != overflow.SessionID {
		t.Fatalf("failed admission was not retained for bounded retry: %+v", coordinator.delayed)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	writable, err = store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g.ix = writable
	t.Cleanup(func() {
		stopSettledCheckpoints(g)
		_ = writable.Close()
	})
	coordinator.admitDelayed(time.Now().Add(time.Hour))
	rescans, err := writable.TranscriptRescanSources(10)
	if err != nil || len(rescans) != 1 || rescans[0].SessionID != overflow.SessionID {
		t.Fatalf("retried overflow rescans=%+v err=%v", rescans, err)
	}
}

func TestResultReceiptLifecycleAdmissionFailsForFirstAndDuplicate(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.sqlite")
	writable, err := store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenRO(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(readOnly, nil)
	coordinator := newLifecycleCoordinator(g)
	g.lifecycle = coordinator
	t.Cleanup(func() {
		stopSettledCheckpoints(g)
		_ = readOnly.Close()
	})
	dir := t.TempDir()
	for index := 0; index < lifecycleQueueCapacity; index++ {
		if err := coordinator.enqueue(store.LiveSessionRuntime{SessionID: fmt.Sprintf("queued-result-%d", index),
			Runtime: "codex", TranscriptPath: filepath.Join(dir, fmt.Sprintf("result-%d.jsonl", index))}); err != nil {
			t.Fatal(err)
		}
	}
	envelope := validResultEnvelope("result-admission", "call-admission")
	envelope.Runtime = "codex"
	envelope.TranscriptPath = filepath.Join(dir, "result-admission.jsonl")
	for _, duplicate := range []bool{false, true} {
		receipt := observation.ResultReceipt{Schema: observation.ResultSchemaV1,
			ObservationID: envelope.ObservationID, ResultID: 1, Duplicate: duplicate}
		if _, err := resultReceiptWithLifecycleHint(g, envelope, receipt); err == nil {
			t.Fatalf("duplicate=%v read-only rescan admission unexpectedly succeeded", duplicate)
		}
	}
	if coordinator.stats.failed.Load() != 2 {
		t.Fatalf("result admission failures=%d want 2", coordinator.stats.failed.Load())
	}
}

func TestIngestResultFirstAndDuplicateRequestLifecycleRescan(t *testing.T) {
	g := resultTestGovernor(t)
	coordinator := newLifecycleCoordinator(g)
	g.lifecycle = coordinator
	dir := t.TempDir()
	for index := 0; index < lifecycleQueueCapacity; index++ {
		if err := coordinator.enqueue(store.LiveSessionRuntime{SessionID: fmt.Sprintf("ingest-queued-%d", index),
			Runtime: "claude", TranscriptPath: filepath.Join(dir, fmt.Sprintf("ingest-%d.jsonl", index))}); err != nil {
			t.Fatal(err)
		}
	}
	appendResultTestAttempt(t, g, "ingest-result-hint", "call-result-hint")
	envelope := validResultEnvelope("ingest-result-hint", "call-result-hint")
	envelope.TranscriptPath = filepath.Join(dir, "ingest-result-hint.jsonl")
	first, err := ingestResultV1(g, envelope)
	if err != nil || first.Duplicate || coordinator.stats.overflow.Load() != 1 {
		t.Fatalf("first receipt=%+v overflow=%d err=%v", first,
			coordinator.stats.overflow.Load(), err)
	}
	stopSettledCheckpoints(g)
	duplicate, err := ingestResultV1(g, envelope)
	if err != nil || !duplicate.Duplicate || coordinator.stats.overflow.Load() != 2 {
		t.Fatalf("duplicate receipt=%+v overflow=%d err=%v", duplicate,
			coordinator.stats.overflow.Load(), err)
	}
	rescans, err := g.ix.TranscriptRescanSources(10)
	if err != nil || len(rescans) != 1 || rescans[0].SessionID != envelope.SessionID {
		t.Fatalf("result lifecycle rescans=%+v err=%v", rescans, err)
	}
}

func TestLifecycleResolutionFailureLeavesCursorAndRetries(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.sqlite")
	transcript := filepath.Join(t.TempDir(), "resolution.jsonl")
	body := []byte("{}\n")
	if err := os.WriteFile(transcript, body, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(transcript)
	if err != nil {
		t.Fatal(err)
	}
	generation := mustTranscriptGeneration(t, transcript)
	segment := lifecycleSegment(transcript)
	cursor := store.TranscriptCursor{Runtime: "claude", SourceRef: transcript,
		SourceSegmentID: segment, SessionID: "resolution", FileSize: info.Size(),
		FileMTime: info.ModTime().UnixNano(), GenerationDigest: generation,
		ActionParserVersion: 1, UpdatedAt: 10}
	writable, err := store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.UpsertTranscriptCursor(cursor); err != nil {
		t.Fatal(err)
	}
	if err := writable.EnsureCollectionIssue(store.CollectionIssue{
		IssueID:   observationIssueID("transcript-unavailable", cursor.Runtime+"\x00"+transcript),
		SessionID: cursor.SessionID, Runtime: cursor.Runtime, ObservationID: transcript,
		CollectorID: "harvest-lifecycle", Kind: "transcript-unavailable", AffectedCount: 1,
		FirstSeen: 1, LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenRO(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(readOnly, nil)
	err = reconcileLifecycleSource(t.Context(), g,
		store.LiveSessionRuntime{SessionID: cursor.SessionID, Runtime: cursor.Runtime},
		lifecycleSource{path: transcript, segment: segment})
	if err == nil {
		t.Fatal("read-only issue resolution unexpectedly succeeded")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	writable, err = store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g.ix = writable
	t.Cleanup(func() {
		stopSettledCheckpoints(g)
		_ = writable.Close()
	})
	unchanged, found, err := writable.TranscriptCursor(cursor.Runtime, transcript, segment)
	if err != nil || !found || unchanged.CommittedOffset != 0 || unchanged.UpdatedAt != cursor.UpdatedAt {
		t.Fatalf("cursor changed after resolution failure: %+v found=%v err=%v", unchanged, found, err)
	}
	issues, err := writable.CollectionIssuesForSession(cursor.SessionID, 10)
	if err != nil || len(issues) != 1 || issues[0].ResolvedAt != 0 {
		t.Fatalf("issue changed after failed resolution: %+v err=%v", issues, err)
	}
	if err := reconcileLifecycleSource(t.Context(), g,
		store.LiveSessionRuntime{SessionID: cursor.SessionID, Runtime: cursor.Runtime},
		lifecycleSource{path: transcript, segment: segment}); err != nil {
		t.Fatal(err)
	}
	retried, _, err := writable.TranscriptCursor(cursor.Runtime, transcript, segment)
	issues, issueErr := writable.CollectionIssuesForSession(cursor.SessionID, 10)
	if err != nil || issueErr != nil || retried.CommittedOffset != int64(len(body)) ||
		len(issues) != 1 || issues[0].ResolvedAt == 0 {
		t.Fatalf("retry cursor=%+v issues=%+v cursorErr=%v issueErr=%v", retried, issues, err, issueErr)
	}
}

func TestMissingResultFinalizationFailureRetriesExactlyOnce(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.sqlite")
	transcript := filepath.Join(t.TempDir(), "missing.jsonl")
	body := []byte("{}\n")
	if err := os.WriteFile(transcript, body, 0o600); err != nil {
		t.Fatal(err)
	}
	writable, err := store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(writable, nil)
	appendResultTestAttempt(t, g, "missing-finalizer", "call-missing", transcript)
	info, err := os.Stat(transcript)
	if err != nil {
		t.Fatal(err)
	}
	segment := lifecycleSegment(transcript)
	if err := writable.UpsertTranscriptCursor(store.TranscriptCursor{Runtime: "claude",
		SourceRef: transcript, SourceSegmentID: segment, SessionID: "missing-finalizer",
		FileSize: info.Size(), FileMTime: info.ModTime().UnixNano(),
		GenerationDigest: mustTranscriptGeneration(t, transcript), CommittedOffset: info.Size(),
		SourceLine: 1, ActionParserVersion: 1, UpdatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenRO(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g.ix = readOnly
	if err := finalizeMissingResults(g, "missing-finalizer"); err == nil {
		t.Fatal("read-only missing-result issue write unexpectedly succeeded")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	writable, err = store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g.ix = writable
	t.Cleanup(func() {
		stopSettledCheckpoints(g)
		_ = writable.Close()
	})
	if err := finalizeMissingResults(g, "missing-finalizer"); err != nil {
		t.Fatal(err)
	}
	if err := finalizeMissingResults(g, "missing-finalizer"); err != nil {
		t.Fatal(err)
	}
	issues, err := writable.CollectionIssuesForSession("missing-finalizer", 10)
	if err != nil || len(issues) != 1 || issues[0].Kind != "result-missing" ||
		issues[0].AffectedCount != 1 {
		t.Fatalf("retried finalizer issues=%+v err=%v", issues, err)
	}
}

func TestIdleTimeoutTickCountsFinalizerFailureAndRetriesCapturingCheckpoint(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.sqlite")
	writable, err := store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(writable, nil)
	coordinator := newLifecycleCoordinator(g)
	g.lifecycle = coordinator
	g.startedAt = time.Now().Add(-time.Minute).Unix()
	appendResultTestAttempt(t, g, "idle-finalizer", "call-idle")
	now := time.Now().Add(10 * time.Minute)
	idle, err := writable.IdleSessions(now.Add(-5*time.Minute).Unix(), g.startedAt, 10)
	if err != nil || len(idle) != 1 {
		t.Fatalf("idle sessions=%+v err=%v", idle, err)
	}
	requestID := observation.DigestBytes([]byte("timeout\x00" + idle[0].SessionID + "\x00" +
		time.Unix(idle[0].LastEventAt, 0).String()))
	envelope := observation.Envelope{ObservationID: requestID, SessionID: idle[0].SessionID,
		Runtime: idle[0].Runtime, Cwd: idle[0].WorkingDirectory}
	checkpoint := checkpointRequest(envelope, "timeout", requestID, "point-in-time")
	checkpoint.RequestedAt = now.Unix()
	got, err := ensureCheckpoint(g, checkpoint)
	if err != nil {
		t.Fatalf("checkpoint=%+v err=%v", got, err)
	}
	got, won, err := writable.ClaimSessionCheckpoint(got.ID, now.Unix(), 0, "point-in-time")
	if err != nil || !won || got.Status != "capturing" {
		t.Fatalf("claimed checkpoint=%+v won=%v err=%v", got, won, err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenRO(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g.ix = readOnly
	if err := runIdleTimeoutTick(g, now); err == nil {
		t.Fatal("read-only idle finalization unexpectedly succeeded")
	}
	if coordinator.stats.failed.Load() != 1 {
		t.Fatalf("idle scheduler failure count=%d want 1", coordinator.stats.failed.Load())
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	writable, err = store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	g.ix = writable
	t.Cleanup(func() {
		stopSettledCheckpoints(g)
		_ = writable.Close()
	})
	if err := runIdleTimeoutTick(g, now); err != nil {
		t.Fatal(err)
	}
	if err := runIdleTimeoutTick(g, now); err != nil {
		t.Fatal(err)
	}
	issues, err := writable.CollectionIssuesForSession("idle-finalizer", 10)
	if err != nil || len(issues) != 2 {
		t.Fatalf("retried idle issues=%+v err=%v", issues, err)
	}
	for _, issue := range issues {
		if issue.AffectedCount != 1 {
			t.Fatalf("idle issue inflated on retry: %+v", issue)
		}
	}
	current, err := writable.SessionCheckpointByID(got.ID)
	if err != nil || current.Status != "capturing" {
		t.Fatalf("capturing checkpoint changed during finalizer retry: %+v err=%v",
			current, err)
	}
}

func TestLifecycleRepairAddsExactCodexPatchAliasWithoutCheckpoint(t *testing.T) {
	g := resultTestGovernor(t)
	_, err := g.ObserveV1(Observation{SessionID: "codex-repair", Runtime: "codex", Tool: "apply_patch",
		Decision: "allow", TS: time.Now().Unix(), Origin: "live"}, ObservationEvidence{
		Delivery: store.EventDelivery{ObservationID: "obs_codex_repair_0123456789abcdef", ObservationSchema: observation.SchemaV1,
			EnvelopeDigest: "sha256-v1:attempt", CollectorID: observation.CollectorPreTool,
			NativeCallID: "exec-repair", NativeCallKind: "tool_use_id", ReceivedAt: time.Now().Unix(),
			DeliveryAttempts: 1, DeliveryMode: "direct"},
		Input: store.EventInput{Completeness: "unavailable"}})
	if err != nil {
		t.Fatal(err)
	}
	resultID, _, err := g.ix.AppendResultObservation(store.ResultObservation{ObservationID: "res_codex_repair_0123456789abcdef",
		SessionID: "codex-repair", Runtime: "codex", NativeCallID: "exec-repair", NativeCallKind: "patch_call_id",
		SourceKind: "vendor-patch-result", SourceDigest: "sha256-v1:result", State: "success",
		Completeness: "metadata-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := g.ix.ReconcileResultObservation(resultID, time.Now().Unix()); err != nil || got.JoinClass != "unjoined" {
		t.Fatalf("before repair=%+v err=%v", got, err)
	}
	before, err := g.ix.SessionCheckpoints("codex-repair", 20)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newLifecycleCoordinator(g)
	coordinator.repairResultAliases()
	after, err := g.ix.SessionCheckpoints("codex-repair", 20)
	if err != nil || len(after) != len(before) {
		t.Fatalf("checkpoint population before=%d after=%d err=%v", len(before), len(after), err)
	}
	aliases, err := g.ix.ResultNativeCallAliases(resultID)
	if err != nil || len(aliases) != 1 || aliases[0].NativeCallID != "exec-repair" {
		t.Fatalf("aliases=%+v err=%v", aliases, err)
	}
	got, err := g.ix.ReconcileResultObservation(resultID, time.Now().Unix())
	if err != nil || got.JoinClass != "exact" || coordinator.stats.aliasesRepaired.Load() != 1 {
		t.Fatalf("after repair=%+v repaired=%d err=%v", got, coordinator.stats.aliasesRepaired.Load(), err)
	}
	coordinator.repairResultAliases()
	if coordinator.stats.aliasesRepaired.Load() != 1 {
		t.Fatalf("repair replay changed counter=%d", coordinator.stats.aliasesRepaired.Load())
	}
}

func TestLifecycleReconciliationBoundsEachLargeTranscriptTurn(t *testing.T) {
	g := resultTestGovernor(t)
	transcript := filepath.Join(t.TempDir(), "session-bounded.jsonl")
	line := `{"type":"assistant","sessionId":"session-bounded","message":{"content":[]},"pad":"` + strings.Repeat("x", 256) + `"}` + "\n"
	var body strings.Builder
	for body.Len() < 5<<20 {
		body.WriteString(line)
	}
	if err := os.WriteFile(transcript, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	appendResultTestAttempt(t, g, "session-bounded", "toolu-bounded", transcript)
	if err := reconcileLifecycleOnce(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	first, found, err := g.ix.TranscriptCursor("claude", transcript, "session-bounded")
	if err != nil || !found || first.CommittedOffset <= 0 || first.SourceLine > harvest.DefaultLifecycleReadRecords ||
		first.CommittedOffset >= int64(body.Len()) || !first.RescanNeeded {
		t.Fatalf("first bounded cursor=%+v size=%d found=%v err=%v", first, body.Len(), found, err)
	}
	if first.CommittedOffset > harvest.DefaultLifecycleReadBytes+int64(len(line)) {
		t.Fatalf("first turn read %d bytes beyond budget %d", first.CommittedOffset, harvest.DefaultLifecycleReadBytes)
	}
	if err := reconcileLifecycleOnce(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	second, _, err := g.ix.TranscriptCursor("claude", transcript, "session-bounded")
	if err != nil || second.CommittedOffset <= first.CommittedOffset ||
		second.CommittedOffset-first.CommittedOffset > harvest.DefaultLifecycleReadBytes+int64(len(line)) {
		t.Fatalf("second bounded cursor first=%+v second=%+v err=%v", first, second, err)
	}
}

func TestLifecycleCursorDoesNotMoveWhenLossIssuePersistenceFails(t *testing.T) {
	for _, test := range []struct {
		name           string
		body           string
		previousSize   int64
		previousDigest string
		kind           string
		occurrence     func(store.TranscriptCursor, string, int64) string
		affectedCount  int
	}{
		{name: "malformed record", body: "not-json\n", kind: "malformed", affectedCount: 1,
			occurrence: func(cursor store.TranscriptCursor, _ string, next int64) string {
				return fmt.Sprintf("malformed-records=1;range=%d:%d", cursor.CommittedOffset, next)
			}},
		{name: "transcript changed", body: "{}\n", previousSize: 99,
			previousDigest: "sha256-v1:previous", kind: "transcript-changed", affectedCount: 1,
			occurrence: func(cursor store.TranscriptCursor, generation string, next int64) string {
				return fmt.Sprintf("previous-generation=%s;current-generation=%s;previous-size=%d;current-size=%d",
					cursor.GenerationDigest, generation, cursor.FileSize, next)
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := resultTestGovernor(t)
			transcript := filepath.Join(t.TempDir(), "session-loss.jsonl")
			if err := os.WriteFile(transcript, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(transcript)
			if err != nil {
				t.Fatal(err)
			}
			generation := mustTranscriptGeneration(t, transcript)
			segment := lifecycleSegment(transcript)
			cursor := store.TranscriptCursor{Runtime: "claude", SourceRef: transcript,
				SourceSegmentID: segment, SessionID: "session-loss", FileSize: info.Size(),
				FileMTime: info.ModTime().UnixNano(), GenerationDigest: generation,
				CommittedOffset: 0, SourceLine: 0, ActionParserVersion: 1, UpdatedAt: 10}
			if test.previousSize != 0 {
				cursor.FileSize = test.previousSize
			}
			if test.previousDigest != "" {
				cursor.GenerationDigest = test.previousDigest
			}
			if err := g.ix.UpsertTranscriptCursor(cursor); err != nil {
				t.Fatal(err)
			}
			occurrence := test.occurrence(cursor, generation, info.Size())
			collision := lifecycleCursorIssue(cursor.SessionID, cursor.Runtime, transcript,
				segment, generation, test.kind, occurrence, test.affectedCount, 20)
			collision.CollectorID = "collision-fixture"
			if err := g.ix.EnsureCollectionIssue(collision); err != nil {
				t.Fatal(err)
			}
			err = reconcileLifecycleSource(t.Context(), g,
				store.LiveSessionRuntime{SessionID: cursor.SessionID, Runtime: cursor.Runtime},
				lifecycleSource{path: transcript, segment: segment})
			if !errors.Is(err, store.ErrCollectionIssueCollision) {
				t.Fatalf("reconcile error=%v, want collection issue collision", err)
			}
			got, found, err := g.ix.TranscriptCursor(cursor.Runtime, transcript, segment)
			if err != nil || !found {
				t.Fatalf("cursor found=%v err=%v", found, err)
			}
			if got.CommittedOffset != cursor.CommittedOffset || got.SourceLine != cursor.SourceLine ||
				got.FileSize != cursor.FileSize || got.GenerationDigest != cursor.GenerationDigest ||
				got.UpdatedAt != cursor.UpdatedAt {
				t.Fatalf("cursor moved after issue failure: before=%+v after=%+v", cursor, got)
			}
		})
	}
}

func TestLifecycleDuplicateDoesNotRescheduleSettledCheckpoint(t *testing.T) {
	g := resultTestGovernor(t)
	transcript := filepath.Join(t.TempDir(), "session-replay.jsonl")
	now := time.Now().UTC()
	body := fmt.Sprintf(`{"type":"assistant","sessionId":"session-replay","timestamp":%q,"message":{"content":[{"type":"tool_use","id":"toolu_replay","name":"Edit","input":{"file_path":"src/a.go","old_string":"old","new_string":"new"}}]}}
{"type":"user","sessionId":"session-replay","timestamp":%q,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_replay","content":"Updated src/a.go","is_error":false}]}}
`, now.Add(-time.Second).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, supported, err := harvest.Lifecycle("claude", transcript)
	if err != nil || !supported || len(batch.Results) != 1 {
		t.Fatalf("batch supported=%v results=%d err=%v", supported, len(batch.Results), err)
	}
	if err := appendLifecycleBatch(g, "session-replay", t.TempDir(), batch); err != nil {
		t.Fatal(err)
	}
	g.settled.Lock()
	first := len(g.settled.items)
	g.settled.Unlock()
	if first != 1 {
		t.Fatalf("new live result scheduled checkpoints=%d want 1", first)
	}
	stopSettledCheckpoints(g)
	if err := appendLifecycleBatch(g, "session-replay", t.TempDir(), batch); err != nil {
		t.Fatal(err)
	}
	g.settled.Lock()
	replayed := len(g.settled.items)
	g.settled.Unlock()
	if replayed != 0 {
		t.Fatalf("duplicate replay scheduled %d settled checkpoints", replayed)
	}
}

func TestCodexLifecycleOuterActionStoresInputAndReconcilesOnlyExactCallID(t *testing.T) {
	g := resultTestGovernor(t)
	transcript := filepath.Join(t.TempDir(), "rollout-outer.jsonl")
	body := `{"timestamp":"2026-08-24T10:00:00Z","payload":{"type":"session_meta","payload":{"session_id":"outer-session"}}}
{"timestamp":"2026-08-24T10:00:01Z","payload":{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_outer","name":"exec","input":"await tools.exec_command({cmd:\"go test ./...\"})"}}}
{"timestamp":"2026-08-24T10:00:02Z","payload":{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_outer","output":"ok"}}}
`
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// An independently observed inner call has a different identity and remains a
	// separate action; no ordering/proximity relation may join the outer result to it.
	_, err := g.ObserveV1(Observation{SessionID: "outer-session", Runtime: "codex", Tool: "Bash",
		Decision: "allow", TS: time.Now().Unix(), Origin: "live"}, ObservationEvidence{
		Delivery: store.EventDelivery{ObservationID: "obs_inner_0123456789abcdef",
			ObservationSchema: observation.SchemaV1, EnvelopeDigest: "sha256-v1:inner",
			CollectorID: observation.CollectorPreTool, NativeCallID: "exec-inner",
			NativeCallKind: "tool_use_id", ReceivedAt: time.Now().Unix(), DeliveryAttempts: 1,
			DeliveryMode: "direct"}, Input: store.EventInput{Completeness: "unavailable"}})
	if err != nil {
		t.Fatal(err)
	}
	batch, supported, err := harvest.Lifecycle("codex", transcript)
	if err != nil || !supported || len(batch.Actions) != 1 || len(batch.Results) != 1 {
		t.Fatalf("batch actions=%d results=%d supported=%v err=%v", len(batch.Actions), len(batch.Results), supported, err)
	}
	if err := appendLifecycleBatch(g, "outer-session", t.TempDir(), batch); err != nil {
		t.Fatal(err)
	}
	events, err := g.ix.EventsForSession("outer-session", 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	outer := events[0]
	if events[1].Origin == "transcript" {
		outer = events[1]
	}
	if outer.Origin != "transcript" || outer.Tool != "exec" || outer.Verb != "use" || outer.Decision != "" {
		t.Fatalf("outer event=%+v", outer)
	}
	delivery, input, found, err := g.ix.ObservationEvidence(batch.Actions[0].ObservationID)
	if err != nil || !found || delivery.NativeCallID != "call_outer" || delivery.NativeCallKind != "call_id" ||
		delivery.DeliveryMode != "transcript" || input.Completeness != "complete" ||
		string(input.Payload) != `await tools.exec_command({cmd:"go test ./..."})` {
		t.Fatalf("delivery=%+v input=%+v found=%v err=%v", delivery, input, found, err)
	}
	results, err := g.ix.ResultsForSession("outer-session", 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	reconciliation, err := g.ix.ReconcileResultObservation(results[0].ID, time.Now().Unix())
	if err != nil || reconciliation.JoinClass != "exact" || len(reconciliation.Candidates) != 1 ||
		reconciliation.Candidates[0].EventID != outer.ID {
		t.Fatalf("reconciliation=%+v err=%v", reconciliation, err)
	}
	checkpoints, err := g.ix.SessionCheckpoints("outer-session", 10)
	if err != nil || len(checkpoints) != 0 {
		t.Fatalf("transcript action scheduled checkpoints=%+v err=%v", checkpoints, err)
	}
	if err := appendLifecycleBatch(g, "outer-session", t.TempDir(), batch); err != nil {
		t.Fatal(err)
	}
	events, _ = g.ix.EventsForSession("outer-session", 10)
	results, _ = g.ix.ResultsForSession("outer-session", 10)
	if len(events) != 2 || len(results) != 1 {
		t.Fatalf("replay grew populations events=%d results=%d", len(events), len(results))
	}
}

func TestCodexLifecycleOverBudgetActionIsMetadataOnlyAndMissingOutputRemainsVisible(t *testing.T) {
	g := resultTestGovernor(t)
	payload := []byte(strings.Repeat("x", observation.MaxRetainedInput+1))
	action := harvest.LifecycleAction{ObservationID: "act_over_budget_0123456789abcdef",
		Runtime: "codex", SourceKind: "vendor-transcript", SourceRef: "/tmp/rollout.jsonl",
		SourceSequence: "1:0", SourceDigest: observation.DigestBytes([]byte("record")),
		NativeCallID: "call_over", NativeCallKind: "call_id", Tool: "exec",
		MediaType: "text/plain; charset=utf-8", RawBytes: len(payload),
		DecodedBytes: len(payload), InputDigest: observation.DigestBytes(payload),
		Completeness: "complete", InputPayload: payload}
	if err := appendLifecycleBatch(g, "over-budget", "", harvest.LifecycleBatch{Actions: []harvest.LifecycleAction{action}}); err != nil {
		t.Fatal(err)
	}
	_, input, found, err := g.ix.ObservationEvidence(action.ObservationID)
	if err != nil || !found || input.Completeness != "metadata-only" || input.CapturedBytes != 0 ||
		len(input.Payload) != 0 || input.RawBytes != len(payload) || input.Digest != action.InputDigest {
		t.Fatalf("bounded input=%+v found=%v err=%v", input, found, err)
	}
	missing, err := g.ix.MissingResultsForSession("over-budget")
	if err != nil || len(missing) != 1 || missing[0].NativeCallID != "call_over" {
		t.Fatalf("missing transcript output=%+v err=%v", missing, err)
	}
}

func TestActionBackfillSkipsUnavailableCandidateAndPacesContinuation(t *testing.T) {
	g := resultTestGovernor(t)
	coordinator := newLifecycleCoordinator(g)
	dir := t.TempDir()
	missingPath := filepath.Join(dir, "missing.jsonl")
	availablePath := filepath.Join(dir, "available.jsonl")
	if err := os.WriteFile(availablePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for index, fixture := range []struct {
		session string
		path    string
		at      int64
	}{{"available", availablePath, 1}, {"missing", missingPath, 2}} {
		if err := g.ix.UpsertTranscriptCursor(store.TranscriptCursor{Runtime: "codex",
			SourceRef: fixture.path, SourceSegmentID: lifecycleSegment(fixture.path),
			SessionID: fixture.session, FileSize: 3, GenerationDigest: "sha256-v1:g",
			CommittedOffset: 3, SourceLine: 1, ActionParserVersion: 0, UpdatedAt: fixture.at}); err != nil {
			t.Fatal(err)
		}
		_, _, err := g.ix.AppendResultObservation(store.ResultObservation{
			ObservationID: fmt.Sprintf("res_backfill_%d", index), SessionID: fixture.session,
			Runtime: "codex", NativeCallID: fmt.Sprintf("call_%d", index), NativeCallKind: "call_id",
			SourceKind: "vendor-transcript", SourceRef: fixture.path,
			SourceDigest: fmt.Sprintf("sha256-v1:%d", index), State: "success",
			Completeness: "metadata-only"}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	coordinator.startActionBackfill()
	claimed, found, err := g.ix.TranscriptCursor("codex", availablePath, lifecycleSegment(availablePath))
	if err != nil || !found || claimed.ActionParserVersion != -1 || claimed.CommittedOffset != 0 ||
		!claimed.RescanNeeded || !claimed.ContinuationNeeded {
		t.Fatalf("claimed cursor=%+v found=%v err=%v", claimed, found, err)
	}
	unavailable, _, _ := g.ix.TranscriptCursor("codex", missingPath, lifecycleSegment(missingPath))
	if unavailable.ActionParserVersion != 0 {
		t.Fatalf("unavailable cursor was claimed: %+v", unavailable)
	}
	if err := os.WriteFile(missingPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator.startActionBackfill()
	unavailable, _, _ = g.ix.TranscriptCursor("codex", missingPath, lifecycleSegment(missingPath))
	if unavailable.ActionParserVersion != 0 {
		t.Fatalf("second source was admitted while one repair is active: %+v", unavailable)
	}
	// A historical continuation waits in the delayed map and cannot immediately fill
	// the worker queue.
	work := store.LiveSessionRuntime{SessionID: "available", Runtime: "codex", TranscriptPath: availablePath}
	if err := coordinator.delay(work, time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(coordinator.queue) != 1 { // the one claim from startActionBackfill
		t.Fatalf("unexpected immediate continuation queue=%d", len(coordinator.queue))
	}
	coordinator.admitDelayed(time.Now().Add(2 * time.Hour))
	if len(coordinator.queue) != 1 { // coalesced while the claimed work is still queued
		t.Fatalf("delayed continuation bypassed coalescing queue=%d", len(coordinator.queue))
	}
	if err := os.Remove(availablePath); err != nil {
		t.Fatal(err)
	}
	coordinator.startActionBackfill()
	parked, _, _ := g.ix.TranscriptCursor("codex", availablePath, lifecycleSegment(availablePath))
	next, _, _ := g.ix.TranscriptCursor("codex", missingPath, lifecycleSegment(missingPath))
	if parked.ActionParserVersion != -2 || next.ActionParserVersion != -1 {
		t.Fatalf("unavailable active source starved next repair parked=%+v next=%+v", parked, next)
	}
}

func TestCodexActionBackfillReachesCompleteBoundaryWithoutCheckpointAmplification(t *testing.T) {
	g := resultTestGovernor(t)
	transcript := filepath.Join(t.TempDir(), "rollout-repair.jsonl")
	body := `{"timestamp":"2026-08-24T10:00:00Z","payload":{"type":"session_meta","payload":{"session_id":"repair-session"}}}
{"timestamp":"2026-08-24T10:00:01Z","payload":{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_repair","name":"exec","input":"read-only"}}}
{"timestamp":"2026-08-24T10:00:02Z","payload":{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_repair","output":"done"}}}
`
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, _, err := harvest.Lifecycle("codex", transcript)
	if err != nil || len(batch.Results) != 1 {
		t.Fatalf("fixture batch=%+v err=%v", batch, err)
	}
	r := batch.Results[0]
	resultID, _, err := g.ix.AppendResultObservation(store.ResultObservation{
		ObservationID: r.ObservationID, SessionID: "repair-session", Runtime: r.Runtime,
		Tool: r.Tool, NativeCallID: r.NativeCallID, NativeCallKind: r.NativeCallKind,
		SourceKind: r.SourceKind, SourceRef: r.SourceRef, SourceSequence: r.SourceSequence,
		SourceDigest: r.SourceDigest, SourceSegmentID: r.SourceSegmentID, State: r.State,
		CompletedAt: time.Now().Add(-time.Hour).Unix(), RawBytes: r.RawBytes,
		RawFieldBytes: r.RawBytes, DecodedBytes: r.DecodedBytes, PayloadDigest: r.PayloadDigest,
		Completeness: r.Completeness, DeliveryAttempts: 1, DeliveryMode: "transcript",
		ReceivedAt: time.Now().Unix()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(transcript)
	segment := lifecycleSegment(transcript)
	if err := g.ix.UpsertTranscriptCursor(store.TranscriptCursor{Runtime: "codex", SourceRef: transcript,
		SourceSegmentID: segment, SessionID: "repair-session", FileSize: info.Size(),
		FileMTime: info.ModTime().UnixNano(), GenerationDigest: mustTranscriptGeneration(t, transcript),
		CommittedOffset: info.Size(), SourceLine: 3, ActionParserVersion: 0,
		UpdatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	claimed, err := g.ix.BeginTranscriptActionBackfill("codex", transcript, segment, 1, time.Now().Unix())
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := reconcileLifecycleSessions(context.Background(), g, []store.LiveSessionRuntime{{
		SessionID: "repair-session", Runtime: "codex", TranscriptPath: transcript}}); err != nil {
		t.Fatal(err)
	}
	cursor, found, err := g.ix.TranscriptCursor("codex", transcript, segment)
	if err != nil || !found || cursor.ActionParserVersion != 1 || cursor.RescanNeeded || cursor.ContinuationNeeded {
		t.Fatalf("completed cursor=%+v found=%v err=%v", cursor, found, err)
	}
	reconciliation, err := g.ix.ReconcileResultObservation(resultID, time.Now().Unix())
	if err != nil || reconciliation.JoinClass != "exact" || len(reconciliation.Candidates) != 1 {
		t.Fatalf("reconciliation=%+v err=%v", reconciliation, err)
	}
	checkpoints, err := g.ix.SessionCheckpoints("repair-session", 10)
	if err != nil || len(checkpoints) != 0 {
		t.Fatalf("repair checkpoints=%+v err=%v", checkpoints, err)
	}
}

func TestCodexActionBackfillCollisionIsParkedInsteadOfRetried(t *testing.T) {
	g := resultTestGovernor(t)
	transcript := filepath.Join(t.TempDir(), "rollout-collision.jsonl")
	body := `{"timestamp":"2026-08-24T10:00:00Z","payload":{"type":"session_meta","payload":{"session_id":"collision-session"}}}
{"timestamp":"2026-08-24T10:00:01Z","payload":{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_collision","name":"exec","input":"read-only"}}}
`
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, _, err := harvest.Lifecycle("codex", transcript)
	if err != nil || len(batch.Actions) != 1 {
		t.Fatalf("fixture batch=%+v err=%v", batch, err)
	}
	action := batch.Actions[0]
	_, err = g.ObserveV1(Observation{SessionID: "collision-session", Runtime: "codex",
		Tool: action.Tool, TS: time.Now().Unix(), Origin: "transcript"}, ObservationEvidence{
		Delivery: store.EventDelivery{ObservationID: action.ObservationID,
			ObservationSchema: lifecycleTranscriptActionSchema, EnvelopeDigest: "sha256-v1:different",
			CollectorID: "harvest-lifecycle", NativeCallID: action.NativeCallID,
			NativeCallKind: action.NativeCallKind, ReceivedAt: time.Now().Unix(), DeliveryAttempts: 1,
			DeliveryMode: "transcript"}, Input: store.EventInput{Completeness: "unavailable"}})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(transcript)
	segment := lifecycleSegment(transcript)
	if err := g.ix.UpsertTranscriptCursor(store.TranscriptCursor{Runtime: "codex", SourceRef: transcript,
		SourceSegmentID: segment, SessionID: "collision-session", FileSize: info.Size(),
		FileMTime: info.ModTime().UnixNano(), GenerationDigest: mustTranscriptGeneration(t, transcript),
		ActionParserVersion: -1, RescanNeeded: true, ContinuationNeeded: true,
		UpdatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := reconcileLifecycleSessions(context.Background(), g, []store.LiveSessionRuntime{{
		SessionID: "collision-session", Runtime: "codex", TranscriptPath: transcript}}); err != nil {
		t.Fatal(err)
	}
	cursor, _, err := g.ix.TranscriptCursor("codex", transcript, segment)
	if err != nil || cursor.ActionParserVersion != -2 || cursor.RescanNeeded || cursor.ContinuationNeeded {
		t.Fatalf("collision cursor=%+v err=%v", cursor, err)
	}
	issues, err := g.ix.CollectionIssuesForSession("collision-session", 10)
	if err != nil || len(issues) != 1 || issues[0].Kind != "collision" || issues[0].ResolvedAt != 0 {
		t.Fatalf("collision issues=%+v err=%v", issues, err)
	}
}

func mustTranscriptGeneration(t *testing.T, path string) string {
	t.Helper()
	digest, err := transcriptGenerationDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestLifecycleSlotBoundsConcurrentReconciliation(t *testing.T) {
	g := resultTestGovernor(t)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withLifecycleSlot(context.Background(), g, func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ran := false
	err := withLifecycleSlot(ctx, g, func() error { ran = true; return nil })
	if err != context.DeadlineExceeded || ran {
		t.Fatalf("second reconciliation err=%v ran=%v", err, ran)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestResultBeforeAttemptReconcilesAndResolvesUnjoinedGap(t *testing.T) {
	g := resultTestGovernor(t)
	if receipt, err := ingestResultV1(g, validResultEnvelope("late-session", "late-call")); err != nil || receipt.JoinClass != "unjoined" {
		t.Fatalf("early result receipt=%+v err=%v", receipt, err)
	}
	issues, err := g.ix.CollectionIssuesForSession("late-session", 10)
	if err != nil || len(issues) != 1 || issues[0].Kind != "result-unjoined" || issues[0].ResolvedAt != 0 {
		t.Fatalf("early issues=%+v err=%v", issues, err)
	}
	envelope := observation.Envelope{Schema: observation.SchemaV1,
		ObservationID: "obs_abcdef0123456789abcdef0123456789", CollectorID: observation.CollectorPreTool,
		NativeCallID: "late-call", NativeCallKind: "tool_use_id", SessionID: "late-session",
		Runtime: "claude", Tool: "Edit", Decision: "allow", ToolInputCompleteness: "unavailable",
		TS: time.Now().Unix(), QueuedAt: time.Now().Unix(), DeliveryAttempts: 1, DeliveryMode: "direct"}
	if _, err := ingestObservationV1(context.Background(), g, envelope); err != nil {
		t.Fatal(err)
	}
	issues, err = g.ix.CollectionIssuesForSession("late-session", 10)
	resolved := false
	for _, issue := range issues {
		if issue.Kind == "result-unjoined" && issue.ResolvedAt != 0 {
			resolved = true
		}
	}
	if err != nil || !resolved {
		t.Fatalf("resolved issues=%+v err=%v", issues, err)
	}
	stats, err := g.ix.ResultStatsForSession("late-session")
	if err != nil || stats.Exact != 1 || stats.Unjoined != 0 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}
