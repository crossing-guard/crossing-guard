package daemon

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/memcli"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Opening a handoff on the local API (team rest-of-release plan §6.3, §6.5, §6.9):
// what the open sheet needs, the open itself and its cancellation, Deliver again, the
// document a claimed session asks for through get_handoff, and the facts the opened
// session's own header shows. Typed responses only (ADR 0022). None of these routes is
// device-facing, and none launches anything: the session is started by the person's
// first prompt, through the task service, carrying the ticket the open returned.

func init() { handoffOpensOf = handoffOpensFor }

// registerTeamHandoffOpenRoutes registers this file's routes.
func registerTeamHandoffOpenRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/team/handoffs/{id}/open-options", handleTeamHandoffOpenOptions)
	mux.HandleFunc("POST /api/team/handoffs/{id}/open", handleTeamHandoffOpen)
	mux.HandleFunc("POST /api/team/handoffs/{id}/open/{ticket}/cancel", handleTeamHandoffOpenCancel)
	mux.HandleFunc("POST /api/team/handoffs/{id}/deliver-again", handleTeamHandoffDeliverAgain)
	mux.HandleFunc("GET /api/team/handoffs/claimed", handleTeamHandoffClaimed)
	mux.HandleFunc("GET /api/team/handoffs/session", handleTeamHandoffSession)
}

// The brief's state for one opened session, as data codes. The console words them:
// "started in <session>; waiting for its next prompt", "brief not delivered".
const (
	handoffBriefWaiting      = "waiting_for_next_prompt"
	handoffBriefDelivered    = "delivered"
	handoffBriefNotDelivered = "not_delivered"
)

// Actions a ticket offers.
const (
	handoffActionOpen         = "open"
	handoffActionDeliverAgain = "deliver-again"
	handoffActionRetry        = "retry"
)

// teamHandoffOpenSession is the session an Open started, by the two values the ticket
// holds: the runtime and the native id. The catalog and resume ids are the store's
// session row's; the console reads them there and never joins on one id.
type teamHandoffOpenSession struct {
	Runtime  string `json:"runtime"`
	NativeID string `json:"native_id"`
}

// handoffOpensFor lists a handoff's opens on this device, newest first.
func handoffOpensFor(h store.Handoff) []teamHandoffOpen {
	out := []teamHandoffOpen{}
	if governor == nil || governor.ix == nil {
		return out
	}
	tickets, err := governor.ix.HandoffOpensFor(h.OrganizationID, h.ID)
	if err != nil {
		return out
	}
	live := !teamwire.HandoffTerminal(h.State)
	for _, ticket := range tickets {
		out = append(out, handoffOpenView(ticket, live))
	}
	return out
}

// handoffOpenView is one ticket as the console shows it. live is whether the handoff
// is not terminal: only then does a ticket offer anything.
func handoffOpenView(t store.HandoffOpen, live bool) teamHandoffOpen {
	view := teamHandoffOpen{TicketID: t.TicketID, Runtime: t.Runtime, State: t.State, CheckoutRoot: t.CheckoutRoot,
		EndedCode: t.CancelReason, EndedDetail: t.CancelDetail, Offers: []string{}, OpenedAt: rfc3339(t.CreatedAt)}
	if t.Claimed() {
		view.Session = &teamHandoffOpenSession{Runtime: t.ClaimedRuntime, NativeID: t.ClaimedNativeID}
		view.StartedAt = rfc3339(t.ClaimedAt)
		switch {
		case t.BriefConfirmedAt > 0:
			view.Brief, view.BriefDeliveredAt = handoffBriefDelivered, rfc3339(t.BriefConfirmedAt)
		case t.BriefGivenUpAt > 0:
			view.Brief = handoffBriefNotDelivered
		default:
			view.Brief = handoffBriefWaiting
		}
		// Asked for again and not yet handed over: the next prompt carries it.
		view.BriefDueAgain = t.BriefDue && t.BriefConfirmedAt > 0
		if live && t.State == store.HandoffOpenClaimed {
			view.Offers = append(view.Offers, handoffActionDeliverAgain)
		}
	}
	// Retry is offered in one case only: the runtime never started a session.
	if live && t.State == store.HandoffOpenCancelled && t.CancelReason == store.HandoffOpenRuntimeNotStarted {
		view.Offers = append(view.Offers, handoffActionRetry)
	}
	return view
}

// Why a runtime is not ready for Open, as data codes: exactly what is missing.
const (
	handoffMissingSessionEntry = "session_entry_not_observed"
	handoffMissingPrompt       = "prompt_event_not_observed"
	handoffMissingCarrierKind  = "prompt_kind_not_a_carrier_kind"
	// handoffMissingAfterLaunch: a session Crossing Guard started for an Open made no
	// claim — the hook did not run in it — and no row newer than that has been seen.
	handoffMissingAfterLaunch = "hook_did_not_run_in_launched_session"
)

// teamHandoffRecall is one runtime's memory recall, as the open sheet and the
// Runtimes settings card state it (OD-22): whether the memory hook has been OBSERVED
// injecting — a settings entry proves nothing — whether an entry exists and whether
// the Turn on action wrote it, and whether this daemon may turn it on or off.
type teamHandoffRecall struct {
	Runtime string `json:"runtime"`
	// ObservedInjecting says a recent session of the runtime carries a memory index
	// this device's hook emitted; SessionsChecked and SessionsInjected are the counts.
	ObservedInjecting bool `json:"observed_injecting"`
	SessionsChecked   int  `json:"sessions_checked"`
	SessionsInjected  int  `json:"sessions_injected"`
	// EntryPresent says the runtime's settings hold a memory hook entry;
	// AddedByAction that the Turn on action wrote it, so Turn off removes it.
	EntryPresent  bool   `json:"entry_present"`
	AddedByAction bool   `json:"added_by_action"`
	ConfigPath    string `json:"config_path,omitempty"`
	// Offers lists the actions this daemon may take now: "attach", "detach".
	Offers []string `json:"offers"`
	// WithheldCode says why it may take none: the installation is not this
	// daemon's (OD-24), or the runtime has no lifecycle hook here.
	WithheldCode string `json:"withheld_code,omitempty"`
	Withheld     string `json:"withheld,omitempty"`
}

// teamHandoffOpenRuntime is one runtime a handoff can be opened in.
type teamHandoffOpenRuntime struct {
	Runtime string `json:"runtime"`
	// Ready says Open is offered in this runtime. Missing lists exactly what has not
	// been observed inside handoff.firing_window when it is not.
	Ready             bool     `json:"ready"`
	Missing           []string `json:"missing"`
	SessionEntryAt    string   `json:"session_entry_at,omitempty"`
	PromptAt          string   `json:"prompt_at,omitempty"`
	UnclaimedLaunchAt string   `json:"unclaimed_launch_at,omitempty"`
	// RecallTools is the recall tools' registration for the runtime (the state of
	// its MCP entry): where they are not registered the brief still arrives and
	// get_handoff does not.
	RecallTools  string            `json:"recall_tools"`
	MemoryRecall teamHandoffRecall `json:"memory_recall"`
}

// teamHandoffOpenOptionsResponse is GET /api/team/handoffs/{id}/open-options: what the
// open sheet shows before the person chooses.
type teamHandoffOpenOptionsResponse struct {
	HandoffID string `json:"handoff_id"`
	// Offered says the item itself offers Open; WithheldCode says why not.
	Offered      bool   `json:"offered"`
	WithheldCode string `json:"withheld_code,omitempty"`
	// CheckoutRoots are the folders this device has resolved to the handoff's
	// repository, newest first. The person may choose another.
	CheckoutRoots []string                 `json:"checkout_roots"`
	Runtimes      []teamHandoffOpenRuntime `json:"runtimes"`
	FiringWindow  string                   `json:"firing_window"`
}

// handoffRecallToolsUnknown is the registration state of a runtime that cannot
// register the recall tools at all.
const handoffRecallToolsUnknown = "unavailable"

// daemonExecutable is this daemon's own executable. A test replaces it.
var daemonExecutable = os.Executable

// handoffRuntimeReadiness reads what this device has observed of one runtime's hook
// and says whether Open is offered in it (§6.3 "When Open is offered", K-7).
func handoffRuntimeReadiness(runtime string, now time.Time) (teamHandoffOpenRuntime, error) {
	out := teamHandoffOpenRuntime{Runtime: runtime, Missing: []string{}}
	config, _ := consoleConfig()
	since := now.Add(-config.Handoff.FiringWindow.Duration).Unix()
	firing, err := governor.ix.HandoffRuntimeFiring(runtime, handoffPromptKind, since)
	if err != nil {
		return out, err
	}
	// After a launched turn that started a session and made no claim, only rows
	// NEWER than it count: readiness does not go stale.
	fresh := func(at int64) bool { return at > 0 && at > firing.UnclaimedLaunchAt }
	if firing.SessionEntryAt > 0 {
		out.SessionEntryAt = rfc3339(firing.SessionEntryAt)
	}
	if firing.PromptAt > 0 {
		out.PromptAt = rfc3339(firing.PromptAt)
	}
	if firing.UnclaimedLaunchAt > 0 {
		out.UnclaimedLaunchAt = rfc3339(firing.UnclaimedLaunchAt)
	}
	stale := firing.UnclaimedLaunchAt > 0 && (!fresh(firing.SessionEntryAt) || !fresh(firing.PromptAt))
	switch {
	case stale:
		out.Missing = append(out.Missing, handoffMissingAfterLaunch)
	default:
		if firing.SessionEntryAt == 0 {
			out.Missing = append(out.Missing, handoffMissingSessionEntry)
		}
		if firing.PromptAt == 0 {
			out.Missing = append(out.Missing, handoffMissingPrompt)
		}
	}
	if !orchestrationConfig().IsCarrierKind(handoffPromptKind) {
		out.Missing = append(out.Missing, handoffMissingCarrierKind)
	}
	out.Ready = len(out.Missing) == 0
	return out, nil
}

// handoffRecallTools is the recall tools' registration state for a runtime.
func handoffRecallTools(runtime string) string {
	self, err := daemonExecutable()
	if err != nil || !slices.Contains(guardcli.RecallRuntimes(), runtime) {
		return handoffRecallToolsUnknown
	}
	state, err := guardcli.RecallStatusFor(runtime, guardcli.RecallHookConfig(runtime), self)
	if err != nil || state == "" {
		return handoffRecallToolsUnknown
	}
	return state
}

func handleTeamHandoffOpenOptions(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	h, found := findHandoff(w, r)
	if !found {
		return
	}
	config, _ := consoleConfig()
	out := teamHandoffOpenOptionsResponse{HandoffID: h.ID, CheckoutRoots: []string{}, Runtimes: []teamHandoffOpenRuntime{},
		FiringWindow: config.Handoff.FiringWindow.Duration.String()}
	if refusal := store.HandoffOpenRefusal(h); refusal != nil {
		out.WithheldCode = refusal.Code
	} else {
		out.Offered = true
	}
	team.mu.Lock()
	rootsLimit := team.doc.IdentityUpgradeRoots
	team.mu.Unlock()
	if roots, err := governor.ix.HandoffCheckoutRoots(h.RepositoryID, rootsLimit); err == nil {
		out.CheckoutRoots = roots
	}
	now := team.now()
	for _, runtime := range handoffOpenRuntimes() {
		readiness, err := handoffRuntimeReadiness(runtime, now)
		if err != nil {
			writeHandoffError(w, http.StatusInternalServerError, "", "the runtime's readiness could not be read: "+err.Error())
			return
		}
		readiness.RecallTools = handoffRecallTools(runtime)
		readiness.MemoryRecall = memoryRecallState(runtime, config.Handoff.RecallCheckSessions)
		out.Runtimes = append(out.Runtimes, readiness)
	}
	writeJSON(w, out)
}

// teamHandoffOpenRequest is POST /api/team/handoffs/{id}/open: the runtime the person
// chose and the folder the session will start in.
type teamHandoffOpenRequest struct {
	Runtime      string `json:"runtime"`
	CheckoutRoot string `json:"checkout_root"`
}

// teamHandoffOpenResponse is the ticket and what the composer needs: the composer
// sends the person's first prompt to POST /api/runtime-tasks with handoff_ticket set
// to Ticket.TicketID, and cancels the ticket if it is closed without sending.
type teamHandoffOpenResponse struct {
	Ticket  teamHandoffOpen `json:"ticket"`
	Handoff teamHandoffItem `json:"handoff"`
}

// Refusal codes of the open route that are its own.
const (
	handoffCodeNotReady    = "runtime_not_ready"
	handoffCodeNoCheckout  = "checkout_root_required"
	handoffCodeBadCheckout = "checkout_root_unusable"
)

// handleTeamHandoffOpen writes a ticket. 400 for a request that names no runtime or
// no usable folder; 404 for an unknown handoff; 409, with the code, when the item does
// not offer Open, the runtime is not one a handoff opens in, or its hook has not been
// observed firing.
func handleTeamHandoffOpen(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	h, found := findHandoff(w, r)
	if !found {
		return
	}
	var req teamHandoffOpenRequest
	if err := decodeManagedJSON(w, r, &req); err != nil {
		writeHandoffError(w, http.StatusBadRequest, handoffCodeInvalid, "body must be an open request (runtime, checkout_root)")
		return
	}
	if !slices.Contains(handoffOpenRuntimes(), req.Runtime) {
		writeHandoffError(w, http.StatusConflict, handoffCodeRuntimeNotOff, "a handoff cannot be opened in this runtime")
		return
	}
	if strings.TrimSpace(req.CheckoutRoot) == "" {
		writeHandoffError(w, http.StatusBadRequest, handoffCodeNoCheckout, "choose the folder the session starts in")
		return
	}
	root, err := validateChatCwd(req.CheckoutRoot)
	if err != nil {
		writeHandoffError(w, http.StatusBadRequest, handoffCodeBadCheckout, err.Error())
		return
	}
	now := team.now()
	readiness, err := handoffRuntimeReadiness(req.Runtime, now)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the runtime's readiness could not be read: "+err.Error())
		return
	}
	if !readiness.Ready {
		writeHandoffError(w, http.StatusConflict, handoffCodeNotReady, "this runtime's hook has not been observed firing here: "+strings.Join(readiness.Missing, ", "))
		return
	}
	ticket, err := governor.ix.CreateHandoffOpen(store.HandoffOpenCreate{OrganizationID: h.OrganizationID, HandoffID: h.ID,
		Runtime: req.Runtime, CheckoutRoot: root, At: now.Unix()})
	var refusal *store.HandoffRefusal
	switch {
	case errors.As(err, &refusal):
		writeHandoffError(w, http.StatusConflict, refusal.Code, handoffOpenRefusalSentence(refusal.Code))
		return
	case errors.Is(err, store.ErrHandoffNotFound):
		writeHandoffError(w, http.StatusNotFound, "", "no such handoff on this device")
		return
	case err != nil:
		writeHandoffError(w, http.StatusInternalServerError, "", "the open could not be saved")
		return
	}
	writeJSON(w, teamHandoffOpenResponse{Ticket: handoffOpenView(ticket, true), Handoff: handoffItem(h)})
}

// handoffOpenRefusalSentence words a refusal of the open and deliver-again routes.
func handoffOpenRefusalSentence(code string) string {
	if sentence, ok := handoffLaunchSentences[code]; ok {
		return sentence
	}
	if sentence, ok := handoffRefusalSentences[code]; ok {
		return sentence
	}
	switch code {
	case store.HandoffCodeNoText:
		return "this device holds no text for this handoff"
	case store.HandoffCodeNotClaimed:
		return "no session on this device started for this handoff"
	}
	return "this handoff does not offer that now"
}

// teamHandoffOpenCancelResponse is POST …/open/{ticket}/cancel.
type teamHandoffOpenCancelResponse struct {
	Ticket teamHandoffOpen `json:"ticket"`
	// Cancelled is false when the ticket was no longer waiting, or had already
	// launched its session: closing the composer then changes nothing.
	Cancelled bool `json:"cancelled"`
}

// handoffTicketOf reads the ticket a route names and checks it is this handoff's. It
// writes the refusal itself.
func handoffTicketOf(w http.ResponseWriter, h store.Handoff, ticketID string) (store.HandoffOpen, bool) {
	ticket, found, err := governor.ix.HandoffOpenByTicket(ticketID)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the open could not be read: "+err.Error())
		return store.HandoffOpen{}, false
	}
	if !found || ticket.HandoffID != h.ID || ticket.OrganizationID != h.OrganizationID {
		writeHandoffError(w, http.StatusNotFound, store.HandoffCodeTicketNotFound, "no such open of this handoff on this device")
		return store.HandoffOpen{}, false
	}
	return ticket, true
}

// handleTeamHandoffOpenCancel cancels a ticket whose composer was closed without
// sending. A ticket has no timer; this is how one that was never used ends.
func handleTeamHandoffOpenCancel(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	h, found := findHandoff(w, r)
	if !found {
		return
	}
	ticket, ok := handoffTicketOf(w, h, r.PathValue("ticket"))
	if !ok {
		return
	}
	cancelled, err := governor.ix.CancelHandoffOpen(ticket.TicketID, team.now().Unix())
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the open could not be cancelled")
		return
	}
	after, _, err := governor.ix.HandoffOpenByTicket(ticket.TicketID)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the open could not be read: "+err.Error())
		return
	}
	writeJSON(w, teamHandoffOpenCancelResponse{Ticket: handoffOpenView(after, !teamwire.HandoffTerminal(h.State)), Cancelled: cancelled})
}

// teamHandoffDeliverAgainRequest is POST …/deliver-again. Ticket names which opened
// session; it may be omitted when the handoff has exactly one.
type teamHandoffDeliverAgainRequest struct {
	Ticket string `json:"ticket"`
}

// teamHandoffDeliverAgainResponse answers with the ticket. Armed is false when the
// session already holds as many undelivered messages as the device allows: the brief
// is armed at the next sweep.
type teamHandoffDeliverAgainResponse struct {
	Ticket teamHandoffOpen `json:"ticket"`
	Armed  bool            `json:"armed"`
}

const handoffCodeWhichSession = "ticket_required"

// handleTeamHandoffDeliverAgain re-arms the brief for a claimed session (§6.5): on
// every claimed, non-terminal item, started or opened. It never produces a second
// opened. 409 with the code on a withdrawn or otherwise ended handoff, or when no
// session started for it here.
func handleTeamHandoffDeliverAgain(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	h, found := findHandoff(w, r)
	if !found {
		return
	}
	var req teamHandoffDeliverAgainRequest
	if r.ContentLength != 0 {
		if err := decodeManagedJSON(w, r, &req); err != nil {
			writeHandoffError(w, http.StatusBadRequest, handoffCodeInvalid, "body must be a deliver-again request (ticket)")
			return
		}
	}
	if teamwire.HandoffTerminal(h.State) {
		code := teamwire.HandoffTerminalCode(h.State)
		writeHandoffError(w, http.StatusConflict, code, handoffOpenRefusalSentence(code))
		return
	}
	ticketID, refused := deliverAgainTicket(w, h, req.Ticket)
	if refused {
		return
	}
	armed, err := governor.ix.DeliverHandoffBriefAgain(ticketID, team.now().Unix(), handoffBriefPolicy())
	var refusal *store.HandoffRefusal
	switch {
	case errors.As(err, &refusal):
		writeHandoffError(w, http.StatusConflict, refusal.Code, handoffOpenRefusalSentence(refusal.Code))
		return
	case err != nil:
		writeHandoffError(w, http.StatusInternalServerError, "", "the brief could not be armed")
		return
	}
	after, _, err := governor.ix.HandoffOpenByTicket(ticketID)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the open could not be read: "+err.Error())
		return
	}
	writeJSON(w, teamHandoffDeliverAgainResponse{Ticket: handoffOpenView(after, true), Armed: armed})
}

// deliverAgainTicket settles which opened session a deliver-again is for.
func deliverAgainTicket(w http.ResponseWriter, h store.Handoff, named string) (ticketID string, refused bool) {
	if named != "" {
		ticket, ok := handoffTicketOf(w, h, named)
		return ticket.TicketID, !ok
	}
	tickets, err := governor.ix.HandoffOpensFor(h.OrganizationID, h.ID)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the opens could not be read: "+err.Error())
		return "", true
	}
	claimed := []string{}
	for _, ticket := range tickets {
		if ticket.State == store.HandoffOpenClaimed {
			claimed = append(claimed, ticket.TicketID)
		}
	}
	switch len(claimed) {
	case 0:
		writeHandoffError(w, http.StatusConflict, store.HandoffCodeNotClaimed, handoffOpenRefusalSentence(store.HandoffCodeNotClaimed))
		return "", true
	case 1:
		return claimed[0], false
	}
	writeHandoffError(w, http.StatusBadRequest, handoffCodeWhichSession, "more than one session started for this handoff; name the one (ticket)")
	return "", true
}

// teamHandoffClaimedResponse is GET /api/team/handoffs/claimed: the full document and
// its excerpt, for the session that claimed the ticket and no other caller. From is
// the sender as this device knows them.
type teamHandoffClaimedResponse struct {
	HandoffID string                 `json:"handoff_id"`
	From      teamHandoffMember      `json:"from"`
	Local     bool                   `json:"local,omitempty"`
	State     string                 `json:"state"`
	Document  teamwire.HandoffRecord `json:"document"`
}

// Refusal codes of the claimed route.
const (
	handoffCodeCallerUnidentified = "caller_unidentified"
	handoffCodeNotTheOpenedCaller = "not_the_opened_session"
)

// handleTeamHandoffClaimed backs the recall tool get_handoff (§6.5 "The rest"). The
// daemon answers only when the caller's (runtime, native id) is a ticket's claimed
// session and the handoff was not withdrawn before that session was handed the brief.
// A caller with no identity is refused caller_unidentified; any other caller is
// refused. This is an addressing rule, not a confidentiality boundary: the route is
// loopback under the daemon's token, and on Claude a session cleared in the same MCP
// process still presents the claimed id (plan §17.1 F-5).
func handleTeamHandoffClaimed(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	runtime := strings.TrimSpace(r.URL.Query().Get("caller_runtime"))
	nativeID := strings.TrimSpace(r.URL.Query().Get("caller_id"))
	if runtime == "" || nativeID == "" {
		writeHandoffError(w, http.StatusForbidden, handoffCodeCallerUnidentified, "the calling session is not identified, so no handoff can be addressed to it")
		return
	}
	ticket, h, found, err := governor.ix.HandoffOpenForSession(runtime, nativeID)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the handoff could not be read: "+err.Error())
		return
	}
	if !found {
		writeHandoffError(w, http.StatusForbidden, handoffCodeNotTheOpenedCaller, "this session was not opened for a handoff")
		return
	}
	if h.State == teamwire.HandoffWithdrawn && ticket.BriefConfirmedAt == 0 {
		writeHandoffError(w, http.StatusConflict, teamwire.CodeHandoffWithdrawn, handoffOpenRefusalSentence(teamwire.CodeHandoffWithdrawn))
		return
	}
	rec, held, err := h.Document()
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the stored handoff could not be read")
		return
	}
	if !held {
		writeHandoffError(w, http.StatusConflict, store.HandoffCodeNoText, handoffOpenRefusalSentence(store.HandoffCodeNoText))
		return
	}
	writeJSON(w, teamHandoffClaimedResponse{HandoffID: h.ID, From: teamHandoffMember{UserID: h.PeerUserID, DisplayName: h.PeerName},
		Local: h.Local(), State: h.State, Document: rec})
}

// teamHandoffSessionResponse is GET /api/team/handoffs/session: whether a session on
// this device was opened for a handoff, and the facts its header shows — "continues
// <title> from <sender>". The session is named by runtime and native id.
type teamHandoffSessionResponse struct {
	Opened    bool               `json:"opened"`
	HandoffID string             `json:"handoff_id,omitempty"`
	Title     string             `json:"title,omitempty"`
	From      *teamHandoffMember `json:"from,omitempty"`
	Local     bool               `json:"local,omitempty"`
	State     string             `json:"state,omitempty"`
	Ticket    *teamHandoffOpen   `json:"ticket,omitempty"`
}

func handleTeamHandoffSession(w http.ResponseWriter, r *http.Request) {
	if team == nil || governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	runtime := strings.TrimSpace(r.URL.Query().Get("runtime"))
	nativeID := strings.TrimSpace(r.URL.Query().Get("native_id"))
	if runtime == "" || nativeID == "" {
		writeHandoffError(w, http.StatusBadRequest, handoffCodeInvalid, "name the session by runtime and native_id")
		return
	}
	ticket, h, found, err := governor.ix.HandoffOpenForSession(runtime, nativeID)
	if err != nil {
		writeHandoffError(w, http.StatusInternalServerError, "", "the handoff could not be read: "+err.Error())
		return
	}
	if !found {
		writeJSON(w, teamHandoffSessionResponse{})
		return
	}
	view := handoffOpenView(ticket, !teamwire.HandoffTerminal(h.State))
	writeJSON(w, teamHandoffSessionResponse{Opened: true, HandoffID: h.ID, Title: h.Title, Local: h.Local(), State: h.State,
		From: &teamHandoffMember{UserID: h.PeerUserID, DisplayName: h.PeerName}, Ticket: &view})
}

// ---------- memory recall (OD-22, OD-24) ----------

// registerMemoryRecallRoutes registers the memory-recall attach action.
func registerMemoryRecallRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/memory/attach", handleMemoryRecallState)
	mux.HandleFunc("POST /api/memory/attach", handleMemoryRecallAttach)
}

// Why this daemon may not edit a runtime's settings for memory recall.
const (
	memoryRecallNotThisInstallation = "not_this_installation"
	// memoryRecallOtherSettings: the memory hook's settings file is not in the
	// settings the ownership check read, so nothing proves it is this installation's.
	memoryRecallOtherSettings   = "settings_not_checked"
	memoryRecallNoLifecycleHook = "no_lifecycle_hook"
	memoryRecallUnknownRuntime  = "unknown_runtime"
	memoryRecallConsentRequired = "consent_required"
	memoryRecallInvalid         = "invalid_request"
)

// memoryRecallOwnership is OD-24's rule: a daemon may attach or detach for a runtime
// only when that home's existing lifecycle-hook entry for the runtime names this
// daemon's own executable. A development build pointed at someone's real home is
// refused, and the sentence says why. It returns "" when the daemon may.
func memoryRecallOwnership(runtime string) (code, sentence string) {
	self, err := daemonExecutable()
	if err != nil {
		return memoryRecallNotThisInstallation, "this daemon's own executable could not be resolved, so it cannot show that the installation is its own"
	}
	switch ownership, binary := guardcli.LifecycleHookOwner(runtime, self); ownership {
	case "self":
		return memoryRecallWritesWhatWasChecked(runtime)
	case "none":
		return memoryRecallNoLifecycleHook, "this home has no Crossing Guard hook for " + runtime + "; connect the runtime first"
	default:
		return memoryRecallNotThisInstallation, "this home's " + runtime + " hook runs " + binary + ", not this daemon (" + self +
			"); only the installation that owns the hook may change the runtime's settings"
	}
}

// memoryRecallWritesWhatWasChecked refuses when the file an attach or a detach would
// write is not the settings the ownership check read. The two can differ: the write
// goes to the path an earlier attach recorded, when there is one, and that record may
// name a file anywhere (the `memory attach` verb takes a path). Ownership proved for
// one home's settings says nothing about another file.
func memoryRecallWritesWhatWasChecked(runtime string) (code, sentence string) {
	attachment, err := memcli.RuntimeRecallAttachment(runtime)
	if err != nil {
		return memoryRecallUnknownRuntime, "this runtime has no memory recall hook"
	}
	checked := guardcli.LifecycleHookConfig(runtime)
	if settingsPathWithin(attachment.ConfigPath, checked) {
		return "", ""
	}
	return memoryRecallOtherSettings, "memory recall for " + runtime + " was set up in " + attachment.ConfigPath +
		", which is not the settings this daemon's hook was checked in (" + checked +
		"); `crossing-guard attach " + runtime + "` sets it up in this home's own settings"
}

// settingsPathWithin reports whether path is the checked settings file itself, or a
// file directly inside the checked settings directory.
func settingsPathWithin(path, checked string) bool {
	if path == "" || checked == "" {
		return false
	}
	path, checked = resolvedPath(path), resolvedPath(checked)
	return path == checked || filepath.Dir(path) == checked
}

// resolvedPath is a path cleaned, with symbolic links followed as far as the path
// exists (a settings file may not exist yet; its directory's links still count).
func resolvedPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path))
	}
	return path
}

// memoryRecallState reads one runtime's memory recall.
func memoryRecallState(runtime string, recentSessions int) teamHandoffRecall {
	out := teamHandoffRecall{Runtime: runtime, Offers: []string{}}
	attachment, err := memcli.RuntimeRecallAttachment(runtime)
	if err != nil {
		out.WithheldCode, out.Withheld = memoryRecallUnknownRuntime, "this runtime has no memory recall hook"
		return out
	}
	out.EntryPresent, out.AddedByAction, out.ConfigPath = attachment.EntryPresent, attachment.AddedByAction, attachment.ConfigPath
	out.SessionsChecked, out.SessionsInjected = memcli.RecallObserved(runtime, recentSessions)
	out.ObservedInjecting = out.SessionsInjected > 0
	if code, sentence := memoryRecallOwnership(runtime); code != "" {
		out.WithheldCode, out.Withheld = code, sentence
		return out
	}
	if !attachment.EntryPresent {
		out.Offers = append(out.Offers, memoryRecallAttach)
	}
	if attachment.AddedByAction {
		out.Offers = append(out.Offers, memoryRecallDetach)
	}
	return out
}

// memoryRecallStateResponse is GET /api/memory/attach.
type memoryRecallStateResponse struct {
	Runtimes []teamHandoffRecall `json:"runtimes"`
}

func handleMemoryRecallState(w http.ResponseWriter, _ *http.Request) {
	config, _ := consoleConfig()
	out := memoryRecallStateResponse{Runtimes: []teamHandoffRecall{}}
	for _, runtime := range memcli.RecallRuntimes() {
		out.Runtimes = append(out.Runtimes, memoryRecallState(runtime, config.Handoff.RecallCheckSessions))
	}
	writeJSON(w, out)
}

// The two actions of POST /api/memory/attach.
const (
	memoryRecallAttach = "attach"
	memoryRecallDetach = "detach"
)

// memoryRecallRequest is POST /api/memory/attach. Consent is the person's click on
// "Turn on memory recall for <runtime>" (or Turn off): without it nothing is written.
type memoryRecallRequest struct {
	Runtime string `json:"runtime"`
	Action  string `json:"action"`
	Consent bool   `json:"consent"`
}

// memoryRecallResponse says what the action did and the state afterwards. Changed is
// false when the settings file was left as it was; Code then says why: the entry was
// already present, or it is not one the action added.
type memoryRecallResponse struct {
	Action  string            `json:"action"`
	Changed bool              `json:"changed"`
	Code    string            `json:"code,omitempty"`
	State   teamHandoffRecall `json:"state"`
}

// memoryRecallAlreadyPresent is the code of an attach that found the entry there.
const memoryRecallAlreadyPresent = "already_present"

// handleMemoryRecallAttach turns memory recall on or off for a runtime (OD-22): the
// existing memory hook, through the existing adapter, in that runtime's settings.
// 400 without consent — nothing is written; 409 when the installation is not this
// daemon's (OD-24).
func handleMemoryRecallAttach(w http.ResponseWriter, r *http.Request) {
	var req memoryRecallRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || (req.Action != memoryRecallAttach && req.Action != memoryRecallDetach) {
		writeHandoffError(w, http.StatusBadRequest, memoryRecallInvalid, "body must name a runtime, an action (attach or detach) and consent")
		return
	}
	if !slices.Contains(memcli.RecallRuntimes(), req.Runtime) {
		writeHandoffError(w, http.StatusNotFound, memoryRecallUnknownRuntime, "this runtime has no memory recall hook")
		return
	}
	if !req.Consent {
		writeHandoffError(w, http.StatusBadRequest, memoryRecallConsentRequired, "changing a runtime's settings needs the person's consent; nothing was written")
		return
	}
	if code, sentence := memoryRecallOwnership(req.Runtime); code != "" {
		writeHandoffError(w, http.StatusConflict, code, sentence)
		return
	}
	out := memoryRecallResponse{Action: req.Action}
	if req.Action == memoryRecallAttach {
		// The path written is the one the ownership check just proved the home's
		// lifecycle hook names: this daemon's own executable.
		self, err := daemonExecutable()
		if err != nil {
			writeHandoffError(w, http.StatusInternalServerError, "", "this daemon's own executable could not be resolved")
			return
		}
		result, err := memcli.AttachRuntime(req.Runtime, self)
		if err != nil {
			writeHandoffError(w, http.StatusInternalServerError, "", "memory recall could not be turned on: "+err.Error())
			return
		}
		if out.Changed = result.Wrote; !result.Wrote {
			out.Code = memoryRecallAlreadyPresent
		}
	} else {
		result, err := memcli.DetachRuntime(req.Runtime)
		if err != nil {
			writeHandoffError(w, http.StatusInternalServerError, "", "memory recall could not be turned off: "+err.Error())
			return
		}
		out.Changed, out.Code = result.Removed, result.LeftAlone
	}
	config, _ := consoleConfig()
	out.State = memoryRecallState(req.Runtime, config.Handoff.RecallCheckSessions)
	log.Printf("memory recall: %s for %s (changed=%v %s)", req.Action, req.Runtime, out.Changed, out.Code)
	writeJSON(w, out)
}
