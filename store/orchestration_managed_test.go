package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// TestActiveOrchestrationTagsIsOneRowPerLiveKey pins
// The distinct read behind the tag display: rows are append-only, so
// 250 newer repeats of one key used to fill a 200-row read and hide the older
// keys. The read now returns the newest live row of each (agent_key, tag).
func TestActiveOrchestrationTagsIsOneRowPerLiveKey(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_pw4", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root", RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 1, UpdatedAt: 1}
	run := ManagedRun{RunID: "orun_pw4", IdempotencyKey: "idem_pw4", GroupID: group.GroupID, BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000)
	row := func(id, tag, session string, applied, expires int64) OrchestrationTag {
		return OrchestrationTag{TagID: id, RunID: run.RunID, BindingID: binding.BindingID, AgentKey: "agent:managed-follower:" + tag,
			Tag: tag, SessionID: session, AppliedAt: applied, ExpiresAt: expires}
	}
	rows := []OrchestrationTag{
		row("tag_reviewed", "reviewed", "S", now-1000, 0),
		row("tag_reviewed_b", "reviewed", "S", now-1000, 0), // same second: tag_id DESC decides
		row("tag_plan_old", "plan", "S", now-900, 0),
		row("tag_plan_new", "plan", "S", now-800, 0), // retracted below: the older live row stands
		row("tag_stale", "stale", "S", now-700, now-1),
		row("tag_other_T", "elsewhere", "T", now, 0),
	}
	for i := 0; i < 250; i++ {
		rows = append(rows, row(fmt.Sprintf("tag_other_%03d", i), "other", "S", now-500+int64(i), 0))
	}
	if err := ix.PutOrchestrationTags(rows); err != nil {
		t.Fatal(err)
	}
	if err := ix.RetractOrchestrationTag("tag_plan_new", now-1); err != nil {
		t.Fatal(err)
	}
	tags, err := ix.ActiveOrchestrationTags("S", now)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, tag := range tags {
		got = append(got, tag.TagID)
	}
	if want := "tag_other_249,tag_plan_old,tag_reviewed_b"; strings.Join(got, ",") != want {
		t.Fatalf("rows=%v; want %s (each live key once, its newest live row, newest first)", got, want)
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
	// The reverse lookups apply the same liveness rule.
	byTag, err := ix.ActiveOrchestrationTagsWith("tag", "plan", now, 10)
	if err != nil || len(byTag) != 1 || byTag[0].SessionID != "native-exp" {
		t.Fatalf("by tag = %+v err=%v", byTag, err)
	}
	if dead, err := ix.ActiveOrchestrationTagsWith("tag", "stale", now, 10); err != nil || len(dead) != 0 {
		t.Fatalf("expired tag found by tag: %+v err=%v", dead, err)
	}
	byAgent, err := ix.ActiveOrchestrationTagsWith("agent_key", "agent:managed-follower:red-team", now, 10)
	if err != nil || len(byAgent) != 1 || byAgent[0].Tag != "red-team" {
		t.Fatalf("by agent = %+v err=%v", byAgent, err)
	}
	if _, err := ix.ActiveOrchestrationTagsWith("session_id; DROP TABLE x", "v", now, 10); err == nil {
		t.Fatal("an unknown lookup field must be refused")
	}
	if upper, err := ix.ActiveOrchestrationTagsWith("tag", "PLAN", now, 10); err != nil || len(upper) != 1 {
		t.Fatalf("a tag lookup must ignore case: %+v err=%v", upper, err)
	}
}

// TestActiveOrchestrationTagKeysIsUncut pins the stateful decision's read:
// tag rows are
// append-only, so 250 newer repeats of one key push an older, different key
// past the display read's 200-row cap. The keys read must still return it —
// a missing key turns a `not: agent:x` term true.
func TestActiveOrchestrationTagKeysIsUncut(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_keys", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root", RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 1, UpdatedAt: 1}
	run := ManagedRun{RunID: "orun_keys", IdempotencyKey: "idem_keys", GroupID: group.GroupID, BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000) // seconds epoch
	const (
		reviewed = "agent:managed-follower:reviewed"
		other    = "agent:managed-follower:other"
		bounded  = "agent:managed-follower:bounded"
	)
	tag := func(id, key, session string, applied, expires int64) OrchestrationTag {
		return OrchestrationTag{TagID: id, RunID: run.RunID, BindingID: binding.BindingID, AgentKey: key, Tag: key[len("agent:managed-follower:"):], SessionID: session, AppliedAt: applied, ExpiresAt: expires}
	}
	// The old keys are strictly older than every repeat, so the cap — not the
	// tag_id tiebreak — is what drops them from the row read.
	rows := []OrchestrationTag{
		tag("tag_reviewed", reviewed, "long", now-1000, 0),
		tag("tag_bounded", bounded, "long", now-1000, now+60),
		tag("tag_expired", "agent:managed-follower:expired", "long", now-1000, now-1),
		tag("tag_other_session", "agent:managed-follower:elsewhere", "short", now-1000, 0),
		tag("tag_retracted", "agent:managed-follower:retracted", "long", now-1000, 0),
	}
	for i := 0; i < 250; i++ {
		rows = append(rows, tag(fmt.Sprintf("tag_other_%03d", i), other, "long", now-500+int64(i), 0))
	}
	if err := ix.PutOrchestrationTags(rows); err != nil {
		t.Fatal(err)
	}
	if err := ix.RetractOrchestrationTag("tag_retracted", now-1); err != nil {
		t.Fatal(err)
	}

	// The display read returns the newest live row per (agent_key, tag) with no cut
	// (PW-4), so 250 repeats of one key no longer hide the older keys from it either.
	display, err := ix.ActiveOrchestrationTags("long", now)
	if err != nil || len(display) != 3 {
		t.Fatalf("display rows=%d err=%v", len(display), err)
	}

	keys, err := ix.ActiveOrchestrationTagKeys("long", now)
	if err != nil {
		t.Fatal(err)
	}
	want := []OrchestrationTagKey{{bounded, "model-claimed"}, {other, "model-claimed"}, {reviewed, "model-claimed"}}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("keys=%+v want %+v", keys, want)
	}
	// Same liveness as the row read, in seconds: a millisecond now expires the
	// bounded key and nothing else.
	keys, err = ix.ActiveOrchestrationTagKeys("long", now*1000)
	if err != nil || fmt.Sprint(keys) != fmt.Sprint(want[1:]) {
		t.Fatalf("millisecond now: keys=%+v err=%v", keys, err)
	}
	if none, err := ix.ActiveOrchestrationTagKeys("absent", now); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("absent session: keys=%+v err=%v", none, err)
	}
}

// TestCrossSessionOrchestrationTagReadsAreOneRowPerSessionKey pins the
// cross-session distinct read: rows are
// append-only, so one session's re-claims used to fill the cross-session reads'
// limit and push every other session out. Each read now holds the newest live
// row of each (runtime, session, agent, tag).
func TestCrossSessionOrchestrationTagReadsAreOneRowPerSessionKey(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_xs", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root", RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 1, UpdatedAt: 1}
	run := ManagedRun{RunID: "orun_xs", IdempotencyKey: "idem_xs", GroupID: group.GroupID, BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000)
	row := func(id, runtime, session, tag string, applied, expires int64) OrchestrationTag {
		return OrchestrationTag{TagID: id, RunID: run.RunID, BindingID: binding.BindingID, AgentKey: "agent:managed-follower:" + tag,
			Tag: tag, Runtime: runtime, SessionID: session, AppliedAt: applied, ExpiresAt: expires}
	}
	tags := []OrchestrationTag{
		row("tag_q1", "codex", "quiet-1", "reviewed", 10, 0),
		row("tag_q2", "codex", "quiet-2", "reviewed", 11, 0),
		row("tag_q3", "codex", "quiet-3", "reviewed", 12, 0),
		row("tag_plan_live", "codex", "busy", "plan", 20, 0),
		row("tag_plan_retracted", "codex", "busy", "plan", 400, 0),
		row("tag_same_a", "codex", "busy", "reviewed", 350, 0),
		row("tag_same_b", "codex", "busy", "reviewed", 350, 0),
		row("tag_blank_codex", "codex", "", "plan", 5, 0),
		row("tag_blank_claude", "claude", "", "plan", 6, 0),
		row("tag_gone", "codex", "gone", "reviewed", 30, now-1),
	}
	for i := 0; i < 250; i++ {
		tags = append(tags, row(fmt.Sprintf("tag_busy_%03d", i), "codex", "busy", "reviewed", int64(100+i), 0))
	}
	if err := ix.PutOrchestrationTags(tags); err != nil {
		t.Fatal(err)
	}
	if err := ix.RetractOrchestrationTag("tag_plan_retracted", 401); err != nil {
		t.Fatal(err)
	}
	ids := func(rows []OrchestrationTag) string {
		out := []string{}
		for _, row := range rows {
			out = append(out, row.TagID)
		}
		return strings.Join(out, ",")
	}

	all, cut, err := ix.AllActiveOrchestrationTags(now, 4)
	if err != nil || !cut || ids(all) != "tag_same_b,tag_plan_live,tag_q3,tag_q2" {
		t.Fatalf("all(4) = %s cut=%v err=%v; want tag_same_b,tag_plan_live,tag_q3,tag_q2 cut", ids(all), cut, err)
	}
	all, cut, err = ix.AllActiveOrchestrationTags(now, 7)
	if err != nil || cut || ids(all) != "tag_same_b,tag_plan_live,tag_q3,tag_q2,tag_q1,tag_blank_claude,tag_blank_codex" {
		t.Fatalf("all(7) = %s cut=%v err=%v; want every session key once, uncut", ids(all), cut, err)
	}

	byTag, err := ix.ActiveOrchestrationTagsWith("tag", "REVIEWED", now, 4)
	if err != nil || ids(byTag) != "tag_same_b,tag_q3,tag_q2,tag_q1" {
		t.Fatalf("by tag = %s err=%v; want one row per session", ids(byTag), err)
	}
	if cut, err := ix.ActiveOrchestrationTagsWith("tag", "reviewed", now, 3); err != nil || ids(cut) != "tag_same_b,tag_q3,tag_q2" {
		t.Fatalf("by tag at a cutting limit = %s err=%v; want the newest three sessions", ids(cut), err)
	}
	byAgent, err := ix.ActiveOrchestrationTagsWith("agent_key", "agent:managed-follower:reviewed", now, 5)
	if err != nil || ids(byAgent) != "tag_same_b,tag_q3,tag_q2,tag_q1" {
		t.Fatalf("by agent = %s err=%v; want one row per session", ids(byAgent), err)
	}
	byPlan, err := ix.ActiveOrchestrationTagsWith("tag", "plan", now, 10)
	if err != nil || ids(byPlan) != "tag_plan_live,tag_blank_claude,tag_blank_codex" {
		t.Fatalf("by plan = %s err=%v; a retracted newest repeat must not hide the older live row", ids(byPlan), err)
	}
}
