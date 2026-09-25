package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

const orchestrationReviewRequestLimit = 32 * 1024

type reviewProfileOption struct {
	ProfileID        string   `json:"profile_id"`
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	SourceDigest     string   `json:"source_digest"`
	BundleDigest     string   `json:"bundle_digest"`
	Compatible       bool     `json:"compatible"`
	Reason           string   `json:"reason,omitempty"`
	TimeoutCeilingMS int      `json:"timeout_ceiling_ms,omitempty"`
	MaxInputBytes    int      `json:"max_input_bytes,omitempty"`
	MaxOutputBytes   int      `json:"max_output_bytes,omitempty"`
	MaxTokens        int      `json:"max_tokens,omitempty"`
	MaxConcurrency   int      `json:"max_concurrency,omitempty"`
	Effects          []string `json:"effects"`
}

type reviewHistoryItem struct {
	Invocation                 store.ReviewInvocation    `json:"invocation"`
	SourceEvidenceRetained     bool                      `json:"source_evidence_retained"`
	ObservedGovernanceDecision string                    `json:"observed_governance_decision,omitempty"`
	ActualOutcome              store.ReviewActualOutcome `json:"actual_outcome"`
}

func registerOrchestrationReviewRoutes(mux *http.ServeMux, host *orchestrationReviewHost, profiles *profilefs.Owner) {
	mux.HandleFunc("GET /api/orchestration/reviews/settings", func(w http.ResponseWriter, _ *http.Request) {
		if host == nil {
			writeReviewUnavailable(w)
			return
		}
		binding, found, err := host.ix.ReviewBinding()
		if err != nil {
			writeReviewError(w, err)
			return
		}
		options, err := reviewProfileOptions(profiles)
		if err != nil {
			writeReviewError(w, err)
			return
		}
		token := store.ReviewBindingAbsentToken()
		var current any
		updateAvailable := false
		if found {
			token, current = binding.StateToken, binding
			if detail, detailErr := profiles.Get(binding.ProfileID); detailErr == nil {
				updateAvailable = detail.Current.SourceDigest != binding.ProfileSourceDigest ||
					detail.Current.BundleDigest != binding.ProfileBundleDigest
			}
		}
		host.mu.RLock()
		startupProblem := host.startupProblem
		runtimeReady := host.runtimeBinding != nil
		host.mu.RUnlock()
		recent, err := reviewHistory(host.ix, 10, "", "")
		if err != nil {
			writeReviewError(w, err)
			return
		}
		writeJSON(w, map[string]any{"binding": current, "state_token": token,
			"profiles": options, "update_available": updateAvailable,
			"runtime_ready": runtimeReady, "problem": startupProblem, "recent": recent,
			"effect":       reviewEffectDescription(binding, found),
			"availability": "Saving verifies configuration only; model availability is learned from invocation history."})
	})

	mux.HandleFunc("PUT /api/orchestration/reviews/binding", func(w http.ResponseWriter, r *http.Request) {
		if host == nil {
			writeReviewUnavailable(w)
			return
		}
		var request struct {
			ProfileID             string `json:"profile_id"`
			ProfileSourceDigest   string `json:"profile_source_digest"`
			ProfileBundleDigest   string `json:"profile_bundle_digest"`
			Endpoint              string `json:"endpoint"`
			Model                 string `json:"model"`
			TimeoutMS             int    `json:"timeout_ms"`
			Effect                string `json:"effect"`
			ApprovalSubdeadlineMS int    `json:"approval_subdeadline_ms"`
			AnswerChoicePrompts   *bool  `json:"answer_choice_prompts"`
			RuntimeFilter         string `json:"runtime_filter"`
			ExpectedStateToken    string `json:"expected_state_token"`
			Confirmed             bool   `json:"confirmed"`
		}
		if err := decodeReviewJSON(w, r, &request); err != nil {
			writeReviewError(w, err)
			return
		}
		if !request.Confirmed {
			writeReviewError(w, &reviewHTTPProblem{code: "confirmation_required", message: "Explicit binding confirmation is required."})
			return
		}
		binding, err := host.putBinding(reviewBindingCommand{ProfileID: request.ProfileID,
			ProfileSourceDigest: request.ProfileSourceDigest, ProfileBundleDigest: request.ProfileBundleDigest,
			Endpoint: request.Endpoint, Model: request.Model, TimeoutMS: request.TimeoutMS,
			Effect: request.Effect, ApprovalSubdeadlineMS: request.ApprovalSubdeadlineMS,
			// Absent means yes: a delegated reviewer the operator has not spoken
			// about may answer questions, which is what the grant is for.
			AnswerChoicePrompts: request.AnswerChoicePrompts == nil || *request.AnswerChoicePrompts,
			RuntimeFilter:       request.RuntimeFilter, ExpectedStateToken: request.ExpectedStateToken})
		if err != nil {
			writeReviewError(w, err)
			return
		}
		writeJSON(w, map[string]any{"binding": binding,
			"note": "Enabled for new matching actions. Availability remains unverified until a real invocation succeeds."})
	})

	mux.HandleFunc("POST /api/orchestration/reviews/binding/disable", func(w http.ResponseWriter, r *http.Request) {
		if host == nil {
			writeReviewUnavailable(w)
			return
		}
		var request struct {
			ExpectedStateToken string `json:"expected_state_token"`
			Confirmed          bool   `json:"confirmed"`
		}
		if err := decodeReviewJSON(w, r, &request); err != nil {
			writeReviewError(w, err)
			return
		}
		if !request.Confirmed {
			writeReviewError(w, &reviewHTTPProblem{code: "confirmation_required", message: "Explicit disable confirmation is required."})
			return
		}
		binding, err := host.disableBinding(request.ExpectedStateToken)
		if err != nil {
			writeReviewError(w, err)
			return
		}
		writeJSON(w, map[string]any{"binding": binding,
			"note": "Disabled for new actions. Existing report-only history is retained."})
	})

	mux.HandleFunc("GET /api/orchestration/reviews/session", func(w http.ResponseWriter, r *http.Request) {
		if host == nil {
			writeReviewUnavailable(w)
			return
		}
		runtime, sessionID := r.URL.Query().Get("runtime"), r.URL.Query().Get("id")
		limit, err := reviewLimit(r.URL.Query().Get("limit"))
		if err != nil || runtime == "" || sessionID == "" || len(runtime) > 64 || len(sessionID) > 512 || !validRuntimeName(runtime) {
			writeReviewError(w, &reviewHTTPProblem{code: "invalid_request", message: "An exact bounded runtime and session ID are required."})
			return
		}
		items, err := reviewHistory(host.ix, limit, runtime, sessionID)
		if err != nil {
			writeReviewError(w, err)
			return
		}
		writeJSON(w, map[string]any{"runtime": runtime, "session_id": sessionID, "reviews": items,
			"label":               "Independent review recommendation · report only",
			"actual_outcome_note": "Not observed means Crossing Guard has no exact result observation; it does not mean the tool did not run."})
	})
}

func reviewEffectDescription(binding store.ReviewBinding, found bool) string {
	if found && binding.Effect == "delegated-first" {
		return "Delegated first for an existing ask. The approval owner still enforces the exact deadline and a human answer takes over."
	}
	return "Independent review recommendation · report only. It cannot allow, deny, hold, or interrupt an action."
}

func reviewProfileOptions(owner *profilefs.Owner) ([]reviewProfileOption, error) {
	listed, err := owner.List()
	if err != nil {
		return nil, err
	}
	options := make([]reviewProfileOption, 0, len(listed.Profiles))
	for _, summary := range listed.Profiles {
		option := reviewProfileOption{ProfileID: summary.ProfileID, Name: summary.Name, Version: summary.Version,
			SourceDigest: summary.SourceDigest, BundleDigest: summary.BundleDigest, Effects: []string{}}
		detail, detailErr := owner.GetRevision(summary.ProfileID, summary.SourceDigest, summary.BundleDigest)
		if detailErr == nil && detail.Normalized != nil {
			var profile orchestration.Profile
			profile, detailErr = sliceCProfile(*detail.Normalized, summary.SourceDigest, summary.BundleDigest)
			if detailErr == nil {
				if detail.Normalized.Output.Kind == "approval-response" {
					option.Effects = []string{"delegated-first"}
				} else {
					option.Effects = []string{"report-only"}
				}
				option.TimeoutCeilingMS = int(profile.Timeout.Milliseconds())
				option.MaxInputBytes, option.MaxOutputBytes = profile.MaxInputBytes, profile.MaxOutputBytes
				option.MaxTokens, option.MaxConcurrency = profile.MaxTokens, profile.MaxConcurrency
			}
		}
		option.Compatible = detailErr == nil
		if !option.Compatible {
			option.Reason = "This selected profile requests context, authority, output, or limits outside report-only one-shot review."
		}
		options = append(options, option)
	}
	return options, nil
}

func reviewHistory(ix *store.Index, limit int, runtime, sessionID string) ([]reviewHistoryItem, error) {
	var records []store.ReviewInvocation
	var err error
	if runtime != "" || sessionID != "" {
		records, err = ix.ReviewInvocationsForSession(runtime, sessionID, limit)
	} else {
		records, err = ix.ReviewInvocations(limit, "")
	}
	if err != nil {
		return nil, err
	}
	items := make([]reviewHistoryItem, 0, len(records))
	for _, record := range records {
		retained, readErr := ix.ReviewSourceEvidenceRetained(record.EventID)
		if readErr != nil {
			return nil, readErr
		}
		governanceDecision := ""
		if retained {
			event, eventErr := ix.EventForSession(record.SessionID, record.EventID)
			if eventErr == nil {
				governanceDecision = event.Decision
			} else if !errors.Is(eventErr, sql.ErrNoRows) {
				return nil, eventErr
			}
		}
		outcome, readErr := ix.ReviewActualOutcome(record.EventID)
		if readErr != nil {
			return nil, readErr
		}
		items = append(items, reviewHistoryItem{Invocation: record,
			SourceEvidenceRetained: retained, ObservedGovernanceDecision: governanceDecision,
			ActualOutcome: outcome})
	}
	return items, nil
}

func reviewLimit(raw string) (int, error) {
	if raw == "" {
		return 25, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 100 {
		return 0, errors.New("invalid limit")
	}
	return limit, nil
}

type reviewHTTPProblem struct {
	code    string
	message string
}

func (problem *reviewHTTPProblem) Error() string { return problem.message }

func decodeReviewJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, orchestrationReviewRequestLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return &reviewHTTPProblem{code: "invalid_request", message: "The review settings request is invalid."}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return &reviewHTTPProblem{code: "invalid_request", message: "The review settings request must contain one JSON value."}
	}
	return nil
}

func writeReviewUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	writeJSON(w, map[string]any{"error": map[string]string{"code": "host_unavailable",
		"message": "Report-only review configuration is unavailable. Monitoring and governance are unaffected."}})
}

func writeReviewError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusUnprocessableEntity, "invalid_binding", "The report-only review binding is incompatible."
	if errors.Is(err, store.ErrReviewBindingConflict) {
		status, code, message = http.StatusConflict, "state_conflict", "The review binding changed; reload Settings before retrying."
	} else if errors.Is(err, store.ErrReviewActionConflict) {
		status, code, message = http.StatusConflict, "action_conflict", "The action identity conflicts with retained review evidence."
	} else {
		var problem *reviewHTTPProblem
		if errors.As(err, &problem) {
			status, code, message = http.StatusBadRequest, problem.code, problem.message
		} else if strings.Contains(err.Error(), "store") || strings.Contains(err.Error(), "database") {
			status, code, message = http.StatusInternalServerError, "storage_error", "Report-only review storage is unavailable."
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
