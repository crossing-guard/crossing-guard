package daemon

// The host's half of the socket tier (session-message-layer plan §5.3, D5):
// a socket receipt is recorded on the run detail, the pending store holds
// nothing, and a later hook boundary drains nothing. The crash window's
// fault injection lives here because the merge belongs to the host.

import (
	"context"
	"strings"
	"testing"

	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

// socketHostFixtureDriver answers the helper's send_message claim with a
// socket receipt, as the real adapter does after a successful post.
type socketHostFixtureDriver struct{ boundaryFixtureDriver }

func (d socketHostFixtureDriver) DeliverSessionMessage(_ context.Context, target SessionIdentity, _ string) SessionMessageReceipt {
	_ = target
	return SessionMessageReceipt{State: "accepted", Tier: "socket-post", Carrier: sessionMessageCarrierSocket,
		Boundary: "the session's inbox (peer message; vendor controls apply)",
		Detail:   "Transport accepted; consumption is not confirmed."}
}

func TestSocketReceiptMergesWithoutEnqueue(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	driver := socketHostFixtureDriver{boundaryFixtureDriver{managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(`{"action":"send_message","message":"Peer message for the open session.","citations":[]}`)
		}
		return jsonTextCommand("Source design question")
	}}}}
	fixture := newAgentHostFixture(t, driver)
	chatDrivers["claude"] = &driver
	authored := strings.ReplaceAll(string(courseCorrectorProfileSource()), "request-interrupt", "send-message")
	profile := selectManagedProfile(t, fixture.owner, []byte(authored))
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-socket", ProfileID: profile.ProfileID, ProfileSourceDigest: profile.SourceDigest, ProfileBundleDigest: profile.BundleDigest, ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{"send-message"}, AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-socket")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "Source", SessionID: "ses-socket", Cwd: fixture.root}, "socket-source"); err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, r := range runs {
			if v, _ := r.Detail["delivery"].(map[string]any); v != nil && v["tier"] == "socket-post" {
				return true
			}
		}
		return false
	})
	var run store.ManagedRun
	for _, r := range runs {
		if r.Action == "send_message" {
			run = r
		}
	}
	delivery, _ := run.Detail["delivery"].(map[string]any)
	if delivery["tier"] != "socket-post" || delivery["carrier"] != sessionMessageCarrierSocket || delivery["state"] != "accepted" {
		t.Fatalf("receipt: %+v", delivery)
	}
	// D5: no pending record exists for a socket post.
	if _, found, err := fixture.host.ix.SessionDeliveryForRun(run.RunID); found || err != nil {
		t.Fatalf("socket tier must leave the pending store untouched: %+v %v %v", nil, found, err)
	}
	// A later hook boundary drains nothing for this session.
	g := NewGovernor(fixture.host.ix, nil)
	defer swapSessionStatusRefold(func(string, string) {})()
	envelope := observation.SessionTurnEnvelope{Schema: observation.SessionTurnSchemaV1, ObservationID: "trn_" + strings.Repeat("a", 32),
		CollectorID: observation.CollectorSessionTurn, Runtime: "claude", SessionID: "ses-socket", Kind: "turn.started",
		NativeSource: "UserPromptSubmit", ObservedAt: 1_800_000_000, QueuedAt: 1_800_000_000, DeliveryAttempts: 1, DeliveryMode: "direct", Carrier: true}
	receipt, err := postSessionTurn(t, g, envelope)
	if err != nil || len(receipt.Deliveries) != 0 {
		t.Fatalf("boundary claimed a socket delivery: %+v %v", receipt, err)
	}
	// Nothing settled, nothing claimed: the store never saw the delivery.
	if _, found, err := fixture.host.ix.SessionDeliveryForRun(run.RunID); found || err != nil {
		t.Fatalf("the store saw the delivery: %v %v", found, err)
	}
}

// The crash window (confirming pass C-5, §10.6): a fault between the
// adapter's post and the run-detail merge loses the audit row while the
// receiver still got the message — replay never sends a second copy because
// the pending store holds nothing. The fault is injected by a driver whose
// post succeeds and whose receipt the host then fails to record, which the
// replay path must survive honestly.
func TestSocketCrashWindowReplaySendsAtMostOnce(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	posts := 0
	driver := crashWindowDriver{posts: &posts, socketHostFixtureDriver: socketHostFixtureDriver{boundaryFixtureDriver{managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(`{"action":"send_message","message":"Crash window message.","citations":[]}`)
		}
		return jsonTextCommand("Source design question")
	}}}}}
	fixture := newAgentHostFixture(t, driver)
	chatDrivers["claude"] = &driver
	authored := strings.ReplaceAll(string(courseCorrectorProfileSource()), "request-interrupt", "send-message")
	profile := selectManagedProfile(t, fixture.owner, []byte(authored))
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-crash", ProfileID: profile.ProfileID, ProfileSourceDigest: profile.SourceDigest, ProfileBundleDigest: profile.BundleDigest, ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{"send-message"}, AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-crash")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "Source", SessionID: "ses-crash", Cwd: fixture.root}, "crash-source"); err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, r := range runs {
			if v, _ := r.Detail["delivery"].(map[string]any); v != nil && v["tier"] == "socket-post" {
				return true
			}
		}
		return false
	})
	postsAfterFirst := *driver.posts
	if postsAfterFirst != 1 {
		t.Fatalf("posts after the first completion: %d", postsAfterFirst)
	}
	var run store.ManagedRun
	for _, r := range runs {
		if r.Action == "send_message" {
			run = r
		}
	}
	// Replay the completed claim: the receipt is recorded once, the post
	// happens once, and no row exists for a second transport to carry.
	if err := fixture.host.finishManagedChild(run, TaskEvent{TaskID: run.ChildTaskID, Kind: "task.completed"}); err != nil {
		t.Fatal(err)
	}
	if *driver.posts != postsAfterFirst {
		t.Fatalf("replay re-posted: %d → %d", postsAfterFirst, *driver.posts)
	}
	// The record was never written, so replay also cannot claim anything.
	if _, found, err := fixture.host.ix.SessionDeliveryForRun(run.RunID); found || err != nil {
		t.Fatalf("replay found a record to claim: %v %v", found, err)
	}
}

type crashWindowDriver struct {
	socketHostFixtureDriver
	posts *int
}

func (d *crashWindowDriver) DeliverSessionMessage(ctx context.Context, target SessionIdentity, message string) SessionMessageReceipt {
	*d.posts++
	return d.socketHostFixtureDriver.DeliverSessionMessage(ctx, target, message)
}
