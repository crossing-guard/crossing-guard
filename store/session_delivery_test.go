package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openDeliveryTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

// One record per run, a visible cap, a claim that commits before the caller
// responds, identity by either alternate, creation order, and expiry that
// never touches a claimed record (helper-session-attachment plan D5).
func TestSessionDeliveryEnqueueClaimCapAndExpiry(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	record := func(id string, created int64) SessionDelivery {
		return SessionDelivery{DeliveryID: id, RunID: "orun_" + id, Runtime: "claude", NativeSessionID: "ses-hook",
			CatalogSessionID: "ses-catalog", Message: "remember " + id, Boundary: "next hook boundary",
			CreatedAt: created, ExpiresAt: created + 600}
	}
	for index, id := range []string{"odel_b", "odel_a", "odel_c"} {
		if err := ix.EnqueueSessionDelivery(record(id, int64(100+index)), 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := ix.EnqueueSessionDelivery(record("odel_d", 200), 3); !errors.Is(err, ErrSessionDeliveryCap) {
		t.Fatalf("cap not enforced: %v", err)
	}
	// A repeat of the same delivery id is a no-op, not a fourth record.
	if err := ix.EnqueueSessionDelivery(record("odel_c", 102), 3); err != nil {
		t.Fatal(err)
	}
	if err := ix.EnqueueSessionDelivery(SessionDelivery{DeliveryID: "x", RunID: "r", Runtime: "claude", NativeSessionID: "s", Message: "m", CreatedAt: 5, ExpiresAt: 5}, 3); err == nil {
		t.Fatal("accepted a record that expires when created")
	}
	// Another session is not blocked by this one's cap.
	other := record("odel_other", 300)
	other.NativeSessionID, other.CatalogSessionID = "ses-other", ""
	if err := ix.EnqueueSessionDelivery(other, 3); err != nil {
		t.Fatal(err)
	}
	// Claim by the catalog alternate, at now=150: two records are live, one
	// (odel_c, created 102, expires 702) too — all three drain in creation order.
	claimed, err := ix.ClaimSessionDeliveries("claude", "ses-catalog", "turn.started", "trn_1", "", 150, 4096)
	if err != nil || len(claimed) != 3 || claimed[0].DeliveryID != "odel_b" || claimed[1].DeliveryID != "odel_a" || claimed[2].DeliveryID != "odel_c" {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if claimed[0].State != "delivered" || claimed[0].DeliveredKind != "turn.started" || claimed[0].DeliveredObservationID != "trn_1" || claimed[0].DeliveredAt != 150 {
		t.Fatalf("claimed record not marked: %+v", claimed[0])
	}
	// A second boundary finds nothing: the claim committed.
	again, err := ix.ClaimSessionDeliveries("claude", "ses-hook", "tool.completed", "res_1", "call-1", 151, 4096)
	if err != nil || len(again) != 0 {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	// Wrong runtime never matches a same-named session.
	if wrong, _ := ix.ClaimSessionDeliveries("codex", "ses-other", "turn.started", "trn_2", "", 301, 4096); len(wrong) != 0 {
		t.Fatal(wrong)
	}
	// Expiry settles only pending records past their deadline.
	expired, err := ix.ExpireSessionDeliveries(1000)
	if err != nil || len(expired) != 1 || expired[0].DeliveryID != "odel_other" || expired[0].State != "expired" {
		t.Fatalf("expired=%+v err=%v", expired, err)
	}
	got, found, err := ix.SessionDeliveryForRun("orun_odel_a")
	if err != nil || !found || got.State != "delivered" {
		t.Fatalf("%+v %v %v", got, found, err)
	}
	// An expired record cannot be claimed later.
	if late, _ := ix.ClaimSessionDeliveries("claude", "ses-other", "turn.started", "trn_3", "", 1001, 4096); len(late) != 0 {
		t.Fatal(late)
	}
}

// One boundary drains in creation order only until the byte budget is
// reached; the first record always drains; the rest stay pending.
func TestSessionDeliveryClaimStopsAtByteBudget(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	for index, size := range []int{50, 50, 50} {
		if err := ix.EnqueueSessionDelivery(SessionDelivery{DeliveryID: "odel_" + string(rune('a'+index)), RunID: "orun_" + string(rune('a'+index)),
			Runtime: "claude", NativeSessionID: "ses", Message: string(make([]byte, size)), CreatedAt: int64(10 + index), ExpiresAt: 900}, 5); err != nil {
			t.Fatal(err)
		}
	}
	first, err := ix.ClaimSessionDeliveries("claude", "ses", "turn.started", "trn_1", "", 100, 80)
	if err != nil || len(first) != 1 || first[0].DeliveryID != "odel_a" {
		t.Fatalf("budget 80 must drain exactly the first 50-byte record: %+v %v", first, err)
	}
	second, err := ix.ClaimSessionDeliveries("claude", "ses", "turn.started", "trn_2", "", 101, 100)
	if err != nil || len(second) != 2 || second[0].DeliveryID != "odel_b" || second[1].DeliveryID != "odel_c" {
		t.Fatalf("budget 100 must drain the remaining two: %+v %v", second, err)
	}
	tiny, err := ix.ClaimSessionDeliveries("claude", "ses", "turn.started", "trn_3", "", 102, 1)
	if err != nil || len(tiny) != 0 {
		t.Fatalf("nothing pending: %+v %v", tiny, err)
	}
}

// A claim whose reply never reached the hook is released back to pending
// and delivered by the next boundary; only the releasing boundary's own rows
// move, never a record another boundary holds.
func TestSessionDeliveryReleaseReturnsClaimToPending(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	for index, id := range []string{"odel_a", "odel_b"} {
		if err := ix.EnqueueSessionDelivery(SessionDelivery{DeliveryID: id, RunID: "orun_" + id, Runtime: "claude",
			NativeSessionID: "ses", Message: "m " + id, CreatedAt: int64(10 + index), ExpiresAt: 900}, 5); err != nil {
			t.Fatal(err)
		}
	}
	first, err := ix.ClaimSessionDeliveries("claude", "ses", "tool.started", "obs_1", "call-1", 100, 4096)
	if err != nil || len(first) != 2 {
		t.Fatalf("claim: %+v %v", first, err)
	}
	if n, err := ix.ReleaseSessionDeliveries([]string{"odel_a", "odel_b"}, "obs_other"); err != nil || n != 0 {
		t.Fatalf("another boundary's release must move nothing: %d %v", n, err)
	}
	if n, err := ix.ReleaseSessionDeliveries([]string{"odel_a", "odel_b"}, "obs_1"); err != nil || n != 2 {
		t.Fatalf("release: %d %v", n, err)
	}
	got, found, err := ix.SessionDeliveryForRun("orun_odel_a")
	if err != nil || !found || got.State != "pending" || got.DeliveredAt != 0 || got.DeliveredKind != "" || got.DeliveredObservationID != "" || got.DeliveredNativeCallID != "" {
		t.Fatalf("released record must look never claimed: %+v %v %v", got, found, err)
	}
	second, err := ix.ClaimSessionDeliveries("claude", "ses", "turn.started", "trn_2", "", 101, 4096)
	if err != nil || len(second) != 2 || second[0].DeliveredObservationID != "trn_2" {
		t.Fatalf("the next boundary delivers what was released: %+v %v", second, err)
	}
	if n, _ := ix.ReleaseSessionDeliveries([]string{"odel_a"}, "obs_1"); n != 0 {
		t.Fatal("a stale release must not undo a later boundary's claim")
	}
	if n, _ := ix.ReleaseSessionDeliveries(nil, "obs_1"); n != 0 {
		t.Fatal("empty release")
	}
}

// The natural emitter's cursor readers: turn rows carry their rowid, and only
// hook-delivered (live-post-tool) results qualify while the cursor still
// advances past transcript-derived rows (red-team H6).
func TestNaturalSignalReadersReturnRowIDsAndFilterLiveResults(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.EnsureSessionRoot("claude", "ses-1", "", "/repo"); err != nil {
		t.Fatal(err)
	}
	for index, kind := range []string{"turn.started", "turn.ended"} {
		if _, _, err := tx.AppendSessionTurn(SessionTurnObservation{ObservationID: "trn_" + kind, Runtime: "claude", SessionID: "ses-1",
			Kind: kind, ObservedAt: int64(10 + index), ReceivedAtMS: int64(10000 + index), EvidenceDigest: "sha256-v1:x",
			CollectorID: "c", DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.AppendSessionActivity(SessionActivityObservation{ObservationID: "act_1", Runtime: "claude", SessionID: "ses-1",
		State: "open", ObservedAt: 10, ValidUntil: 70, EvidenceClass: "positive-open", EntryKind: "start",
		EvidenceDigest: "sha256-v1:x", CollectorID: "c", ReceivedAt: 10, DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	turns, last, err := ix.SessionTurnsAfter(0, 10)
	if err != nil || len(turns) != 2 || turns[0].RowID == 0 || turns[1].RowID != last || turns[0].Kind != "turn.started" {
		t.Fatalf("turns=%+v last=%d err=%v", turns, last, err)
	}
	if more, next, err := ix.SessionTurnsAfter(last, 10); err != nil || len(more) != 0 || next != last {
		t.Fatalf("cursor did not hold: %+v %d %v", more, next, err)
	}
	activity, _, err := ix.SessionActivityAfter(0, 10)
	if err != nil || len(activity) != 1 || activity[0].RowID == 0 {
		t.Fatalf("activity rowid missing: %+v %v", activity, err)
	}
	for index, sourceKind := range []string{"vendor-transcript", "live-post-tool", "vendor-patch-result", "live-post-tool"} {
		if _, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res_" + string(rune('a'+index)), SessionID: "ses-1",
			Runtime: "claude", Tool: "Bash", NativeCallID: "call-" + string(rune('a'+index)), NativeCallKind: "tool_use_id",
			SourceKind: sourceKind, SourceSequence: string(rune('a' + index)), SourceDigest: "sha256-v1:" + string(rune('a'+index)), State: "success",
			Completeness: "unavailable", CompletedAt: int64(20 + index), DeliveryMode: "direct", DeliveryAttempts: 1}, nil); err != nil {
			t.Fatal(err)
		}
	}
	results, last, err := ix.ResultObservationsAfter(0, 10)
	if err != nil || len(results) != 2 || results[0].Tool != "Bash" || results[0].SessionID != "ses-1" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if last != results[1].ID {
		t.Fatalf("cursor must advance to the last examined id: last=%d results=%+v", last, results)
	}
	// A bounded read whose window ends on a filtered row still advances.
	partial, cursor, err := ix.ResultObservationsAfter(0, 1)
	if err != nil || len(partial) != 0 || cursor != 1 {
		t.Fatalf("partial=%+v cursor=%d err=%v", partial, cursor, err)
	}
}

// A helper attached to a terminal session has no parent runtime_task row;
// the caused edge still resolves through the run's group root, and the
// parent endpoint carries that identity (plan D7).
func TestCausedRelationsResolveNaturalParentsThroughGroupRoots(t *testing.T) {
	ix := openDeliveryTestIndex(t)
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_natural", BindingID: binding.BindingID, State: "active", RootTaskID: "natural:codex:ses-hook",
		RootRuntime: "codex", RootCatalogSessionID: "ses-catalog", RootNativeSessionID: "ses-hook",
		ProjectRoot: "/repo", CreatedAt: 2, UpdatedAt: 2}
	run := ManagedRun{RunID: "orun_nat", IdempotencyKey: "idem_nat", GroupID: group.GroupID,
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "helper",
		ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest,
		ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: group.RootTaskID, SourceEventID: 7,
		AdmittedAt: 3, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 8, MaxActive: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`INSERT INTO runtime_task(id,console_scope,idempotency_key,request_digest,runtime,
		catalog_session_id,native_session_id,working_directory,lifecycle,ownership,observation_mode,freshness,
		controllable,created_at,updated_at,retention_deadline) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"task_child", "scope", "task_child", "digest", "codex", "", "child-native", "/repo", "running", "crossing-guard", "stream", "live",
		1, 1, 1, 99999); err != nil {
		t.Fatal(err)
	}
	if err := ix.StartManagedRun("orun_nat", "task_child", "orel_nat", 4); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ses-hook", "ses-catalog", "child-native"} {
		relations, err := ix.CausedRelationsForSession(id, 10)
		if err != nil || len(relations) != 1 {
			t.Fatalf("%s: relations=%+v err=%v", id, relations, err)
		}
		rel := relations[0]
		if rel.ParentSessionID != "ses-hook" || rel.ParentRuntime != "codex" || rel.ChildSessionID != "child-native" || rel.ChildRuntime != "codex" {
			t.Fatalf("%s: endpoints not resolved through the group root: %+v", id, rel)
		}
	}
	if relations, _ := ix.CausedRelationsForSession("unrelated", 10); len(relations) != 0 {
		t.Fatal(relations)
	}
}
