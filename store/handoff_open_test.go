package store

import (
	"errors"
	"strings"
	"testing"

	"crossing-guard/teamwire"
)

var briefPolicy = HandoffBriefPolicy{TTLSeconds: 1800, MaxPending: 3}

// receivedHandoff lands one handoff addressed to this device's user.
func receivedHandoff(t *testing.T, ix *Index, seq int64) (teamwire.HandoffRecord, []byte) {
	t.Helper()
	rec, body := handoffRecord("Finish the drain", "the body", "usr_me")
	landPage(t, ix, seq-1, seq, pulled(seq, rec, body, teamwire.HandoffReceived))
	return rec, body
}

func openTicket(t *testing.T, ix *Index, org, handoffID, runtime string) HandoffOpen {
	t.Helper()
	ticket, err := ix.CreateHandoffOpen(HandoffOpenCreate{OrganizationID: org, HandoffID: handoffID, Runtime: runtime, CheckoutRoot: "/work/repo", At: 300})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func claimTicket(t *testing.T, ix *Index, ticketID, runtime, nativeID string, at int64) HandoffClaimResult {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	result, err := ix.ClaimHandoffTicketTx(tx, HandoffClaim{TicketID: ticketID, Runtime: runtime, NativeID: nativeID,
		Transcript: "/t/" + nativeID + ".jsonl", BriefText: "the brief", At: at, Policy: briefPolicy})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return result
}

func mustTicket(t *testing.T, ix *Index, ticketID string) HandoffOpen {
	t.Helper()
	ticket, found, err := ix.HandoffOpenByTicket(ticketID)
	if err != nil || !found {
		t.Fatalf("ticket %s: found=%v err=%v", ticketID, found, err)
	}
	return ticket
}

// handOver claims what a prompt of the session may carry, as the carrier does.
func handOver(t *testing.T, ix *Index, runtime, nativeID, observation string, now int64) []SessionDelivery {
	t.Helper()
	claimed, err := ix.ClaimSessionDeliveriesAt(SessionDeliveryClaim{Runtime: runtime, SessionID: nativeID, Kind: "turn.started",
		ObservationID: observation, Now: now, MaxBytes: 7000, CarriesHandoff: true})
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func briefRows(t *testing.T, ix *Index, ticketID string) []SessionDelivery {
	t.Helper()
	rows, err := ix.db.Query(`SELECT `+sessionDeliveryCols+` FROM session_delivery WHERE run_id=? ORDER BY created_at, delivery_id`, HandoffDeliveryRun(ticketID))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []SessionDelivery
	for rows.Next() {
		d, err := scanSessionDelivery(rows)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func receiptsOf(t *testing.T, ix *Index, transition string) int {
	t.Helper()
	n := 0
	for _, row := range pendingOfKind(t, ix, OutboxHandoffReceipt) {
		if strings.Contains(row.SentWireBody, `"transition":"`+transition+`"`) {
			n++
		}
	}
	return n
}

func refusalCode(err error) string {
	var refusal *HandoffRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// §6.3, §6.7 "open": a ticket has no timer; it is cancelled when its composer closes
// without sending, a send with it is admitted once, for its runtime only.
func TestHandoffOpenTicketEndsByCancelOrLaunch(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	closed := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	if closed.State != HandoffOpenWaiting || !strings.HasPrefix(closed.TicketID, teamwire.TicketIDPrefix+"_") || closed.CheckoutRoot != "/work/repo" {
		t.Fatalf("an open writes a waiting ticket: %+v", closed)
	}
	if cancelled, err := ix.CancelHandoffOpen(closed.TicketID, 310); err != nil || !cancelled {
		t.Fatalf("a closed composer cancels its ticket: %v %v", cancelled, err)
	}
	if got := mustTicket(t, ix, closed.TicketID); got.State != HandoffOpenCancelled || got.CancelReason != HandoffOpenComposerClosed {
		t.Fatalf("cancelled by the composer: %+v", got)
	}
	if _, err := ix.LaunchHandoffOpen(closed.TicketID, "codex", "task_1"); refusalCode(err) != HandoffCodeTicketCancelled {
		t.Fatalf("a cancelled ticket launches nothing: %v", err)
	}

	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	if _, err := ix.LaunchHandoffOpen(ticket.TicketID, "claude", "task_2"); refusalCode(err) != HandoffCodeTicketRuntime {
		t.Fatalf("a ticket launches its own runtime only: %v", err)
	}
	if refusal, err := ix.HandoffOpenLaunchRefusal(ticket.TicketID, "codex"); err != nil || refusal != nil {
		t.Fatalf("a waiting ticket is admitted: %v %v", refusal, err)
	}
	if _, err := ix.LaunchHandoffOpen(ticket.TicketID, "codex", "task_3"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.LaunchHandoffOpen(ticket.TicketID, "codex", "task_4"); refusalCode(err) != HandoffCodeTicketUsed {
		t.Fatalf("one ticket, one launch: %v", err)
	}
	if cancelled, _ := ix.CancelHandoffOpen(ticket.TicketID, 320); cancelled {
		t.Fatal("a launched ticket is not cancelled by a closing composer")
	}
	if byTask, found, _ := ix.HandoffOpenByTask("task_3"); !found || byTask.TicketID != ticket.TicketID {
		t.Fatalf("the ticket names its launched task: %+v", byTask)
	}
	if _, err := ix.LaunchHandoffOpen("tkt_missing", "codex", "task_5"); refusalCode(err) != HandoffCodeTicketNotFound {
		t.Fatalf("an unknown ticket: %v", err)
	}
}

// Open is offered on any non-terminal item of the recipient's, never on the
// originating device of a team send, never on a row with no text.
func TestHandoffOpenIsOfferedOnlyToTheRecipientsHeldCopy(t *testing.T) {
	ix := handoffDevice(t)
	sent, _ := sendTestHandoff(t, ix, handoffTestOrg, "usr_teammate", false)
	if _, err := ix.CreateHandoffOpen(HandoffOpenCreate{OrganizationID: handoffTestOrg, HandoffID: sent.ID, Runtime: "claude", At: 1}); refusalCode(err) != HandoffCodeNotOffered {
		t.Fatalf("the originating device has no Open: %v", err)
	}
	never, _ := handoffRecord("never", "never", "usr_me")
	row := pulled(1, never, nil, teamwire.HandoffExpired)
	row.Handoff.WireHash = ""
	landPage(t, ix, 0, 1, row)
	if _, err := ix.CreateHandoffOpen(HandoffOpenCreate{OrganizationID: handoffTestOrg, HandoffID: never.ID, Runtime: "claude", At: 1}); refusalCode(err) != teamwire.CodeHandoffExpired {
		t.Fatalf("an expired handoff has no Open: %v", err)
	}
	if _, err := ix.CreateHandoffOpen(HandoffOpenCreate{OrganizationID: handoffTestOrg, HandoffID: "hnd_missing", Runtime: "claude", At: 1}); !errors.Is(err, ErrHandoffNotFound) {
		t.Fatalf("an unknown handoff: %v", err)
	}
}

// §6.4 rules 1, 2, 3, 6 and criterion 64: only the entry that carries the ticket
// claims; a duplicate changes nothing; a claimed ticket is never claimed again; once
// the launched task's session is known any other session is refused; and the claim is
// one transaction — the ticket, the handoff's state, the armed brief and the started
// receipt.
func TestClaimIsExactIdempotentAndTheLaunchedSessions(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	if _, err := ix.LaunchHandoffOpen(ticket.TicketID, "claude", "task_1"); err != nil {
		t.Fatal(err)
	}
	if got := claimTicket(t, ix, "tkt_unknown", "claude", "ses-a", 400); got.Outcome != HandoffClaimUnknownTicket {
		t.Fatalf("an unknown ticket claims nothing: %+v", got)
	}
	if got := claimTicket(t, ix, ticket.TicketID, "codex", "ses-a", 400); got.Outcome != HandoffClaimWrongRuntime {
		t.Fatalf("another runtime's session claims nothing: %+v", got)
	}
	got := claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 400)
	if got.Outcome != HandoffClaimClaimed || !got.BriefArmed || !got.Receipt || got.HandoffID != rec.ID {
		t.Fatalf("the claim: %+v", got)
	}
	claimed := mustTicket(t, ix, ticket.TicketID)
	if claimed.State != HandoffOpenClaimed || claimed.ClaimedRuntime != "claude" || claimed.ClaimedNativeID != "ses-a" ||
		claimed.ClaimedTranscript != "/t/ses-a.jsonl" || claimed.ClaimedAt != 400 || !claimed.BriefDue {
		t.Fatalf("the ticket holds the claim: %+v", claimed)
	}
	if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffStarted {
		t.Fatalf("claim is started: %+v", h.State)
	}
	rows := briefRows(t, ix, ticket.TicketID)
	if len(rows) != 1 || rows[0].State != "pending" || rows[0].NativeSessionID != "ses-a" || rows[0].CatalogSessionID != "" ||
		rows[0].Message != "the brief" || rows[0].ExpiresAt != 400+briefPolicy.TTLSeconds {
		t.Fatalf("one pending brief row, matched by native id only: %+v", rows)
	}
	if receiptsOf(t, ix, teamwire.TransitionStarted) != 1 {
		t.Fatal("one started receipt")
	}
	started := pendingOfKind(t, ix, OutboxHandoffReceipt)
	if body := started[len(started)-1].SentWireBody; !strings.Contains(body, ticket.TicketID) || !strings.Contains(body, `"native_id":"ses-a"`) {
		t.Fatalf("the started receipt carries the ticket and the session: %s", body)
	}

	if dup := claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 500); dup.Outcome != HandoffClaimDuplicate {
		t.Fatalf("a duplicate or replayed entry changes nothing: %+v", dup)
	}
	if other := claimTicket(t, ix, ticket.TicketID, "claude", "ses-spawned", 500); other.Outcome != HandoffClaimAlreadyClaimed {
		t.Fatalf("a claimed ticket is never claimed again: %+v", other)
	}
	if after := mustTicket(t, ix, ticket.TicketID); after.ClaimedNativeID != "ses-a" || after.ClaimedAt != 400 {
		t.Fatalf("the claim is unchanged: %+v", after)
	}
	if len(briefRows(t, ix, ticket.TicketID)) != 1 || receiptsOf(t, ix, teamwire.TransitionStarted) != 1 {
		t.Fatal("no second brief and no second receipt")
	}

	// A second ticket whose launched session is already known refuses every other one.
	known := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	if _, err := ix.LaunchHandoffOpen(known.TicketID, "claude", "task_2"); err != nil {
		t.Fatal(err)
	}
	if frame, err := ix.RecordHandoffOpenFrame(known.TicketID, "ses-launched", 600, briefPolicy); err != nil || !frame.Recorded || frame.Moved {
		t.Fatalf("the first frame is recorded: %+v %v", frame, err)
	}
	if spawned := claimTicket(t, ix, known.TicketID, "claude", "ses-spawned", 610); spawned.Outcome != HandoffClaimNotLaunched {
		t.Fatalf("a process the opened session spawned is refused once the launched session is known: %+v", spawned)
	}
	if mine := claimTicket(t, ix, known.TicketID, "claude", "ses-launched", 620); mine.Outcome != HandoffClaimClaimed {
		t.Fatalf("the launched session claims: %+v", mine)
	}
}

// §6.4 rule 3, K-4: a claim that arrives before the frame is accepted and checked when
// the frame arrives; if they differ the claim, the armed brief and a NEW started
// receipt move to the frame's session.
func TestClaimMadeBeforeTheFrameMovesToTheLaunchedSession(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	if _, err := ix.LaunchHandoffOpen(ticket.TicketID, "claude", "task_1"); err != nil {
		t.Fatal(err)
	}
	if early := claimTicket(t, ix, ticket.TicketID, "claude", "ses-spawned", 400); early.Outcome != HandoffClaimClaimed {
		t.Fatalf("a claim before the frame is accepted: %+v", early)
	}
	if late := claimTicket(t, ix, ticket.TicketID, "claude", "ses-launched", 401); late.Outcome != HandoffClaimAlreadyClaimed {
		t.Fatalf("the launched session's own entry does not claim a held ticket: %+v", late)
	}
	frame, err := ix.RecordHandoffOpenFrame(ticket.TicketID, "ses-launched", 410, briefPolicy)
	if err != nil || !frame.Recorded || !frame.Moved || !frame.Receipt {
		t.Fatalf("the frame moves the claim: %+v %v", frame, err)
	}
	moved := mustTicket(t, ix, ticket.TicketID)
	if moved.ClaimedNativeID != "ses-launched" || moved.LaunchedNativeID != "ses-launched" || moved.State != HandoffOpenClaimed {
		t.Fatalf("the claim is the launched session's: %+v", moved)
	}
	rows := briefRows(t, ix, ticket.TicketID)
	if len(rows) != 2 || rows[0].State != "expired" || rows[0].Detail != HandoffBriefClaimMoved || rows[0].NativeSessionID != "ses-spawned" ||
		rows[1].State != "pending" || rows[1].NativeSessionID != "ses-launched" {
		t.Fatalf("the armed brief moved: %+v", rows)
	}
	if handed := handOver(t, ix, "claude", "ses-spawned", "trn_1", 420); len(handed) != 0 {
		t.Fatalf("the other session is handed nothing: %+v", handed)
	}
	if receiptsOf(t, ix, teamwire.TransitionStarted) != 2 {
		t.Fatal("a new started receipt names the frame's session (its id includes the native id)")
	}
	// A second frame of the same task changes nothing.
	if again, _ := ix.RecordHandoffOpenFrame(ticket.TicketID, "ses-other", 430, briefPolicy); again.Recorded || again.Moved {
		t.Fatalf("only the first frame counts: %+v", again)
	}
}

// §6.5 "once-only is the ticket's state" and criterion 77: the brief is handed over at
// one prompt and to none after; the opened receipt is enqueued only when
// brief_confirmed_at was empty, so a second confirmation after Deliver again or a
// compaction re-delivery enqueues none.
func TestBriefIsHandedOverOnceAndOpenedIsSentOnce(t *testing.T) {
	ix := handoffDevice(t)
	rec, body := receivedHandoff(t, ix, 1)
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 400)

	first := handOver(t, ix, "claude", "ses-a", "trn_1", 410)
	if len(first) != 1 || first[0].Message != "the brief" || first[0].RunID != HandoffDeliveryRun(ticket.TicketID) {
		t.Fatalf("the first prompt carries the brief: %+v", first)
	}
	if second := handOver(t, ix, "claude", "ses-a", "trn_2", 411); len(second) != 0 {
		t.Fatalf("a second prompt of the same session is handed the brief again: %+v", second)
	}
	confirmed, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-a", 412)
	if err != nil || !confirmed.First || !confirmed.Receipt || confirmed.HandoffID != rec.ID {
		t.Fatalf("the first confirmation enqueues opened: %+v %v", confirmed, err)
	}
	if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffOpened {
		t.Fatalf("the handoff is opened here: %s", h.State)
	}
	if got := mustTicket(t, ix, ticket.TicketID); got.BriefConfirmedAt != 412 || got.BriefDue {
		t.Fatalf("confirmed and no longer owed: %+v", got)
	}
	if third := handOver(t, ix, "claude", "ses-a", "trn_3", 420); len(third) != 0 {
		t.Fatalf("later console turns receive nothing again: %+v", third)
	}

	// Deliver again on an opened item: re-armed for the claimed session.
	if armed, err := ix.DeliverHandoffBriefAgain(ticket.TicketID, 500, briefPolicy); err != nil || !armed {
		t.Fatalf("deliver again re-arms: %v %v", armed, err)
	}
	if again := handOver(t, ix, "claude", "ses-a", "trn_4", 510); len(again) != 1 {
		t.Fatalf("the next prompt carries it: %+v", again)
	}
	second, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-a", 512)
	if err != nil || second.First || second.Receipt {
		t.Fatalf("a second confirmation is not the first and enqueues no opened: %+v %v", second, err)
	}
	if got := mustTicket(t, ix, ticket.TicketID); got.BriefConfirmedAt != 412 {
		t.Fatalf("brief_confirmed_at is set once: %d", got.BriefConfirmedAt)
	}
	if receiptsOf(t, ix, teamwire.TransitionOpened) != 1 {
		t.Fatalf("never a second opened: %d", receiptsOf(t, ix, teamwire.TransitionOpened))
	}

	// A withdrawal that lands after the confirmation keeps the copy a session holds.
	landPage(t, ix, 1, 2, pulled(2, rec, nil, teamwire.HandoffWithdrawn))
	if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.WireBody != string(body) {
		t.Fatal("a confirmed copy is not erased by a later withdrawal")
	}
	if _, err := ix.DeliverHandoffBriefAgain(ticket.TicketID, 600, briefPolicy); refusalCode(err) != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("deliver again on a withdrawn handoff is refused: %v", err)
	}
}

// §6.5: handoff rows sort first in the claim, ahead of older helper rows, and are
// claimable only at a boundary that says it carries them.
func TestBriefSortsFirstAndOnlyAtACarryingBoundary(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	if err := ix.EnqueueSessionDelivery(SessionDelivery{DeliveryID: "odel_old", RunID: "orun_old", Runtime: "codex", NativeSessionID: "ses-c",
		Message: "an older helper message", CreatedAt: 100, ExpiresAt: 5000}, 3); err != nil {
		t.Fatal(err)
	}
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	claimTicket(t, ix, ticket.TicketID, "codex", "ses-c", 400)

	tool, err := ix.ClaimSessionDeliveriesAt(SessionDeliveryClaim{Runtime: "codex", SessionID: "ses-c", Kind: "tool.started",
		ObservationID: "obs_1", Now: 410, MaxBytes: 7000})
	if err != nil || len(tool) != 1 || tool[0].DeliveryID != "odel_old" {
		t.Fatalf("a boundary that does not carry a handoff takes the helper row and leaves the brief: %+v %v", tool, err)
	}
	if rows := briefRows(t, ix, ticket.TicketID); len(rows) != 1 || rows[0].State != "pending" {
		t.Fatalf("the brief is still pending: %+v", rows)
	}
	if err := ix.EnqueueSessionDelivery(SessionDelivery{DeliveryID: "odel_older", RunID: "orun_older", Runtime: "codex", NativeSessionID: "ses-c",
		Message: "another helper message", CreatedAt: 50, ExpiresAt: 5000}, 3); err != nil {
		t.Fatal(err)
	}
	prompt := handOver(t, ix, "codex", "ses-c", "trn_1", 420)
	if len(prompt) != 2 || prompt[0].RunID != HandoffDeliveryRun(ticket.TicketID) || prompt[1].DeliveryID != "odel_older" {
		t.Fatalf("the brief first, ahead of an older helper row: %+v", prompt)
	}
	// The default claim never carries a brief.
	claimTicket(t, ix, openTicket(t, ix, handoffTestOrg, rec.ID, "codex").TicketID, "codex", "ses-d", 430)
	if plain, _ := ix.ClaimSessionDeliveries("codex", "ses-d", "turn.started", "trn_2", "", 440, 7000); len(plain) != 0 {
		t.Fatalf("a caller that does not say it carries a handoff is handed none: %+v", plain)
	}
}

// Criterion 67 and §6.7 "land withdrawn": every waiting or claimed ticket is
// cancelled, the armed brief is cancelled and never delivered, an unopened copy is
// blanked, and a send in a composer whose ticket a withdrawal cancelled is refused
// with the withdrawal's code.
func TestWithdrawalCancelsTicketsAndTheArmedBrief(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	waiting := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	claimed := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	claimTicket(t, ix, claimed.TicketID, "claude", "ses-a", 400)

	landPage(t, ix, 1, 2, pulled(2, rec, nil, teamwire.HandoffWithdrawn))
	for _, id := range []string{waiting.TicketID, claimed.TicketID} {
		if got := mustTicket(t, ix, id); got.State != HandoffOpenCancelled || got.CancelReason != teamwire.CodeHandoffWithdrawn {
			t.Fatalf("cancelled by the withdrawal: %+v", got)
		}
	}
	if got := mustTicket(t, ix, claimed.TicketID); got.ClaimedNativeID != "ses-a" || got.BriefDue {
		t.Fatalf("a cancelled claimed ticket keeps the facts of its claim and owes no brief: %+v", got)
	}
	rows := briefRows(t, ix, claimed.TicketID)
	if len(rows) != 1 || rows[0].State != "expired" || rows[0].Detail != HandoffBriefWithdrawn {
		t.Fatalf("the armed brief is cancelled with the named detail: %+v", rows)
	}
	if handed := handOver(t, ix, "claude", "ses-a", "trn_1", 500); len(handed) != 0 {
		t.Fatalf("a brief is never delivered after the withdrawal lands: %+v", handed)
	}
	if eff, err := ix.SweepHandoffBriefs(HandoffBriefSweep{Now: 5000, Policy: briefPolicy, BriefWaitSeconds: 100000}); err != nil || eff.Armed != 0 {
		t.Fatalf("the sweep re-arms nothing for a withdrawn handoff: %+v %v", eff, err)
	}
	if _, err := ix.LaunchHandoffOpen(waiting.TicketID, "claude", "task_1"); refusalCode(err) != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("a send in the composer is refused with the withdrawal: %v", err)
	}
	if late := claimTicket(t, ix, waiting.TicketID, "claude", "ses-late", 600); late.Outcome != HandoffClaimEnded {
		t.Fatalf("a cancelled ticket is never claimed later: %+v", late)
	}
	if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.WireBody != "" || h.Title != "" {
		t.Fatalf("withdrawn after started and before opened: the copy is erased: %+v", h)
	}
	if _, err := ix.DeliverHandoffBriefAgain(claimed.TicketID, 700, briefPolicy); refusalCode(err) != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("deliver again is refused: %v", err)
	}
	if _, err := ix.CreateHandoffOpen(HandoffOpenCreate{OrganizationID: handoffTestOrg, HandoffID: rec.ID, Runtime: "claude", At: 800}); refusalCode(err) != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("no Open on a withdrawn handoff: %v", err)
	}
}

// Landing expired on a held, unopened copy blanks it as a withdrawal does: the server
// erases the title and the text at expiry. An opened copy keeps its text.
func TestExpiryBlanksAHeldUnopenedCopyAndKeepsAnOpenedOne(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	eff := landPage(t, ix, 1, 2, pulled(2, rec, nil, teamwire.HandoffExpired))
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if eff.Erased != 1 || h.State != teamwire.HandoffExpired || h.WireBody != "" || h.Title != "" || !h.Held {
		t.Fatalf("an expired unopened copy is blanked: %+v %+v", eff, h)
	}
	if got := mustTicket(t, ix, ticket.TicketID); got.State != HandoffOpenCancelled || got.CancelReason != teamwire.CodeHandoffExpired {
		t.Fatalf("its waiting ticket is cancelled: %+v", got)
	}

	opened, openedBody := handoffRecord("Opened one", "opened body", "usr_me")
	landPage(t, ix, 2, 3, pulled(3, opened, openedBody, teamwire.HandoffReceived))
	openedTicket := openTicket(t, ix, handoffTestOrg, opened.ID, "claude")
	claimTicket(t, ix, openedTicket.TicketID, "claude", "ses-o", 400)
	handOver(t, ix, "claude", "ses-o", "trn_1", 410)
	if _, err := ix.ConfirmHandoffBrief(openedTicket.TicketID, "claude", "ses-o", 411); err != nil {
		t.Fatal(err)
	}
	if eff := landPage(t, ix, 3, 4, pulled(4, opened, nil, teamwire.HandoffExpired)); eff.Erased != 0 {
		t.Fatalf("an opened copy is not erased: %+v", eff)
	}
	if h := mustHandoff(t, ix, handoffTestOrg, opened.ID); h.WireBody != string(openedBody) {
		t.Fatal("a session that was confirmed keeps its text across an expiry")
	}
	// A served document is never kept for a row that lands already expired.
	served, servedBody := handoffRecord("served", "served body", "usr_me")
	landPage(t, ix, 4, 5, pulled(5, served, servedBody, teamwire.HandoffExpired))
	if h := mustHandoff(t, ix, handoffTestOrg, served.ID); h.WireBody != "" || h.Title != "" {
		t.Fatalf("an expired handoff's document is never kept: %+v", h)
	}
}

// Criterion 68: a ticket waiting at unlink is cancelled and never claimed later; what
// was received stays openable locally, by the same ticket, claim and delivery, and
// nothing is sent.
func TestUnlinkCancelsWaitingTicketsAndOpensLocallyAfterwards(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	waiting := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	held := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	claimTicket(t, ix, held.TicketID, "claude", "ses-held", 350)
	if err := ix.SetLinked(false); err != nil {
		t.Fatal(err)
	}
	if err := ix.ResetSyncQueue(); err != nil {
		t.Fatal(err)
	}
	if got := mustTicket(t, ix, waiting.TicketID); got.State != HandoffOpenCancelled || got.CancelReason != HandoffCodeLinkEnded {
		t.Fatalf("a ticket waiting at unlink is cancelled and shown as cancelled: %+v", got)
	}
	if late := claimTicket(t, ix, waiting.TicketID, "claude", "ses-late", 400); late.Outcome != HandoffClaimEnded {
		t.Fatalf("never claimed later: %+v", late)
	}
	if got := mustTicket(t, ix, held.TicketID); got.State != HandoffOpenClaimed {
		t.Fatalf("a claimed ticket keeps its claim across an unlink: %+v", got)
	}

	after := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	got := claimTicket(t, ix, after.TicketID, "codex", "ses-local", 500)
	if got.Outcome != HandoffClaimClaimed || got.Receipt || !got.BriefArmed {
		t.Fatalf("opens locally after unlink, with no receipt: %+v", got)
	}
	if handed := handOver(t, ix, "codex", "ses-local", "trn_1", 510); len(handed) != 1 {
		t.Fatalf("the brief is delivered: %+v", handed)
	}
	confirmed, err := ix.ConfirmHandoffBrief(after.TicketID, "codex", "ses-local", 511)
	if err != nil || !confirmed.First || confirmed.Receipt {
		t.Fatalf("confirmed with nothing sent: %+v %v", confirmed, err)
	}
	if rows := pendingOfKind(t, ix, OutboxHandoffReceipt); len(rows) != 0 {
		t.Fatalf("nothing is sent after unlink: %+v", rows)
	}
	// Linked elsewhere, an ended link's row still sends nothing.
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetLinkedOrganization("org_other", 2); err != nil {
		t.Fatal(err)
	}
	elsewhere := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	if got := claimTicket(t, ix, elsewhere.TicketID, "codex", "ses-elsewhere", 600); got.Outcome != HandoffClaimClaimed || got.Receipt {
		t.Fatalf("no receipt of the earlier organization's handoff reaches another organization's queue: %+v", got)
	}
}

// A local handoff opens by the same ticket, claim and delivery with no server and no
// receipt, on a device that was never linked.
func TestLocalHandoffOpensWithNoReceipt(t *testing.T) {
	ix := openResultTestIndex(t)
	local, body := handoffRecord("local", "local body", "")
	if err := ix.SendHandoff(HandoffSend{Record: local, Body: body, At: 5}); err != nil {
		t.Fatal(err)
	}
	ticket := openTicket(t, ix, "", local.ID, "claude")
	if got := claimTicket(t, ix, ticket.TicketID, "claude", "ses-l", 10); got.Outcome != HandoffClaimClaimed || got.Receipt || !got.BriefArmed {
		t.Fatalf("a local claim: %+v", got)
	}
	handOver(t, ix, "claude", "ses-l", "trn_1", 11)
	if confirmed, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-l", 12); err != nil || !confirmed.First || confirmed.Receipt {
		t.Fatalf("a local confirmation: %+v %v", confirmed, err)
	}
	if h := mustHandoff(t, ix, "", local.ID); h.State != teamwire.HandoffOpened {
		t.Fatalf("opened locally: %s", h.State)
	}
	if rows := pendingOfKind(t, ix, OutboxHandoffReceipt); len(rows) != 0 {
		t.Fatalf("a local handoff has no receipts: %+v", rows)
	}
	gotTicket, gotHandoff, found, err := ix.HandoffOpenForSession("claude", "ses-l")
	if err != nil || !found || gotTicket.TicketID != ticket.TicketID || gotHandoff.ID != local.ID {
		t.Fatalf("the session's ticket is found by (runtime, native id): %+v %v", gotTicket, err)
	}
	if _, _, found, _ := ix.HandoffOpenForSession("codex", "ses-l"); found {
		t.Fatal("another runtime's session of the same id holds no ticket")
	}
}

// Criterion 86 and §6.5 "re-arm", "give up": a row handed over with no confirmation, a
// row that reached its expiry, and a row past its lifetime that nothing expired are
// each replaced by a new pending row with the same text; past the wait re-arming stops
// and nothing is sent as opened.
func TestSweepRearmsALostBriefAndGivesUpAfterTheWait(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 1000)
	sweep := func(now, grace int64) HandoffBriefSweepEffects {
		t.Helper()
		eff, err := ix.SweepHandoffBriefs(HandoffBriefSweep{Now: now, Policy: briefPolicy, BriefWaitSeconds: 10000, HandedOverGraceSeconds: grace})
		if err != nil {
			t.Fatal(err)
		}
		return eff
	}
	if eff := sweep(1001, 30); eff != (HandoffBriefSweepEffects{}) {
		t.Fatalf("an armed brief waiting for its prompt is left alone: %+v", eff)
	}

	// The daemon died after the hand-over and before the confirmation.
	if handed := handOver(t, ix, "claude", "ses-a", "trn_1", 1010); len(handed) != 1 {
		t.Fatal("handed over")
	}
	if eff := sweep(1015, 30); eff.Armed != 0 {
		t.Fatalf("a row handed over a moment ago is not taken for lost: %+v", eff)
	}
	if eff := sweep(1015, 0); eff.Armed != 1 {
		t.Fatalf("at start-up a handed-over row with no confirmation is re-armed: %+v", eff)
	}
	rows := briefRows(t, ix, ticket.TicketID)
	if len(rows) != 2 || rows[1].State != "pending" || rows[1].Message != rows[0].Message {
		t.Fatalf("a new pending row with the same text: %+v", rows)
	}

	// The 1,800 s expiry, run by the table's own sweep.
	if expired, err := ix.ExpireSessionDeliveries(1015 + briefPolicy.TTLSeconds); err != nil || len(expired) != 1 {
		t.Fatalf("the table's expiry: %+v %v", expired, err)
	}
	if eff := sweep(3000, 30); eff.Armed != 1 || eff.Expired != 0 {
		t.Fatalf("an expired row is re-armed: %+v", eff)
	}
	// Past its lifetime and still pending: the handoff owner expires its own row.
	if eff := sweep(3000+briefPolicy.TTLSeconds, 30); eff.Armed != 1 || eff.Expired != 1 {
		t.Fatalf("an overdue pending row is expired and re-armed here: %+v", eff)
	}
	if handed := handOver(t, ix, "claude", "ses-a", "trn_2", 5000); len(handed) != 1 {
		t.Fatalf("the brief is delivered in the end: %+v", handed)
	}
	if confirmed, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-a", 5001); err != nil || !confirmed.Receipt {
		t.Fatalf("and one opened: %+v %v", confirmed, err)
	}
	if eff := sweep(5100, 0); eff != (HandoffBriefSweepEffects{}) {
		t.Fatalf("a confirmed brief is not re-armed: %+v", eff)
	}
	if receiptsOf(t, ix, teamwire.TransitionOpened) != 1 {
		t.Fatal("one opened")
	}

	// Give up: a second ticket never gets a prompt.
	lost := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	claimTicket(t, ix, lost.TicketID, "claude", "ses-b", 6000)
	if eff := sweep(6000+10000, 30); eff.GaveUp != 1 || eff.Armed != 0 {
		t.Fatalf("past the wait re-arming stops: %+v", eff)
	}
	gone := mustTicket(t, ix, lost.TicketID)
	if gone.BriefGivenUpAt == 0 || gone.BriefConfirmedAt != 0 {
		t.Fatalf("brief not delivered: %+v", gone)
	}
	if handed := handOver(t, ix, "claude", "ses-b", "trn_3", 16001); len(handed) != 0 {
		t.Fatalf("nothing is delivered after the device gave up: %+v", handed)
	}
	if eff := sweep(20000, 0); eff != (HandoffBriefSweepEffects{}) {
		t.Fatalf("a given-up brief is not swept again: %+v", eff)
	}
	if receiptsOf(t, ix, teamwire.TransitionOpened) != 1 {
		t.Fatal("no opened was sent for it")
	}
	// Deliver again reaches it afterwards.
	if armed, err := ix.DeliverHandoffBriefAgain(lost.TicketID, 21000, briefPolicy); err != nil || !armed {
		t.Fatalf("deliver again after give-up: %v %v", armed, err)
	}
	if got := mustTicket(t, ix, lost.TicketID); got.BriefGivenUpAt != 0 {
		t.Fatalf("asked for again: %+v", got)
	}
}

// §6.5: if the session already holds the table's pending cap, the brief is armed at
// the next sweep.
func TestBriefWaitsForRoomUnderThePendingCap(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	for _, id := range []string{"odel_1", "odel_2", "odel_3"} {
		if err := ix.EnqueueSessionDelivery(SessionDelivery{DeliveryID: id, RunID: "orun_" + id, Runtime: "claude", NativeSessionID: "ses-a",
			Message: "helper " + id, CreatedAt: 100, ExpiresAt: 9000}, 3); err != nil {
			t.Fatal(err)
		}
	}
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	if got := claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 400); got.Outcome != HandoffClaimClaimed || got.BriefArmed {
		t.Fatalf("claimed with the brief waiting for room: %+v", got)
	}
	if eff, _ := ix.SweepHandoffBriefs(HandoffBriefSweep{Now: 410, Policy: briefPolicy, BriefWaitSeconds: 10000}); eff.Waiting != 1 || eff.Armed != 0 {
		t.Fatalf("still no room: %+v", eff)
	}
	if _, err := ix.ClaimSessionDeliveries("claude", "ses-a", "tool.started", "obs_1", "", 420, 7000); err != nil {
		t.Fatal(err)
	}
	if eff, _ := ix.SweepHandoffBriefs(HandoffBriefSweep{Now: 430, Policy: briefPolicy, BriefWaitSeconds: 10000}); eff.Armed != 1 {
		t.Fatalf("armed at the next sweep: %+v", eff)
	}
}

// §6.4 rule 5: a compaction of the claimed session arms the brief once more while the
// handoff is not terminal; a new id (clear, fork) reaches no ticket.
func TestCompactionOfTheClaimedSessionRearmsTheBrief(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	claimTicket(t, ix, ticket.TicketID, "codex", "ses-a", 400)
	handOver(t, ix, "codex", "ses-a", "trn_1", 410)
	if _, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-a", 411); err != nil {
		t.Fatal(err)
	}
	rearm := func(nativeID string, at int64) int {
		t.Helper()
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		asked, err := ix.RearmHandoffBriefsForSessionTx(tx, "codex", nativeID, at, briefPolicy)
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return asked
	}
	if has, _ := ix.HasClaimedHandoffOpen("codex", "ses-new"); has {
		t.Fatal("a cleared or forked session is a new id and holds no ticket")
	}
	if rearm("ses-new", 500) != 0 {
		t.Fatal("a new id re-arms nothing")
	}
	if has, _ := ix.HasClaimedHandoffOpen("codex", "ses-a"); !has || rearm("ses-a", 500) != 1 {
		t.Fatal("a compaction of the claimed session arms the brief once more")
	}
	if rearm("ses-a", 501) != 1 || len(briefRows(t, ix, ticket.TicketID)) != 2 {
		t.Fatalf("a second compaction before the next prompt arms no second row: %+v", briefRows(t, ix, ticket.TicketID))
	}
	if handed := handOver(t, ix, "codex", "ses-a", "trn_2", 510); len(handed) != 1 {
		t.Fatalf("the next prompt carries it: %+v", handed)
	}
	if confirmed, _ := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-a", 511); confirmed.First || confirmed.Receipt {
		t.Fatalf("no second opened after a compaction re-delivery: %+v", confirmed)
	}
	landPage(t, ix, 1, 2, pulled(2, rec, nil, teamwire.HandoffClosed))
	if rearm("ses-a", 600) != 0 {
		t.Fatal("never re-armed once the handoff is terminal")
	}
}

// §6.3, criteria 84 and 94: a launched turn that ends with no claim cancels the ticket
// in one of two ways; only "a session started and the hook did not run" records the
// fact for the runtime; and a spooled entry replayed late still claims.
func TestLaunchedTurnThatEndsWithoutAClaim(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	notStarted := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	if _, err := ix.LaunchHandoffOpen(notStarted.TicketID, "codex", "task_1"); err != nil {
		t.Fatal(err)
	}
	if ended, err := ix.EndHandoffOpenLaunch(notStarted.TicketID, HandoffOpenRuntimeNotStarted, "codex was not found", 500); err != nil || !ended {
		t.Fatalf("ended: %v %v", ended, err)
	}
	if got := mustTicket(t, ix, notStarted.TicketID); got.State != HandoffOpenCancelled || got.CancelReason != HandoffOpenRuntimeNotStarted || got.CancelDetail != "codex was not found" {
		t.Fatalf("could not start, with the task's reason: %+v", got)
	}
	if firing, _ := ix.HandoffRuntimeFiring("codex", "turn.started", 0); firing.UnclaimedLaunchAt != 0 {
		t.Fatalf("a runtime that never started a session does not clear readiness: %+v", firing)
	}

	noHook := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	if _, err := ix.LaunchHandoffOpen(noHook.TicketID, "codex", "task_2"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.RecordHandoffOpenFrame(noHook.TicketID, "ses-launched", 510, briefPolicy); err != nil {
		t.Fatal(err)
	}
	if ended, _ := ix.EndHandoffOpenLaunch(noHook.TicketID, HandoffOpenHookDidNotRun, "", 520); !ended {
		t.Fatal("ended")
	}
	if firing, _ := ix.HandoffRuntimeFiring("codex", "turn.started", 0); firing.UnclaimedLaunchAt != 520 {
		t.Fatalf("the unclaimed launch is recorded for the runtime: %+v", firing)
	}
	if other := claimTicket(t, ix, noHook.TicketID, "codex", "ses-other", 530); other.Outcome != HandoffClaimEnded {
		t.Fatalf("another session never claims an ended ticket: %+v", other)
	}
	if late := claimTicket(t, ix, noHook.TicketID, "codex", "ses-launched", 540); late.Outcome != HandoffClaimClaimed {
		t.Fatalf("the launched session's spooled entry, replayed late, claims: %+v", late)
	}
	if firing, _ := ix.HandoffRuntimeFiring("codex", "turn.started", 0); firing.UnclaimedLaunchAt != 0 {
		t.Fatalf("the hook did run: the mark is withdrawn: %+v", firing)
	}

	// A claimed ticket is not cancelled by its turn ending; its launch is settled.
	claimed := openTicket(t, ix, handoffTestOrg, rec.ID, "codex")
	if _, err := ix.LaunchHandoffOpen(claimed.TicketID, "codex", "task_3"); err != nil {
		t.Fatal(err)
	}
	claimTicket(t, ix, claimed.TicketID, "codex", "ses-claimed", 545)
	if launched, _ := ix.HandoffOpensLaunched(); len(launched) != 1 || launched[0].TicketID != claimed.TicketID {
		t.Fatalf("a launched turn that has not ended is still watched, claimed or not: %+v", launched)
	}
	if ended, _ := ix.EndHandoffOpenLaunch(claimed.TicketID, HandoffOpenRuntimeNotStarted, "", 550); ended {
		t.Fatal("a claimed ticket is left alone")
	}
	if got := mustTicket(t, ix, claimed.TicketID); got.State != HandoffOpenClaimed {
		t.Fatalf("still claimed: %+v", got)
	}
	if launched, _ := ix.HandoffOpensLaunched(); len(launched) != 0 {
		t.Fatalf("nothing left to watch: %+v", launched)
	}
}

// Red-team M4: a brief that was handed over when its handoff was withdrawn, its claim
// moved, or the device gave up on it, was not pending, so none of those cancelled it;
// the reply then failed and the release put it back to pending, to be delivered after
// the withdrawal had landed. The release now ends such a row. A release with nothing
// in its way still returns the brief to pending.
func TestAReleasedBriefIsNotDeliveredAfterAWithdrawalAGiveUpOrAMovedClaim(t *testing.T) {
	release := func(t *testing.T, ix *Index, handed []SessionDelivery, observation string) int64 {
		t.Helper()
		if len(handed) != 1 {
			t.Fatalf("the brief was not handed over: %+v", handed)
		}
		released, err := ix.ReleaseSessionDeliveries([]string{handed[0].DeliveryID}, observation)
		if err != nil {
			t.Fatal(err)
		}
		return released
	}
	wantEnded := func(t *testing.T, ix *Index, ticketID, session, detail string, released int64) {
		t.Helper()
		rows := briefRows(t, ix, ticketID)
		if released != 0 || rows[0].State != "expired" || rows[0].Detail != detail {
			t.Fatalf("the released brief must end %s, not go back to pending: released=%d rows=%+v", detail, released, rows)
		}
		if handed := handOver(t, ix, "claude", session, "trn_next", 900); len(handed) != 0 {
			t.Fatalf("the brief was delivered afterwards: %+v", handed)
		}
	}

	t.Run("nothing in the way", func(t *testing.T) {
		ix := handoffDevice(t)
		rec, _ := receivedHandoff(t, ix, 1)
		ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
		claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 400)
		if released := release(t, ix, handOver(t, ix, "claude", "ses-a", "trn_1", 410), "trn_1"); released != 1 {
			t.Fatalf("a brief whose reply failed goes back to pending: %d", released)
		}
		if handed := handOver(t, ix, "claude", "ses-a", "trn_2", 420); len(handed) != 1 {
			t.Fatalf("and is delivered at the next prompt: %+v", handed)
		}
	})
	t.Run("withdrawal lands while handed over", func(t *testing.T) {
		ix := handoffDevice(t)
		rec, _ := receivedHandoff(t, ix, 1)
		ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
		claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 400)
		handed := handOver(t, ix, "claude", "ses-a", "trn_1", 410)
		landPage(t, ix, 1, 2, pulled(2, rec, nil, teamwire.HandoffWithdrawn))
		wantEnded(t, ix, ticket.TicketID, "ses-a", HandoffBriefWithdrawn, release(t, ix, handed, "trn_1"))
	})
	t.Run("give-up while handed over", func(t *testing.T) {
		ix := handoffDevice(t)
		rec, _ := receivedHandoff(t, ix, 1)
		ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
		claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 400)
		handed := handOver(t, ix, "claude", "ses-a", "trn_1", 410)
		if eff, err := ix.SweepHandoffBriefs(HandoffBriefSweep{Now: 600, Policy: briefPolicy, BriefWaitSeconds: 100}); err != nil || eff.GaveUp != 1 {
			t.Fatalf("give up: %+v %v", eff, err)
		}
		wantEnded(t, ix, ticket.TicketID, "ses-a", HandoffBriefWaitElapsed, release(t, ix, handed, "trn_1"))
	})
	t.Run("claim moved while handed over", func(t *testing.T) {
		ix := handoffDevice(t)
		rec, _ := receivedHandoff(t, ix, 1)
		ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
		if _, err := ix.LaunchHandoffOpen(ticket.TicketID, "claude", "task_1"); err != nil {
			t.Fatal(err)
		}
		claimTicket(t, ix, ticket.TicketID, "claude", "ses-spawned", 400)
		handed := handOver(t, ix, "claude", "ses-spawned", "trn_1", 410)
		if frame, err := ix.RecordHandoffOpenFrame(ticket.TicketID, "ses-launched", 420, briefPolicy); err != nil || !frame.Moved {
			t.Fatalf("the claim moves to the launched session: %+v %v", frame, err)
		}
		released := release(t, ix, handed, "trn_1")
		rows := briefRows(t, ix, ticket.TicketID)
		if released != 0 || rows[0].State != "expired" || rows[0].Detail != HandoffBriefClaimMoved {
			t.Fatalf("the spawned session's brief must end claim_moved: released=%d rows=%+v", released, rows)
		}
		if handed := handOver(t, ix, "claude", "ses-spawned", "trn_2", 430); len(handed) != 0 {
			t.Fatalf("the other session got the brief after the claim moved: %+v", handed)
		}
		if handed := handOver(t, ix, "claude", "ses-launched", "trn_3", 440); len(handed) != 1 {
			t.Fatalf("the launched session still gets its brief: %+v", handed)
		}
	})
}

// Red-team Low 1: a confirmation is the delivered row's session's, not the ticket's
// alone. A row handed to a spawned session whose claim then moved to the launched
// session (§6.4 rule 3) confirms nothing: the launched session has not been handed the
// brief, so the handoff is not opened and no opened receipt leaves.
func TestOnlyTheClaimingSessionsDeliveryConfirmsTheBrief(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := receivedHandoff(t, ix, 1)
	ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
	if _, err := ix.LaunchHandoffOpen(ticket.TicketID, "claude", "task_1"); err != nil {
		t.Fatal(err)
	}
	claimTicket(t, ix, ticket.TicketID, "claude", "ses-spawned", 400)
	if handed := handOver(t, ix, "claude", "ses-spawned", "trn_1", 410); len(handed) != 1 {
		t.Fatalf("handed to the spawned session: %+v", handed)
	}
	if frame, err := ix.RecordHandoffOpenFrame(ticket.TicketID, "ses-launched", 420, briefPolicy); err != nil || !frame.Moved {
		t.Fatalf("the claim moves: %+v %v", frame, err)
	}
	stale, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-spawned", 421)
	if err != nil || stale.First || stale.Receipt {
		t.Fatalf("the other session's delivery confirmed the brief: %+v %v", stale, err)
	}
	if got := mustTicket(t, ix, ticket.TicketID); got.BriefConfirmedAt != 0 || !got.BriefDue {
		t.Fatalf("the launched session is still owed the brief: %+v", got)
	}
	if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffStarted || receiptsOf(t, ix, teamwire.TransitionOpened) != 0 {
		t.Fatalf("nothing is opened: state=%s", h.State)
	}
	if handed := handOver(t, ix, "claude", "ses-launched", "trn_2", 430); len(handed) != 1 {
		t.Fatalf("handed to the launched session: %+v", handed)
	}
	if own, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-launched", 431); err != nil || !own.First || !own.Receipt {
		t.Fatalf("the launched session's own delivery confirms: %+v %v", own, err)
	}
}
