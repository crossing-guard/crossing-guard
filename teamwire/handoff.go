package teamwire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"crossing-guard/engine"
)

// Handoff between members (team rest-of-release plan §6). A handoff is one immutable
// document (handoff.schema.json 1.1) the sender's device pushes; everything that
// changes afterwards is an immutable receipt (handoff-receipt.schema.json 1.0). The
// server folds receipts into one delivery row per handoff and serves each device the
// rows of its own user, on a per-organization sequence that is NOT the memory
// sequence: the two streams have different visibility (§6.6).

// Wire schema versions this package encodes.
const (
	HandoffSchemaVersion        = "1.1"
	HandoffReceiptSchemaVersion = "1.0"
)

// Push kinds. An older server answers both unsupported_kind and the drain parks them.
const (
	KindHandoff        = "handoff"
	KindHandoffReceipt = "handoff_receipt"
)

// RouteMembers is the member directory a device reads: user id and display name only
// (OD-10). The handoff pull is RoutePull with kinds ["handoff"] and its own cursor.
const RouteMembers = "/sync/v1/members"

// Typed-id prefixes minted for handoffs.
const (
	HandoffIDPrefix = "hnd"
	ReceiptIDPrefix = "rcp"
	TicketIDPrefix  = "tkt"
)

// Receipt transitions (§6.2). Who may send each is the server's rule: received,
// started, opened, declined and closed come from a device of the recipient; withdrawn
// from a device of the sender.
const (
	TransitionReceived  = "received"
	TransitionStarted   = "started"
	TransitionOpened    = "opened"
	TransitionDeclined  = "declined"
	TransitionClosed    = "closed"
	TransitionWithdrawn = "withdrawn"
)

// Delivery states of the fold. sent is the state before any receipt; expired is set by
// the server's retention sweep, never by a receipt.
const (
	HandoffSent      = "sent"
	HandoffReceived  = "received"
	HandoffStarted   = "started"
	HandoffOpened    = "opened"
	HandoffDeclined  = "declined"
	HandoffClosed    = "closed"
	HandoffWithdrawn = "withdrawn"
	HandoffExpired   = "expired"
)

// HandoffTerminal reports whether state ends a handoff: the first terminal receipt by
// sequence wins and every later receipt is rejected with that state's code.
func HandoffTerminal(state string) bool {
	switch state {
	case HandoffDeclined, HandoffClosed, HandoffWithdrawn, HandoffExpired:
		return true
	}
	return false
}

// Answer codes for a pushed handoff (§6.6) and a pushed receipt (§6.2). A receipt that
// arrives after a terminal state is rejected with handoff_<state>.
const (
	CodeUnknownRecipient   = "unknown_recipient"
	CodeRecipientInactive  = "recipient_inactive"
	CodeRecipientInboxFull = "recipient_inbox_full"
	CodeRateLimited        = "rate_limited"
	CodeOverCap            = "over_cap"
	CodeUnknownHandoff     = "unknown_handoff"
	CodeNotRecipient       = "not_recipient"
	CodeNotSender          = "not_sender"
	// CodeHandoffStarted answers a decline that arrives after a session started for
	// the handoff: decline is offered only while the item is received (§6.2).
	CodeHandoffStarted   = "handoff_started"
	CodeHandoffDeclined  = "handoff_declined"
	CodeHandoffClosed    = "handoff_closed"
	CodeHandoffWithdrawn = "handoff_withdrawn"
	CodeHandoffExpired   = "handoff_expired"
)

// HandoffTerminalCode is the rejection code for a receipt that arrives after state.
func HandoffTerminalCode(state string) string {
	return map[string]string{HandoffDeclined: CodeHandoffDeclined, HandoffClosed: CodeHandoffClosed,
		HandoffWithdrawn: CodeHandoffWithdrawn, HandoffExpired: CodeHandoffExpired}[state]
}

// HandoffRecipient is the typed recipient: a server user id from the member directory.
type HandoffRecipient struct {
	UserID string `json:"user_id"`
}

// HandoffAgent references a shared agent attached to the sender's session. Never a body.
type HandoffAgent struct {
	ProfileID    string `json:"profile_id"`
	Name         string `json:"name"`
	SourceDigest string `json:"source_digest"`
	BundleDigest string `json:"bundle_digest"`
}

// HandoffTurn is one turn of the optional excerpt.
type HandoffTurn struct {
	Seq  int    `json:"seq"`
	Role string `json:"role"`
	Text string `json:"text"`
}

// HandoffConversation is the excerpt the sender ticked. Absent unless ticked.
type HandoffConversation struct {
	Turns     []HandoffTurn `json:"turns"`
	Truncated bool          `json:"truncated"`
}

// HandoffTag is one declared governance tag.
type HandoffTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// HandoffGovernance is the sender session's declared governance state.
type HandoffGovernance struct {
	WaterMark string       `json:"water_mark"`
	Tags      []HandoffTag `json:"tags"`
}

// HandoffRecord is schemas/handoff.schema.json 1.1 — the document as it leaves the
// sender's device, after the portable-text and secret checks. ContentHash is
// HandoffWireHash of this record.
type HandoffRecord struct {
	SchemaVersion   string               `json:"schema_version"`
	ID              string               `json:"id"`
	Session         SessionIdentity      `json:"session"`
	RepositoryID    *string              `json:"repository_id"`
	CreatedAt       string               `json:"created_at"`
	CreatedBy       Actor                `json:"created_by"`
	Recipient       HandoffRecipient     `json:"recipient"`
	Title           string               `json:"title"`
	BodyMarkdown    string               `json:"body_markdown"`
	Remaining       []string             `json:"remaining"`
	Agents          []HandoffAgent       `json:"agents"`
	Conversation    *HandoffConversation `json:"conversation,omitempty"`
	GovernanceState HandoffGovernance    `json:"governance_state"`
	AnchorsScope    string               `json:"anchors_scope"`
	ContentHash     string               `json:"content_hash"`
}

// HandoffAnchorsScope is the one value anchors_scope takes.
const HandoffAnchorsScope = "sender-local"

// HandoffWireHash is the handoff's wire hash: sha256 over the JSON of the whole
// redacted record with content_hash emptied. The sending device computes it once and
// freezes the body; the server keeps it as the compare key and never recomputes it
// from a body its own backstop may have redacted further.
func HandoffWireHash(r HandoffRecord) string {
	r.ContentHash = ""
	if r.Remaining == nil {
		r.Remaining = []string{}
	}
	if r.Agents == nil {
		r.Agents = []HandoffAgent{}
	}
	if r.GovernanceState.Tags == nil {
		r.GovernanceState.Tags = []HandoffTag{}
	}
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HandoffReceipt is schemas/handoff-receipt.schema.json 1.0.
type HandoffReceipt struct {
	SchemaVersion string           `json:"schema_version"`
	ID            string           `json:"id"`
	HandoffID     string           `json:"handoff_id"`
	Transition    string           `json:"transition"`
	TicketID      string           `json:"ticket_id,omitempty"`
	Session       *SessionIdentity `json:"session,omitempty"`
	CreatedAt     string           `json:"created_at"`
}

// ReceiptID is a receipt's deterministic id over (handoff, transition, device, ticket,
// native session id), so a retry is byte-identical and item 4's immutable push fits.
// The native id is part of it on purpose: when a claim moves to the launched task's own
// session (§6.4 rule 3) the corrected started receipt is a distinct record.
func ReceiptID(handoffID, transition, deviceID, ticketID, nativeID string) string {
	return engine.DeterministicTypedID(ReceiptIDPrefix,
		"handoff-receipt-v1\x00"+handoffID+"\x00"+transition+"\x00"+deviceID+"\x00"+ticketID+"\x00"+nativeID)
}

// HandoffOpenedBy names the recipient session a delivery row points at: the first
// opened by sequence, or before any opened the latest started.
type HandoffOpenedBy struct {
	DeviceID string          `json:"device_id"`
	Session  SessionIdentity `json:"session"`
}

// PulledHandoff is one delivery row as the server serves it to a device of the sender
// or of the recipient. Document is present only for a device of the recipient, and only
// while the body is not erased (withdrawn, or past retention); a sender's other devices
// get who, when and the state — no title and no text (§6.6).
type PulledHandoff struct {
	ID               string           `json:"id"`
	ToMe             bool             `json:"to_me"`
	FromMe           bool             `json:"from_me"`
	SenderUserID     string           `json:"sender_user_id"`
	SenderName       string           `json:"sender_name"`
	SenderDeviceID   string           `json:"sender_device_id"`
	RecipientUserID  string           `json:"recipient_user_id"`
	RecipientName    string           `json:"recipient_name"`
	State            string           `json:"state"`
	CreatedAt        string           `json:"created_at"`
	StateAt          string           `json:"state_at"`
	WireHash         string           `json:"wire_hash,omitempty"`
	Document         json.RawMessage  `json:"document,omitempty"`
	OpenedBy         *HandoffOpenedBy `json:"opened_by,omitempty"`
	OtherOpenedCount int              `json:"other_opened_count,omitempty"`
}

// Member is one entry of the member directory: a server user id and a display name,
// and nothing else (OD-10).
type Member struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
}

// MembersRequest asks for the organization's active members.
type MembersRequest struct {
	SchemaVersion string `json:"schema_version"`
}

// MembersResponse is the directory. Self is the calling device's own user id, so a
// device can draw "your other devices" without a second fact source.
type MembersResponse struct {
	Members    []Member `json:"members"`
	Self       string   `json:"self"`
	ServerTime int64    `json:"server_time"`
}
