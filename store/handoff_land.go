package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"crossing-guard/teamwire"
)

// Landing the handoff pull (team rest-of-release plan §6.2, §6.6, §6.7). The handoff
// pull is its own request with its own cursor — a position in ONE organization's
// handoff sequence, which is not the memory sequence — and one page lands in one
// transaction: every row or its update, the received receipts, and the cursor's
// compare-and-set. A crash before the commit re-pulls the page and lands it again; a
// replayed page updates nothing and duplicates nothing.

// HandoffCursorPrefix is the sync_cursor scope the handoff pull keeps, completed by the
// organization's id.
const HandoffCursorPrefix = "handoff:"

// HandoffCursorScope is the handoff pull's cursor row for one organization.
func HandoffCursorScope(organizationID string) string { return HandoffCursorPrefix + organizationID }

// HandoffBootstrapPrefix is the sync_cursor scope recording that this link's first
// handoff pull drained; until then every page is asked for with bootstrap set.
const HandoffBootstrapPrefix = "handoff-bootstrap:"

// HandoffBootstrapScope is that row for one organization.
func HandoffBootstrapScope(organizationID string) string {
	return HandoffBootstrapPrefix + organizationID
}

// HandoffBootstrapped reports whether this link's first handoff pull has drained.
func (ix *Index) HandoffBootstrapped(organizationID string) (bool, error) {
	n, err := ix.SyncCursor(HandoffBootstrapScope(organizationID))
	return n > 0, err
}

// HandoffPage is one pulled page. From is the cursor the tick read before it asked; To
// is the page's cursor.
type HandoffPage struct {
	OrganizationID string
	Rows           []teamwire.PullRow
	From, To       int64
	// Drained says the server reported no further page: landing it records that the
	// link's first handoff pull is done.
	Drained bool
	At      int64
}

// HandoffLandEffects counts what landing one page did. Ids only, never a title or text.
type HandoffLandEffects struct {
	Landed        int // rows this device did not hold
	Updated       int // held rows whose facts or state moved
	Stale         int // rows at or below the sequence already landed: ignored
	HashConflicts int // rows whose wire hash differed from the held one: refused
	Erased        int // unopened copies a withdrawal or an expiry blanked
	Unlandable    int // rows this build could not read: skipped
	Receipts      int // received receipts enqueued
	// Moved is false when the cursor was no longer From (another tick, or a reset):
	// nothing landed.
	Moved bool
}

// LandHandoffPage lands a page and advances the cursor from p.From to p.To, in one
// transaction. It checks, inside the transaction, that the device is still linked to
// p.OrganizationID (a page in flight across an unlink lands nothing: ErrLinkChanged)
// and that the cursor is still p.From.
func (ix *Index) LandHandoffPage(p HandoffPage) (HandoffLandEffects, error) {
	var eff HandoffLandEffects
	tx, err := ix.db.Begin()
	if err != nil {
		return eff, err
	}
	defer func() { _ = tx.Rollback() }()
	linked, err := linkedOrganizationTx(tx)
	if err != nil {
		return eff, err
	}
	if linked != p.OrganizationID || p.OrganizationID == "" {
		return eff, ErrLinkChanged
	}
	scope := HandoffCursorScope(p.OrganizationID)
	current, err := cursorTx(tx, scope)
	if err != nil {
		return eff, err
	}
	if current != p.From {
		return eff, nil
	}
	for _, row := range p.Rows {
		if row.Handoff == nil {
			continue // not a handoff row: another stream's, never landed here
		}
		if err := ix.landHandoffTx(tx, p.OrganizationID, row.Seq, *row.Handoff, p.At, &eff); err != nil {
			return HandoffLandEffects{}, fmt.Errorf("handoff %s: %w", row.Handoff.ID, err)
		}
	}
	if p.To > p.From {
		if _, err := tx.Exec(`INSERT INTO sync_cursor(scope,cursor,updated_at) VALUES(?,?,?)
			ON CONFLICT(scope) DO UPDATE SET cursor=excluded.cursor, updated_at=excluded.updated_at`, scope, fmt.Sprint(p.To), p.At); err != nil {
			return HandoffLandEffects{}, err
		}
	}
	if p.Drained {
		if _, err := tx.Exec(`INSERT INTO sync_cursor(scope,cursor,updated_at) VALUES(?,'1',?) ON CONFLICT(scope) DO NOTHING`,
			HandoffBootstrapScope(p.OrganizationID), p.At); err != nil {
			return HandoffLandEffects{}, err
		}
	}
	eff.Moved = true
	return eff, tx.Commit()
}

func cursorTx(q queryRower, scope string) (int64, error) {
	var raw string
	switch err := q.QueryRow(`SELECT cursor FROM sync_cursor WHERE scope=?`, scope).Scan(&raw); {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, err
	}
	var n int64
	_, err := fmt.Sscan(raw, &n)
	return n, err
}

// pulledDocument is a pulled row's document as this device will hold it.
type pulledDocument struct {
	record teamwire.HandoffRecord
	body   string
	hash   string
}

// readPulledDocument decodes the document a recipient's device was served. ok is false
// when the row carries none. The wire hash is the server's compare key, taken as
// served: the device does not recompute it, because the server's secret backstop may
// have redacted the body further than the sender did.
func readPulledDocument(p teamwire.PulledHandoff) (doc pulledDocument, ok bool, err error) {
	if len(p.Document) == 0 || string(p.Document) == "null" {
		return doc, false, nil
	}
	if err := json.Unmarshal(p.Document, &doc.record); err != nil {
		return doc, false, err
	}
	if doc.record.ID != p.ID {
		return doc, false, fmt.Errorf("the document names %q", doc.record.ID)
	}
	doc.body, doc.hash = string(p.Document), p.WireHash
	if doc.hash == "" {
		doc.hash = doc.record.ContentHash
	}
	return doc, true, nil
}

// landHandoffTx lands one delivery row by (id, wire hash): a row this device does not
// hold is written; a held row with the same hash has its facts and state updated when
// the sequence is higher; a different hash for a known id is refused and counted and
// the held row is unchanged (criterion 66). A device never skips a row it already has.
func (ix *Index) landHandoffTx(tx *sql.Tx, organizationID string, seq int64, p teamwire.PulledHandoff, at int64, eff *HandoffLandEffects) error {
	doc, hasDoc, err := readPulledDocument(p)
	if err != nil {
		eff.Unlandable++ // skipped, not fatal: one unreadable row must not hold the cursor
		return nil
	}
	held, found, err := handoffTx(tx, organizationID, p.ID)
	if err != nil {
		return err
	}
	servedHash := p.WireHash
	if hasDoc {
		servedHash = doc.hash
	}
	switch {
	case !found:
		if err := insertPulledHandoffTx(tx, organizationID, seq, p, doc, hasDoc, at); err != nil {
			return err
		}
		eff.Landed++
	case servedHash != "" && held.WireHash != "" && servedHash != held.WireHash:
		eff.HashConflicts++
		_, err := tx.Exec(`UPDATE handoff SET hash_conflicts = hash_conflicts + 1 WHERE organization_id=? AND id=?`, organizationID, p.ID)
		return err
	case seq <= held.StateSeq:
		eff.Stale++
	default:
		if err := ix.updatePulledHandoffTx(tx, held, seq, p, doc, hasDoc, at, eff); err != nil {
			return err
		}
		eff.Updated++
	}
	wrote, err := ix.ensureReceivedReceiptTx(tx, organizationID, p.ID, at)
	if wrote {
		eff.Receipts++
	}
	return err
}

// pulledPeer is the other party of a pulled row: the sender on a row addressed to this
// user, the recipient on a row this user sent from another device.
func pulledPeer(p teamwire.PulledHandoff) (userID, name string) {
	if p.ToMe {
		return p.SenderUserID, p.SenderName
	}
	return p.RecipientUserID, p.RecipientName
}

// handoffWireTime reads an RFC 3339 time as Unix seconds; fallback when it does not parse.
func handoffWireTime(value string, fallback int64) int64 {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.Unix()
	}
	return fallback
}

func openedJSON(by *teamwire.HandoffOpenedBy) (string, error) {
	if by == nil {
		return "", nil
	}
	raw, err := json.Marshal(by)
	return string(raw), err
}

// insertPulledHandoffTx writes a row this device did not hold. With a document it is
// the recipient's copy. Without one it is either the sender's other device's row —
// who it went to, when, and the state, no title and no text — or a handoff that ended
// before it reached this device: the sender and the time only (criterion 78).
func insertPulledHandoffTx(tx *sql.Tx, organizationID string, seq int64, p teamwire.PulledHandoff, doc pulledDocument, hasDoc bool, at int64) error {
	peerID, peerName := pulledPeer(p)
	opened, err := openedJSON(p.OpenedBy)
	if err != nil {
		return err
	}
	var rec teamwire.HandoffRecord
	repo, body, hash := "", "", p.WireHash
	// A withdrawn or expired handoff's document is never kept, even if a server served one.
	if hasDoc && !handoffStateErases(p.State) {
		rec, body, hash = doc.record, doc.body, doc.hash
		if rec.RepositoryID != nil {
			repo = *rec.RepositoryID
		}
	}
	_, err = tx.Exec(`INSERT INTO handoff(organization_id, id, sent_here, to_me, from_me, peer_user_id, peer_name, sender_device_id,
		source_runtime, source_native_id, source_catalog_id, source_resume_id, source_wire_session,
		repository_id, title, wire_body, wire_hash, held, state, state_seq, state_at, opened_json, other_opened_count, created_at, updated_at)
		VALUES(?,?,0,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		organizationID, p.ID, p.ToMe, p.FromMe, peerID, peerName, p.SenderDeviceID,
		rec.Session.Runtime, rec.Session.NativeID, rec.Session.CatalogID, rec.Session.ResumeID, rec.Session.ID,
		repo, rec.Title, body, hash, body != "", p.State, seq, handoffWireTime(p.StateAt, at), opened, p.OtherOpenedCount, handoffWireTime(p.CreatedAt, at), at)
	return err
}

// updatePulledHandoffTx moves a held row to a higher sequence: the facts, a document
// this device was owed and did not have, and the state — with what a terminal state
// requires (landTerminalTx). A served state that is not terminal never lowers what
// this device already knows (pulledStateKeepsHeldTx).
func (ix *Index) updatePulledHandoffTx(tx *sql.Tx, held Handoff, seq int64, p teamwire.PulledHandoff, doc pulledDocument, hasDoc bool, at int64, eff *HandoffLandEffects) error {
	peerID, peerName := pulledPeer(p)
	if held.SentHere {
		// The originating device wrote the recipient itself; a pulled row only renames.
		peerID = held.PeerUserID
	}
	if peerName == "" {
		peerName = held.PeerName
	}
	opened, err := openedJSON(p.OpenedBy)
	if err != nil {
		return err
	}
	stateAt := handoffWireTime(p.StateAt, at)
	if _, err := tx.Exec(`UPDATE handoff SET to_me = MAX(to_me, ?), from_me = MAX(from_me, ?), peer_user_id=?, peer_name=?,
		sender_device_id = CASE WHEN ? != '' THEN ? ELSE sender_device_id END,
		state_seq=?, opened_json=?, other_opened_count=?, updated_at=? WHERE organization_id=? AND id=?`,
		p.ToMe, p.FromMe, peerID, peerName, p.SenderDeviceID, p.SenderDeviceID, seq, opened, p.OtherOpenedCount, at,
		held.OrganizationID, held.ID); err != nil {
		return err
	}
	if hasDoc && held.WireBody == "" && !teamwire.HandoffTerminal(p.State) {
		repo := ""
		if doc.record.RepositoryID != nil {
			repo = *doc.record.RepositoryID
		}
		s := doc.record.Session
		if _, err := tx.Exec(`UPDATE handoff SET title=?, wire_body=?, wire_hash=?, held=1, repository_id=?, source_runtime=?, source_native_id=?,
			source_catalog_id=?, source_resume_id=?, source_wire_session=? WHERE organization_id=? AND id=?`,
			doc.record.Title, doc.body, doc.hash, repo, s.Runtime, s.NativeID, s.CatalogID, s.ResumeID, s.ID,
			held.OrganizationID, held.ID); err != nil {
			return err
		}
	}
	if teamwire.HandoffTerminal(p.State) {
		if p.State == held.State {
			return nil
		}
		erases := handoffStateErases(p.State) && !held.SentHere && held.State != teamwire.HandoffOpened && held.WireBody != ""
		if err := ix.landTerminalTx(tx, held, p.State, stateAt); err != nil {
			return err
		}
		if erases {
			eff.Erased++
		}
		return nil
	}
	if keep, err := ix.pulledStateKeepsHeldTx(tx, held); err != nil || keep {
		return err
	}
	_, err = tx.Exec(`UPDATE handoff SET state=?, state_at=? WHERE organization_id=? AND id=?`, p.State, stateAt, held.OrganizationID, held.ID)
	return err
}

// pulledStateKeepsHeldTx reports whether the state this device holds stands against a
// non-terminal state the server served. The server's fold can be behind what happened
// here, and writing it over the row would undo an act:
//
//   - opened is never lowered. A session on this device holds the text; a row put back
//     to started would let a later withdrawal erase a copy that was opened.
//   - a terminal state stands. This device declined, closed or withdrew and the
//     receipt is still on its way (or was accepted and the fold has not caught up), or
//     the server itself reported the state earlier. Its tickets are already cancelled.
//     It gives way only when the server rejected this device's own receipt for it: the
//     server never held that state, the row follows the server again, and the person
//     can act again (HandoffReceiptInput.Again).
func (ix *Index) pulledStateKeepsHeldTx(tx *sql.Tx, held Handoff) (bool, error) {
	if held.State == teamwire.HandoffOpened {
		return true, nil
	}
	if !teamwire.HandoffTerminal(held.State) {
		return false, nil
	}
	rejected, err := ownTerminalReceiptRejectedTx(tx, held)
	return !rejected, err
}

// ownTerminalReceiptRejectedTx reports whether this device sent a receipt for the
// terminal state the row holds and the server answered it with a rejection.
func ownTerminalReceiptRejectedTx(tx *sql.Tx, held Handoff) (bool, error) {
	transition := handoffStateTransition(held.State)
	if transition == "" {
		return false, nil
	}
	var deviceID string
	if err := tx.QueryRow(`SELECT id FROM sync_device LIMIT 1`).Scan(&deviceID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	var acked sql.NullInt64
	var code string
	err := tx.QueryRow(`SELECT acked_at, ack_code FROM sync_outbox WHERE record_kind=? AND global_id=? ORDER BY seq DESC LIMIT 1`,
		OutboxHandoffReceipt, teamwire.ReceiptID(held.ID, transition, deviceID, "", "")).Scan(&acked, &code)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return acked.Valid && !receiptAnswerAccepted(code), nil
}

// ensureReceivedReceiptTx enqueues this device's received receipt for a handoff it
// holds the document of, once: each recipient device that lands the document says so
// (§6.2). The originating device of a self-send sends none, and a handoff already in a
// terminal state is owed none (the server would reject it).
func (ix *Index) ensureReceivedReceiptTx(tx *sql.Tx, organizationID, id string, at int64) (bool, error) {
	h, found, err := handoffTx(tx, organizationID, id)
	if err != nil || !found {
		return false, err
	}
	if !h.ToMe || h.SentHere || h.WireBody == "" || h.LinkEnded || teamwire.HandoffTerminal(h.State) {
		return false, nil
	}
	queued, err := ix.EnqueueHandoffReceiptTx(tx, HandoffReceiptInput{OrganizationID: organizationID, HandoffID: id,
		Transition: teamwire.TransitionReceived, At: at})
	if err != nil {
		return false, err
	}
	// This device holds the document, so here the handoff IS received: the row does not
	// wait for the server to fold this device's own receipt and serve it back.
	if _, err := tx.Exec(`UPDATE handoff SET state=? WHERE organization_id=? AND id=? AND state=?`,
		teamwire.HandoffReceived, organizationID, id, teamwire.HandoffSent); err != nil {
		return false, err
	}
	return queued, nil
}
