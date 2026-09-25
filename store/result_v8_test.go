package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func requireQueryPlanUses(t *testing.T, ix *Index, query, index string, args ...any) {
	t.Helper()
	rows, err := ix.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	details := []string{}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
		if strings.Contains(detail, index) {
			return
		}
	}
	t.Fatalf("query plan did not use %s: %v", index, details)
}

func openResultTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func appendResultTestEvent(t *testing.T, ix *Index, session, tool, nativeID string, at int64) int64 {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(EventRecord{TS: at, SessionID: session, Runtime: "codex",
		Tool: tool, Verb: "write", Decision: "allow", Origin: "live"}, nil)
	if err == nil {
		err = tx.AppendEventDelivery(EventDelivery{EventID: eventID,
			ObservationID: fmt.Sprintf("obs-%s-%d", nativeID, eventID), ObservationSchema: "pretool-observation-v1",
			EnvelopeDigest: fmt.Sprintf("sha256-v1:%d", eventID), CollectorID: "guardcli-pretool",
			NativeCallID: nativeID, NativeCallKind: "tool_use_id", ReceivedAt: at,
			DeliveryAttempts: 1, DeliveryMode: "direct"})
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	return eventID
}

func TestExactResultObservationPageIsBoundedAndReportsExactTotal(t *testing.T) {
	ix := openResultTestIndex(t)
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "bounded-results",
		Runtime: "opaque-r1", Tool: "native-edit", Verb: "write", Decision: "allow", Origin: "live"}, nil)
	if err == nil {
		err = tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs-bounded-results",
			ObservationSchema: "test", EnvelopeDigest: "sha256-v1:event", CollectorID: "test",
			NativeCallID: "call-bounded-results", NativeCallKind: "opaque-call-id",
			ReceivedAt: 1, DeliveryAttempts: 1, DeliveryMode: "direct"})
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		observationID := fmt.Sprintf("result-bounded-%d", index)
		resultID, duplicate, appendErr := ix.AppendResultObservation(ResultObservation{
			ObservationID: observationID, SessionID: "bounded-results", Runtime: "opaque-r1",
			Tool: "native-edit", NativeCallID: "call-bounded-results", NativeCallKind: "opaque-call-id",
			SourceKind: "live-post-tool", SourceDigest: "sha256-v1:" + observationID,
			State: "success", CompletedAt: int64(index + 2), Completeness: "metadata-only",
			PayloadDigest: "sha256-v1:payload-" + observationID}, nil)
		if appendErr != nil || duplicate {
			t.Fatalf("append result %d duplicate=%v err=%v", resultID, duplicate, appendErr)
		}
		if _, reconcileErr := ix.ReconcileResultObservation(resultID, int64(index+10)); reconcileErr != nil {
			t.Fatal(reconcileErr)
		}
	}
	page, total, err := ix.ExactResultObservationIDsForEventPage(eventID, 2)
	if err != nil || len(page) != 2 || total != 5 {
		t.Fatalf("page=%v total=%d err=%v", page, total, err)
	}
	all, err := ix.ExactResultObservationIDsForEvent(eventID)
	if err != nil || len(all) != 5 {
		t.Fatalf("all=%v err=%v", all, err)
	}
}

type equivalentEventFixture struct {
	TS, CollectorVersion                  int64
	CollectorID, InputDigest, ResourceRaw string
	SkipInput                             bool
}

func appendEquivalentEvent(t *testing.T, ix *Index, session, nativeID string,
	fixture equivalentEventFixture) int64 {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(EventRecord{TS: fixture.TS, SessionID: session,
		Runtime: "codex", Verb: "write", Tool: "apply_patch", TargetEntityID: "file:/repo/a.go",
		Tags: `[{"key":"phase","value":"test"}]`, Decision: "allow", Reason: "test",
		Origin: "live"}, nil)
	if err == nil {
		err = tx.AppendEventDelivery(EventDelivery{EventID: eventID,
			ObservationID: fmt.Sprintf("obs-equivalent-%d", eventID), ObservationSchema: "pretool-observation-v1",
			EnvelopeDigest: fmt.Sprintf("sha256-v1:envelope-%d", eventID), CollectorID: fixture.CollectorID,
			CollectorVersion: fmt.Sprintf("v%d", fixture.CollectorVersion), NativeCallID: nativeID,
			NativeCallKind: "tool_use_id", ReceivedAt: fixture.TS, DeliveryAttempts: 1,
			DeliveryMode: "direct"})
	}
	if err == nil && !fixture.SkipInput {
		err = tx.AppendEventInput(EventInput{EventID: eventID, MediaType: "application/json",
			RawBytes: 2, CapturedBytes: 0, Digest: fixture.InputDigest,
			Completeness: "metadata-only", SourceRef: "/tmp/session.jsonl"})
	}
	if err == nil {
		err = tx.UpsertEntity("file:/repo/a.go", "file", "/repo/a.go", fixture.TS)
	}
	if err == nil {
		err = tx.AppendEventResourceEvidence(EventResourceEvidence{EventID: eventID, Ordinal: 0,
			EntityID: "file:/repo/a.go", Source: "structured", RawIdentity: fixture.ResourceRaw,
			Operation: "patch", EvidenceClass: "declared", SourceField: "tool_input.patch",
			Completeness: "complete"})
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	return eventID
}

func TestSchemaV12EquivalentActionDeliveriesSelectStableRepresentative(t *testing.T) {
	ix := openResultTestIndex(t)
	fixture := equivalentEventFixture{TS: 10, CollectorVersion: 1, CollectorID: "guardcli-pretool",
		InputDigest: "sha256-v1:input", ResourceRaw: "src/a.go"}
	first := appendEquivalentEvent(t, ix, "equivalent", "native-equivalent", fixture)
	second := appendEquivalentEvent(t, ix, "equivalent", "native-equivalent", fixture)
	resultID := appendCoverageResult(t, ix, "res-equivalent", "equivalent", "native-equivalent",
		"vendor-transcript", 11)
	reconciliation, err := ix.ReconcileResultObservation(resultID, 20)
	if err != nil || reconciliation.JoinClass != "exact" ||
		reconciliation.Algorithm != "native-id-duplicate-delivery-v1" || len(reconciliation.Candidates) != 2 {
		t.Fatalf("equivalent reconciliation=%+v err=%v", reconciliation, err)
	}
	selected := int64(0)
	for _, candidate := range reconciliation.Candidates {
		if candidate.Selected {
			selected = candidate.EventID
		}
	}
	if selected != first || second <= first {
		t.Fatalf("selected=%d first=%d second=%d", selected, first, second)
	}
	var events int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM event WHERE session_id='equivalent'`).Scan(&events); err != nil || events != 2 {
		t.Fatalf("raw events=%d err=%v", events, err)
	}
	candidates, err := ix.AmbiguousResultRepairCandidates(256)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("repaired candidate still qualifies=%+v err=%v", candidates, err)
	}
}

func TestSchemaV12ActionDeliveryDifferencesRemainAmbiguous(t *testing.T) {
	base := equivalentEventFixture{TS: 10, CollectorVersion: 1, CollectorID: "guardcli-pretool",
		InputDigest: "sha256-v1:input", ResourceRaw: "src/a.go"}
	for _, test := range []struct {
		name   string
		change func(*equivalentEventFixture)
	}{
		{"event", func(f *equivalentEventFixture) { f.TS++ }},
		{"delivery", func(f *equivalentEventFixture) { f.CollectorVersion++ }},
		{"input", func(f *equivalentEventFixture) { f.InputDigest = "sha256-v1:different" }},
		{"resource", func(f *equivalentEventFixture) { f.ResourceRaw = "src/different.go" }},
		{"missing-input", func(f *equivalentEventFixture) { f.SkipInput = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ix := openResultTestIndex(t)
			appendEquivalentEvent(t, ix, test.name, "native-"+test.name, base)
			changed := base
			test.change(&changed)
			appendEquivalentEvent(t, ix, test.name, "native-"+test.name, changed)
			resultID := appendCoverageResult(t, ix, "res-"+test.name, test.name,
				"native-"+test.name, "vendor-transcript", 20)
			reconciliation, err := ix.ReconcileResultObservation(resultID, 30)
			if err != nil || reconciliation.JoinClass != "ambiguous" {
				t.Fatalf("difference %s reconciliation=%+v err=%v", test.name, reconciliation, err)
			}
		})
	}
}

func TestSchemaV8StoresResultBeforeActionAndReconcilesLater(t *testing.T) {
	ix := openResultTestIndex(t)
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	result := ResultObservation{ObservationID: "res-1", SessionID: "s", Runtime: "codex",
		NativeCallID: "call-1", NativeCallKind: "call_id", Tool: "apply_patch",
		SourceKind: "vendor-patch-result", SourceRef: "rollout.jsonl", SourceSequence: "2:0",
		SourceDigest: "sha256-v1:result", SourceSegmentID: "rollout-1", State: "success",
		RawBytes: 10, DecodedBytes: 5, PayloadDigest: "sha256-v1:payload", Completeness: "metadata-only"}
	id, duplicate, err := ix.AppendResultObservation(result, []ResultEffect{{Ordinal: 0,
		RawIdentity: "src/a.go", Operation: "update", EvidenceSource: "runtime-result",
		SourceField: "changes", Completeness: "complete", DiffBytes: 4,
		DiffDigest: retainedBodyDigest([]byte("diff")), DiffCompleteness: "complete", DiffPayload: []byte("diff")}})
	if err != nil || duplicate || id == 0 {
		t.Fatalf("append result: id=%d duplicate=%v err=%v", id, duplicate, err)
	}
	joined, err := ix.ReconcileResultObservation(id, 10)
	if err != nil || joined.JoinClass != "unjoined" || len(joined.Candidates) != 0 {
		t.Fatalf("early reconcile=%#v err=%v", joined, err)
	}

	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(EventRecord{TS: 11, SessionID: "s", Runtime: "codex", Tool: "apply_patch", Origin: "live"}, nil)
	if err == nil {
		err = tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs-1",
			ObservationSchema: "pretool-observation-v1", EnvelopeDigest: "sha256-v1:event",
			CollectorID: "guardcli-pretool", NativeCallID: "call-1", NativeCallKind: "call_id",
			ReceivedAt: 11, DeliveryAttempts: 1, DeliveryMode: "direct"})
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	joined, err = ix.ReconcileResultObservation(id, 12)
	if err != nil || joined.JoinClass != "exact" || len(joined.Candidates) != 1 || !joined.Candidates[0].Selected {
		t.Fatalf("late reconcile=%#v err=%v", joined, err)
	}
}

func appendCoverageResult(t *testing.T, ix *Index, observationID, session, nativeID,
	sourceKind string, completedAt int64) int64 {
	t.Helper()
	id, duplicate, err := ix.AppendResultObservation(ResultObservation{ObservationID: observationID,
		SessionID: session, Runtime: "codex", Tool: "apply_patch", NativeCallID: nativeID,
		NativeCallKind: "tool_use_id", SourceKind: sourceKind, SourceDigest: "sha256-v1:" + observationID,
		State: "success", CompletedAt: completedAt, Completeness: "metadata-only",
		PayloadDigest: "sha256-v1:payload-" + observationID}, nil)
	if err != nil || duplicate {
		t.Fatalf("append coverage result id=%d duplicate=%v err=%v", id, duplicate, err)
	}
	if _, err := ix.ReconcileResultObservation(id, completedAt+1); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSchemaV11ClassifiesResultActionCoverageWithoutTimestampJoining(t *testing.T) {
	ix := openResultTestIndex(t)
	noBoundary := appendCoverageResult(t, ix, "res-no-boundary", "coverage-no-boundary",
		"missing-a", "vendor-transcript", 10)
	if got, err := ix.ResultActionCoverage(noBoundary); err != nil || got.Class != "no-action-boundary" {
		t.Fatalf("no boundary coverage=%+v err=%v", got, err)
	}

	appendResultTestEvent(t, ix, "coverage-window", "apply_patch", "observed", 20)
	before := appendCoverageResult(t, ix, "res-before", "coverage-window", "missing-b",
		"vendor-transcript", 10)
	after := appendCoverageResult(t, ix, "res-after", "coverage-window", "missing-c",
		"vendor-transcript", 30)
	exact := appendCoverageResult(t, ix, "res-exact", "coverage-window", "observed",
		"vendor-transcript", 21)
	for _, check := range []struct {
		id   int64
		want string
	}{{before, "before-action-boundary"}, {after, "expected-action-missing"}, {exact, "action-observed"}} {
		got, err := ix.ResultActionCoverage(check.id)
		if err != nil || got.Class != check.want || got.BoundaryAt != 20 {
			t.Fatalf("coverage id=%d got=%+v want=%s err=%v", check.id, got, check.want, err)
		}
	}

	direct := appendCoverageResult(t, ix, "res-direct", "coverage-direct", "missing-direct",
		"live-post-tool", 0)
	if got, err := ix.ResultActionCoverage(direct); err != nil || got.Class != "expected-action-missing" {
		t.Fatalf("direct coverage=%+v err=%v", got, err)
	}
	stats, err := ix.ResultStatsForSession("coverage-window")
	if err != nil || stats.ActionObserved != 1 || stats.BeforeBoundary != 1 ||
		stats.ExpectedMissing != 1 || stats.Unjoined != 2 || stats.Exact != 1 {
		t.Fatalf("coverage stats=%+v err=%v", stats, err)
	}
	results, err := ix.ResultsForSession("coverage-window", 10)
	if err != nil || len(results) != 3 {
		t.Fatalf("coverage results=%+v err=%v", results, err)
	}
	for _, result := range results {
		if result.ActionCoverageClass == "" || result.ActionBoundaryAt != 20 {
			t.Fatalf("result coverage missing: %+v", result)
		}
	}
}

func TestSchemaV11MigrationKeepsOnlyPostBoundaryUnjoinedIssueActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	appendResultTestEvent(t, ix, "migration-coverage", "apply_patch", "observed", 20)
	before := appendCoverageResult(t, ix, "res-migration-before", "migration-coverage",
		"missing-before", "vendor-transcript", 10)
	after := appendCoverageResult(t, ix, "res-migration-after", "migration-coverage",
		"missing-after", "vendor-transcript", 30)
	for _, issue := range []struct {
		id  int64
		obs string
	}{{before, "res-migration-before"}, {after, "res-migration-after"}} {
		_ = issue.id
		if err := ix.RecordCollectionIssue(CollectionIssue{IssueID: "result-unjoined:" + issue.obs,
			SessionID: "migration-coverage", Runtime: "codex", ObservationID: issue.obs,
			CollectorID: "test", Kind: "result-unjoined", FirstSeen: 1, LastSeen: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.db.Exec(`DROP INDEX collection_issue_active;
		ALTER TABLE collection_issue DROP COLUMN resolution_class;
		PRAGMA user_version=10`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	issues, err := ix.CollectionIssuesForSession("migration-coverage", 10)
	if err != nil || len(issues) != 2 {
		t.Fatalf("migrated issues=%+v err=%v", issues, err)
	}
	byObservation := map[string]CollectionIssue{}
	for _, issue := range issues {
		byObservation[issue.ObservationID] = issue
	}
	if got := byObservation["res-migration-before"]; got.ResolutionClass != "outside-observation" || got.ResolvedAt == 0 {
		t.Fatalf("before-boundary issue=%+v", got)
	}
	if got := byObservation["res-migration-after"]; got.ResolutionClass != "" || got.ResolvedAt != 0 {
		t.Fatalf("post-boundary issue=%+v", got)
	}
}

func TestSchemaV8ResultReplayIsIdempotentAndCollisionFails(t *testing.T) {
	ix := openResultTestIndex(t)
	r := ResultObservation{ObservationID: "res-1", SessionID: "s", Runtime: "claude",
		SourceKind: "live-post-tool", SourceRef: "hook", SourceSequence: "1", SourceDigest: "sha256-v1:a",
		SourceSegmentID: "segment", State: "success", Completeness: "metadata-only"}
	first, duplicate, err := ix.AppendResultObservation(r, nil)
	if err != nil || duplicate {
		t.Fatalf("first append: %d %v %v", first, duplicate, err)
	}
	second, duplicate, err := ix.AppendResultObservation(r, nil)
	if err != nil || !duplicate || second != first {
		t.Fatalf("replay: %d %v %v", second, duplicate, err)
	}
	r.SourceDigest = "sha256-v1:different"
	if _, _, err := ix.AppendResultObservation(r, nil); err != ErrResultObservationCollision {
		t.Fatalf("collision err=%v", err)
	}
}

func TestSchemaV8KeepsSourceAndLogicalResultCountsSeparate(t *testing.T) {
	ix := openResultTestIndex(t)
	for i, source := range []string{"live-post-tool", "vendor-transcript"} {
		resultID, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res-" + source,
			SessionID: "s", Runtime: "claude", NativeCallID: "toolu-1", NativeCallKind: "tool_use_id",
			SourceKind: source, SourceRef: source, SourceSequence: "1", SourceDigest: "sha256-v1:" + source,
			SourceSegmentID: "segment", State: "success", CompletedAt: int64(i + 1), Completeness: "metadata-only"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := ix.RelateLogicalResults(resultID, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := ix.ResultStatsForSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if stats.SourceObserved != 2 || stats.LogicalCompletions != 1 {
		t.Fatalf("stats=%#v", stats)
	}
}

func TestSchemaV9SeparatesPrimaryResultFromFirstLogicalCompletion(t *testing.T) {
	ix := openResultTestIndex(t)
	ids := []int64{}
	for index, source := range []string{"live-post-tool", "vendor-transcript"} {
		id, duplicate, err := ix.AppendResultObservation(ResultObservation{
			ObservationID: "res-logical-" + source, SessionID: "logical", Runtime: "claude",
			NativeCallID: "toolu-logical", NativeCallKind: "tool_use_id", SourceKind: source,
			SourceRef: source, SourceSequence: "1", SourceDigest: "sha256-v1:" + source,
			SourceSegmentID: "segment", State: "success", CompletedAt: int64(index + 1),
			Completeness: "metadata-only"}, nil)
		if err != nil || duplicate {
			t.Fatalf("append %s id=%d duplicate=%v err=%v", source, id, duplicate, err)
		}
		if err := ix.RelateLogicalResults(id, int64(index+1)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	first, err := ix.IsFirstLogicalCompletion(ids[0])
	if err != nil || !first {
		t.Fatalf("first logical=%v err=%v", first, err)
	}
	second, err := ix.IsFirstLogicalCompletion(ids[1])
	if err != nil || second {
		t.Fatalf("second logical=%v err=%v", second, err)
	}
}

func TestSchemaV10CodexPatchAliasReconcilesWithoutRewritingRawIdentity(t *testing.T) {
	ix := openResultTestIndex(t)
	eventID := appendResultTestEvent(t, ix, "codex-patch", "apply_patch", "exec-exact", 1)
	resultID, duplicate, err := ix.AppendResultObservation(ResultObservation{
		ObservationID: "res-codex-patch", SessionID: "codex-patch", Runtime: "codex",
		Tool: "apply_patch", NativeCallID: "exec-exact", NativeCallKind: "patch_call_id",
		SourceKind: "vendor-patch-result", SourceRef: "rollout.jsonl", SourceSequence: "1:0",
		SourceDigest: "sha256-v1:patch", SourceSegmentID: "rollout", State: "success",
		Completeness: "metadata-only"}, nil)
	if err != nil || duplicate {
		t.Fatalf("append result id=%d duplicate=%v err=%v", resultID, duplicate, err)
	}
	before, err := ix.ReconcileResultObservation(resultID, 2)
	if err != nil || before.JoinClass != "unjoined" {
		t.Fatalf("before alias=%+v err=%v", before, err)
	}
	alias := ResultNativeCallAlias{ResultID: resultID, NativeCallKind: "tool_use_id",
		NativeCallID: "exec-exact", Algorithm: "codex-patch-exec-id-v1"}
	if duplicate, err := ix.AppendResultNativeCallAlias(alias); err != nil || duplicate {
		t.Fatalf("append alias duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, err := ix.AppendResultNativeCallAlias(alias); err != nil || !duplicate {
		t.Fatalf("replay alias duplicate=%v err=%v", duplicate, err)
	}
	conflict := alias
	conflict.Algorithm = "different"
	if _, err := ix.AppendResultNativeCallAlias(conflict); err != ErrResultAliasCollision {
		t.Fatalf("alias collision err=%v", err)
	}
	after, err := ix.ReconcileResultObservation(resultID, 3)
	if err != nil || after.JoinClass != "exact" || after.Algorithm != "native-id-alias-v1" ||
		len(after.Candidates) != 1 || after.Candidates[0].EventID != eventID || !after.Candidates[0].Selected ||
		!strings.Contains(after.Candidates[0].CandidateReason, "codex-patch-exec-id-v1") {
		t.Fatalf("after alias=%+v err=%v", after, err)
	}
	var rawKind, rawID string
	if err := ix.db.QueryRow(`SELECT native_call_kind,native_call_id FROM result_observation WHERE id=?`, resultID).
		Scan(&rawKind, &rawID); err != nil || rawKind != "patch_call_id" || rawID != "exec-exact" {
		t.Fatalf("raw identity kind=%q id=%q err=%v", rawKind, rawID, err)
	}
}

func TestSchemaV10AliasRelatesDirectAndPatchSourcesAndCountsOneLogicalCompletion(t *testing.T) {
	ix := openResultTestIndex(t)
	patchID, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res-patch",
		SessionID: "logical-alias", Runtime: "codex", Tool: "apply_patch", NativeCallID: "exec-logical",
		NativeCallKind: "patch_call_id", SourceKind: "vendor-patch-result", SourceDigest: "sha256-v1:patch",
		State: "success", Completeness: "metadata-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.AppendResultNativeCallAlias(ResultNativeCallAlias{ResultID: patchID,
		NativeCallKind: "tool_use_id", NativeCallID: "exec-logical", Algorithm: "codex-patch-exec-id-v1"}); err != nil {
		t.Fatal(err)
	}
	directID, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res-direct",
		SessionID: "logical-alias", Runtime: "codex", Tool: "apply_patch", NativeCallID: "exec-logical",
		NativeCallKind: "tool_use_id", SourceKind: "live-post-tool", SourceDigest: "sha256-v1:direct",
		State: "success", Completeness: "metadata-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.RelateLogicalResults(patchID, 1); err != nil {
		t.Fatal(err)
	}
	if err := ix.RelateLogicalResults(directID, 2); err != nil {
		t.Fatal(err)
	}
	first, err := ix.IsFirstLogicalCompletion(patchID)
	if err != nil || !first {
		t.Fatalf("patch first=%v err=%v", first, err)
	}
	second, err := ix.IsFirstLogicalCompletion(directID)
	if err != nil || second {
		t.Fatalf("direct first=%v err=%v", second, err)
	}
	stats, err := ix.ResultStatsForSession("logical-alias")
	if err != nil || stats.SourceObserved != 2 || stats.LogicalCompletions != 1 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestSchemaV10CodexPatchRepairCandidatesRequireOneExactApplyPatchEvent(t *testing.T) {
	ix := openResultTestIndex(t)
	appendResultTestEvent(t, ix, "repair", "apply_patch", "exec-one", 1)
	appendResultTestEvent(t, ix, "repair", "apply_patch", "exec-many", 2)
	appendResultTestEvent(t, ix, "repair", "apply_patch", "exec-many", 3)
	appendResultTestEvent(t, ix, "repair", "Bash", "exec-shell", 4)
	for index, nativeID := range []string{"exec-one", "exec-many", "exec-shell"} {
		if _, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "repair-" + nativeID,
			SessionID: "repair", Runtime: "codex", NativeCallID: nativeID, NativeCallKind: "patch_call_id",
			SourceKind: "vendor-patch-result", SourceDigest: fmt.Sprintf("sha256-v1:%d", index),
			State: "success", Completeness: "metadata-only"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	contract := ResultAliasRepairContract{Runtime: "codex", ResultSourceKind: "vendor-patch-result",
		ResultNativeCallKind: "patch_call_id", AliasNativeCallKind: "tool_use_id", Tool: "apply_patch"}
	candidates, err := ix.ResultAliasRepairCandidates(contract, 256)
	if err != nil || len(candidates) != 1 || candidates[0].NativeCallID != "exec-one" {
		t.Fatalf("repair candidates=%+v err=%v", candidates, err)
	}
}

func TestSchemaV10MigratesV9IdentityLinkageWithoutPopulationDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	appendResultTestEvent(t, ix, "migration", "apply_patch", "exec-migration", 1)
	if _, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res-migration",
		SessionID: "migration", Runtime: "codex", NativeCallID: "exec-migration", NativeCallKind: "patch_call_id",
		SourceKind: "vendor-patch-result", SourceDigest: "sha256-v1:migration", State: "success",
		Completeness: "metadata-only"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP TABLE result_native_call_alias;
		DROP INDEX event_delivery_native; PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var version, events, results int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM result_observation`).Scan(&results); err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := ix.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || events != 1 || results != 1 || integrity != "ok" {
		t.Fatalf("version=%d events=%d results=%d integrity=%q", version, events, results, integrity)
	}
	contract := ResultAliasRepairContract{Runtime: "codex", ResultSourceKind: "vendor-patch-result",
		ResultNativeCallKind: "patch_call_id", AliasNativeCallKind: "tool_use_id", Tool: "apply_patch"}
	candidates, err := ix.ResultAliasRepairCandidates(contract, 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("migrated candidates=%+v err=%v", candidates, err)
	}
}

func TestSchemaV9TranscriptCursorRetainsCompleteBoundaryAndDurableRescan(t *testing.T) {
	ix := openResultTestIndex(t)
	state := []byte(`{"canonical":"session"}`)
	want := TranscriptCursor{Runtime: "codex", SourceRef: "/tmp/rollout.jsonl",
		SourceSegmentID: "rollout", SessionID: "session", WorkingDirectory: "/repo",
		FileSize: 200, FileMTime: 42, GenerationDigest: "sha256-v1:g",
		SourceSequence: "2:0", CommittedOffset: 150, SourceLine: 2, ParserState: state,
		ParserStateDigest: "sha256-v1:s", RescanNeeded: true, UpdatedAt: 99}
	if err := ix.UpsertTranscriptCursor(want); err != nil {
		t.Fatal(err)
	}
	got, found, err := ix.TranscriptCursor(want.Runtime, want.SourceRef, want.SourceSegmentID)
	if err != nil || !found || got.CommittedOffset != want.CommittedOffset || got.SourceLine != 2 ||
		string(got.ParserState) != string(state) || !got.RescanNeeded || got.SessionID != "session" {
		t.Fatalf("cursor=%+v found=%v err=%v", got, found, err)
	}
	rescans, err := ix.TranscriptRescanSources(10)
	if err != nil || len(rescans) != 1 || rescans[0].TranscriptPath != want.SourceRef {
		t.Fatalf("rescans=%+v err=%v", rescans, err)
	}
}

func TestSchemaV13TranscriptActionBackfillClaimsAndParksOneCursor(t *testing.T) {
	ix := openResultTestIndex(t)
	path := "/tmp/rollout-action.jsonl"
	if err := ix.UpsertTranscriptCursor(TranscriptCursor{Runtime: "codex", SourceRef: path,
		SourceSegmentID: "rollout-action", SessionID: "action-session", FileSize: 100,
		GenerationDigest: "sha256-v1:g", CommittedOffset: 100, SourceLine: 10,
		ActionParserVersion: 0, UpdatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res_action_backfill",
		SessionID: "action-session", Runtime: "codex", NativeCallID: "call_action",
		NativeCallKind: "call_id", SourceKind: "vendor-transcript", SourceRef: path,
		SourceDigest: "sha256-v1:r", State: "success", Completeness: "metadata-only"}, nil); err != nil {
		t.Fatal(err)
	}
	candidates, err := ix.TranscriptActionBackfillSources("codex", "vendor-transcript", "call_id", 1, 8)
	if err != nil || len(candidates) != 1 || candidates[0].TranscriptPath != path {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	claimed, err := ix.BeginTranscriptActionBackfill("codex", path, "rollout-action", 1, 20)
	if err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	claimedAgain, err := ix.BeginTranscriptActionBackfill("codex", path, "rollout-action", 1, 21)
	if err != nil || claimedAgain {
		t.Fatalf("second claim=%v err=%v", claimedAgain, err)
	}
	cursor, found, err := ix.TranscriptCursor("codex", path, "rollout-action")
	if err != nil || !found || cursor.ActionParserVersion != -1 || cursor.CommittedOffset != 0 ||
		cursor.SourceLine != 0 || !cursor.RescanNeeded || !cursor.ContinuationNeeded {
		t.Fatalf("in-progress cursor=%+v found=%v err=%v", cursor, found, err)
	}
	if err := ix.BlockTranscriptActionBackfill("codex", path, "rollout-action", 30); err != nil {
		t.Fatal(err)
	}
	cursor, _, err = ix.TranscriptCursor("codex", path, "rollout-action")
	if err != nil || cursor.ActionParserVersion != -2 || cursor.RescanNeeded || cursor.ContinuationNeeded {
		t.Fatalf("blocked cursor=%+v err=%v", cursor, err)
	}
	candidates, err = ix.TranscriptActionBackfillSources("codex", "vendor-transcript", "call_id", 1, 8)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("blocked cursor remained eligible candidates=%+v err=%v", candidates, err)
	}
}

func TestSchemaV9CheckpointBodiesAreContentAddressedAcrossCaptures(t *testing.T) {
	ix := openResultTestIndex(t)
	body := []byte("same checkpoint body")
	for index := 0; index < 2; index++ {
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "dedup",
			ScopeKey: "checkout:one", Kind: "settled", RequestID: fmt.Sprintf("dedup-%d", index),
			WorkingDirectory: "/repo", RepositoryID: "repo", CheckoutID: "checkout",
			CheckoutRoot: "/repo", Status: "pending", BoundaryClass: "settled", RequestedAt: int64(index + 1)})
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, won, err := ix.ClaimSessionCheckpoint(checkpoint.ID, int64(index+1), 0, "settled"); err != nil || !won {
			t.Fatalf("claim won=%v err=%v", won, err)
		}
		record := &ChangeRecord{SessionID: "dedup", RepositoryID: "repo", CheckoutID: "checkout",
			RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo", Kind: "revision",
			EvidenceClass: "observed", SourceKind: "git", SourceDigest: fmt.Sprintf("snapshot-%d", index),
			RecordedAt: int64(index + 1), CaptureStartedAt: int64(index + 1),
			CaptureEndedAt: int64(index + 2), BaseRevision: "base", HeadRevision: "head",
			SnapshotDigest: fmt.Sprintf("snapshot-%d", index)}
		payload := CheckpointPayload{Path: "src/a.go", Layer: "worktree", Status: "modified",
			ContentBytes: len(body), ContentDigest: retainedBodyDigest(body), ContentCompleteness: "complete",
			ContentPayload: body, PatchCompleteness: "unavailable"}
		if err := ix.CompleteSessionCheckpointWithPayload(checkpoint.ID, record, []CheckpointPayload{payload}, int64(index+2)); err != nil {
			t.Fatal(err)
		}
	}
	var bodies, refs, inlineBytes int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM evidence_body`).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM checkpoint_payload_body`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT COALESCE(SUM(length(content_payload)),0) FROM checkpoint_payload`).Scan(&inlineBytes); err != nil {
		t.Fatal(err)
	}
	if bodies != 1 || refs != 2 || inlineBytes != 0 {
		t.Fatalf("content addressing bodies=%d refs=%d inline=%d", bodies, refs, inlineBytes)
	}
}

func TestLifecycleQueriesTargetOneSessionAndExcludePrestartIdleHistory(t *testing.T) {
	ix := openResultTestIndex(t)
	for _, event := range []EventRecord{
		{TS: 100, SessionID: "historical", Runtime: "codex", Origin: "live"},
		{TS: 200, SessionID: "current", Runtime: "claude", Origin: "live"},
	} {
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.AppendEvent(event, nil); err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	targeted, err := ix.LiveSessionRuntimesForSession("current", 20)
	if err != nil || len(targeted) != 1 || targeted[0].SessionID != "current" {
		t.Fatalf("targeted=%+v err=%v", targeted, err)
	}
	idle, err := ix.IdleSessions(300, 150, 10)
	if err != nil || len(idle) != 1 || idle[0].SessionID != "current" {
		t.Fatalf("idle=%+v err=%v", idle, err)
	}
}

func TestLiveSessionRuntimesSelectsLatestTranscriptPerRuntime(t *testing.T) {
	ix := openResultTestIndex(t)
	for index, source := range []string{"/tmp/old.jsonl", "/tmp/current.jsonl"} {
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		eventID, err := tx.AppendEvent(EventRecord{TS: int64(index + 1), SessionID: "current",
			Runtime: "codex", Origin: "live"}, nil)
		if err == nil {
			err = tx.AppendEventInput(EventInput{EventID: eventID, MediaType: "application/json",
				Completeness: "metadata-only", Digest: fmt.Sprintf("sha256-v1:%d", index), SourceRef: source})
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, load := range map[string]func() ([]LiveSessionRuntime, error){
		"all":     func() ([]LiveSessionRuntime, error) { return ix.LiveSessionRuntimes(20) },
		"session": func() ([]LiveSessionRuntime, error) { return ix.LiveSessionRuntimesForSession("current", 20) },
	} {
		runtimes, err := load()
		if err != nil || len(runtimes) != 1 || runtimes[0].TranscriptPath != "/tmp/current.jsonl" {
			t.Fatalf("%s runtimes=%+v err=%v", name, runtimes, err)
		}
	}
}

func TestSchemaV10LifecycleAndIdentityQueriesUseBoundedOwnerIndexes(t *testing.T) {
	ix := openResultTestIndex(t)
	requireQueryPlanUses(t, ix, `SELECT id FROM event WHERE origin='live' AND session_id=?
		AND runtime=? ORDER BY ts DESC LIMIT 1`, "event_live_session_runtime_ts", "s", "codex")
	requireQueryPlanUses(t, ix, `SELECT session_id,runtime,source_ref,working_directory
		FROM transcript_cursor WHERE rescan_needed=1 AND session_id!='' AND source_ref!=''
		ORDER BY updated_at,runtime,source_ref LIMIT ?`, "transcript_cursor_rescan", 64)
	requireQueryPlanUses(t, ix, `SELECT id FROM session_checkpoint
		WHERE status IN ('pending','capturing') AND kind NOT IN ('attachment','pre-mutation')
		ORDER BY requested_at,id LIMIT ?`, "session_checkpoint_status", 2)
	requireQueryPlanUses(t, ix, `SELECT event_id FROM event_delivery
		WHERE native_call_kind=? AND native_call_id=?`, "event_delivery_native", "tool_use_id", "exec-1")
	requireQueryPlanUses(t, ix, `SELECT result_id FROM result_native_call_alias
		WHERE native_call_kind=? AND native_call_id=?`, "result_native_call_alias_lookup", "tool_use_id", "exec-1")
	requireQueryPlanUses(t, ix, `SELECT id FROM collection_issue
		WHERE kind=? AND resolved_at=? ORDER BY id LIMIT ?`, "collection_issue_active", "result-unjoined", 0, 256)
	requireQueryPlanUses(t, ix, `SELECT result_id FROM result_reconciliation
		WHERE join_class='ambiguous' ORDER BY result_id LIMIT ?`, "result_reconciliation_ambiguous", 256)
	actionBackfill := `SELECT c.session_id,c.runtime,c.source_ref,c.working_directory
		FROM transcript_cursor c
		WHERE c.action_parser_version>=0 AND c.action_parser_version<?
		AND c.runtime=? AND c.session_id!='' AND c.source_ref!=''
		AND EXISTS (SELECT 1 FROM result_observation r
			WHERE r.session_id=c.session_id AND r.runtime=c.runtime AND r.source_ref=c.source_ref
			AND r.source_kind=? AND r.native_call_kind=?)
		ORDER BY c.updated_at DESC,c.source_ref LIMIT ?`
	args := []any{1, "codex", "vendor-transcript", "call_id", 8}
	requireQueryPlanUses(t, ix, actionBackfill, "transcript_cursor_action_backfill", args...)
	requireQueryPlanUses(t, ix, actionBackfill, "result_observation_action_backfill", args...)
	requireQueryPlanUses(t, ix, `SELECT 1 FROM transcript_cursor
		WHERE action_parser_version=-1 LIMIT 1`, "transcript_cursor_action_active")
}

func TestSchemaV8ResultSelectsAllowedExecutionAfterAskHold(t *testing.T) {
	ix := openResultTestIndex(t)
	var allowEventID int64
	for index, decision := range []string{"ask", "allow"} {
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		eventID, err := tx.AppendEvent(EventRecord{TS: int64(index + 1), SessionID: "s", Runtime: "claude",
			Tool: "Edit", Decision: decision, Origin: "live"}, nil)
		if err == nil {
			err = tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs-" + decision,
				ObservationSchema: "pretool-observation-v1", EnvelopeDigest: "sha256-v1:" + decision,
				CollectorID: "guardcli-pretool", NativeCallID: "toolu-ask", NativeCallKind: "tool_use_id",
				ReceivedAt: int64(index + 1), DeliveryAttempts: 1, DeliveryMode: "direct"})
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		if decision == "allow" {
			allowEventID = eventID
		}
	}
	resultID, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res-ask",
		SessionID: "s", Runtime: "claude", Tool: "Edit", NativeCallID: "toolu-ask",
		NativeCallKind: "tool_use_id", SourceKind: "live-post-tool", SourceDigest: "sha256-v1:result",
		State: "success", Completeness: "metadata-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := ix.ReconcileResultObservation(resultID, 3)
	if err != nil || joined.JoinClass != "exact" {
		t.Fatalf("reconciliation = %+v err=%v", joined, err)
	}
	selected := int64(0)
	for _, candidate := range joined.Candidates {
		if candidate.Selected {
			selected = candidate.EventID
		}
	}
	if selected != allowEventID {
		t.Fatalf("selected event=%d want allowed execution=%d candidates=%+v", selected, allowEventID, joined.Candidates)
	}
}

func TestV7ToV8MigrationPreservesPopulatedAttachmentCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "legacy",
		ScopeKey: "cwd:/repo", Kind: "attachment", RequestID: "fixture-v8", WorkingDirectory: "/repo",
		Status: "pending", BoundaryClass: "unconfirmed", RequestedAt: 7})
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	_, err = ix.db.Exec(`
DROP TABLE path_reconciliation;
DROP TABLE checkpoint_payload;
ALTER TABLE session_checkpoint RENAME TO session_checkpoint_v8_fixture;
DROP INDEX IF EXISTS session_checkpoint_status;
DROP INDEX IF EXISTS session_checkpoint_single_boundary;
CREATE TABLE session_checkpoint(
  id INTEGER PRIMARY KEY,
  session_id TEXT NOT NULL,
  scope_key TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind='attachment'),
  trigger_event_id INTEGER REFERENCES event(id) ON DELETE SET NULL,
  trigger_observation_id TEXT NOT NULL DEFAULT '',
  working_directory TEXT NOT NULL,
  repository_id TEXT NOT NULL DEFAULT '',
  checkout_id TEXT NOT NULL DEFAULT '',
  checkout_root TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('pending','capturing','complete','failed','unavailable')),
  boundary_class TEXT NOT NULL CHECK(boundary_class IN ('direct-pre-release','late-replay','unconfirmed')),
  requested_at INTEGER NOT NULL,
  capture_started_at INTEGER NOT NULL DEFAULT 0,
  capture_ended_at INTEGER NOT NULL DEFAULT 0,
  capture_attempts INTEGER NOT NULL DEFAULT 0 CHECK(capture_attempts >= 0),
  change_record_id INTEGER REFERENCES change_record(id) ON DELETE RESTRICT,
  failure_kind TEXT NOT NULL DEFAULT '',
  detail_digest TEXT NOT NULL DEFAULT '',
  UNIQUE(session_id,scope_key,kind)
);
INSERT INTO session_checkpoint(id,session_id,scope_key,kind,trigger_event_id,
  trigger_observation_id,working_directory,repository_id,checkout_id,checkout_root,status,
  boundary_class,requested_at,capture_started_at,capture_ended_at,capture_attempts,
  change_record_id,failure_kind,detail_digest)
SELECT id,session_id,scope_key,kind,trigger_event_id,trigger_observation_id,
  working_directory,repository_id,checkout_id,checkout_root,status,boundary_class,
  requested_at,capture_started_at,capture_ended_at,capture_attempts,change_record_id,
  failure_kind,detail_digest FROM session_checkpoint_v8_fixture;
DROP TABLE session_checkpoint_v8_fixture;
DROP TABLE logical_result_relation;
DROP TABLE result_reconciliation_candidate;
DROP TABLE result_reconciliation;
DROP TABLE result_effect;
DROP TABLE result_observation;
DROP TABLE transcript_cursor;
PRAGMA user_version=7;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	got, err := ix.SessionCheckpointByID(checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "legacy" || got.RequestID != "legacy-attachment-1" || got.Status != "pending" {
		t.Fatalf("migrated checkpoint=%+v", got)
	}
	for _, table := range []string{"checkpoint_payload", "path_reconciliation"} {
		var canonicalParents, staleParents int
		if err := ix.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list(?) WHERE "table"='session_checkpoint'`, table).Scan(&canonicalParents); err != nil {
			t.Fatalf("%s canonical foreign keys: %v", table, err)
		}
		if err := ix.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list(?) WHERE "table"='session_checkpoint_v7'`, table).Scan(&staleParents); err != nil {
			t.Fatalf("%s stale foreign keys: %v", table, err)
		}
		if canonicalParents == 0 || staleParents != 0 {
			t.Fatalf("%s foreign keys canonical=%d stale=%d", table, canonicalParents, staleParents)
		}
	}
	var bodyCanonical, bodyStale int
	if err := ix.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('checkpoint_payload_body')
		WHERE "table"='checkpoint_payload'`).Scan(&bodyCanonical); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('checkpoint_payload_body')
		WHERE "table"='checkpoint_payload_v8_stale'`).Scan(&bodyStale); err != nil {
		t.Fatal(err)
	}
	if bodyCanonical == 0 || bodyStale != 0 {
		t.Fatalf("checkpoint body foreign keys canonical=%d stale=%d", bodyCanonical, bodyStale)
	}
	if _, err := ix.db.Exec(`INSERT INTO checkpoint_payload(
		checkpoint_id,ordinal,path,layer,status,content_completeness,patch_completeness)
		VALUES(?,0,'src/a.go','worktree','modified','unavailable','unavailable')`, checkpoint.ID); err != nil {
		t.Fatalf("insert checkpoint payload after migration: %v", err)
	}
	if _, err := ix.db.Exec(`INSERT INTO path_reconciliation(
		session_id,checkout_id,current_checkpoint_id,path,classification,algorithm,reconciled_at)
		VALUES('legacy','checkout',?,'src/a.go','unavailable','test',8)`, checkpoint.ID); err != nil {
		t.Fatalf("insert path reconciliation after migration: %v", err)
	}
	var foreignKeyViolations int
	if err := ix.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil {
		t.Fatal(err)
	}
	if foreignKeyViolations != 0 {
		t.Fatalf("foreign key violations=%d", foreignKeyViolations)
	}
	_, _, err = ix.AppendResultObservation(ResultObservation{ObservationID: "res-migrated",
		SessionID: "legacy", Runtime: "codex", SourceKind: "vendor-transcript",
		SourceDigest: "sha256-v1:migrated", State: "success", Completeness: "metadata-only"}, nil)
	if err != nil {
		t.Fatalf("append result after migration: %v", err)
	}
}
