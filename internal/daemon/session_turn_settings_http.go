package daemon

import (
	"errors"
	"net/http"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

type sessionEffortRequest struct {
	Runtime   string               `json:"runtime"`
	SessionID string               `json:"session_id"`
	Model     string               `json:"model"`
	Token     string               `json:"token"`
	Effort    store.ThinkingEffort `json:"thinking_effort"`
}

func effortSessionID(runtime, id string) (string, error) {
	if runtime == "" || id == "" || len(runtime) > 100 || len(id) > 1000 {
		return "", effortError("identity_pending", "A known session is required before saving effort")
	}
	row, ok := findSession(ScanSessions(), sessionRef{Runtime: runtime, ID: id})
	if !ok {
		return "", effortError("identity_pending", "Session identity is not available yet")
	}
	return harvest.CanonicalID(row), nil
}
func writeEffortError(w http.ResponseWriter, err error) bool {
	var failure *EffortError
	if errors.Is(err, store.ErrSessionEffortConflict) {
		failure = &EffortError{Code: "stale_session_default", Field: "thinking_effort", Message: err.Error()}
	}
	if failure == nil && !errors.As(err, &failure) {
		return false
	}
	status := http.StatusBadRequest
	if failure.Code == "stale_session_default" || failure.Code == "identity_pending" {
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, failure)
	return true
}
func handleSessionEffortGet(w http.ResponseWriter, r *http.Request) {
	if runtimeTaskIndex == nil {
		http.Error(w, runtimeTasksUnavailable("task settings unavailable"), http.StatusServiceUnavailable)
		return
	}
	runtime, id, model := r.URL.Query().Get("runtime"), r.URL.Query().Get("session_id"), r.URL.Query().Get("model")
	canonical, err := effortSessionID(runtime, id)
	if err != nil {
		writeEffortError(w, err)
		return
	}
	if model == "" || len(model) > 256 {
		writeEffortError(w, effortError("invalid_choice", "Concrete model required"))
		return
	}
	result, err := runtimeTaskIndex.SessionTurnSettings(runtime, canonical, model)
	if err != nil {
		http.Error(w, "settings read failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}
func handleSessionEffortPut(w http.ResponseWriter, r *http.Request) {
	if runtimeTaskIndex == nil {
		http.Error(w, runtimeTasksUnavailable("task settings unavailable"), http.StatusServiceUnavailable)
		return
	}
	var request sessionEffortRequest
	if err := decodeManagedJSON(w, r, &request); err != nil {
		http.Error(w, "invalid settings request", http.StatusBadRequest)
		return
	}
	canonical, err := effortSessionID(request.Runtime, request.SessionID)
	if err != nil {
		writeEffortError(w, err)
		return
	}
	req, err := normalizeEffort(ChatRequest{Runtime: request.Runtime, Model: request.Model, SessionID: request.SessionID, ThinkingEffort: &request.Effort})
	if err == nil {
		_, err = resolveRequestEffort(req)
	}
	if err != nil {
		writeEffortError(w, err)
		return
	}
	result, err := runtimeTaskIndex.SaveSessionTurnSettings(request.Runtime, canonical, request.Model, request.Token, request.Effort)
	if err != nil {
		if !writeEffortError(w, err) {
			http.Error(w, "settings write failed", http.StatusBadRequest)
		}
		return
	}
	writeJSON(w, result)
}

// Preview is read-only and does not call a canonicalizer that may spawn a
// process. Runtime-specific parsing remains on each adapter's pure effort seam.
type effortSyntaxParser interface {
	ParseEffort(ChatRequest) (ChatRequest, error)
}
type effortPreviewRequest struct {
	Runtime   string                `json:"runtime"`
	Model     string                `json:"model"`
	ExtraArgs string                `json:"extra_args"`
	Effort    *store.ThinkingEffort `json:"thinking_effort,omitempty"`
}

func handleEffortPreview(w http.ResponseWriter, r *http.Request) {
	var request effortPreviewRequest
	if err := decodeManagedJSON(w, r, &request); err != nil {
		http.Error(w, "invalid effort preview", http.StatusBadRequest)
		return
	}
	req, err := normalizeEffort(ChatRequest{Runtime: request.Runtime, Model: request.Model, ExtraArgs: request.ExtraArgs, ThinkingEffort: request.Effort})
	driver, ok := chatDrivers[request.Runtime]
	if !ok {
		http.Error(w, "unknown runtime", http.StatusBadRequest)
		return
	}
	if err == nil {
		if parser, ok := driver.(effortSyntaxParser); ok {
			req, err = parser.ParseEffort(req)
		}
	}
	if err != nil {
		if !writeEffortError(w, err) {
			http.Error(w, "invalid legacy effort settings", http.StatusBadRequest)
		}
		return
	}
	writeJSON(w, requestedSettings(req))
}
