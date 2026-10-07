package daemon

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func responseTestID(letter string) string {
	return "apr_" + strings.Repeat(letter, 26)
}

func admittedResponseTestApproval(t *testing.T, hub *approvalsHub, id, mode string, deadline time.Time) (*Approval, chan *Approval, approvalResponderCapability) {
	t.Helper()
	approval := &Approval{
		ID:        id,
		CreatedAt: deadline.Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		Deadline:  deadline.UTC().Format(time.RFC3339Nano),
		Origin:    ApprovalOriginPolicyHook,
		Rule:      "response-test",
		Mode:      mode,
		Status:    "pending",
	}
	waiter := make(chan *Approval, 1)
	if err := hub.admit(approval, waiter); err != nil {
		t.Fatal(err)
	}
	capability, outcome := hub.interactiveCapability(id)
	if outcome != "" {
		t.Fatalf("interactive capability lookup = %q", outcome)
	}
	return approval, waiter, capability
}

func responseTestCommand(id, responseID, decision, reason string, submitted time.Time, capability approvalResponderCapability) approvalResponseCommand {
	return approvalResponseCommand{
		approvalID: id,
		responseID: responseID,
		responder:  interactiveConsoleResponder,
		decision:   decision,
		reason:     reason,
		submitted:  submitted,
		capability: capability,
	}
}

func TestApprovalResponseRecordsOperativeInteractiveProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 8, 28, 12, 0, 0, 123456789, time.UTC)
	hub := newApprovalsHub()
	hub.now = func() time.Time { return now }
	approval, waiter, capability := admittedResponseTestApproval(t, hub, "ap_operative", "confirm-and-record", now.Add(time.Minute))

	outcome := hub.respond(responseTestCommand(approval.ID, responseTestID("A"), "allow", "  reviewed  ", now, capability))
	if outcome.kind != approvalResponseAccepted || outcome.approval.Status != "allowed" {
		t.Fatalf("operative outcome = %q %+v", outcome.kind, outcome.approval)
	}
	if len(outcome.approval.Responses) != 1 {
		t.Fatalf("responses = %+v", outcome.approval.Responses)
	}
	response := outcome.approval.Responses[0]
	if response.ID != responseTestID("A") || response.Responder != interactiveConsoleResponder ||
		response.Decision != "allow" || response.Reason != "reviewed" || response.Disposition != "operative" ||
		response.SubmittedAt != now.Format(time.RFC3339Nano) || response.AcceptedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("response provenance = %+v", response)
	}
	if decided := <-waiter; decided.Status != "allowed" || decided.Reason != "reviewed" {
		t.Fatalf("waiter result = %+v", decided)
	}
	if _, exists := hub.responderGrants[approval.ID]; exists {
		t.Fatal("accepted response retained its responder grant")
	}
	encoded, err := json.Marshal(outcome.approval)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"capability", "nonce"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("approval JSON exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestApprovalResponseAtDeadlineIsLateAndWaiterFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deadline := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	hub := newApprovalsHub()
	hub.now = func() time.Time { return deadline }
	approval, waiter, capability := admittedResponseTestApproval(t, hub, "ap_deadline", "ask", deadline)

	outcome := hub.respond(responseTestCommand(approval.ID, responseTestID("B"), "allow", "too late", deadline, capability))
	if outcome.kind != approvalResponseLateAdvisory || outcome.approval.Status != "expired" || !outcome.approval.Late {
		t.Fatalf("deadline outcome = %q %+v", outcome.kind, outcome.approval)
	}
	if len(outcome.approval.Responses) != 1 || outcome.approval.Responses[0].Disposition != "late-advisory" {
		t.Fatalf("late response = %+v", outcome.approval.Responses)
	}
	closed := <-waiter
	if closed.Status != "expired" || closed.Late || len(closed.Responses) != 0 {
		t.Fatalf("waiter was not immutable fail-closed snapshot: %+v", closed)
	}
}

func TestApprovalResponseAfterExpiryStaysAdvisory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	hub := newApprovalsHub()
	hub.now = func() time.Time { return now }
	approval, _, capability := admittedResponseTestApproval(t, hub, "ap_expired_first", "ask", now.Add(time.Minute))
	hub.finishExpired(approval)
	if len(approval.Responses) != 0 {
		t.Fatalf("expiry credited a responder: %+v", approval.Responses)
	}

	outcome := hub.respond(responseTestCommand(approval.ID, responseTestID("C"), "deny", "audit answer", now.Add(-time.Second), capability))
	if outcome.kind != approvalResponseLateAdvisory || outcome.approval.Status != "expired" ||
		outcome.approval.Responses[0].Disposition != "late-advisory" {
		t.Fatalf("expiry-lock winner was changed: %q %+v", outcome.kind, outcome.approval)
	}
}

func TestApprovalResponseCapabilitiesRejectSpoofCrossUseReplayAndDuplicateID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	hub := newApprovalsHub()
	hub.now = func() time.Time { return now }
	first, _, firstCapability := admittedResponseTestApproval(t, hub, "ap_first", "ask", now.Add(time.Minute))
	second, _, secondCapability := admittedResponseTestApproval(t, hub, "ap_second", "ask", now.Add(time.Minute))

	wrongNonce := firstCapability
	wrongNonce.nonce[0] ^= 0xff
	if got := hub.respond(responseTestCommand(first.ID, responseTestID("D"), "deny", "", now, wrongNonce)); got.kind != approvalResponseUnauthorized {
		t.Fatalf("wrong nonce outcome = %q", got.kind)
	}
	spoof := responseTestCommand(first.ID, responseTestID("E"), "deny", "", now, firstCapability)
	spoof.responder = ApprovalResponder{Kind: "service", ID: "spoof"}
	if got := hub.respond(spoof); got.kind != approvalResponseUnauthorized {
		t.Fatalf("spoofed principal outcome = %q", got.kind)
	}
	if got := hub.respond(responseTestCommand(second.ID, responseTestID("F"), "deny", "", now, firstCapability)); got.kind != approvalResponseUnauthorized {
		t.Fatalf("cross-approval capability outcome = %q", got.kind)
	}

	acceptedID := responseTestID("G")
	if got := hub.respond(responseTestCommand(first.ID, acceptedID, "deny", "", now, firstCapability)); got.kind != approvalResponseAccepted {
		t.Fatalf("legitimate response outcome = %q", got.kind)
	}
	if got := hub.respond(responseTestCommand(first.ID, responseTestID("H"), "deny", "", now, firstCapability)); got.kind != approvalResponseAlreadyResolved {
		t.Fatalf("replayed capability outcome = %q", got.kind)
	}
	if got := hub.respond(responseTestCommand(second.ID, acceptedID, "deny", "", now, secondCapability)); got.kind != approvalResponseAlreadyResolved {
		t.Fatalf("duplicate response ID outcome = %q", got.kind)
	}
	if second.Status != "pending" {
		t.Fatalf("refused duplicate mutated approval: %+v", second)
	}
	if got := hub.respond(responseTestCommand(second.ID, responseTestID("I"), "deny", "", now, secondCapability)); got.kind != approvalResponseAccepted {
		t.Fatalf("capability was consumed by refusal: %q", got.kind)
	}
}

func TestApprovalResponseRefusalsDoNotConsumeInteractiveGrant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	hub := newApprovalsHub()
	hub.now = func() time.Time { return now }
	hardBlock, _, hardBlockCapability := admittedResponseTestApproval(t, hub, "ap_hard", "hard-block", now.Add(time.Minute))
	if got := hub.respond(responseTestCommand(hardBlock.ID, responseTestID("J"), "allow", "please", now, hardBlockCapability)); got.kind != approvalResponseHardBlock {
		t.Fatalf("hard-block allow outcome = %q", got.kind)
	}
	if got := hub.respond(responseTestCommand(hardBlock.ID, responseTestID("K"), "deny", "", now, hardBlockCapability)); got.kind != approvalResponseAccepted {
		t.Fatalf("hard-block corrective deny outcome = %q", got.kind)
	}

	confirmed, _, confirmedCapability := admittedResponseTestApproval(t, hub, "ap_confirm", "confirm-and-record", now.Add(time.Minute))
	if got := hub.respond(responseTestCommand(confirmed.ID, responseTestID("L"), "allow", "", now, confirmedCapability)); got.kind != approvalResponseReasonRequired {
		t.Fatalf("reasonless allow outcome = %q", got.kind)
	}
	if got := hub.respond(responseTestCommand(confirmed.ID, responseTestID("M"), "allow", "reviewed", now, confirmedCapability)); got.kind != approvalResponseAccepted {
		t.Fatalf("corrected allow outcome = %q", got.kind)
	}
}

func TestApprovalResponseConcurrentAnswersHaveOneWinner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	hub := newApprovalsHub()
	hub.now = func() time.Time { return now }
	approval, waiter, capability := admittedResponseTestApproval(t, hub, "ap_race", "ask", now.Add(time.Minute))

	commands := []approvalResponseCommand{
		responseTestCommand(approval.ID, responseTestID("N"), "allow", "", now, capability),
		responseTestCommand(approval.ID, responseTestID("O"), "deny", "", now, capability),
	}
	start := make(chan struct{})
	outcomes := make(chan approvalResponseOutcomeKind, len(commands))
	var group sync.WaitGroup
	for _, command := range commands {
		group.Add(1)
		go func(command approvalResponseCommand) {
			defer group.Done()
			<-start
			outcomes <- hub.respond(command).kind
		}(command)
	}
	close(start)
	group.Wait()
	close(outcomes)
	counts := map[approvalResponseOutcomeKind]int{}
	for outcome := range outcomes {
		counts[outcome]++
	}
	if counts[approvalResponseAccepted] != 1 || counts[approvalResponseAlreadyResolved] != 1 {
		t.Fatalf("concurrent outcomes = %#v", counts)
	}
	<-waiter
	select {
	case duplicate := <-waiter:
		t.Fatalf("waiter received duplicate result: %+v", duplicate)
	default:
	}
}

func TestApprovalServiceGrantIsExactAndHumanTakeoverRevokesIt(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	hub := newApprovalsHub()
	hub.now = func() time.Time { return now }
	approval, _, _ := admittedResponseTestApproval(t, hub, "ap_delegated", "ask", now.Add(time.Minute))
	approval.GrantOptions = []ApprovalGrantOption{requestGrantOption, exactRunGrantOption}
	responder := ApprovalResponder{Kind: "service", ID: "orchestration:reviewer"}
	serviceCapability, err := hub.grantServiceResponder(approval.ID, responder)
	if err != nil {
		t.Fatal(err)
	}
	widen := responseTestCommand(approval.ID, responseTestID("O"), "allow", "model claim", now, serviceCapability)
	widen.responder = responder
	widen.grantID = approvalGrantRunExact
	if got := hub.respond(widen); got.kind != approvalResponseUnauthorized {
		t.Fatalf("service widened grant outcome = %q", got.kind)
	}
	interactive, lookup := hub.takeoverInteractiveCapability(approval.ID)
	if lookup != "" {
		t.Fatalf("takeover lookup = %q", lookup)
	}
	serviceCommand := responseTestCommand(approval.ID, responseTestID("P"), "deny", "model claim", now, serviceCapability)
	serviceCommand.responder = responder
	if got := hub.respond(serviceCommand); got.kind != approvalResponseUnauthorized {
		t.Fatalf("revoked service outcome = %q", got.kind)
	}
	if got := hub.respond(responseTestCommand(approval.ID, responseTestID("Q"), "allow", "human answer", now, interactive)); got.kind != approvalResponseAccepted {
		t.Fatalf("human takeover outcome = %q", got.kind)
	}
}

func TestApprovalPendingObserverGetsPrivateExactIssuerAndCanUnsubscribe(t *testing.T) {
	hub := newApprovalsHub()
	notified := 0
	var capability approvalResponderCapability
	unsubscribe := hub.subscribePending(func(approval Approval, issue func(ApprovalResponder) (approvalResponderCapability, error)) {
		notified++
		var err error
		capability, err = issue(ApprovalResponder{Kind: "service", ID: "service:test"})
		if err != nil {
			t.Errorf("issue grant: %v", err)
		}
	})
	approval, _, _ := admittedResponseTestApproval(t, hub, "ap_observer", "ask", time.Now().Add(time.Minute))
	hub.notifyPending(cloneApproval(approval))
	if notified != 1 || capability.approvalID != approval.ID || capability.responder.Kind != "service" {
		t.Fatalf("notification=%d capability=%+v", notified, capability)
	}
	unsubscribe()
	hub.notifyPending(cloneApproval(approval))
	if notified != 1 {
		t.Fatalf("observer called after unsubscribe: %d", notified)
	}
}

func TestApprovalResponseBoundsAndHistoryEviction(t *testing.T) {
	hub := newApprovalsHub()
	approval := &Approval{ID: "ap_bounds"}
	for index := 0; index < maxApprovalResponses+2; index++ {
		appendApprovalResponse(approval, approvalResponseCommand{
			responseID: responseTestID(string(rune('A' + index))), responder: interactiveConsoleResponder,
			decision: "deny", submitted: time.Unix(int64(index), 0),
		}, time.Unix(int64(index), 0), "operative")
	}
	if len(approval.Responses) != maxApprovalResponses || approval.Responses[0].ID != responseTestID("C") {
		t.Fatalf("response bound = %+v", approval.Responses)
	}

	hub.mu.Lock()
	for index := 0; index < maxResponderGrants; index++ {
		responder := ApprovalResponder{Kind: "service", ID: "service-" + string(rune('a'+index))}
		if err := hub.addResponderGrantLocked(approvalResponderCapability{approvalID: approval.ID, responder: responder}); err != nil {
			t.Fatal(err)
		}
	}
	if err := hub.addResponderGrantLocked(approvalResponderCapability{
		approvalID: approval.ID, responder: ApprovalResponder{Kind: "service", ID: "service-overflow"},
	}); err == nil {
		t.Fatal("fifth responder grant was accepted")
	}

	for index := 0; index <= activeApprovalsConfig().HistoryCap; index++ {
		id := "ap_history_" + strconv.Itoa(index)
		responseID := "apr_history_" + strconv.Itoa(index)
		historyApproval := &Approval{ID: id, Responses: []ApprovalResponse{{ID: responseID}}}
		hub.responderGrants[id] = map[string]approvalResponderCapability{"test": {approvalID: id}}
		hub.responseIDs[responseID] = id
		hub.addHistoryLocked(historyApproval)
	}
	hub.mu.Unlock()
	if len(hub.history) != activeApprovalsConfig().HistoryCap {
		t.Fatalf("history length = %d", len(hub.history))
	}
	if _, exists := hub.responderGrants["ap_history_0"]; exists {
		t.Fatal("evicted approval retained responder grants")
	}
	if _, exists := hub.responseIDs["apr_history_0"]; exists {
		t.Fatal("evicted approval retained response ID index")
	}
}

func TestApprovalResponseValidationBounds(t *testing.T) {
	if !validResponder(interactiveConsoleResponder) ||
		!validResponder(ApprovalResponder{Kind: "service", ID: strings.Repeat("a", maxResponderIDBytes)}) {
		t.Fatal("valid responder was refused")
	}
	for _, responder := range []ApprovalResponder{
		{Kind: "human", ID: "local-console"},
		{Kind: "interactive", ID: ""},
		{Kind: "interactive", ID: "Uppercase"},
		{Kind: "interactive", ID: strings.Repeat("a", maxResponderIDBytes+1)},
	} {
		if validResponder(responder) {
			t.Fatalf("invalid responder accepted: %+v", responder)
		}
	}
	if !validApprovalResponseID(responseTestID("A")) {
		t.Fatal("valid response ID was refused")
	}
	for _, id := range []string{
		"response_" + strings.Repeat("A", 26),
		"apr_short",
		"apr_" + strings.Repeat("0", 26),
		"apr_" + strings.Repeat("A", maxResponseIDBytes),
	} {
		if validApprovalResponseID(id) {
			t.Fatalf("invalid response ID accepted: %q", id)
		}
	}

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	valid := approvalResponseCommand{
		approvalID: "ap_validation", responseID: responseTestID("A"),
		responder: interactiveConsoleResponder, decision: "deny", submitted: now,
	}
	for name, mutate := range map[string]func(*approvalResponseCommand){
		"approval":  func(command *approvalResponseCommand) { command.approvalID = "" },
		"response":  func(command *approvalResponseCommand) { command.responseID = "bad" },
		"principal": func(command *approvalResponseCommand) { command.responder.Kind = "human" },
		"decision":  func(command *approvalResponseCommand) { command.decision = "abstain" },
		"reason":    func(command *approvalResponseCommand) { command.reason = strings.Repeat("x", 4097) },
		"time":      func(command *approvalResponseCommand) { command.submitted = time.Time{} },
	} {
		command := valid
		mutate(&command)
		if got := validateApprovalResponseCommand(command); got != approvalResponseInvalid {
			t.Fatalf("%s bound outcome = %q", name, got)
		}
	}
}

func TestApprovalAdmissionFailsClosedWhenCapabilityMintingFails(t *testing.T) {
	hub := newApprovalsHub()
	hub.newCapabilityNonce = func() ([32]byte, error) {
		return [32]byte{}, errors.New("entropy unavailable")
	}
	approval := &Approval{ID: "ap_entropy", Status: "pending"}
	if err := hub.admit(approval, make(chan *Approval, 1)); err == nil {
		t.Fatal("approval admitted without a responder capability")
	}
	if len(hub.pending) != 0 || len(hub.waiters) != 0 || len(hub.responderGrants) != 0 {
		t.Fatalf("failed admission mutated hub: pending=%d waiters=%d grants=%d",
			len(hub.pending), len(hub.waiters), len(hub.responderGrants))
	}
}
