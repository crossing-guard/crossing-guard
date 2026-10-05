package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"crossing-guard/teamwire"
)

// Handoff between members, the device's rows (team rest-of-release plan §6.2, §6.7).
// This file is the handoff table's one owner: the send, the push answer, the local
// transitions (decline, close, withdraw), the receipt writer, and the reads. Landing a
// pulled page lives in handoff_land.go, the member directory in handoff_members.go, and
// the open ticket's cancellation seam in handoff_open.go.
//
// Every step is one transaction, and anything that must reach the server is an outbox
// row written in that same transaction with its wire body frozen, so a crash resends
// byte-identical bytes and the server answers duplicate.

// Outbox kinds for the two handoff records; the wire kinds share the spelling.
const (
	OutboxHandoff        = "handoff"
	OutboxHandoffReceipt = "handoff_receipt"
)

// Device-only states. The wire's states (teamwire.Handoff*) are the server's fold; these
// two exist only on the device that originated a handoff: queued until the server
// answers the push, refused when it answered with a refusal.
const (
	HandoffQueued  = "queued"
	HandoffRefused = "refused"
)

// Refusal codes the device writes itself. The server's codes are teamwire's.
const (
	// HandoffCodeLinkEnded is the refusal_code of a handoff that was still queued when
	// its link ended, and the refusal of an action on a row of an ended link.
	HandoffCodeLinkEnded = "link_ended"
	// HandoffCodeWrongState refuses an action the row's state does not offer (§6.2).
	HandoffCodeWrongState = "wrong_state"
	// HandoffCodeNotOffered refuses an action this device's part does not offer: a
	// decline of a handoff not addressed to this user, a withdraw of one it did not send.
	HandoffCodeNotOffered = "not_offered"
)

// ErrHandoffNotFound is returned for an id this device holds no row for.
var ErrHandoffNotFound = errors.New("no such handoff on this device")

// ErrNotLinked refuses a team send from a device that is not linked.
var ErrNotLinked = errors.New("this device is not linked to a team")

// HandoffRefusal is a local action the row does not offer. Code is a data code the
// caller shows; State is the row's state when it was refused.
type HandoffRefusal struct {
	Code  string
	State string
}

func (r *HandoffRefusal) Error() string {
	return fmt.Sprintf("the handoff does not offer this action (%s; state %s)", r.Code, r.State)
}

// Handoff is one device row. WireBody is loaded only by the single-row reads; a listing
// leaves it empty and reports HasBody.
type Handoff struct {
	OrganizationID string
	ID             string
	SentHere       bool
	ToMe           bool
	FromMe         bool
	PeerUserID     string
	PeerName       string
	SenderDeviceID string
	Source         teamwire.SessionIdentity
	RepositoryID   string
	Title          string
	WireBody       string
	HasBody        bool
	// Held says this device held the document at some time: false on a row that
	// landed already expired or withdrawn, and on a sender's other device.
	Held             bool
	WireHash         string
	State            string
	StateSeq         int64
	StateAt          int64
	RefusalCode      string
	ReceiptCode      string
	OpenedBy         *teamwire.HandoffOpenedBy
	OtherOpenedCount int
	HashConflicts    int
	LinkEnded        bool
	CreatedAt        int64
	UpdatedAt        int64
}

// Local reports a same-device handoff: no organization, no server, no receipt.
func (h Handoff) Local() bool { return h.OrganizationID == "" }

// Document decodes the frozen document. ok is false when this device holds no text for
// the row (a sender's other device, an expiry it never received, an erased copy).
func (h Handoff) Document() (teamwire.HandoffRecord, bool, error) {
	if h.WireBody == "" {
		return teamwire.HandoffRecord{}, false, nil
	}
	var rec teamwire.HandoffRecord
	if err := json.Unmarshal([]byte(h.WireBody), &rec); err != nil {
		return teamwire.HandoffRecord{}, false, fmt.Errorf("handoff %s: stored document: %w", h.ID, err)
	}
	return rec, true, nil
}

const handoffCols = `organization_id, id, sent_here, to_me, from_me, peer_user_id, peer_name, sender_device_id,
	source_runtime, source_native_id, source_catalog_id, source_resume_id, source_wire_session,
	repository_id, title, wire_hash, state, state_seq, state_at, refusal_code, receipt_code, opened_json,
	other_opened_count, hash_conflicts, link_ended, created_at, updated_at, wire_body != '', held`

type rowScanner interface{ Scan(dest ...any) error }

func scanHandoff(row rowScanner, body *string) (Handoff, error) {
	var h Handoff
	var opened string
	dest := []any{&h.OrganizationID, &h.ID, &h.SentHere, &h.ToMe, &h.FromMe, &h.PeerUserID, &h.PeerName, &h.SenderDeviceID,
		&h.Source.Runtime, &h.Source.NativeID, &h.Source.CatalogID, &h.Source.ResumeID, &h.Source.ID,
		&h.RepositoryID, &h.Title, &h.WireHash, &h.State, &h.StateSeq, &h.StateAt, &h.RefusalCode, &h.ReceiptCode, &opened,
		&h.OtherOpenedCount, &h.HashConflicts, &h.LinkEnded, &h.CreatedAt, &h.UpdatedAt, &h.HasBody, &h.Held}
	if body != nil {
		dest = append(dest, body)
	}
	if err := row.Scan(dest...); err != nil {
		return Handoff{}, err
	}
	if opened != "" {
		var by teamwire.HandoffOpenedBy
		if err := json.Unmarshal([]byte(opened), &by); err != nil {
			return Handoff{}, fmt.Errorf("handoff %s: stored opened-by: %w", h.ID, err)
		}
		h.OpenedBy = &by
	}
	return h, nil
}

// handoffTx reads one row with its document inside the caller's transaction.
func handoffTx(q queryRower, organizationID, id string) (Handoff, bool, error) {
	var body string
	h, err := scanHandoff(q.QueryRow(`SELECT `+handoffCols+`, wire_body FROM handoff WHERE organization_id=? AND id=?`, organizationID, id), &body)
	if err == sql.ErrNoRows {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}
	h.WireBody = body
	return h, true, nil
}

// HandoffByID reads one row with its frozen document (Handoff.Document decodes it).
// ok is false when this device holds no such row.
func (ix *Index) HandoffByID(organizationID, id string) (Handoff, bool, error) {
	return handoffTx(ix.db, organizationID, id)
}

// FindHandoff reads a handoff by id alone, as a person names it: among the rows a
// listing for linkedOrganization shows (ListHandoffs's rule), so a row of another
// organization is never reached by guessing its id.
func (ix *Index) FindHandoff(linkedOrganization, id string) (Handoff, bool, error) {
	q := `SELECT organization_id FROM handoff WHERE id=?`
	args := []any{id}
	if linkedOrganization != "" {
		q += ` AND organization_id IN (?, '')`
		args = append(args, linkedOrganization)
	}
	var org string
	err := ix.db.QueryRow(q+` ORDER BY updated_at DESC LIMIT 1`, args...).Scan(&org)
	if err == sql.ErrNoRows {
		return Handoff{}, false, nil
	}
	if err != nil {
		return Handoff{}, false, err
	}
	return ix.HandoffByID(org, id)
}

// ListHandoffs lists rows without their documents, newest first. Linked to an
// organization, it lists that organization's rows and the local ones — an earlier
// organization's rows are not listed (criterion 68). Unlinked (""), it lists every row
// on the device: what was received stays readable after an unlink.
func (ix *Index) ListHandoffs(linkedOrganization string) ([]Handoff, error) {
	q := `SELECT ` + handoffCols + ` FROM handoff`
	var args []any
	if linkedOrganization != "" {
		q += ` WHERE organization_id IN (?, '')`
		args = append(args, linkedOrganization)
	}
	rows, err := ix.db.Query(q+` ORDER BY updated_at DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Handoff
	for rows.Next() {
		h, err := scanHandoff(rows, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HandoffHashConflicts counts, for one organization, the pulled rows whose wire hash
// differed from the one held for their id (criterion 66's fail path).
func (ix *Index) HandoffHashConflicts(organizationID string) (int, error) {
	var n int
	err := ix.db.QueryRow(`SELECT COALESCE(SUM(hash_conflicts),0) FROM handoff WHERE organization_id=?`, organizationID).Scan(&n)
	return n, err
}

// HandoffSend is one handoff leaving this device, or — with no organization — a local
// one. Body is the frozen wire document: exactly the bytes the preview showed, and the
// bytes every push of it carries.
type HandoffSend struct {
	OrganizationID string
	Record         teamwire.HandoffRecord
	Body           []byte
	PeerName       string
	// SelfSend marks a handoff addressed to the sender's own user: the originating
	// device holds both facts and lists it under Sent only.
	SelfSend bool
	At       int64
}

// SendHandoff writes the row and, for a team handoff, its outbox row with the frozen
// body, in one transaction (§6.7 "send"). A local handoff writes the row alone: no
// server, no receipt, no outbox row, and it works unlinked. A team send from a device
// whose link is not to in.OrganizationID is refused, nothing written.
func (ix *Index) SendHandoff(in HandoffSend) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rec := in.Record
	local := in.OrganizationID == ""
	state, toMe := HandoffQueued, in.SelfSend
	if local {
		// A local handoff is already where it is going: it is listed as received here.
		state, toMe = teamwire.HandoffReceived, true
	} else {
		linked, err := deviceLinked(tx)
		if err != nil {
			return err
		}
		org, err := linkedOrganizationTx(tx)
		if err != nil {
			return err
		}
		if !linked || org == "" {
			return ErrNotLinked
		}
		if org != in.OrganizationID {
			return ErrLinkChanged
		}
	}
	var deviceID string
	if err := tx.QueryRow(`SELECT id FROM sync_device LIMIT 1`).Scan(&deviceID); err != nil && err != sql.ErrNoRows {
		return err
	}
	repo := ""
	if rec.RepositoryID != nil {
		repo = *rec.RepositoryID
	}
	if _, err := tx.Exec(`INSERT INTO handoff(organization_id, id, sent_here, to_me, from_me, peer_user_id, peer_name, sender_device_id,
		source_runtime, source_native_id, source_catalog_id, source_resume_id, source_wire_session,
		repository_id, title, wire_body, wire_hash, held, state, state_at, created_at, updated_at)
		VALUES(?,?,1,?,0,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)`,
		in.OrganizationID, rec.ID, toMe, rec.Recipient.UserID, in.PeerName, deviceID,
		rec.Session.Runtime, rec.Session.NativeID, rec.Session.CatalogID, rec.Session.ResumeID, rec.Session.ID,
		repo, rec.Title, string(in.Body), rec.ContentHash, state, in.At, in.At, in.At); err != nil {
		return fmt.Errorf("handoff %s: %w", rec.ID, err)
	}
	if !local {
		if err := enqueueOutboxRow(tx, outboxInsert{kind: OutboxHandoff, globalID: rec.ID, contentHash: rec.ContentHash,
			scope: in.OrganizationID, at: in.At, sentWireBody: string(in.Body), sentWireHash: rec.ContentHash}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HandoffSettle is the server's answer to one pushed handoff or receipt row: accepted,
// duplicate, conflict, or the rejection's code.
type HandoffSettle struct {
	Seq  int64
	Code string
}

// HandoffSettled says what a settle did, as ids and codes only (never a title or text):
// the drain logs and chains it.
type HandoffSettled struct {
	Kind           string
	OrganizationID string
	HandoffID      string
	Transition     string // a receipt's transition; "" for the document
	Code           string
	State          string // the row's state after the settle
}

// SettleHandoffPush acknowledges the outbox row with its answer and moves the handoff's
// state, in one transaction (§6.7 "push answer"). A pushed document: accepted or
// duplicate makes it sent; any other answer makes it refused, terminal, with the code
// kept and the text kept. A pushed receipt: a rejection naming a terminal state lands
// that state (the server folded another terminal receipt first) and the code is kept
// for the person; handoff_started (a decline that arrived after a session started)
// lands started. A row already acknowledged keeps its first answer.
func (ix *Index) SettleHandoffPush(s HandoffSettle, at int64) (HandoffSettled, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return HandoffSettled{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var out HandoffSettled
	var body string
	var acked sql.NullInt64
	err = tx.QueryRow(`SELECT record_kind, global_id, scope, sent_wire_body, acked_at FROM sync_outbox WHERE seq=?`, s.Seq).
		Scan(&out.Kind, &out.HandoffID, &out.OrganizationID, &body, &acked)
	if err == sql.ErrNoRows || (err == nil && acked.Valid) {
		return HandoffSettled{}, nil
	}
	if err != nil {
		return HandoffSettled{}, err
	}
	out.Code = s.Code
	if _, err := tx.Exec(`UPDATE sync_outbox SET acked_at=?, ack_code=? WHERE seq=? AND acked_at IS NULL`, at, s.Code, s.Seq); err != nil {
		return HandoffSettled{}, err
	}
	accepted := receiptAnswerAccepted(s.Code)
	switch out.Kind {
	case OutboxHandoff:
		if accepted {
			_, err = tx.Exec(`UPDATE handoff SET state=?, state_at=?, updated_at=? WHERE organization_id=? AND id=? AND state=?`,
				teamwire.HandoffSent, at, at, out.OrganizationID, out.HandoffID, HandoffQueued)
		} else {
			// The text stays in wire_body so the person can send it again as a new handoff.
			_, err = tx.Exec(`UPDATE handoff SET refusal_code=?, state=CASE WHEN state=? THEN ? ELSE state END, state_at=?, updated_at=?
				WHERE organization_id=? AND id=?`, s.Code, HandoffQueued, HandoffRefused, at, at, out.OrganizationID, out.HandoffID)
		}
	case OutboxHandoffReceipt:
		var receipt teamwire.HandoffReceipt
		if err := json.Unmarshal([]byte(body), &receipt); err != nil {
			return HandoffSettled{}, fmt.Errorf("outbox row %d: frozen receipt: %w", s.Seq, err)
		}
		out.HandoffID, out.Transition = receipt.HandoffID, receipt.Transition
		if !accepted {
			err = ix.landRejectedReceiptTx(tx, out.OrganizationID, receipt.HandoffID, s.Code, at)
		}
	default:
		return HandoffSettled{}, fmt.Errorf("outbox row %d is a %s row, not a handoff row", s.Seq, out.Kind)
	}
	if err != nil {
		return HandoffSettled{}, err
	}
	if h, found, err := handoffTx(tx, out.OrganizationID, out.HandoffID); err != nil {
		return HandoffSettled{}, err
	} else if found {
		out.State = h.State
	}
	return out, tx.Commit()
}

// landRejectedReceiptTx keeps a rejected receipt's code on the row and, when the code
// names a terminal state the server already folded, lands that state.
func (ix *Index) landRejectedReceiptTx(tx *sql.Tx, organizationID, handoffID, code string, at int64) error {
	if _, err := tx.Exec(`UPDATE handoff SET receipt_code=?, updated_at=? WHERE organization_id=? AND id=?`,
		code, at, organizationID, handoffID); err != nil {
		return err
	}
	if code == teamwire.CodeHandoffStarted {
		// A decline arrived after a session started on another of the recipient's
		// devices: the item is started, as the server holds it, and not declined.
		_, err := tx.Exec(`UPDATE handoff SET state=?, state_at=? WHERE organization_id=? AND id=?`,
			teamwire.HandoffStarted, at, organizationID, handoffID)
		return err
	}
	for _, state := range []string{teamwire.HandoffDeclined, teamwire.HandoffClosed, teamwire.HandoffWithdrawn, teamwire.HandoffExpired} {
		if teamwire.HandoffTerminalCode(state) != code {
			continue
		}
		h, found, err := handoffTx(tx, organizationID, handoffID)
		if err != nil || !found {
			return err
		}
		return ix.landTerminalTx(tx, h, state, at)
	}
	return nil
}

// landTerminalTx moves a row to a terminal state the server reported or a person chose,
// and does what that state requires on this device: the open tickets and pending brief
// rows are cancelled (handoff_open.go), and a withdrawal or an expiry blanks a copy
// that was never opened here and that this device did not originate (handoffStateErases)
// — the title, the document, and the brief armed from them (eraseHandoffBriefsTx).
// A session that already confirmed the brief keeps what is in its context.
func (ix *Index) landTerminalTx(tx *sql.Tx, h Handoff, state string, at int64) error {
	if _, err := tx.Exec(`UPDATE handoff SET state=?, state_at=?, updated_at=? WHERE organization_id=? AND id=?`,
		state, at, at, h.OrganizationID, h.ID); err != nil {
		return err
	}
	if err := ix.cancelHandoffOpensTx(tx, h.OrganizationID, h.ID, state); err != nil {
		return err
	}
	if handoffStateErases(state) && !h.SentHere && h.State != teamwire.HandoffOpened {
		if _, err := tx.Exec(`UPDATE handoff SET wire_body='', title='' WHERE organization_id=? AND id=?`, h.OrganizationID, h.ID); err != nil {
			return err
		}
		return eraseHandoffBriefsTx(tx, h.OrganizationID, h.ID)
	}
	return nil
}

// eraseHandoffBriefsTx blanks the brief of every ticket of a handoff whose text is
// being taken back: the text as armed on the ticket, and the message of each delivery
// row made from it, whatever the row's state. The rows stay as the record of what
// happened; a session already handed a brief keeps what is in its context.
func eraseHandoffBriefsTx(tx *sql.Tx, organizationID, handoffID string) error {
	if _, err := tx.Exec(`UPDATE session_delivery SET message='' WHERE run_id IN
		(SELECT ? || ticket_id FROM handoff_open WHERE organization_id=? AND handoff_id=?)`,
		HandoffDeliveryRunPrefix, organizationID, handoffID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE handoff_open SET brief_text='' WHERE organization_id=? AND handoff_id=?`, organizationID, handoffID)
	return err
}

// handoffStateErases reports the terminal states that take the text back from a copy
// never opened here: a withdrawal (the sender took it back) and an expiry (the server
// erases the title and the text when the window runs out, and the device's copy
// follows it). A declined or closed handoff keeps its text: the recipient ended it.
func handoffStateErases(state string) bool {
	return state == teamwire.HandoffWithdrawn || state == teamwire.HandoffExpired
}

// HandoffTransition is a person's act on a row: decline, close, or withdraw.
type HandoffTransition struct {
	OrganizationID string
	ID             string
	Transition     string // teamwire.TransitionDeclined, TransitionClosed or TransitionWithdrawn
	At             int64
}

// TransitionHandoff applies a decline, a close or a withdraw: the state and, for a team
// handoff, the receipt's outbox row, in one transaction (§6.7). §6.2's "when offered"
// rules are enforced here, inside the transaction, so two acts cannot both pass:
// decline only while received; close once started or opened; withdraw on any
// non-terminal item, by a device of the sender. A row of an ended link offers none —
// nothing is sent after an unlink. The refusal is a *HandoffRefusal.
func (ix *Index) TransitionHandoff(in HandoffTransition) (Handoff, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return Handoff{}, err
	}
	defer func() { _ = tx.Rollback() }()
	h, found, err := handoffTx(tx, in.OrganizationID, in.ID)
	if err != nil {
		return Handoff{}, err
	}
	if !found {
		return Handoff{}, ErrHandoffNotFound
	}
	if refusal := transitionRefusal(h, in.Transition); refusal != nil {
		return h, refusal
	}
	if err := ix.landTerminalTx(tx, h, handoffTransitionStates[in.Transition], in.At); err != nil {
		return Handoff{}, err
	}
	if _, err := ix.EnqueueHandoffReceiptTx(tx, HandoffReceiptInput{OrganizationID: h.OrganizationID, HandoffID: h.ID,
		Transition: in.Transition, At: in.At, Again: true}); err != nil {
		return Handoff{}, err
	}
	h, _, err = handoffTx(tx, in.OrganizationID, in.ID)
	if err != nil {
		return Handoff{}, err
	}
	return h, tx.Commit()
}

// handoffTransitionStates is the terminal state each of a person's acts lands.
var handoffTransitionStates = map[string]string{teamwire.TransitionDeclined: teamwire.HandoffDeclined,
	teamwire.TransitionClosed: teamwire.HandoffClosed, teamwire.TransitionWithdrawn: teamwire.HandoffWithdrawn}

// handoffStateTransition is the act that lands a terminal state, or "" for a state no
// act on a device lands (an expiry is the server's).
func handoffStateTransition(state string) string {
	for transition, landed := range handoffTransitionStates {
		if landed == state {
			return transition
		}
	}
	return ""
}

// receiptAnswerAccepted reports whether a push answer means the server holds the record.
func receiptAnswerAccepted(code string) bool {
	return code == teamwire.StatusAccepted || code == teamwire.StatusDuplicate
}

// HandoffOffers lists the transitions a row offers a person on this device now —
// §6.2's rules, the same ones TransitionHandoff enforces.
func HandoffOffers(h Handoff) []string {
	offers := []string{}
	for _, transition := range []string{teamwire.TransitionDeclined, teamwire.TransitionClosed, teamwire.TransitionWithdrawn} {
		if transitionRefusal(h, transition) == nil {
			offers = append(offers, transition)
		}
	}
	return offers
}

// transitionRefusal is §6.2's "when the console offers each", as a check.
func transitionRefusal(h Handoff, transition string) *HandoffRefusal {
	refuse := func(code string) *HandoffRefusal { return &HandoffRefusal{Code: code, State: h.State} }
	if h.LinkEnded {
		return refuse(HandoffCodeLinkEnded)
	}
	if teamwire.HandoffTerminal(h.State) {
		return refuse(teamwire.HandoffTerminalCode(h.State))
	}
	// A self-send's originating device lists the item under Sent only: it may withdraw,
	// never decline or close (§6.2's table).
	recipientPart := h.ToMe && (!h.SentHere || h.Local())
	switch transition {
	case teamwire.TransitionDeclined:
		if !recipientPart {
			return refuse(HandoffCodeNotOffered)
		}
		// "While the item is received": before a session started for it on any of the
		// recipient's devices. A copy that just landed still reads sent until the
		// server folds this device's own received receipt; it is received here.
		if h.State != teamwire.HandoffReceived && h.State != teamwire.HandoffSent {
			return refuse(HandoffCodeWrongState)
		}
	case teamwire.TransitionClosed:
		if !recipientPart {
			return refuse(HandoffCodeNotOffered)
		}
		if h.State != teamwire.HandoffStarted && h.State != teamwire.HandoffOpened {
			return refuse(HandoffCodeWrongState)
		}
	case teamwire.TransitionWithdrawn:
		if !h.SentHere && !h.FromMe {
			return refuse(HandoffCodeNotOffered)
		}
		if h.State == HandoffRefused {
			return refuse(HandoffCodeWrongState)
		}
	default:
		return refuse(HandoffCodeNotOffered)
	}
	return nil
}

// HandoffReceiptInput is one receipt to enqueue. TicketID and Session are set for
// started and opened (the session that claimed the ticket: runtime, native id, and the
// wire id derived from them); the other transitions carry neither.
type HandoffReceiptInput struct {
	OrganizationID string
	HandoffID      string
	Transition     string
	TicketID       string
	Session        *teamwire.SessionIdentity
	At             int64
	// Again is set for a person's act (decline, close, withdraw): if the server
	// rejected this same receipt before, it is queued again. Without it the act would
	// change the row here and tell the server nothing.
	Again bool
}

// EnqueueHandoffReceiptTx writes one receipt's outbox row, body frozen, inside the
// caller's transaction, and reports whether it wrote one. The receipt's id is
// deterministic (teamwire.ReceiptID), and a receipt this device already queued or sent
// is not written again: a second row would carry a different created_at under the same
// id, which the server must answer as a conflict. The one exception is in.Again: when
// every earlier row for the id was rejected, a new row is queued carrying the rejected
// row's frozen body byte for byte — the same record, sent again. A local handoff has no
// receipts, and an unlinked device enqueues nothing.
func (ix *Index) EnqueueHandoffReceiptTx(tx *sql.Tx, in HandoffReceiptInput) (bool, error) {
	if in.OrganizationID == "" {
		return false, nil
	}
	var deviceID string
	var linked int
	if err := tx.QueryRow(`SELECT id, linked FROM sync_device LIMIT 1`).Scan(&deviceID, &linked); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	if linked != 1 {
		return false, nil
	}
	nativeID := ""
	if in.Session != nil {
		nativeID = in.Session.NativeID
	}
	id := teamwire.ReceiptID(in.HandoffID, in.Transition, deviceID, in.TicketID, nativeID)
	frozen, live, err := earlierReceiptTx(tx, id)
	if err != nil {
		return false, err
	}
	if live || (frozen != "" && !in.Again) {
		return false, nil
	}
	body := []byte(frozen)
	if frozen == "" {
		body, err = json.Marshal(teamwire.HandoffReceipt{SchemaVersion: teamwire.HandoffReceiptSchemaVersion, ID: id,
			HandoffID: in.HandoffID, Transition: in.Transition, TicketID: in.TicketID, Session: in.Session,
			CreatedAt: time.Unix(in.At, 0).UTC().Format(time.RFC3339)})
		if err != nil {
			return false, err
		}
	}
	hash := teamwire.ContentHash(body)
	if err := enqueueOutboxRow(tx, outboxInsert{kind: OutboxHandoffReceipt, globalID: id, contentHash: hash,
		scope: in.OrganizationID, at: in.At, sentWireBody: string(body), sentWireHash: hash}); err != nil {
		return false, err
	}
	return true, nil
}

// earlierReceiptTx reads what this device already queued for one receipt id. live says
// a row is still waiting for its answer or was answered accepted: the server holds the
// record or will. Otherwise frozen is the newest rejected row's body, "" when there is
// no row at all.
func earlierReceiptTx(tx *sql.Tx, id string) (frozen string, live bool, err error) {
	rows, err := tx.Query(`SELECT sent_wire_body, acked_at, ack_code FROM sync_outbox WHERE record_kind=? AND global_id=? ORDER BY seq`,
		OutboxHandoffReceipt, id)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var body, code string
		var acked sql.NullInt64
		if err := rows.Scan(&body, &acked, &code); err != nil {
			return "", false, err
		}
		if !acked.Valid || receiptAnswerAccepted(code) {
			live = true
		}
		if body != "" {
			frozen = body
		}
	}
	return frozen, live, rows.Err()
}

// MarkHandoffsLinkEnded marks every team handoff row as an ended link's (§6.7
// "unlink"): the rows stay readable, nothing is sent for them, and a handoff still
// queued can no longer leave, so it reads refused with the text kept. The unlink path
// reaches this through ResetSyncQueue; it is exported for a caller that ends a link
// without resetting the queue.
func (ix *Index) MarkHandoffsLinkEnded(organizationID string) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ix.endHandoffLinkTx(tx, organizationID); err != nil {
		return err
	}
	return tx.Commit()
}

// endHandoffLinkTx is the handoff half of an unlink, inside the queue reset's
// transaction: rows marked, queued sends refused by name, the handoff pull's cursors
// and the member directory forgotten, and the open tickets of the ended link handed to
// the open owner to cancel (handoff_open.go). organizationID is the link that ended;
// every team row is marked whichever organization it names, since no link remains.
func (ix *Index) endHandoffLinkTx(tx *sql.Tx, organizationID string) error {
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`UPDATE handoff SET refusal_code=?, state=? WHERE organization_id != '' AND state=?`,
			[]any{HandoffCodeLinkEnded, HandoffRefused, HandoffQueued}},
		{`UPDATE handoff SET link_ended=1 WHERE organization_id != '' AND link_ended=0`, nil},
		{`DELETE FROM sync_cursor WHERE scope LIKE ?`, []any{HandoffCursorPrefix + "%"}},
		{`DELETE FROM sync_cursor WHERE scope LIKE ?`, []any{HandoffBootstrapPrefix + "%"}},
		{`DELETE FROM team_member`, nil},
	} {
		if _, err := tx.Exec(step.query, step.args...); err != nil {
			return err
		}
	}
	return ix.cancelHandoffOpensTx(tx, organizationID, "", HandoffCodeLinkEnded)
}

// ReclaimHandoffs gives a link back the rows an earlier link to the SAME organization
// left behind (item 5's rule for acknowledgements, applied to handoffs): they are this
// link's again, so their actions are offered and the next pull updates them.
func (ix *Index) ReclaimHandoffs(organizationID string) error {
	if organizationID == "" {
		return nil
	}
	_, err := ix.db.Exec(`UPDATE handoff SET link_ended=0 WHERE organization_id=? AND link_ended=1`, organizationID)
	return err
}
