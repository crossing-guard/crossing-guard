package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Handoff between members on the local API (team rest-of-release plan §6.9): the member
// directory, the Received and Sent lists, one handoff, the send with its preview, and
// decline, withdraw and close. Typed responses only (ADR 0022); the console and the
// handoff verbs never talk to the team server themselves. Opening a handoff, delivering
// its brief again and the memory-recall attach action are team_handoff_open_http.go's.
//
// None of these routes is device-facing, and there is no route — and no MCP tool —
// through which an agent session receives or sends a handoff on its own.

// registerTeamHandoffRoutes registers this file's routes.
func registerTeamHandoffRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/team/members", handleTeamMembers)
	mux.HandleFunc("GET /api/team/handoffs", handleTeamHandoffs)
	mux.HandleFunc("GET /api/team/handoffs/{id}", handleTeamHandoff)
	mux.HandleFunc("POST /api/team/handoffs/send", handleTeamHandoffSend)
	mux.HandleFunc("POST /api/team/handoffs/{id}/decline", handoffTransitionHandler(teamwire.TransitionDeclined))
	mux.HandleFunc("POST /api/team/handoffs/{id}/withdraw", handoffTransitionHandler(teamwire.TransitionWithdrawn))
	mux.HandleFunc("POST /api/team/handoffs/{id}/close", handoffTransitionHandler(teamwire.TransitionClosed))
	registerTeamHandoffOpenRoutes(mux)
	registerMemoryRecallRoutes(mux)
}

// teamHandoffError is the refusal body of every route here: a sentence and a data code.
type teamHandoffError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// Refusal codes of the send route that are the device's own (the server's are
// teamwire's; a row's are store.HandoffCode*).
const (
	handoffCodeNotLinked       = "not_linked"
	handoffCodePreviewMismatch = "preview_mismatch"
	handoffCodePreviewRequired = "preview_required"
	handoffCodeInvalid         = "invalid_handoff"
	handoffCodeAmbiguous       = "ambiguous_recipient"
	handoffCodeNoSession       = "unknown_session"
)

func writeHandoffError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(teamHandoffError{Error: message, Code: code})
}

// teamHandoffMember is one member as a device knows them: a user id and a display
// name, and nothing else (OD-10). Self marks this device's own user.
type teamHandoffMember struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Self        bool   `json:"self,omitempty"`
}

// teamMembersResponse is GET /api/team/members: the cached directory of the linked
// organization. Unlinked, it is empty.
type teamMembersResponse struct {
	Linked         bool                `json:"linked"`
	OrganizationID string              `json:"organization_id,omitempty"`
	Members        []teamHandoffMember `json:"members"`
	RefreshedAt    string              `json:"refreshed_at,omitempty"`
	Problem        string              `json:"problem,omitempty"`
}

func handleTeamMembers(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	if r.URL.Query().Get("refresh") != "" {
		team.refreshMembersNow()
	}
	orgID, err := governor.ix.LinkedOrganization()
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the link could not be read: "+err.Error())
		return
	}
	out := teamMembersResponse{Linked: orgID != "", OrganizationID: orgID, Members: []teamHandoffMember{}}
	if orgID != "" {
		members, err := governor.ix.TeamMembers(orgID)
		if err != nil {
			writeHandoffError(w, http.StatusInternalServerError, "", "the member directory could not be read: "+err.Error())
			return
		}
		var newest int64
		for _, m := range members {
			out.Members = append(out.Members, teamHandoffMember{UserID: m.UserID, DisplayName: m.DisplayName, Self: m.Self})
			newest = max(newest, m.RefreshedAt)
		}
		if newest > 0 {
			out.RefreshedAt = rfc3339(newest)
		}
		out.Problem = team.handoffTransportStatus(orgID).MembersProblem
	}
	writeJSON(w, out)
}

// teamHandoffItem is one handoff as this device lists it: who, when, the state, and
// what this device's part offers. It never carries the body; Title is empty on a row
// this device holds no document for.
type teamHandoffItem struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id,omitempty"`
	// Local marks a same-device handoff: no server, no receipt.
	Local    bool `json:"local,omitempty"`
	SentHere bool `json:"sent_here"`
	ToMe     bool `json:"to_me"`
	// FromAnotherDevice marks a handoff this user sent from another of their devices:
	// listed under Sent, withdrawable here, with no title and no text held here.
	FromAnotherDevice bool              `json:"from_another_device,omitempty"`
	Peer              teamHandoffMember `json:"peer"`
	Title             string            `json:"title,omitempty"`
	State             string            `json:"state"`
	StateAt           string            `json:"state_at,omitempty"`
	CreatedAt         string            `json:"created_at"`
	// RefusalCode is why a send was refused; ReceiptCode is the code the server
	// rejected this device's last receipt with (the handoff had ended another way).
	RefusalCode string `json:"refusal_code,omitempty"`
	ReceiptCode string `json:"receipt_code,omitempty"`
	// HasText says this device holds the document now; EverHeld that it held it at
	// some time. to_me with neither is a handoff that ended before it reached here.
	HasText          bool                      `json:"has_text"`
	EverHeld         bool                      `json:"ever_held"`
	Session          *teamwire.SessionIdentity `json:"session,omitempty"`
	RepositoryID     string                    `json:"repository_id,omitempty"`
	OpenedBy         *teamwire.HandoffOpenedBy `json:"opened_by,omitempty"`
	OtherOpenedCount int                       `json:"other_opened_count,omitempty"`
	// LinkEnded marks a row of a link that ended: readable, nothing is sent for it.
	LinkEnded bool `json:"link_ended,omitempty"`
	// Offers lists the transitions this device may ask for now (§6.2).
	Offers []string `json:"offers"`
	// OpenOffered says Open is offered on this item here (§6.3); OpenWithheldCode
	// says why it is not. Which runtimes are ready is the open-options route's.
	OpenOffered      bool   `json:"open_offered"`
	OpenWithheldCode string `json:"open_withheld_code,omitempty"`
	// Opens lists this handoff's opens on this device: the sessions that started
	// for it, the state of each one's brief, and what each offers.
	Opens []teamHandoffOpen `json:"opens"`
}

func handoffItem(h store.Handoff) teamHandoffItem {
	item := teamHandoffItem{ID: h.ID, OrganizationID: h.OrganizationID, Local: h.Local(), SentHere: h.SentHere, ToMe: h.ToMe,
		FromAnotherDevice: h.FromMe && !h.SentHere && !h.ToMe,
		Peer:              teamHandoffMember{UserID: h.PeerUserID, DisplayName: h.PeerName},
		Title:             h.Title, State: h.State, CreatedAt: rfc3339(h.CreatedAt), RefusalCode: h.RefusalCode, ReceiptCode: h.ReceiptCode,
		HasText: h.HasBody, EverHeld: h.Held, RepositoryID: h.RepositoryID, OpenedBy: h.OpenedBy, OtherOpenedCount: h.OtherOpenedCount,
		LinkEnded: h.LinkEnded, Offers: store.HandoffOffers(h)}
	if refusal := store.HandoffOpenRefusal(h); refusal != nil {
		item.OpenWithheldCode = refusal.Code
	} else {
		item.OpenOffered = true
	}
	if item.Opens = handoffOpensOf(h); item.Opens == nil {
		item.Opens = []teamHandoffOpen{}
	}
	if h.StateAt > 0 {
		item.StateAt = rfc3339(h.StateAt)
	}
	if h.Source.Runtime != "" || h.Source.NativeID != "" {
		source := h.Source
		item.Session = &source
	}
	return item
}

// handoffListedAsSent is §6.2's table: the originating device lists a handoff under
// Sent — a self-send included, once — and so does the sender's other device. A local
// handoff is listed under Received: it is already where it is going.
func handoffListedAsSent(h store.Handoff) bool {
	if h.Local() {
		return false
	}
	return h.SentHere || (h.FromMe && !h.ToMe)
}

// teamHandoffsResponse is GET /api/team/handoffs.
type teamHandoffsResponse struct {
	Linked         bool              `json:"linked"`
	OrganizationID string            `json:"organization_id,omitempty"`
	Received       []teamHandoffItem `json:"received"`
	Sent           []teamHandoffItem `json:"sent"`
	Transport      handoffTransport  `json:"transport"`
	// CheckoutLeftovers, present only when asked for with ?leftovers=1, names the
	// checkouts this device has seen that still hold a handoff file or marker block
	// an earlier version wrote; `crossing-guard handoff clean <dir>` removes them.
	CheckoutLeftovers []HandoffLeftover `json:"checkout_leftovers,omitempty"`
}

func handleTeamHandoffs(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	orgID, err := governor.ix.LinkedOrganization()
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the link could not be read: "+err.Error())
		return
	}
	rows, err := governor.ix.ListHandoffs(orgID)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the handoffs could not be read: "+err.Error())
		return
	}
	out := teamHandoffsResponse{Linked: orgID != "", OrganizationID: orgID, Received: []teamHandoffItem{}, Sent: []teamHandoffItem{},
		Transport: team.handoffTransportStatus(orgID)}
	for _, h := range rows {
		if handoffListedAsSent(h) {
			out.Sent = append(out.Sent, handoffItem(h))
		} else {
			out.Received = append(out.Received, handoffItem(h))
		}
	}
	if r.URL.Query().Get("leftovers") != "" {
		out.CheckoutLeftovers = seenCheckoutLeftovers()
	}
	writeJSON(w, out)
}

// seenCheckoutLeftovers inspects the checkout roots this device's sessions recorded —
// as many as team.json's identity_upgrade_roots, the link's bound on a pass over them.
func seenCheckoutLeftovers() []HandoffLeftover {
	team.mu.Lock()
	limit := team.doc.IdentityUpgradeRoots
	team.mu.Unlock()
	roots, err := governor.ix.CheckoutRoots(limit)
	if err != nil {
		return nil
	}
	return CheckoutsWithHandoffLeftovers(roots)
}

// teamHandoffOpen is one Open of a handoff on this device, as the detail route lists
// it. The open lane owns its rows and fills them through handoffOpensOf; the fields
// here are the ticket's identity and state, which that lane extends.
type teamHandoffOpen struct {
	TicketID string `json:"ticket_id"`
	Runtime  string `json:"runtime"`
	// State is waiting (its composer is open), claimed (a session started for it)
	// or cancelled.
	State        string `json:"state"`
	CheckoutRoot string `json:"checkout_root,omitempty"`
	OpenedAt     string `json:"opened_at,omitempty"`
	// EndedCode says why an open ended without a session, as a data code:
	// composer_closed, runtime_not_started (EndedDetail is the task's reason),
	// hook_did_not_run, link_ended, or the handoff's terminal code.
	EndedCode   string `json:"ended_code,omitempty"`
	EndedDetail string `json:"ended_detail,omitempty"`
	// Session is the session that started for this open, and StartedAt when.
	Session   *teamHandoffOpenSession `json:"session,omitempty"`
	StartedAt string                  `json:"started_at,omitempty"`
	// Brief is the brief's state for that session, as a data code:
	// waiting_for_next_prompt, delivered or not_delivered. BriefDueAgain says it was
	// asked for again and the session's next prompt carries it.
	Brief            string `json:"brief,omitempty"`
	BriefDeliveredAt string `json:"brief_delivered_at,omitempty"`
	BriefDueAgain    bool   `json:"brief_due_again,omitempty"`
	// Offers lists what this open offers now: deliver-again, retry.
	Offers []string `json:"offers"`
}

// handoffOpensOf lists a handoff's opens on this device. It returns none until the
// open lane sets it.
var handoffOpensOf = func(store.Handoff) []teamHandoffOpen { return []teamHandoffOpen{} }

// teamHandoffDetailResponse is GET /api/team/handoffs/{id}: the item, the document
// when this device holds it — a device of the recipient, or the originating device
// (OD-18: reading on the recipient's own device is allowed) — and its opens here.
type teamHandoffDetailResponse struct {
	Handoff  teamHandoffItem         `json:"handoff"`
	Document *teamwire.HandoffRecord `json:"document,omitempty"`
	Opens    []teamHandoffOpen       `json:"opens"`
}

// findHandoff reads a handoff by the id in the request path, among the rows this
// device lists. It writes the refusal itself and reports whether it found one.
func findHandoff(w http.ResponseWriter, r *http.Request) (store.Handoff, bool) {
	orgID, err := governor.ix.LinkedOrganization()
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the link could not be read: "+err.Error())
		return store.Handoff{}, false
	}
	h, found, err := governor.ix.FindHandoff(orgID, r.PathValue("id"))
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the handoff could not be read: "+err.Error())
		return store.Handoff{}, false
	}
	if !found {
		writeHandoffError(w, http.StatusNotFound, "", "no such handoff on this device")
		return store.Handoff{}, false
	}
	return h, true
}

func handoffDetail(h store.Handoff) (teamHandoffDetailResponse, error) {
	item := handoffItem(h)
	out := teamHandoffDetailResponse{Handoff: item, Opens: item.Opens}
	rec, held, err := h.Document()
	if err != nil {
		return out, err
	}
	if held {
		out.Document = &rec
	}
	if out.Opens == nil {
		out.Opens = []teamHandoffOpen{}
	}
	return out, nil
}

func handleTeamHandoff(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	h, found := findHandoff(w, r)
	if !found {
		return
	}
	out, err := handoffDetail(h)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the stored handoff could not be read")
		return
	}
	writeJSON(w, out)
}

// teamHandoffSendRequest is POST /api/team/handoffs/send. Title, BodyMarkdown and
// Remaining are the person's final text. With Preview the route writes nothing and
// answers what would leave; the send then repeats the same request with the previewed
// ID, CreatedAt and WireHash.
type teamHandoffSendRequest struct {
	Runtime   string `json:"runtime"`
	SessionID string `json:"session_id"`
	// To names the recipient: a user id or a display name from the member directory.
	// Ignored for a local handoff.
	To                  string   `json:"to"`
	Local               bool     `json:"local"`
	Title               string   `json:"title"`
	BodyMarkdown        string   `json:"body_markdown"`
	Remaining           []string `json:"remaining"`
	IncludeConversation bool     `json:"include_conversation"`
	Preview             bool     `json:"preview"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	WireHash            string   `json:"wire_hash"`
}

// teamHandoffExcerpt is the excerpt as it will leave: how many turns, how many bytes
// of text, and every turn.
type teamHandoffExcerpt struct {
	TurnCount int                    `json:"turn_count"`
	Bytes     int                    `json:"bytes"`
	Truncated bool                   `json:"truncated"`
	Turns     []teamwire.HandoffTurn `json:"turns"`
}

// teamHandoffSendResponse is exactly the content that leaves (§6.1): every field of the
// document a person can read, the frozen wire body's size, and what the checks changed.
// On a send it also carries the row's state.
type teamHandoffSendResponse struct {
	Preview        bool                       `json:"preview"`
	ID             string                     `json:"id"`
	CreatedAt      string                     `json:"created_at"`
	WireHash       string                     `json:"wire_hash"`
	Local          bool                       `json:"local,omitempty"`
	OrganizationID string                     `json:"organization_id,omitempty"`
	Recipient      *teamHandoffMember         `json:"recipient,omitempty"`
	Title          string                     `json:"title"`
	Remaining      []string                   `json:"remaining"`
	BodyMarkdown   string                     `json:"body_markdown"`
	Conversation   *teamHandoffExcerpt        `json:"conversation,omitempty"`
	Session        teamwire.SessionIdentity   `json:"session"`
	RepositoryID   *string                    `json:"repository_id"`
	Governance     teamwire.HandoffGovernance `json:"governance_state"`
	Agents         []teamwire.HandoffAgent    `json:"agents"`
	// Bytes is the size of the frozen wire body: what one push of it carries.
	Bytes  int           `json:"bytes"`
	Checks handoffChecks `json:"checks"`
	State  string        `json:"state,omitempty"`
}

func handoffSendResponse(rec teamwire.HandoffRecord, wire []byte, checks handoffChecks) teamHandoffSendResponse {
	out := teamHandoffSendResponse{ID: rec.ID, CreatedAt: rec.CreatedAt, WireHash: rec.ContentHash, Title: rec.Title,
		Remaining: rec.Remaining, BodyMarkdown: rec.BodyMarkdown, Session: rec.Session, RepositoryID: rec.RepositoryID,
		Governance: rec.GovernanceState, Agents: rec.Agents, Bytes: len(wire), Checks: checks}
	if rec.Conversation != nil {
		excerpt := &teamHandoffExcerpt{TurnCount: len(rec.Conversation.Turns), Truncated: rec.Conversation.Truncated, Turns: rec.Conversation.Turns}
		for _, turn := range rec.Conversation.Turns {
			excerpt.Bytes += len(turn.Text)
		}
		out.Conversation = excerpt
	}
	return out
}

// handoffSendBodyLimit bounds the send request: a handoff must fit one push, so the
// link's push_max_bytes (team.json; its default loads unlinked) bounds the request too.
func handoffSendBodyLimit() int64 {
	team.mu.Lock()
	defer team.mu.Unlock()
	return team.doc.PushMaxBytes
}

// handoffIdentity settles the document's id and creation time: minted for a preview,
// required of a send — the send repeats the preview's, or the hashes cannot agree.
func handoffIdentity(req teamHandoffSendRequest, now time.Time) (id, createdAt string, ok bool) {
	if req.Preview {
		return engine.NewTypedID(teamwire.HandoffIDPrefix), now.UTC().Format(time.RFC3339), true
	}
	if !engine.IsTypedID(req.ID) || !strings.HasPrefix(req.ID, teamwire.HandoffIDPrefix+"_") || req.WireHash == "" {
		return "", "", false
	}
	if _, err := time.Parse(time.RFC3339, req.CreatedAt); err != nil {
		return "", "", false
	}
	return req.ID, req.CreatedAt, true
}

// handleTeamHandoffSend previews or sends a handoff. 400 for a document that cannot
// leave as written; 404 for an unknown session; 409 when the device is not linked, the
// recipient is not in the directory (recipient_inactive), or the document no longer
// hashes to what the preview showed (preview_mismatch).
func handleTeamHandoffSend(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req teamHandoffSendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, handoffSendBodyLimit()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeHandoffError(w, http.StatusBadRequest, handoffCodeInvalid, "body must be a handoff send request (runtime, session_id, to or local, title, body_markdown, remaining, include_conversation, preview)")
		return
	}
	now := team.now()
	id, createdAt, ok := handoffIdentity(req, now)
	if !ok {
		writeHandoffError(w, http.StatusBadRequest, handoffCodePreviewRequired, "a send repeats its preview's id, created_at and wire_hash; ask for a preview first")
		return
	}
	orgID, recipient, refused := handoffDestination(w, req)
	if refused {
		return
	}
	session, err := LoadSession(req.Runtime, req.SessionID)
	if err != nil {
		writeHandoffError(w, http.StatusNotFound, handoffCodeNoSession, "no such session on this device")
		return
	}
	deviceID, _, err := governor.ix.Device()
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the device identity could not be read: "+err.Error())
		return
	}
	detectors, err := engine.DefaultDetectors()
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the secret patterns could not be loaded: "+err.Error())
		return
	}
	config, _ := consoleConfig()
	rec, wire, checks, err := buildHandoffDocument(handoffDocumentInput{ID: id, CreatedAt: createdAt, DeviceID: deviceID, Session: session,
		Checkout: handoffCheckout(session), Recipient: recipient.UserID, Title: req.Title, Body: req.BodyMarkdown, Remaining: req.Remaining,
		IncludeConversation: req.IncludeConversation, Limits: config.Handoff}, detectors)
	var invalid *handoffInvalid
	switch {
	case errors.As(err, &invalid):
		writeHandoffError(w, http.StatusBadRequest, handoffCodeInvalid, invalid.reason)
		return
	case err != nil:
		writeHandoffError(w, http.StatusInternalServerError, "", "the handoff could not be built")
		return
	}
	out := handoffSendResponse(rec, wire, checks)
	out.Preview, out.Local, out.OrganizationID = req.Preview, req.Local, orgID
	if !req.Local {
		out.Recipient = &recipient
	}
	if req.Preview {
		writeJSON(w, out)
		return
	}
	if rec.ContentHash != req.WireHash {
		writeHandoffError(w, http.StatusConflict, handoffCodePreviewMismatch, "the handoff is no longer what the preview showed (the text, the recipient, or the session changed); nothing was sent — preview it again")
		return
	}
	if err := governor.ix.SendHandoff(store.HandoffSend{OrganizationID: orgID, Record: rec, Body: wire, PeerName: recipient.DisplayName,
		SelfSend: recipient.Self, At: now.Unix()}); err != nil {
		if errors.Is(err, store.ErrNotLinked) || errors.Is(err, store.ErrLinkChanged) {
			writeHandoffError(w, http.StatusConflict, handoffCodeNotLinked, "the team link changed; nothing was sent")
			return
		}
		writeHandoffError(w, http.StatusInternalServerError, "", "the handoff could not be saved")
		return
	}
	if req.Local {
		team.linkEvent("handoff.local", "local handoff "+rec.ID+" written")
		out.State = teamwire.HandoffReceived
	} else {
		team.linkEvent("team.handoff.send", "handoff "+rec.ID+" queued for "+recipient.UserID)
		out.State = store.HandoffQueued
		team.pushSoon()
	}
	writeJSON(w, out)
}

// handoffDestination settles where a send goes: nowhere (local), or the linked
// organization and a member of its directory. It writes the refusal itself.
func handoffDestination(w http.ResponseWriter, req teamHandoffSendRequest) (orgID string, recipient teamHandoffMember, refused bool) {
	if req.Local {
		return "", teamHandoffMember{UserID: handoffLocalRecipient}, false
	}
	orgID, err := governor.ix.LinkedOrganization()
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the link could not be read: "+err.Error())
		return "", recipient, true
	}
	if orgID == "" || !team.sameLinkNow() {
		writeHandoffError(w, http.StatusConflict, handoffCodeNotLinked, "this device is not linked to a team; a handoff to a member needs a link (a local handoff does not)")
		return "", recipient, true
	}
	recipient, err = resolveRecipient(orgID, strings.TrimSpace(req.To))
	switch {
	case errors.Is(err, errRecipientNotListed):
		writeHandoffError(w, http.StatusConflict, teamwire.CodeRecipientInactive, "the member directory does not list that recipient; nothing was sent and the text is unchanged")
		return "", recipient, true
	case errors.Is(err, errRecipientAmbiguous):
		writeHandoffError(w, http.StatusBadRequest, handoffCodeAmbiguous, err.Error())
		return "", recipient, true
	case err != nil:
		writeHandoffError(w, http.StatusInternalServerError, "", "the member directory could not be read: "+err.Error())
		return "", recipient, true
	}
	return orgID, recipient, false
}

// sameLinkNow reports whether the device is linked right now.
func (t *teamLinker) sameLinkNow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state == teamLinked
}

// handoffTransitionEvents names the chained event of each transition a person asks for.
var handoffTransitionEvents = map[string]string{
	teamwire.TransitionDeclined:  "team.handoff.decline",
	teamwire.TransitionWithdrawn: "team.handoff.withdraw",
	teamwire.TransitionClosed:    "team.handoff.close",
}

// handoffRefusalSentences words a row's refusal codes. The code is the data; the
// sentence is for a terminal.
var handoffRefusalSentences = map[string]string{
	store.HandoffCodeLinkEnded:    "this handoff belongs to a team link that ended; nothing is sent for it",
	store.HandoffCodeWrongState:   "the handoff's state does not offer this",
	store.HandoffCodeNotOffered:   "this device's part in the handoff does not offer this",
	teamwire.CodeHandoffWithdrawn: "the sender withdrew this handoff",
	teamwire.CodeHandoffDeclined:  "this handoff was declined",
	teamwire.CodeHandoffClosed:    "this handoff was closed",
	teamwire.CodeHandoffExpired:   "this handoff expired",
	teamwire.CodeHandoffStarted:   "a session already started for this handoff",
}

// handoffTransitionHandler serves decline, withdraw and close: the state and the
// receipt in one store transaction, then one drain so the receipt leaves at once.
// 404 for an unknown id; 409, with the code, when the row does not offer it.
func handoffTransitionHandler(transition string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if team == nil || governor == nil {
			writeGovernorUnavailable(w)
			return
		}
		h, found := findHandoff(w, r)
		if !found {
			return
		}
		after, err := governor.ix.TransitionHandoff(store.HandoffTransition{OrganizationID: h.OrganizationID, ID: h.ID,
			Transition: transition, At: team.now().Unix()})
		var refusal *store.HandoffRefusal
		switch {
		case errors.As(err, &refusal):
			writeHandoffError(w, http.StatusConflict, refusal.Code, handoffRefusalSentences[refusal.Code])
			return
		case errors.Is(err, store.ErrHandoffNotFound):
			writeHandoffError(w, http.StatusNotFound, "", "no such handoff on this device")
			return
		case err != nil:
			writeHandoffError(w, http.StatusInternalServerError, "", "the handoff could not be changed")
			return
		}
		team.linkEvent(handoffTransitionEvents[transition], "handoff "+after.ID+" "+after.State)
		if !after.Local() {
			team.pushSoon()
		}
		out, err := handoffDetail(after)
		if err != nil {
			writeHandoffError(w, http.StatusInternalServerError, "", "the stored handoff could not be read")
			return
		}
		writeJSON(w, out)
	}
}
