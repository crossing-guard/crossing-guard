package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func testManagedBinding() ManagedBinding {
	return ManagedBinding{BindingID: "managed-follower", State: "enabled", Role: "follower", ProjectRoot: "/repo", ProfileID: "follower", ProfileSourceDigest: "sha256-v1:source", ProfileBundleDigest: "sha256-v1:bundle", Runtime: "codex", Mode: "", Authority: []string{"draft-reply"}, AllowedProfiles: []ManagedProfileRef{}}
}

func TestManagedBindingCASRunReplayBudgetAndControlTruth(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil || binding.StateToken == "" {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if _, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 2); !errors.Is(err, ErrManagedBindingConflict) {
		t.Fatalf("stale create=%v", err)
	}
	group := ManagedGroup{GroupID: "org_1", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root", RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 3, UpdatedAt: 3}
	run := ManagedRun{RunID: "orun_1", IdempotencyKey: "idem_1", GroupID: group.GroupID, BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: "task_root", SourceEventID: 9, AdmittedAt: 3, Citations: []string{}, Detail: map[string]any{}}
	run, created, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2})
	if err != nil || !created || run.State != "admitted" {
		t.Fatalf("run=%+v created=%v err=%v", run, created, err)
	}
	duplicate, created, err := ix.AdmitManagedRun(group, ManagedRun{RunID: "other", IdempotencyKey: "idem_1"}, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2})
	if err != nil || created || duplicate.RunID != run.RunID {
		t.Fatalf("duplicate=%+v created=%v err=%v", duplicate, created, err)
	}
	if err := ix.StartManagedRun(run.RunID, "task_child", "orel_1", 4); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteManagedRun(run.RunID, "completed", "draft_reply", "Use the design.", []string{"source.task"}, nil, "", "", 5); err != nil {
		t.Fatal(err)
	}
	stored, found, err := ix.ManagedRun(run.RunID)
	if err != nil || !found || stored.Action != "draft_reply" {
		t.Fatalf("stored=%+v found=%v err=%v", stored, found, err)
	}
	control := ManagedControl{ControlID: "octl_1", RunID: run.RunID, TaskID: "task_root", RequestedAction: "interrupt", RequestState: "requested", RequestedAt: 6}
	if err := ix.PutManagedControl(control); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteManagedControl(control.ControlID, "unavailable", "authority lost", 7); err != nil {
		t.Fatal(err)
	}
	controls, err := ix.ManagedControls(10)
	if err != nil || len(controls) != 1 || controls[0].Outcome != "unavailable" {
		t.Fatalf("controls=%+v err=%v", controls, err)
	}
	if err := ix.PutOrchestrationStreamPosition(managedTaskCursorKindForTest, 10, 8); err != nil {
		t.Fatal(err)
	}
	if err := ix.PutOrchestrationStreamPosition(managedTaskCursorKindForTest, 9, 9); err == nil {
		t.Fatal("accepted cursor regression")
	}
}

const managedTaskCursorKindForTest = "runtime-task-events-v1"

// TestAdmitManagedRunDeferredSkipsBudgetAndDetailMerge pins the arbitration
// contract (plan §2 Q1): a pre-set deferred run is recorded without consuming
// the group budget, the group row is readable, and the off-pump resolver can
// merge resolution detail into a terminal run without touching its state.
func TestAdmitManagedRunDeferredSkipsBudgetAndDetailMerge(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_d", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root", RootRuntime: "codex", RootCatalogSessionID: "sess-1", ProjectRoot: "/repo", CreatedAt: 3, UpdatedAt: 3}
	base := ManagedRun{GroupID: group.GroupID, BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "helper", ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: "task_root", AdmittedAt: 3, Citations: []string{}, Detail: map[string]any{}}
	deferredRun := base
	deferredRun.RunID, deferredRun.IdempotencyKey, deferredRun.SourceEventID = "orun_deferred", "idem_deferred", 8
	deferredRun.State, deferredRun.ErrorClass = "deferred", "priority_deferred"
	stored, created, err := ix.AdmitManagedRun(group, deferredRun, ManagedGroupBudget{MaxTotal: 1, MaxActive: 1})
	if err != nil || !created || stored.State != "deferred" {
		t.Fatalf("deferred=%+v created=%v err=%v", stored, created, err)
	}
	// The deferred row did not consume the MaxTotal=1 budget: a real run still admits.
	winner := base
	winner.RunID, winner.IdempotencyKey, winner.SourceEventID = "orun_winner", "idem_winner", 9
	stored, created, err = ix.AdmitManagedRun(group, winner, ManagedGroupBudget{MaxTotal: 1, MaxActive: 1})
	if err != nil || !created || stored.State != "admitted" {
		t.Fatalf("winner=%+v created=%v err=%v", stored, created, err)
	}
	if _, _, err := ix.AdmitManagedRun(group, ManagedRun{}, ManagedGroupBudget{}); err == nil {
		t.Fatal("admission without a resolved budget was accepted")
	}
	readGroup, found, err := ix.ManagedGroup(group.GroupID)
	if err != nil || !found || readGroup.RootCatalogSessionID != "sess-1" {
		t.Fatalf("group=%+v found=%v err=%v", readGroup, found, err)
	}
	if err := ix.CompleteManagedRun(winner.RunID, "completed", "draft_reply", "ok", nil, map[string]any{"verdict": "pass"}, "", "", 5); err != nil {
		t.Fatal(err)
	}
	if err := ix.MergeManagedRunDetail(winner.RunID, map[string]any{"findings": []any{map[string]any{"severity": "info"}}}); err != nil {
		t.Fatal(err)
	}
	merged, _, err := ix.ManagedRun(winner.RunID)
	if err != nil || merged.Detail["verdict"] != "pass" || merged.Detail["findings"] == nil || merged.State != "completed" {
		t.Fatalf("merged=%+v err=%v", merged, err)
	}
}

func TestAdmitManagedRunRefusesStaleBindingConsent(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding := testManagedBinding()
	binding.Role = "follower"
	stored, err := ix.PutManagedBinding(binding, ManagedBindingAbsentToken(binding.BindingID), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.DisableManagedBinding(stored.BindingID, stored.StateToken, 2); err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_s", BindingID: stored.BindingID, State: "active", RootTaskID: "t", RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 3, UpdatedAt: 3}
	run := ManagedRun{RunID: "orun_s", IdempotencyKey: "idem_s", GroupID: group.GroupID, BindingID: stored.BindingID,
		BindingStateToken: stored.StateToken, Role: "follower", ProfileID: "p", ProfileSourceDigest: "s", ProfileBundleDigest: "d",
		SourceTaskID: "t", SourceEventID: 1, AdmittedAt: 3, Citations: []string{}, Detail: map[string]any{}}
	admitted, created, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2})
	if err != nil || !created {
		t.Fatalf("admitted=%+v created=%v err=%v", admitted, created, err)
	}
	if admitted.State != "suppressed" || admitted.ErrorClass != "binding_changed" {
		t.Fatalf("stale consent must suppress: %+v", admitted)
	}
}

// TestActiveOrchestrationTagsExpiryBoundaryIsSeconds pins the expiry unit: the
// governance read passes time.Now().Unix() (seconds), so a tag whose
// expires_at is seconds-based must stay visible until that instant — a caller
// passing milliseconds would see every bounded tag as expired (2026-08-30
// red-team fold C1).
func TestActiveOrchestrationTagsExpiryBoundaryIsSeconds(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_tag", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root", RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 1, UpdatedAt: 1}
	run := ManagedRun{RunID: "orun_tag", IdempotencyKey: "idem_tag", GroupID: group.GroupID, BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000) // seconds epoch
	if err := ix.PutOrchestrationTags([]OrchestrationTag{
		{TagID: "tag_live", RunID: run.RunID, BindingID: binding.BindingID, AgentKey: "agent:managed-follower:plan", Tag: "plan", SessionID: "native-exp", AppliedAt: now - 10, ExpiresAt: now + 60},
		{TagID: "tag_open", RunID: run.RunID, BindingID: binding.BindingID, AgentKey: "agent:managed-follower:red-team", Tag: "red-team", SessionID: "native-exp", AppliedAt: now - 10},
		{TagID: "tag_dead", RunID: run.RunID, BindingID: binding.BindingID, AgentKey: "agent:managed-follower:stale", Tag: "stale", SessionID: "native-exp", AppliedAt: now - 10, ExpiresAt: now - 1},
	}); err != nil {
		t.Fatal(err)
	}
	tags, err := ix.ActiveOrchestrationTags("native-exp", now)
	if err != nil || len(tags) != 2 {
		t.Fatalf("tags=%+v err=%v", tags, err)
	}
	for _, tag := range tags {
		if tag.Tag == "stale" {
			t.Fatalf("expired tag returned: %+v", tag)
		}
	}
	// The exact boundary: a millisecond-scale "now" would wrongly expire the
	// bounded tag (now*1000 > now+60).
	tags, err = ix.ActiveOrchestrationTags("native-exp", now*1000)
	if err != nil || len(tags) != 1 || tags[0].Tag != "red-team" {
		t.Fatalf("millisecond now must expire bounded tags, got %+v err=%v", tags, err)
	}
}
