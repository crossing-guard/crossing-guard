package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/internal/taskinput"
)

var (
	taskInputs        *taskinput.Service
	taskInputsConfig  taskinput.Config
	taskInputsProblem string
)

func initTaskInputs(dataDir string) {
	loaded, err := taskinput.LoadConfig(dataDir)
	if err != nil {
		taskInputs, taskInputsConfig, taskInputsProblem = nil, taskinput.Config{}, err.Error()
		return
	}
	service, err := taskinput.NewService(filepath.Join(dataDir, "task-inputs"), loaded.Config, nil)
	if err != nil {
		taskInputs, taskInputsConfig, taskInputsProblem = nil, taskinput.Config{}, err.Error()
		return
	}
	taskInputs, taskInputsConfig, taskInputsProblem = service, loaded.Config, ""
}

// configuredTaskInputs returns the staging service as the claimer port, or a true
// nil when task inputs are unconfigured. Passing the typed nil pointer directly
// would satisfy the interface and turn every "unavailable" guard into a nil
// dereference on task completion.
func configuredTaskInputs() taskInputClaimer {
	if taskInputs == nil {
		return nil
	}
	return taskInputs
}

func closeTaskInputs() {
	taskInputs, taskInputsConfig, taskInputsProblem = nil, taskinput.Config{}, ""
}

func registerTaskInputRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/task-inputs", handleTaskInputStage)
	mux.HandleFunc("GET /api/task-inputs", handleTaskInputList)
	mux.HandleFunc("GET /api/task-inputs/{input}/content", handleTaskInputPreview)
	mux.HandleFunc("DELETE /api/task-inputs/{input}", handleTaskInputRemove)
}

func requireTaskInputs(w http.ResponseWriter) *taskinput.Service {
	if taskInputs != nil {
		return taskInputs
	}
	problem := taskInputsProblem
	if problem == "" {
		problem = "task input admission is unavailable"
	}
	writeTaskInputError(w, http.StatusServiceUnavailable, &taskinput.Error{Code: "unavailable", Message: problem})
	return nil
}

func handleTaskInputStage(w http.ResponseWriter, r *http.Request) {
	service := requireTaskInputs(w)
	if service == nil {
		return
	}
	limit := taskInputsConfig.Limits.MaxSourceBytesPerItem + 1<<20
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeTaskInputError(w, http.StatusBadRequest, &taskinput.Error{Code: "invalid_multipart", Message: "attachment upload is invalid or too large"})
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeTaskInputError(w, http.StatusBadRequest, &taskinput.Error{Code: "file_required", Message: "exactly one file field is required"})
		return
	}
	file, err := files[0].Open()
	if err != nil {
		writeTaskInputError(w, http.StatusBadRequest, err)
		return
	}
	defer func() { _ = file.Close() }()
	scope, input, err := service.Stage(r.Context(), taskInputScope(r), taskinput.Source(r.FormValue("source")), files[0].Filename, file)
	if err != nil {
		writeTaskInputServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"scope": scope, "input": input})
}

func handleTaskInputList(w http.ResponseWriter, r *http.Request) {
	service := requireTaskInputs(w)
	if service == nil {
		return
	}
	scope, err := service.List(taskInputScope(r))
	if err != nil {
		writeTaskInputServiceError(w, err)
		return
	}
	writeJSON(w, scope)
}

func handleTaskInputPreview(w http.ResponseWriter, r *http.Request) {
	service := requireTaskInputs(w)
	if service == nil {
		return
	}
	file, input, err := service.Preview(taskInputScope(r), strings.TrimSpace(r.PathValue("input")))
	if err != nil {
		writeTaskInputServiceError(w, err)
		return
	}
	defer func() { _ = file.Close() }()
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Text previews are display bytes, never a document: an admitted .html file must
	// not be rendered as HTML on the daemon origin, so the extension-derived media
	// type is display metadata only and the wire type is always plain text.
	mediaType := "text/plain; charset=utf-8"
	if input.Kind == taskinput.KindImage {
		mediaType = "image/png"
	}
	w.Header().Set("Content-Type", mediaType)
	http.ServeContent(w, r, "", time.Time{}, file)
}

func handleTaskInputRemove(w http.ResponseWriter, r *http.Request) {
	service := requireTaskInputs(w)
	if service == nil {
		return
	}
	scope, err := service.Remove(taskInputScope(r), strings.TrimSpace(r.PathValue("input")))
	if err != nil {
		writeTaskInputServiceError(w, err)
		return
	}
	writeJSON(w, scope)
}

func taskInputScope(r *http.Request) string {
	if scope := strings.TrimSpace(r.Header.Get("X-CG-Input-Scope")); scope != "" {
		return scope
	}
	return strings.TrimSpace(r.URL.Query().Get("scope"))
}

func writeTaskInputServiceError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var inputErr *taskinput.Error
	if !errors.As(err, &inputErr) {
		status = http.StatusInternalServerError
	}
	if inputErr != nil && (inputErr.Code == "scope_not_found" || inputErr.Code == "input_not_found") {
		status = http.StatusNotFound
	}
	if inputErr != nil && (inputErr.Code == "scope_closed" || inputErr.Code == "scope_expired") {
		status = http.StatusConflict
	}
	writeTaskInputError(w, status, err)
}

func writeTaskInputError(w http.ResponseWriter, status int, err error) {
	inputErr := &taskinput.Error{Code: "internal_error", Message: "task input operation failed"}
	var typed *taskinput.Error
	if errors.As(err, &typed) {
		inputErr = typed
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(inputErr)
}
