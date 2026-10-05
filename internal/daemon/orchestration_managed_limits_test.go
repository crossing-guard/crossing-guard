package daemon

// Managed-turn profile limits:
// what the host enforces, refuses, and states for the compiled fields a
// managed turn relies on. Generic on purpose — no vendor names; the Codex
// locality grammar is pinned in chat_codex_test.go.

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// waitForRunsWithin is waitForRuns with the caller's deadline, for waits that
// include a profile timeout plus the watcher's cadence.
func waitForRunsWithin(t *testing.T, host *orchestrationManagedHost, within time.Duration, accept func([]store.ManagedRun) bool) []store.ManagedRun {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		runs, err := host.ix.ManagedRuns(50)
		if err != nil {
			t.Fatal(err)
		}
		if accept(runs) {
			return runs
		}
		time.Sleep(20 * time.Millisecond)
	}
	runs, _ := host.ix.ManagedRuns(50)
	t.Fatalf("runs never reached the expected shape: %+v", runs)
	return nil
}

func compiledHelperProfile(t *testing.T, edit func(string) string) profilefs.CompiledProfile {
	t.Helper()
	document, err := profilefs.Parse("PROFILE.md", []byte(edit(string(helperAgentProfileSource()))))
	if err != nil {
		t.Fatal(err)
	}
	return document.Profile
}

func withProfileEdit(from, to string) func(string) string {
	return func(source string) string { return strings.Replace(source, from, to, 1) }
}

const allowRemoteDestination = "    - managed-turn\n  destination:\n    locality: explicit-local-or-remote\n"

func TestManagedGateRefusesValuesTheHostDoesNotImplement(t *testing.T) {
	for _, accepted := range []func(string) string{
		func(source string) string { return source },
		withProfileEdit("  event: task.completed\n", "  event: task.completed\n  debounce: \"0ms\"\n  ignore-origin: self\n"),
		withProfileEdit("    - managed-turn\n", allowRemoteDestination),
	} {
		if err := validateManagedProfile(compiledHelperProfile(t, accepted)); err != nil {
			t.Fatalf("a value the host implements was refused: %v", err)
		}
	}
	for _, refused := range []struct{ name, from, to, reason string }{
		{"no-network destination", "    - managed-turn\n", "    - managed-turn\n  destination:\n    locality: no-network-destination\n", "no-network-destination"},
		{"trigger states", "  event: task.completed\n", "  event: task.completed\n  states:\n    - completed\n", "trigger.states"},
		{"ignore origin none", "  event: task.completed\n", "  event: task.completed\n  ignore-origin: none\n", "ignore-origin"},
		{"debounce window", "  event: task.completed\n", "  event: task.completed\n  debounce: 5s\n", "debounce"},
		{"failure block", "  timeout: record-unavailable\n", "  timeout: block\n", "record-unavailable only"},
		{"failure skip", "  malformed-output: record-unavailable\n", "  malformed-output: skip\n", "record-unavailable only"},
	} {
		err := validateManagedProfile(compiledHelperProfile(t, withProfileEdit(refused.from, refused.to)))
		if err == nil || !strings.Contains(err.Error(), refused.reason) {
			t.Fatalf("%s: want a refusal naming %q, got %v", refused.name, refused.reason, err)
		}
	}
}

// Every shipped managed example keeps passing the gate, and a local-only one
// is refused on a hosted route and admitted on a declared local one.
func TestShippedExampleProfilesPassTheGateAndStateTheirDestination(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "orchestration", "*", "PROFILE.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("examples not found: %v", err)
	}
	for _, path := range paths {
		source := mustReadFile(t, path)
		document, err := profilefs.Parse("PROFILE.md", source)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if document.Profile.Execution != "managed-turn" {
			continue
		}
		if err := validateManagedProfile(document.Profile); err != nil {
			t.Fatalf("%s no longer passes the managed gate: %v", path, err)
		}
		if document.Profile.Requirements.Destination.Locality != "local-only" {
			continue
		}
		if managedRouteDestinationProblem(document.Profile, store.ManagedRoute{Runtime: "managed-fixture", Model: fixtureNonLocalModel}) == "" {
			t.Fatalf("%s: a local-only profile was admitted on a route no runtime declares local", path)
		}
	}
}

func TestDestinationIsJudgedAtSaveForEveryRouteAndChild(t *testing.T) {
	fixture := newAgentHostFixture(t, managedFixtureDriver{command: jsonTextCommand("unused")})
	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	save := func(id string, edit func(*managedBindingCommand)) error {
		command := managedBindingCommand{BindingID: id, ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest,
			ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), ScopeRuntime: "managed-fixture", ExpectedStateToken: store.ManagedBindingAbsentToken(id)}
		edit(&command)
		_, err := fixture.host.putBinding(command)
		return err
	}
	if err := save("hosted-primary", func(c *managedBindingCommand) {
		c.RouteID = testRouteID(fixture.host, "managed-fixture", fixtureNonLocalModel, nil)
	}); err == nil ||
		!strings.Contains(err.Error(), "local-only destination") || !strings.Contains(err.Error(), "leaves this machine") ||
		strings.Contains(err.Error(), fixtureNonLocalModel) || strings.Contains(err.Error(), "Managed fixture") {
		// The sentence is shown on agent surfaces: it must name no runtime and no model id
		// (rest-of-release plan §5.5, criterion 56).
		t.Fatalf("hosted primary route must be refused without naming a runtime or a model: %v", err)
	}
	if err := save("hosted-fallback", func(c *managedBindingCommand) {
		c.Routes = testChain(fixture.host, store.ManagedRoute{Runtime: "managed-fixture", Model: fixtureNonLocalModel})
	}); err == nil || !strings.Contains(err.Error(), "local-only destination") {
		t.Fatalf("a fallback route an outage would move the work to was not judged: %v", err)
	}
	if err := save("local-primary", func(*managedBindingCommand) {}); err != nil {
		t.Fatalf("a declared local route was refused: %v", err)
	}
	// An auto-acting reply on a local model with no matching source scope
	// would resume other runtimes' sessions with a model only this runtime
	// runs: refused; scoped to its own runtime it saves.
	autoReply := func(c *managedBindingCommand) { c.GrantedAuthority, c.AutoAction = []string{"reply"}, true }
	if err := save("auto-reply-unscoped", func(c *managedBindingCommand) { autoReply(c); c.ScopeRuntime = "" }); err == nil ||
		!strings.Contains(err.Error(), "resumes another runtime's session") {
		t.Fatalf("an unscoped auto reply on a local model was saved: %v", err)
	}
	if err := save("auto-reply-scoped", autoReply); err != nil {
		t.Fatalf("an auto reply scoped to its own runtime was refused: %v", err)
	}
}

func TestAllowlistedChildDestinationIsJudgedAtSaveAndAtLaunch(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		switch {
		case strings.Contains(request.Prompt, "child helper"):
			return jsonTextCommand("Child review complete.")
		case strings.Contains(request.Prompt, "helper agent"):
			return jsonTextCommand(`{"action":"launch_profile","message":"Ready for review.","citations":["source.final_message"],"stage_id":"design-review","child_profile_id":"design-review-child"}`)
		default:
			return jsonTextCommand("Design plan complete.")
		}
	}}
	fixture := newAgentHostFixture(t, driver)
	selectManagedProfile(t, fixture.owner, delegateProfileSource()) // local-only by default
	launcher := selectManagedProfile(t, fixture.owner, []byte(strings.Replace(string(coordinatorProfileSource()),
		"    - profile-launch\n", "    - profile-launch\n  destination:\n    locality: explicit-local-or-remote\n", 1)))
	command := managedBindingCommand{BindingID: "agent-launch", ProfileID: launcher.ProfileID, ProfileSourceDigest: launcher.SourceDigest,
		ProfileBundleDigest: launcher.BundleDigest, ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", fixtureNonLocalModel, nil), GrantedAuthority: []string{"launch-profile"}, AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-launch")}
	if _, err := fixture.host.putBinding(command); err == nil || !strings.Contains(err.Error(), "design-review-child") {
		t.Fatalf("a local-only child under a hosted binding was saved: %v", err)
	}
	// The binding a build without destination checks saved: the child is
	// refused at launch as a recorded suppressed row, never launched.
	command.RouteID = testRouteID(fixture.host, "managed-fixture", "", nil)
	binding, err := fixture.host.putBinding(command)
	if err != nil {
		t.Fatal(err)
	}
	binding.Model = fixtureNonLocalModel
	if _, err := fixture.host.ix.PutManagedBinding(binding, binding.StateToken, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root stage", Cwd: fixture.root}, "child-root"); err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.Kind == "delegate" {
				return true
			}
		}
		return false
	})
	var claim, child store.ManagedRun
	for _, run := range runs {
		if run.Kind == "delegate" {
			child = run
		} else {
			claim = run
		}
	}
	if child.State != "suppressed" || child.ErrorClass != "destination_locality" || child.ChildTaskID != "" {
		t.Fatalf("child was not refused as a recorded outcome: %+v", child)
	}
	settled := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.RunID == claim.RunID && run.Detail["auto_action_suppressed"] == "destination_locality" {
				return true
			}
		}
		return false
	})
	_ = settled
	// A retried launch answers with the refusal, not with success (R2-10).
	claim, _, _ = fixture.host.ix.ManagedRun(claim.RunID)
	var suppressed errManagedSuppressed
	if err := fixture.host.launchHelperChild(claim, binding, "design-review-child", "again"); !errors.As(err, &suppressed) {
		t.Fatalf("retried child launch answered %v, want the recorded refusal", err)
	}
}

// A binding saved before destination was judged (the installed migration):
// every signal from one source session leaves ONE visible refusal, nothing
// launches, and the agents surface says why.
func TestExistingHostedBindingIsRefusedOncePerSessionWithoutLaunching(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	driver := &helperSessionFixtureDriver{command: func(ChatRequest) string { return jsonTextCommand("unused") }}
	fixture := naturalFixture(t, driver)
	binding := bindSessionKindsFollower(t, fixture, "agent-hosted", true)
	binding.Model = fixtureNonLocalModel
	if _, err := fixture.host.ix.PutManagedBinding(binding, binding.StateToken, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	appendNaturalTurn(t, fixture, "turn.started", time.Now().Add(-10*time.Second).UnixMilli())
	fixture.host.emitNaturalSignalsOnce()
	appendNaturalTurn(t, fixture, "turn.ended", time.Now().Add(-9*time.Second).UnixMilli())
	fixture.host.emitNaturalSignalsOnce()
	runs := runsFor(t, fixture, "agent-hosted")
	if len(runs) != 1 {
		t.Fatalf("want one refusal row for the session, got %d: %+v", len(runs), runs)
	}
	refused := runs[0]
	if refused.State != "suppressed" || refused.ErrorClass != "destination_locality" || refused.ChildTaskID != "" ||
		!strings.Contains(refused.Recovery, "explicit-local-or-remote") {
		t.Fatalf("refusal is not a visible recorded outcome: %+v", refused)
	}
	if requests := driver.helperRequests(); len(requests) != 0 {
		t.Fatalf("a refused binding launched %d helper turns", len(requests))
	}
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, fixture.host, fixture.owner)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/agents", nil))
	var body struct {
		Agents []agentProjection `json:"agents"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Agents) != 1 || body.Agents[0].DestinationProblem == "" || body.Agents[0].Destination == nil ||
		body.Agents[0].Destination.Required != "local-only" || body.Agents[0].Destination.Routes[0].Local {
		t.Fatalf("agents surface does not state the destination problem: %+v", body.Agents)
	}
}

// ── Turn deadlines ──────────────────────────────────────────────────────────

func deadlineConfig(check time.Duration) OrchestrationConfig {
	config := settledConfig()
	config.HelperSession.TurnDeadlineCheckMS = int(check.Milliseconds())
	return config
}

func oneSecondHelperSource() []byte {
	return []byte(strings.Replace(string(helperAgentProfileSource()), "  timeout: 2m\n", "  timeout: 1s\n", 1))
}

func bindOneSecondHelper(t *testing.T, fixture agentHostFixture, bindingID string) store.ManagedBinding {
	t.Helper()
	preview := selectManagedProfile(t, fixture.owner, oneSecondHelperSource())
	binding, err := fixture.host.putBinding(managedBindingCommand{BindingID: bindingID, ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), ScopeRuntime: "managed-fixture", GrantedAuthority: []string{},
		ExpectedStateToken: store.ManagedBindingAbsentToken(bindingID)})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

const noActionClaim = `{"action":"no_action","message":"Noted.","citations":[]}`

// The watcher on its configured cadence stops an overdue helper turn: the run
// fails as a timeout naming the limit, the helper session survives a vendor
// that never answered, and the next signal runs in that same session.
func TestTurnDeadlineStopsAnOverdueTurnAndKeepsTheHelperSession(t *testing.T) {
	defer swapOrchestrationConfig(deadlineConfig(50 * time.Millisecond))()
	var resumed int32
	driver := &helperSessionFixtureDriver{}
	driver.command = func(request ChatRequest) string {
		if !strings.Contains(request.Prompt, "Crossing Guard") {
			return jsonTextCommand("Root done.")
		}
		if request.SessionID == "" {
			return helperTurnCommand("hs-1", noActionClaim, "")
		}
		if atomic.AddInt32(&resumed, 1) == 1 {
			return "sleep 30" // the vendor never answers this resume
		}
		return helperTurnCommand(request.SessionID, noActionClaim, "")
	}
	fixture := newAgentHostFixture(t, driver)
	bindOneSecondHelper(t, fixture, "agent-deadline")
	source := func(key string) {
		task, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-deadline", Cwd: fixture.root}, key)
		if err != nil {
			t.Fatal(err)
		}
		wantTaskState(t, fixture.tasks, task.ID, TaskCompleted, 3*time.Second)
	}
	source("deadline-1")
	first := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "completed" })[0]
	waitForGroup(t, fixture, first.GroupID, func(group store.ManagedGroup) bool { return group.HelperNativeSessionID == "hs-1" })
	source("deadline-2")
	var stopped store.ManagedRun
	waitForRunsWithin(t, fixture.host, 6*time.Second, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.RunID != first.RunID && run.State == "failed" {
				stopped = run
				return true
			}
		}
		return false
	})
	marker, current := turnTimeoutMarker(stopped)
	if stopped.ErrorClass != "timeout" || !current || !strings.Contains(stopped.Recovery, "profile timeout is 1s") ||
		!strings.Contains(stopped.Recovery, "same helper session") {
		t.Fatalf("timeout not recorded as the profile's limit: %+v marker=%+v", stopped, marker)
	}
	if child, _, _ := fixture.tasks.Task(stopped.ChildTaskID); child.Lifecycle != TaskInterrupted {
		t.Fatalf("the overdue child is still %s", child.Lifecycle)
	}
	group, _, _ := fixture.host.ix.ManagedGroup(stopped.GroupID)
	if group.HelperNativeSessionID != "hs-1" {
		t.Fatalf("a timeout cleared the helper session: %+v", group)
	}
	if wrote, err := fixture.host.ix.MarkManagedRunTimedOut(stopped.RunID, stopped.ChildTaskID, map[string]any{}); err != nil || wrote {
		t.Fatalf("a settled run accepted a timeout marker: wrote=%v err=%v", wrote, err)
	}
	source("deadline-3")
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		completed := 0
		for _, run := range runs {
			if run.State == "completed" {
				completed++
			}
		}
		return completed == 2
	})
}

// A provider-classified child that times out still counts toward its
// route's breaker, and settles as a timeout — never parked for relaunch.
func TestTimedOutChildWithProviderClassFailsAndFeedsTheBreaker(t *testing.T) {
	defer swapOrchestrationConfig(deadlineConfig(50 * time.Millisecond))()
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		return `printf 'AI_APICallError: Bad Gateway\n' >&2; sleep 30`
	}}
	fixture := newAgentHostFixture(t, driver)
	bindOneSecondHelper(t, fixture, "agent-outage-deadline")
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", Cwd: fixture.root}, "outage-root"); err != nil {
		t.Fatal(err)
	}
	run := waitForRunsWithin(t, fixture.host, 6*time.Second, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && (runs[0].State == "failed" || runs[0].State == "parked")
	})[0]
	if run.State != "failed" || run.ErrorClass != "timeout" {
		t.Fatalf("a timed-out provider failure must not park: %+v", run)
	}
	fixture.host.mu.RLock()
	state := fixture.host.breaker[providerRouteKey("managed-fixture", "")]
	fixture.host.mu.RUnlock()
	if state == nil || state.Failures != 1 {
		t.Fatalf("the breaker did not learn from the timed-out provider failure: %+v", state)
	}
}

// seedRunningRun links a running run of the given kind to an existing child
// task, as the launch paths do, without the pump owning that child's events.
func seedRunningRun(t *testing.T, fixture agentHostFixture, binding store.ManagedBinding, kind, childTaskID string) store.ManagedRun {
	t.Helper()
	group, err := fixture.host.groupForSource(binding, RuntimeTask{ID: "task_seed_" + kind, Runtime: "managed-fixture", NativeSessionID: "native-seed-" + kind, WorkingDirectory: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	run := store.ManagedRun{RunID: managedID("orun_", "seed", kind, childTaskID), IdempotencyKey: managedID("oridem_", "seed", kind, childTaskID),
		GroupID: group.GroupID, BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: binding.Role, Kind: kind,
		ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		SourceTaskID: "task_seed_" + kind, SourceEventID: 1, AdmittedAt: now, Citations: []string{}, Detail: map[string]any{}}
	run, _, err = fixture.host.ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 100, MaxActive: 100})
	if err != nil || run.State != "admitted" {
		t.Fatalf("seed admission: %+v %v", run, err)
	}
	if err := fixture.host.ix.StartManagedRun(run.RunID, childTaskID, managedID("orel_", run.RunID, childTaskID), now); err != nil {
		t.Fatal(err)
	}
	seeded, _, err := fixture.host.ix.ManagedRun(run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return seeded
}

// Queue time, reply runs, and children that already ended are never stopped:
// the watcher bounds a helper turn's own run time only.
func TestTurnDeadlineIgnoresQueuedChildrenReplyRunsAndEndedChildren(t *testing.T) {
	defer swapOrchestrationConfig(deadlineConfig(time.Hour))()
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "blocker") {
			return "sleep 30"
		}
		return jsonTextCommand("done")
	}}
	fixture := newAgentHostFixture(t, driver)
	binding := bindOneSecondHelper(t, fixture, "agent-deadline-scope")
	elsewhere := t.TempDir() // outside the binding's root: these tasks are no one's source
	create := func(key, prompt string) RuntimeTask {
		task, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: prompt, Cwd: elsewhere}, key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = fixture.tasks.Interrupt(task.ID) })
		return task
	}
	ended := create("ended-quick", "quick")
	wantTaskState(t, fixture.tasks, ended.ID, TaskCompleted, 3*time.Second)
	running := create("blocker-1", "blocker")
	create("blocker-2", "blocker")
	queued := create("blocker-queued", "blocker")
	wantTaskState(t, fixture.tasks, running.ID, TaskRunning, 3*time.Second)
	if task, _, _ := fixture.tasks.Task(queued.ID); task.Lifecycle != TaskQueued {
		t.Fatalf("expected the third task to wait for a runtime slot, got %s", task.Lifecycle)
	}

	queuedRun := seedRunningRun(t, fixture, binding, "", queued.ID)
	replyRun := seedRunningRun(t, fixture, binding, "reply", running.ID)
	endedRun := seedRunningRun(t, fixture, binding, "delegate", ended.ID)
	fixture.host.enforceTurnDeadlinesOnce(time.Now().Add(time.Hour), map[string]pinnedTurnLimit{})
	for _, run := range []store.ManagedRun{queuedRun, replyRun, endedRun} {
		after, _, _ := fixture.host.ix.ManagedRun(run.RunID)
		if _, marked := after.Detail["timeout"]; marked {
			t.Fatalf("kind %q run was marked for a timeout: %+v", run.Kind, after.Detail)
		}
	}
	if task, _, _ := fixture.tasks.Task(running.ID); task.Lifecycle != TaskRunning {
		t.Fatalf("a reply run's child was stopped: %s", task.Lifecycle)
	}
	// The same live child under a helper turn IS overdue.
	helperRun := seedRunningRun(t, fixture, binding, "", running.ID)
	fixture.host.enforceTurnDeadlinesOnce(time.Now().Add(time.Hour), map[string]pinnedTurnLimit{})
	if after, _, _ := fixture.host.ix.ManagedRun(helperRun.RunID); turnTimeoutFrom(after.Detail).ChildTaskID != running.ID {
		t.Fatalf("an overdue helper turn was not marked: %+v", after.Detail)
	}
}

// A marker names one child: a relaunch drops it, and the next attempt is
// judged on its own (RT-1).
func TestTimeoutMarkerNeverOutlivesTheChildItNames(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	var attempts int32
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		if atomic.AddInt32(&attempts, 1) == 1 {
			return classifiedFailureCommand
		}
		return jsonTextCommand(helperClaimV2)
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.bindHelper(t, "agent-marker", 10, false, nil, store.ManagedLimits{})
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", Cwd: fixture.root}, "marker-root"); err != nil {
		t.Fatal(err)
	}
	parked := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "parked" })[0]
	// The race the guard exists for: a marker written for the dead attempt.
	if err := fixture.host.ix.MergeManagedRunDetail(parked.RunID, map[string]any{"timeout": map[string]any{"child_task_id": parked.ChildTaskID}}); err != nil {
		t.Fatal(err)
	}
	if run, _, _ := fixture.host.ix.ManagedRun(parked.RunID); !func() bool { _, ok := turnTimeoutMarker(run); return ok }() {
		t.Fatal("setup: the marker should name the parked attempt's child")
	}
	fixture.host.relaunchParkedRunsOnce()
	completed := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "completed" })[0]
	if _, carried := completed.Detail["timeout"]; carried {
		t.Fatalf("the relaunched attempt inherited the previous child's marker: %+v", completed.Detail)
	}
}

// ── Relaunch routes (point 3) ────────────────────────────────────────────────

func TestRelaunchSkipsForbiddenRoutesAndFailsWhenNoneIsPermitted(t *testing.T) {
	for _, withPermitted := range []bool{true, false} {
		t.Run(map[bool]string{true: "skips to a permitted route", false: "ends with none permitted"}[withPermitted], func(t *testing.T) {
			relaunchAcrossForbiddenRoute(t, withPermitted)
		})
	}
}

func relaunchAcrossForbiddenRoute(t *testing.T, withPermitted bool) {
	{
		pinProviderRetryPolicy(t, immediateRetryPolicy)
		primary := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
			if strings.Contains(request.Prompt, "root question") {
				return jsonTextCommand("Root done.")
			}
			return quotaFailureCommand
		}}
		fixture := newAgentHostFixture(t, primary)
		chatDrivers["fallback-fixture"] = fallbackFixtureDriver{managedDynamicFixtureDriver{
			commandFor: func(ChatRequest) string { return jsonTextCommand(helperClaimV2) }}}
		fixture.bindHelper(t, "agent-chain", 10, false, nil, store.ManagedLimits{})
		binding, _, _ := fixture.host.ix.ManagedBinding("agent-chain")
		// Saved by a build that did not judge fallback destinations.
		binding.Routes = []store.ManagedRoute{{Runtime: "managed-fixture", Model: fixtureNonLocalModel}}
		if withPermitted {
			binding.Routes = append(binding.Routes, store.ManagedRoute{Runtime: "fallback-fixture"})
		}
		if _, err := fixture.host.ix.PutManagedBinding(binding, binding.StateToken, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", Cwd: fixture.root}, "chain-root"); err != nil {
			t.Fatal(err)
		}
		waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "parked" })
		fixture.host.relaunchParkedRunsOnce()
		settled := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
			return len(runs) == 1 && (runs[0].State == "completed" || runs[0].State == "failed")
		})[0]
		ledger := providerOutageLedgerFrom(settled.Detail)
		if withPermitted {
			if settled.State != "completed" || ledger.RouteIndex != 2 {
				t.Fatalf("relaunch did not skip the forbidden route to the permitted one: %+v %+v", settled, ledger)
			}
			return
		}
		if settled.State != "failed" || settled.ErrorClass != "destination_locality" {
			t.Fatalf("with no permitted route left the run must end, not park: %+v", settled)
		}
	}
}

// ── The reply route (D-5) ────────────────────────────────────────────────────

func TestResumeRefusesACrossRuntimeResumeOnALocalModel(t *testing.T) {
	driver := &helperSessionFixtureDriver{command: func(ChatRequest) string { return helperTurnCommand("native-other", "Source done.", "") }}
	fixture := newAgentHostFixture(t, driver)
	chatDrivers["other-fixture"] = driver
	binding := fixture.bindHelperFor(t, "agent-resume-guard", "")
	source, _, err := fixture.tasks.Create(ChatRequest{Runtime: "other-fixture", Prompt: "root question", SessionID: "native-other", Cwd: t.TempDir()}, "resume-source")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, source.ID, TaskCompleted, 3*time.Second)
	run := seedRunningRun(t, fixture, binding, "", "task_unused_child")
	run.SourceTaskID = source.ID
	err = fixture.host.resumeParent(run, "Consider the ADR.", "manual")
	var suppressed errManagedSuppressed
	if !errors.As(err, &suppressed) || suppressed.class != "cross_runtime_local_model" || !strings.Contains(err.Error(), "Nothing was sent") {
		t.Fatalf("a cross-runtime resume on a local model was not refused: %v", err)
	}
}

// bindHelperFor binds the standard helper with an explicit source scope.
func (fixture agentHostFixture) bindHelperFor(t *testing.T, bindingID, scopeRuntime string) store.ManagedBinding {
	t.Helper()
	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	binding, err := fixture.host.putBinding(managedBindingCommand{BindingID: bindingID, ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), ScopeRuntime: scopeRuntime, GrantedAuthority: []string{},
		ExpectedStateToken: store.ManagedBindingAbsentToken(bindingID)})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

// ── Structural backstop (R2-6) ───────────────────────────────────────────────

// Agent-side turns reach the task service only through createAgentTask, whose
// destination check a future launch path cannot forget; resumeParent — the
// source session's own turn — is the one other caller.
func TestTaskCreateIsCalledOnlyThroughTheAgentBackstop(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	callers := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Create" {
					if inner, ok := selector.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "tasks" {
						if base, ok := inner.X.(*ast.Ident); ok && base.Name == "host" {
							callers[function.Name.Name] = true
						}
					}
				}
				return true
			})
		}
	}
	got := make([]string, 0, len(callers))
	for name := range callers {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "createAgentTask,resumeParent" {
		t.Fatalf("host.tasks.Create is called from %v; agent-side launches must go through createAgentTask", got)
	}
}
