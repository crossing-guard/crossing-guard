package daemon

// The approvals inbox (gui-design §10.1; PRD gate 6): when a live hook hits
// an ask / confirm-and-record rule, the blocking decision arrives HERE —
// replacing the macOS-only osascript dialog. The hook long-polls
// /api/approvals/request inside its own timeout budget and FAILS CLOSED if
// no decision arrives; the runtime's own hook timeout (fail-open on every
// runtime) sits behind that, which is why the card shows a deadline and an
// after-deadline decision is recorded advisory-late, never enforced.
//
// Records keep observed vs claimed separated: what the system saw (rule,
// tags, command) is observed; the typed override reason is claimed —
// attributed, logged, and it never un-taints the session.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"crossing-guard/internal/approvalchoice"
	"crossing-guard/internal/guardcli"
)

// maxApprovalRequestBudget is a defensive ceiling on any client's requested
// timeout_ms — the hook itself never asks for more than guardcli.MaxAskBudget,
// but a stray client must not be able to pin a slot open indefinitely.
const maxApprovalRequestBudget = 10 * time.Minute

// maxApprovalRequestBytes bounds the whole request body. It is a transport
// ceiling rather than an operator preference, so it stays compiled; the display,
// prompt, hold and history ceilings live in approvals.json.
const maxApprovalRequestBytes = 64 << 10

type ApprovalOrigin string

const (
	ApprovalOriginPolicyHook  ApprovalOrigin = "policy_hook"
	ApprovalOriginRuntimeTool ApprovalOrigin = "runtime_tool"
)

type Approval struct {
	ID               string         `json:"id"`
	CreatedAt        string         `json:"created_at"`
	Deadline         string         `json:"deadline"`
	Origin           ApprovalOrigin `json:"origin"`
	Session          string         `json:"session,omitempty"`
	Runtime          string         `json:"runtime,omitempty"`
	TaskID           string         `json:"task_id,omitempty"`
	CatalogSessionID string         `json:"catalog_session_id,omitempty"`
	NativeSessionID  string         `json:"native_session_id,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolName         string         `json:"tool_name,omitempty"`
	Rule             string         `json:"rule,omitempty"`
	Mode             string         `json:"mode"` // hard-block | confirm-and-record | ask (legacy guard)
	Message          string         `json:"message,omitempty"`
	Command          string         `json:"command,omitempty"` // the guarded action (daemon-side redaction applied)
	Summary          string         `json:"summary,omitempty"` // bounded provider-owned display summary
	// Prompts are the questions the held call is asking its approver. An
	// operative allow on an approval that has prompts must answer all of them.
	// PromptsCompleteness says whether what the requester sent was carried
	// ("complete"), dropped for exceeding the configured ceilings
	// ("truncated"), or absent (empty).
	Prompts             []approvalchoice.ChoicePrompt `json:"prompts,omitempty"`
	PromptsCompleteness string                        `json:"prompts_completeness,omitempty"`
	FiredTags           []string                      `json:"fired_tags,omitempty"`
	Boundary            string                        `json:"boundary,omitempty"` // the compiled honest label (P-COMPILE-2)

	Status    string             `json:"status"` // pending | allowed | denied | expired
	Reason    string             `json:"reason,omitempty"`
	DecidedAt string             `json:"decided_at,omitempty"`
	Responses []ApprovalResponse `json:"responses,omitempty"`
	// Late is set when a decision arrived AFTER the hook's budget lapsed:
	// recorded + attributed, but the hook already failed closed — advisory.
	Late bool `json:"late,omitempty"`
}

type approvalRequest struct {
	Origin           ApprovalOrigin                `json:"origin,omitempty"`
	Session          string                        `json:"session,omitempty"`
	Runtime          string                        `json:"runtime,omitempty"`
	TaskID           string                        `json:"task_id,omitempty"`
	CatalogSessionID string                        `json:"catalog_session_id,omitempty"`
	NativeSessionID  string                        `json:"native_session_id,omitempty"`
	ToolCallID       string                        `json:"tool_call_id,omitempty"`
	ToolName         string                        `json:"tool_name,omitempty"`
	Rule             string                        `json:"rule,omitempty"`
	Mode             string                        `json:"mode,omitempty"`
	Message          string                        `json:"message,omitempty"`
	Command          string                        `json:"command,omitempty"`
	Summary          string                        `json:"summary,omitempty"`
	Prompts          []approvalchoice.ChoicePrompt `json:"prompts,omitempty"`
	FiredTags        []string                      `json:"fired_tags,omitempty"`
	Boundary         string                        `json:"boundary,omitempty"`
	TimeoutMS        int                           `json:"timeout_ms"`
}

type approvalsHub struct {
	mu                 sync.Mutex
	pending            map[string]*Approval
	waiters            map[string]chan *Approval // hook long-polls, one waiter per id
	history            []*Approval               // decided/expired, newest first, capped
	streams            map[chan []byte]bool      // SSE clients
	responderGrants    map[string]map[string]approvalResponderCapability
	responseIDs        map[string]string
	pendingObservers   map[uint64]approvalPendingObserver
	nextObserverID     uint64
	now                func() time.Time
	newResponseID      func() string
	newCapabilityNonce func() ([32]byte, error)
}

func newApprovalsHub() *approvalsHub {
	return &approvalsHub{
		pending:            map[string]*Approval{},
		waiters:            map[string]chan *Approval{},
		streams:            map[chan []byte]bool{},
		responderGrants:    map[string]map[string]approvalResponderCapability{},
		responseIDs:        map[string]string{},
		pendingObservers:   map[uint64]approvalPendingObserver{},
		now:                defaultApprovalNow,
		newResponseID:      mintApprovalResponseID,
		newCapabilityNonce: mintApprovalCapabilityNonce,
	}
}

func (h *approvalsHub) subscribePending(observer approvalPendingObserver) func() {
	if observer == nil {
		return func() {}
	}
	h.mu.Lock()
	h.nextObserverID++
	id := h.nextObserverID
	h.pendingObservers[id] = observer
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.pendingObservers, id)
		h.mu.Unlock()
	}
}

func (h *approvalsHub) notifyPending(approval Approval) {
	h.mu.Lock()
	observers := make([]approvalPendingObserver, 0, len(h.pendingObservers))
	for _, observer := range h.pendingObservers {
		observers = append(observers, observer)
	}
	h.mu.Unlock()
	issuer := func(responder ApprovalResponder) (approvalResponderCapability, error) {
		return h.grantServiceResponder(approval.ID, responder)
	}
	for _, observer := range observers {
		observer(approval, issuer)
	}
}

var approvals = newApprovalsHub()

func (h *approvalsHub) broadcast(kind string, a *Approval) {
	msg, _ := json.Marshal(map[string]any{"kind": kind, "approval": a})
	h.mu.Lock()
	for ch := range h.streams {
		select {
		case ch <- msg:
		default: // disconnect a stalled client so its reconnect receives a snapshot
			delete(h.streams, ch)
			close(ch)
		}
	}
	h.mu.Unlock()
}

func decodeApprovalRequest(w http.ResponseWriter, r *http.Request) (approvalRequest, error) {
	var in approvalRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxApprovalRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid approval request: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return in, fmt.Errorf("approval request must contain exactly one JSON object")
	}
	return in, nil
}

func normalizeApprovalRequest(in approvalRequest) (approvalRequest, time.Duration, error) {
	if in.Origin == "" {
		in.Origin = ApprovalOriginPolicyHook
	}
	trim := func(value string, max int, field string) (string, error) {
		value = strings.TrimSpace(value)
		if len(value) > max {
			return "", fmt.Errorf("%s is too long", field)
		}
		return value, nil
	}
	var err error
	if in.Runtime, err = trim(in.Runtime, 64, "runtime"); err != nil {
		return in, 0, err
	}
	if in.Session, err = trim(in.Session, 1000, "session"); err != nil {
		return in, 0, err
	}
	if in.TaskID, err = trim(in.TaskID, 128, "task_id"); err != nil {
		return in, 0, err
	}
	if in.CatalogSessionID, err = trim(in.CatalogSessionID, 1000, "catalog_session_id"); err != nil {
		return in, 0, err
	}
	if in.NativeSessionID, err = trim(in.NativeSessionID, 1000, "native_session_id"); err != nil {
		return in, 0, err
	}
	if in.ToolCallID, err = trim(in.ToolCallID, 512, "tool_call_id"); err != nil {
		return in, 0, err
	}
	if in.ToolName, err = trim(in.ToolName, 256, "tool_name"); err != nil {
		return in, 0, err
	}
	if in.Rule, err = trim(in.Rule, 256, "rule"); err != nil {
		return in, 0, err
	}
	if in.Mode, err = trim(in.Mode, 64, "mode"); err != nil {
		return in, 0, err
	}
	if in.Message, err = trim(in.Message, 4096, "message"); err != nil {
		return in, 0, err
	}
	if in.Boundary, err = trim(in.Boundary, 2048, "boundary"); err != nil {
		return in, 0, err
	}
	config := activeApprovalsConfig()
	if len(in.Summary) > config.MaxSummaryBytes {
		return in, 0, fmt.Errorf("summary is too long")
	}
	if len(in.Command) > config.MaxSummaryBytes {
		return in, 0, fmt.Errorf("command is too long")
	}
	// A malformed question is a bug in the requesting adapter and is refused.
	// Questions that merely exceed the operator's ceilings are not refused here;
	// carriedPrompts drops them and says so.
	if err := approvalchoice.CheckPromptShape(in.Prompts); err != nil {
		return in, 0, fmt.Errorf("prompts are not answerable: %w", err)
	}
	if len(in.FiredTags) > 128 {
		return in, 0, fmt.Errorf("too many fired_tags")
	}

	var fallback time.Duration
	switch in.Origin {
	case ApprovalOriginPolicyHook:
		if in.Rule == "" {
			return in, 0, fmt.Errorf("rule required for policy_hook")
		}
		if in.Mode == "" {
			in.Mode = "ask"
		}
		fallback = guardcli.MaxAskBudget
	case ApprovalOriginRuntimeTool:
		if in.Runtime == "" || in.TaskID == "" || in.ToolCallID == "" || in.ToolName == "" {
			return in, 0, fmt.Errorf("runtime, task_id, tool_call_id, and tool_name required for runtime_tool")
		}
		in.Mode = "ask"
		fallback = config.runtimeToolTimeout()
	default:
		return in, 0, fmt.Errorf("unsupported approval origin")
	}
	budget := time.Duration(in.TimeoutMS) * time.Millisecond
	if budget <= 0 || budget > maxApprovalRequestBudget {
		budget = fallback
	}
	return in, budget, nil
}

func approvalAttentionText(a *Approval) string {
	if a.Origin == ApprovalOriginRuntimeTool {
		return fmt.Sprintf("Approval needed: %s (%s)", a.ToolName, a.Runtime)
	}
	return fmt.Sprintf("Approval needed: %s (%s)", a.Rule, a.Mode)
}

// POST /api/approvals/request blocks the requesting policy hook or runtime
// callback until the canonical human decision lands or its own deadline expires.
func handleApprovalRequest(w http.ResponseWriter, r *http.Request) {
	in, err := decodeApprovalRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	in, budget, err := normalizeApprovalRequest(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := mintApprovalID()
	now := time.Now().UTC()
	a := &Approval{
		ID: id, CreatedAt: now.Format(time.RFC3339),
		Deadline: now.Add(budget).Format(time.RFC3339), Origin: in.Origin,
		Session: in.Session, Runtime: in.Runtime, TaskID: in.TaskID,
		CatalogSessionID: in.CatalogSessionID, NativeSessionID: in.NativeSessionID,
		ToolCallID: in.ToolCallID, ToolName: in.ToolName,
		Rule: in.Rule, Mode: in.Mode, Message: in.Message,
		Command: redactSecrets(in.Command), Summary: redactSecrets(in.Summary),
		FiredTags: in.FiredTags, Boundary: in.Boundary, Status: "pending",
	}
	a.Prompts, a.PromptsCompleteness = carriedPrompts(in.Prompts, activeApprovalsConfig())
	wait := make(chan *Approval, 1)
	if err := approvals.admit(a, wait); err != nil {
		http.Error(w, "could not prepare approval", http.StatusInternalServerError)
		return
	}
	snap := cloneApproval(a)
	approvals.broadcast("pending", &snap)
	approvals.notifyPending(snap)
	approvalAttention.Notify(approvalAttentionText(a))

	select {
	case decided := <-wait:
		writeApprovalWaitResult(w, id, decided)
	case <-time.After(budget):
		// a decision landing exactly at the deadline may have won the race:
		// drain the waiter before declaring expiry so the enforced outcome
		// and the record can't diverge (red-team finding)
		select {
		case decided := <-wait:
			writeApprovalWaitResult(w, id, decided)
			return
		default:
		}
		approvals.finishExpired(a)
		writeJSON(w, expiredApprovalResult(id))
	case <-r.Context().Done(): // requesting hook/runtime process died — nothing to answer
		approvals.finishExpired(a)
	}
}

// ApprovalWaitResult is what a blocked requester receives when its hold ends. It is
// a declared type rather than a map literal because a runtime adapter on the other
// end has to build its own reply from these exact fields.
type ApprovalWaitResult struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
	Note     string `json:"note,omitempty"`
	// Selections is the approver's answer, present only when the held call was
	// asking questions and the answer became operative. PromptsCompleteness says
	// what the inbox did with the questions that were sent, so an adapter can
	// tell its session "nobody saw your options" rather than leaving it to
	// conclude that nobody answered.
	Selections          []approvalchoice.ChoiceSelection `json:"selections,omitempty"`
	PromptsCompleteness string                           `json:"prompts_completeness,omitempty"`
}

func expiredApprovalResult(id string) ApprovalWaitResult {
	return ApprovalWaitResult{ID: id, Decision: "expired",
		Note: "no decision in time — the request failed closed; a later decision cannot affect it"}
}

func writeApprovalWaitResult(w http.ResponseWriter, id string, approval *Approval) {
	if approval.Status == "expired" {
		writeJSON(w, expiredApprovalResult(id))
		return
	}
	result := ApprovalWaitResult{ID: id, Decision: approval.Status, Reason: approval.Reason,
		PromptsCompleteness: approval.PromptsCompleteness}
	if response := operativeApprovalResponse(approval); response != nil {
		result.Selections = response.Selections
	}
	writeJSON(w, result)
}

// carriedPrompts applies the operator's ceilings. Prompts that fit are carried and
// marked complete; prompts that do not are dropped whole and marked truncated,
// because showing half a question would invite an answer to the wrong thing.
func carriedPrompts(prompts []approvalchoice.ChoicePrompt, config ApprovalsConfig) ([]approvalchoice.ChoicePrompt, string) {
	if len(prompts) == 0 {
		return nil, ""
	}
	limits := config.choiceLimits()
	encoded, err := json.Marshal(prompts)
	if err != nil || len(encoded) > limits.MaxWireBytes ||
		!approvalchoice.PromptsWithinLimits(prompts, limits) {
		return nil, "truncated"
	}
	carried := append([]approvalchoice.ChoicePrompt(nil), prompts...)
	if limits.MaxFreeTextBytes == 0 {
		for index := range carried {
			carried[index].FreeText = false
		}
	}
	return carried, "complete"
}

// pendingForSession counts pending approvals that belong to one session by
// exact identity — the browser's former rule, ported verbatim: a task id is
// exact only inside the catalog session the approval itself asserts, and can
// never override an explicit catalog mismatch; a native id matches only when
// the catalog id IS the native id.
func (h *approvalsHub) pendingForSession(runtime, catalogID, nativeID string, taskIDs map[string]bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, approval := range h.pending {
		if approval == nil || approval.Status != "pending" || approval.Runtime != runtime {
			continue
		}
		if approval.CatalogSessionID != "" && approval.CatalogSessionID != catalogID {
			continue
		}
		switch {
		case approval.TaskID != "" && taskIDs[approval.TaskID]:
			count++
		case approval.CatalogSessionID != "":
			count++
		case catalogID == nativeID && approval.NativeSessionID == nativeID:
			count++
		}
	}
	return count
}

// refoldForApproval republishes the session an approval belongs to. A decided
// or expired approval must clear the amber dot without waiting for a tick.
func refoldForApproval(approval *Approval) {
	if approval == nil {
		return
	}
	id := approval.NativeSessionID
	if id == "" {
		id = approval.CatalogSessionID
	}
	if id == "" {
		id = approval.Session
	}
	sessionStatusRefold(approval.Runtime, id)
}

// finishExpired marks a pending approval expired (no waiter handoff — the
// long-poll side is the caller or is gone). No-op if already decided.
func (h *approvalsHub) finishExpired(a *Approval) {
	h.mu.Lock()
	snap, _, expired := h.expireLocked(a, h.now())
	h.mu.Unlock()
	if !expired {
		return
	}
	h.broadcast("decided", snap)
	appendApprovalRecord(snap)
	refoldForApproval(snap)
}

// POST /api/approvals/decision — the human act, from the console.
func handleApprovalDecision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID         string                           `json:"id"`
		Decision   string                           `json:"decision"` // allow | deny
		Reason     string                           `json:"reason"`
		Selections []approvalchoice.ChoiceSelection `json:"selections,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.ID == "" ||
		(in.Decision != "allow" && in.Decision != "deny") {
		http.Error(w, "id and decision (allow|deny) required", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "decision must contain exactly one JSON object", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > 4096 {
		http.Error(w, "reason is too long", http.StatusBadRequest)
		return
	}
	capability, lookup := approvals.takeoverInteractiveCapability(in.ID)
	if lookup == approvalResponseUnknown {
		http.Error(w, "no such approval (pending or recent history)", http.StatusNotFound)
		return
	}
	if lookup == approvalResponseAlreadyResolved {
		http.Error(w, "already decided", http.StatusConflict)
		return
	}
	outcome := approvals.respond(approvalResponseCommand{
		approvalID: in.ID,
		responseID: approvals.newResponseID(),
		responder:  interactiveConsoleResponder,
		decision:   in.Decision,
		reason:     reason,
		selections: in.Selections,
		submitted:  approvals.now(),
		capability: capability,
	})
	switch outcome.kind {
	case approvalResponseAccepted:
		// A decided approval must clear the amber dot now, not on the next tick.
		refoldForApproval(outcome.approval)
		writeJSON(w, map[string]any{"result": outcome.approval.Status})
	case approvalResponseLateAdvisory:
		writeJSON(w, map[string]any{"result": "advisory-late — recorded; the request already failed closed at the deadline"})
	case approvalResponseHardBlock:
		message := "non-overridable — Restricted class (hard-block)"
		if outcome.approval != nil && outcome.approval.Status == "expired" {
			message += "; applies to advisory records too"
		}
		http.Error(w, message, http.StatusForbidden)
	case approvalResponseReasonRequired:
		http.Error(w, "an override requires a typed reason (attributed, recorded, does not un-taint)", http.StatusUnprocessableEntity)
	case approvalResponseSelectionRequired:
		http.Error(w, "this request is asking a question: "+outcome.detail, http.StatusUnprocessableEntity)
	case approvalResponseAlreadyResolved:
		http.Error(w, "already decided", http.StatusConflict)
	case approvalResponseUnknown:
		http.Error(w, "no such approval (pending or recent history)", http.StatusNotFound)
	case approvalResponseUnauthorized:
		http.Error(w, "approval response was not authorized", http.StatusForbidden)
	default:
		http.Error(w, "invalid approval response", http.StatusBadRequest)
	}
}

// GET /api/approvals — pending (oldest first: they're deadlines) + history.
// Value snapshots are taken under the lock: history entries can gain an
// advisory-late verdict concurrently.
func handleApprovalsList(w http.ResponseWriter, r *http.Request) {
	approvals.mu.Lock()
	pending := make([]Approval, 0, len(approvals.pending))
	for _, a := range approvals.pending {
		pending = append(pending, cloneApproval(a))
	}
	hist := make([]Approval, 0, len(approvals.history))
	for _, a := range approvals.history {
		hist = append(hist, cloneApproval(a))
	}
	approvals.mu.Unlock()
	for i := 0; i < len(pending); i++ { // small n; deadline order
		for j := i + 1; j < len(pending); j++ {
			if pending[j].Deadline < pending[i].Deadline {
				pending[i], pending[j] = pending[j], pending[i]
			}
		}
	}
	writeJSON(w, map[string]any{"pending": pending, "history": hist})
}

// GET /api/approvals/stream — SSE push for the badge + inbox.
// approvalLegacyKeepalive is the single-feed route's comment cadence; the
// multiplexed route reads its cadence from the session-stream configuration.
const approvalLegacyKeepalive = 25 * time.Second

func handleApprovalsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	snapshot, ch, unsubscribe := approvals.subscribe()
	defer unsubscribe()
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", snapshot)
	fl.Flush()
	keepalive := time.NewTicker(approvalLegacyKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: approval\ndata: %s\n\n", msg)
			fl.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// appendApprovalRecord writes the observed/claimed record to the decision
// log the policy view already tails (daemon-side — the "daemon fires the
// notify and owns the record" fact from the checkpoint design).
func appendApprovalRecord(a *Approval) {
	path := filepath.Join(homeDir(), ".crossing-guard", "policy", "decisions.jsonl")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	rec := map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "kind": "approval",
		"id": a.ID, "origin": a.Origin, "rule": a.Rule, "mode": a.Mode, "decision": a.Status,
		"session": a.Session, "via": approvalRecordVia(a),
		"observed": map[string]any{"command": a.Command, "fired_tags": a.FiredTags,
			"boundary": a.Boundary, "summary": a.Summary, "runtime": a.Runtime,
			"task_id": a.TaskID, "catalog_session_id": a.CatalogSessionID,
			"native_session_id": a.NativeSessionID, "tool_call_id": a.ToolCallID,
			"tool_name": a.ToolName},
		"untainted": false,
	}
	if a.Reason != "" {
		rec["claimed"] = a.Reason + " (ATTRIBUTED, NOT VERIFIED)"
	}
	if a.Late {
		rec["late"] = true
	}
	if len(a.Responses) != 0 {
		rec["response"] = a.Responses[len(a.Responses)-1]
	}
	b, _ := json.Marshal(rec)
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.Write(append(b, '\n'))
		f.Close()
	}
}

func approvalRecordVia(a *Approval) string {
	if len(a.Responses) != 0 && a.Responses[len(a.Responses)-1].Responder.Kind == "service" {
		return "approval-responder"
	}
	return "console-inbox"
}

// redactSecrets applies generic secret shapes before a command string is
// stored or displayed — the console never shows a raw secret it caught
// (gui-design §3.4; the daemon redacts before serialize).
var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),
}

func redactSecrets(s string) string {
	for _, re := range secretShapes {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			if len(m) > 8 {
				return m[:4] + "…[redacted]"
			}
			return "[redacted]"
		})
	}
	return s
}

func mintApprovalID() string {
	return "ap_" + rand.Text()
}
