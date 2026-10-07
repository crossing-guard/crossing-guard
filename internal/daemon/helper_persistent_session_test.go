package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/store"
)

// helperSessionFixtureDriver reports a vendor session id the way a real
// adapter does (a "session" stream event) so the host can adopt it, and
// records every ChatRequest it was launched with.
type helperSessionFixtureDriver struct {
	mu       sync.Mutex
	requests []ChatRequest
	command  func(ChatRequest) string
}

func (driver *helperSessionFixtureDriver) BuildCmd(request ChatRequest, _ ChatLaunchContext) (*exec.Cmd, error) {
	driver.mu.Lock()
	driver.requests = append(driver.requests, request)
	driver.mu.Unlock()
	return exec.Command("/bin/sh", "-c", driver.command(request)), nil
}

func (driver *helperSessionFixtureDriver) ProjectEvent(object map[string]any) []ChatEvent {
	if id := anyString(object["session_id"]); id != "" {
		return []ChatEvent{{"type": "session", "id": id}}
	}
	return []ChatEvent{{"type": "text", "text": anyString(object["text"])}}
}

func (driver *helperSessionFixtureDriver) LocalRoute(request ChatRequest) (bool, string) {
	return fixtureLocalRoute(request)
}

func (driver *helperSessionFixtureDriver) ChatCapability() ChatCapability {
	return managedFixtureDriver{}.ChatCapability()
}

func (driver *helperSessionFixtureDriver) helperRequests() []ChatRequest {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	out := []ChatRequest{}
	for _, request := range driver.requests {
		if strings.Contains(request.Prompt, "Crossing Guard") {
			out = append(out, request)
		}
	}
	return out
}

// helperTurnCommand prints the vendor's session answer then the claim. A
// non-empty gate path makes the turn wait until the test creates that file,
// so "a signal lands mid-turn" is arranged, never raced on a clock.
func helperTurnCommand(sessionID, claim, gate string) string {
	prefix := ""
	if gate != "" {
		prefix = "while [ ! -f '" + gate + "' ]; do sleep 0.02; done; "
	}
	answer := ""
	if sessionID != "" {
		answer = "'{\"session_id\":\"" + sessionID + "\"}' "
	}
	return prefix + "printf '%s\\n' " + answer + "'{\"text\":\"" + escapeShellJSON(claim) + "\"}'"
}

func waitForGroup(t *testing.T, fixture agentHostFixture, groupID string, accept func(store.ManagedGroup) bool) store.ManagedGroup {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		group, found, err := fixture.host.ix.ManagedGroup(groupID)
		if err != nil {
			t.Fatal(err)
		}
		if found && accept(group) {
			return group
		}
		time.Sleep(20 * time.Millisecond)
	}
	group, _, _ := fixture.host.ix.ManagedGroup(groupID)
	t.Fatalf("group never reached the expected shape: %+v", group)
	return group
}

func groupsFor(t *testing.T, fixture agentHostFixture, runs []store.ManagedRun) map[string]store.ManagedGroup {
	t.Helper()
	out := map[string]store.ManagedGroup{}
	for _, run := range runs {
		group, found, err := fixture.host.ix.ManagedGroup(run.GroupID)
		if err != nil || !found {
			t.Fatalf("group %s: %v %v", run.GroupID, found, err)
		}
		out[run.GroupID] = group
	}
	return out
}

// One source session, several source turns and signals → ONE helper session:
// the first helper turn starts fresh and reports its id, the group adopts it,
// every later turn resumes it; a signal that lands while a turn is running
// coalesces and launches (once, with its wait recorded) when the turn ends;
// all runs share one group whose root identity is the source session; and
// the helper session's own hook rows never admit a run.
func TestHelperSessionIsOnePerSourceSessionAndCoalescesMidTurn(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	claim := `{"action":"no_action","message":"Noted.","citations":[]}`
	driver := &helperSessionFixtureDriver{}
	root := t.TempDir()
	gate := filepath.Join(root, "first-turn-may-finish")
	driver.command = func(request ChatRequest) string {
		if !strings.Contains(request.Prompt, "Crossing Guard") {
			// The source's turn starts at its session frame.
			return helperTurnCommand(request.SessionID, "Source turn text", "")
		}
		if request.SessionID == "" {
			// The first turn holds until the test has SEEN the source's next
			// signal coalesce behind it.
			return helperTurnCommand("hs-1", claim, gate)
		}
		return helperTurnCommand(request.SessionID, claim, "")
	}
	fixture := newAgentHostFixtureAt(t, driver, root)
	chatDrivers["claude"] = driver
	binding := bindSessionKindsFollower(t, fixture, "agent-persistent", true)
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "Source one", SessionID: "ses-src", Cwd: fixture.root}, "src-first-turn"); err != nil {
		t.Fatal(err)
	}
	// turn-started fires the first helper turn; the source completes while
	// it holds, so turn-ended coalesces; then the gate opens and it drains.
	first := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.BindingID == binding.BindingID && run.State == "running" {
				return true
			}
		}
		return false
	})
	var firstRun store.ManagedRun
	for _, run := range first {
		if run.BindingID == binding.BindingID {
			firstRun = run
		}
	}
	waitForGroup(t, fixture, firstRun.GroupID, func(group store.ManagedGroup) bool { return group.PendingEventID != 0 })
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		completed := 0
		for _, run := range runs {
			if run.BindingID == binding.BindingID && run.State == "completed" {
				completed++
			}
		}
		return completed == 2
	})
	// Session adoption is deferred until after run completion is persisted.
	waitForGroup(t, fixture, firstRun.GroupID, func(group store.ManagedGroup) bool { return group.HelperTurns == 2 })
	runs := runsFor(t, fixture, binding.BindingID)
	if len(runs) != 2 {
		t.Fatalf("two turns expected after one source turn: %+v", runs)
	}
	// Adoption, the turn count, and the slot clear land after completed.
	waitForTerminalsHandled(t, fixture, runs)
	groups := groupsFor(t, fixture, runs)
	if len(groups) != 1 {
		t.Fatalf("one source session must pair with one group: %+v", groups)
	}
	var group store.ManagedGroup
	for _, item := range groups {
		group = item
	}
	if group.HelperNativeSessionID != "hs-1" || group.HelperRuntime != "managed-fixture" || group.HelperTurns != 2 || group.HelperSessionReplaced != 0 {
		t.Fatalf("helper session not adopted: %+v", group)
	}
	if group.RootNativeSessionID != "ses-src" || group.RootRuntime != "claude" {
		t.Fatalf("group root must be the source session: %+v", group)
	}
	drained := 0
	for _, run := range runs {
		if signal, _ := run.Detail["signal"].(string); signal == "session.turn-ended" {
			if n, _ := run.Detail["coalesced_signals"].(float64); n != 1 {
				t.Fatalf("turn-ended must record that it waited: %+v", run.Detail)
			}
			if waited, ok := run.Detail["waited_ms"].(float64); !ok || waited < 0 {
				t.Fatalf("waited_ms missing: %+v", run.Detail)
			}
			drained++
		}
	}
	if drained != 1 {
		t.Fatalf("exactly one drained turn: %+v", runs)
	}
	requests := driver.helperRequests()
	if len(requests) != 2 || requests[0].SessionID != "" || requests[1].SessionID != "hs-1" || requests[1].Runtime != "managed-fixture" {
		t.Fatalf("first turn fresh, second resumes hs-1: %+v", requests)
	}
	if !strings.Contains(requests[1].Prompt, "Profile instructions") {
		t.Fatal("a continuation turn re-sends the profile instructions")
	}
	if group.PendingEventID != 0 || group.PendingCoalesced != 0 {
		t.Fatalf("the slot must be clear after the drain: %+v", group)
	}
	// A second source turn on the same session is a new task; it joins the
	// same group and its helper turns resume the same session.
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "Source two", SessionID: "ses-src", Cwd: fixture.root}, "src-second-turn"); err != nil {
		t.Fatal(err)
	}
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		completed := 0
		for _, run := range runs {
			if run.BindingID == binding.BindingID && run.State == "completed" {
				completed++
			}
		}
		return completed == 4
	})
	waitForGroup(t, fixture, firstRun.GroupID, func(group store.ManagedGroup) bool { return group.HelperTurns == 4 })
	runs = runsFor(t, fixture, binding.BindingID)
	waitForTerminalsHandled(t, fixture, runs)
	if groups = groupsFor(t, fixture, runs); len(groups) != 1 {
		t.Fatalf("a resumed source turn must stay in the pairing group: %+v", groups)
	}
	for _, request := range driver.helperRequests()[1:] {
		if request.SessionID != "hs-1" {
			t.Fatalf("every later turn resumes hs-1: %+v", request)
		}
	}
	group, _, _ = fixture.host.ix.ManagedGroup(runs[0].GroupID)
	if group.HelperTurns != 4 || group.HelperSessionReplaced != 0 {
		t.Fatalf("turn count: %+v", group)
	}
	// The helper session's own hook rows are task-owned: no run, ever.
	before := len(runsFor(t, fixture, binding.BindingID))
	if err := fixture.host.routeNaturalSignal(naturalSessionSignal{Signal: "session.tool-completed", Runtime: "managed-fixture",
		CatalogSessionID: "hs-1", NativeSessionID: "hs-1", ProjectRoot: fixture.root, Producer: naturalResultStreamKind, EventRowID: 777}); err != nil {
		t.Fatal(err)
	}
	if after := len(runsFor(t, fixture, binding.BindingID)); after != before {
		t.Fatalf("the helper followed itself: %d → %d", before, after)
	}
}

// The sweep is the drain of last resort: a pending signal whose occupying run
// ended without a terminal path (restart flips runs by SQL) launches on the
// next pass, and a run still marked running whose child task is gone is
// finished so it can no longer wedge the helper session.
func TestHelperSessionSweepDrainsPendingAndReconcilesWedgedRuns(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	claim := `{"action":"no_action","message":"Noted.","citations":[]}`
	driver := &helperSessionFixtureDriver{}
	driver.command = func(request ChatRequest) string {
		if !strings.Contains(request.Prompt, "Crossing Guard") {
			return jsonTextCommand("Source turn text")
		}
		return helperTurnCommand("hs-9", claim, "")
	}
	fixture := newAgentHostFixture(t, driver)
	chatDrivers["claude"] = driver
	binding := bindSessionKindsFollower(t, fixture, "agent-sweep", true)
	source, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "Source", SessionID: "ses-sweep", Cwd: fixture.root}, "src-sweep")
	if err != nil {
		t.Fatal(err)
	}
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.BindingID == binding.BindingID && run.State == "completed" {
				return true
			}
		}
		return false
	})
	runs := runsFor(t, fixture, binding.BindingID)
	seed := runs[0]
	group, _, _ := fixture.host.ix.ManagedGroup(seed.GroupID)
	// Wait for the source session's own signals to settle — every run
	// terminal AND the pending slot drained — so the seeded occupancy below
	// is the only thing in flight and the coalesce count starts at zero.
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.BindingID == binding.BindingID && run.State != "completed" {
				return false
			}
		}
		current, _, _ := fixture.host.ix.ManagedGroup(group.GroupID)
		return current.PendingEventID == 0
	})
	// An occupying run that will never see a terminal event: its child task
	// does not exist (failed before the link, or the daemon restarted).
	wedged := seed
	wedged.RunID, wedged.IdempotencyKey, wedged.SourceEventID = "orun_wedged", "idem_wedged", 990001
	wedged.State, wedged.ChildTaskID, wedged.Detail = "", "", map[string]any{"signal": "session.turn-ended"}
	if _, created, err := fixture.host.ix.AdmitManagedRun(group, wedged, store.ManagedGroupBudget{MaxTotal: 48, MaxActive: 1}); err != nil || !created {
		t.Fatalf("seed wedged: %v %v", created, err)
	}
	if err := fixture.host.ix.StartManagedRun("orun_wedged", "ghost-task", "orel_wedged", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// A signal that coalesced behind it.
	sourceTask, _, _ := fixture.tasks.Task(source.ID)
	pending := pendingSignalFor(binding, sourceTask, TaskEvent{Producer: managedTaskStreamKind, EventID: 990002, TaskID: sourceTask.ID, Kind: "task.completed"}, "session.turn-ended")
	waiting := seed
	waiting.RunID, waiting.IdempotencyKey, waiting.SourceEventID = "orun_waiting", "idem_waiting", 990002
	waiting.State, waiting.ChildTaskID, waiting.Detail = "", "", map[string]any{"signal": "session.turn-ended"}
	coalesced, _, err := fixture.host.ix.AdmitManagedTurn(group, waiting, store.ManagedGroupBudget{MaxTotal: 48, MaxActive: 1, CoalesceWhenOccupied: true}, pending)
	if err != nil || coalesced.State != "coalesced" {
		t.Fatalf("seed pending: %+v %v", coalesced, err)
	}
	fixture.host.reconcileHelperSessionsOnce()
	wedgedNow, _, _ := fixture.host.ix.ManagedRun("orun_wedged")
	if wedgedNow.State != "failed" {
		t.Fatalf("a run whose child is gone must be finished by the sweep: %+v", wedgedNow)
	}
	runs = waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.SourceEventID == 990002 && run.State == "completed" {
				return true
			}
		}
		return false
	})
	var drained store.ManagedRun
	for _, run := range runs {
		if run.SourceEventID == 990002 {
			drained = run
		}
	}
	if n, _ := drained.Detail["coalesced_signals"].(float64); n != 1 || drained.GroupID != group.GroupID {
		t.Fatalf("drained run: %+v", drained)
	}
	group, _, _ = fixture.host.ix.ManagedGroup(group.GroupID)
	if group.PendingEventID != 0 {
		t.Fatalf("slot must clear after the drain: %+v", group)
	}
	requests := driver.helperRequests()
	if last := requests[len(requests)-1]; last.SessionID != "hs-9" {
		t.Fatalf("the drained turn resumes the helper session: %+v", last)
	}
	// Dropped honestly when the binding no longer exists: a pending slot for
	// a gone binding is cleared, not launched.
	stale := pending
	stale.BindingID = "agent-gone"
	orphan := waiting
	orphan.RunID, orphan.IdempotencyKey, orphan.SourceEventID = "orun_o1", "idem_o1", 990003
	if _, created, err := fixture.host.ix.AdmitManagedRun(group, orphan, store.ManagedGroupBudget{MaxTotal: 48, MaxActive: 1}); err != nil || !created {
		t.Fatalf("seed occupant: %v %v", created, err)
	}
	orphan2 := waiting
	orphan2.RunID, orphan2.IdempotencyKey, orphan2.SourceEventID = "orun_o2", "idem_o2", 990004
	if state, _, _ := fixture.host.ix.AdmitManagedTurn(group, orphan2, store.ManagedGroupBudget{MaxTotal: 48, MaxActive: 1, CoalesceWhenOccupied: true}, stale); state.State != "coalesced" {
		t.Fatalf("seed stale pending: %+v", state)
	}
	if err := fixture.host.ix.CompleteManagedRun("orun_o1", "completed", "no_action", "", nil, nil, "", "", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	fixture.host.reconcileHelperSessionsOnce()
	group, _, _ = fixture.host.ix.ManagedGroup(group.GroupID)
	if group.PendingEventID != 0 || group.PendingDropped != 1 || !strings.Contains(group.PendingDroppedReason, "binding") {
		t.Fatalf("a pending signal for a gone binding must be dropped AND recorded: %+v", group)
	}
	if _, found, _ := fixture.host.ix.ManagedRun(managedRunID("orun_", binding, TaskEvent{Producer: managedTaskStreamKind, EventID: 990004}, "session.turn-ended", sourceTask)); found {
		t.Fatal("a dropped signal must not launch")
	}
}

// The turn request resumes only the binding's primary runtime's recorded
// session; a fallback route runs fresh and a foreign runtime never inherits
// the id. Concurrency is the profile's declared value bounded by the
// adapter's capability (false on every shipped adapter), never a constant.
func TestHelperTurnRequestAndBudgetFollowCapability(t *testing.T) {
	fixture := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("x") }})
	binding := bindSessionKindsFollower(t, fixture, "agent-req", false)
	group := store.ManagedGroup{GroupID: "org_req", BindingID: binding.BindingID, State: "active", RootTaskID: "t", RootRuntime: "claude", ProjectRoot: fixture.root, CreatedAt: 1, UpdatedAt: 1}
	run := store.ManagedRun{RunID: "orun_req", IdempotencyKey: "idem_req", GroupID: "org_req", BindingID: binding.BindingID, BindingStateToken: binding.StateToken,
		Role: "follower", ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		SourceTaskID: "t", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := fixture.host.ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 1}); err != nil {
		t.Fatal(err)
	}
	if request := fixture.host.helperTurnRequest("org_req", "managed-fixture", "managed-fixture", "", "", fixture.root, "p", nil); request.SessionID != "" {
		t.Fatalf("no session yet: %+v", request)
	}
	if err := fixture.host.ix.SetGroupHelperSession("org_req", "managed-fixture", "hs-r", 0, true, 2); err != nil {
		t.Fatal(err)
	}
	if request := fixture.host.helperTurnRequest("org_req", "managed-fixture", "managed-fixture", "m", "", fixture.root, "p", nil); request.SessionID != "hs-r" || request.Model != "m" {
		t.Fatalf("primary route resumes: %+v", request)
	}
	if request := fixture.host.helperTurnRequest("org_req", "managed-fixture", "codex", "", "", fixture.root, "p", nil); request.SessionID != "" {
		t.Fatalf("a fallback route runs fresh: %+v", request)
	}
	if request := fixture.host.helperTurnRequest("org_req", "codex", "codex", "", "", fixture.root, "p", nil); request.SessionID != "" {
		t.Fatalf("a recorded session never crosses runtimes: %+v", request)
	}
	profile := selectManagedProfile(t, fixture.owner, []byte(strings.Replace(string(sessionKindsFollowerProfileSource()), "  max-depth: 1\n", "  max-depth: 1\n  max-concurrency: 3\n", 1)))
	revision, err := fixture.owner.GetRevision(profile.ProfileID, profile.SourceDigest, profile.BundleDigest)
	if err != nil || revision.Normalized == nil || revision.Normalized.Limits.MaxConcurrency != 3 {
		t.Fatalf("profile concurrency: %v %+v", err, revision.Normalized)
	}
	budget := helperSessionBudget(binding, *revision.Normalized)
	if budget.MaxActive != 1 || !budget.CoalesceWhenOccupied || budget.MaxTotal != 48 {
		t.Fatalf("budget must be capability-bound with the shipped MaxTotal: %+v", budget)
	}
	if !persistentAgent(*revision.Normalized) {
		t.Fatal("a managed-turn follower forms a helper session")
	}
	body, _ := json.Marshal(pendingSignalFor(binding, RuntimeTask{ID: "t-x", Runtime: "claude"}, TaskEvent{Producer: managedTaskStreamKind, EventID: 4, Kind: "task.completed"}, "session.turn-ended"))
	if !strings.Contains(string(body), `"id":"t-x"`) || !strings.Contains(string(body), `"event_id":4`) {
		t.Fatalf("pending payload: %s", body)
	}
}

// The restart path: runs flipped to `unknown` by SQL leave a pending slot
// with no terminal path to drain it; the sweep's pending-group loop launches
// it — through a resumed turn — on the next pass.
func TestHelperSessionRestartSweepDrainsPendingSlot(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	claim := `{"action":"no_action","message":"Noted.","citations":[]}`
	driver := &helperSessionFixtureDriver{}
	driver.command = func(request ChatRequest) string {
		if !strings.Contains(request.Prompt, "Crossing Guard") {
			return jsonTextCommand("Source turn text")
		}
		return helperTurnCommand("hs-r", claim, "")
	}
	fixture := newAgentHostFixture(t, driver)
	chatDrivers["claude"] = driver
	binding := bindSessionKindsFollower(t, fixture, "agent-restart", true)
	source, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "Source", SessionID: "ses-restart", Cwd: fixture.root}, "src-restart-turn")
	if err != nil {
		t.Fatal(err)
	}
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.BindingID == binding.BindingID && run.State == "completed" {
				return true
			}
		}
		return false
	})
	seed := runsFor(t, fixture, binding.BindingID)[0]
	group := waitForGroup(t, fixture, seed.GroupID, func(group store.ManagedGroup) bool {
		return group.PendingEventID == 0 && group.HelperNativeSessionID == "hs-r"
	})
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.BindingID == binding.BindingID && run.State != "completed" {
				return false
			}
		}
		return true
	})
	// A turn that was running when the daemon died, and a signal behind it.
	occupant := seed
	occupant.RunID, occupant.IdempotencyKey, occupant.SourceEventID = "orun_dying", "idem_dying", 880001
	occupant.State, occupant.ChildTaskID, occupant.Detail = "", "", map[string]any{"signal": "session.turn-ended"}
	if _, created, err := fixture.host.ix.AdmitManagedRun(group, occupant, store.ManagedGroupBudget{MaxTotal: 48, MaxActive: 1}); err != nil || !created {
		t.Fatalf("seed occupant: %v %v", created, err)
	}
	if err := fixture.host.ix.StartManagedRun("orun_dying", "task-that-died-with-the-daemon", "orel_dying", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	sourceTask, _, _ := fixture.tasks.Task(source.ID)
	pending := pendingSignalFor(binding, sourceTask, TaskEvent{Producer: managedTaskStreamKind, EventID: 880002, TaskID: sourceTask.ID, Kind: "task.completed"}, "session.turn-ended")
	waiting := occupant
	waiting.RunID, waiting.IdempotencyKey, waiting.SourceEventID = "orun_waiting_r", "idem_waiting_r", 880002
	if state, _, err := fixture.host.ix.AdmitManagedTurn(group, waiting, store.ManagedGroupBudget{MaxTotal: 48, MaxActive: 1, CoalesceWhenOccupied: true}, pending); err != nil || state.State != "coalesced" {
		t.Fatalf("seed pending: %+v %v", state, err)
	}
	// Restart: the store flips active runs by SQL; no terminal path runs.
	if _, err := fixture.host.ix.RecoverManagedRunsUnknown(time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if dying, _, _ := fixture.host.ix.ManagedRun("orun_dying"); dying.State != "unknown" {
		t.Fatalf("restart flip: %+v", dying)
	}
	fixture.host.reconcileHelperSessionsOnce()
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.SourceEventID == 880002 && run.State == "completed" {
				return true
			}
		}
		return false
	})
	requests := driver.helperRequests()
	if last := requests[len(requests)-1]; last.SessionID != "hs-r" {
		t.Fatalf("the drained turn resumes the helper session after restart: %+v", last)
	}
	group = waitForGroup(t, fixture, group.GroupID, func(group store.ManagedGroup) bool { return group.PendingEventID == 0 })
	if group.HelperSessionReplaced != 0 || group.PendingDropped != 0 {
		t.Fatalf("restart must neither replace the session nor drop the signal: %+v", group)
	}
}

// Vendor answers decide the helper session's fate, never the failure class
// alone: a resumed turn that reports a DIFFERENT session (fork) is adopted
// as a replacement; a failed resumed turn whose vendor still answered keeps
// the session; a failed turn with no vendor answer forgets it.
func TestHelperSessionAdoptsForksAndClearsOnlyUnansweredFailures(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	claim := `{"action":"no_action","message":"Noted.","citations":[]}`
	var mode atomicString
	mode.Store("fresh")
	driver := &helperSessionFixtureDriver{}
	driver.command = func(request ChatRequest) string {
		if !strings.Contains(request.Prompt, "Crossing Guard") {
			// The source's turn starts at its session frame.
			return helperTurnCommand(request.SessionID, "Source turn text", "")
		}
		switch mode.Load() {
		case "fork":
			return helperTurnCommand("hs-forked", claim, "")
		case "fail-answered":
			return helperTurnCommand(request.SessionID, "", "") + "; exit 3"
		case "fail-silent":
			return "exit 3"
		}
		return helperTurnCommand("hs-a", claim, "")
	}
	fixture := newAgentHostFixture(t, driver)
	chatDrivers["claude"] = driver
	binding := bindSessionKindsFollower(t, fixture, "agent-fork", true)
	settledCount := func(n int) func([]store.ManagedRun) bool {
		return func(runs []store.ManagedRun) bool {
			count := 0
			for _, run := range runs {
				if run.BindingID == binding.BindingID && (run.State == "completed" || run.State == "failed") {
					count++
				}
			}
			return count >= n
		}
	}
	turn := func(label, key string, n int) store.ManagedGroup {
		t.Helper()
		if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: label, SessionID: "ses-fork", Cwd: fixture.root}, key); err != nil {
			t.Fatal(err)
		}
		waitForRuns(t, fixture.host, settledCount(n))
		// Adopt/clear run after the settle write; "keeps the session" is only
		// provable once they have.
		runs := runsFor(t, fixture, binding.BindingID)
		waitForTerminalsHandled(t, fixture, runs)
		seed := runs[0]
		return waitForGroup(t, fixture, seed.GroupID, func(group store.ManagedGroup) bool {
			occupying, _ := fixture.host.ix.OccupyingManagedRuns(group.GroupID)
			return group.PendingEventID == 0 && occupying == 0
		})
	}
	group := turn("Source one", "src-fork-turn-one", 2)
	if group.HelperNativeSessionID != "hs-a" || group.HelperSessionReplaced != 0 {
		t.Fatalf("fresh adoption: %+v", group)
	}
	mode.Store("fork")
	group = turn("Source two", "src-fork-turn-two", 4)
	if group.HelperNativeSessionID != "hs-forked" || group.HelperSessionReplaced != 1 {
		t.Fatalf("a fork is adopted as a replacement: %+v", group)
	}
	mode.Store("fail-answered")
	group = turn("Source three", "src-fork-turn-three", 6)
	if group.HelperNativeSessionID != "hs-forked" || group.HelperSessionReplaced != 1 {
		t.Fatalf("a failed turn the vendor answered keeps the session: %+v", group)
	}
	mode.Store("fail-silent")
	group = turn("Source four", "src-fork-turn-four", 8)
	if group.HelperNativeSessionID != "" || group.HelperSessionReplaced != 2 {
		t.Fatalf("a silent failure forgets the session: %+v", group)
	}
	mode.Store("fresh")
	group = turn("Source five", "src-fork-turn-five", 10)
	if group.HelperNativeSessionID != "hs-a" || group.HelperSessionReplaced != 2 {
		t.Fatalf("the next turn starts fresh and adopts: %+v", group)
	}
}

type atomicString struct {
	mu    sync.Mutex
	value string
}

func (a *atomicString) Store(v string) { a.mu.Lock(); a.value = v; a.mu.Unlock() }
func (a *atomicString) Load() string   { a.mu.Lock(); defer a.mu.Unlock(); return a.value }
