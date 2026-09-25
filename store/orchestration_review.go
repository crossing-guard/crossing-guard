package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const ReviewBindingID = "pretool-report-default"

var (
	ErrReviewBindingConflict = errors.New("review binding changed")
	ErrReviewActionConflict  = errors.New("review action identity reused with different evidence")
)

type ReviewBinding struct {
	BindingID             string `json:"binding_id"`
	State                 string `json:"state"`
	Effect                string `json:"effect"`
	RuntimeFilter         string `json:"runtime_filter,omitempty"`
	ProfileID             string `json:"profile_id"`
	ProfileSourceDigest   string `json:"profile_source_digest"`
	ProfileBundleDigest   string `json:"profile_bundle_digest"`
	InstructionDigest     string `json:"instruction_digest"`
	RequestPathKind       string `json:"request_path_kind"`
	RequestPathDigest     string `json:"request_path_digest"`
	Endpoint              string `json:"endpoint"`
	Model                 string `json:"model"`
	TimeoutMS             int    `json:"timeout_ms"`
	ApprovalSubdeadlineMS int    `json:"approval_subdeadline_ms,omitempty"`
	// AnswerChoicePrompts is whether this reviewer may answer a held call's
	// questions. Off means prompt-bearing approvals are never offered to it.
	AnswerChoicePrompts bool   `json:"answer_choice_prompts"`
	MaxInputBytes       int    `json:"max_input_bytes"`
	MaxOutputBytes      int    `json:"max_output_bytes"`
	MaxTokens           int    `json:"max_tokens"`
	MaxConcurrency      int    `json:"max_concurrency"`
	StateToken          string `json:"state_token"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
}

type ReviewInvocation struct {
	InvocationID        string `json:"invocation_id"`
	ActionID            string `json:"action_id"`
	ActionDigest        string `json:"action_digest"`
	ObservationID       string `json:"observation_id"`
	EventID             int64  `json:"event_id"`
	Runtime             string `json:"runtime,omitempty"`
	SessionID           string `json:"session_id"`
	NativeCallID        string `json:"native_call_id,omitempty"`
	NativeCallKind      string `json:"native_call_kind,omitempty"`
	Tool                string `json:"tool"`
	BindingID           string `json:"binding_id"`
	BindingStateToken   string `json:"binding_state_token"`
	ProfileID           string `json:"profile_id"`
	ProfileSourceDigest string `json:"profile_source_digest"`
	ProfileBundleDigest string `json:"profile_bundle_digest"`
	InstructionDigest   string `json:"instruction_digest"`
	RequestPathKind     string `json:"request_path_kind"`
	RequestPathDigest   string `json:"request_path_digest"`
	Endpoint            string `json:"endpoint"`
	Model               string `json:"model"`
	TimeoutMS           int    `json:"timeout_ms"`
	MaxInputBytes       int    `json:"max_input_bytes"`
	MaxOutputBytes      int    `json:"max_output_bytes"`
	MaxTokens           int    `json:"max_tokens"`
	MaxConcurrency      int    `json:"max_concurrency"`
	State               string `json:"state"`
	AdmittedAt          int64  `json:"admitted_at"`
	StartedAt           int64  `json:"started_at,omitempty"`
	CompletedAt         int64  `json:"completed_at,omitempty"`
	DurationMS          int64  `json:"duration_ms,omitempty"`
	// Action is the reviewer's recommendation on the unified agent-claim wire
	// (allow/deny/abstain). It is stored in the historical `decision` column.
	Action             string   `json:"action,omitempty"`
	Message            string   `json:"message,omitempty"`
	Citations          []string `json:"citations"`
	RequestBytes       int      `json:"request_bytes,omitempty"`
	ResponseBytes      int      `json:"response_bytes,omitempty"`
	PromptTokens       int      `json:"prompt_tokens,omitempty"`
	CompletionTokens   int      `json:"completion_tokens,omitempty"`
	ErrorClass         string   `json:"error_class,omitempty"`
	Recovery           string   `json:"recovery,omitempty"`
	TimingClass        string   `json:"timing_class"`
	ApprovalID         string   `json:"approval_id,omitempty"`
	ApprovalResponseID string   `json:"approval_response_id,omitempty"`
	ApprovalOutcome    string   `json:"approval_outcome,omitempty"`
	// SelectionsJSON is what the reviewer CLAIMED to pick, verbatim. What became
	// operative is the approval owner's response, joined by ApprovalResponseID.
	SelectionsJSON string `json:"selections_json,omitempty"`
}

type ReviewCompletion struct {
	State, Action, Message, ErrorClass, Recovery string
	// SelectionsJSON is the reviewer's claimed answer to a held question, if any.
	SelectionsJSON                 string
	Citations                      []string
	CompletedAt, DurationMS        int64
	RequestBytes, ResponseBytes    int
	PromptTokens, CompletionTokens int
}

type ReviewActualOutcome struct {
	Observed   bool   `json:"observed"`
	State      string `json:"state,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	ObservedAt int64  `json:"observed_at,omitempty"`
}

const reviewBindingColumns = `binding_id,state,effect,runtime_filter,profile_id,profile_source_digest,
	profile_bundle_digest,instruction_digest,request_path_kind,request_path_digest,endpoint,model,
	timeout_ms,approval_subdeadline_ms,answer_choice_prompts,max_input_bytes,max_output_bytes,max_tokens,max_concurrency,state_token,created_at,updated_at`

func scanReviewBinding(row interface{ Scan(...any) error }) (ReviewBinding, error) {
	var binding ReviewBinding
	err := row.Scan(&binding.BindingID, &binding.State, &binding.Effect, &binding.RuntimeFilter, &binding.ProfileID,
		&binding.ProfileSourceDigest, &binding.ProfileBundleDigest, &binding.InstructionDigest,
		&binding.RequestPathKind, &binding.RequestPathDigest, &binding.Endpoint, &binding.Model,
		&binding.TimeoutMS, &binding.ApprovalSubdeadlineMS, &binding.AnswerChoicePrompts,
		&binding.MaxInputBytes, &binding.MaxOutputBytes, &binding.MaxTokens,
		&binding.MaxConcurrency, &binding.StateToken, &binding.CreatedAt, &binding.UpdatedAt)
	return binding, err
}

func (ix *Index) ReviewBinding() (ReviewBinding, bool, error) {
	binding, err := scanReviewBinding(ix.db.QueryRow(`SELECT `+reviewBindingColumns+
		` FROM orchestration_review_binding WHERE binding_id=?`, ReviewBindingID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewBinding{}, false, nil
	}
	return binding, err == nil, err
}

func ReviewBindingAbsentToken() string {
	return reviewDigest("crossing-guard-report-review-binding-absent-v1\x00", []byte(ReviewBindingID))
}

func ReviewBindingStateToken(binding ReviewBinding) string {
	copyBinding := binding
	copyBinding.StateToken, copyBinding.UpdatedAt = "", 0
	body, _ := json.Marshal(copyBinding)
	return reviewDigest("crossing-guard-report-review-binding-state-v1\x00", body)
}

func (ix *Index) PutReviewBinding(binding ReviewBinding, expectedToken string, now int64) (ReviewBinding, error) {
	if binding.BindingID == "" {
		binding.BindingID = ReviewBindingID
	}
	if binding.Effect == "" {
		binding.Effect = "report-only"
	}
	if binding.BindingID != ReviewBindingID || binding.State != "enabled" || expectedToken == "" ||
		(binding.Effect != "report-only" && binding.Effect != "delegated-first") ||
		(binding.Effect == "report-only" && binding.ApprovalSubdeadlineMS != 0) ||
		(binding.Effect == "delegated-first" && (binding.ApprovalSubdeadlineMS < 250 || binding.ApprovalSubdeadlineMS > binding.TimeoutMS)) {
		return ReviewBinding{}, ErrReviewBindingConflict
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return ReviewBinding{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, readErr := scanReviewBinding(tx.QueryRow(`SELECT `+reviewBindingColumns+
		` FROM orchestration_review_binding WHERE binding_id=?`, ReviewBindingID))
	if errors.Is(readErr, sql.ErrNoRows) {
		if expectedToken != ReviewBindingAbsentToken() {
			return ReviewBinding{}, ErrReviewBindingConflict
		}
		binding.CreatedAt = now
	} else if readErr != nil {
		return ReviewBinding{}, readErr
	} else {
		if expectedToken != current.StateToken {
			return ReviewBinding{}, ErrReviewBindingConflict
		}
		binding.CreatedAt = current.CreatedAt
	}
	binding.UpdatedAt = now
	binding.StateToken = ReviewBindingStateToken(binding)
	_, err = tx.Exec(`INSERT INTO orchestration_review_binding(`+reviewBindingColumns+`) VALUES(`+
		`?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(binding_id) DO UPDATE SET
		state=excluded.state,effect=excluded.effect,runtime_filter=excluded.runtime_filter,profile_id=excluded.profile_id,
		profile_source_digest=excluded.profile_source_digest,profile_bundle_digest=excluded.profile_bundle_digest,
		instruction_digest=excluded.instruction_digest,request_path_kind=excluded.request_path_kind,
		request_path_digest=excluded.request_path_digest,endpoint=excluded.endpoint,model=excluded.model,
		timeout_ms=excluded.timeout_ms,approval_subdeadline_ms=excluded.approval_subdeadline_ms,
		answer_choice_prompts=excluded.answer_choice_prompts,max_input_bytes=excluded.max_input_bytes,
		max_output_bytes=excluded.max_output_bytes,max_tokens=excluded.max_tokens,
		max_concurrency=excluded.max_concurrency,state_token=excluded.state_token,updated_at=excluded.updated_at`,
		binding.BindingID, binding.State, binding.Effect, binding.RuntimeFilter, binding.ProfileID,
		binding.ProfileSourceDigest, binding.ProfileBundleDigest, binding.InstructionDigest,
		binding.RequestPathKind, binding.RequestPathDigest, binding.Endpoint, binding.Model,
		binding.TimeoutMS, binding.ApprovalSubdeadlineMS, binding.AnswerChoicePrompts,
		binding.MaxInputBytes, binding.MaxOutputBytes, binding.MaxTokens,
		binding.MaxConcurrency, binding.StateToken, binding.CreatedAt, binding.UpdatedAt)
	if err != nil {
		return ReviewBinding{}, fmt.Errorf("put review binding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ReviewBinding{}, err
	}
	return binding, nil
}

func (ix *Index) DisableReviewBinding(expectedToken string, now int64) (ReviewBinding, error) {
	current, found, err := ix.ReviewBinding()
	if err != nil || !found {
		if err == nil {
			err = ErrReviewBindingConflict
		}
		return ReviewBinding{}, err
	}
	current.State = "disabled"
	return ix.putReviewBindingState(current, expectedToken, now)
}

func (ix *Index) putReviewBindingState(binding ReviewBinding, expectedToken string, now int64) (ReviewBinding, error) {
	if expectedToken != binding.StateToken {
		return ReviewBinding{}, ErrReviewBindingConflict
	}
	previous := binding.StateToken
	binding.UpdatedAt = now
	binding.StateToken = ReviewBindingStateToken(binding)
	result, err := ix.db.Exec(`UPDATE orchestration_review_binding SET state=?,state_token=?,updated_at=?
		WHERE binding_id=? AND state_token=?`, binding.State, binding.StateToken, now, binding.BindingID, previous)
	if err != nil {
		return ReviewBinding{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ReviewBinding{}, ErrReviewBindingConflict
	}
	return binding, nil
}

const reviewInvocationColumns = `invocation_id,action_id,action_digest,observation_id,event_id,runtime,
	session_id,native_call_id,native_call_kind,tool,binding_id,binding_state_token,profile_id,
	profile_source_digest,profile_bundle_digest,instruction_digest,request_path_kind,request_path_digest,
	endpoint,model,timeout_ms,max_input_bytes,max_output_bytes,max_tokens,max_concurrency,state,admitted_at,
	started_at,completed_at,duration_ms,decision,message,citations_json,request_bytes,response_bytes,
	prompt_tokens,completion_tokens,error_class,recovery,timing_class,approval_id,approval_response_id,approval_outcome,selections_json`

func scanReviewInvocation(row interface{ Scan(...any) error }) (ReviewInvocation, error) {
	var invocation ReviewInvocation
	var citations string
	err := row.Scan(&invocation.InvocationID, &invocation.ActionID, &invocation.ActionDigest,
		&invocation.ObservationID, &invocation.EventID, &invocation.Runtime, &invocation.SessionID,
		&invocation.NativeCallID, &invocation.NativeCallKind, &invocation.Tool, &invocation.BindingID,
		&invocation.BindingStateToken, &invocation.ProfileID, &invocation.ProfileSourceDigest,
		&invocation.ProfileBundleDigest, &invocation.InstructionDigest, &invocation.RequestPathKind,
		&invocation.RequestPathDigest, &invocation.Endpoint, &invocation.Model, &invocation.TimeoutMS,
		&invocation.MaxInputBytes, &invocation.MaxOutputBytes, &invocation.MaxTokens,
		&invocation.MaxConcurrency, &invocation.State, &invocation.AdmittedAt, &invocation.StartedAt,
		&invocation.CompletedAt, &invocation.DurationMS, &invocation.Action, &invocation.Message,
		&citations, &invocation.RequestBytes, &invocation.ResponseBytes, &invocation.PromptTokens,
		&invocation.CompletionTokens, &invocation.ErrorClass, &invocation.Recovery, &invocation.TimingClass,
		&invocation.ApprovalID, &invocation.ApprovalResponseID, &invocation.ApprovalOutcome,
		&invocation.SelectionsJSON)
	if err == nil {
		err = json.Unmarshal([]byte(citations), &invocation.Citations)
	}
	if invocation.Citations == nil {
		invocation.Citations = []string{}
	}
	return invocation, err
}

func (ix *Index) AdmitReview(invocation ReviewInvocation) (ReviewInvocation, bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return ReviewInvocation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, readErr := scanReviewInvocation(tx.QueryRow(`SELECT `+reviewInvocationColumns+
		` FROM orchestration_review_invocation WHERE action_id=?`, invocation.ActionID))
	if readErr == nil {
		if existing.ActionDigest != invocation.ActionDigest {
			return ReviewInvocation{}, false, ErrReviewActionConflict
		}
		return existing, false, nil
	}
	if !errors.Is(readErr, sql.ErrNoRows) {
		return ReviewInvocation{}, false, readErr
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM orchestration_review_invocation
		WHERE binding_id=? AND profile_id=? AND profile_bundle_digest=? AND state IN ('admitted','running')`,
		invocation.BindingID, invocation.ProfileID, invocation.ProfileBundleDigest).Scan(&active); err != nil {
		return ReviewInvocation{}, false, err
	}
	if active >= invocation.MaxConcurrency {
		invocation.State = "suppressed"
		invocation.ErrorClass = "concurrency_limit"
		invocation.Recovery = "Wait for an active report review to finish; this action will not be replayed."
		invocation.CompletedAt = invocation.AdmittedAt
		invocation.TimingClass = "result_not_observed_at_completion"
	} else {
		invocation.State = "admitted"
	}
	if invocation.Citations == nil {
		invocation.Citations = []string{}
	}
	citations, err := json.Marshal(invocation.Citations)
	if err != nil {
		return ReviewInvocation{}, false, err
	}
	_, err = tx.Exec(`INSERT INTO orchestration_review_invocation(`+reviewInvocationColumns+`) VALUES(`+
		`?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		invocation.InvocationID, invocation.ActionID, invocation.ActionDigest, invocation.ObservationID,
		invocation.EventID, invocation.Runtime, invocation.SessionID, invocation.NativeCallID,
		invocation.NativeCallKind, invocation.Tool, invocation.BindingID, invocation.BindingStateToken,
		invocation.ProfileID, invocation.ProfileSourceDigest, invocation.ProfileBundleDigest,
		invocation.InstructionDigest, invocation.RequestPathKind, invocation.RequestPathDigest,
		invocation.Endpoint, invocation.Model, invocation.TimeoutMS, invocation.MaxInputBytes,
		invocation.MaxOutputBytes, invocation.MaxTokens, invocation.MaxConcurrency, invocation.State,
		invocation.AdmittedAt, invocation.StartedAt, invocation.CompletedAt, invocation.DurationMS,
		invocation.Action, invocation.Message, string(citations), invocation.RequestBytes,
		invocation.ResponseBytes, invocation.PromptTokens, invocation.CompletionTokens,
		invocation.ErrorClass, invocation.Recovery, invocation.TimingClass, invocation.ApprovalID,
		invocation.ApprovalResponseID, invocation.ApprovalOutcome, invocation.SelectionsJSON)
	if err != nil {
		return ReviewInvocation{}, false, fmt.Errorf("admit review: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ReviewInvocation{}, false, err
	}
	return invocation, true, nil
}

func (ix *Index) MarkReviewRunning(invocationID string, startedAt int64) error {
	result, err := ix.db.Exec(`UPDATE orchestration_review_invocation SET state='running',started_at=?
		WHERE invocation_id=? AND state='admitted'`, startedAt, invocationID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("review invocation is not admitted")
	}
	return nil
}

func (ix *Index) CompleteReview(invocationID string, completion ReviewCompletion) error {
	citations, err := json.Marshal(completion.Citations)
	if err != nil {
		return err
	}
	timing := "result_not_observed_at_completion"
	var eventID int64
	if err := ix.db.QueryRow(`SELECT event_id FROM orchestration_review_invocation WHERE invocation_id=?`, invocationID).Scan(&eventID); err != nil {
		return err
	}
	var observedAt int64
	err = ix.db.QueryRow(`SELECT ro.received_at FROM result_observation ro
		JOIN result_reconciliation rr ON rr.result_id=ro.id
		JOIN result_reconciliation_candidate candidate ON candidate.reconciliation_id=rr.id
		WHERE candidate.event_id=? AND candidate.selected=1 AND rr.join_class='exact'
		AND NOT EXISTS(SELECT 1 FROM result_reconciliation newer WHERE newer.result_id=rr.result_id
			AND (newer.reconciled_at>rr.reconciled_at OR (newer.reconciled_at=rr.reconciled_at AND newer.id>rr.id)))
		ORDER BY ro.received_at,ro.id LIMIT 1`, eventID).Scan(&observedAt)
	if err == nil && observedAt > 0 {
		switch {
		case completion.CompletedAt < observedAt:
			timing = "completed_before_exact_result_observation"
		case completion.CompletedAt > observedAt:
			timing = "completed_after_exact_result_observation"
		default:
			timing = "exact_result_timing_unknown"
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	result, err := ix.db.Exec(`UPDATE orchestration_review_invocation SET state=?,completed_at=?,duration_ms=?,
		decision=?,message=?,citations_json=?,selections_json=?,request_bytes=?,response_bytes=?,prompt_tokens=?,
		completion_tokens=?,error_class=?,recovery=?,timing_class=? WHERE invocation_id=? AND state='running'`,
		completion.State, completion.CompletedAt, completion.DurationMS, completion.Action,
		completion.Message, string(citations), completion.SelectionsJSON, completion.RequestBytes,
		completion.ResponseBytes, completion.PromptTokens, completion.CompletionTokens,
		completion.ErrorClass, completion.Recovery, timing, invocationID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("review invocation is not running")
	}
	return nil
}

// LinkReviewApprovalOutcome records the generic approval owner's answer to a delegated
// recommendation. The model claim is already complete; this linkage never changes it
// into governance truth.
func (ix *Index) LinkReviewApprovalOutcome(invocationID, approvalID, responseID, outcome string) error {
	if invocationID == "" || approvalID == "" || len(responseID) > 128 || len(outcome) > 64 {
		return errors.New("invalid delegated approval linkage")
	}
	result, err := ix.db.Exec(`UPDATE orchestration_review_invocation SET approval_id=?,
		approval_response_id=?,approval_outcome=? WHERE invocation_id=? AND state NOT IN ('admitted','running')`,
		approvalID, responseID, outcome, invocationID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("review invocation is not complete")
	}
	return nil
}

func (ix *Index) RecoverReviewInvocationsUnknown(now int64) (int64, error) {
	result, err := ix.db.Exec(`UPDATE orchestration_review_invocation SET state='unknown',completed_at=?,
		error_class='restart_unknown',recovery='The daemon restarted during review; this request was not replayed.',
		timing_class='result_not_observed_at_completion' WHERE state IN ('admitted','running')`, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (ix *Index) ReviewInvocations(limit int, before string) ([]ReviewInvocation, error) {
	return ix.reviewInvocations("", "", limit, before)
}

func (ix *Index) ReviewInvocationsForSession(runtime, sessionID string, limit int) ([]ReviewInvocation, error) {
	return ix.reviewInvocations(runtime, sessionID, limit, "")
}

func (ix *Index) ReviewSourceEvidenceRetained(eventID int64) (bool, error) {
	var found int
	err := ix.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM event_delivery WHERE event_id=?)`, eventID).Scan(&found)
	return found == 1, err
}

func (ix *Index) ReviewActualOutcome(eventID int64) (ReviewActualOutcome, error) {
	var outcome ReviewActualOutcome
	err := ix.db.QueryRow(`SELECT ro.state,ro.error_class,ro.received_at FROM result_observation ro
		JOIN result_reconciliation rr ON rr.result_id=ro.id
		JOIN result_reconciliation_candidate candidate ON candidate.reconciliation_id=rr.id
		WHERE candidate.event_id=? AND candidate.selected=1 AND rr.join_class='exact'
		AND NOT EXISTS(SELECT 1 FROM result_reconciliation newer WHERE newer.result_id=rr.result_id
			AND (newer.reconciled_at>rr.reconciled_at OR (newer.reconciled_at=rr.reconciled_at AND newer.id>rr.id)))
		ORDER BY ro.received_at DESC,ro.id DESC LIMIT 1`, eventID).Scan(&outcome.State, &outcome.ErrorClass, &outcome.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewActualOutcome{}, nil
	}
	if err != nil {
		return ReviewActualOutcome{}, err
	}
	outcome.Observed = true
	return outcome, nil
}

func (ix *Index) reviewInvocations(runtime, sessionID string, limit int, before string) ([]ReviewInvocation, error) {
	if limit < 1 || limit > 100 {
		limit = 25
	}
	query := `SELECT ` + reviewInvocationColumns + ` FROM orchestration_review_invocation`
	args := []any{}
	conditions := []string{}
	if runtime != "" || sessionID != "" {
		conditions = append(conditions, "runtime=?", "session_id=?")
		args = append(args, runtime, sessionID)
	}
	if before != "" {
		conditions = append(conditions, `(admitted_at,invocation_id)<(SELECT admitted_at,invocation_id
			FROM orchestration_review_invocation WHERE invocation_id=?)`)
		args = append(args, before)
	}
	if len(conditions) > 0 {
		query += " WHERE "
		for index, condition := range conditions {
			if index > 0 {
				query += " AND "
			}
			query += condition
		}
	}
	query += ` ORDER BY admitted_at DESC,invocation_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewInvocation{}
	for rows.Next() {
		invocation, err := scanReviewInvocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, invocation)
	}
	return out, rows.Err()
}

func reviewDigest(frame string, body []byte) string {
	sum := sha256.Sum256(append([]byte(frame), body...))
	return "sha256-v1:" + hex.EncodeToString(sum[:])
}
