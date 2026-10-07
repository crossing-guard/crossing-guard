package daemon

// Focused tests for the helper-session-attachment plan: session-scoped kinds
// from hook rows with hook identity, producer-qualified run keys, the launch
// settle window, one fire per fact under the wildcard on the managed path,
// the boundary carrier end to end (enqueue → claim at ingest → run detail),
// replay never claiming, the pending cap, expiry, and the capability answer
// for reply off a session the daemon does not own.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

func sessionKindsFollowerProfileSource() []byte {
	return []byte(strings.Replace(string(naturalFollowerProfileSource()), `stages:
  session.started: Record that the session began.
  session.ended: Review the ended session.
  session.active: Record that the session is active.`, `stages:
  session.turn-started: A turn began.
  session.tool-completed: A tool completed.
  session.turn-ended: A turn ended.`, 1))
}

func bindSessionKindsFollower(t *testing.T, fixture agentHostFixture, bindingID string, watch bool) store.ManagedBinding {
	t.Helper()
	preview := selectManagedProfile(t, fixture.owner, sessionKindsFollowerProfileSource())
	binding, err := fixture.host.putBinding(managedBindingCommand{BindingID: bindingID,
		ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest,
		ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{}, WatchNatural: watch,
		ExpectedStateToken: store.ManagedBindingAbsentToken(bindingID)})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func appendNaturalTurn(t *testing.T, fixture agentHostFixture, kind string, receivedAtMS int64) {
	t.Helper()
	if err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error {
		_, _, err := tx.AppendSessionTurn(store.SessionTurnObservation{ObservationID: "trn_" + kind + "_" + time.Now().Format("150405.000000000"),
			Runtime: "codex", SessionID: "ses-natural", Kind: kind, ObservedAt: receivedAtMS / 1000, ReceivedAtMS: receivedAtMS,
			EvidenceDigest: "sha256-v1:test", CollectorID: "daemon-session-turn-v1", DeliveryAttempts: 1, DeliveryMode: "direct"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func appendNaturalResult(t *testing.T, fixture agentHostFixture, sourceKind, id string, receivedAt int64) {
	t.Helper()
	if _, _, err := fixture.host.ix.AppendResultObservation(store.ResultObservation{ObservationID: "res_" + id, SessionID: "ses-natural",
		Runtime: "codex", Tool: "Bash", NativeCallID: "call-" + id, NativeCallKind: "call_id", SourceKind: sourceKind,
		SourceSequence: id, SourceDigest: "sha256-v1:" + id, State: "success", Completeness: "unavailable",
		CompletedAt: receivedAt, ReceivedAt: receivedAt, DeliveryMode: "direct", DeliveryAttempts: 1}, nil); err != nil {
		t.Fatal(err)
	}
}

func settledConfig() OrchestrationConfig {
	config := defaultOrchestrationConfig()
	config.NaturalSignal.TaskSettleMS = 1
	return config
}

func runsFor(t *testing.T, fixture agentHostFixture, bindingID string) []store.ManagedRun {
	t.Helper()
	runs, err := fixture.host.ix.ManagedRuns(50)
	if err != nil {
		t.Fatal(err)
	}
	out := []store.ManagedRun{}
	for _, run := range runs {
		if run.BindingID == bindingID {
			out = append(out, run)
		}
	}
	return out
}

func signalsOf(runs []store.ManagedRun) map[string]int {
	out := map[string]int{}
	for _, run := range runs {
		signal, _ := run.Detail["signal"].(string)
		out[signal]++
	}
	return out
}

// Turn rows and live tool results fire the session-scoped kinds with the
// hook's own session id as the native identity and the row id as the anchor;
// transcript-derived results never fire; input.requested is inert;
// re-emission is a no-op. (Producer-qualified key collision is pinned
// directly in TestManagedRunIDIsQualifiedByProducer.)
func TestNaturalTurnAndResultRowsFireSessionKindsWithHookIdentity(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	gate := filepath.Join(t.TempDir(), "finish-turn")
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return helperTurnCommand("", `{"action":"no_action","message":"Noted.","citations":[]}`, gate)
	}}
	fixture := naturalFixture(t, driver)
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o600) })
	bindSessionKindsFollower(t, fixture, "agent-session-kinds", true)
	past := time.Now().Add(-10 * time.Second)
	appendNaturalTurn(t, fixture, "turn.started", past.UnixMilli())
	appendNaturalTurn(t, fixture, "input.requested", past.UnixMilli()+1)
	appendNaturalResult(t, fixture, "vendor-transcript", "t1", past.Unix())
	appendNaturalResult(t, fixture, "live-post-tool", "l1", past.Unix())
	fixture.host.emitNaturalSignalsOnce()
	// The second signal queues behind the held child. Release it, then observe
	// the ordinary asynchronous completion/drain before inspecting both runs.
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		if len(runs) < 2 {
			return false
		}
		for _, run := range runs {
			if run.State != "completed" {
				return false
			}
		}
		return true
	})
	signals := signalsOf(runs)
	if len(runs) != 2 || signals["session.turn-started"] != 1 || signals["session.tool-completed"] != 1 {
		t.Fatalf("runs=%d signals=%v problem=%q", len(runs), signals, fixture.host.problem)
	}
	for _, run := range runs {
		group, found, err := fixture.host.ix.ManagedGroup(run.GroupID)
		if err != nil || !found || group.RootNativeSessionID != "ses-natural" || group.RootCatalogSessionID != "ses-natural" || group.RootRuntime != "codex" {
			t.Fatalf("group identity: %+v %v %v", group, found, err)
		}
		// The anchor is the producing row's id: turn rowid 1, and result id 2
		// (the transcript-derived row took id 1 and never fired).
		signal, _ := run.Detail["signal"].(string)
		if want := map[string]int64{"session.turn-started": 1, "session.tool-completed": 2}[signal]; run.SourceEventID != want {
			t.Fatalf("%s anchor must be its row id %d, got %d", signal, want, run.SourceEventID)
		}
	}
	fixture.host.emitNaturalSignalsOnce()
	if again := runsFor(t, fixture, "agent-session-kinds"); len(again) != 2 {
		t.Fatalf("re-emission fired again: %d", len(again))
	}
	appendNaturalTurn(t, fixture, "turn.ended", past.UnixMilli()+2)
	fixture.host.emitNaturalSignalsOnce()
	runs = waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.Detail["signal"] == "session.turn-ended" && run.State == "completed" {
				return true
			}
		}
		return false
	})
	if signals := signalsOf(runs); len(runs) != 3 || signals["session.turn-ended"] != 1 {
		t.Fatalf("turn.ended did not fire once: %v", signals)
	}
}

// A row younger than the settle window waits (the position stops before it)
// and fires on a later pass once the window has passed (red-team M6).
func TestNaturalSettleWindowHoldsFreshRows(t *testing.T) {
	holding := defaultOrchestrationConfig()
	holding.NaturalSignal.TaskSettleMS = 60_000
	restore := swapOrchestrationConfig(holding)
	defer restore()
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Noted.","citations":[]}`)
	}}
	fixture := naturalFixture(t, driver)
	bindSessionKindsFollower(t, fixture, "agent-settle", true)
	appendNaturalTurn(t, fixture, "turn.started", time.Now().UnixMilli())
	fixture.host.emitNaturalSignalsOnce()
	if runs := runsFor(t, fixture, "agent-settle"); len(runs) != 0 {
		t.Fatalf("fresh row fired inside the settle window: %+v", runs)
	}
	if position, _ := fixture.host.ix.OrchestrationStreamPosition(naturalTurnStreamKind); position != 0 {
		t.Fatalf("position advanced past a held row: %d", position)
	}
	restore()
	defer swapOrchestrationConfig(settledConfig())()
	time.Sleep(5 * time.Millisecond)
	fixture.host.emitNaturalSignalsOnce()
	if runs := runsFor(t, fixture, "agent-settle"); len(runs) != 1 {
		t.Fatalf("held row did not fire after the window: %+v", runs)
	}
}

// On the managed path a wildcard profile fires once per fact: the session
// kind, never the superseded task kind alongside it (red-team H3).
func TestManagedTaskFiresSessionKindOnceUnderWildcard(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		claim := jsonTextCommand(`{"action":"no_action","message":"Noted.","citations":[]}`)
		if strings.Contains(request.Prompt, "Crossing Guard") {
			return claim
		}
		// The source reports its session, as every shipped runtime does.
		return `printf '%s\n' '{"session_id":"ses-wild"}'; ` + claim
	}}
	fixture := newAgentHostFixture(t, driver)
	source := strings.Replace(string(naturalFollowerProfileSource()), `stages:
  session.started: Record that the session began.
  session.ended: Review the ended session.
  session.active: Record that the session is active.`, `stages:
  "*": Observe everything.`, 1)
	preview := selectManagedProfile(t, fixture.owner, []byte(source))
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-wild", ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{}, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-wild")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "source", Cwd: fixture.root}, "wild-source"); err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return signalsOf(runs)["session.turn-ended"] >= 1
	})
	signals := signalsOf(runs)
	if signals["session.turn-ended"] != 1 || signals["task.completed"] != 0 || signals["task.tool-completed"] != 0 || signals["session.turn-started"] != 1 {
		t.Fatalf("wildcard fired: %v", signals)
	}
}

type boundaryFixtureDriver struct{ managedDynamicFixtureDriver }

func (d boundaryFixtureDriver) ChatCapability() ChatCapability {
	capability := d.managedDynamicFixtureDriver.ChatCapability()
	capability.MessageDelivery = SessionMessageCapability{Supported: true, Boundary: "fixture boundary", Detail: "test carrier"}
	return capability
}

func (boundaryFixtureDriver) DeliverSessionMessage(_ context.Context, target SessionIdentity, _ string) SessionMessageReceipt {
	if target.NativeID == "" {
		return SessionMessageReceipt{State: "unavailable", Tier: "none"}
	}
	return SessionMessageReceipt{State: "accepted", Tier: "queued-delivery", Carrier: sessionMessageCarrierBoundary, Boundary: "fixture boundary"}
}

// The boundary carrier end to end: the helper's send_message becomes one
// pending record addressed by the group root; a direct, non-duplicate carrier
// receipt for that session claims it and the run reads delivered; duplicate
// and replay receipts never claim; a non-carrier kind never claims; the
// pending cap and expiry are visible outcomes.
func TestBoundaryCarrierEnqueuesClaimsAndSettles(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	driver := boundaryFixtureDriver{managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(`{"action":"send_message","message":"We discussed this before: ADR 0020.","citations":["source.final_message"]}`)
		}
		return jsonTextCommand("Source design question")
	}}}
	fixture := newAgentHostFixture(t, driver)
	chatDrivers["claude"] = driver
	authored := strings.ReplaceAll(string(courseCorrectorProfileSource()), "request-interrupt", "send-message")
	profile := selectManagedProfile(t, fixture.owner, []byte(authored))
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-carrier", ProfileID: profile.ProfileID, ProfileSourceDigest: profile.SourceDigest, ProfileBundleDigest: profile.BundleDigest, ProjectRoot: fixture.root, RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{"send-message"}, AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-carrier")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "Source", SessionID: "ses-carrier", Cwd: fixture.root}, "carrier-source"); err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, r := range runs {
			if v, _ := r.Detail["delivery"].(map[string]any); v != nil && v["state"] == "accepted" {
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
	if delivery["tier"] != "queued-delivery" || delivery["boundary"] != "fixture boundary" || !strings.HasPrefix(delivery["message_id"].(string), "odel_") {
		t.Fatalf("receipt: %+v", delivery)
	}
	record, found, err := fixture.host.ix.SessionDeliveryForRun(run.RunID)
	if err != nil || !found || record.State != "pending" || record.NativeSessionID != "ses-carrier" || record.Runtime != "claude" || !strings.Contains(record.Message, "not operator authorization") {
		t.Fatalf("record: %+v %v %v", record, found, err)
	}
	g := NewGovernor(fixture.host.ix, nil)
	defer swapSessionStatusRefold(func(string, string) {})()
	envelope := func(id, kind, mode string) observation.SessionTurnEnvelope {
		return observation.SessionTurnEnvelope{Schema: observation.SessionTurnSchemaV1, ObservationID: "trn_" + strings.Repeat(id, 32),
			CollectorID: observation.CollectorSessionTurn, Runtime: "claude", SessionID: "ses-carrier", Kind: kind,
			NativeSource: "UserPromptSubmit", ObservedAt: 1_800_000_000, QueuedAt: 1_800_000_000, DeliveryAttempts: 1, DeliveryMode: mode, Carrier: true}
	}
	// A boundary that cannot carry (an old plugin, a hook with no encoder)
	// never claims, whatever its kind: the record waits for a carrier.
	silent := envelope("0", "turn.started", "direct")
	silent.Carrier = false
	receipt, err := postSessionTurn(t, g, silent)
	if err != nil || len(receipt.Deliveries) != 0 {
		t.Fatalf("non-carrier boundary claimed: %+v %v", receipt, err)
	}
	// A non-carrier kind on the direct path never claims.
	receipt, err = postSessionTurn(t, g, envelope("a", "turn.ended", "direct"))
	if err != nil || len(receipt.Deliveries) != 0 {
		t.Fatalf("turn.ended claimed: %+v %v", receipt, err)
	}
	// A replayed carrier kind never claims (red-team H1).
	receipt, err = postSessionTurn(t, g, envelope("b", "turn.started", "replay"))
	if err != nil || len(receipt.Deliveries) != 0 {
		t.Fatalf("replay claimed: %+v %v", receipt, err)
	}
	// The direct carrier boundary drains it exactly once.
	receipt, err = postSessionTurn(t, g, envelope("c", "turn.started", "direct"))
	if err != nil || len(receipt.Deliveries) != 1 || receipt.Deliveries[0].DeliveryID != record.DeliveryID || !strings.Contains(receipt.Deliveries[0].Message, "ADR 0020") {
		t.Fatalf("carrier: %+v %v", receipt, err)
	}
	again, err := postSessionTurn(t, g, envelope("c", "turn.started", "direct"))
	if err != nil || !again.Duplicate || len(again.Deliveries) != 0 {
		t.Fatalf("duplicate claimed: %+v %v", again, err)
	}
	settled, _, _ := fixture.host.ix.ManagedRunByChildTask(run.ChildTaskID)
	delivery, _ = settled.Detail["delivery"].(map[string]any)
	evidence, _ := settled.Detail["delivery_evidence"].(map[string]any)
	if delivery["state"] != "delivered" || evidence["observation_kind"] != "turn.started" || evidence["observation_id"] != "trn_"+strings.Repeat("c", 32) {
		t.Fatalf("run not updated: %+v %+v", delivery, evidence)
	}
	// Cap and expiry are visible outcomes, never a queue. Each pending record
	// belongs to its own run (one record per run by construction); expiry
	// settles those runs, never the already-delivered one.
	for _, id := range []string{"1", "2", "3"} {
		if err := fixture.host.ix.EnqueueSessionDelivery(store.SessionDelivery{DeliveryID: "odel_cap" + id, RunID: "orun_cap" + id, Runtime: "claude",
			NativeSessionID: "ses-carrier", Message: "m", CreatedAt: 1, ExpiresAt: 2}, 3); err != nil {
			t.Fatal(err)
		}
	}
	capped := fixture.host.enqueueSessionDelivery(run, SessionIdentity{Runtime: "claude", NativeID: "ses-carrier"}, "m", SessionMessageReceipt{State: "accepted", Boundary: "b"}, orchestrationConfig())
	if capped.State != "unavailable" || !strings.Contains(capped.Detail, "pending_cap") {
		t.Fatalf("cap outcome: %+v", capped)
	}
	fixture.host.expireDeliveriesOnce()
	for _, id := range []string{"1", "2", "3"} {
		record, found, err := fixture.host.ix.SessionDeliveryForRun("orun_cap" + id)
		if err != nil || !found || record.State != "expired" {
			t.Fatalf("expiry: %+v %v %v", record, found, err)
		}
	}
	settled, _, _ = fixture.host.ix.ManagedRunByChildTask(run.ChildTaskID)
	delivery, _ = settled.Detail["delivery"].(map[string]any)
	if delivery["state"] != "delivered" {
		t.Fatalf("expiry touched a delivered run: %+v", delivery)
	}
}

// Run identity is qualified by the producing stream (plan B1): two events
// equal in id, signal, and task but read from different streams admit two
// runs instead of colliding on the primary key.
func TestManagedRunIDIsQualifiedByProducer(t *testing.T) {
	binding := store.ManagedBinding{StateToken: "tok"}
	task := RuntimeTask{ID: "natural:codex:ses"}
	fromTurn := managedRunID("orun_", binding, TaskEvent{Producer: naturalTurnStreamKind, EventID: 7}, "session.tool-completed", task)
	fromResult := managedRunID("orun_", binding, TaskEvent{Producer: naturalResultStreamKind, EventID: 7}, "session.tool-completed", task)
	fromPump := managedRunID("orun_", binding, TaskEvent{Producer: managedTaskStreamKind, EventID: 7}, "session.tool-completed", task)
	otherKind := managedRunID("orun_", binding, TaskEvent{Producer: managedTaskStreamKind, EventID: 7}, "task.tool-completed", task)
	if fromTurn == fromResult || fromTurn == fromPump || fromResult == fromPump || fromPump == otherKind {
		t.Fatalf("collision: %s %s %s %s", fromTurn, fromResult, fromPump, otherKind)
	}
	if again := managedRunID("orun_", binding, TaskEvent{Producer: naturalTurnStreamKind, EventID: 7}, "session.tool-completed", task); again != fromTurn {
		t.Fatal("run identity must be deterministic")
	}
}

// The pure half of session.messages: only conversation kinds, pinned by
// sequence, newest tail, byte-bounded oldest-first, coverage always present.
func TestBoundTranscriptEventsPinsTailsAndBounds(t *testing.T) {
	events := []harvest.CanonicalEvent{
		{Seq: 1, Kind: "user", Text: "question"}, {Seq: 2, Kind: "thinking", Text: "hidden"},
		{Seq: 3, Kind: "assistant", Text: "answer"}, {Seq: 4, Kind: "tool_call", Name: "Bash", Text: "ls"},
		{Seq: 5, Kind: "tool_result", Text: "files"}, {Seq: 6, Kind: "assistant", Text: "after the trigger"},
	}
	body, highest, omitted, since, err := boundTranscriptEvents(events, 5, 0, 200, 4096)
	if err != nil || highest != 5 || omitted != 0 || since != 0 || strings.Contains(body, "after the trigger") || strings.Contains(body, "hidden") || !strings.Contains(body, "question") {
		t.Fatalf("%s %d %d %v", body, highest, omitted, err)
	}
	body, highest, omitted, _, err = boundTranscriptEvents(events, 0, 0, 2, 4096)
	if err != nil || highest != 6 || omitted != 3 || strings.Contains(body, "question") || !strings.Contains(body, "after the trigger") {
		t.Fatalf("tail: %s %d %d %v", body, highest, omitted, err)
	}
	body, _, omitted, _, err = boundTranscriptEvents(events, 0, 0, 200, 300)
	if err != nil || omitted == 0 || !strings.Contains(body, "omitted_events") {
		t.Fatalf("bound: %s %d %v", body, omitted, err)
	}
	if _, _, _, _, err = boundTranscriptEvents(events, 0, 0, 200, 10); err == nil {
		t.Fatal("impossible bound accepted")
	}
	// Continuation (schema 30): a lower bound drops what the helper session
	// already saw; a bound at or past the head falls back to the pinned tail.
	body, highest, omitted, since, err = boundTranscriptEvents(events, 0, 4, 200, 4096)
	if err != nil || highest != 6 || omitted != 0 || since != 4 || strings.Contains(body, "question") || strings.Contains(body, `"seq":4`) || !strings.Contains(body, "files") {
		t.Fatalf("since: %s %d %d %d %v", body, highest, omitted, since, err)
	}
	body, highest, _, since, err = boundTranscriptEvents(events, 0, 6, 200, 4096)
	if err != nil || highest != 6 || since != 0 || !strings.Contains(body, "question") {
		t.Fatalf("since fallback: %s %d %d %v", body, highest, since, err)
	}
	body, highest, _, since, err = boundTranscriptEvents(events, 5, 9, 200, 4096)
	if err != nil || highest != 5 || since != 0 || !strings.Contains(body, "question") || strings.Contains(body, "after the trigger") {
		t.Fatalf("since past pin: %s %d %d %v", body, highest, since, err)
	}
}

// A row held inside the settle window fires on the emitter's own re-arm,
// with no further ingest and long before the safety sweep.
func TestNaturalSettleHoldRearmsWithoutIngest(t *testing.T) {
	config := defaultOrchestrationConfig()
	config.NaturalSignal.SweepSeconds = 3600
	config.NaturalSignal.CoalesceMS = 20
	config.NaturalSignal.TaskSettleMS = 300
	defer swapOrchestrationConfig(config)()
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Noted.","citations":[]}`)
	}}
	fixture := naturalFixture(t, driver)
	bindSessionKindsFollower(t, fixture, "agent-rearm", true)
	appendNaturalTurn(t, fixture, "turn.started", time.Now().UnixMilli())
	fixture.host.nudge <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(runsFor(t, fixture, "agent-rearm")) == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("held row never fired on the re-arm")
}

// A terminal signal off a session the daemon does not own may not resume it:
// the run records the capability outcome, and the helper never learns why.
func TestReplyOffNaturalSessionIsAnAttendedSessionOutcome(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(`{"action":"reply","message":"Link the ADR before merging.","citations":["source.task"]}`)
		}
		return jsonTextCommand("unused")
	}}
	fixture := naturalFixture(t, driver)
	source := strings.Replace(string(helperAgentProfileSource()), "task.completed", "session.turn-ended", -1)
	// A profile meant for every session reads session-scoped context; the
	// task-only final response would be honestly unavailable off a hook row.
	source = strings.Replace(source, "  - kind: task.final-response\n    required: true", "  - kind: session.tags", 1)
	// The natural source is a Codex session: an auto-reply resumes it with the
	// binding's model, so the helper route is a hosted one the profile allows
	// (a model the fixture runs locally would be refused at save for exactly
	// that cross-runtime resume).
	source = strings.Replace(source, "    - managed-turn\n", "    - managed-turn\n  destination:\n    locality: explicit-local-or-remote\n", 1)
	preview := selectManagedProfile(t, fixture.owner, []byte(source))
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-reply-natural", ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		RouteID: testRouteID(fixture.host, "managed-fixture", fixtureNonLocalModel, nil), GrantedAuthority: []string{"reply"}, AutoAction: true, WatchNatural: true,
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-reply-natural")}); err != nil {
		t.Fatal(err)
	}
	appendNaturalTurn(t, fixture, "turn.ended", time.Now().Add(-10*time.Second).UnixMilli())
	fixture.host.emitNaturalSignalsOnce()
	// Settled means the receipt left pending: the claim is stored before the
	// delivery decision, so "completed" alone races the outcome write
	// (escalation-delivery plan §4, RT-1e).
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, r := range runs {
			receipt, _ := r.Detail["delivery"].(map[string]any)
			if r.BindingID == "agent-reply-natural" && r.State == "completed" && receipt != nil && receipt["state"] != "pending" {
				return true
			}
		}
		return false
	})
	// The capability outcome is merged after the run reads completed.
	runs := runsFor(t, fixture, "agent-reply-natural")
	waitForTerminalsHandled(t, fixture, runs)
	runs = runsFor(t, fixture, "agent-reply-natural")
	for _, run := range runs {
		if run.BindingID != "agent-reply-natural" {
			continue
		}
		if run.Detail["auto_reply_suppressed"] != "attended_session" {
			t.Fatalf("reply off a natural session was not answered as a capability outcome: %+v", run.Detail)
		}
	}
}

// A host meeting a store that already holds turn and result rows must not
// replay them as signals: its cursors are born at the head and only rows
// written afterwards fire (the 2026-09-12 installed defect).
func TestNaturalStreamsBootstrapAtHeadNotHistory(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Noted.","citations":[]}`)
	}}
	root := t.TempDir()
	path := filepath.Join(root, "index.sqlite")
	seed, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := govTx(t, seed, func(tx *store.GovTx) error {
		if err := tx.EnsureSessionRoot("codex", "ses-history", "", root); err != nil {
			return err
		}
		_, _, err := tx.AppendSessionTurn(store.SessionTurnObservation{ObservationID: "trn_history", Runtime: "codex", SessionID: "ses-history",
			Kind: "turn.started", ObservedAt: 1, ReceivedAtMS: 1000, EvidenceDigest: "sha256-v1:h", CollectorID: "c", DeliveryAttempts: 1, DeliveryMode: "direct"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := seed.AppendResultObservation(store.ResultObservation{ObservationID: "res_history", SessionID: "ses-history", Runtime: "codex",
		Tool: "Bash", NativeCallID: "h", NativeCallKind: "call_id", SourceKind: "live-post-tool", SourceSequence: "h", SourceDigest: "sha256-v1:h",
		State: "success", Completeness: "unavailable", CompletedAt: 1, ReceivedAt: 1, DeliveryMode: "direct", DeliveryAttempts: 1}, nil); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()
	fixture := newAgentHostFixtureAt(t, driver, root)
	if err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error { return tx.EnsureSessionRoot("codex", "ses-natural", "", fixture.root) }); err != nil {
		t.Fatal(err)
	}
	bindSessionKindsFollower(t, fixture, "agent-bootstrap", true)
	fixture.host.emitNaturalSignalsOnce()
	if runs := runsFor(t, fixture, "agent-bootstrap"); len(runs) != 0 {
		t.Fatalf("history replayed as signals: %+v", runs)
	}
	for _, kind := range []string{naturalTurnStreamKind, naturalResultStreamKind} {
		if position, _ := fixture.host.ix.OrchestrationStreamPosition(kind); position == 0 {
			t.Fatalf("%s was not born at the head", kind)
		}
	}
	appendNaturalTurn(t, fixture, "turn.started", time.Now().Add(-10*time.Second).UnixMilli())
	fixture.host.emitNaturalSignalsOnce()
	if signals := signalsOf(runsFor(t, fixture, "agent-bootstrap")); signals["session.turn-started"] != 1 {
		t.Fatalf("a row written after bootstrap must fire: %v", signals)
	}
}

// postSessionTurn drives one turn boundary through the HTTP handler, the
// only path that claims helper messages (delivery-claim-on-reply plan D1),
// with a live hook deadline.
func postSessionTurn(t *testing.T, g *Governor, turn observation.SessionTurnEnvelope) (observation.SessionTurnReceipt, error) {
	t.Helper()
	return postSessionTurnWith(t, g, turn, time.Now().Add(observation.HookDeliveryBudget), nil, httptest.NewRecorder())
}

func postSessionTurnWith(t *testing.T, g *Governor, turn observation.SessionTurnEnvelope, deadline time.Time,
	ctx context.Context, w http.ResponseWriter) (observation.SessionTurnReceipt, error) {
	t.Helper()
	previous := governor
	governor = g
	defer func() { governor = previous }()
	body, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/govern/session-turn/v1", bytes.NewReader(body))
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	if !deadline.IsZero() {
		r.Header.Set(observation.HookDeadlineHeader, strconv.FormatInt(deadline.UnixMilli(), 10))
	}
	handleGovernSessionTurnV1(w, r)
	var receipt observation.SessionTurnReceipt
	recorder, ok := w.(*httptest.ResponseRecorder)
	if !ok {
		return receipt, nil
	}
	if recorder.Code != http.StatusOK {
		return receipt, fmt.Errorf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	return receipt, json.Unmarshal(recorder.Body.Bytes(), &receipt)
}

type failingReplyWriter struct{ header http.Header }

func (f *failingReplyWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *failingReplyWriter) Write([]byte) (int, error) { return 0, errors.New("hook stopped waiting") }
func (f *failingReplyWriter) WriteHeader(int)           {}

// A helper message is claimed only when the hook can still read the reply:
// a deadline already past, or inside the claim margin, and a request whose
// client has gone leave it pending; a reply that fails to write puts it back
// to pending; a live reply carries it and marks it delivered. Measured before
// this change: 7 of 15 "delivered" messages never reached a transcript.
func TestDeliveryIsClaimedOnlyOnAReplyTheHookCanRead(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	defer swapSessionStatusRefold(func(string, string) {})()
	// Turn ingest resolves catalog ids through harvest, which scans the real
	// vendor trees under $HOME; a scratch home keeps each post fast so the
	// deadlines below measure the claim, not a developer's session history.
	t.Setenv("HOME", t.TempDir())
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := NewGovernor(ix, nil)
	if err := ix.EnqueueSessionDelivery(store.SessionDelivery{DeliveryID: "odel_reply", RunID: "orun_reply", Runtime: "claude",
		NativeSessionID: "ses-reply", Message: "We discussed this before.", CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 600}, 3); err != nil {
		t.Fatal(err)
	}
	turn := func(id string) observation.SessionTurnEnvelope {
		return observation.SessionTurnEnvelope{Schema: observation.SessionTurnSchemaV1, ObservationID: "trn_" + strings.Repeat(id, 32),
			CollectorID: observation.CollectorSessionTurn, Runtime: "claude", SessionID: "ses-reply", Kind: "turn.started",
			NativeSource: "UserPromptSubmit", ObservedAt: 1_800_000_000, QueuedAt: 1_800_000_000, DeliveryAttempts: 1, DeliveryMode: "direct", Carrier: true}
	}
	state := func() store.SessionDelivery {
		record, found, err := ix.SessionDeliveryForRun("orun_reply")
		if err != nil || !found {
			t.Fatalf("record: %v %v", found, err)
		}
		return record
	}
	// The hook already gave up.
	receipt, err := postSessionTurnWith(t, g, turn("1"), time.Now().Add(-time.Second), nil, httptest.NewRecorder())
	if err != nil || len(receipt.Deliveries) != 0 || state().State != "pending" {
		t.Fatalf("past deadline claimed: %+v %v %+v", receipt, err, state())
	}
	// Less than the claim margin remains.
	receipt, err = postSessionTurnWith(t, g, turn("2"), time.Now().Add(50*time.Millisecond), nil, httptest.NewRecorder())
	if err != nil || len(receipt.Deliveries) != 0 || state().State != "pending" {
		t.Fatalf("inside the margin claimed: %+v %v %+v", receipt, err, state())
	}
	// The client connection is gone (older hook, no deadline header).
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err = postSessionTurnWith(t, g, turn("3"), time.Time{}, gone, httptest.NewRecorder())
	if err != nil || len(receipt.Deliveries) != 0 || state().State != "pending" {
		t.Fatalf("cancelled request claimed: %+v %v %+v", receipt, err, state())
	}
	// The reply fails to reach the hook: claimed, then released.
	if _, err := postSessionTurnWith(t, g, turn("4"), time.Now().Add(30*time.Second), nil, &failingReplyWriter{}); err != nil {
		t.Fatal(err)
	}
	if record := state(); record.State != "pending" || record.DeliveredObservationID != "" || record.DeliveredAt != 0 {
		t.Fatalf("a failed reply must release the claim: %+v", record)
	}
	// The claim itself waits on the governor's write lock past the hook's
	// deadline: claimed, then released before the reply, never carried.
	g.writeMu.Lock()
	go func() { time.Sleep(2500 * time.Millisecond); g.writeMu.Unlock() }()
	receipt, err = postSessionTurnWith(t, g, turn("6"), time.Now().Add(2*time.Second), nil, httptest.NewRecorder())
	if err != nil || len(receipt.Deliveries) != 0 {
		t.Fatalf("a claim that outlived the deadline must not be carried: %+v %v", receipt, err)
	}
	if record := state(); record.State != "pending" || record.DeliveredObservationID != "" {
		t.Fatalf("a claim that outlived the deadline must be released: %+v", record)
	}
	// A reply the hook can read carries it and marks it delivered.
	receipt, err = postSessionTurnWith(t, g, turn("5"), time.Now().Add(30*time.Second), nil, httptest.NewRecorder())
	if err != nil || len(receipt.Deliveries) != 1 || receipt.Deliveries[0].DeliveryID != "odel_reply" {
		t.Fatalf("live reply did not carry the message: %+v %v", receipt, err)
	}
	if record := state(); record.State != "delivered" || record.DeliveredObservationID != "trn_"+strings.Repeat("5", 32) {
		t.Fatalf("delivered record: %+v", record)
	}
}

// Only an allowed pre-tool boundary carries helper messages: a deny hook
// prints its denial and drops the receipt's deliveries (review F3).
func TestObserveBoundaryCarriesOnlyWhenTheToolRuns(t *testing.T) {
	for decision, want := range map[string]bool{"allow": true, "deny": false, "ask": false, "": false} {
		if got := observeBoundaryCarries(observation.Envelope{Carrier: true, Decision: decision}); got != want {
			t.Fatalf("decision %q carries=%v, want %v", decision, got, want)
		}
	}
	if observeBoundaryCarries(observation.Envelope{Carrier: false, Decision: "allow"}) {
		t.Fatal("a hook that cannot print context never carries")
	}
	// With enforcement off the tool runs and the hook prints the reply's
	// context, so a would-block allow carries like any other allow
	// (enforcement-off-carrier-delivery plan D2).
	wouldBlock := observation.Envelope{Carrier: true, Decision: "allow", Rule: "deny-alpha",
		Reason: "WOULD BLOCK (Blocked by rule deny-alpha) — enforcement off: maintenance window"}
	if !observeBoundaryCarries(wouldBlock) {
		t.Fatal("a would-block allow is a boundary where the tool runs and must carry")
	}
}
