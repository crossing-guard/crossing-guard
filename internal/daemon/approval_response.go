package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"crossing-guard/internal/approvalchoice"
)

const (
	maxApprovalResponses = 8
	maxResponderGrants   = 4
	maxResponderIDBytes  = 128
	maxResponseIDBytes   = 128
)

type ApprovalResponder struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type ApprovalResponse struct {
	ID        string            `json:"id"`
	Responder ApprovalResponder `json:"responder"`
	Decision  string            `json:"decision"`
	Reason    string            `json:"reason,omitempty"`
	// Selections is what the responder chose, when the held call was asking
	// questions. It is recorded separately from Reason: the answer is the act,
	// the reason is the claim about it.
	Selections  []approvalchoice.ChoiceSelection `json:"selections,omitempty"`
	SubmittedAt string                           `json:"submitted_at"`
	AcceptedAt  string                           `json:"accepted_at"`
	Disposition string                           `json:"disposition"`
}

// operativeApprovalResponse returns the response that decided this approval, or nil
// when none did. A late advisory response never counts as the deciding one.
func operativeApprovalResponse(approval *Approval) *ApprovalResponse {
	for index := len(approval.Responses) - 1; index >= 0; index-- {
		if approval.Responses[index].Disposition == "operative" {
			return &approval.Responses[index]
		}
	}
	return nil
}

var interactiveConsoleResponder = ApprovalResponder{Kind: "interactive", ID: "local-console"}

var responderIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)

type approvalResponderCapability struct {
	approvalID string
	responder  ApprovalResponder
	nonce      [32]byte
	allow      bool
	deny       bool
}

// approvalPendingObserver is a generic in-process composition seam. The approval
// owner supplies an immutable pending snapshot and a private exact-grant issuer; it
// knows nothing about profiles, models, routes, or orchestration.
type approvalPendingObserver func(Approval, func(ApprovalResponder) (approvalResponderCapability, error))

type approvalResponseCommand struct {
	approvalID string
	responseID string
	responder  ApprovalResponder
	decision   string
	reason     string
	selections []approvalchoice.ChoiceSelection
	submitted  time.Time
	capability approvalResponderCapability
}

type approvalResponseOutcomeKind string

const (
	approvalResponseAccepted          approvalResponseOutcomeKind = "accepted"
	approvalResponseLateAdvisory      approvalResponseOutcomeKind = "late-advisory"
	approvalResponseHardBlock         approvalResponseOutcomeKind = "hard-block"
	approvalResponseReasonRequired    approvalResponseOutcomeKind = "reason-required"
	approvalResponseSelectionRequired approvalResponseOutcomeKind = "selection-required"
	approvalResponseAlreadyResolved   approvalResponseOutcomeKind = "already-resolved"
	approvalResponseUnknown           approvalResponseOutcomeKind = "unknown"
	approvalResponseUnauthorized      approvalResponseOutcomeKind = "unauthorized"
	approvalResponseInvalid           approvalResponseOutcomeKind = "invalid"
)

type approvalResponseOutcome struct {
	kind     approvalResponseOutcomeKind
	approval *Approval
	detail   string
}

type approvalResponseEffects struct {
	waiter     chan *Approval
	waiterSnap *Approval
	broadcasts []*Approval
	records    []*Approval
}

func defaultApprovalNow() time.Time { return time.Now().UTC() }

func mintApprovalResponseID() string { return "apr_" + rand.Text() }

func mintApprovalCapabilityNonce() ([32]byte, error) {
	var nonce [32]byte
	_, err := rand.Read(nonce[:])
	return nonce, err
}

func validResponder(responder ApprovalResponder) bool {
	if responder.Kind != "interactive" && responder.Kind != "service" {
		return false
	}
	return len(responder.ID) <= maxResponderIDBytes && responderIDPattern.MatchString(responder.ID)
}

func validApprovalResponseID(id string) bool {
	if !strings.HasPrefix(id, "apr_") || len(id) > maxResponseIDBytes {
		return false
	}
	suffix := strings.TrimPrefix(id, "apr_")
	if len(suffix) < 26 {
		return false
	}
	for _, r := range suffix {
		if (r < 'A' || r > 'Z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

func responderKey(responder ApprovalResponder) string {
	return responder.Kind + "\x00" + responder.ID
}

func sameApprovalCapability(left, right approvalResponderCapability) bool {
	return left.approvalID == right.approvalID && left.responder == right.responder &&
		left.allow == right.allow && left.deny == right.deny &&
		subtle.ConstantTimeCompare(left.nonce[:], right.nonce[:]) == 1
}

func (capability approvalResponderCapability) permits(decision string) bool {
	return decision == "allow" && capability.allow || decision == "deny" && capability.deny
}

func cloneApproval(approval *Approval) Approval {
	clone := *approval
	clone.FiredTags = append([]string(nil), approval.FiredTags...)
	clone.Responses = append([]ApprovalResponse(nil), approval.Responses...)
	return clone
}

func (h *approvalsHub) addResponderGrantLocked(capability approvalResponderCapability) error {
	key := responderKey(capability.responder)
	grants := h.responderGrants[capability.approvalID]
	if grants == nil {
		grants = map[string]approvalResponderCapability{}
		h.responderGrants[capability.approvalID] = grants
	}
	if _, exists := grants[key]; !exists && len(grants) >= maxResponderGrants {
		return errors.New("approval responder grant limit reached")
	}
	grants[key] = capability
	return nil
}

func (h *approvalsHub) admit(a *Approval, waiter chan *Approval) error {
	if a == nil || a.ID == "" || waiter == nil {
		return errors.New("approval admission requires identity and waiter")
	}
	nonce, err := h.newCapabilityNonce()
	if err != nil {
		return fmt.Errorf("mint approval responder capability: %w", err)
	}
	capability := approvalResponderCapability{
		approvalID: a.ID,
		responder:  interactiveConsoleResponder,
		nonce:      nonce,
		allow:      true,
		deny:       true,
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.pending[a.ID]; exists {
		return errors.New("duplicate approval identity")
	}
	if err := h.addResponderGrantLocked(capability); err != nil {
		return err
	}
	h.pending[a.ID] = a
	h.waiters[a.ID] = waiter
	return nil
}

func (h *approvalsHub) interactiveCapability(id string) (approvalResponderCapability, approvalResponseOutcomeKind) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if grants := h.responderGrants[id]; grants != nil {
		if capability, ok := grants[responderKey(interactiveConsoleResponder)]; ok {
			return capability, ""
		}
	}
	for _, approval := range h.history {
		if approval.ID == id {
			return approvalResponderCapability{}, approvalResponseAlreadyResolved
		}
	}
	return approvalResponderCapability{}, approvalResponseUnknown
}

func (h *approvalsHub) takeoverInteractiveCapability(id string) (approvalResponderCapability, approvalResponseOutcomeKind) {
	h.mu.Lock()
	defer h.mu.Unlock()
	grants := h.responderGrants[id]
	capability, ok := grants[responderKey(interactiveConsoleResponder)]
	if ok {
		for key, grant := range grants {
			if grant.responder.Kind == "service" {
				delete(grants, key)
			}
		}
		return capability, ""
	}
	for _, approval := range h.history {
		if approval.ID == id {
			return approvalResponderCapability{}, approvalResponseAlreadyResolved
		}
	}
	return approvalResponderCapability{}, approvalResponseUnknown
}

func (h *approvalsHub) grantServiceResponder(id string, responder ApprovalResponder) (approvalResponderCapability, error) {
	if responder.Kind != "service" || !validResponder(responder) {
		return approvalResponderCapability{}, errors.New("invalid service responder")
	}
	nonce, err := h.newCapabilityNonce()
	if err != nil {
		return approvalResponderCapability{}, err
	}
	capability := approvalResponderCapability{approvalID: id, responder: responder, nonce: nonce,
		allow: true, deny: true}
	h.mu.Lock()
	defer h.mu.Unlock()
	approval := h.pending[id]
	if approval == nil || approval.Status != "pending" {
		return approvalResponderCapability{}, errors.New("approval is not pending")
	}
	if err := h.addResponderGrantLocked(capability); err != nil {
		return approvalResponderCapability{}, err
	}
	return capability, nil
}

func (h *approvalsHub) revokeServiceResponder(capability approvalResponderCapability) {
	if capability.responder.Kind != "service" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	grants := h.responderGrants[capability.approvalID]
	key := responderKey(capability.responder)
	if current, ok := grants[key]; ok && sameApprovalCapability(current, capability) {
		delete(grants, key)
	}
}

func validateApprovalResponseCommand(command approvalResponseCommand) approvalResponseOutcomeKind {
	if command.approvalID == "" || !validApprovalResponseID(command.responseID) ||
		!validResponder(command.responder) || command.submitted.IsZero() {
		return approvalResponseInvalid
	}
	if command.decision != "allow" && command.decision != "deny" {
		return approvalResponseInvalid
	}
	if len(command.reason) > 4096 {
		return approvalResponseInvalid
	}
	return ""
}

// validateApprovalAnswer judges one answer against this exact approval. Order
// matters: a hard block refuses an allow whatever else the answer carries.
func validateApprovalAnswer(approval *Approval, command approvalResponseCommand) (approvalResponseOutcomeKind, string) {
	if command.decision == "allow" {
		if approval.Mode == "hard-block" {
			return approvalResponseHardBlock, ""
		}
		if approval.Mode == "confirm-and-record" && command.reason == "" {
			return approvalResponseReasonRequired, ""
		}
	}
	if command.decision == "deny" && len(command.selections) != 0 {
		return approvalResponseInvalid, "a denial cannot carry an answer"
	}
	if command.decision != "allow" {
		return "", ""
	}
	limits := activeApprovalsConfig().choiceLimits()
	if err := approvalchoice.ValidateSelections(approval.Prompts, command.selections, limits); err != nil {
		if len(approval.Prompts) == 0 {
			return approvalResponseInvalid, err.Error()
		}
		return approvalResponseSelectionRequired, err.Error()
	}
	return "", ""
}

func findApproval(history []*Approval, id string) *Approval {
	for _, approval := range history {
		if approval.ID == id {
			return approval
		}
	}
	return nil
}

func (h *approvalsHub) addHistoryLocked(approval *Approval) {
	h.history = append([]*Approval{approval}, h.history...)
	for len(h.history) > activeApprovalsConfig().HistoryCap {
		last := len(h.history) - 1
		evicted := h.history[last]
		h.history = h.history[:last]
		delete(h.responderGrants, evicted.ID)
		for _, response := range evicted.Responses {
			delete(h.responseIDs, response.ID)
		}
	}
}

func (h *approvalsHub) expireLocked(approval *Approval, at time.Time) (*Approval, chan *Approval, bool) {
	current, ok := h.pending[approval.ID]
	if !ok || current != approval || approval.Status != "pending" {
		return nil, nil, false
	}
	delete(h.pending, approval.ID)
	waiter := h.waiters[approval.ID]
	delete(h.waiters, approval.ID)
	approval.Status = "expired"
	approval.DecidedAt = at.UTC().Format(time.RFC3339)
	h.addHistoryLocked(approval)
	snapshot := cloneApproval(approval)
	return &snapshot, waiter, true
}

func appendApprovalResponse(approval *Approval, command approvalResponseCommand, accepted time.Time, disposition string) {
	approval.Responses = append(approval.Responses, ApprovalResponse{
		ID:          command.responseID,
		Responder:   command.responder,
		Decision:    command.decision,
		Reason:      command.reason,
		Selections:  command.selections,
		SubmittedAt: command.submitted.UTC().Format(time.RFC3339Nano),
		AcceptedAt:  accepted.UTC().Format(time.RFC3339Nano),
		Disposition: disposition,
	})
	if len(approval.Responses) > maxApprovalResponses {
		approval.Responses = approval.Responses[len(approval.Responses)-maxApprovalResponses:]
	}
}

func (h *approvalsHub) respond(command approvalResponseCommand) approvalResponseOutcome {
	command.reason = strings.TrimSpace(command.reason)
	if invalid := validateApprovalResponseCommand(command); invalid != "" {
		return approvalResponseOutcome{kind: invalid, detail: "invalid approval response"}
	}

	effects := approvalResponseEffects{}
	h.mu.Lock()
	if approval := findApproval(h.history, command.approvalID); approval != nil &&
		(approval.Status != "expired" || approval.Late || len(approval.Responses) != 0) {
		h.mu.Unlock()
		return approvalResponseOutcome{kind: approvalResponseAlreadyResolved, detail: "approval was already resolved"}
	}
	grants := h.responderGrants[command.approvalID]
	grant, granted := grants[responderKey(command.responder)]
	if !granted || !sameApprovalCapability(grant, command.capability) || !grant.permits(command.decision) {
		h.mu.Unlock()
		return approvalResponseOutcome{kind: approvalResponseUnauthorized, detail: "approval response was not authorized"}
	}
	if _, duplicate := h.responseIDs[command.responseID]; duplicate {
		h.mu.Unlock()
		return approvalResponseOutcome{kind: approvalResponseAlreadyResolved, detail: "approval response was already used"}
	}

	approval := h.pending[command.approvalID]
	if approval != nil {
		deadline, err := time.Parse(time.RFC3339Nano, approval.Deadline)
		if err != nil {
			h.mu.Unlock()
			return approvalResponseOutcome{kind: approvalResponseInvalid, detail: "approval deadline was invalid"}
		}
		if !command.submitted.Before(deadline) {
			expiredAt := h.now()
			expired, waiter, ok := h.expireLocked(approval, expiredAt)
			if ok {
				effects.waiter, effects.waiterSnap = waiter, expired
				effects.broadcasts = append(effects.broadcasts, expired)
				effects.records = append(effects.records, expired)
			}
		}
	}

	if approval == nil || approval.Status != "pending" {
		approval = findApproval(h.history, command.approvalID)
		if approval == nil {
			h.mu.Unlock()
			return approvalResponseOutcome{kind: approvalResponseUnknown, detail: "no such approval"}
		}
		if approval.Status != "expired" || approval.Late || len(approval.Responses) != 0 {
			h.mu.Unlock()
			return approvalResponseOutcome{kind: approvalResponseAlreadyResolved, detail: "approval was already resolved"}
		}
		if rejected, detail := validateApprovalAnswer(approval, command); rejected != "" {
			snapshot := cloneApproval(approval)
			h.mu.Unlock()
			runApprovalResponseEffects(h, effects)
			if detail == "" {
				detail = "late approval response was refused"
			}
			return approvalResponseOutcome{kind: rejected, approval: &snapshot, detail: detail}
		}
		acceptedAt := h.now()
		appendApprovalResponse(approval, command, acceptedAt, "late-advisory")
		approval.Late = true
		approval.Reason = command.reason
		h.responseIDs[command.responseID] = approval.ID
		delete(h.responderGrants, approval.ID)
		historySnapshot := cloneApproval(approval)
		record := historySnapshot
		record.Responses = append([]ApprovalResponse(nil), historySnapshot.Responses...)
		record.Status = "advisory-late-" + command.decision
		effects.records = append(effects.records, &record)
		effects.broadcasts = append(effects.broadcasts, &historySnapshot)
		h.mu.Unlock()
		runApprovalResponseEffects(h, effects)
		return approvalResponseOutcome{kind: approvalResponseLateAdvisory, approval: &historySnapshot}
	}

	if rejected, detail := validateApprovalAnswer(approval, command); rejected != "" {
		snapshot := cloneApproval(approval)
		h.mu.Unlock()
		if detail == "" {
			detail = "approval response was refused"
		}
		return approvalResponseOutcome{kind: rejected, approval: &snapshot, detail: detail}
	}
	acceptedAt := h.now()
	status := "denied"
	if command.decision == "allow" {
		status = "allowed"
	}
	delete(h.pending, approval.ID)
	approval.Status = status
	approval.Reason = command.reason
	approval.DecidedAt = acceptedAt.UTC().Format(time.RFC3339)
	appendApprovalResponse(approval, command, acceptedAt, "operative")
	h.addHistoryLocked(approval)
	waiter := h.waiters[approval.ID]
	delete(h.waiters, approval.ID)
	h.responseIDs[command.responseID] = approval.ID
	delete(h.responderGrants, approval.ID)
	snapshot := cloneApproval(approval)
	effects.waiter, effects.waiterSnap = waiter, &snapshot
	effects.broadcasts = append(effects.broadcasts, &snapshot)
	effects.records = append(effects.records, &snapshot)
	h.mu.Unlock()
	runApprovalResponseEffects(h, effects)
	return approvalResponseOutcome{kind: approvalResponseAccepted, approval: &snapshot}
}

func runApprovalResponseEffects(h *approvalsHub, effects approvalResponseEffects) {
	if effects.waiter != nil && effects.waiterSnap != nil {
		effects.waiter <- effects.waiterSnap
	}
	for _, approval := range effects.broadcasts {
		h.broadcast("decided", approval)
	}
	for _, approval := range effects.records {
		appendApprovalRecord(approval)
	}
}
