package daemon

// Escalation delivery: the
// governance fixes, the delivery receipt and the owner-attention classes,
// driven through finishManagedChild end to end with a real store, a real
// task service and a real flow grant — no mocks around the settle path.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// escalationFixture is a helper binding with reply authority, optionally
// governed by an armed flow grant over one session, and a source task that
// makes that session daemon-owned when owned is true.
type escalationFixture struct {
	agentHostFixture
	binding   store.ManagedBinding
	sessionID string
	sourceID  string
	flowID    string
	runs      int
}

// escalationHelperSource is a helper profile that fires on the natural
// hand-back and asks for reply authority.
func escalationHelperSource() []byte {
	source := strings.Replace(string(helperAgentProfileSource()), "task.completed", "session.turn-ended", -1)
	return []byte(strings.Replace(source, "  - kind: task.final-response\n    required: true", "  - kind: session.tags", 1))
}

// escalationOptions shapes one fixture: whether the session is daemon-owned,
// the armed flow grant (nil for none), the granted authority, the profile
// sources selected in order (the last is bound), and whether the helper acts
// automatically.
type escalationOptions struct {
	owned    bool
	grant    *FlowStageProfile
	granted  []string
	sources  [][]byte
	proposal bool
}

func newEscalationFixture(t *testing.T, owned bool, grant *FlowStageProfile, granted []string, sources ...[]byte) *escalationFixture {
	t.Helper()
	return newEscalationFixtureWith(t, escalationOptions{owned: owned, grant: grant, granted: granted, sources: sources})
}

func newEscalationFixtureWith(t *testing.T, options escalationOptions) *escalationFixture {
	t.Helper()
	owned, grant, granted, sources := options.owned, options.grant, options.granted, options.sources
	t.Cleanup(swapOrchestrationConfig(settledConfig()))
	fixture := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("resumed") }})
	prior := indexPath()
	setIndexPath(fixture.root)
	t.Cleanup(func() { setIndexPath(prior) })
	if len(sources) == 0 {
		sources = [][]byte{escalationHelperSource()}
	}
	var preview profilefs.Preview
	for _, source := range sources {
		preview = selectManagedProfile(t, fixture.owner, source)
	}
	binding, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-escalation", ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		// Scoped to its own runtime: the fixture adapter's route is local, and an
		// auto-acting reply may not resume another runtime's session with a local
		// model (managed-turn-profile-limits plan §4.1).
		RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), ScopeRuntime: "managed-fixture", GrantedAuthority: granted, AutoAction: !options.proposal,
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-escalation")})
	if err != nil {
		t.Fatal(err)
	}
	fx := &escalationFixture{agentHostFixture: fixture, binding: binding, sessionID: "ses-escalation"}
	now := time.Now()
	if owned {
		source, _, _, err := fixture.tasks.repository.Create(taskCreateRecord{ID: "task-escalation-source", ConsoleScope: "test",
			IdempotencyKey: "escalation-source", RequestDigest: "escalation-source", Runtime: "managed-fixture",
			CatalogSessionID: fx.sessionID, NativeSessionID: fx.sessionID, WorkingDirectory: fixture.root, CreatedAt: now.UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		fx.sourceID = source.ID
	}
	if grant != nil {
		grant.ProfileID = preview.ProfileID
		flow := SavedFlow{ID: "esc", Name: "Escalation", MembershipTags: []string{"flow:esc"},
			Stages: []FlowStage{{ID: "building", Name: "Building", Membership: "tag:work=uncommitted", Profiles: []FlowStageProfile{*grant}}}}
		if err := writeFlows(flowsPath(fixture.root), []SavedFlow{flow}); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(flow)
		if err := fixture.host.ix.EnableFlow(flow.ID, raw, now.Unix()); err != nil {
			t.Fatal(err)
		}
		if err := fixture.host.ix.PutFlowBinding(store.OrchestrationFlowBinding{FlowID: flow.ID, Stage: "building",
			MemberRuntime: "managed-fixture", MemberSessionID: fx.sessionID, ProfileID: preview.ProfileID}, "", now.Unix()); err != nil {
			t.Fatal(err)
		}
		if err := fixture.host.ix.ArmFlowBinding(flow.ID, "building", "managed-fixture", fx.sessionID, preview.ProfileID, now.Unix()); err != nil {
			t.Fatal(err)
		}
		fx.flowID = flow.ID
	}
	return fx
}

// settleClaim admits one running helper run on the fixture session whose
// child answered claim, then drives its terminal event through the host.
func (fx *escalationFixture) settleClaim(t *testing.T, signal, claim string) (store.ManagedRun, error) {
	t.Helper()
	return fx.finish(t, fx.admitClaim(t, signal, claim))
}

// finish drives one admitted run's terminal event through the host.
func (fx *escalationFixture) finish(t *testing.T, started store.ManagedRun) (store.ManagedRun, error) {
	t.Helper()
	settleErr := fx.host.finishManagedChild(started, TaskEvent{TaskID: started.ChildTaskID, Kind: "task.completed"})
	settled, _, err := fx.host.ix.ManagedRun(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return settled, settleErr
}

// admitClaim admits and starts one helper run whose child answered claim.
func (fx *escalationFixture) admitClaim(t *testing.T, signal, claim string) store.ManagedRun {
	t.Helper()
	fx.runs++
	now := time.Now()
	child, _, _, err := fx.tasks.repository.Create(taskCreateRecord{ID: fmt.Sprintf("task-escalation-child-%d", fx.runs), ConsoleScope: "test",
		IdempotencyKey: fmt.Sprintf("escalation-child-%d", fx.runs), RequestDigest: "child", Runtime: "managed-fixture", CreatedAt: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.tasks.repository.Append(child.ID, "message.completed", "test", map[string]any{"text": claim}, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "grp-escalation", BindingID: fx.binding.BindingID, State: "active", RootTaskID: fx.sourceID,
		RootRuntime: "managed-fixture", RootCatalogSessionID: fx.sessionID, RootNativeSessionID: fx.sessionID,
		ProjectRoot: fx.root, CreatedAt: now.Unix(), UpdatedAt: now.Unix()}
	runID := fmt.Sprintf("orun_escalation_%d", fx.runs)
	run := store.ManagedRun{RunID: runID, IdempotencyKey: runID, GroupID: group.GroupID, BindingID: fx.binding.BindingID,
		BindingStateToken: fx.binding.StateToken, Role: fx.binding.Role, ProfileID: fx.binding.ProfileID,
		ProfileSourceDigest: fx.binding.ProfileSourceDigest, ProfileBundleDigest: fx.binding.ProfileBundleDigest,
		SourceTaskID: fx.sourceID, SourceEventID: int64(fx.runs), AdmittedAt: now.Unix(), Citations: []string{},
		Detail: map[string]any{"signal": signal, "labels": []any{"source.task"}}}
	if _, created, err := fx.host.ix.AdmitManagedRun(group, run, agentGroupBudget(fx.binding)); err != nil || !created {
		t.Fatalf("admit: created=%v err=%v", created, err)
	}
	if err := fx.host.ix.StartManagedRun(runID, child.ID, "orel_"+runID, now.Unix()); err != nil {
		t.Fatal(err)
	}
	started, _, err := fx.host.ix.ManagedRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	return started
}

func flowGrantProfile(maxDeliveries int64) *FlowStageProfile {
	return &FlowStageProfile{MaxDeliveries: maxDeliveries, ReplyClass: "short", ReplyClassBytes: 200}
}

// RT-1a: a ceiling breach decided after the claim is stored lands ON the
// completed run, and the roster lane counts it. Before the fix the breach
// completed the run a second time, the store refused, and nothing was kept.
func TestFlowCeilingBreachIsRecordedOnTheCompletedRun(t *testing.T) {
	fx := newEscalationFixture(t, true, flowGrantProfile(1), []string{"reply"})
	// The one allowed delivery is already spent.
	if err := fx.host.ix.CountFlowDelivery(fx.flowID, "managed-fixture", fx.sessionID, "earlier", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Keep going.","citations":[]}`)
	if err != nil {
		t.Fatalf("breach settle returned %v", err)
	}
	if run.State != "completed" || run.ErrorClass != "flow_ceiling_breach" || run.Detail["flow_ceiling"] != "flow_ceiling_breach" {
		t.Fatalf("breach not recorded on the run: state=%s class=%q detail=%+v", run.State, run.ErrorClass, run.Detail)
	}
	breached, err := fx.host.ix.FlowCeilingBreachActive(fx.binding.ProfileID)
	if err != nil || !breached {
		t.Fatalf("roster breach lane = %v (%v), want true", breached, err)
	}
	// G3: the owner's re-tag re-arms the ceiling, and the lane clears.
	if err := fx.host.ix.ArmFlowBinding(fx.flowID, "building", "managed-fixture", fx.sessionID, fx.binding.ProfileID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if breached, err := fx.host.ix.FlowCeilingBreachActive(fx.binding.ProfileID); err != nil || breached {
		t.Fatalf("roster breach lane after re-arm = %v (%v), want false", breached, err)
	}
}

func (fx *escalationFixture) ceilingDeliveries(t *testing.T) (int64, int64) {
	t.Helper()
	ceiling, found, err := fx.host.ix.FlowCeiling(fx.flowID, "managed-fixture", fx.sessionID)
	if err != nil || !found {
		t.Fatalf("ceiling row: found=%v err=%v", found, err)
	}
	return ceiling.Deliveries, ceiling.Breached
}

// RT-7 / AC-3: advice, drafts and no-action deliver nothing, so an armed
// grant neither refuses nor counts them.
func TestFlowGrantNeverMeetsNonActingClaims(t *testing.T) {
	fx := newEscalationFixture(t, true, flowGrantProfile(1), []string{"reply"})
	long := strings.Repeat("x", 400) // over the declared class bound
	for _, action := range []string{"advise_user", "draft_reply", "no_action"} {
		run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"`+action+`","message":"`+long+`","citations":[]}`)
		if err != nil {
			t.Fatal(err)
		}
		if run.Detail["flow_grant_refused"] != nil || run.ErrorClass != "" {
			t.Fatalf("%s met the grant: class=%q detail=%+v", action, run.ErrorClass, run.Detail)
		}
	}
	if deliveries, breached := fx.ceilingDeliveries(t); deliveries != 0 || breached != 0 {
		t.Fatalf("non-acting claims counted: deliveries=%d breached=%d", deliveries, breached)
	}
}

// RT-7 / AC-3: under an armed grant, an acting claim that is not the declared
// reply class is refused and recorded, never sent and never counted.
func TestFlowGrantRefusesLaunchProfile(t *testing.T) {
	fx := newEscalationFixture(t, true, flowGrantProfile(3), []string{"launch-profile"}, delegateProfileSource(), coordinatorProfileSource())
	run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"launch_profile","message":"Review it.","citations":[],"child_profile_id":"design-review-child"}`)
	if err != nil {
		t.Fatal(err)
	}
	if run.Detail["flow_grant_refused"] != "action_outside_declared_reply_class" {
		t.Fatalf("launch_profile under an armed grant was not refused: %+v", run.Detail)
	}
	if deliveries, _ := fx.ceilingDeliveries(t); deliveries != 0 {
		t.Fatalf("refused claim counted: %d", deliveries)
	}
}

// RT-8a / AC-4: the transport decision runs before the ceiling, so two
// identical continues on an attended session count nothing and trip no
// no-op breach.
func TestAttendedContinuesAreNeverCounted(t *testing.T) {
	fx := newEscalationFixture(t, false, flowGrantProfile(3), []string{"reply"})
	for i := 0; i < 2; i++ {
		run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead.","citations":[]}`)
		if err != nil {
			t.Fatal(err)
		}
		if run.Detail["auto_reply_suppressed"] != "attended_session" || run.ErrorClass != "" {
			t.Fatalf("continue %d: class=%q detail=%+v", i+1, run.ErrorClass, run.Detail)
		}
	}
	if deliveries, breached := fx.ceilingDeliveries(t); deliveries != 0 || breached != 0 {
		t.Fatalf("suppressed continues counted: deliveries=%d breached=%d", deliveries, breached)
	}
}

// A conforming continue on an owned session is sent and counted once.
func TestOwnedContinueIsCountedOnce(t *testing.T) {
	fx := newEscalationFixture(t, true, flowGrantProfile(3), []string{"reply"})
	run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if run.Detail["flow_grant_refused"] != nil || run.Detail["auto_reply_suppressed"] != nil {
		t.Fatalf("owned continue refused: %+v", run.Detail)
	}
	if deliveries, _ := fx.ceilingDeliveries(t); deliveries != 1 {
		t.Fatalf("deliveries=%d, want 1", deliveries)
	}
}

// receiptOf reads one run's delivery receipt as the projection sees it.
func receiptOf(t *testing.T, run store.ManagedRun) (string, string) {
	t.Helper()
	raw, err := json.Marshal(run.Detail["delivery"])
	if err != nil {
		t.Fatal(err)
	}
	var receipt SessionMessageReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("receipt %s: %v", raw, err)
	}
	return receipt.State, receipt.ReasonClass
}

// AC-1: every acting claim settles one receipt; non-acting claims carry none.
func TestEveryActingClaimSettlesOneReceipt(t *testing.T) {
	cases := []struct {
		name        string
		options     escalationOptions
		signal      string
		claim       string
		missingTask bool
		state       string
		reason      string
	}{
		{name: "delivered reply", options: escalationOptions{owned: true, granted: []string{"reply"}},
			signal: "session.turn-ended", claim: `{"action":"reply","message":"Go ahead.","citations":[]}`, state: "started"},
		{name: "attended reply", options: escalationOptions{granted: []string{"reply"}},
			signal: "session.turn-ended", claim: `{"action":"reply","message":"The owner needs to pick.","citations":[]}`,
			state: "unavailable", reason: "attended_session"},
		{name: "non-terminal signal", options: escalationOptions{owned: true, granted: []string{"reply"}},
			signal: "session.tool-completed", claim: `{"action":"reply","message":"Go ahead.","citations":[]}`,
			state: "unavailable", reason: "non_terminal_signal"},
		{name: "proposal only", options: escalationOptions{owned: true, granted: []string{"reply"}, proposal: true},
			signal: "session.turn-ended", claim: `{"action":"reply","message":"Go ahead.","citations":[]}`, state: "not_requested"},
		{name: "dry run", options: escalationOptions{owned: true, granted: []string{"reply"},
			grant: &FlowStageProfile{MaxDeliveries: 3, ReplyClass: "short", ReplyClassBytes: 200, DryRun: true}},
			signal: "session.turn-ended", claim: `{"action":"reply","message":"Go ahead.","citations":[]}`,
			state: "unavailable", reason: "dry_run"},
		{name: "grant refusal", options: escalationOptions{owned: true, granted: []string{"reply"}, grant: flowGrantProfile(3)},
			signal: "session.turn-ended", claim: `{"action":"reply","message":"` + strings.Repeat("x", 300) + `","citations":[]}`,
			state: "unavailable", reason: "grant:reply_exceeds_declared_max_bytes"},
		{name: "resume error", options: escalationOptions{owned: true, granted: []string{"reply"}},
			signal: "session.turn-ended", claim: `{"action":"reply","message":"Go ahead.","citations":[]}`, missingTask: true,
			state: "unavailable", reason: "resume_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEscalationFixtureWith(t, tc.options)
			if tc.missingTask {
				fx.sourceID = "task-escalation-missing"
			}
			run, err := fx.settleClaim(t, tc.signal, tc.claim)
			if err != nil && !tc.missingTask {
				t.Fatal(err)
			}
			if state, reason := receiptOf(t, run); state != tc.state || reason != tc.reason {
				t.Fatalf("receipt = %s/%s, want %s/%s (detail %+v)", state, reason, tc.state, tc.reason, run.Detail)
			}
		})
	}
	fx := newEscalationFixture(t, true, nil, []string{"reply"})
	for _, action := range []string{"advise_user", "draft_reply", "no_action"} {
		run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"`+action+`","message":"A note.","citations":[]}`)
		if err != nil {
			t.Fatal(err)
		}
		if run.Detail["delivery"] != nil {
			t.Fatalf("%s carries a receipt: %+v", action, run.Detail["delivery"])
		}
	}
}

// completeWithPendingReceipt stores an acting claim the way a crash between
// the claim write and its outcome leaves it.
func (fx *escalationFixture) completeWithPendingReceipt(t *testing.T, completedAt int64) string {
	t.Helper()
	fx.runs++
	now := time.Now()
	group := store.ManagedGroup{GroupID: "grp-escalation", BindingID: fx.binding.BindingID, State: "active",
		RootRuntime: "managed-fixture", RootCatalogSessionID: fx.sessionID, RootNativeSessionID: fx.sessionID,
		CreatedAt: now.Unix(), UpdatedAt: now.Unix()}
	runID := fmt.Sprintf("orun_escalation_crash_%d", fx.runs)
	run := store.ManagedRun{RunID: runID, IdempotencyKey: runID, GroupID: group.GroupID, BindingID: fx.binding.BindingID,
		BindingStateToken: fx.binding.StateToken, Role: fx.binding.Role, ProfileID: fx.binding.ProfileID,
		ProfileSourceDigest: fx.binding.ProfileSourceDigest, ProfileBundleDigest: fx.binding.ProfileBundleDigest,
		SourceEventID: int64(1000 + fx.runs), AdmittedAt: now.Unix(), Citations: []string{}, Detail: map[string]any{}}
	if _, created, err := fx.host.ix.AdmitManagedRun(group, run, agentGroupBudget(fx.binding)); err != nil || !created {
		t.Fatalf("admit: %v %v", created, err)
	}
	if err := fx.host.ix.CompleteManagedRun(runID, "completed", "reply", "Go ahead.", nil,
		map[string]any{"delivery": claimDeliveryReceipt(true)}, "", "", completedAt); err != nil {
		t.Fatal(err)
	}
	return runID
}

// P2-7 / AC-1 / G4 / G5: a receipt left pending by a crash settles — at the
// next start for claims an earlier process stored, and on the sweep once
// older than the delivery TTL, across the whole ask horizon (not only a
// 2·TTL band). Its state is unknown, never a draft; a pending claim whose
// send is proven by the run it launched settles as started. A fresh pending
// receipt is left alone.
func TestPendingReceiptsSettleAsInterrupted(t *testing.T) {
	fx := newEscalationFixture(t, true, nil, []string{"reply"})
	ttl := int64(orchestrationConfig().TTL() / time.Second)
	earlier := fx.completeWithPendingReceipt(t, fx.host.startedAt-5)
	// A new process meets the store: startup reconciliation.
	restarted, err := newOrchestrationManagedHost(fx.host.ix, fx.owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted.close()
	// Written after that start: only the sweep may settle these.
	stale := fx.completeWithPendingReceipt(t, time.Now().Unix()-ttl-60)
	old := fx.completeWithPendingReceipt(t, time.Now().Unix()-3*ttl)
	sent := fx.completeWithPendingReceipt(t, time.Now().Unix()-ttl-60)
	fresh := fx.completeWithPendingReceipt(t, time.Now().Unix())
	// The crash came after the send: a reply run names `sent` as its source.
	group := store.ManagedGroup{GroupID: "grp-escalation", BindingID: fx.binding.BindingID, State: "active",
		RootRuntime: "managed-fixture", RootCatalogSessionID: fx.sessionID, RootNativeSessionID: fx.sessionID}
	resume := store.ManagedRun{RunID: "orun_escalation_resume", IdempotencyKey: "orun_escalation_resume", GroupID: group.GroupID,
		BindingID: fx.binding.BindingID, BindingStateToken: fx.binding.StateToken, Role: fx.binding.Role, Kind: "reply",
		ProfileID: fx.binding.ProfileID, ProfileSourceDigest: fx.binding.ProfileSourceDigest, ProfileBundleDigest: fx.binding.ProfileBundleDigest,
		SourceEventID: 999, AdmittedAt: time.Now().Unix(), Citations: []string{}, Detail: map[string]any{"source_run_id": sent}}
	if _, created, err := fx.host.ix.AdmitManagedRun(group, resume, agentGroupBudget(fx.binding)); err != nil || !created {
		t.Fatalf("admit resume: %v %v", created, err)
	}
	if err := fx.host.ix.StartManagedRun(resume.RunID, "task-escalation-resumed", "orel_escalation_resume", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	state := func(runID string) (string, string) {
		run, _, err := fx.host.ix.ManagedRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		return receiptOf(t, run)
	}
	if got, reason := state(earlier); got != "unknown" || reason != "interrupted" {
		t.Fatalf("earlier-process receipt = %s/%s", got, reason)
	}
	fx.host.expireDeliveriesOnce()
	for name, runID := range map[string]string{"stale": stale, "older than 2·TTL": old} {
		if got, reason := state(runID); got != "unknown" || reason != "interrupted" {
			t.Fatalf("%s receipt = %s/%s", name, got, reason)
		}
	}
	if got, _ := state(sent); got != "started" {
		t.Fatalf("a proven send settled as %s", got)
	}
	if got, _ := state(fresh); got != "pending" {
		t.Fatalf("fresh receipt settled early: %s", got)
	}
	if class, _ := orchestration.ClaimAttention("reply", "unknown"); class != "" {
		t.Fatalf("an unknown outcome was classed %q", class)
	}
}

// swapOrchestrationConfigRejection substitutes a rejected-file outcome for
// tests: the defaults in force plus the decoder's reason.
func swapOrchestrationConfigRejection(reason string) func() {
	restore := swapOrchestrationConfig(defaultOrchestrationConfig())
	orchestrationConfigMu.Lock()
	orchestrationConfigRejected = reason
	orchestrationConfigMu.Unlock()
	return restore
}

// §5 / AC-10: a rejected orchestration file is a roster problem naming the
// decoder's reason; the defaults stay in force; a valid file raises none.
func TestRejectedOrchestrationConfigIsARosterProblem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orchestration.json"), []byte(`{"format_version":1,"delivery":{"no_such_setting":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, origin, rejected := resolveOrchestrationConfig(dir)
	if rejected == "" || !strings.Contains(rejected, "no_such_setting") || origin != "builtin-default-after-error" ||
		config.Delivery.TTLSeconds != defaultOrchestrationConfig().Delivery.TTLSeconds {
		t.Fatalf("resolve = origin %q rejected %q", origin, rejected)
	}
	fixture := newRosterFixture(t)
	roster := func() rosterResponse {
		status, body := fixture.call(t, http.MethodGet, "/api/orchestration/roster", nil)
		if status != http.StatusOK {
			t.Fatalf("roster: %d %s", status, body)
		}
		var out rosterResponse
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	restore := swapOrchestrationConfigRejection(rejected)
	problems := roster().Problems
	restore()
	if len(problems) != 1 || problems[0].Problem.Code != "orchestration_config_rejected" ||
		!strings.Contains(problems[0].Problem.Message, "no_such_setting") || !strings.Contains(problems[0].Problem.Recovery, "restart") {
		t.Fatalf("problems = %+v", problems)
	}
	defer swapOrchestrationConfig(defaultOrchestrationConfig())()
	if problems := roster().Problems; len(problems) != 0 || problems == nil {
		t.Fatalf("a usable file raised problems: %#v", problems)
	}
}

// Code red-team: a send that fails after the ceiling check is never counted
// and leaves no digest, so the next identical continue is not a no-op breach.
func TestRefusedSendIsNeverCounted(t *testing.T) {
	fx := newEscalationFixture(t, true, flowGrantProfile(3), []string{"reply"})
	owned := fx.sourceID
	fx.sourceID = "task-escalation-missing" // the resume cannot find its source task
	run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead.","citations":[]}`)
	if err == nil {
		t.Fatal("a failed resume reported success")
	}
	if state, reason := receiptOf(t, run); state != "unavailable" || reason != "resume_error" {
		t.Fatalf("receipt = %s/%s", state, reason)
	}
	if deliveries, _ := fx.ceilingDeliveries(t); deliveries != 0 {
		t.Fatalf("a failed send was counted: %d", deliveries)
	}
	fx.sourceID = owned
	run, err = fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead.","citations":[]}`)
	if err != nil || run.ErrorClass != "" {
		t.Fatalf("the identical continue after a failed one: err=%v class=%q", err, run.ErrorClass)
	}
	if deliveries, _ := fx.ceilingDeliveries(t); deliveries != 1 {
		t.Fatalf("deliveries = %d, want 1", deliveries)
	}
}

// injectSQL changes the fixture's store behind the host's back: the fault
// injection for the fail-closed pins.
func (fx *escalationFixture) injectSQL(t *testing.T, statement string) {
	t.Helper()
	raw, err := driver.Open("file:"+filepath.Join(fx.root, "index.sqlite"), fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

// G1: a delivery that started but could not be counted fails closed: the
// ceiling is marked breached (the grant disarms) and the run says why.
func TestUncountableDeliveryDisarmsTheGrant(t *testing.T) {
	fx := newEscalationFixture(t, true, flowGrantProfile(3), []string{"reply"})
	fx.injectSQL(t, `CREATE TRIGGER inject_count_failure BEFORE UPDATE OF deliveries ON orchestration_flow_ceiling
		WHEN NEW.deliveries > OLD.deliveries BEGIN SELECT RAISE(ABORT, 'injected count failure'); END`)
	run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := receiptOf(t, run); state != "started" || run.Detail["count_error"] == nil {
		t.Fatalf("receipt %s, detail %+v", state, run.Detail)
	}
	if deliveries, breached := fx.ceilingDeliveries(t); deliveries != 0 || breached != 1 {
		t.Fatalf("an uncounted delivery left the grant armed: deliveries=%d breached=%d", deliveries, breached)
	}
}

// G2: unknown flow state is never "no flow" — a failed grant read refuses the
// delivery instead of skipping conformance, the approval floor and the ceiling.
func TestFlowGrantReadFailureFailsClosed(t *testing.T) {
	fx := newEscalationFixture(t, true, flowGrantProfile(3), []string{"reply"})
	fx.injectSQL(t, `ALTER TABLE orchestration_flow_binding RENAME TO orchestration_flow_binding_hidden`)
	run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead.","citations":[]}`)
	if err == nil {
		t.Fatal("a delivery decided with an unreadable flow grant reported success")
	}
	if state, reason := receiptOf(t, run); state != "unavailable" || reason != "decision_error" {
		t.Fatalf("receipt = %s/%s", state, reason)
	}
	runs, _ := fx.host.ix.ManagedRuns(50)
	for _, other := range runs {
		if other.Kind == "reply" {
			t.Fatalf("the reply was sent: %+v", other)
		}
	}
}

// G14: request_interrupt under an armed grant is refused like any acting
// claim outside the declared reply class.
func TestFlowGrantRefusesRequestInterrupt(t *testing.T) {
	fx := newEscalationFixture(t, true, flowGrantProfile(3), []string{"request-interrupt"}, courseCorrectorProfileSource())
	run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"request_interrupt","message":"Stop.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if run.Detail["flow_grant_refused"] != "action_outside_declared_reply_class" {
		t.Fatalf("request_interrupt under an armed grant: %+v", run.Detail)
	}
}

// G14: the approval floor runs before dry-run and before the transport
// decision: a pending approval on an attended dry-run member is reported as
// the approval, the one outcome the owner must act on.
func TestApprovalFloorPrecedesDryRunAndTransport(t *testing.T) {
	grant := flowGrantProfile(3)
	grant.DryRun = true
	fx := newEscalationFixture(t, false, grant, []string{"reply"})
	previous := approvals
	approvals = newApprovalsHub()
	t.Cleanup(func() { approvals = previous })
	approvals.pending["appr-floor"] = &Approval{ID: "appr-floor", Status: "pending", Runtime: "managed-fixture", NativeSessionID: fx.sessionID}
	run, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := receiptOf(t, run); state != "unavailable" || reason != "pending_approval" {
		t.Fatalf("receipt = %s/%s, want the approval floor first", state, reason)
	}
}
