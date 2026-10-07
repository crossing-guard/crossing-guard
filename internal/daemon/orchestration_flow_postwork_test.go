package daemon

// Postwork-fold tests for the flows pilot's fixed defects (2026-09-27 owner
// review): stage placement runs the stage's membership query; evaluation
// admits AND binds; transitions evaluate on live signals with the When tag
// condition; the pending-approval floor is an observed-state check.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

// fixtureFlow builds a two-stage flow exercising the owner's original
// vision: building (uncommitted work) continues until a red-teamed fact,
// then pops to review.
func fixtureFlow() SavedFlow {
	return SavedFlow{ID: "fix", Name: "Fix flow", MembershipTags: []string{"flow:active"},
		Stages: []FlowStage{
			{ID: "building", Name: "Building", Membership: "tag:work=uncommitted",
				Profiles: []FlowStageProfile{{ProfileID: "continue-helper", MaxDeliveries: 3,
					ReplyClass: "short", ReplyClassBytes: 64}},
				Transitions: []FlowStageTransit{{Kind: "session.turn-ended", When: "tag:phase=red-teamed", To: "review"}}},
			{ID: "review", Name: "Waiting to approve implementation", Membership: "tag:phase=red-teamed",
				Profiles: []FlowStageProfile{{ProfileID: "review-helper", MaxDeliveries: 1,
					ReplyClass: "short", ReplyClassBytes: 64}}},
		}}
}

// TestFlowPlacementRunsTheStageMembershipQuery pins the placement fold:
// a candidate carrying the flow tag is placed in the stage whose QUERY
// matches its facts — not always stages[0].
func TestFlowPlacementRunsTheStageMembershipQuery(t *testing.T) {
	ix := openFlowBoundaryIndex(t)
	flow := fixtureFlow()
	if err := validateSavedFlow(flow); err != nil {
		t.Fatal(err)
	}
	if err := ix.EnableFlow(flow.ID, []byte(`{"id":"fix"}`), 100); err != nil {
		t.Fatal(err)
	}
	// The candidate: flow tag + red-teamed phase, NO uncommitted-work fact.
	// Stage "building" requires tag:work=uncommitted — it must NOT match;
	// stage "review" requires phase=red-teamed — it must.
	target := store.SessionOwnerTarget{Runtime: "claude", SessionID: "s1", Title: "T", Cwd: "/w", TouchedAt: 5}
	if err := ix.ApplySessionOwnerTags([]store.SessionOwnerTarget{target},
		[]store.SessionOwnerTagValue{{Key: "flow", Value: "active"}, {Key: "phase", Value: "red-teamed"}}, 100); err != nil {
		t.Fatal(err)
	}
	host := &orchestrationManagedHost{ix: ix}
	candidates, err := host.flowTaggedCandidates(flow)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %+v (%v)", candidates, err)
	}
	if stage := host.flowPlacementStage(flow, candidates[0]); stage != "review" {
		t.Fatalf("placement = %q, want review (the query decides, not the array order)", stage)
	}
	// The same session with an uncommitted-work fact matches building.
	if err := ix.UpsertSessionStateDirect("s1", store.FactUncommittedWork, "change_record=1", 200); err != nil {
		t.Fatal(err)
	}
	if stage := host.flowPlacementStage(flow, candidates[0]); stage != "building" {
		t.Fatalf("placement with WIP fact = %q, want building", stage)
	}
}

// TestFlowWhenHoldsMatchesTagCondition pins the transition fold's When
// clause: the owner's "until a red-teamed tag appears" is a tag-term query
// against the member's CURRENT vocabulary.
func TestFlowWhenHoldsMatchesTagCondition(t *testing.T) {
	ix := openFlowBoundaryIndex(t)
	host := &orchestrationManagedHost{ix: ix}
	candidate := flowMemberCandidate{Runtime: "claude", SessionID: "s2"}
	// No phase tag: the When clause does not hold.
	if host.flowWhenHolds("tag:phase=red-teamed", candidate) {
		t.Fatal("When held without the tag")
	}
	// The tag arrives: the When clause holds.
	target := store.SessionOwnerTarget{Runtime: "claude", SessionID: "s2", Title: "T", Cwd: "/w"}
	if err := ix.ApplySessionOwnerTags([]store.SessionOwnerTarget{target},
		[]store.SessionOwnerTagValue{{Key: "phase", Value: "red-teamed"}}, 100); err != nil {
		t.Fatal(err)
	}
	if !host.flowWhenHolds("tag:phase=red-teamed", candidate) {
		t.Fatal("When did not hold with the tag present")
	}
	// The uncommitted-work FACT satisfies the same grammar (facets are
	// vocabulary too), so a When can reference observed state.
	if err := ix.UpsertSessionStateDirect("s2", store.FactUncommittedWork, "change_record=2", 200); err != nil {
		t.Fatal(err)
	}
	if !host.flowWhenHolds("tag:work=uncommitted", candidate) {
		t.Fatal("When did not hold against a settled fact")
	}
}

// TestFlowEvaluationBindsStageProfiles pins the evaluation-binds fold:
// members the initial evaluation admits get stage bindings, so the
// stalled backlog population receives the grant — the defect the owner
// review caught (admitted-but-unbound members could never fire).
func TestFlowEvaluationBindsStageProfiles(t *testing.T) {
	host, ix, flow := flowTestHost(t)
	target := store.SessionOwnerTarget{Runtime: "claude", SessionID: "s3", Title: "T", Cwd: "/w", TouchedAt: 5}
	if err := ix.ApplySessionOwnerTags([]store.SessionOwnerTarget{target},
		[]store.SessionOwnerTagValue{{Key: "flow", Value: "active"}}, 100); err != nil {
		t.Fatal(err)
	}
	// The WIP fact so placement puts the member in "building".
	if err := ix.UpsertSessionStateDirect("s3", store.FactUncommittedWork, "change_record=3", 150); err != nil {
		t.Fatal(err)
	}
	// Enablement: the flow record with a pending evaluation.
	if err := host.flowEnablementSync(160); err != nil {
		t.Fatal(err)
	}
	members, err := ix.FlowMembers(flow.ID)
	if err != nil || len(members) != 1 {
		t.Fatalf("members = %+v (%v)", members, err)
	}
	if members[0].Stage != "building" || members[0].Excluded != "" {
		t.Fatalf("member = %+v, want stage building unexcluded", members[0])
	}
	bindings, err := ix.FlowBindings(flow.ID, "claude", "s3")
	if err != nil || len(bindings) == 0 {
		t.Fatalf("evaluation admitted but bound nothing: %+v (%v)", bindings, err)
	}
	found := false
	for _, binding := range bindings {
		if binding.ProfileID == "continue-helper" && binding.Stage == "building" && binding.InertReason == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("continue-helper binding missing: %+v", bindings)
	}
	// The ceiling row exists (the arm path created it): the grant can fire.
	ceiling, foundRow, err := ix.FlowCeiling(flow.ID, "claude", "s3")
	if err != nil || !foundRow || ceiling.Deliveries != 0 {
		t.Fatalf("ceiling row missing after evaluation bind: %+v found=%v (%v)", ceiling, foundRow, err)
	}
	// Restart discipline (pass-2 C2): a second enablement pass re-runs
	// nothing — evaluation_state is done, members and bindings unchanged.
	if err := host.flowEnablementSync(170); err != nil {
		t.Fatal(err)
	}
	membersAgain, _ := ix.FlowMembers(flow.ID)
	bindingsAgain, _ := ix.FlowBindings(flow.ID, "claude", "s3")
	if len(membersAgain) != 1 || len(bindingsAgain) != len(bindings) {
		t.Fatalf("re-enable duplicated state: members=%d bindings=%d", len(membersAgain), len(bindingsAgain))
	}
}

// TestFlowPendingApprovalFloorIsStateNotProse pins the floor: with a
// pending approval on the member session, the grant delivers nothing;
// with none, the floor is open. No prose is examined.
func TestFlowPendingApprovalFloorIsStateNotProse(t *testing.T) {
	ix := openFlowBoundaryIndex(t)
	host := &orchestrationManagedHost{ix: ix}
	// The installed approvals hub reads empty in this test: the floor must
	// read open for a session with nothing pending.
	if host.flowPendingApprovalFloor("claude", "s-none", "s-none") {
		t.Fatal("floor tripped with no pending approval")
	}
	// A pending approval in the hub's pending state trips the floor for its
	// exact session, and only there. The hub entry is written directly
	// under its lock (the hub's admit path mints responder capabilities
	// and waiters, which the floor never reads).
	approval := &Approval{ID: "appr-floor-test", Runtime: "claude",
		CatalogSessionID: "s-pend", NativeSessionID: "s-pend", Status: "pending"}
	approvals.mu.Lock()
	approvals.pending[approval.ID] = approval
	approvals.mu.Unlock()
	t.Cleanup(func() {
		approvals.mu.Lock()
		delete(approvals.pending, approval.ID)
		approvals.mu.Unlock()
	})
	if !host.flowPendingApprovalFloor("claude", "s-pend", "s-pend") {
		t.Fatal("floor open with a pending approval on the exact session")
	}
	if host.flowPendingApprovalFloor("claude", "s-other", "s-other") {
		t.Fatal("floor tripped for a different session")
	}
}

// flowTestHost binds a host to a throwaway store (the host reads the same
// index the test writes) whose data dir holds the fixture flow, with
// presence available and nobody open — without it the
// shared-checkout floor fails closed to a class the v39 CHECK already
// allowed, and a no-stage test would pass on the defective code (plan
// flow-member-exclusion-class RT-5).
func flowTestHost(t *testing.T) (*orchestrationManagedHost, *store.Index, SavedFlow) {
	t.Helper()
	// The tag emitter resolves catalog ids through the vendor transcript
	// finder; an empty HOME keeps it off the developer's real transcripts.
	t.Setenv("HOME", t.TempDir())
	data := t.TempDir()
	ix, err := store.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	prior := indexPath()
	setIndexPath(data)
	t.Cleanup(func() { setIndexPath(prior) })
	flow := fixtureFlow()
	if err := writeFlows(flowsPath(data), []SavedFlow{flow}); err != nil {
		t.Fatal(err)
	}
	priorPresence := nativeSessionActivityRef.Load()
	presence := sessionactivity.NewService(func(context.Context, time.Time) (sessionactivity.Capability, []sessionactivity.Item) {
		return sessionactivity.Capability{Status: "available"}, nil
	}, time.Hour, time.Second)
	nativeSessionActivityRef.Store(presence)
	t.Cleanup(func() {
		nativeSessionActivityRef.Store(priorPresence)
		presence.Close()
	})
	return &orchestrationManagedHost{ix: ix}, ix, flow
}

func requireNoStageMember(t *testing.T, ix *store.Index, flowID, sessionID string) {
	t.Helper()
	members, err := ix.FlowMembers(flowID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.SessionID != sessionID {
			continue
		}
		if member.Excluded != store.FlowMemberExcludedNoStage || member.Stage != "" || member.ExclusionReason == "" {
			t.Fatalf("member = %+v, want excluded no-stage with a reason", member)
		}
		return
	}
	t.Fatalf("no member row for %s: %+v", sessionID, members)
}

// TestFlowEvaluationRecordsNoStageCandidate is A-6a: a flow-tagged session
// whose facts match no stage's membership query is recorded excluded
// no-stage by the initial evaluation (ReplaceFlowMembers), the evaluation
// completes, and no problem is raised. On schema 39 the CHECK refused the
// class and the evaluation failed every sweep.
func TestFlowEvaluationRecordsNoStageCandidate(t *testing.T) {
	host, ix, flow := flowTestHost(t)
	// Flow tag only: neither tag:work=uncommitted (building) nor
	// tag:phase=red-teamed (review) holds.
	target := store.SessionOwnerTarget{Runtime: "claude", SessionID: "s1", Title: "T", Cwd: "/w", TouchedAt: 5}
	if err := ix.ApplySessionOwnerTags([]store.SessionOwnerTarget{target},
		[]store.SessionOwnerTagValue{{Key: "flow", Value: "active"}}, 100); err != nil {
		t.Fatal(err)
	}
	if err := host.flowEnablementSync(160); err != nil {
		t.Fatal(err)
	}
	requireNoStageMember(t, ix, flow.ID, "s1")
	records, err := ix.OrchestrationFlowRecords()
	if err != nil || len(records) != 1 || records[0].EvaluationState != "done" {
		t.Fatalf("records = %+v (%v), want one flow with evaluation done", records, err)
	}
	if host.problem != "" {
		t.Fatalf("problem = %q", host.problem)
	}
}

// TestFlowLiveTagSignalRecordsNoStageMember is A-6b: the flow's evaluation
// completed with no candidates; then the owner tags a session whose facts
// match no stage. The real journal write is translated by the real tag
// emitter with the flow consumer as the route, and UpsertFlowMember records
// the no-stage member and the flow's journal position advances.
func TestFlowLiveTagSignalRecordsNoStageMember(t *testing.T) {
	host, ix, flow := flowTestHost(t)
	if err := host.flowEnablementSync(100); err != nil {
		t.Fatal(err)
	}
	if members, err := ix.FlowMembers(flow.ID); err != nil || len(members) != 0 {
		t.Fatalf("evaluation with no tagged session admitted %+v (%v)", members, err)
	}
	target := store.SessionOwnerTarget{Runtime: "claude", SessionID: "s2", Title: "T", Cwd: "/w", TouchedAt: 5}
	if err := ix.ApplySessionOwnerTags([]store.SessionOwnerTarget{target},
		[]store.SessionOwnerTagValue{{Key: "flow", Value: "active"}}, 200); err != nil {
		t.Fatal(err)
	}
	head, err := ix.SessionOwnerTagChangeHead()
	if err != nil || head < 1 {
		t.Fatalf("journal head = %d (%v)", head, err)
	}
	if err := emitTagChangeSignals(ix, func(signal naturalSessionSignal) error {
		host.flowSignalConsumer(signal)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireNoStageMember(t, ix, flow.ID, "s2")
	position, err := ix.FlowJournalPosition(flow.ID)
	if err != nil || position != head {
		t.Fatalf("flow journal position = %d (%v), want %d", position, err, head)
	}
	if host.problem != "" {
		t.Fatalf("problem = %q", host.problem)
	}
}

// TestFlowMemberClassesAreStoreConstants is A-7: every flow member class
// written outside the store is a store.FlowMemberExcluded* constant (or ""
// for "not excluded"). The CHECK is generated from those constants, so any
// other value is how schema 39's CHECK drifted from the code. It walks the
// syntax tree of every shipped file under internal/ and cmd/, covering both
// assignment (member.Excluded = …) and keyed composite literals
// (Excluded: …). Not followed: a value routed through a local variable or a
// positional composite literal — review owns those; the store's CHECK still
// refuses any unlisted class at runtime.
func TestFlowMemberClassesAreStoreConstants(t *testing.T) {
	allowed := func(expr ast.Expr) bool {
		if literal, ok := expr.(*ast.BasicLit); ok {
			return literal.Kind == token.STRING && literal.Value == `""`
		}
		selector, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := selector.X.(*ast.Ident)
		return ok && pkg.Name == "store" && strings.HasPrefix(selector.Sel.Name, "FlowMemberExcluded")
	}
	root := organizationRepositoryRoot(t)
	for _, path := range shippedGoFiles(t, "internal", "cmd") {
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		report := func(node ast.Node) {
			relative, _ := filepath.Rel(root, path)
			t.Errorf("%s:%d writes a flow member class that is not a store.FlowMemberExcluded* constant",
				relative, fileSet.Position(node.Pos()).Line)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				for index, left := range typed.Lhs {
					if selector, ok := left.(*ast.SelectorExpr); ok && selector.Sel.Name == "Excluded" &&
						index < len(typed.Rhs) && !allowed(typed.Rhs[index]) {
						report(typed)
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := typed.Key.(*ast.Ident); ok && key.Name == "Excluded" && !allowed(typed.Value) {
					report(typed)
				}
			}
			return true
		})
	}
}
