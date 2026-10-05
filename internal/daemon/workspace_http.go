package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/workspace"
	"crossing-guard/store"
)

type workspaceHost struct {
	service *workspace.SelectionService
	index   *store.Index
}

const workspaceConfigName = "workspace.json"

func newWorkspaceHost(dataDir, storePath string) (*workspaceHost, error) {
	configFile := filepath.Join(dataDir, workspaceConfigName)
	config, err := workspace.LoadConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("workspace configuration: %w", err)
	}
	index, err := store.Open(storePath)
	if err != nil {
		return nil, fmt.Errorf("workspace store: %w", err)
	}
	if _, err := index.ReleaseTerminalTaskWorkspaceLeases(time.Now().UnixMilli()); err != nil {
		index.Close()
		return nil, fmt.Errorf("workspace lease recovery: %w", err)
	}
	return &workspaceHost{service: workspace.NewSelectionService(config, index), index: index}, nil
}

func (h *workspaceHost) close() {
	if h != nil && h.index != nil {
		_ = h.index.Close()
	}
}

func registerWorkspaceRoutes(mux *http.ServeMux, host *workspaceHost) {
	mux.HandleFunc("GET /api/workspaces", func(w http.ResponseWriter, r *http.Request) {
		if host == nil {
			writeWorkspaceProblem(w, http.StatusServiceUnavailable, "workspace_unavailable",
				"Workspace selection is not configured. Add a valid workspace.json to the daemon data directory.")
			return
		}
		subject, err := workspaceSubjectFromQuery(r)
		if err != nil {
			writeWorkspaceProblem(w, http.StatusBadRequest, "invalid_subject", err.Error())
			return
		}
		roots, err := host.index.WorkspaceSubjectRoots(subject.Kind, subject.ID)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		set, err := host.service.List(subject, roots)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		writeJSON(w, set)
	})

	mux.HandleFunc("POST /api/workspace-selections", func(w http.ResponseWriter, r *http.Request) {
		if host == nil {
			writeWorkspaceProblem(w, http.StatusServiceUnavailable, "workspace_unavailable",
				"Workspace selection is not configured. Add a valid workspace.json to the daemon data directory.")
			return
		}
		var request workspace.BindRequest
		if err := decodeWorkspaceJSON(w, r, &request); err != nil {
			writeWorkspaceProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		roots, err := host.index.WorkspaceSubjectRoots(request.SubjectKind, request.SubjectID)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		selection, created, err := host.service.Bind(request, roots)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		if created {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
		}
		writeJSON(w, selection)
	})
}

// The live diff routes (workspace-panes plan §4.1; owner decision O-G). They
// need no workspace file: the review service reads the folder the session
// recorded. Subject-scoped failures travel in the body as a typed problem so
// the pane can render the state; only a malformed request is an HTTP error.
func registerWorkspaceDiffRoutes(mux *http.ServeMux, review *workspace.ReviewService) {
	mux.HandleFunc("GET /api/workspace-diff/checkout", requireReviewService(review, func(w http.ResponseWriter, r *http.Request) {
		subject, ok := workspaceDiffSubject(w, r)
		if !ok {
			return
		}
		response, err := review.Checkout(r.Context(), subject)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		writeJSON(w, response)
	}))
	mux.HandleFunc("GET /api/workspace-diff", requireReviewService(review, func(w http.ResponseWriter, r *http.Request) {
		subject, ok := workspaceDiffSubject(w, r)
		if !ok {
			return
		}
		scope, base, ok := workspaceDiffScope(w, r)
		if !ok {
			return
		}
		response, err := review.Diff(r.Context(), subject, scope, base)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		writeJSON(w, response)
	}))
	mux.HandleFunc("GET /api/workspace-diff/file", requireReviewService(review, func(w http.ResponseWriter, r *http.Request) {
		subject, ok := workspaceDiffSubject(w, r)
		if !ok {
			return
		}
		scope, base, ok := workspaceDiffScope(w, r)
		if !ok {
			return
		}
		path := r.URL.Query().Get("path")
		if !changeenv.ValidRepositoryPath(path) {
			writeWorkspaceProblem(w, http.StatusBadRequest, "invalid_request", "path must be a relative repository path")
			return
		}
		response, err := review.File(r.Context(), subject, scope, base, path)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		writeJSON(w, response)
	}))
	mux.HandleFunc("GET /api/workspace-diff/refs", requireReviewService(review, func(w http.ResponseWriter, r *http.Request) {
		subject, ok := workspaceDiffSubject(w, r)
		if !ok {
			return
		}
		response, err := review.Refs(r.Context(), subject)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		writeJSON(w, response)
	}))
}

// The Files pane's routes (console-files-pane-plan §4): one directory's
// children and one bounded file read, over the same review service.
func registerWorkspaceFilesRoutes(mux *http.ServeMux, review *workspace.ReviewService) {
	mux.HandleFunc("GET /api/workspace-files", requireReviewService(review, func(w http.ResponseWriter, r *http.Request) {
		subject, ok := workspaceDiffSubject(w, r)
		if !ok {
			return
		}
		dir, ok := changeenv.ValidTreeDir(r.URL.Query().Get("dir"))
		if !ok {
			writeWorkspaceProblem(w, http.StatusBadRequest, "invalid_request", "dir must be a relative repository path")
			return
		}
		response, err := review.Tree(r.Context(), subject, dir)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		writeJSON(w, response)
	}))
	mux.HandleFunc("GET /api/workspace-files/read", requireReviewService(review, func(w http.ResponseWriter, r *http.Request) {
		subject, ok := workspaceDiffSubject(w, r)
		if !ok {
			return
		}
		path := r.URL.Query().Get("path")
		if !changeenv.ValidTreePath(path) {
			writeWorkspaceProblem(w, http.StatusBadRequest, "invalid_request", "path must be a relative repository path outside .git")
			return
		}
		response, err := review.Read(r.Context(), subject, path)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		writeJSON(w, response)
	}))
}

// requireReviewService guards the git-scope routes: without the governor's store
// the review service is absent (a degraded start), and every scope answers
// review-unavailable rather than dereferencing it.
func requireReviewService(review *workspace.ReviewService, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if review == nil {
			writeWorkspaceProblem(w, http.StatusServiceUnavailable, "review-unavailable",
				"Recorded session folders cannot be read because the governor did not start. See Governance for the reason.")
			return
		}
		next(w, r)
	}
}

// workspaceDiffSubject reads the session identity the console carries (`id`,
// with `session_id` accepted for parity with the selection routes).
func workspaceDiffSubject(w http.ResponseWriter, r *http.Request) (workspace.Subject, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		id = strings.TrimSpace(r.URL.Query().Get("session_id"))
	}
	if id == "" {
		writeWorkspaceProblem(w, http.StatusBadRequest, "invalid_subject", "id is required")
		return workspace.Subject{}, false
	}
	return workspace.Subject{Kind: "session", ID: id}, true
}

func workspaceDiffScope(w http.ResponseWriter, r *http.Request) (scope, base string, ok bool) {
	scope = strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = changeenv.LiveScopes[0]
	}
	if !slices.Contains(changeenv.LiveScopes, scope) {
		writeWorkspaceProblem(w, http.StatusBadRequest, "invalid_request", "scope must be one of "+strings.Join(changeenv.LiveScopes, ", "))
		return "", "", false
	}
	base = strings.TrimSpace(r.URL.Query().Get("base"))
	if err := changeenv.ValidBaseRef(base); err != nil {
		writeWorkspaceProblem(w, http.StatusBadRequest, "invalid_request", "base must be a ref name")
		return "", "", false
	}
	return scope, base, true
}

func workspaceSubjectFromQuery(r *http.Request) (workspace.Subject, error) {
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if (taskID == "") == (sessionID == "") {
		return workspace.Subject{}, fmt.Errorf("provide exactly one task_id or session_id")
	}
	if taskID != "" {
		return workspace.Subject{Kind: "task", ID: taskID}, nil
	}
	return workspace.Subject{Kind: "session", ID: sessionID}, nil
}

func decodeWorkspaceJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func writeWorkspaceError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusInternalServerError, "workspace_error", "Workspace selection failed."
	switch {
	case errors.Is(err, store.ErrWorkspaceSelectionVersionConflict),
		errors.Is(err, store.ErrWorkspaceSelectionIdempotencyConflict),
		errors.Is(err, store.ErrWorkspaceSelectionNotCurrent),
		errors.Is(err, store.ErrWorkspaceSelectionLeased),
		errors.Is(err, store.ErrWorkspaceLeaseConflict):
		status, code, message = http.StatusConflict, "workspace_conflict", err.Error()
	case strings.Contains(err.Error(), "candidate is unavailable") || strings.Contains(err.Error(), "candidate_id"):
		status, code, message = http.StatusConflict, "candidate_unavailable", err.Error()
	case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "subject"):
		status, code, message = http.StatusBadRequest, "invalid_request", err.Error()
	}
	writeWorkspaceProblem(w, status, code, message)
}

// workspaceProblemResponse is the typed error envelope of every workspace route.
type workspaceProblemResponse struct {
	Error workspaceProblemBody `json:"error"`
}

type workspaceProblemBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeWorkspaceProblem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, workspaceProblemResponse{Error: workspaceProblemBody{Code: code, Message: message}})
}
