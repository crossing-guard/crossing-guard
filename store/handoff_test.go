package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/teamwire"
)

const handoffTestOrg = "org_1"

// handoffDevice is a store linked to handoffTestOrg.
func handoffDevice(t *testing.T) *Index {
	t.Helper()
	ix := openResultTestIndex(t)
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetLinkedOrganization(handoffTestOrg, 1); err != nil {
		t.Fatal(err)
	}
	return ix
}

func handoffRecord(title, body, recipient string) (teamwire.HandoffRecord, []byte) {
	rec := teamwire.HandoffRecord{SchemaVersion: teamwire.HandoffSchemaVersion, ID: engine.NewTypedID(teamwire.HandoffIDPrefix),
		Session:   teamwire.SessionIdentity{ID: "ses_x", Runtime: "claude", NativeID: "native-1", CatalogID: "catalog-1", ResumeID: "resume-1"},
		CreatedAt: "2026-10-04T16:00:00Z", CreatedBy: teamwire.Actor{Type: "user", ID: "member"},
		Recipient: teamwire.HandoffRecipient{UserID: recipient}, Title: title, BodyMarkdown: body,
		Remaining: []string{"one"}, Agents: []teamwire.HandoffAgent{}, GovernanceState: teamwire.HandoffGovernance{Tags: []teamwire.HandoffTag{}},
		AnchorsScope: teamwire.HandoffAnchorsScope}
	rec.ContentHash = teamwire.HandoffWireHash(rec)
	raw, _ := json.Marshal(rec)
	return rec, raw
}

func sendTestHandoff(t *testing.T, ix *Index, org, recipient string, self bool) (teamwire.HandoffRecord, []byte) {
	t.Helper()
	rec, body := handoffRecord("Finish the drain", "the body", recipient)
	if err := ix.SendHandoff(HandoffSend{OrganizationID: org, Record: rec, Body: body, PeerName: "Teammate", SelfSend: self, At: 100}); err != nil {
		t.Fatal(err)
	}
	return rec, body
}

func mustHandoff(t *testing.T, ix *Index, org, id string) Handoff {
	t.Helper()
	h, found, err := ix.HandoffByID(org, id)
	if err != nil || !found {
		t.Fatalf("handoff %s: found=%v err=%v", id, found, err)
	}
	return h
}

func pendingOfKind(t *testing.T, ix *Index, kind string) []OutboxRow {
	t.Helper()
	rows, err := ix.OutboxBatch(500, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []OutboxRow
	for _, r := range rows {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// pulled builds a delivery row as the server serves it to a recipient's device.
func pulled(seq int64, rec teamwire.HandoffRecord, body []byte, state string) teamwire.PullRow {
	return teamwire.PullRow{Seq: seq, Kind: teamwire.KindHandoff, Handoff: &teamwire.PulledHandoff{ID: rec.ID, ToMe: true,
		SenderUserID: "usr_sender", SenderName: "Sender", SenderDeviceID: "dev_s", RecipientUserID: "usr_me", RecipientName: "Me",
		State: state, CreatedAt: "2026-10-04T16:00:00Z", StateAt: "2026-10-04T16:00:05Z", WireHash: rec.ContentHash, Document: body}}
}

func landPage(t *testing.T, ix *Index, from, to int64, rows ...teamwire.PullRow) HandoffLandEffects {
	t.Helper()
	eff, err := ix.LandHandoffPage(HandoffPage{OrganizationID: handoffTestOrg, Rows: rows, From: from, To: to, At: 200})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// §6.7 "send": the row and its outbox row, body frozen, commit together; the body the
// drain reads after a restart is byte-identical to the one that was written.
func TestSendHandoffWritesRowAndFrozenOutboxRowTogether(t *testing.T) {
	ix := handoffDevice(t)
	rec, body := sendTestHandoff(t, ix, handoffTestOrg, "usr_teammate", false)
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if !h.SentHere || h.ToMe || h.State != HandoffQueued || h.WireBody != string(body) || h.WireHash != rec.ContentHash {
		t.Fatalf("a sent item not yet accepted reads queued and keeps its frozen body: %+v", h)
	}
	if h.Source.Runtime != "claude" || h.Source.NativeID != "native-1" || h.Source.CatalogID != "catalog-1" || h.Source.ResumeID != "resume-1" || h.Source.ID != "ses_x" {
		t.Fatalf("the three session identities and the wire id are four values: %+v", h.Source)
	}
	rows := pendingOfKind(t, ix, OutboxHandoff)
	if len(rows) != 1 || rows[0].GlobalID != rec.ID || rows[0].SentWireBody != string(body) || rows[0].SentWireHash != rec.ContentHash {
		t.Fatalf("one outbox row with the frozen body: %+v", rows)
	}
	again := pendingOfKind(t, ix, OutboxHandoff)
	if again[0].SentWireBody != rows[0].SentWireBody {
		t.Fatal("a retry reads the same bytes")
	}
	// A send to another organization than the linked one writes nothing.
	other, otherBody := handoffRecord("t", "b", "usr_x")
	if err := ix.SendHandoff(HandoffSend{OrganizationID: "org_other", Record: other, Body: otherBody, At: 1}); !errors.Is(err, ErrLinkChanged) {
		t.Fatalf("want ErrLinkChanged, got %v", err)
	}
	if _, found, _ := ix.HandoffByID("org_other", other.ID); found {
		t.Fatal("a refused send writes no row")
	}
}

// Criterion 62: with 10,000 event rows queued ahead of it, a handoff is in the next
// drain's batch, at its head, and its receipt follows it in order.
func TestHandoffLeavesAheadOfTenThousandQueuedRows(t *testing.T) {
	ix := handoffDevice(t)
	tx, err := ix.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10_000; i++ {
		if _, err := tx.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at) VALUES('event',?,?,'s',1)`,
			"evt_"+strings.Repeat("0", 5)+string(rune('a'+i%26))+engine.NewTypedID("x"), "h"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rec, _ := sendTestHandoff(t, ix, handoffTestOrg, "usr_teammate", false)
	if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: teamwire.TransitionWithdrawn, At: 101}); err != nil {
		t.Fatal(err)
	}
	batch, err := ix.OutboxBatch(50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 50 || batch[0].Kind != OutboxHandoff || batch[0].GlobalID != rec.ID || batch[1].Kind != OutboxHandoffReceipt {
		t.Fatalf("handoff rows lead the batch, oldest first: %s %s", batch[0].Kind, batch[1].Kind)
	}
	for _, r := range batch[2:] {
		if r.Kind != OutboxEvent {
			t.Fatalf("then the backlog in sequence order, got %s", r.Kind)
		}
	}
	// A parked handoff kind is left out and the backlog still drains.
	batch, _ = ix.OutboxBatch(50, []string{OutboxHandoff, OutboxHandoffReceipt})
	if batch[0].Kind != OutboxEvent {
		t.Fatalf("parked kinds never occupy the head: %s", batch[0].Kind)
	}
}

// §6.6 push answers, written with the acknowledgement in one transaction.
func TestHandoffPushAnswerSettlesStateWithTheAcknowledgement(t *testing.T) {
	for _, code := range []string{teamwire.StatusAccepted, teamwire.StatusDuplicate} {
		ix := handoffDevice(t)
		rec, _ := sendTestHandoff(t, ix, handoffTestOrg, "usr_teammate", false)
		row := pendingOfKind(t, ix, OutboxHandoff)[0]
		out, err := ix.SettleHandoffPush(HandoffSettle{Seq: row.Seq, Code: code}, 150)
		if err != nil || out.State != teamwire.HandoffSent || out.HandoffID != rec.ID {
			t.Fatalf("%s → sent: %+v %v", code, out, err)
		}
		if len(pendingOfKind(t, ix, OutboxHandoff)) != 0 {
			t.Fatalf("%s acknowledges the row", code)
		}
		// A second settle of an acknowledged row changes nothing.
		if out, err := ix.SettleHandoffPush(HandoffSettle{Seq: row.Seq, Code: teamwire.CodeOverCap}, 151); err != nil || out.HandoffID != "" {
			t.Fatalf("an acknowledged row keeps its first answer: %+v %v", out, err)
		}
		if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffSent || h.RefusalCode != "" {
			t.Fatalf("state after a late answer: %+v", h)
		}
	}
	for _, code := range []string{teamwire.CodeUnknownRecipient, teamwire.CodeRecipientInactive, teamwire.CodeRecipientInboxFull,
		teamwire.CodeRateLimited, teamwire.CodeOverCap, teamwire.StatusConflict} {
		ix := handoffDevice(t)
		rec, body := sendTestHandoff(t, ix, handoffTestOrg, "usr_teammate", false)
		row := pendingOfKind(t, ix, OutboxHandoff)[0]
		if _, err := ix.SettleHandoffPush(HandoffSettle{Seq: row.Seq, Code: code}, 150); err != nil {
			t.Fatal(err)
		}
		h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
		if h.State != HandoffRefused || h.RefusalCode != code || h.WireBody != string(body) || h.Title == "" {
			t.Fatalf("%s → refused, the code kept, the text kept: %+v", code, h)
		}
		var ack string
		if err := ix.db.QueryRow(`SELECT ack_code FROM sync_outbox WHERE seq=?`, row.Seq).Scan(&ack); err != nil || ack != code {
			t.Fatalf("the acknowledgement records the answer: %q %v", ack, err)
		}
		if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: teamwire.TransitionWithdrawn, At: 160}); err == nil {
			t.Fatal("a refused handoff is terminal: nothing to withdraw")
		}
	}
}

// §6.7 "land a pulled document": the row, the received receipt and the cursor commit
// together; a re-pull updates, never duplicates; a stale page is ignored; a state
// change lands only at a higher sequence. Criterion 66's fail path: a different wire
// hash for a known id is refused and counted and the held row is unchanged.
func TestLandPulledHandoffByIDAndWireHash(t *testing.T) {
	ix := handoffDevice(t)
	rec, body := handoffRecord("Finish the drain", "the body", "usr_me")
	eff := landPage(t, ix, 0, 5, pulled(5, rec, body, teamwire.HandoffSent))
	if !eff.Moved || eff.Landed != 1 || eff.Receipts != 1 {
		t.Fatalf("first landing: %+v", eff)
	}
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if !h.ToMe || h.SentHere || h.PeerUserID != "usr_sender" || h.PeerName != "Sender" || h.Title != rec.Title || h.WireBody != string(body) || h.StateSeq != 5 {
		t.Fatalf("the recipient's copy: %+v", h)
	}
	if c, _ := ix.SyncCursor(HandoffCursorScope(handoffTestOrg)); c != 5 {
		t.Fatalf("cursor %d", c)
	}
	receipts := pendingOfKind(t, ix, OutboxHandoffReceipt)
	if len(receipts) != 1 {
		t.Fatalf("one received receipt: %+v", receipts)
	}
	var receipt teamwire.HandoffReceipt
	if err := json.Unmarshal([]byte(receipts[0].SentWireBody), &receipt); err != nil || receipt.Transition != teamwire.TransitionReceived || receipt.HandoffID != rec.ID {
		t.Fatalf("frozen receipt: %+v %v", receipt, err)
	}
	deviceID, _, _ := ix.Device()
	if receipt.ID != teamwire.ReceiptID(rec.ID, teamwire.TransitionReceived, deviceID, "", "") {
		t.Fatal("the receipt id is the deterministic one")
	}
	// A tick that read the old cursor lands nothing (the compare-and-set).
	if eff := landPage(t, ix, 0, 5, pulled(5, rec, body, teamwire.HandoffSent)); eff.Moved {
		t.Fatalf("a stale cursor lands nothing: %+v", eff)
	}
	// A replayed page from the current cursor: one handoff, one state, one receipt.
	if eff := landPage(t, ix, 5, 5, pulled(5, rec, body, teamwire.HandoffSent)); eff.Stale != 1 || eff.Receipts != 0 || eff.Landed != 0 {
		t.Fatalf("a replayed page is ignored: %+v", eff)
	}
	if n := len(pendingOfKind(t, ix, OutboxHandoffReceipt)); n != 1 {
		t.Fatalf("no duplicated receipt: %d", n)
	}
	// A state change at a higher sequence lands; one at a lower sequence does not.
	started := pulled(7, rec, nil, teamwire.HandoffStarted)
	started.Handoff.OpenedBy = &teamwire.HandoffOpenedBy{DeviceID: "dev_other", Session: teamwire.SessionIdentity{ID: "ses_o", Runtime: "codex", NativeID: "n2"}}
	if eff := landPage(t, ix, 5, 7, started); eff.Updated != 1 {
		t.Fatalf("state change: %+v", eff)
	}
	if eff := landPage(t, ix, 7, 7, pulled(6, rec, body, teamwire.HandoffReceived)); eff.Stale != 1 {
		t.Fatalf("a lower sequence is stale: %+v", eff)
	}
	h = mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if h.State != teamwire.HandoffStarted || h.StateSeq != 7 || h.OpenedBy == nil || h.OpenedBy.Session.Runtime != "codex" || h.WireBody != string(body) {
		t.Fatalf("after the state change: %+v", h)
	}
	// A different wire hash for the known id: refused, counted, the held row unchanged.
	forged, _ := handoffRecord("Another title", "another body", "usr_me")
	forged.ID = rec.ID
	forged.ContentHash = teamwire.HandoffWireHash(forged)
	forgedBody, _ := json.Marshal(forged)
	if eff := landPage(t, ix, 7, 9, pulled(9, forged, forgedBody, teamwire.HandoffOpened)); eff.HashConflicts != 1 || eff.Updated != 0 {
		t.Fatalf("hash conflict: %+v", eff)
	}
	after := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if after.Title != rec.Title || after.WireBody != string(body) || after.State != teamwire.HandoffStarted || after.StateSeq != 7 || after.HashConflicts != 1 {
		t.Fatalf("the held row is unchanged and the conflict counted: %+v", after)
	}
	if n, _ := ix.HandoffHashConflicts(handoffTestOrg); n != 1 {
		t.Fatalf("counted: %d", n)
	}
	// A row naming another document id is skipped and does not hold the cursor.
	wrong := pulled(11, rec, body, teamwire.HandoffSent)
	wrong.Handoff.ID = engine.NewTypedID(teamwire.HandoffIDPrefix)
	if eff := landPage(t, ix, 9, 11, wrong); eff.Unlandable != 1 || !eff.Moved {
		t.Fatalf("unlandable: %+v", eff)
	}
}

// Criterion 67, device half: landing a withdrawal erases the recipient's unopened copy;
// a copy a session already opened keeps its text. Criterion 78: a handoff that ended
// before it reached this device lands as the sender and the time only.
func TestLandWithdrawnAndNeverHeldRows(t *testing.T) {
	ix := handoffDevice(t)
	rec, body := handoffRecord("Finish the drain", "the body", "usr_me")
	landPage(t, ix, 0, 1, pulled(1, rec, body, teamwire.HandoffReceived))
	if eff := landPage(t, ix, 1, 2, pulled(2, rec, nil, teamwire.HandoffWithdrawn)); eff.Erased != 1 {
		t.Fatalf("a withdrawal erases the unopened copy: %+v", eff)
	}
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if h.State != teamwire.HandoffWithdrawn || h.WireBody != "" || h.Title != "" || h.HasBody || !h.Held {
		t.Fatalf("erased: %+v", h)
	}
	if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: teamwire.TransitionClosed, At: 300}); err == nil {
		t.Fatal("a withdrawn handoff offers nothing")
	} else if refusal := new(HandoffRefusal); !errors.As(err, &refusal) || refusal.Code != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("the refusal names the withdrawal: %v", err)
	}

	opened, openedBody := handoffRecord("Opened one", "opened body", "usr_me")
	landPage(t, ix, 2, 3, pulled(3, opened, openedBody, teamwire.HandoffOpened))
	if eff := landPage(t, ix, 3, 4, pulled(4, opened, nil, teamwire.HandoffWithdrawn)); eff.Erased != 0 {
		t.Fatalf("an opened copy is not erased: %+v", eff)
	}
	if h := mustHandoff(t, ix, handoffTestOrg, opened.ID); h.State != teamwire.HandoffWithdrawn || h.WireBody != string(openedBody) {
		t.Fatalf("a session that was confirmed keeps its text: %+v", h)
	}

	// In sequence order: a page lands only from the cursor it was read at.
	for index, state := range []string{teamwire.HandoffExpired, teamwire.HandoffWithdrawn} {
		seq := int64(5 + index)
		never, _ := handoffRecord("never seen", "never seen body", "usr_me")
		row := pulled(seq, never, nil, state)
		row.Handoff.WireHash = ""
		eff, err := ix.LandHandoffPage(HandoffPage{OrganizationID: handoffTestOrg, Rows: []teamwire.PullRow{row}, From: seq - 1, To: seq, At: 200})
		if err != nil || eff.Landed != 1 || eff.Receipts != 0 {
			t.Fatalf("%s never held: %+v %v", state, eff, err)
		}
		h := mustHandoff(t, ix, handoffTestOrg, never.ID)
		if h.State != state || h.Title != "" || h.WireBody != "" || h.PeerName != "Sender" || h.CreatedAt == 0 || !h.ToMe || h.Held {
			t.Fatalf("%s before it reached this device: the sender and the time only: %+v", state, h)
		}
	}
}

// A handoff another of the recipient's devices opened, landing here after the team
// server erased its text: the row has no document and was never held, its state is
// still opened, Open is refused for want of text — as a check on the row and inside
// the Open's own transaction — and Close is the one thing it offers.
func TestLandOpenedRowWithoutDocumentOffersNoOpen(t *testing.T) {
	ix := handoffDevice(t)
	rec, _ := handoffRecord("opened elsewhere", "text the server erased", "usr_me")
	row := pulled(1, rec, nil, teamwire.HandoffOpened)
	row.Handoff.WireHash = ""
	if eff := landPage(t, ix, 0, 1, row); eff.Landed != 1 || eff.Receipts != 0 {
		t.Fatalf("an opened row with no document lands and sends no received: %+v", eff)
	}
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if h.State != teamwire.HandoffOpened || h.Held || h.HasBody || h.WireBody != "" || h.Title != "" || !h.ToMe {
		t.Fatalf("never held, still opened: %+v", h)
	}
	if refusal := HandoffOpenRefusal(h); refusal == nil || refusal.Code != HandoffCodeNoText {
		t.Fatalf("no document, no Open: %+v", refusal)
	}
	if offers := HandoffOffers(h); len(offers) != 1 || offers[0] != teamwire.TransitionClosed {
		t.Fatalf("an opened handoff offers Close and nothing else: %v", offers)
	}
	_, err := ix.CreateHandoffOpen(HandoffOpenCreate{OrganizationID: handoffTestOrg, HandoffID: rec.ID, Runtime: "codex", CheckoutRoot: t.TempDir(), At: 300})
	if refusal := new(HandoffRefusal); !errors.As(err, &refusal) || refusal.Code != HandoffCodeNoText {
		t.Fatalf("an Open of a row with no document writes nothing: %v", err)
	}
}

// §6.2's table: the originating device of a self-send holds both facts and sends no
// received; the person's other device is a recipient's device; the sender's other
// device holds neither fact, no text, and may withdraw.
func TestHandoffDevicePartsAndSelfSend(t *testing.T) {
	origin := handoffDevice(t)
	rec, body := sendTestHandoff(t, origin, handoffTestOrg, "usr_me", true)
	self := pulled(3, rec, body, teamwire.HandoffSent)
	self.Handoff.FromMe, self.Handoff.SenderUserID, self.Handoff.SenderName = true, "usr_me", "Me"
	if eff := landPage(t, origin, 0, 3, self); eff.Updated != 1 || eff.Receipts != 0 {
		t.Fatalf("the originating device updates its row and sends no received: %+v", eff)
	}
	h := mustHandoff(t, origin, handoffTestOrg, rec.ID)
	if !h.SentHere || !h.ToMe || h.State != teamwire.HandoffSent {
		t.Fatalf("both facts: %+v", h)
	}
	for _, transition := range []string{teamwire.TransitionDeclined, teamwire.TransitionClosed} {
		if _, err := origin.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: transition, At: 5}); err == nil {
			t.Fatalf("the originating device of a self-send never offers %s", transition)
		}
	}

	second := handoffDevice(t)
	if eff := landPage(t, second, 0, 3, self); eff.Landed != 1 || eff.Receipts != 1 {
		t.Fatalf("the person's other device is a recipient's device: %+v", eff)
	}
	if h := mustHandoff(t, second, handoffTestOrg, rec.ID); !h.ToMe || h.SentHere || h.WireBody == "" {
		t.Fatalf("other device: %+v", h)
	}

	senderOther := handoffDevice(t)
	other, _ := handoffRecord("not held", "not held", "usr_teammate")
	row := teamwire.PullRow{Seq: 4, Kind: teamwire.KindHandoff, Handoff: &teamwire.PulledHandoff{ID: other.ID, FromMe: true,
		SenderUserID: "usr_me", SenderName: "Me", RecipientUserID: "usr_teammate", RecipientName: "Teammate",
		State: teamwire.HandoffReceived, CreatedAt: "2026-10-04T16:00:00Z", StateAt: "2026-10-04T16:00:05Z"}}
	if eff := landPage(t, senderOther, 0, 4, row); eff.Landed != 1 || eff.Receipts != 0 {
		t.Fatalf("the sender's other device: %+v", eff)
	}
	h = mustHandoff(t, senderOther, handoffTestOrg, other.ID)
	if h.SentHere || h.ToMe || !h.FromMe || h.Title != "" || h.WireBody != "" || h.PeerName != "Teammate" {
		t.Fatalf("neither flag, from_me, no title and no text: %+v", h)
	}
	if _, err := senderOther.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: other.ID, Transition: teamwire.TransitionDeclined, At: 5}); err == nil {
		t.Fatal("a sender's device cannot decline")
	}
	h, err := senderOther.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: other.ID, Transition: teamwire.TransitionWithdrawn, At: 6})
	if err != nil || h.State != teamwire.HandoffWithdrawn {
		t.Fatalf("it may withdraw: %+v %v", h, err)
	}
	receipts := pendingOfKind(t, senderOther, OutboxHandoffReceipt)
	if len(receipts) != 1 || !strings.Contains(receipts[0].SentWireBody, `"transition":"withdrawn"`) {
		t.Fatalf("a withdrawn receipt is queued: %+v", receipts)
	}
}

// §6.2 "when the console offers each", and a rejected receipt landing the state the
// server folded first.
func TestHandoffTransitionsFollowTheOfferRules(t *testing.T) {
	ix := handoffDevice(t)
	rec, body := handoffRecord("t", "b", "usr_me")
	landPage(t, ix, 0, 1, pulled(1, rec, body, teamwire.HandoffReceived))
	act := func(transition string) error {
		_, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: transition, At: 300})
		return err
	}
	if err := act(teamwire.TransitionClosed); err == nil {
		t.Fatal("close is offered only once started or opened")
	}
	if err := act(teamwire.TransitionWithdrawn); err == nil {
		t.Fatal("a recipient cannot withdraw")
	}
	landPage(t, ix, 1, 2, pulled(2, rec, nil, teamwire.HandoffStarted))
	if err := act(teamwire.TransitionDeclined); err == nil {
		t.Fatal("decline is offered only while received")
	}
	if err := act(teamwire.TransitionClosed); err != nil {
		t.Fatal(err)
	}
	if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffClosed {
		t.Fatalf("closed: %+v", h)
	}
	if err := act(teamwire.TransitionClosed); err == nil {
		t.Fatal("a second close is refused: one handoff, one state")
	}
	// The server folded the sender's withdrawal first and rejects the close.
	var closing OutboxRow
	for _, r := range pendingOfKind(t, ix, OutboxHandoffReceipt) {
		if strings.Contains(r.SentWireBody, `"transition":"closed"`) {
			closing = r
		}
	}
	out, err := ix.SettleHandoffPush(HandoffSettle{Seq: closing.Seq, Code: teamwire.CodeHandoffWithdrawn}, 310)
	if err != nil || out.State != teamwire.HandoffWithdrawn || out.Transition != teamwire.TransitionClosed {
		t.Fatalf("a rejected receipt lands the server's state: %+v %v", out, err)
	}
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if h.ReceiptCode != teamwire.CodeHandoffWithdrawn || h.State != teamwire.HandoffWithdrawn {
		t.Fatalf("the code is kept for the person: %+v", h)
	}
	// A decline that reaches the server after a session started is rejected
	// handoff_started: the item is started, as the server holds it.
	late, lateBody := handoffRecord("late", "late body", "usr_me")
	landPage(t, ix, 2, 3, pulled(3, late, lateBody, teamwire.HandoffReceived))
	if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: late.ID, Transition: teamwire.TransitionDeclined, At: 320}); err != nil {
		t.Fatal(err)
	}
	for _, r := range pendingOfKind(t, ix, OutboxHandoffReceipt) {
		if strings.Contains(r.SentWireBody, late.ID) && strings.Contains(r.SentWireBody, `"transition":"declined"`) {
			if out, err := ix.SettleHandoffPush(HandoffSettle{Seq: r.Seq, Code: teamwire.CodeHandoffStarted}, 330); err != nil || out.State != teamwire.HandoffStarted {
				t.Fatalf("handoff_started lands started: %+v %v", out, err)
			}
		}
	}
	if h := mustHandoff(t, ix, handoffTestOrg, late.ID); h.State != teamwire.HandoffStarted || h.ReceiptCode != teamwire.CodeHandoffStarted || h.WireBody == "" {
		t.Fatalf("the item is started, the code shown, the text kept: %+v", h)
	}
	if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: "hnd_missing", Transition: teamwire.TransitionClosed}); !errors.Is(err, ErrHandoffNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// §6.6 local handoff: a row with an empty organization id, no outbox row, no receipt,
// written by a device that is not linked.
func TestLocalHandoffWritesNoOutboxRowAndWorksUnlinked(t *testing.T) {
	ix := openResultTestIndex(t)
	rec, body := handoffRecord("local", "local body", "")
	if err := ix.SendHandoff(HandoffSend{Record: rec, Body: body, At: 5}); err != nil {
		t.Fatal(err)
	}
	h := mustHandoff(t, ix, "", rec.ID)
	if !h.Local() || !h.ToMe || !h.SentHere || h.State != teamwire.HandoffReceived {
		t.Fatalf("local row: %+v", h)
	}
	if n, _ := ix.OutboxPending(); n != 0 {
		t.Fatalf("no outbox row: %d", n)
	}
	if _, err := ix.TransitionHandoff(HandoffTransition{ID: rec.ID, Transition: teamwire.TransitionDeclined, At: 6}); err != nil {
		t.Fatal(err)
	}
	if n, _ := ix.OutboxPending(); n != 0 {
		t.Fatalf("no receipt for a local handoff: %d", n)
	}
	team, _ := handoffRecord("team", "team body", "usr_x")
	teamBody, _ := json.Marshal(team)
	if err := ix.SendHandoff(HandoffSend{OrganizationID: handoffTestOrg, Record: team, Body: teamBody, At: 5}); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("a team send needs a link: %v", err)
	}
}

// Criterion 68, the rows and lists: after unlink the rows stay readable and nothing is
// sent; a relink to the same organization takes them back; linked elsewhere, the
// earlier organization's handoffs and members are not listed.
func TestUnlinkMarksHandoffsAndRelinkElsewhereHidesThem(t *testing.T) {
	ix := handoffDevice(t)
	rec, body := handoffRecord("received one", "received body", "usr_me")
	if _, err := ix.LandHandoffPage(HandoffPage{OrganizationID: handoffTestOrg, Rows: []teamwire.PullRow{pulled(1, rec, body, teamwire.HandoffReceived)}, From: 0, To: 1, Drained: true, At: 200}); err != nil {
		t.Fatal(err)
	}
	if done, _ := ix.HandoffBootstrapped(handoffTestOrg); !done {
		t.Fatal("a drained page records that the first handoff pull is done")
	}
	queued, queuedBody := sendTestHandoff(t, ix, handoffTestOrg, "usr_teammate", false)
	local, localBody := handoffRecord("local", "local body", "")
	if err := ix.SendHandoff(HandoffSend{Record: local, Body: localBody, At: 5}); err != nil {
		t.Fatal(err)
	}
	if err := ix.ReplaceTeamMembers(handoffTestOrg, []teamwire.Member{{UserID: "usr_teammate", DisplayName: "Teammate"}}, "usr_me", 9); err != nil {
		t.Fatal(err)
	}
	// The unlink path: the flag, then the queue reset.
	if err := ix.SetLinked(false); err != nil {
		t.Fatal(err)
	}
	if err := ix.ResetSyncQueue(); err != nil {
		t.Fatal(err)
	}
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if !h.LinkEnded || h.WireBody != string(body) || h.State != teamwire.HandoffReceived {
		t.Fatalf("a received handoff stays readable, marked as the ended link's: %+v", h)
	}
	if q := mustHandoff(t, ix, handoffTestOrg, queued.ID); q.State != HandoffRefused || q.RefusalCode != HandoffCodeLinkEnded || q.WireBody != string(queuedBody) {
		t.Fatalf("a queued send can no longer leave: refused by name, text kept: %+v", q)
	}
	if n, _ := ix.OutboxPending(); n != 0 {
		t.Fatalf("unsent rows go with the queue reset: %d", n)
	}
	if c, _ := ix.SyncCursor(HandoffCursorScope(handoffTestOrg)); c != 0 {
		t.Fatalf("the handoff cursor is forgotten: %d", c)
	}
	if done, _ := ix.HandoffBootstrapped(handoffTestOrg); done {
		t.Fatal("the first-pull mark is reset with the link")
	}
	if members, _ := ix.TeamMembers(handoffTestOrg); len(members) != 0 {
		t.Fatalf("the directory is forgotten: %+v", members)
	}
	if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: teamwire.TransitionDeclined, At: 20}); err == nil {
		t.Fatal("nothing is sent for a row of an ended link")
	}
	unlinked, _ := ix.ListHandoffs("")
	if len(unlinked) != 3 {
		t.Fatalf("unlinked, everything on the device is listed: %d", len(unlinked))
	}
	// Linked elsewhere: only the local row.
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetLinkedOrganization("org_2", 30); err != nil {
		t.Fatal(err)
	}
	elsewhere, _ := ix.ListHandoffs("org_2")
	if len(elsewhere) != 1 || elsewhere[0].ID != local.ID {
		t.Fatalf("the earlier organization's handoffs are not listed: %+v", elsewhere)
	}
	if _, found, _ := ix.FindHandoff("org_2", rec.ID); found {
		t.Fatal("nor reachable by id")
	}
	if err := ix.ReplaceTeamMembers(handoffTestOrg, nil, "", 31); !errors.Is(err, ErrLinkChanged) {
		t.Fatalf("a refresh for an organization that is not the link's writes nothing: %v", err)
	}
	// Back to the first organization: its rows are this link's again.
	if err := ix.SetLinkedOrganization(handoffTestOrg, 40); err != nil {
		t.Fatal(err)
	}
	if err := ix.ReclaimHandoffs(handoffTestOrg); err != nil {
		t.Fatal(err)
	}
	if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.LinkEnded {
		t.Fatalf("a relink to the same organization takes its rows back: %+v", h)
	}
	if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: teamwire.TransitionDeclined, At: 41}); err != nil {
		t.Fatal(err)
	}
}

// Criterion 79: the directory holds a user id and a display name; a refresh replaces
// it, so a removed member is absent.
func TestTeamMembersReplaceWhole(t *testing.T) {
	ix := handoffDevice(t)
	if err := ix.ReplaceTeamMembers(handoffTestOrg, []teamwire.Member{{UserID: "usr_a", DisplayName: "Ada"}, {UserID: "usr_b", DisplayName: "Bo"}}, "usr_a", 10); err != nil {
		t.Fatal(err)
	}
	members, _ := ix.TeamMembers(handoffTestOrg)
	if len(members) != 2 || !members[0].Self || members[0].DisplayName != "Ada" || members[1].Self {
		t.Fatalf("directory: %+v", members)
	}
	if err := ix.ReplaceTeamMembers(handoffTestOrg, []teamwire.Member{{UserID: "usr_a", DisplayName: "Ada"}}, "usr_a", 20); err != nil {
		t.Fatal(err)
	}
	if members, _ := ix.TeamMembers(handoffTestOrg); len(members) != 1 || members[0].UserID != "usr_a" || members[0].RefreshedAt != 20 {
		t.Fatalf("a removed member is absent at the next refresh: %+v", members)
	}
	cols, err := columnSet(ix.db, "team_member")
	if err != nil {
		t.Fatal(err)
	}
	for name := range cols {
		switch name {
		case "organization_id", "user_id", "display_name", "is_self", "refreshed_at":
		default:
			t.Fatalf("the directory holds a user id and a display name and nothing else about a member; found column %s", name)
		}
	}
}

// Criterion 67, device half: a started receipt for a handoff the sender already
// withdrew is refused by the server; the device lands the withdrawal — the unopened
// copy is erased — and keeps the code that says the sender withdrew. The same seam the
// open lane enqueues started and opened through.
func TestRefusedStartedReceiptLandsTheWithdrawal(t *testing.T) {
	ix := handoffDevice(t)
	rec, body := handoffRecord("t", "b", "usr_me")
	landPage(t, ix, 0, 1, pulled(1, rec, body, teamwire.HandoffReceived))
	session := &teamwire.SessionIdentity{ID: "ses_new", Runtime: "codex", NativeID: "thread-1"}
	enqueue := func() bool {
		t.Helper()
		tx, err := ix.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		wrote, err := ix.EnqueueHandoffReceiptTx(tx, HandoffReceiptInput{OrganizationID: handoffTestOrg, HandoffID: rec.ID,
			Transition: teamwire.TransitionStarted, TicketID: "tkt_01J8ZQ4M7T2V9K3NXW5R6YHBCM", Session: session, At: 400})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return wrote
	}
	if !enqueue() {
		t.Fatal("the started receipt is queued")
	}
	if enqueue() {
		t.Fatal("the same receipt is never queued twice: a retry is the frozen row, byte-identical")
	}
	var started OutboxRow
	for _, r := range pendingOfKind(t, ix, OutboxHandoffReceipt) {
		if strings.Contains(r.SentWireBody, `"transition":"started"`) {
			started = r
		}
	}
	var receipt teamwire.HandoffReceipt
	if err := json.Unmarshal([]byte(started.SentWireBody), &receipt); err != nil || receipt.TicketID == "" || receipt.Session == nil ||
		receipt.Session.NativeID != "thread-1" || receipt.Session.Runtime != "codex" {
		t.Fatalf("started carries the ticket and the session identity: %+v %v", receipt, err)
	}
	deviceID, _, _ := ix.Device()
	if receipt.ID != teamwire.ReceiptID(rec.ID, teamwire.TransitionStarted, deviceID, receipt.TicketID, "thread-1") {
		t.Fatal("the receipt id covers the ticket and the native session id")
	}
	out, err := ix.SettleHandoffPush(HandoffSettle{Seq: started.Seq, Code: teamwire.CodeHandoffWithdrawn}, 410)
	if err != nil || out.State != teamwire.HandoffWithdrawn {
		t.Fatalf("a refused started lands the withdrawal: %+v %v", out, err)
	}
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if h.ReceiptCode != teamwire.CodeHandoffWithdrawn || h.WireBody != "" || h.Title != "" {
		t.Fatalf("the device says the sender withdrew and holds no text: %+v", h)
	}
}

// Criterion 65, device half: a handoff's text is in the handoff row's own columns and
// the outbox's frozen body, and in no other column of any table.
func TestHandoffTextLivesOnlyInItsOwnColumns(t *testing.T) {
	const planted = "ZEBRA-PLANTED-7731"
	ix := handoffDevice(t)
	rec, _ := handoffRecord("Title "+planted, "Body "+planted, "usr_teammate")
	rec.Remaining = []string{"Remaining " + planted}
	rec.Conversation = &teamwire.HandoffConversation{Turns: []teamwire.HandoffTurn{{Seq: 1, Role: "user", Text: "Turn " + planted}}}
	rec.ContentHash = teamwire.HandoffWireHash(rec)
	body, _ := json.Marshal(rec)
	if err := ix.SendHandoff(HandoffSend{OrganizationID: handoffTestOrg, Record: rec, Body: body, PeerName: "Teammate", At: 100}); err != nil {
		t.Fatal(err)
	}
	in, inBody := handoffRecord("Pulled "+planted, "Pulled body "+planted, "usr_me")
	landPage(t, ix, 0, 1, pulled(1, in, inBody, teamwire.HandoffReceived))
	if _, err := ix.SettleHandoffPush(HandoffSettle{Seq: pendingOfKind(t, ix, OutboxHandoff)[0].Seq, Code: teamwire.CodeRateLimited}, 150); err != nil {
		t.Fatal(err)
	}
	// Claim and arm first: the brief is written to the ticket and to a delivery row.
	ticket := openTicket(t, ix, handoffTestOrg, in.ID, "claude")
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.ClaimHandoffTicketTx(tx, HandoffClaim{TicketID: ticket.TicketID, Runtime: "claude", NativeID: "ses-a",
		Transcript: "/t/ses-a.jsonl", BriefText: "Brief " + planted, At: 155, Policy: briefPolicy}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: in.ID, Transition: teamwire.TransitionClosed, At: 160}); err != nil {
		t.Fatal(err)
	}
	// The brief a claim arms is the handoff's text too: it lives in the ticket's own
	// column and in the delivery row it is handed over from, and nowhere else.
	allowed := map[string]bool{"handoff.wire_body": true, "handoff.title": true, "sync_outbox.sent_wire_body": true,
		"handoff_open.brief_text": true, "session_delivery.message": true}
	found := columnsHolding(t, ix, planted)
	for place := range found {
		if !allowed[place] {
			t.Fatalf("handoff text was written to %s", place)
		}
	}
	for place := range allowed {
		if !found[place] {
			t.Fatalf("the scan did not find the text where it belongs (%s): the scan is not looking", place)
		}
	}
}

// columnsHolding scans every column of every table for a planted marker and returns
// the table.column places that hold it.
func columnsHolding(t *testing.T, ix *Index, planted string) map[string]bool {
	t.Helper()
	tables, err := ix.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = tables.Close()
	found := map[string]bool{}
	for _, table := range names {
		cols, err := columnSet(ix.db, table)
		if err != nil || len(cols) == 0 {
			continue // a virtual table's shadow layout: not a place a handoff is written
		}
		for col := range cols {
			var n int
			if err := ix.db.QueryRow(`SELECT count(*) FROM "`+table+`" WHERE CAST("`+col+`" AS TEXT) LIKE ?`, "%"+planted+"%").Scan(&n); err != nil {
				continue
			}
			if n > 0 {
				found[table+"."+col] = true
			}
		}
	}
	return found
}

// Red-team M6: a withdrawal or an expiry takes the text back from a recipient's copy
// that was never opened — all of it. The brief a claim armed is that text too, in the
// ticket's row and in the delivery row; both were left behind. A row already handed
// over (and never confirmed) is blanked as well. An opened copy keeps everything.
func TestErasureTakesTheArmedBriefWithTheText(t *testing.T) {
	const planted = "YAK-PLANTED-4402"
	claimWithBrief := func(t *testing.T, ix *Index, ticketID, session string) {
		t.Helper()
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ix.ClaimHandoffTicketTx(tx, HandoffClaim{TicketID: ticketID, Runtime: "claude", NativeID: session,
			BriefText: "Brief " + planted, At: 400, Policy: briefPolicy}); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{teamwire.HandoffWithdrawn, teamwire.HandoffExpired} {
		t.Run(state, func(t *testing.T) {
			ix := handoffDevice(t)
			rec, body := handoffRecord("Title "+planted, "Body "+planted, "usr_me")
			landPage(t, ix, 0, 1, pulled(1, rec, body, teamwire.HandoffReceived))
			armed := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
			claimWithBrief(t, ix, armed.TicketID, "ses-armed")
			handed := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
			claimWithBrief(t, ix, handed.TicketID, "ses-handed")
			if got := handOver(t, ix, "claude", "ses-handed", "trn_1", 410); len(got) != 1 {
				t.Fatalf("handed over: %+v", got)
			}
			if before := columnsHolding(t, ix, planted); !before["handoff_open.brief_text"] || !before["session_delivery.message"] {
				t.Fatalf("the brief is armed before the erasure: %v", before)
			}
			landPage(t, ix, 1, 2, pulled(2, rec, nil, state))
			if left := columnsHolding(t, ix, planted); len(left) != 0 {
				t.Fatalf("the text survived its erasure in %v", left)
			}
		})
	}
	t.Run("an opened copy keeps its text", func(t *testing.T) {
		ix := handoffDevice(t)
		rec, body := handoffRecord("Title "+planted, "Body "+planted, "usr_me")
		landPage(t, ix, 0, 1, pulled(1, rec, body, teamwire.HandoffReceived))
		ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
		claimWithBrief(t, ix, ticket.TicketID, "ses-a")
		handOver(t, ix, "claude", "ses-a", "trn_1", 410)
		if confirmed, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-a", 411); err != nil || !confirmed.Receipt {
			t.Fatalf("opened: %+v %v", confirmed, err)
		}
		landPage(t, ix, 1, 2, pulled(2, rec, nil, teamwire.HandoffWithdrawn))
		if kept := columnsHolding(t, ix, planted); !kept["handoff.wire_body"] || !kept["handoff_open.brief_text"] {
			t.Fatalf("an opened copy was erased: %v", kept)
		}
	})
}

// Red-team M5: a pull never writes the server's older, non-terminal state over what
// this device holds. An opened row stays opened, so a later withdrawal does not erase
// a copy a session holds; a local decline stays declined while its receipt is on its
// way; and a decline the server rejected gives way to the server's state, after which
// declining again sends the receipt again instead of telling the server nothing.
func TestAPullNeverLowersOpenedOrALocalTerminalState(t *testing.T) {
	declineReceipts := func(t *testing.T, ix *Index) []OutboxRow {
		t.Helper()
		var out []OutboxRow
		for _, row := range pendingOfKind(t, ix, OutboxHandoffReceipt) {
			if strings.Contains(row.SentWireBody, `"transition":"declined"`) {
				out = append(out, row)
			}
		}
		return out
	}
	t.Run("opened is never lowered", func(t *testing.T) {
		ix := handoffDevice(t)
		rec, body := receivedHandoff(t, ix, 1)
		ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
		claimTicket(t, ix, ticket.TicketID, "claude", "ses-a", 400)
		handOver(t, ix, "claude", "ses-a", "trn_1", 410)
		if confirmed, err := ix.ConfirmHandoffBrief(ticket.TicketID, "claude", "ses-a", 411); err != nil || !confirmed.Receipt {
			t.Fatalf("opened: %+v %v", confirmed, err)
		}
		// The server's fold is behind: it serves started at a higher sequence.
		landPage(t, ix, 1, 2, pulled(2, rec, body, teamwire.HandoffStarted))
		if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffOpened || h.StateSeq != 2 {
			t.Fatalf("a pull lowered an opened row: state=%s seq=%d", h.State, h.StateSeq)
		}
		landPage(t, ix, 2, 3, pulled(3, rec, nil, teamwire.HandoffWithdrawn))
		if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffWithdrawn || h.WireBody == "" || h.Title == "" {
			t.Fatalf("a withdrawal after the open erased a copy a session holds: %+v", h)
		}
	})
	t.Run("a local decline stands while its receipt is unanswered, and after it is accepted", func(t *testing.T) {
		ix := handoffDevice(t)
		rec, body := receivedHandoff(t, ix, 1)
		ticket := openTicket(t, ix, handoffTestOrg, rec.ID, "claude")
		if _, err := ix.TransitionHandoff(HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: teamwire.TransitionDeclined, At: 300}); err != nil {
			t.Fatal(err)
		}
		landPage(t, ix, 1, 2, pulled(2, rec, body, teamwire.HandoffReceived))
		if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffDeclined {
			t.Fatalf("a pull reverted a decline whose receipt is unanswered: %s", h.State)
		}
		if got := mustTicket(t, ix, ticket.TicketID); got.State != HandoffOpenCancelled {
			t.Fatalf("the ticket of a declined handoff: %+v", got)
		}
		receipts := declineReceipts(t, ix)
		if len(receipts) != 1 {
			t.Fatalf("one decline receipt is queued: %d", len(receipts))
		}
		if _, err := ix.SettleHandoffPush(HandoffSettle{Seq: receipts[0].Seq, Code: teamwire.StatusAccepted}, 310); err != nil {
			t.Fatal(err)
		}
		landPage(t, ix, 2, 3, pulled(3, rec, body, teamwire.HandoffReceived))
		if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffDeclined {
			t.Fatalf("a pull reverted an accepted decline: %s", h.State)
		}
	})
	t.Run("a rejected decline follows the server and can be sent again", func(t *testing.T) {
		ix := handoffDevice(t)
		rec, body := receivedHandoff(t, ix, 1)
		decline := HandoffTransition{OrganizationID: handoffTestOrg, ID: rec.ID, Transition: teamwire.TransitionDeclined, At: 300}
		if _, err := ix.TransitionHandoff(decline); err != nil {
			t.Fatal(err)
		}
		first := declineReceipts(t, ix)
		if len(first) != 1 {
			t.Fatalf("one decline receipt is queued: %d", len(first))
		}
		if _, err := ix.SettleHandoffPush(HandoffSettle{Seq: first[0].Seq, Code: "not_ready"}, 310); err != nil {
			t.Fatal(err)
		}
		landPage(t, ix, 1, 2, pulled(2, rec, body, teamwire.HandoffReceived))
		if h := mustHandoff(t, ix, handoffTestOrg, rec.ID); h.State != teamwire.HandoffReceived {
			t.Fatalf("the server never held the decline, so the row follows the server: %s", h.State)
		}
		decline.At = 400
		if h, err := ix.TransitionHandoff(decline); err != nil || h.State != teamwire.HandoffDeclined {
			t.Fatalf("declining again: %+v %v", h, err)
		}
		again := declineReceipts(t, ix)
		if len(again) != 1 || again[0].Seq == first[0].Seq || again[0].SentWireBody != first[0].SentWireBody {
			t.Fatalf("the second decline must queue the same receipt again, byte for byte: %+v", again)
		}
	})
}

// Journey rig: a recipient's device that lands a handoff's document holds it, so on
// that device the handoff IS received at once — it does not wait for the server to
// fold this device's own received receipt and serve the state back (commit 841ca56c
// changed ensureReceivedReceiptTx to say so, and nothing pinned it). The sender's own
// device, a self-send's originating device and a terminal row are not moved.
func TestARecipientDeviceLandingTheDocumentReadsItReceivedAtOnce(t *testing.T) {
	ix := handoffDevice(t)
	rec, body := handoffRecord("Finish the drain", "the body", "usr_me")
	eff := landPage(t, ix, 0, 1, pulled(1, rec, body, teamwire.HandoffSent))
	if eff.Landed != 1 || eff.Receipts != 1 {
		t.Fatalf("the document lands and its received receipt is queued: %+v", eff)
	}
	h := mustHandoff(t, ix, handoffTestOrg, rec.ID)
	if h.State != teamwire.HandoffReceived || !h.Held || h.WireBody == "" {
		t.Fatalf("a device that holds the document reads it received in the same transaction, not sent: state=%s held=%v", h.State, h.Held)
	}
	if offers := HandoffOffers(h); len(offers) != 1 || offers[0] != teamwire.TransitionDeclined {
		t.Fatalf("a received handoff offers Decline at once: %v", offers)
	}

	// A row this device sent from another device carries no document: it stays sent.
	other, _ := handoffRecord("Mine from elsewhere", "b", "usr_teammate")
	row := pulled(2, other, nil, teamwire.HandoffSent)
	row.Handoff.ToMe, row.Handoff.FromMe = false, true
	landPage(t, ix, 1, 2, row)
	if h := mustHandoff(t, ix, handoffTestOrg, other.ID); h.State != teamwire.HandoffSent {
		t.Fatalf("a device that holds no document does not read it received: %s", h.State)
	}

	// A handoff that was withdrawn before it reached this device is not moved either.
	gone, goneBody := handoffRecord("Withdrawn", "b", "usr_me")
	landPage(t, ix, 2, 3, pulled(3, gone, goneBody, teamwire.HandoffWithdrawn))
	if h := mustHandoff(t, ix, handoffTestOrg, gone.ID); h.State != teamwire.HandoffWithdrawn {
		t.Fatalf("a terminal row stays terminal: %s", h.State)
	}
}
