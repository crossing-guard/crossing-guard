package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
)

// The console never asks a person to type a path for a handoff's folder (team
// rest-of-release plan §14 Q19): it lists the checkouts this device knows and offers
// "Choose a folder…", which opens the operating system's own folder dialog. A browser
// cannot read an absolute path out of its own picker, so the daemon — a process of the
// same person on the same machine — shows the dialog and answers the folder chosen.

// registerFolderChooseRoutes registers the folder dialog route.
func registerFolderChooseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/folder/choose", handleFolderChoose)
}

// folderChooseRequest is POST /api/folder/choose. Start is the folder the dialog
// opens at; it is ignored unless it is an existing directory.
type folderChooseRequest struct {
	Start string `json:"start"`
}

// folderChooseResponse is the answer: Chosen is false, and Path empty, when the
// person closed the dialog without choosing.
type folderChooseResponse struct {
	Chosen bool   `json:"chosen"`
	Path   string `json:"path,omitempty"`
}

// folderChooseError is the refusal body: a sentence and a data code.
type folderChooseError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// Refusal codes of the folder dialog route.
const (
	folderChooseUnavailable = "picker_unavailable"
	folderChooseFailed      = "picker_failed"
	folderChooseInvalid     = "invalid_request"
	// folderChooseOpen: a dialog this route opened is still on screen.
	folderChooseOpen = "picker_open"
)

// folderDialogOpen is set while one dialog is on screen. There is one screen and one
// person: a second request while a dialog is open is refused, never stacked.
var folderDialogOpen atomic.Bool

// errFolderChooseCancelled is what a chooser returns when the person closed the
// dialog without choosing; errFolderChooseUnsupported when this platform has none.
var (
	errFolderChooseCancelled   = errors.New("the folder dialog was closed without a choice")
	errFolderChooseUnsupported = errors.New("this platform has no folder dialog the daemon can open")
)

// chooseFolder opens the operating system's folder dialog and returns the folder
// chosen. The platform file supplies it; a test replaces it.
var chooseFolder = systemChooseFolder

func writeFolderChooseError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(folderChooseError{Error: message, Code: code})
}

// handleFolderChoose shows the folder dialog and waits for the person. It has no
// timer of its own: the dialog ends when the person chooses or closes it, or when the
// request is abandoned.
func handleFolderChoose(w http.ResponseWriter, r *http.Request) {
	var req folderChooseRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeFolderChooseError(w, http.StatusBadRequest, folderChooseInvalid, "body must be {\"start\": \"<folder>\"}; start may be empty")
		return
	}
	if !folderDialogOpen.CompareAndSwap(false, true) {
		writeFolderChooseError(w, http.StatusConflict, folderChooseOpen, "a folder dialog is already open; choose or close it first")
		return
	}
	defer folderDialogOpen.Store(false)
	path, err := chooseFolder(r.Context(), existingDirectory(req.Start))
	switch {
	case errors.Is(err, errFolderChooseCancelled) || errors.Is(err, context.Canceled):
		writeJSON(w, folderChooseResponse{})
	case errors.Is(err, errFolderChooseUnsupported):
		writeFolderChooseError(w, http.StatusNotImplemented, folderChooseUnavailable, err.Error())
	case err != nil:
		writeFolderChooseError(w, http.StatusInternalServerError, folderChooseFailed, "the folder dialog could not be shown: "+err.Error())
	default:
		writeJSON(w, folderChooseResponse{Chosen: true, Path: filepath.Clean(path)})
	}
}

// existingDirectory returns path when it names a directory that exists, else "": a
// dialog told to open at a folder that is gone would fail instead of opening.
func existingDirectory(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ""
	}
	return path
}
