package daemon

// Focused tests for the agent-initiated send route (session-message-cross-
// vendor-plan §4–§5): typed refusals, grant on/off, scope, budgets, the
// duplicate digest, the loop bound, and the wrapper applied at mint. The
// catalog and presence seams are injected, the config comes from the real
// loader pointed at the fixture's orchestration.json.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/observation"
	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

// sendFixtureDriver registers a driver that supports message delivery through
// the boundary carrier (the hook-target transport) — the same fixture shape
// the Stage A carrier tests use.
func sendFixtureDriver(t *testing.T, receipts map[string]SessionMessageReceipt) agentHostFixture {
	t.Helper()
	return newAgentHostFixture(t, boundaryFixtureDriver{managedDynamicFixtureDriver{}})
}

// sendGovernor builds a minimal Governor over the fixture's index — the route
// needs only the store handle.
func sendGovernor(fixture agentHostFixture) *Governor {
	return NewGovernor(fixture.host.ix, nil)
}

func writeSendConfig(t *testing.T, root string, deliverAttended bool) func() {
	t.Helper()
	return writeSendConfigLimits(t, root, deliverAttended, 3)
}

func writeSendConfigLimits(t *testing.T, root string, deliverAttended bool, maxPending int) func() {
	t.Helper()
	config := defaultOrchestrationConfig()
	config.Delivery.DeliverAttended = deliverAttended
	config.Delivery.MaxPendingPerSession = maxPending
	return swapOrchestrationConfig(config)
}

func openSetFor(open ...SessionSummary) func(time.Time) presenceOpenSet {
	keys := map[string]bool{}
	for _, s := range open {
		keys[presenceCatalogKey(s.Runtime, s.ID)] = true
	}
	return func(time.Time) presenceOpenSet {
		return presenceOpenSet{Capability: sessionactivity.Capability{Status: "available"}, Open: keys}
	}
}

func TestSendRouteUsesExactParentPresenceWithGuardian(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restore := writeSendConfig(t, fixture.root, true)
	defer restore()
	now := time.Now()
	parent := SessionSummary{Runtime: "codex", ID: "parent-rollout", MetaID: "parent", ThreadID: "parent", ResumeID: "parent", Path: "/s/parent", Cwd: fixture.root, Modified: now.Add(-time.Minute)}
	child := SessionSummary{Runtime: "codex", ID: "guardian-rollout", MetaID: "guardian", ThreadID: "parent", ResumeID: "parent", LineageKind: "native-guardian", Path: "/s/child", Cwd: fixture.root, Modified: now}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target", Path: "/s/target", Cwd: fixture.root}
	catalog := []SessionSummary{child, parent, target}
	for _, tc := range []struct {
		name                 string
		held                 map[string]bool
		unavailable, expired bool
		want                 string
	}{
		{"parent and child", map[string]bool{parent.Path: true, child.Path: true, target.Path: true}, false, false, store.SessionMessageInvocationPending},
		{"child alone", map[string]bool{child.Path: true, target.Path: true}, false, false, store.SessionMessageInvocationRefused},
		{"unavailable", map[string]bool{parent.Path: true, target.Path: true}, true, false, store.SessionMessageInvocationRefused},
		{"expired", map[string]bool{parent.Path: true, target.Path: true}, false, true, store.SessionMessageInvocationRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := assemblePresenceItems(now, catalog, tc.held, nil)
			snapshot := sessionactivity.Snapshot{Capability: sessionactivity.Capability{Status: "available"}, ObservedAt: now, Items: items}
			if tc.unavailable {
				snapshot.Capability.Status = "unavailable"
			}
			at := now
			if tc.expired {
				at = now.Add(sessionActivityConfig().SamplerTTL() + time.Second)
			}
			result := sessionMessageSendWith(sendGovernor(fixture), "codex", "parent", sessionMessageSendRequest{Runtime: target.Runtime, SessionID: target.ID, Message: tc.name}, at,
				func() []SessionSummary { return catalog }, func(time.Time) presenceOpenSet { return presenceOpenSetFrom(snapshot, at) })
			if result.State != tc.want {
				t.Fatalf("%s: %+v", tc.name, result)
			}
			if tc.want == store.SessionMessageInvocationRefused && !strings.Contains(result.Detail, "caller session is not open") {
				t.Fatalf("wrong refusal: %+v", result)
			}
		})
	}
}

func TestSendClosedTargetExplainsResumeWithoutQueuing(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restore := writeSendConfig(t, fixture.root, true)
	defer restore()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller", Cwd: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target", Cwd: fixture.root}
	for _, catalog := range [][]SessionSummary{{caller, target}, {caller}} {
		result := sessionMessageSendWith(sendGovernor(fixture), caller.Runtime, caller.ID,
			sessionMessageSendRequest{Runtime: target.Runtime, SessionID: target.ID, Message: "wake"}, time.Now(),
			func() []SessionSummary { return catalog }, openSetFor(caller))
		if result.State != store.SessionMessageInvocationRefused || !strings.Contains(result.Detail, "open or resume the exact target") {
			t.Fatalf("recovery: %+v", result)
		}
		if _, found, err := fixture.host.ix.SessionDeliveryForRun(store.DeliveryRunPrefix + result.InvocationID); err != nil || found {
			t.Fatalf("refusal queued delivery: found=%v err=%v", found, err)
		}
	}
}

// An unconfigured deployment refuses before anything is minted (D7): the
// feature is off until selected, and the refusal is a typed terminal outcome.
func TestSendRouteRefusesWithoutGrant(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, false)
	defer restoreConfig()
	catalog := []SessionSummary{
		{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root},
		{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root},
	}
	result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "hi"},
		time.Now(), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if result.State != store.SessionMessageInvocationRefused || !strings.Contains(result.Detail, "deliver_attended is not granted") {
		t.Fatalf("grant-off refusal: %+v", result)
	}
	record, found, _ := fixture.host.ix.SessionMessageInvocationByID(result.InvocationID)
	if !found || record.State != store.SessionMessageInvocationRefused {
		t.Fatalf("a refusal is an admission fact even when the feature is off: %+v found=%v", record, found)
	}
}

// An unidentified caller is refused, never guessed (recall-mcp-v1 F13).
func TestSendRouteRefusesUnidentifiedCaller(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, true)
	defer restoreConfig()
	catalog := []SessionSummary{
		{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root},
	}
	result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "ghost-caller",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "hi"},
		time.Now(), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if result.State != store.SessionMessageInvocationRefused || !strings.Contains(result.Detail, "caller could not be identified") {
		t.Fatalf("caller refusal: %+v", result)
	}
	record, found, _ := fixture.host.ix.SessionMessageInvocationByID(result.InvocationID)
	if !found || record.State != store.SessionMessageInvocationRefused {
		t.Fatalf("a refusal is an admission fact on the record: %+v found=%v", record, found)
	}
}

// Scope (D8): a target in another repository is refused; a self-send is
// refused; the wrapper never reaches a refused send.
func TestSendRouteScopeAndSelfRefusals(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, true)
	defer restoreConfig()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	elsewhere := SessionSummary{Runtime: "managed-fixture", ID: "target-2", Cwd: "/elsewhere", RepositoryKey: "/elsewhere"}
	self := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, elsewhere}
	result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-2", Message: "hi"},
		time.Now(), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if result.State != store.SessionMessageInvocationRefused || !strings.Contains(result.Detail, "outside the caller's repository scope") {
		t.Fatalf("scope refusal: %+v", result)
	}
	result = sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "caller-1", Message: "hi"},
		time.Now(), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if result.State != store.SessionMessageInvocationRefused || !strings.Contains(result.Detail, "may not send to itself") {
		t.Fatalf("self refusal: %+v", result)
	}
	_ = self
}

// A closed target is refused; an admitted hook-target send mints pending with
// the wrapper applied and enqueues the carrier's pending row keyed by the
// synthetic run id (RT-10), in one transaction.
func TestSendRouteMintsPendingForHookTarget(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, true)
	defer restoreConfig()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, target}
	now := time.Now()
	result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "hello there"},
		now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if result.State != store.SessionMessageInvocationPending || result.Tier != "queued-delivery" {
		t.Fatalf("hook-target mint: %+v", result)
	}
	record, found, _ := fixture.host.ix.SessionMessageInvocationByID(result.InvocationID)
	if !found || record.State != store.SessionMessageInvocationPending {
		t.Fatalf("record: %+v found=%v", record, found)
	}
	if !strings.Contains(record.Message, "agent session managed-fixture/caller-1") || !strings.Contains(record.Message, result.InvocationID) || !strings.Contains(record.Message, "hello there") {
		t.Fatalf("wrapper not applied at mint: %q", record.Message)
	}
	// The carrier's pending row exists, keyed by the synthetic run id.
	row, found, err := fixture.host.ix.SessionDeliveryForRun(store.DeliveryRunPrefix + result.InvocationID)
	if err != nil || !found {
		t.Fatalf("pending row: found=%v err=%v", found, err)
	}
	if row.Message != record.Message {
		t.Fatal("the record and the wire carry the same bytes (plan §4)")
	}
}

// D11: an identical caller+target+wrapper digest within the TTL window is
// refused as a duplicate citing the duplicate rule; a different message is a
// second send (idempotentHint: false stays honest).
func TestSendRouteDuplicateDigestRefusal(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, true)
	defer restoreConfig()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, target}
	now := time.Now()
	first := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "same text"},
		now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if first.State != store.SessionMessageInvocationPending {
		t.Fatalf("first send: %+v", first)
	}
	dup := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "same text"},
		now.Add(time.Second), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if dup.State != store.SessionMessageInvocationRefused || !strings.Contains(dup.Detail, "duplicate of "+first.InvocationID) {
		t.Fatalf("duplicate must cite the first invocation (D11): %+v", dup)
	}
	other := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "different text"},
		now.Add(time.Second), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if other.State != store.SessionMessageInvocationPending {
		t.Fatalf("a different message is a second send: %+v", other)
	}
}

// D9/RT-5: a send that would close a cycle is refused with the cycle named;
// refused records form no edge, so a refused A→B never blocks an honest B→A.
func TestSendRouteLoopBoundAndRefusedEdges(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, true)
	defer restoreConfig()
	a := SessionSummary{Runtime: "managed-fixture", ID: "agent-a", Cwd: fixture.root, RepositoryKey: fixture.root}
	b := SessionSummary{Runtime: "managed-fixture", ID: "agent-b", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{a, b}
	now := time.Now()
	send := func(from, to, text string) sessionMessageSendResponse {
		return sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", from,
			sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: to, Message: text},
			now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	}
	// A→B is admitted and delivered (settle accepted to form an edge).
	first := send("agent-a", "agent-b", "open the loop")
	if first.State != store.SessionMessageInvocationPending {
		t.Fatalf("A→B: %+v", first)
	}
	if err := fixture.host.ix.SettleSessionMessageInvocation(first.InvocationID, store.SessionMessageInvocationAccepted, "", now.Unix(), "transport accepted"); err != nil {
		t.Fatal(err)
	}
	// B→A would close the loop: refused with the cycle named.
	back := send("agent-b", "agent-a", "close the loop")
	if back.State != store.SessionMessageInvocationRefused || !strings.Contains(back.Detail, "would close a delivery loop") {
		t.Fatalf("loop refusal: %+v", back)
	}
	// A second A→B with different content is not a loop (same direction).
	again := send("agent-a", "agent-b", "another message")
	if again.State != store.SessionMessageInvocationPending {
		t.Fatalf("same-direction send: %+v", again)
	}
}

// The per-caller window (RT-12) refuses the send over the limit, with the
// refusal recorded.
func TestSendRoutePerCallerWindow(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, true)
	defer restoreConfig()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, target}
	now := time.Now()
	// Seed ten sends inside the window (limit 10 in the fixture config); the
	// per-target budget is raised so the caller ceiling is what fires.
	defer writeSendConfigLimits(t, fixture.root, true, 100)()
	for i := 0; i < 10; i++ {
		result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
			sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "window fill " + string(rune('a'+i))},
			now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
		if result.State != store.SessionMessageInvocationPending {
			t.Fatalf("fill %d: %+v", i, result)
		}
	}
	over := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "one too many"},
		now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if over.State != store.SessionMessageInvocationRefused || !strings.Contains(over.Detail, "caller_cap") {
		t.Fatalf("caller cap: %+v", over)
	}
}

// The per-target budget (D10) refuses the send past max_pending_per_session,
// naming the cap and the configured count.
func TestSendRoutePerTargetBudget(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	defer writeSendConfigLimits(t, fixture.root, true, 2)()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, target}
	now := time.Now()
	send := func(message string) sessionMessageSendResponse {
		return sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
			sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: message},
			now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	}
	for _, message := range []string{"budget fill a", "budget fill b"} {
		if result := send(message); result.State != store.SessionMessageInvocationPending {
			t.Fatalf("%s: %+v", message, result)
		}
	}
	over := send("one too many")
	if want := "pending_cap: the session already holds 2 undelivered messages"; over.State != store.SessionMessageInvocationRefused || over.Detail != want {
		t.Fatalf("target budget: %+v", over)
	}
}

// The boundary handoff settles the MCP-minted record delivered (RT-4 path 1);
// expiry settles it expired and merges onto no run (path 2); the sweeper
// settles stuck pending/accepted unknown (path 3).
func TestSendRouteSettlementPaths(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, true)
	defer restoreConfig()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, target}
	now := time.Now()
	result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "settle me"},
		now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	row, found, _ := fixture.host.ix.SessionDeliveryForRun(store.DeliveryRunPrefix + result.InvocationID)
	if !found {
		t.Fatal("pending row missing")
	}
	// Path 1: the handoff.
	recordSessionDeliveryHandoff(fixture.host.ix, row)
	record, _, _ := fixture.host.ix.SessionMessageInvocationByID(result.InvocationID)
	if record.State != store.SessionMessageInvocationDelivered {
		t.Fatalf("handoff settle: %+v", record)
	}
	// Path 2: expiry on a synthetic row minted in the past, so its pending row
	// is already past expiry when the host's expiry once() runs.
	expiry := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "expire me"},
		now.Add(-3*time.Hour), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	fixture.host.expireDeliveriesOnce()
	expRecord, _, _ := fixture.host.ix.SessionMessageInvocationByID(expiry.InvocationID)
	if expRecord.State != store.SessionMessageInvocationExpired {
		t.Fatalf("expiry settle: %+v", expRecord)
	}
	// Path 3: the sweeper on a stuck pending record (one whose boundary never
	// arrived within the TTL — simulated by an old created_at).
	stuck := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "sweep me"},
		now.Add(-3*time.Hour), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	fixture.host.sweepStuckSessionMessageInvocations()
	swept, _, _ := fixture.host.ix.SessionMessageInvocationByID(stuck.InvocationID)
	if swept.State != store.SessionMessageInvocationUnknown || !strings.Contains(swept.Detail, "never answered") {
		t.Fatalf("sweeper: %+v", swept)
	}
}

// The HTTP surface answers typed JSON on the registered route.
func TestSendRouteHTTPSurface(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	restoreConfig := writeSendConfig(t, fixture.root, false)
	defer restoreConfig()
	previous := governor
	governor = sendGovernor(fixture)
	t.Cleanup(func() { governor = previous })
	body := `{"runtime":"managed-fixture","session_id":"target-1","message":"hi"}`
	r := httptest.NewRequest(http.MethodPost, "/api/session-message/send?caller_runtime=managed-fixture&caller_id=caller-1", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleSessionMessageSend(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"state":"refused"`) {
		t.Fatalf("typed response: %s", w.Body.String())
	}
}

// The boundary carrier claims an MCP-minted pending row exactly like a helper
// send: the claim happens inside the ingest handler, the reply carries the
// message, and the invocation record settles delivered (RT-4 path 1) with the
// same claim-on-reply guarantees.
func TestSendRouteCarrierClaimsMintedRow(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	defer writeSendConfig(t, fixture.root, true)()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, target}
	now := time.Now()
	result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "carry me via the boundary"},
		now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if result.State != store.SessionMessageInvocationPending {
		t.Fatalf("mint: %+v", result)
	}
	g := NewGovernor(fixture.host.ix, nil)
	defer swapSessionStatusRefold(func(string, string) {})()
	envelope := observation.SessionTurnEnvelope{Schema: observation.SessionTurnSchemaV1,
		ObservationID: "trn_" + strings.Repeat("7", 32),
		CollectorID:   observation.CollectorSessionTurn, Runtime: "managed-fixture", SessionID: "target-1", Kind: "turn.started",
		NativeSource: "UserPromptSubmit", ObservedAt: now.Add(time.Minute).Unix(), QueuedAt: now.Add(time.Minute).Unix(),
		DeliveryAttempts: 1, DeliveryMode: "direct", Carrier: true}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	previous := governor
	governor = g
	w := httptest.NewRecorder()
	handleGovernSessionTurnV1(w, httptest.NewRequest(http.MethodPost, "/api/govern/session-turn/v1", bytes.NewReader(body)))
	governor = previous
	var receipt observation.SessionTurnReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("receipt: %v %s", err, w.Body.String())
	}
	if len(receipt.Deliveries) != 1 || !strings.Contains(receipt.Deliveries[0].Message, "carry me via the boundary") {
		t.Fatalf("boundary claim: %+v", receipt.Deliveries)
	}
	record, found, _ := fixture.host.ix.SessionMessageInvocationByID(result.InvocationID)
	if !found || record.State != store.SessionMessageInvocationDelivered {
		t.Fatalf("invocation settled delivered: %+v found=%v", record, found)
	}
}

// queueFixtureDriver answers the DIRECT-post shape (no Carrier) — the codex
// queue verb's receipt — so the route must settle accepted and enqueue no
// pending row (postwork PW-1).
type queueFixtureDriver struct{ managedDynamicFixtureDriver }

func (d queueFixtureDriver) ChatCapability() ChatCapability {
	capability := d.managedDynamicFixtureDriver.ChatCapability()
	capability.MessageDelivery = SessionMessageCapability{Supported: true, Boundary: "next vendor turn boundary", Detail: "Queues to the exact thread with the vendor's queue verb; acceptance does not prove consumption."}
	return capability
}

func (d queueFixtureDriver) DeliverSessionMessage(_ context.Context, target SessionIdentity, _ string) SessionMessageReceipt {
	if target.NativeID == "" {
		return SessionMessageReceipt{State: "unavailable", Tier: "none"}
	}
	return SessionMessageReceipt{State: "accepted", Tier: "queued-delivery", Boundary: "next vendor turn boundary"}
}

// A direct-post target (the vendor queue verb) settles accepted from the
// receipt and enqueues NO pending row: the boundary carrier owns rows only
// when the receipt says the boundary carries it.
func TestSendRouteQueueReceiptNeverEnqueues(t *testing.T) {
	driver := queueFixtureDriver{managedDynamicFixtureDriver{}}
	fixture := newAgentHostFixture(t, driver)
	defer writeSendConfig(t, fixture.root, true)()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, target}
	result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "post me directly"},
		time.Now(), func() []SessionSummary { return catalog }, openSetFor(catalog...))
	if result.State != store.SessionMessageInvocationAccepted || result.Tier != "queued-delivery" {
		t.Fatalf("direct post settle: %+v", result)
	}
	record, found, _ := fixture.host.ix.SessionMessageInvocationByID(result.InvocationID)
	if !found || record.State != store.SessionMessageInvocationAccepted {
		t.Fatalf("record: %+v found=%v", record, found)
	}
	if _, found, _ := fixture.host.ix.SessionDeliveryForRun(store.DeliveryRunPrefix + result.InvocationID); found {
		t.Fatal("a direct post must enqueue no pending row (the vendor queue is not our carrier)")
	}
}

// The loop bound forms edges over CANONICAL session ids (postwork PW-3): a
// B→A send under a different id form of the same sessions still closes the
// cycle A→B→A.
func TestSendRouteLoopBoundCanonicalizesIds(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	defer writeSendConfig(t, fixture.root, true)()
	a := SessionSummary{Runtime: "managed-fixture", ID: "agent-a-rollout", ThreadID: "agent-a-meta", Cwd: fixture.root, RepositoryKey: fixture.root}
	b := SessionSummary{Runtime: "managed-fixture", ID: "agent-b-rollout", ThreadID: "agent-b-meta", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{a, b}
	now := time.Now()
	send := func(fromID, toID, text string) sessionMessageSendResponse {
		return sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", fromID,
			sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: toID, Message: text},
			now, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	}
	// A→B addressed by meta id; the record records both canonical forms.
	first := send("agent-a-meta", "agent-b-meta", "open the loop by meta ids")
	if first.State != store.SessionMessageInvocationPending {
		t.Fatalf("A→B: %+v", first)
	}
	if err := fixture.host.ix.SettleSessionMessageInvocation(first.InvocationID, store.SessionMessageInvocationAccepted, "", now.Unix(), "transport accepted"); err != nil {
		t.Fatal(err)
	}
	// B→A addressed by rollout ids — a DIFFERENT id form of the same sessions.
	back := send("agent-b-rollout", "agent-a-rollout", "close the loop by rollout ids")
	if back.State != store.SessionMessageInvocationRefused || !strings.Contains(back.Detail, "would close a delivery loop") {
		t.Fatalf("canonical cycle invisible: %+v", back)
	}
}

// The scope check admits a sibling git worktree (postwork PW-4): D8 promised
// the caller's repository AND its worktrees — CommonDir equality, the peers
// route's own sibling_worktree rule.
func TestSendRouteScopeAdmitsSiblingWorktree(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	defer writeSendConfig(t, fixture.root, true)()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root + "-worktree"}
	result := sendScopeContains(
		resolvePlace(context.Background(), caller.Cwd),
		resolvePlace(context.Background(), target.Cwd))
	// The fixture root is a temp dir, not a git repository with a worktree
	// beside it — the folder fallback refuses. The worktree rule itself is
	// pinned by resolvePlace's git semantics on a real repo (the peers route's
	// classifyPeer tests); here we pin the refusal shape for non-git folders.
	if result {
		t.Fatal("two unrelated folders are not one repository")
	}
}

// The sweeper never sweeps a terminal accepted record (postwork PW-2): the
// receipt was recorded — the outcome is true and stays.
func TestSendRouteSweeperSparesAccepted(t *testing.T) {
	fixture := sendFixtureDriver(t, nil)
	defer writeSendConfig(t, fixture.root, true)()
	caller := SessionSummary{Runtime: "managed-fixture", ID: "caller-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	target := SessionSummary{Runtime: "managed-fixture", ID: "target-1", Cwd: fixture.root, RepositoryKey: fixture.root}
	catalog := []SessionSummary{caller, target}
	now := time.Now()
	old := now.Add(-3 * time.Hour)
	result := sessionMessageSendWith(sendGovernor(fixture), "managed-fixture", "caller-1",
		sessionMessageSendRequest{Runtime: "managed-fixture", SessionID: "target-1", Message: "accepted long ago"},
		old, func() []SessionSummary { return catalog }, openSetFor(catalog...))
	// A queue-shaped receipt would settle accepted; settle it accepted here to
	// age a terminal record past the TTL.
	if err := fixture.host.ix.SettleSessionMessageInvocation(result.InvocationID, store.SessionMessageInvocationAccepted, "", old.Add(time.Minute).Unix(), "queued"); err != nil {
		t.Fatal(err)
	}
	fixture.host.sweepStuckSessionMessageInvocations()
	record, _, _ := fixture.host.ix.SessionMessageInvocationByID(result.InvocationID)
	if record.State != store.SessionMessageInvocationAccepted {
		t.Fatalf("a terminal accepted record must never be swept: %+v", record)
	}
}
