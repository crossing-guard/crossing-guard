package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func openRosterTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func rosterBinding(id string) ManagedBinding {
	binding := testManagedBinding()
	binding.BindingID = id
	return binding
}

func TestPutManagedBindingsIsAllOrNothingAndKeepsState(t *testing.T) {
	ix := openRosterTestIndex(t)
	one, err := ix.PutManagedBinding(rosterBinding("place-one"), ManagedBindingAbsentToken("place-one"), 1)
	if err != nil {
		t.Fatal(err)
	}
	off := rosterBinding("place-two")
	off.State = "disabled"
	two, err := ix.PutManagedBinding(off, ManagedBindingAbsentToken("place-two"), 1)
	if err != nil || two.State != "disabled" {
		t.Fatalf("a disabled create must stay disabled: %+v %v", two, err)
	}
	edited := one
	edited.Priority = 7
	editedTwo := two
	editedTwo.Priority = 7
	// A stale token on the second change: nothing is written, the first included.
	_, err = ix.PutManagedBindings([]ManagedBindingChange{
		{Binding: edited, Expected: one.StateToken},
		{Binding: editedTwo, Expected: "stale"},
	}, 2)
	if !errors.Is(err, ErrManagedBindingConflict) {
		t.Fatalf("stale batch = %v", err)
	}
	if stored, _, _ := ix.ManagedBinding("place-one"); stored.Priority != 0 || stored.StateToken != one.StateToken {
		t.Fatalf("a failed batch wrote a change: %+v", stored)
	}
	writes, err := ix.PutManagedBindings([]ManagedBindingChange{
		{Binding: edited, Expected: one.StateToken},
		{Binding: editedTwo, Expected: two.StateToken},
	}, 3)
	if err != nil || len(writes) != 2 {
		t.Fatalf("batch = %+v %v", writes, err)
	}
	if writes[0].Prior == nil || writes[0].Prior.Priority != 0 || writes[1].Saved.State != "disabled" {
		t.Fatalf("pre-images and states = %+v", writes)
	}
	bad := edited
	bad.State = "paused"
	if _, err := ix.PutManagedBindings([]ManagedBindingChange{{Binding: bad, Expected: writes[0].Saved.StateToken}}, 4); !errors.Is(err, ErrManagedBindingState) {
		t.Fatalf("an unknown state must be a state error, got %v", err)
	}
	disabled, err := ix.PutManagedBindings([]ManagedBindingChange{{BindingID: "place-one", Disable: true, Expected: writes[0].Saved.StateToken}}, 5)
	if err != nil || disabled[0].Saved.State != "disabled" || disabled[0].Prior.State != "enabled" {
		t.Fatalf("disable change = %+v %v", disabled, err)
	}
}

func seedRosterRun(t *testing.T, ix *Index, group ManagedGroup, binding ManagedBinding, id string, admitted int64, kind, state, action string, detail map[string]any) {
	t.Helper()
	run := ManagedRun{RunID: id, IdempotencyKey: "idem_" + id, GroupID: group.GroupID, BindingID: binding.BindingID,
		BindingStateToken: binding.StateToken, Role: binding.Role, Kind: kind, ProfileID: binding.ProfileID,
		ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		SourceTaskID: "task_" + id, SourceEventID: admitted, AdmittedAt: admitted, Citations: []string{}, Detail: map[string]any{}}
	if _, created, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 1000, MaxActive: 1000}); err != nil || !created {
		t.Fatalf("admit %s: %v", id, err)
	}
	if state == "admitted" {
		return
	}
	if err := ix.CompleteManagedRun(id, state, action, "", []string{}, detail, "", "", admitted+1); err != nil {
		t.Fatalf("complete %s: %v", id, err)
	}
}

func TestOutcomeWindowPagesAndTotals(t *testing.T) {
	ix := openRosterTestIndex(t)
	binding, err := ix.PutManagedBinding(rosterBinding("place-one"), ManagedBindingAbsentToken("place-one"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "grp_1", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root",
		RootRuntime: "rt-a", ProjectRoot: "/repo", CreatedAt: 1, UpdatedAt: 1}
	const day = int64(86400)
	now := 10*day + day/2
	seedRosterRun(t, ix, group, binding, "r1", now-1*day, "", "completed", "send_message", nil)
	seedRosterRun(t, ix, group, binding, "r2", now-1*day+10, "", "completed", "no_action", nil)
	seedRosterRun(t, ix, group, binding, "r3", now-100, "", "completed", "no_action", map[string]any{"tags": []any{"plan"}})
	seedRosterRun(t, ix, group, binding, "r4", now-50, "", "failed", "", nil)
	seedRosterRun(t, ix, group, binding, "r5", now-20*day, "", "completed", "send_message", nil)
	seedRosterRun(t, ix, group, binding, "r6", now-10, "reply", "completed", "child_completed", nil)
	seedRosterRun(t, ix, group, binding, "r7", now-5, "", "admitted", "", nil)
	scope := HistoryScope{ProfileID: binding.ProfileID}
	window, err := ix.ManagedOutcomeWindow(scope, now, 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	if window.Totals.Runs != 5 || window.Totals.Acted != 2 || window.Totals.Quiet != 1 || window.Totals.Failed != 1 || window.Totals.Running != 1 {
		t.Fatalf("window totals = %+v", window.Totals)
	}
	if len(window.Days) != 7 || window.Days[6].Runs != 3 || window.Days[5].Runs != 2 {
		t.Fatalf("days = %+v", window.Days)
	}
	if window.LastAt != now-5 || window.LastOutcome != OutcomeRunning {
		t.Fatalf("last = %d %s", window.LastAt, window.LastOutcome)
	}
	totals, err := ix.ManagedOutcomeTotals(scope)
	if err != nil || totals.Runs != 6 || totals.Acted != 3 {
		t.Fatalf("all-time totals = %+v %v", totals, err)
	}
	recent, err := ix.RecentSettledOutcomes(binding.ProfileID, []string{binding.BindingID}, 2)
	if err != nil || fmt.Sprint(recent) != "[failed acted]" {
		t.Fatalf("recent settled = %v %v", recent, err)
	}
	page, more, err := ix.ManagedRunsPage(scope, "", PageCursor{}, 2)
	if err != nil || !more || len(page) != 2 || page[0].RunID != "r7" || page[1].RunID != "r4" {
		t.Fatalf("first page = %+v more=%v %v", page, more, err)
	}
	next, more, err := ix.ManagedRunsPage(scope, "", PageCursor{AdmittedAt: page[1].AdmittedAt, ID: page[1].RunID}, 10)
	if err != nil || more || len(next) != 4 || next[0].RunID != "r3" || next[0].Outcome != OutcomeActed {
		t.Fatalf("next page = %+v more=%v %v", next, more, err)
	}
	quiet, _, err := ix.ManagedRunsPage(scope, OutcomeQuiet, PageCursor{}, 10)
	if err != nil || len(quiet) != 1 || quiet[0].RunID != "r2" {
		t.Fatalf("quiet filter = %+v %v", quiet, err)
	}
	offset, err := ix.ManagedOutcomeWindow(scope, now, -3600, 7)
	if err != nil || offset.Totals.Runs != 5 {
		t.Fatalf("offset window = %+v %v", offset.Totals, err)
	}
}

func TestReviewOutcomesSplitAbstainAsQuiet(t *testing.T) {
	ix := openRosterTestIndex(t)
	binding, err := ix.PutReviewBinding(testReviewBinding(), ReviewBindingAbsentToken(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for index, decision := range []string{"allow", "abstain", ""} {
		id := fmt.Sprintf("inv_%d", index)
		invocation := testReviewInvocation(id, "act_"+id, "sha256-v1:"+id, binding, int64(100+index))
		if _, _, err := ix.AdmitReview(invocation); err != nil {
			t.Fatal(err)
		}
		if err := ix.MarkReviewRunning(id, int64(100+index)); err != nil {
			t.Fatal(err)
		}
		state := "completed"
		if decision == "" {
			state = "unavailable"
		}
		if err := ix.CompleteReview(id, ReviewCompletion{State: state, Action: decision, Citations: []string{}, CompletedAt: int64(101 + index)}); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := ix.ReviewOutcomeTotals(HistoryScope{ProfileID: binding.ProfileID})
	if err != nil || totals.Acted != 1 || totals.Quiet != 1 || totals.Failed != 1 {
		t.Fatalf("review totals = %+v %v", totals, err)
	}
	page, more, err := ix.ReviewInvocationsPage(binding.ProfileID, "", PageCursor{}, 2)
	if err != nil || !more || len(page) != 2 || page[0].InvocationID != "inv_2" || page[0].Outcome != OutcomeFailed {
		t.Fatalf("review page = %+v more=%v %v", page, more, err)
	}
}
