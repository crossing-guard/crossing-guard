package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func helperSessionTestGroup(bindingID string) ManagedGroup {
	return ManagedGroup{GroupID: "org_hs", BindingID: bindingID, State: "active", RootTaskID: "t-src",
		RootRuntime: "codex", RootCatalogSessionID: "cat-src", RootNativeSessionID: "nat-src",
		ProjectRoot: "/repo", CreatedAt: 1, UpdatedAt: 1}
}

func helperSessionTestRun(binding ManagedBinding, id string, eventID int64) ManagedRun {
	return ManagedRun{RunID: "orun_" + id, IdempotencyKey: "idem_" + id, GroupID: "org_hs", BindingID: binding.BindingID,
		BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID,
		ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		SourceTaskID: "t-src", SourceEventID: eventID, AdmittedAt: 2, Citations: []string{}, Detail: map[string]any{}}
}

func pendingFor(eventID int64) ManagedPendingSignal {
	return ManagedPendingSignal{BindingID: "managed-follower", Producer: "runtime-task-events-v1", EventID: eventID,
		Signal: "session.turn-ended", Task: json.RawMessage(`{"id":"t-src","session_runtime":"codex"}`), At: 1000 + eventID}
}

// Coalescing is one store transaction: an occupied group (admitted, running,
// or parked run) takes the signal into its pending slot — replaced, never
// queued — and writes no run row; the drain reads the slot only once the
// group is free and clears it only while the taken signal is still the one
// in the slot.
func TestAdmitManagedTurnCoalescesWhileOccupiedAndDrainsOnce(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := helperSessionTestGroup(binding.BindingID)
	budget := ManagedGroupBudget{MaxTotal: 48, MaxActive: 1, CoalesceWhenOccupied: true}
	first, created, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "1", 1), budget, pendingFor(1))
	if err != nil || !created || first.State != "admitted" {
		t.Fatalf("first turn: %+v %v %v", first, created, err)
	}
	second, created, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "2", 2), budget, pendingFor(2))
	if err != nil || created || second.State != "coalesced" {
		t.Fatalf("second turn must coalesce: %+v %v %v", second, created, err)
	}
	if _, found, _ := ix.ManagedRun("orun_2"); found {
		t.Fatal("a coalesced signal must write no run row")
	}
	third, _, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "3", 3), budget, pendingFor(3))
	if err != nil || third.State != "coalesced" {
		t.Fatalf("third: %+v %v", third, err)
	}
	stored, _, _ := ix.ManagedGroup("org_hs")
	if stored.PendingEventID != 3 || stored.PendingCoalesced != 2 || stored.PendingProducer != "runtime-task-events-v1" {
		t.Fatalf("pending slot must hold the newest signal and count both: %+v", stored)
	}
	if _, found, err := ix.TakeGroupPendingSignal("org_hs"); err != nil || found {
		t.Fatalf("take while occupied: %v %v", found, err)
	}
	// Parked occupies too (a parked turn still owns a relaunch).
	if err := ix.StartManagedRun("orun_1", "t-child-1", "orel_1", 3); err != nil {
		t.Fatal(err)
	}
	if err := ix.ParkManagedRun("orun_1", map[string]any{}, "provider_unavailable", "retry", 4); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := ix.TakeGroupPendingSignal("org_hs"); found {
		t.Fatal("a parked run must keep the slot occupied")
	}
	fourth, _, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "4", 4), budget, pendingFor(4))
	if err != nil || fourth.State != "coalesced" {
		t.Fatalf("fourth must coalesce behind the parked run: %+v %v", fourth, err)
	}
	if err := ix.CompleteManagedRun("orun_1", "failed", "", "", nil, nil, "provider_unavailable", "gave up", 5); err != nil {
		t.Fatal(err)
	}
	take, found, err := ix.TakeGroupPendingSignal("org_hs")
	if err != nil || !found || take.Signal.EventID != 4 || take.Coalesced != 3 || take.Token.EventID != 4 {
		t.Fatalf("take: %+v %v %v", take, found, err)
	}
	stale := take.Token
	stale.At++
	if cleared, err := ix.ClearGroupPendingSignal("org_hs", stale, 6); err != nil || cleared {
		t.Fatalf("a stale token must not clear: %v %v", cleared, err)
	}
	// The slot survives a replacement between take and clear.
	fifth, _, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "5", 5), budget, pendingFor(5))
	if err != nil || fifth.State != "admitted" {
		t.Fatalf("fifth admits on a free group: %+v %v", fifth, err)
	}
	sixth, _, _ := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "6", 6), budget, pendingFor(6))
	if sixth.State != "coalesced" {
		t.Fatalf("sixth: %+v", sixth)
	}
	if cleared, _ := ix.ClearGroupPendingSignal("org_hs", take.Token, 7); cleared {
		t.Fatal("the drained token must not clear a slot another signal replaced")
	}
	pending, err := ix.PendingManagedGroups(10)
	if err != nil || len(pending) != 1 || pending[0].PendingEventID != 6 {
		t.Fatalf("pending groups: %+v %v", pending, err)
	}
	// MaxActive above one admits alongside an occupying run (capability-bound
	// at the host; the store honours whatever policy it is handed).
	wide := ManagedGroupBudget{MaxTotal: 48, MaxActive: 2, CoalesceWhenOccupied: true}
	seventh, _, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "7", 7), wide, pendingFor(7))
	if err != nil || seventh.State != "admitted" {
		t.Fatalf("seventh under MaxActive 2: %+v %v", seventh, err)
	}
	if n, _ := ix.OccupyingManagedRuns("org_hs"); n != 2 {
		t.Fatalf("occupying=%d", n)
	}
}

// The helper session identity is adopted from reported ids only; a different
// id counts a replacement; forgetting counts one too; turns and the
// transcript cursor advance monotonically.
func TestSetGroupHelperSessionCountsReplacementsAndTurns(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := helperSessionTestGroup(binding.BindingID)
	if _, _, err := ix.AdmitManagedRun(group, helperSessionTestRun(binding, "1", 1), ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetGroupHelperSession("org_hs", "codex", "hs-1", 10, true, 5); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetGroupHelperSession("org_hs", "codex", "hs-1", 8, true, 6); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := ix.ManagedGroup("org_hs")
	if stored.HelperNativeSessionID != "hs-1" || stored.HelperRuntime != "codex" || stored.HelperTurns != 2 || stored.HelperSessionReplaced != 0 || stored.HelperTranscriptSeq != 10 {
		t.Fatalf("same id: %+v", stored)
	}
	if err := ix.SetGroupHelperSession("org_hs", "codex", "hs-2", 12, true, 7); err != nil {
		t.Fatal(err)
	}
	stored, _, _ = ix.ManagedGroup("org_hs")
	if stored.HelperNativeSessionID != "hs-2" || stored.HelperTurns != 3 || stored.HelperSessionReplaced != 1 || stored.HelperTranscriptSeq != 12 {
		t.Fatalf("fork: %+v", stored)
	}
	if err := ix.ClearGroupHelperSession("org_hs", 8); err != nil {
		t.Fatal(err)
	}
	stored, _, _ = ix.ManagedGroup("org_hs")
	if stored.HelperNativeSessionID != "" || stored.HelperRuntime != "" || stored.HelperSessionReplaced != 2 || stored.HelperTurns != 3 {
		t.Fatalf("clear: %+v", stored)
	}
	if err := ix.ClearGroupHelperSession("org_hs", 9); err != nil {
		t.Fatal(err)
	}
	if stored, _, _ = ix.ManagedGroup("org_hs"); stored.HelperSessionReplaced != 2 {
		t.Fatalf("clearing an empty session must not count: %+v", stored)
	}
}

// Group identity is a lookup: the first console turn (no session id yet) and
// its later turns (session id known) resolve to one group, whose learned root
// identity is written once and never overwritten.
func TestManagedGroupForSourceJoinsTurnsAndLearnsIdentityOnce(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_first", BindingID: binding.BindingID, State: "active", RootTaskID: "t-1",
		RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 1, UpdatedAt: 1}
	firstRun := helperSessionTestRun(binding, "1", 1)
	firstRun.GroupID = "org_first"
	if _, _, err := ix.AdmitManagedRun(group, firstRun, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := ix.ManagedGroupForSource(binding.BindingID, "t-2", "codex", "", "nat-1"); found {
		t.Fatal("an unknown identity must not match a group without one")
	}
	found, ok, err := ix.ManagedGroupForSource(binding.BindingID, "t-1", "codex", "", "nat-1")
	if err != nil || !ok || found.GroupID != "org_first" {
		t.Fatalf("by task id: %+v %v %v", found, ok, err)
	}
	if err := ix.SetGroupRootIdentity("org_first", "", "nat-1", 2); err != nil {
		t.Fatal(err)
	}
	found, ok, _ = ix.ManagedGroupForSource(binding.BindingID, "t-2", "codex", "", "nat-1")
	if !ok || found.GroupID != "org_first" || found.RootNativeSessionID != "nat-1" {
		t.Fatalf("by learned native id: %+v %v", found, ok)
	}
	if _, ok, _ = ix.ManagedGroupForSource(binding.BindingID, "t-2", "claude", "", "nat-1"); ok {
		t.Fatal("identity never crosses runtimes")
	}
	if _, ok, _ = ix.ManagedGroupForSource("other-binding", "t-1", "codex", "", "nat-1"); ok {
		t.Fatal("identity never crosses bindings")
	}
	if err := ix.SetGroupRootIdentity("org_first", "cat-1", "nat-other", 3); err != nil {
		t.Fatal(err)
	}
	found, _, _ = ix.ManagedGroup("org_first")
	if found.RootNativeSessionID != "nat-1" || found.RootCatalogSessionID != "cat-1" {
		t.Fatalf("learned identity must fill empties only: %+v", found)
	}
	if err := ix.StartManagedRun("orun_1", "t-child", "orel_1", 4); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetRelationshipReply("orun_1", "t-reply", 1, "", "", 5); err != nil {
		t.Fatal(err)
	}
	if n, err := ix.ManagedGroupReplyCount("org_first"); err != nil || n != 1 {
		t.Fatalf("reply count: %d %v", n, err)
	}
}

// A follower whose turn coalesced has no run row; the actor hold reads the
// group's pending slot for that exact event instead of releasing early.
func TestActiveAnnotatorRunsCountsPendingFollowerSlot(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := helperSessionTestGroup(binding.BindingID)
	budget := ManagedGroupBudget{MaxTotal: 48, MaxActive: 1, CoalesceWhenOccupied: true}
	if _, _, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "1", 1), budget, pendingFor(1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "2", 2), budget, pendingFor(2)); err != nil {
		t.Fatal(err)
	}
	if active, err := ix.ActiveAnnotatorRuns("t-src", 2); err != nil || !active {
		t.Fatalf("pending follower slot must hold the actor: %v %v", active, err)
	}
	if active, _ := ix.ActiveAnnotatorRuns("t-src", 3); active {
		t.Fatal("another event must not be held by this slot")
	}
	if active, _ := ix.ActiveAnnotatorRuns("t-other", 2); active {
		t.Fatal("another task must not be held by this slot")
	}
}

// The rail fold resolves a helper child session through the group's adopted
// helper id too, so a forked helper session still folds under its parent.
func TestAgentSessionParentsMatchesGroupHelperSession(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, err := ix.db.Exec(`INSERT INTO runtime_task(id,console_scope,idempotency_key,request_digest,runtime,
		catalog_session_id,native_session_id,working_directory,lifecycle,ownership,observation_mode,freshness,
		controllable,created_at,updated_at,retention_deadline) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"t-child", "scope", "t-child", "digest", "codex", "", "requested-id", "/repo", "running", "crossing-guard", "stream", "live",
		1, 1, 1, 99999); err != nil {
		t.Fatal(err)
	}
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := helperSessionTestGroup(binding.BindingID)
	if _, _, err := ix.AdmitManagedRun(group, helperSessionTestRun(binding, "1", 1), ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	if err := ix.StartManagedRun("orun_1", "t-child", "orel_1", 3); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetGroupHelperSession("org_hs", "codex", "forked-id", 0, true, 4); err != nil {
		t.Fatal(err)
	}
	parents, err := ix.AgentSessionParents([]string{"forked-id", "requested-id", "stranger"})
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 2 {
		t.Fatalf("both the task's id and the adopted id fold: %#v", parents)
	}
	for _, id := range []string{"forked-id", "requested-id"} {
		if parent := parents[id]; parent.RootNativeSessionID != "nat-src" || parent.Role != "follower" {
			t.Fatalf("%s: %#v", id, parent)
		}
	}
}

// A v29 store (ten-column group table) gains the helper-session columns and
// the identity index on open; a second open is a no-op.
func TestHelperSessionMigrationV30IsAdditiveAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`
CREATE TABLE orchestration_group_v29(
  group_id TEXT PRIMARY KEY, binding_id TEXT NOT NULL, state TEXT NOT NULL, root_task_id TEXT NOT NULL,
  root_runtime TEXT NOT NULL, root_catalog_session_id TEXT NOT NULL DEFAULT '', root_native_session_id TEXT NOT NULL DEFAULT '',
  project_root TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
INSERT INTO orchestration_group_v29 VALUES('org_old','managed-follower','active','t','codex','','','/repo',1,1);
DROP INDEX IF EXISTS orchestration_group_identity;
DROP INDEX IF EXISTS orchestration_group_root;
DROP TABLE orchestration_group;
ALTER TABLE orchestration_group_v29 RENAME TO orchestration_group;
PRAGMA user_version=29;`); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()
	for pass := 0; pass < 2; pass++ {
		ix, err = Open(path)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		cols, err := columnSet(ix.db, "orchestration_group")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"helper_runtime", "helper_native_session_id", "helper_turns", "helper_session_replaced", "helper_transcript_seq", "pending_signal_json", "pending_producer", "pending_event_id", "pending_coalesced", "pending_at"} {
			if !cols[name] {
				t.Fatalf("pass %d: column %s missing", pass, name)
			}
		}
		var indexed int
		if err := ix.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('orchestration_group_identity','orchestration_group_helper')`).Scan(&indexed); err != nil || indexed != 2 {
			t.Fatalf("pass %d: indexes %d %v", pass, indexed, err)
		}
		group, found, err := ix.ManagedGroup("org_old")
		if err != nil || !found || group.PendingCoalesced != 0 || group.HelperNativeSessionID != "" {
			t.Fatalf("pass %d: migrated row: %+v %v %v", pass, group, found, err)
		}
		var version int
		if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
			t.Fatalf("pass %d: version %d %v", pass, version, err)
		}
		_ = ix.Close()
	}
}

// Fold of code red-team pass B: exactly-once adoption/clear per run, a
// re-coalescing drained signal keeps its count and wait clock, dropped
// signals are recorded, and lost launches are listable.
func TestHelperSessionAdoptClearDropAndStaleAreExactlyOnce(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := helperSessionTestGroup(binding.BindingID)
	budget := ManagedGroupBudget{MaxTotal: 48, MaxActive: 1, CoalesceWhenOccupied: true}
	if _, _, err := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "1", 1), budget, pendingFor(1)); err != nil {
		t.Fatal(err)
	}
	// Adoption before completion is refused; after completion it lands once.
	if adopted, err := ix.AdoptHelperSessionForRun("orun_1", "org_hs", "codex", "hs-1", 5, 3); err != nil || adopted {
		t.Fatalf("adopt on an active run: %v %v", adopted, err)
	}
	if err := ix.StartManagedRun("orun_1", "t-child-1", "orel_1", 3); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteManagedRun("orun_1", "completed", "no_action", "", nil, map[string]any{"signal": "s"}, "", "", 4); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false} {
		if adopted, err := ix.AdoptHelperSessionForRun("orun_1", "org_hs", "codex", "hs-1", 5, 5); err != nil || adopted != want {
			t.Fatalf("adopt pass %d: %v %v", i, adopted, err)
		}
	}
	stored, _, _ := ix.ManagedGroup("org_hs")
	if stored.HelperTurns != 1 || stored.HelperNativeSessionID != "hs-1" || stored.HelperTranscriptSeq != 5 {
		t.Fatalf("adopted once: %+v", stored)
	}
	run, _, _ := ix.ManagedRun("orun_1")
	if run.Detail["helper_session_adopted"] != float64(1) || run.Detail["signal"] != "s" {
		t.Fatalf("run mark must merge into detail: %+v", run.Detail)
	}
	// Clearing needs a failed run and happens once; a completed run never clears.
	if cleared, err := ix.ClearHelperSessionForRun("orun_1", "org_hs", 6); err != nil || cleared {
		t.Fatalf("clear on a completed run: %v %v", cleared, err)
	}
	second := helperSessionTestRun(binding, "2", 2)
	if _, _, err := ix.AdmitManagedTurn(group, second, budget, pendingFor(2)); err != nil {
		t.Fatal(err)
	}
	if err := ix.StartManagedRun("orun_2", "t-child-2", "orel_2", 7); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteManagedRun("orun_2", "failed", "", "", nil, nil, "exit", "died", 8); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false} {
		if cleared, err := ix.ClearHelperSessionForRun("orun_2", "org_hs", 9); err != nil || cleared != want {
			t.Fatalf("clear pass %d: %v %v", i, cleared, err)
		}
	}
	stored, _, _ = ix.ManagedGroup("org_hs")
	if stored.HelperNativeSessionID != "" || stored.HelperSessionReplaced != 1 {
		t.Fatalf("cleared once: %+v", stored)
	}
	// Same signal re-coalescing keeps count and clock; a new one replaces.
	third := helperSessionTestRun(binding, "3", 3)
	if _, _, err := ix.AdmitManagedTurn(group, third, budget, pendingFor(3)); err != nil {
		t.Fatal(err)
	}
	if err := ix.StartManagedRun("orun_3", "t-child-3", "orel_3", 10); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "4", 4), budget, pendingFor(4)); state.State != "coalesced" {
		t.Fatalf("fourth: %+v", state)
	}
	again := pendingFor(4)
	again.At = 999999
	if state, _, _ := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "4", 4), budget, again); state.State != "coalesced" {
		t.Fatalf("fourth again: %+v", state)
	}
	stored, _, _ = ix.ManagedGroup("org_hs")
	if stored.PendingCoalesced != 1 || stored.PendingAt != 1004 {
		t.Fatalf("re-coalescing the same signal must not count or reset its clock: %+v", stored)
	}
	if state, _, _ := ix.AdmitManagedTurn(group, helperSessionTestRun(binding, "5", 5), budget, pendingFor(5)); state.State != "coalesced" {
		t.Fatalf("fifth: %+v", state)
	}
	stored, _, _ = ix.ManagedGroup("org_hs")
	if stored.PendingCoalesced != 2 || stored.PendingEventID != 5 || stored.PendingAt != 1005 {
		t.Fatalf("a new signal replaces and counts: %+v", stored)
	}
	// Dropping records count and reason under the same token guard.
	if err := ix.CompleteManagedRun("orun_3", "completed", "no_action", "", nil, nil, "", "", 11); err != nil {
		t.Fatal(err)
	}
	take, found, _ := ix.TakeGroupPendingSignal("org_hs")
	if !found {
		t.Fatal("take after free")
	}
	stale := take.Token
	stale.EventID = 99
	if dropped, _ := ix.DropGroupPendingSignal("org_hs", stale, "x", 12); dropped {
		t.Fatal("stale token must not drop")
	}
	if dropped, err := ix.DropGroupPendingSignal("org_hs", take.Token, "binding gone", 12); err != nil || !dropped {
		t.Fatalf("drop: %v %v", dropped, err)
	}
	stored, _, _ = ix.ManagedGroup("org_hs")
	if stored.PendingEventID != 0 || stored.PendingDropped != 1 || stored.PendingDroppedReason != "binding gone" {
		t.Fatalf("dropped record: %+v", stored)
	}
	// A lost launch (admitted, never linked) is listable once it is older
	// than the caller's window.
	lost := helperSessionTestRun(binding, "6", 6)
	lost.AdmittedAt = 100
	if _, _, err := ix.AdmitManagedTurn(group, lost, budget, pendingFor(6)); err != nil {
		t.Fatal(err)
	}
	if runs, err := ix.StaleAdmittedManagedRuns(50, 10); err != nil || len(runs) != 0 {
		t.Fatalf("not stale yet: %+v %v", runs, err)
	}
	if runs, err := ix.StaleAdmittedManagedRuns(101, 10); err != nil || len(runs) != 1 || runs[0].RunID != "orun_6" {
		t.Fatalf("stale: %+v %v", runs, err)
	}
}
