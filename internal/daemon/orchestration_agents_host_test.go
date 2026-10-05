package daemon

// Focused tests for the agents redesign host behavior: priority arbitration
// with visible budget-free deferral, claim v2 completion (verdict, findings,
// declared tags), owner budget overrides, the hand-back loop budget, and the
// ART-01 auto-reply provenance marker.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// helperAgentProfileSource is a v2 helper profile: authored type, declared tag
// vocabulary, an open stages selector map, and a declared reply shape.
func helperAgentProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: design-helper
version: "1.0.0"
name: Design helper
description: Reviews completed turns and replies from design guidance.
role: follower
type: helper
may-tag:
  - needs-review
  - regex-hole
stages:
  task.completed: Review the completed turn against the design documents.
reply-shape: One concise paragraph grounded in the cited design documents.
execution: managed-turn
trigger:
  event: task.completed
context:
  - kind: task.final-response
    required: true
  - kind: prior-claims
output:
  kind: draft-reply
authority-requests:
  - reply
requirements:
  capabilities:
    - managed-turn
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Review the returned response and draft a grounded reply.
`)
}

const helperClaimV2 = `{"action":"draft_reply","message":"Link the ADR before merging.",` +
	`"citations":["source.final_message"],"verdict":"needs changes","tags":["needs-review"],` +
	`"findings":[{"severity":"warn","statement":"Missing ADR link",` +
	`"refs":[{"kind":"file","path":"README.md"}]}]}`

type agentHostFixture struct {
	root  string
	owner *profilefs.Owner
	tasks *TaskApplicationService
	host  *orchestrationManagedHost
}

func newAgentHostFixture(t *testing.T, driver ChatDriver) agentHostFixture {
	t.Helper()
	return newAgentHostFixtureAt(t, driver, t.TempDir())
}

// newAgentHostFixtureAt opens the host over an existing root so a test can
// seed the store before the host meets it.
func newAgentHostFixtureAt(t *testing.T, driver ChatDriver, root string) agentHostFixture {
	t.Helper()
	// The natural identity cache resolves hook ids through harvest.FindAll,
	// which scans the REAL vendor session trees under $HOME; on a developer
	// host that tree is gigabytes and a cold scan outruns the settle-window
	// tests (measured 2026-09-12: 37 s first iteration on the merged base).
	// Fixtures never own a real vendor session, so they scan an empty home.
	t.Setenv("HOME", root)
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"managed-fixture": driver}
	t.Cleanup(func() { chatDrivers = original })
	taskIndex, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taskIndex.Close() })
	tasks := NewTaskApplicationService(taskStoreRepository{index: taskIndex}, NewTaskExecutionRegistry(), NewTaskSubscriberHub(), registeredTaskRuntime, func(string, string) (bool, error) { return true, nil }, root)
	hostIndex, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(hostIndex, owner, tasks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = hostIndex.Close() })
	return agentHostFixture{root: root, owner: owner, tasks: tasks, host: host}
}

func (fixture agentHostFixture) bindHelper(t *testing.T, bindingID string, priority int64, autoAction bool, granted []string, limits store.ManagedLimits) {
	t.Helper()
	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: bindingID, ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest,
		ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), ScopeRuntime: "managed-fixture", GrantedAuthority: granted,
		AutoAction: autoAction, Priority: priority, Limits: limits,
		ExpectedStateToken: store.ManagedBindingAbsentToken(bindingID)}); err != nil {
		t.Fatal(err)
	}
}

func waitForRuns(t *testing.T, host *orchestrationManagedHost, accept func([]store.ManagedRun) bool) []store.ManagedRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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

// waitForTerminalsHandled returns once the pump's durable position has passed
// every child task's last event. A run reads completed before its terminal
// path finishes — claim tags, helper-session adoption, the pending drain, and
// held-actor release all land after that write — and the position advances
// only when the whole path has returned (the reconcile sweep's own gate). A
// read after this sees settled state, including state that must NOT change.
func waitForTerminalsHandled(t *testing.T, fixture agentHostFixture, runs []store.ManagedRun) {
	t.Helper()
	handled := func() bool {
		position, err := fixture.host.ix.OrchestrationStreamPosition(managedTaskStreamKind)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.ChildTaskID == "" {
				continue
			}
			task, found, err := fixture.tasks.Task(run.ChildTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if !found || terminalKindForLifecycle(task.Lifecycle) == "" || task.LastEventID > position {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if handled() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	position, _ := fixture.host.ix.OrchestrationStreamPosition(managedTaskStreamKind)
	t.Fatalf("pump never handled the terminal events (position %d): %+v", position, runs)
}

func TestAgentPriorityArbitrationDefersLowerHelperAndWritesClaimV2(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(helperClaimV2)
		}
		return jsonTextCommand("Design plan complete.")
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.bindHelper(t, "agent-a", 10, false, nil, store.ManagedLimits{})
	fixture.bindHelper(t, "agent-b", 1, false, nil, store.ManagedLimits{})
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-arb", Cwd: fixture.root}, "arb-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		completed, deferred := 0, 0
		for _, run := range runs {
			if run.State == "completed" {
				completed++
			}
			if run.State == "deferred" {
				deferred++
			}
		}
		if completed != 1 || deferred != 1 {
			return false
		}
		// Completion is stored before the asynchronous tag projection.
		tags, err := fixture.host.ix.ActiveOrchestrationTags("native-arb", time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		return len(tags) == 1
	})
	var winner, loser *store.ManagedRun
	for index := range runs {
		switch runs[index].State {
		case "completed":
			winner = &runs[index]
		case "deferred":
			loser = &runs[index]
		}
	}
	if winner.BindingID != "agent-a" || winner.Action != "draft_reply" {
		t.Fatalf("winner=%+v", winner)
	}
	if winner.Detail["verdict"] != "needs changes" {
		t.Fatalf("winner detail=%+v", winner.Detail)
	}
	if tags := stringSlice(winner.Detail["tags"]); len(tags) != 1 || tags[0] != "needs-review" {
		t.Fatalf("winner tags=%+v", winner.Detail["tags"])
	}
	if winner.Detail["findings"] == nil {
		t.Fatalf("winner findings missing: %+v", winner.Detail)
	}
	if loser.BindingID != "agent-b" || loser.ErrorClass != "priority_deferred" ||
		!strings.Contains(loser.Recovery, "agent-a") || loser.Detail["deferred_to"] != "agent-a" {
		t.Fatalf("loser=%+v", loser)
	}
	// Claim tags are written after the run reads completed.
	waitForTerminalsHandled(t, fixture, []store.ManagedRun{*winner})
	tags, err := fixture.host.ix.ActiveOrchestrationTags("native-arb", time.Now().Unix())
	if err != nil || len(tags) != 1 {
		t.Fatalf("tags=%+v err=%v", tags, err)
	}
	if tags[0].AgentKey != "agent:agent-a:needs-review" || tags[0].Provenance != "model-claimed" ||
		tags[0].Anchor != fmt.Sprint(winner.SourceEventID) {
		t.Fatalf("tag=%+v", tags[0])
	}

	// The managed projection carries project_root per run, the deferred run,
	// and the group relationships (plan §5, GUI ref-resolver binding).
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, fixture.host, fixture.owner)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/managed", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("managed status=%d body=%s", response.Code, response.Body.String())
	}
	var projection struct {
		Runs []struct {
			State       string `json:"state"`
			ProjectRoot string `json:"project_root"`
		} `json:"runs"`
		Relationships []map[string]any `json:"relationships"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	sawDeferred := false
	for _, run := range projection.Runs {
		if run.ProjectRoot != fixture.root {
			t.Fatalf("run project_root=%q", run.ProjectRoot)
		}
		sawDeferred = sawDeferred || run.State == "deferred"
	}
	if !sawDeferred || len(projection.Relationships) == 0 {
		t.Fatalf("projection=%+v", projection)
	}
}

func TestAgentAutoReplyMarkerLoopBudgetAndReplyCycle(t *testing.T) {
	var mu sync.Mutex
	resumedPrompts := []string{}
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		// The root task also carries a native session id; only a resume of a
		// prompt other than the root question is a hand-back reply.
		if request.SessionID != "" && request.Prompt != "root question" {
			mu.Lock()
			resumedPrompts = append(resumedPrompts, request.Prompt)
			mu.Unlock()
			return jsonTextCommand("Understood.")
		}
		if strings.Contains(request.Prompt, "helper agent") {
			claim := `{"action":"reply","message":"Please cite ADR 0028 in the summary.","citations":["source.final_message"]}`
			return jsonTextCommand(claim)
		}
		return jsonTextCommand("Root turn complete.")
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.bindHelper(t, "agent-loop", 0, true, []string{"reply"}, store.ManagedLimits{LoopBudget: 1})
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-loop", Cwd: fixture.root}, "loop-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.State == "deferred" && run.ErrorClass == "loop_budget" {
				return true
			}
		}
		return false
	})
	sawHelper, sawReply := false, false
	for _, run := range runs {
		sawHelper = sawHelper || (run.Role == "helper" && run.State == "completed" && run.Action == "reply")
		sawReply = sawReply || (run.Kind == "reply" && run.State == "completed")
		if run.ErrorClass == "loop_budget" && !strings.Contains(run.Recovery, "budget of 1") {
			t.Fatalf("loop budget recovery=%q", run.Recovery)
		}
	}
	if !sawHelper || !sawReply {
		t.Fatalf("runs=%+v", runs)
	}
	mu.Lock()
	prompts := append([]string(nil), resumedPrompts...)
	mu.Unlock()
	if len(prompts) != 1 {
		t.Fatalf("resumed prompts=%v", prompts)
	}
	// ART-01 / Q5: the auto-composed reply carries the in-band provenance
	// marker and the profile-declared reply shape.
	if !strings.HasPrefix(prompts[0], "[crossing-guard agent agent-loop drafted this reply]") ||
		!strings.Contains(prompts[0], "Reply shape (profile-declared):") ||
		!strings.Contains(prompts[0], "Please cite ADR 0028") {
		t.Fatalf("auto reply prompt=%q", prompts[0])
	}
	// The durable arc records the resumed task and cycle 1.
	var groupID string
	for _, run := range runs {
		if run.Role == "helper" {
			groupID = run.GroupID
		}
	}
	relationships, err := fixture.host.ix.ManagedRelationships(groupID, 10)
	if err != nil {
		t.Fatal(err)
	}
	sawCycle := false
	for _, relationship := range relationships {
		if reply, _ := relationship["reply_task_id"].(string); reply != "" {
			if cycle, _ := relationship["cycle"].(int64); cycle != 1 {
				t.Fatalf("cycle=%v", relationship["cycle"])
			}
			sawCycle = true
		}
	}
	if !sawCycle {
		t.Fatalf("relationships=%+v", relationships)
	}
}

func TestAgentGroupBudgetOverrideSuppressesReplyAdmission(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if request.SessionID != "" && request.Prompt != "root question" {
			return jsonTextCommand("Understood.")
		}
		if strings.Contains(request.Prompt, "helper agent") {
			claim := `{"action":"reply","message":"Budget-bound reply.","citations":["source.final_message"]}`
			return jsonTextCommand(claim)
		}
		return jsonTextCommand("Root turn complete.")
	}}
	fixture := newAgentHostFixture(t, driver)
	// MaxTotal 1 overrides the shipped default of 8: the agent's own run fills
	// the group, so the auto-reply admission is suppressed with the budget fact.
	fixture.bindHelper(t, "agent-budget", 0, true, []string{"reply"}, store.ManagedLimits{MaxTotal: 1})
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-budget", Cwd: fixture.root}, "budget-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.State == "suppressed" && run.ErrorClass == "group_budget" && run.Kind == "reply" {
				return true
			}
		}
		return false
	})
}

func TestAgentEditedSendCarriesNoProvenanceMarker(t *testing.T) {
	var mu sync.Mutex
	resumedPrompts := []string{}
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		// The root task also carries a native session id; only a resume of a
		// prompt other than the root question is a hand-back reply.
		if request.SessionID != "" && request.Prompt != "root question" {
			mu.Lock()
			resumedPrompts = append(resumedPrompts, request.Prompt)
			mu.Unlock()
			return jsonTextCommand("Understood.")
		}
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(helperClaimV2)
		}
		return jsonTextCommand("Root turn complete.")
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.bindHelper(t, "agent-draft", 0, false, nil, store.ManagedLimits{})
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-send", Cwd: fixture.root}, "send-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.State == "completed" && run.Action == "draft_reply" {
				return true
			}
		}
		return false
	})
	var draft *store.ManagedRun
	for index := range runs {
		if runs[index].Action == "draft_reply" {
			draft = &runs[index]
		}
	}
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, fixture.host, fixture.owner)
	body := strings.NewReader(`{"message":"Operator edited reply.","confirmed":true}`)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/orchestration/managed/runs/"+draft.RunID+"/send", body))
	if response.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(resumedPrompts)
		mu.Unlock()
		if count == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// Q5: operator-edited sends are operator speech — the exact edited text,
	// no in-band marker.
	if len(resumedPrompts) != 1 || resumedPrompts[0] != "Operator edited reply." {
		t.Fatalf("resumed prompts=%v", resumedPrompts)
	}
}

func TestAgentBindingRejectsUnpublishedSelectorAndUndeclaredTags(t *testing.T) {
	fixture := newAgentHostFixture(t, managedFixtureDriver{})
	// A profile triggering on the retired v1 name child.completed no longer
	// even compiles — the trigger vocabulary is the published catalog plus
	// pretool.action, never a private enum.
	bad := bytes.Replace(delegateProfileSource(), []byte("event: task.completed"), []byte("event: child.completed"), 1)
	if _, err := fixture.owner.Preview("PROFILE.md", bad); err == nil ||
		!strings.Contains(err.Error(), "trigger.event") {
		t.Fatalf("unpublished trigger err=%v", err)
	}
	// Declared tags outside the profile's may-tag vocabulary are refused.
	helper := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	_, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-tags", ProfileID: helper.ProfileID,
		ProfileSourceDigest: helper.SourceDigest, ProfileBundleDigest: helper.BundleDigest,
		ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), DeclaredTags: []string{"invented-tag"},
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-tags")})
	if err == nil || !strings.Contains(err.Error(), "may-tag") {
		t.Fatalf("undeclared tag err=%v", err)
	}
	// Auto-reply without a declared reply shape is refused at binding time (ART-01).
	follower := selectManagedProfile(t, fixture.owner, followerProfileSource())
	_, err = fixture.host.putBinding(managedBindingCommand{BindingID: "agent-auto", ProfileID: follower.ProfileID,
		ProfileSourceDigest: follower.SourceDigest, ProfileBundleDigest: follower.BundleDigest,
		ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{"draft-reply"},
		AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-auto")})
	if err == nil {
		t.Fatal("auto action on a passive follower was accepted")
	}
}

func TestAgentPromptContextInjectsGroupNotesAndPriorClaims(t *testing.T) {
	fixture := newAgentHostFixture(t, managedFixtureDriver{})
	binding, err := fixture.host.ix.PutManagedBinding(store.ManagedBinding{BindingID: "agent-notes", State: "enabled",
		Role: "helper", ProjectRoot: fixture.root, ProfileID: "design-helper",
		ProfileSourceDigest: "sha256-v1:s", ProfileBundleDigest: "sha256-v1:b", Runtime: "managed-fixture",
		Mode: "", Authority: []string{}, AllowedProfiles: []store.ManagedProfileRef{}},
		store.ManagedBindingAbsentToken("agent-notes"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_notes", BindingID: binding.BindingID, State: "active",
		RootTaskID: "task_root", RootRuntime: "managed-fixture", ProjectRoot: fixture.root, CreatedAt: 1, UpdatedAt: 1}
	run := store.ManagedRun{RunID: "orun_prior", IdempotencyKey: "idem_prior", GroupID: group.GroupID,
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "helper",
		ProfileID: "design-helper", ProfileSourceDigest: "sha256-v1:s", ProfileBundleDigest: "sha256-v1:b",
		SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := fixture.host.ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.host.ix.StartManagedRun(run.RunID, "task_child_prior", "orel_prior", 2); err != nil {
		t.Fatal(err)
	}
	if err := fixture.host.ix.CompleteManagedRun(run.RunID, "completed", "draft_reply", "Cite ADR 0028.", nil, nil, "", "", 3); err != nil {
		t.Fatal(err)
	}
	if err := fixture.host.ix.PutManagedGroupNote(store.ManagedGroupNote{NoteID: "onote_1", GroupID: group.GroupID,
		Body: "Prefer the peripheral port pattern.", CreatedAt: 4}); err != nil {
		t.Fatal(err)
	}
	compiled := profilefs.CompiledProfile{Context: []profilefs.CompiledContext{{Kind: "prior-claims", MaxBytes: 4096}, {Kind: "session.tags", MaxBytes: 65536}}}
	extras, contextErr := fixture.host.agentPromptContext(compiled, &group)
	if contextErr != nil {
		t.Fatal(contextErr)
	}
	byLabel := map[string]string{}
	for _, extra := range extras.Items {
		byLabel[extra.Label] = extra.Body
	}
	if !strings.Contains(byLabel["operator.group_notes"], "peripheral port pattern") {
		t.Fatalf("group notes context=%+v", byLabel)
	}
	if !strings.Contains(byLabel["context.prior_claims"], "Cite ADR 0028.") {
		t.Fatalf("prior claims context=%+v", byLabel)
	}
}

func followerAnnotatorProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: plan-annotator
version: "1.0.0"
name: Plan annotator
description: Tags hand-backs that contain a plan.
role: follower
type: follower
may-tag:
  - plan
stages:
  task.completed: ANNOTATOR passive follower tags the plan.
execution: managed-turn
trigger:
  event: task.completed
context:
  - kind: task.final-response
    required: true
output:
  kind: advice
authority-requests: []
requirements:
  capabilities:
    - managed-turn
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Tag what the hand-back contains.
`)
}

func helperTagReaderProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: tag-reader-helper
version: "1.0.0"
name: Tag reader helper
description: Decides from the session tag snapshot at each hand-back.
role: follower
type: helper
stages:
  task.completed: TAGREADER helper agent consults session.tags to decide.
execution: managed-turn
trigger:
  event: task.completed
context:
  - kind: task.final-response
    required: true
  - kind: session.tags
    required: true
output:
  kind: draft-reply
authority-requests:
  - reply
requirements:
  capabilities:
    - managed-turn
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Decide from the tags.
`)
}

// The owner's canonical coordination rule: on one signal, annotators run first
// and the actor is HELD until they finish, then decides over the fresh tag
// snapshot injected as the declared session.tags context.
func TestAnnotatorsRunBeforeActorsAndTagSnapshotReachesTheActor(t *testing.T) {
	var mu sync.Mutex
	helperPrompt := ""
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "ANNOTATOR") {
			return jsonTextCommand(`{"action":"no_action","message":"Saw a plan.","citations":[],"tags":["plan"]}`)
		}
		if strings.Contains(request.Prompt, "TAGREADER") {
			mu.Lock()
			helperPrompt = request.Prompt
			mu.Unlock()
			return jsonTextCommand(`{"action":"no_action","message":"Tags consulted.","citations":[]}`)
		}
		return jsonTextCommand("A plan.")
	}}
	fixture := newAgentHostFixture(t, driver)
	annotator := selectManagedProfile(t, fixture.owner, followerAnnotatorProfileSource())
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "annotator", ProfileID: annotator.ProfileID,
		ProfileSourceDigest: annotator.SourceDigest, ProfileBundleDigest: annotator.BundleDigest,
		ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), DeclaredTags: []string{"plan"},
		ExpectedStateToken: store.ManagedBindingAbsentToken("annotator")}); err != nil {
		t.Fatal(err)
	}
	reader := selectManagedProfile(t, fixture.owner, helperTagReaderProfileSource())
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "tag-reader", ProfileID: reader.ProfileID,
		ProfileSourceDigest: reader.SourceDigest, ProfileBundleDigest: reader.BundleDigest,
		ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{"reply"},
		ExpectedStateToken: store.ManagedBindingAbsentToken("tag-reader")}); err != nil {
		t.Fatal(err)
	}
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "make a plan",
		SessionID: "native-hold", Cwd: fixture.root}, "hold-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		completed := 0
		for _, run := range runs {
			if run.State == "completed" {
				completed++
			}
		}
		return completed == 2
	})
	var annotatorRun, readerRun *store.ManagedRun
	for index := range runs {
		switch runs[index].BindingID {
		case "annotator":
			annotatorRun = &runs[index]
		case "tag-reader":
			readerRun = &runs[index]
		}
	}
	if annotatorRun == nil || readerRun == nil {
		t.Fatalf("runs=%+v", runs)
	}
	// The actor was HELD: it may not have been admitted before the annotator's
	// run completed, and its prompt must carry the annotator's fresh tag.
	if readerRun.AdmittedAt < annotatorRun.CompletedAt {
		t.Fatalf("actor admitted at %d before annotator completed at %d", readerRun.AdmittedAt, annotatorRun.CompletedAt)
	}
	mu.Lock()
	prompt := helperPrompt
	mu.Unlock()
	if !strings.Contains(prompt, "session.tags") || !strings.Contains(prompt, "- plan (agent:annotator:plan)") {
		t.Fatalf("actor prompt missing fresh tag snapshot: %q", prompt)
	}
}

// A place saved under one spelling of a folder fires for a console task that
// works there under another — the rail offers /tmp/x while the vendor records
// /private/tmp/x (place-root-folder-identity plan). A place on a sibling
// folder stays silent for the same task.
func TestAgentPlaceFiresForItsFolderUnderAnotherSpelling(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(helperClaimV2)
		}
		return jsonTextCommand("Turn complete.")
	}}
	fixture := newAgentHostFixture(t, driver)
	repo, link, sibling := symlinkedFolder(t, fixture.root)
	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	for bindingID, root := range map[string]string{"agent-place": link, "agent-sibling": sibling} {
		if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: bindingID, ProfileID: preview.ProfileID,
			ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest,
			ProjectRoot: root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{},
			ExpectedStateToken: store.ManagedBindingAbsentToken(bindingID)}); err != nil {
			t.Fatal(err)
		}
	}
	source, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "work", SessionID: "native-spelling", Cwd: repo}, "spelling-source")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, source.ID, TaskCompleted, 3*time.Second)
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.BindingID == "agent-place" && run.State == "completed" {
				return true
			}
		}
		return false
	})
	for _, run := range runs {
		if run.BindingID == "agent-sibling" {
			t.Fatalf("a place on a sibling folder fired: %+v", run)
		}
	}
}
