package daemon

// Runtime integration handlers expose the existing guardcli installer registry through
// an authenticated preview/confirm contract. They never accept a config path, binary,
// argv, raw vendor JSON, or provider-launch request from the browser.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"crossing-guard/internal/guardcli"
)

var (
	runtimeIntegrationExecutable = os.Executable
)

func registerRuntimeIntegrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runtime-integrations", handleRuntimeIntegrations)
	mux.HandleFunc("POST /api/runtime-integrations/{runtime}/preview-connect", handleRuntimeIntegrationPreviewConnect)
	mux.HandleFunc("POST /api/runtime-integrations/{runtime}/connect", handleRuntimeIntegrationConnect)
	mux.HandleFunc("POST /api/runtime-integrations/{runtime}/preview-disconnect", handleRuntimeIntegrationPreviewDisconnect)
	mux.HandleFunc("POST /api/runtime-integrations/{runtime}/disconnect", handleRuntimeIntegrationDisconnect)
	mux.HandleFunc("POST /api/runtime-integrations/{runtime}/watch", handleRuntimeIntegrationWatchStart)
	mux.HandleFunc("GET /api/runtime-integrations/{runtime}/watch/{token}", handleRuntimeIntegrationWatchGet)
	mux.HandleFunc("POST /api/runtime-integrations/{runtime}/watch/{token}/confirm-visible", handleRuntimeIntegrationWatchConfirmVisible)
	registerRuntimeStatusRoutes(mux)
}

func handleRuntimeIntegrations(w http.ResponseWriter, _ *http.Request) {
	executable, err := runtimeIntegrationExecutable()
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "binary_unavailable",
			"Crossing Guard could not resolve its installed binary", nil)
		return
	}
	writeJSON(w, map[string]any{
		"integrations": guardcli.RuntimeConnections(executable),
		"note":         "Detection is read-only. Connecting, launching a provider, and verification are separate actions.",
	})
}

func handleRuntimeIntegrationPreviewConnect(w http.ResponseWriter, r *http.Request) {
	handleRuntimeIntegrationPreview(w, r, guardcli.ConnectionOperationConnect)
}

func handleRuntimeIntegrationPreviewDisconnect(w http.ResponseWriter, r *http.Request) {
	handleRuntimeIntegrationPreview(w, r, guardcli.ConnectionOperationDisconnect)
}

func handleRuntimeIntegrationPreview(w http.ResponseWriter, r *http.Request, operation string) {
	executable, err := runtimeIntegrationExecutable()
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "binary_unavailable",
			"Crossing Guard could not resolve its installed binary", nil)
		return
	}
	preview, err := guardcli.PreviewRuntimeConnection(r.PathValue("runtime"), operation, executable)
	if err != nil {
		writeGuardConnectionError(w, err, nil)
		return
	}
	token, expiresAt, err := runtimeIntegrationPreviews.issue(preview.Runtime, operation, preview.StateDigest)
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "token_unavailable",
			"Crossing Guard could not create a confirmation token", nil)
		return
	}
	writeJSON(w, map[string]any{
		"preview":       preview,
		"preview_token": token,
		"expires_at":    expiresAt.UTC().Format(time.RFC3339),
		"note":          "Preview is read-only. " + preview.DisplayName + " was not launched or changed.",
	})
}

func handleRuntimeIntegrationConnect(w http.ResponseWriter, r *http.Request) {
	handleRuntimeIntegrationCommit(w, r, guardcli.ConnectionOperationConnect)
}

func handleRuntimeIntegrationDisconnect(w http.ResponseWriter, r *http.Request) {
	handleRuntimeIntegrationCommit(w, r, guardcli.ConnectionOperationDisconnect)
}

func handleRuntimeIntegrationCommit(w http.ResponseWriter, r *http.Request, operation string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request struct {
		PreviewToken string `json:"preview_token"`
		Confirmed    bool   `json:"confirmed"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.PreviewToken == "" || !request.Confirmed {
		writeRuntimeIntegrationError(w, http.StatusBadRequest, "confirmation_required",
			"a current preview token and explicit confirmation are required", nil)
		return
	}
	entry, err := runtimeIntegrationPreviews.consume(request.PreviewToken, r.PathValue("runtime"), operation)
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusConflict, "preview_unavailable", err.Error(), nil)
		return
	}
	executable, err := runtimeIntegrationExecutable()
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "binary_unavailable",
			"Crossing Guard could not resolve its installed binary", nil)
		return
	}
	var result guardcli.RuntimeConnectionMutation
	if operation == guardcli.ConnectionOperationConnect {
		result, err = guardcli.ConnectRuntime(entry.Runtime, executable, entry.Digest)
	} else {
		result, err = guardcli.DisconnectRuntime(entry.Runtime, executable, entry.Digest)
	}
	// A connection changed (or may have, on a partial failure): the runtime
	// status read must not answer from observations taken before it.
	dropRuntimeObservationCache()
	if err != nil {
		writeGuardConnectionError(w, err, &result)
		return
	}
	writeJSON(w, result)
}

func writeGuardConnectionError(w http.ResponseWriter, err error, result *guardcli.RuntimeConnectionMutation) {
	var connectionErr *guardcli.RuntimeConnectionError
	if !errors.As(err, &connectionErr) {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "operation_failed",
			"runtime connection operation failed", result)
		return
	}
	status := http.StatusConflict
	switch connectionErr.Kind {
	case "unknown_runtime":
		status = http.StatusNotFound
	case "invalid_operation", "confirmation_required":
		status = http.StatusBadRequest
	case "unsupported_runtime", "unsafe_binary", "preview_blocked":
		status = http.StatusUnprocessableEntity
	case "install_failed", "consent_failed", "partial_connect", "partial_disconnect":
		status = http.StatusInternalServerError
	}
	writeRuntimeIntegrationError(w, status, connectionErr.Kind, connectionErr.Error(), result)
}

func writeRuntimeIntegrationError(w http.ResponseWriter, status int, code, message string, result *guardcli.RuntimeConnectionMutation) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{"error": code, "message": message}
	if result != nil {
		payload["result"] = result
	}
	_ = json.NewEncoder(w).Encode(payload)
}
