package store

import (
	"database/sql"
	"fmt"
	"strings"

	"crossing-guard/engine"
	"crossing-guard/teamwire"
)

// Opening a handoff (team rest-of-release plan §6.3–§6.5, §6.7). This file is the open
// ticket's one owner: the ticket a console Open writes, its launch, its claim by the
// session that launch started, the brief armed for that session as a session_delivery
// row, the confirmation that enqueues the one opened receipt, and every way a ticket or
// its brief is cancelled. The brief's hand-over and release are the session_delivery
// owner's (session_delivery.go); nothing here joins one session identity on another.

// The ticket's states. A ticket has no timer: it ends by claim or by cancellation.
const (
	HandoffOpenWaiting   = "waiting"
	HandoffOpenClaimed   = "claimed"
	HandoffOpenCancelled = "cancelled"
)

// Why a ticket was cancelled, as data codes. A handoff that reached a terminal state
// cancels with that state's wire code (teamwire.HandoffTerminalCode); an ended link
// with HandoffCodeLinkEnded.
const (
	// HandoffOpenComposerClosed: the composer was closed without sending.
	HandoffOpenComposerClosed = "composer_closed"
	// HandoffOpenRuntimeNotStarted: the launched turn ended and the runtime never
	// started a session (no session frame). The only case Retry is offered.
	HandoffOpenRuntimeNotStarted = "runtime_not_started"
	// HandoffOpenHookDidNotRun: a session started and made no claim — the lifecycle
	// hook did not run in it. Readiness for the runtime then needs newer rows.
	HandoffOpenHookDidNotRun = "hook_did_not_run"
)

// Refusal codes of the open, launch and deliver-again steps.
const (
	HandoffCodeNoText          = "no_text"
	HandoffCodeTicketNotFound  = "ticket_not_found"
	HandoffCodeTicketUsed      = "ticket_already_used"
	HandoffCodeTicketCancelled = "ticket_cancelled"
	HandoffCodeTicketRuntime   = "ticket_runtime_mismatch"
	HandoffCodeNotClaimed      = "not_claimed"
)

// The session_delivery detail codes a handoff's brief rows end with (§6.5: cancelled is
// the table's expired state with a named detail; the table's schema does not change).
const (
	HandoffBriefWithdrawn       = "handoff_withdrawn"
	HandoffBriefTicketCancelled = "ticket_cancelled"
	HandoffBriefClaimMoved      = "claim_moved"
	HandoffBriefWaitElapsed     = "brief_wait_elapsed"
	handoffBriefOverdue         = "no carrier boundary before expiry"
)

// HandoffDeliveryRunPrefix marks a session_delivery row as a handoff's brief: its
// run_id is the prefix and the ticket id, which no managed run and no invocation has.
const HandoffDeliveryRunPrefix = "handoff:"

// HandoffDeliveryRun is the run_id of a ticket's brief rows.
func HandoffDeliveryRun(ticketID string) string { return HandoffDeliveryRunPrefix + ticketID }

// HandoffTicketForDeliveryRun reads the ticket id out of a brief row's run_id.
func HandoffTicketForDeliveryRun(runID string) (string, bool) {
	ticket, ok := strings.CutPrefix(runID, HandoffDeliveryRunPrefix)
	return ticket, ok && ticket != ""
}

// handoffBriefBoundary is the boundary column of a brief row: where it lands, in our
// own words.
const handoffBriefBoundary = "the session's next prompt"

// HandoffOpen is one ticket row.
type HandoffOpen struct {
	TicketID          string
	OrganizationID    string
	HandoffID         string
	Runtime           string
	CheckoutRoot      string
	LaunchedTask      string
	LaunchedNativeID  string
	State             string
	CancelReason      string
	CancelDetail      string
	ClaimedRuntime    string
	ClaimedNativeID   string
	ClaimedTranscript string
	BriefText         string
	BriefDue          bool
	BriefRequestedAt  int64
	BriefConfirmedAt  int64
	BriefGivenUpAt    int64
	CreatedAt         int64
	ClaimedAt         int64
	EndedAt           int64
}

// Claimed reports whether a session ever claimed the ticket and still holds the claim.
// A ticket cancelled because its handoff ended keeps the claim's facts.
func (t HandoffOpen) Claimed() bool { return t.ClaimedNativeID != "" }

const handoffOpenCols = `ticket_id, organization_id, handoff_id, runtime, checkout_root, launched_task, launched_native_id,
	state, cancel_reason, cancel_detail, claimed_runtime, claimed_native_id, claimed_transcript, brief_text, brief_due,
	brief_requested_at, brief_confirmed_at, brief_given_up_at, created_at, claimed_at, ended_at`

func scanHandoffOpen(row rowScanner) (HandoffOpen, error) {
	var t HandoffOpen
	err := row.Scan(&t.TicketID, &t.OrganizationID, &t.HandoffID, &t.Runtime, &t.CheckoutRoot, &t.LaunchedTask, &t.LaunchedNativeID,
		&t.State, &t.CancelReason, &t.CancelDetail, &t.ClaimedRuntime, &t.ClaimedNativeID, &t.ClaimedTranscript, &t.BriefText, &t.BriefDue,
		&t.BriefRequestedAt, &t.BriefConfirmedAt, &t.BriefGivenUpAt, &t.CreatedAt, &t.ClaimedAt, &t.EndedAt)
	return t, err
}

func handoffOpenTx(q queryRower, ticketID string) (HandoffOpen, bool, error) {
	t, err := scanHandoffOpen(q.QueryRow(`SELECT `+handoffOpenCols+` FROM handoff_open WHERE ticket_id=?`, ticketID))
	if err == sql.ErrNoRows {
		return HandoffOpen{}, false, nil
	}
	return t, err == nil, err
}

// HandoffOpenByTicket reads one ticket.
func (ix *Index) HandoffOpenByTicket(ticketID string) (HandoffOpen, bool, error) {
	return handoffOpenTx(ix.db, ticketID)
}

type rowsQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func listHandoffOpens(q rowsQuerier, where string, args ...any) ([]HandoffOpen, error) {
	rows, err := q.Query(`SELECT `+handoffOpenCols+` FROM handoff_open `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HandoffOpen{}
	for rows.Next() {
		t, err := scanHandoffOpen(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HandoffOpensFor lists a handoff's tickets on this device, newest first.
func (ix *Index) HandoffOpensFor(organizationID, handoffID string) ([]HandoffOpen, error) {
	return listHandoffOpens(ix.db, `WHERE organization_id=? AND handoff_id=? ORDER BY created_at DESC, ticket_id DESC`, organizationID, handoffID)
}

// HandoffOpensLaunched lists the tickets whose launched turn has not been seen to end:
// the ones whose task the open owner still watches for a session frame and an end.
func (ix *Index) HandoffOpensLaunched() ([]HandoffOpen, error) {
	return listHandoffOpens(ix.db, `WHERE launched_task != '' AND launch_settled_at=0 ORDER BY created_at`)
}

// HandoffOpenByTask reads the ticket a console task was launched for.
func (ix *Index) HandoffOpenByTask(taskID string) (HandoffOpen, bool, error) {
	if taskID == "" {
		return HandoffOpen{}, false, nil
	}
	t, err := scanHandoffOpen(ix.db.QueryRow(`SELECT `+handoffOpenCols+` FROM handoff_open WHERE launched_task=? LIMIT 1`, taskID))
	if err == sql.ErrNoRows {
		return HandoffOpen{}, false, nil
	}
	return t, err == nil, err
}

// HandoffOpenRefusal is §6.3's "Open is offered on any non-terminal item, on any of
// the recipient's devices", as a check on the row: nil when this device may open it. A
// row of an ended link stays openable — locally, with nothing sent (criterion 68).
func HandoffOpenRefusal(h Handoff) *HandoffRefusal {
	refuse := func(code string) *HandoffRefusal { return &HandoffRefusal{Code: code, State: h.State} }
	if teamwire.HandoffTerminal(h.State) {
		return refuse(teamwire.HandoffTerminalCode(h.State))
	}
	// A self-send's originating device lists the item under Sent only and has no Open.
	if !h.ToMe || (h.SentHere && !h.Local()) {
		return refuse(HandoffCodeNotOffered)
	}
	if h.State == HandoffQueued || h.State == HandoffRefused {
		return refuse(HandoffCodeWrongState)
	}
	if !h.HasBody && h.WireBody == "" {
		return refuse(HandoffCodeNoText)
	}
	return nil
}

// HandoffOpenCreate is one console Open: the handoff, the runtime the person chose and
// the folder the session will start in.
type HandoffOpenCreate struct {
	OrganizationID string
	HandoffID      string
	Runtime        string
	CheckoutRoot   string
	At             int64
}

// CreateHandoffOpen writes a waiting ticket (§6.7 "open"). The row's offer is checked
// inside the transaction, so an Open racing a landed withdrawal writes nothing; the
// refusal is a *HandoffRefusal.
func (ix *Index) CreateHandoffOpen(in HandoffOpenCreate) (HandoffOpen, error) {
	if in.Runtime == "" || in.HandoffID == "" {
		return HandoffOpen{}, fmt.Errorf("an open names a handoff and a runtime")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return HandoffOpen{}, err
	}
	defer func() { _ = tx.Rollback() }()
	h, found, err := handoffTx(tx, in.OrganizationID, in.HandoffID)
	if err != nil {
		return HandoffOpen{}, err
	}
	if !found {
		return HandoffOpen{}, ErrHandoffNotFound
	}
	if refusal := HandoffOpenRefusal(h); refusal != nil {
		return HandoffOpen{}, refusal
	}
	ticketID := engine.NewTypedID(teamwire.TicketIDPrefix)
	if _, err := tx.Exec(`INSERT INTO handoff_open(ticket_id, organization_id, handoff_id, runtime, checkout_root, state, created_at)
		VALUES(?,?,?,?,?,?,?)`, ticketID, in.OrganizationID, in.HandoffID, in.Runtime, in.CheckoutRoot, HandoffOpenWaiting, in.At); err != nil {
		return HandoffOpen{}, err
	}
	t, _, err := handoffOpenTx(tx, ticketID)
	if err != nil {
		return HandoffOpen{}, err
	}
	return t, tx.Commit()
}

// cancelWaitingTicketTx cancels one waiting ticket. A claimed or already cancelled
// ticket is left as it is.
func cancelWaitingTicketTx(tx *sql.Tx, ticketID, reason, detail string, at int64) (bool, error) {
	result, err := tx.Exec(`UPDATE handoff_open SET state=?, cancel_reason=?, cancel_detail=?, ended_at=? WHERE ticket_id=? AND state=?`,
		HandoffOpenCancelled, reason, detail, at, ticketID, HandoffOpenWaiting)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// CancelHandoffOpen cancels a ticket that is still waiting and was never launched: its
// composer was closed without sending. It reports whether it cancelled one.
func (ix *Index) CancelHandoffOpen(ticketID string, at int64) (bool, error) {
	result, err := ix.db.Exec(`UPDATE handoff_open SET state=?, cancel_reason=?, ended_at=? WHERE ticket_id=? AND state=? AND launched_task=''`,
		HandoffOpenCancelled, HandoffOpenComposerClosed, at, ticketID, HandoffOpenWaiting)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// LaunchHandoffOpen admits the first turn of an Open and records the console task that
// carries it, in one transaction, before that task's process starts — so the task's
// session frame can never arrive for a ticket that does not name the task yet. The
// ticket must be waiting, not yet launched, for this runtime, and its handoff must not
// have ended. The refusal is a *HandoffRefusal: a ticket a withdrawal cancelled is
// refused with the withdrawal's code, which the caller words "the sender withdrew this
// handoff" (§6.7).
func (ix *Index) LaunchHandoffOpen(ticketID, runtime, taskID string) (HandoffOpen, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return HandoffOpen{}, err
	}
	defer func() { _ = tx.Rollback() }()
	t, found, err := handoffOpenTx(tx, ticketID)
	if err != nil {
		return HandoffOpen{}, err
	}
	if !found {
		return HandoffOpen{}, &HandoffRefusal{Code: HandoffCodeTicketNotFound}
	}
	if refusal, err := launchRefusalTx(tx, t, runtime); err != nil || refusal != nil {
		if err != nil {
			return HandoffOpen{}, err
		}
		return t, refusal
	}
	if _, err := tx.Exec(`UPDATE handoff_open SET launched_task=? WHERE ticket_id=?`, taskID, ticketID); err != nil {
		return HandoffOpen{}, err
	}
	t.LaunchedTask = taskID
	return t, tx.Commit()
}

// HandoffOpenLaunchRefusal is LaunchHandoffOpen's check without the write: what a send
// with this ticket would be refused with now, or nil. A caller that has not minted the
// task yet asks this first; the authoritative check is the launch's own.
func (ix *Index) HandoffOpenLaunchRefusal(ticketID, runtime string) (*HandoffRefusal, error) {
	t, found, err := handoffOpenTx(ix.db, ticketID)
	if err != nil {
		return nil, err
	}
	if !found {
		return &HandoffRefusal{Code: HandoffCodeTicketNotFound}, nil
	}
	return launchRefusalTx(ix.db, t, runtime)
}

func launchRefusalTx(q queryRower, t HandoffOpen, runtime string) (*HandoffRefusal, error) {
	h, found, err := handoffTx(q, t.OrganizationID, t.HandoffID)
	if err != nil {
		return nil, err
	}
	switch {
	case !found:
		return &HandoffRefusal{Code: HandoffCodeTicketNotFound}, nil
	case teamwire.HandoffTerminal(h.State):
		return &HandoffRefusal{Code: teamwire.HandoffTerminalCode(h.State), State: h.State}, nil
	case t.State == HandoffOpenCancelled:
		return &HandoffRefusal{Code: HandoffCodeTicketCancelled, State: h.State}, nil
	case t.State != HandoffOpenWaiting || t.LaunchedTask != "":
		return &HandoffRefusal{Code: HandoffCodeTicketUsed, State: h.State}, nil
	case t.Runtime != runtime:
		return &HandoffRefusal{Code: HandoffCodeTicketRuntime, State: h.State}, nil
	}
	return nil, nil
}

// HandoffBriefPolicy is the session_delivery table's own policy, passed in by the
// caller that reads it from configuration: how long a pending row lives and how many
// pending rows one session may hold.
type HandoffBriefPolicy struct {
	TTLSeconds int64
	MaxPending int
}

// armHandoffBriefTx writes one pending brief row for the ticket's claimed session. It
// reports false, writing nothing, when the session already holds the pending cap (the
// sweep arms it later); a ticket that already has a pending row is armed and gets no
// second one. The row leaves the
// catalog session id empty, so it matches by native id only (§6.5).
func armHandoffBriefTx(tx *sql.Tx, t HandoffOpen, now int64, policy HandoffBriefPolicy) (bool, error) {
	if t.ClaimedNativeID == "" || t.BriefText == "" {
		return false, nil
	}
	if policy.TTLSeconds <= 0 {
		return false, fmt.Errorf("handoff brief: a positive delivery lifetime is required (delivery.ttl_seconds)")
	}
	runID := HandoffDeliveryRun(t.TicketID)
	var own, pending, rows int
	if err := tx.QueryRow(`SELECT count(*) FROM session_delivery WHERE run_id=? AND state='pending' AND expires_at>?`, runID, now).Scan(&own); err != nil {
		return false, err
	}
	if own > 0 {
		return true, nil // already armed: one pending row per ticket
	}
	if err := tx.QueryRow(`SELECT count(*) FROM session_delivery WHERE state='pending' AND runtime=? AND native_session_id=?`,
		t.ClaimedRuntime, t.ClaimedNativeID).Scan(&pending); err != nil {
		return false, err
	}
	if policy.MaxPending > 0 && pending >= policy.MaxPending {
		return false, nil
	}
	if err := tx.QueryRow(`SELECT count(*) FROM session_delivery WHERE run_id=?`, runID).Scan(&rows); err != nil {
		return false, err
	}
	deliveryID := fmt.Sprintf("hbr_%s_%04d", t.TicketID, rows+1)
	_, err := tx.Exec(`INSERT INTO session_delivery(`+sessionDeliveryCols+`) VALUES(?,?,?,?,'',?,?,'pending',?,?,0,'','','','')`,
		deliveryID, runID, t.ClaimedRuntime, t.ClaimedNativeID, t.BriefText, handoffBriefBoundary, now, now+policy.TTLSeconds)
	return err == nil, err
}

// cancelHandoffBriefRowsTx ends a ticket's pending brief rows: the table's expired
// state with a named detail. A row already handed over is left as the evidence it is.
func cancelHandoffBriefRowsTx(tx *sql.Tx, ticketID, detail string) error {
	_, err := tx.Exec(`UPDATE session_delivery SET state='expired', detail=? WHERE run_id=? AND state='pending'`,
		detail, HandoffDeliveryRun(ticketID))
	return err
}

// handoffBriefReleaseEndTx answers, for a delivery row whose reply never reached the
// hook, whether it may go back to pending. A row that is not a handoff's brief may
// ("" is returned). A brief may only while its ticket is still claimed by the session
// the row names, the device has not given up on it, and its handoff is not terminal;
// otherwise the detail the row ends with is returned. The cancellations in this file
// end pending rows only, so this is where a row that was handed over at that moment
// meets them.
func handoffBriefReleaseEndTx(tx *sql.Tx, deliveryID string) (string, error) {
	var runID, runtime, nativeID string
	err := tx.QueryRow(`SELECT run_id, runtime, native_session_id FROM session_delivery WHERE delivery_id=?`, deliveryID).Scan(&runID, &runtime, &nativeID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	ticketID, isBrief := HandoffTicketForDeliveryRun(runID)
	if !isBrief {
		return "", nil
	}
	t, found, err := handoffOpenTx(tx, ticketID)
	if err != nil {
		return "", err
	}
	switch {
	case !found:
		return HandoffBriefTicketCancelled, nil
	case t.State != HandoffOpenClaimed && t.CancelReason == teamwire.HandoffTerminalCode(teamwire.HandoffWithdrawn):
		return HandoffBriefWithdrawn, nil
	case t.State != HandoffOpenClaimed:
		return HandoffBriefTicketCancelled, nil
	case t.ClaimedRuntime != runtime || t.ClaimedNativeID != nativeID:
		return HandoffBriefClaimMoved, nil
	case t.BriefGivenUpAt != 0:
		return HandoffBriefWaitElapsed, nil
	}
	h, found, err := handoffTx(tx, t.OrganizationID, t.HandoffID)
	if err != nil {
		return "", err
	}
	if !found || teamwire.HandoffTerminal(h.State) {
		return HandoffBriefTicketCancelled, nil
	}
	return "", nil
}

// claimedSessionIdentity is the claiming session as a receipt carries it: the runtime,
// the native id, and the wire session id derived from them and this device. The
// catalog and resume ids are not known at claim time; the server attaches them from
// the pushed session record (§6.2).
func claimedSessionIdentity(tx *sql.Tx, runtime, nativeID string) (*teamwire.SessionIdentity, error) {
	var deviceID string
	if err := tx.QueryRow(`SELECT id FROM sync_device LIMIT 1`).Scan(&deviceID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	wireRuntime, wireNative := engine.WireSessionParts(runtime, nativeID)
	return &teamwire.SessionIdentity{ID: engine.WireSessionID(deviceID, wireRuntime+"/"+wireNative), Runtime: wireRuntime, NativeID: wireNative}, nil
}

// enqueueOpenReceiptTx enqueues a started or opened receipt for a ticket's claimed
// session. Nothing is sent for a local handoff, for a row of an ended link, or when
// the device is linked elsewhere now (criterion 68).
func (ix *Index) enqueueOpenReceiptTx(tx *sql.Tx, h Handoff, t HandoffOpen, transition string, at int64) (bool, error) {
	if h.Local() || h.LinkEnded {
		return false, nil
	}
	linked, err := linkedOrganizationTx(tx)
	if err != nil || linked != h.OrganizationID {
		return false, err
	}
	session, err := claimedSessionIdentity(tx, t.ClaimedRuntime, t.ClaimedNativeID)
	if err != nil {
		return false, err
	}
	return ix.EnqueueHandoffReceiptTx(tx, HandoffReceiptInput{OrganizationID: h.OrganizationID, HandoffID: h.ID,
		Transition: transition, TicketID: t.TicketID, Session: session, At: at})
}

// HandoffClaim is one session entry that carried a ticket id (§6.4 rule 1).
type HandoffClaim struct {
	TicketID   string
	Runtime    string
	NativeID   string
	Transcript string
	// BriefText is the brief to arm for the session, built by the caller from the
	// handoff's document within handoff.inject_max_bytes.
	BriefText string
	At        int64
	Policy    HandoffBriefPolicy
}

// The outcomes of a claim attempt, as data codes.
const (
	HandoffClaimClaimed        = "claimed"
	HandoffClaimDuplicate      = "duplicate"       // the same session again: nothing changes (rule 2)
	HandoffClaimAlreadyClaimed = "already_claimed" // another session holds it: never claimed again (rule 2)
	HandoffClaimNotLaunched    = "not_the_launched_session"
	HandoffClaimUnknownTicket  = "unknown_ticket"
	HandoffClaimWrongRuntime   = "wrong_runtime"
	HandoffClaimEnded          = "ticket_ended"
)

// HandoffClaimResult says what a claim attempt did. Ids and codes only.
type HandoffClaimResult struct {
	Outcome        string
	OrganizationID string
	HandoffID      string
	BriefArmed     bool
	Receipt        bool
}

// ClaimHandoffTicketTx claims a ticket for the session whose entry carried its id, in
// the caller's transaction — the session-entry ingest's first one, before the
// checkpoint capture (§6.4 rule 6): the ticket's claim, the handoff's local state, the
// armed brief and the started receipt's outbox row commit together or not at all.
//
// It is exact (only this call claims), idempotent (the same session again changes
// nothing; a claimed ticket is never claimed again), and it refuses any session other
// than the launched task's once that task's first session frame is known (K-4). A claim
// that arrives before the frame is accepted and corrected by RecordHandoffOpenFrame.
// A ticket cancelled because its launched session's hook was thought not to have run
// is claimable by that same session: its entry was spooled and replayed late.
func (ix *Index) ClaimHandoffTicketTx(g *GovTx, in HandoffClaim) (HandoffClaimResult, error) {
	tx := g.tx
	t, found, err := handoffOpenTx(tx, in.TicketID)
	if err != nil {
		return HandoffClaimResult{}, err
	}
	if !found {
		return HandoffClaimResult{Outcome: HandoffClaimUnknownTicket}, nil
	}
	out := HandoffClaimResult{OrganizationID: t.OrganizationID, HandoffID: t.HandoffID}
	lateReplay := t.State == HandoffOpenCancelled && t.CancelReason == HandoffOpenHookDidNotRun && t.LaunchedNativeID == in.NativeID
	switch {
	case t.Runtime != in.Runtime:
		out.Outcome = HandoffClaimWrongRuntime
	case t.Claimed() && t.ClaimedRuntime == in.Runtime && t.ClaimedNativeID == in.NativeID:
		out.Outcome = HandoffClaimDuplicate
	case t.Claimed():
		out.Outcome = HandoffClaimAlreadyClaimed
	case t.State != HandoffOpenWaiting && !lateReplay:
		out.Outcome = HandoffClaimEnded
	case t.LaunchedNativeID != "" && t.LaunchedNativeID != in.NativeID:
		out.Outcome = HandoffClaimNotLaunched
	}
	if out.Outcome != "" {
		return out, nil
	}
	h, found, err := handoffTx(tx, t.OrganizationID, t.HandoffID)
	if err != nil {
		return HandoffClaimResult{}, err
	}
	if !found || teamwire.HandoffTerminal(h.State) {
		out.Outcome = HandoffClaimEnded
		return out, nil
	}
	if _, err := tx.Exec(`UPDATE handoff_open SET state=?, cancel_reason='', cancel_detail='', ended_at=0, claimed_runtime=?, claimed_native_id=?,
		claimed_transcript=?, brief_text=?, brief_due=1, brief_requested_at=?, claimed_at=? WHERE ticket_id=?`,
		HandoffOpenClaimed, in.Runtime, in.NativeID, in.Transcript, in.BriefText, in.At, in.At, t.TicketID); err != nil {
		return HandoffClaimResult{}, err
	}
	if lateReplay {
		// The hook did run; the mark that said otherwise is this ticket's own.
		if _, err := tx.Exec(`DELETE FROM handoff_runtime_readiness WHERE runtime=? AND detail=?`, t.Runtime, t.TicketID); err != nil {
			return HandoffClaimResult{}, err
		}
	}
	t.State, t.ClaimedRuntime, t.ClaimedNativeID, t.BriefText = HandoffOpenClaimed, in.Runtime, in.NativeID, in.BriefText
	if err := markHandoffStartedTx(tx, h, in.At); err != nil {
		return HandoffClaimResult{}, err
	}
	if out.BriefArmed, err = armHandoffBriefTx(tx, t, in.At, in.Policy); err != nil {
		return HandoffClaimResult{}, err
	}
	if out.Receipt, err = ix.enqueueOpenReceiptTx(tx, h, t, teamwire.TransitionStarted, in.At); err != nil {
		return HandoffClaimResult{}, err
	}
	out.Outcome = HandoffClaimClaimed
	return out, nil
}

// markHandoffStartedTx moves the handoff's local state to started when a session
// claimed a ticket for it. An item already opened here stays opened.
func markHandoffStartedTx(tx *sql.Tx, h Handoff, at int64) error {
	_, err := tx.Exec(`UPDATE handoff SET state=?, state_at=?, updated_at=? WHERE organization_id=? AND id=? AND state IN (?,?)`,
		teamwire.HandoffStarted, at, at, h.OrganizationID, h.ID, teamwire.HandoffSent, teamwire.HandoffReceived)
	return err
}

// RearmHandoffBriefsForSessionTx arms the brief once more for every ticket the session
// holds, after a compaction of it (§6.4 rule 5): found by (runtime, native id), since
// such an entry carries no ticket, and only while the handoff is not terminal. A clear
// or a fork is a new id and reaches no ticket. It reports how many it asked for.
func (ix *Index) RearmHandoffBriefsForSessionTx(g *GovTx, runtime, nativeID string, at int64, policy HandoffBriefPolicy) (int, error) {
	tickets, err := listHandoffOpens(g.tx, `WHERE state=? AND claimed_runtime=? AND claimed_native_id=?`, HandoffOpenClaimed, runtime, nativeID)
	if err != nil {
		return 0, err
	}
	asked := 0
	for _, t := range tickets {
		if _, refusal, err := ix.requestBriefTx(g.tx, t, at, policy); err != nil {
			return asked, err
		} else if refusal == nil {
			asked++
		}
	}
	return asked, nil
}

// HasClaimedHandoffOpen reports whether the session holds a claimed ticket: the cheap
// read the session-entry ingest makes before it opens a brief re-arm.
func (ix *Index) HasClaimedHandoffOpen(runtime, nativeID string) (bool, error) {
	var n int
	err := ix.db.QueryRow(`SELECT count(*) FROM handoff_open WHERE state=? AND claimed_runtime=? AND claimed_native_id=?`,
		HandoffOpenClaimed, runtime, nativeID).Scan(&n)
	return n > 0, err
}

// requestBriefTx marks a claimed ticket's brief as owed again and arms it when the
// session has room: the shared step of a compaction re-delivery and Deliver again.
func (ix *Index) requestBriefTx(tx *sql.Tx, t HandoffOpen, at int64, policy HandoffBriefPolicy) (bool, *HandoffRefusal, error) {
	h, found, err := handoffTx(tx, t.OrganizationID, t.HandoffID)
	if err != nil {
		return false, nil, err
	}
	switch {
	case !found:
		return false, &HandoffRefusal{Code: HandoffCodeTicketNotFound}, nil
	case teamwire.HandoffTerminal(h.State):
		return false, &HandoffRefusal{Code: teamwire.HandoffTerminalCode(h.State), State: h.State}, nil
	case t.State != HandoffOpenClaimed:
		return false, &HandoffRefusal{Code: HandoffCodeNotClaimed, State: h.State}, nil
	}
	if _, err := tx.Exec(`UPDATE handoff_open SET brief_due=1, brief_requested_at=?, brief_given_up_at=0 WHERE ticket_id=?`, at, t.TicketID); err != nil {
		return false, nil, err
	}
	armed, err := armHandoffBriefTx(tx, t, at, policy)
	return armed, nil, err
}

// DeliverHandoffBriefAgain re-arms the brief for a ticket's claimed session (§6.5
// "Deliver again"): offered on every claimed, non-terminal item, started or opened. It
// never produces a second opened receipt — that is the confirmation's rule. armed is
// false when the session holds the pending cap; the sweep arms it then. The refusal is
// a *HandoffRefusal: a withdrawn handoff is refused with the withdrawal's code.
func (ix *Index) DeliverHandoffBriefAgain(ticketID string, at int64, policy HandoffBriefPolicy) (armed bool, err error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	t, found, err := handoffOpenTx(tx, ticketID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, &HandoffRefusal{Code: HandoffCodeTicketNotFound}
	}
	armed, refusal, err := ix.requestBriefTx(tx, t, at, policy)
	if err != nil {
		return false, err
	}
	if refusal != nil {
		return false, refusal
	}
	return armed, tx.Commit()
}

// HandoffBriefConfirmation says what a confirmation did.
type HandoffBriefConfirmation struct {
	OrganizationID string
	HandoffID      string
	// First is true for the ticket's first confirmation: the one that marks the
	// handoff opened here. Receipt says its opened receipt was enqueued.
	First   bool
	Receipt bool
}

// ConfirmHandoffBrief records that the reply carrying a ticket's brief was written to
// the hook without error (§6.5 step 3). It is its own transaction, after the reply: the
// brief is no longer owed; and only when the ticket's brief_confirmed_at was empty it
// is set, the handoff is marked opened on this device — so a later withdrawal does not
// erase a copy a session already holds — and the ONE opened receipt is enqueued. A
// second confirmation, after a compaction re-delivery or Deliver again, enqueues none.
//
// runtime and nativeID name the session the delivered row was handed to. Only the
// session that holds the ticket's claim confirms it: a row handed to a session the
// claim has since moved away from (§6.4 rule 3) says nothing about whether the
// launched session received the brief, and confirms nothing.
func (ix *Index) ConfirmHandoffBrief(ticketID, runtime, nativeID string, at int64) (HandoffBriefConfirmation, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return HandoffBriefConfirmation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	t, found, err := handoffOpenTx(tx, ticketID)
	if err != nil || !found || t.ClaimedRuntime != runtime || t.ClaimedNativeID != nativeID {
		return HandoffBriefConfirmation{}, err
	}
	out := HandoffBriefConfirmation{OrganizationID: t.OrganizationID, HandoffID: t.HandoffID}
	if _, err := tx.Exec(`UPDATE handoff_open SET brief_due=0, brief_given_up_at=0 WHERE ticket_id=?`, ticketID); err != nil {
		return HandoffBriefConfirmation{}, err
	}
	if t.BriefConfirmedAt == 0 {
		if out, err = ix.firstBriefConfirmationTx(tx, t, at, out); err != nil {
			return HandoffBriefConfirmation{}, err
		}
	}
	return out, tx.Commit()
}

// firstBriefConfirmationTx is the once-only half of a confirmation.
func (ix *Index) firstBriefConfirmationTx(tx *sql.Tx, t HandoffOpen, at int64, out HandoffBriefConfirmation) (HandoffBriefConfirmation, error) {
	if _, err := tx.Exec(`UPDATE handoff_open SET brief_confirmed_at=? WHERE ticket_id=? AND brief_confirmed_at=0`, at, t.TicketID); err != nil {
		return out, err
	}
	out.First = true
	h, found, err := handoffTx(tx, t.OrganizationID, t.HandoffID)
	if err != nil || !found || teamwire.HandoffTerminal(h.State) {
		// A handoff that ended between the hand-over and here sends no opened: the
		// server would reject it, and the session keeps what is in its context.
		return out, err
	}
	if _, err := tx.Exec(`UPDATE handoff SET state=?, state_at=?, updated_at=? WHERE organization_id=? AND id=? AND state IN (?,?,?)`,
		teamwire.HandoffOpened, at, at, h.OrganizationID, h.ID, teamwire.HandoffSent, teamwire.HandoffReceived, teamwire.HandoffStarted); err != nil {
		return out, err
	}
	out.Receipt, err = ix.enqueueOpenReceiptTx(tx, h, t, teamwire.TransitionOpened, at)
	return out, err
}

// HandoffOpenFrame says what recording a launched task's first session frame did.
type HandoffOpenFrame struct {
	Recorded bool // this was the ticket's first frame
	// Moved is true when a session other than the launched one held the claim: the
	// claim, the armed brief and a new started receipt now name the frame's session.
	Moved   bool
	Receipt bool
}

// RecordHandoffOpenFrame records the session the launched task's first session frame
// named, and checks a claim that arrived before it (§6.4 rule 3). If another session
// holds the claim — a process the opened session spawned, which inherited the ticket
// and posted its entry first — the ticket's claim, the armed brief and a NEW started
// receipt move to the frame's session; the brief already pending for the other session
// is cancelled, and the confirmation is cleared so the launched session's own first
// delivery is the one reported.
func (ix *Index) RecordHandoffOpenFrame(ticketID, nativeID string, at int64, policy HandoffBriefPolicy) (HandoffOpenFrame, error) {
	var out HandoffOpenFrame
	if nativeID == "" {
		return out, nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	t, found, err := handoffOpenTx(tx, ticketID)
	if err != nil || !found || t.LaunchedNativeID != "" {
		return out, err
	}
	if _, err := tx.Exec(`UPDATE handoff_open SET launched_native_id=? WHERE ticket_id=?`, nativeID, ticketID); err != nil {
		return out, err
	}
	out.Recorded = true
	if t.State != HandoffOpenClaimed || t.ClaimedNativeID == nativeID {
		return out, tx.Commit()
	}
	if err := cancelHandoffBriefRowsTx(tx, ticketID, HandoffBriefClaimMoved); err != nil {
		return out, err
	}
	if _, err := tx.Exec(`UPDATE handoff_open SET claimed_native_id=?, claimed_transcript='', brief_due=1, brief_requested_at=?,
		brief_confirmed_at=0, brief_given_up_at=0, claimed_at=? WHERE ticket_id=?`, nativeID, at, at, ticketID); err != nil {
		return out, err
	}
	t.ClaimedNativeID, out.Moved = nativeID, true
	if _, err := armHandoffBriefTx(tx, t, at, policy); err != nil {
		return out, err
	}
	h, found, err := handoffTx(tx, t.OrganizationID, t.HandoffID)
	if err != nil {
		return out, err
	}
	if found && !teamwire.HandoffTerminal(h.State) {
		if out.Receipt, err = ix.enqueueOpenReceiptTx(tx, h, t, teamwire.TransitionStarted, at); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

// EndHandoffOpenLaunch settles a ticket whose launched turn ended without a claim
// (§6.3): reason is HandoffOpenRuntimeNotStarted when the task never had a session
// frame, with the task's own reason as detail, or HandoffOpenHookDidNotRun when a
// session started and made no claim. The second records the fact for the runtime, so
// readiness then needs rows newer than it (K-7). A claimed ticket is left alone. Either
// way the launch is marked settled: its task has ended and is watched no longer. It
// reports whether it cancelled the ticket.
func (ix *Index) EndHandoffOpenLaunch(ticketID, reason, detail string, at int64) (bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	t, found, err := handoffOpenTx(tx, ticketID)
	if err != nil || !found {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE handoff_open SET launch_settled_at=? WHERE ticket_id=? AND launch_settled_at=0`, at, ticketID); err != nil {
		return false, err
	}
	cancelled, err := cancelWaitingTicketTx(tx, ticketID, reason, detail, at)
	if err != nil {
		return false, err
	}
	if !cancelled {
		return false, tx.Commit()
	}
	if reason == HandoffOpenHookDidNotRun {
		if _, err := tx.Exec(`INSERT INTO handoff_runtime_readiness(runtime, unclaimed_launch_at, detail) VALUES(?,?,?)
			ON CONFLICT(runtime) DO UPDATE SET unclaimed_launch_at=excluded.unclaimed_launch_at, detail=excluded.detail`,
			t.Runtime, at, ticketID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// cancelHandoffOpensTx is the one point where a handoff's open tickets and its pending
// brief rows are cancelled (§6.7 "land withdrawn" and "unlink"). The handoff owner
// calls it, inside its own transaction, from:
//
//   - landTerminalTx — the handoff reached a terminal state on this device. reason is
//     that state. Every waiting or claimed ticket for it is cancelled with the state's
//     wire code — a claimed one keeps the facts of its claim, which the item still
//     shows — and every pending brief row is cancelled, so a brief can no longer be
//     delivered after the state lands. A session already handed the brief keeps what
//     is in its context.
//   - endHandoffLinkTx — the link ended. handoffID is "" and reason is
//     HandoffCodeLinkEnded: every waiting ticket of a team handoff is cancelled and
//     never claimed later (criterion 68). A claimed ticket keeps its claim and its
//     brief: the session is on this device and what it needs is here; nothing is sent
//     for it.
func (ix *Index) cancelHandoffOpensTx(tx *sql.Tx, organizationID, handoffID, reason string) error {
	if handoffID == "" {
		_, err := tx.Exec(`UPDATE handoff_open SET state=?, cancel_reason=?, ended_at=CAST(strftime('%s','now') AS INTEGER)
			WHERE organization_id != '' AND state=?`, HandoffOpenCancelled, reason, HandoffOpenWaiting)
		return err
	}
	code, detail := teamwire.HandoffTerminalCode(reason), HandoffBriefTicketCancelled
	if reason == teamwire.HandoffWithdrawn {
		detail = HandoffBriefWithdrawn
	}
	tickets, err := listHandoffOpens(tx, `WHERE organization_id=? AND handoff_id=? AND state IN (?,?)`,
		organizationID, handoffID, HandoffOpenWaiting, HandoffOpenClaimed)
	if err != nil {
		return err
	}
	for _, t := range tickets {
		if err := cancelHandoffBriefRowsTx(tx, t.TicketID, detail); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE handoff_open SET state=?, cancel_reason=?, brief_due=0, ended_at=CAST(strftime('%s','now') AS INTEGER)
			WHERE ticket_id=?`, HandoffOpenCancelled, code, t.TicketID); err != nil {
			return err
		}
	}
	return nil
}

// HandoffBriefSweep is one pass of the re-arm sweep (§6.5 "re-arm", "give up").
type HandoffBriefSweep struct {
	Now    int64
	Policy HandoffBriefPolicy
	// BriefWaitSeconds is handoff.brief_wait: how long after it was last asked for a
	// brief is re-armed before the device gives up on it.
	BriefWaitSeconds int64
	// HandedOverGraceSeconds is how long a handed-over row may sit without a
	// confirmation before it is taken for lost: the confirmation is a separate
	// transaction after the reply, so a row handed over a moment ago is not lost.
	// Zero at start-up, when no reply of this process can be in flight.
	HandedOverGraceSeconds int64
}

// HandoffBriefSweepEffects counts what one pass did.
type HandoffBriefSweepEffects struct {
	Armed   int // a new pending row replaced a lost, expired or missing one
	Expired int // overdue pending rows this pass expired itself
	GaveUp  int // tickets whose wait ran out
	Waiting int // tickets left for the next pass (the session holds the pending cap)
}

// SweepHandoffBriefs is the brief's re-arm sweep, run by the handoff owner on the
// lifecycle coordinator's sweep — not the managed host's, so it runs with no agent
// turned on. For every claimed ticket whose brief is owed and whose handoff is not
// terminal: a row handed over without a confirmation (the daemon died between the
// reply and the confirmation), an expired row, a pending row past its lifetime that no
// other owner expired, or no row at all (the session held the pending cap) is replaced
// by a new pending row with the same text. The brief may be delivered twice; it is
// never dropped. Past the wait, re-arming stops, the pending row is cancelled, and
// nothing is sent as opened.
func (ix *Index) SweepHandoffBriefs(in HandoffBriefSweep) (HandoffBriefSweepEffects, error) {
	var eff HandoffBriefSweepEffects
	// The candidates are read first, outside any transaction: on a device with no
	// brief owed — almost always — a sweep takes no write lock at all.
	const owed = `WHERE state=? AND brief_due=1 AND brief_given_up_at=0 ORDER BY claimed_at`
	if candidates, err := listHandoffOpens(ix.db, owed, HandoffOpenClaimed); err != nil || len(candidates) == 0 {
		return eff, err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return eff, err
	}
	defer func() { _ = tx.Rollback() }()
	tickets, err := listHandoffOpens(tx, owed, HandoffOpenClaimed)
	if err != nil {
		return eff, err
	}
	for _, t := range tickets {
		if err := ix.sweepHandoffBriefTx(tx, t, in, &eff); err != nil {
			return HandoffBriefSweepEffects{}, fmt.Errorf("handoff brief %s: %w", t.TicketID, err)
		}
	}
	if eff == (HandoffBriefSweepEffects{}) {
		return eff, nil
	}
	return eff, tx.Commit()
}

func (ix *Index) sweepHandoffBriefTx(tx *sql.Tx, t HandoffOpen, in HandoffBriefSweep, eff *HandoffBriefSweepEffects) error {
	h, found, err := handoffTx(tx, t.OrganizationID, t.HandoffID)
	if err != nil {
		return err
	}
	if !found || teamwire.HandoffTerminal(h.State) {
		return nil // the terminal landing cancelled it; nothing is re-armed
	}
	if in.BriefWaitSeconds > 0 && in.Now-t.BriefRequestedAt >= in.BriefWaitSeconds {
		if err := cancelHandoffBriefRowsTx(tx, t.TicketID, HandoffBriefWaitElapsed); err != nil {
			return err
		}
		eff.GaveUp++
		_, err := tx.Exec(`UPDATE handoff_open SET brief_given_up_at=? WHERE ticket_id=?`, in.Now, t.TicketID)
		return err
	}
	var deliveryID, state string
	var expiresAt, deliveredAt int64
	err = tx.QueryRow(`SELECT delivery_id, state, expires_at, delivered_at FROM session_delivery WHERE run_id=?
		ORDER BY created_at DESC, delivery_id DESC LIMIT 1`, HandoffDeliveryRun(t.TicketID)).Scan(&deliveryID, &state, &expiresAt, &deliveredAt)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return err
	case state == "pending" && expiresAt > in.Now:
		return nil // armed and waiting for the session's next prompt
	case state == "pending":
		// Past its lifetime and still pending: the managed host's expiry did not run
		// (plan §17.2). The handoff owner expires its own rows.
		if _, err := tx.Exec(`UPDATE session_delivery SET state='expired', detail=? WHERE delivery_id=? AND state='pending'`,
			handoffBriefOverdue, deliveryID); err != nil {
			return err
		}
		eff.Expired++
	case state == "delivered" && deliveredAt > in.Now-in.HandedOverGraceSeconds:
		return nil // handed over a moment ago; its confirmation may still be on its way
	}
	armed, err := armHandoffBriefTx(tx, t, in.Now, in.Policy)
	if err != nil {
		return err
	}
	if armed {
		eff.Armed++
	} else {
		eff.Waiting++
	}
	return nil
}

// HandoffOpenForSession finds the ticket a session claimed, with its handoff — the
// lookup behind get_handoff and behind the opened session's "continues … from …" facts.
// The newest claim wins when one session claimed more than one ticket, which only a
// test can arrange. Matching is by (runtime, native id) and nothing else.
func (ix *Index) HandoffOpenForSession(runtime, nativeID string) (HandoffOpen, Handoff, bool, error) {
	if runtime == "" || nativeID == "" {
		return HandoffOpen{}, Handoff{}, false, nil
	}
	t, err := scanHandoffOpen(ix.db.QueryRow(`SELECT `+handoffOpenCols+` FROM handoff_open
		WHERE claimed_runtime=? AND claimed_native_id=? ORDER BY claimed_at DESC, ticket_id DESC LIMIT 1`, runtime, nativeID))
	if err == sql.ErrNoRows {
		return HandoffOpen{}, Handoff{}, false, nil
	}
	if err != nil {
		return HandoffOpen{}, Handoff{}, false, err
	}
	h, found, err := ix.HandoffByID(t.OrganizationID, t.HandoffID)
	return t, h, found, err
}

// HandoffRuntimeFiring is what this device has observed of one runtime's lifecycle
// hook (§6.3 "When Open is offered"): the newest session-entry row the hook posted and
// the newest row of the prompt kind, and the last launched turn that started a session
// and made no claim. Zero means none was observed inside the window asked for.
type HandoffRuntimeFiring struct {
	Runtime           string
	SessionEntryAt    int64
	PromptAt          int64
	UnclaimedLaunchAt int64
}

// HandoffRuntimeFiring reads the firing evidence for one runtime, no older than since.
// promptKind is the observation kind the brief rides, in the framework's vocabulary.
// A session entry counts only when the hook posted it at session start (directly or
// replayed from its spool): never a row the transcript reader inferred, and never the
// "first-action" row a tool call stands in with when no start was seen — that one
// proves a tool hook fired, not the session-start hook.
func (ix *Index) HandoffRuntimeFiring(runtime, promptKind string, since int64) (HandoffRuntimeFiring, error) {
	out := HandoffRuntimeFiring{Runtime: runtime}
	if err := ix.db.QueryRow(`SELECT COALESCE(MAX(observed_at),0) FROM session_activity_observation
		WHERE runtime=? AND state='open' AND entry_kind != 'first-action' AND delivery_mode IN ('direct','replay') AND observed_at>=?`, runtime, since).Scan(&out.SessionEntryAt); err != nil {
		return out, err
	}
	var promptMS int64
	if err := ix.db.QueryRow(`SELECT COALESCE(MAX(received_at_ms),0) FROM session_turn_observation
		WHERE runtime=? AND kind=? AND received_at_ms>=?`, runtime, promptKind, since*1000).Scan(&promptMS); err != nil {
		return out, err
	}
	out.PromptAt = promptMS / 1000
	err := ix.db.QueryRow(`SELECT unclaimed_launch_at FROM handoff_runtime_readiness WHERE runtime=?`, runtime).Scan(&out.UnclaimedLaunchAt)
	if err == sql.ErrNoRows {
		err = nil
	}
	return out, err
}

// HandoffCheckoutRoots lists the checkout roots this device's sessions resolved to a
// repository id, newest first: the folders an Open of a handoff for that repository is
// offered (§6.3).
func (ix *Index) HandoffCheckoutRoots(repositoryID string, limit int) ([]string, error) {
	if repositoryID == "" {
		return []string{}, nil
	}
	if limit <= 0 {
		return nil, fmt.Errorf("handoff checkout roots: a positive limit is required (team.json identity_upgrade_roots)")
	}
	rows, err := ix.db.Query(`SELECT checkout_root FROM session_checkpoint WHERE repository_id=? AND checkout_root != ''
		GROUP BY checkout_root ORDER BY MAX(id) DESC LIMIT ?`, repositoryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		out = append(out, root)
	}
	return out, rows.Err()
}
