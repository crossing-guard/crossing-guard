package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"crossing-guard/internal/speech"
	"crossing-guard/internal/transcription"
	"crossing-guard/internal/workspace"
)

// Speech routes register under /api/speech/... ; the browser calls the canonical
// /api/v1/speech/... which securityMiddleware rewrites before the mux.

// JSON request bodies on these routes are tiny; the caps only bound abuse.
const (
	speechDisclosureBodyBytes = 4 << 10
	speechCreateBodyBytes     = 8 << 10
	speechFinishBodyBytes     = 1 << 10
)

var (
	speechMu      sync.Mutex
	speechLoaded  *speech.Loaded
	speechProblem string
	dictations    *transcription.Service
	speechStreams chan struct{}
	speechStop    chan struct{}
)

// speechWorkspace is the one thing the hint source needs from workspace
// selection: revalidate a selection and read its branch.
type speechWorkspace interface {
	Revalidate(id string, expectedVersion int64) (workspace.Candidate, error)
}

// speechHintSource derives hints only from validated inputs: the project name
// is the basename of a cwd that passed validateChatCwd; the branch exists only
// when the dictation resolves through a workspace selection.
type speechHintSource struct{ workspace speechWorkspace }

func (h speechHintSource) Hints(_ context.Context, request transcription.HintRequest) (transcription.Hints, error) {
	var hints transcription.Hints
	if request.WorkspaceSelectionID != "" && h.workspace != nil {
		candidate, err := h.workspace.Revalidate(request.WorkspaceSelectionID, request.WorkspaceBindingVersion)
		if err == nil {
			hints.ProjectName = filepath.Base(candidate.Root)
			hints.BranchName = candidate.Branch
			return hints, nil
		}
	}
	if strings.TrimSpace(request.Cwd) != "" {
		cwd, err := validateChatCwd(request.Cwd)
		if err == nil && cwd != "" {
			hints.ProjectName = filepath.Base(cwd)
		}
	}
	return hints, nil
}

func initSpeech(dataDir string, ws speechWorkspace) {
	speechMu.Lock()
	defer speechMu.Unlock()
	loaded, err := speech.Load(dataDir)
	if err != nil {
		speechLoaded, speechProblem, dictations = nil, err.Error(), nil
		log.Printf("dictation unavailable (%s) — text-only composer remains available", speechProblem)
		return
	}
	speechLoaded, speechProblem = &loaded, ""
	if !loaded.Ready {
		dictations = nil
		log.Printf("dictation not ready: %s", strings.Join(loaded.Problems, "; "))
		return
	}
	clipRoot, err := speech.PrepareClipRoot(dataDir)
	if err != nil {
		speechProblem, dictations = err.Error(), nil
		log.Printf("dictation unavailable (%s)", speechProblem)
		return
	}
	service, err := transcription.NewService(transcription.ServiceOptions{
		Config: loaded.File.Transcription, Backend: loaded.Backend, ClipRoot: clipRoot,
		Hints: speechHintSource{workspace: ws}, Logf: log.Printf,
	})
	if err != nil {
		speechProblem, dictations = err.Error(), nil
		return
	}
	dictations = service
	config := loaded.File.Transcription
	speechStreams = make(chan struct{}, config.Streaming.MaxStreams)
	speechStop = make(chan struct{})
	go sweepDictations(service, time.Duration(config.Limits.SweepSeconds)*time.Second, speechStop)
	log.Printf("dictation ready: backend %s, clips under %s", config.Backend, clipRoot)
}

func sweepDictations(service *transcription.Service, every time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			service.Sweep()
		}
	}
}

func closeSpeech() {
	speechMu.Lock()
	defer speechMu.Unlock()
	if speechStop != nil {
		close(speechStop)
		speechStop = nil
	}
	speechLoaded, speechProblem, dictations, speechStreams = nil, "", nil, nil
}

func registerSpeechRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/speech/capabilities", handleSpeechCapabilities)
	mux.HandleFunc("POST /api/speech-disclosures", handleSpeechDisclosure)
	mux.HandleFunc("POST /api/speech/dictations", handleDictationCreate)
	mux.HandleFunc("POST /api/speech/dictations/{id}/frames", handleDictationFrame)
	mux.HandleFunc("GET /api/speech/dictations/{id}/stream", handleDictationStream)
	mux.HandleFunc("POST /api/speech/dictations/{id}/finish", handleDictationFinish)
	mux.HandleFunc("DELETE /api/speech/dictations/{id}", handleDictationCancel)
}

// speechCapabilities is the whole browser contract: what the backend is, what
// state it is in, and every value the composer and Settings need. The browser
// keeps no defaults of its own.
type speechCapabilities struct {
	State      string                      `json:"state"`
	Reason     string                      `json:"reason,omitempty"`
	ConfigPath string                      `json:"config_path,omitempty"`
	Problems   []string                    `json:"problems,omitempty"`
	Backend    *transcription.Capabilities `json:"backend,omitempty"`
	Disclosure *transcription.Disclosure   `json:"disclosure,omitempty"`
	Hints      *transcription.HintPolicy   `json:"hints,omitempty"`
	Audio      *transcription.Audio        `json:"audio,omitempty"`
	Limits     *transcription.Limits       `json:"limits,omitempty"`
	Streaming  *transcription.Streaming    `json:"streaming,omitempty"`
	UI         *transcription.UI           `json:"ui,omitempty"`
}

func currentSpeechCapabilities() speechCapabilities {
	speechMu.Lock()
	defer speechMu.Unlock()
	if speechLoaded == nil {
		return speechCapabilities{State: "unconfigured", Reason: speechProblem}
	}
	config := speechLoaded.File.Transcription
	out := speechCapabilities{ConfigPath: speechLoaded.Path, Problems: speechLoaded.Problems,
		Audio: &config.Audio, Limits: &config.Limits, Streaming: &config.Streaming, UI: &config.UI}
	policy := config.HintPolicyFor(config.Backend)
	out.Hints = &policy
	out.Disclosure = config.DisclosureFor(config.Backend)
	if dictations == nil {
		out.State = "unavailable"
		out.Reason = strings.Join(speechLoaded.Problems, "; ")
		if out.Reason == "" {
			out.Reason = speechProblem
		}
		return out
	}
	caps := dictations.Capabilities()
	out.State, out.Backend = "ready", &caps
	return out
}

func handleSpeechCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, currentSpeechCapabilities())
}

func requireDictations(w http.ResponseWriter) *transcription.Service {
	speechMu.Lock()
	service := dictations
	speechMu.Unlock()
	if service != nil {
		return service
	}
	writeSpeechError(w, http.StatusServiceUnavailable, "unavailable", "dictation is not set up on this machine")
	return nil
}

func handleSpeechDisclosure(w http.ResponseWriter, r *http.Request) {
	service := requireDictations(w)
	if service == nil {
		return
	}
	var body struct {
		Backend   string `json:"backend_id"`
		Operation string `json:"operation"`
		Version   int    `json:"disclosure_version"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, speechDisclosureBodyBytes)).Decode(&body); err != nil || body.Operation != "transcription" {
		writeSpeechError(w, http.StatusBadRequest, "invalid_request", "a backend_id, operation transcription, and disclosure_version are required")
		return
	}
	token, ttl, err := service.MintDisclosure(body.Backend, body.Version)
	if err != nil {
		writeSpeechServiceError(w, err)
		return
	}
	writeJSON(w, speechDisclosureResponse{Token: token, ExpiresInSeconds: int(ttl / time.Second)})
}

// speechDisclosureResponse is the minted disclosure token a client presents before a
// transcription backend will accept audio. Declared rather than a map literal so the
// public contract has a schema — ADR 0022: BYO clients cannot read Go.
type speechDisclosureResponse struct {
	Token            string `json:"token"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

func handleDictationCreate(w http.ResponseWriter, r *http.Request) {
	service := requireDictations(w)
	if service == nil {
		return
	}
	var body struct {
		Cwd                     string `json:"cwd"`
		WorkspaceSelectionID    string `json:"workspace_selection_id"`
		WorkspaceBindingVersion int64  `json:"workspace_binding_version"`
		DisclosureToken         string `json:"disclosure_token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, speechCreateBodyBytes)).Decode(&body); err != nil {
		writeSpeechError(w, http.StatusBadRequest, "invalid_request", "dictation request is not valid JSON")
		return
	}
	created, err := service.Create(r.Context(), transcription.CreateRequest{
		Hint: transcription.HintRequest{Cwd: body.Cwd, WorkspaceSelectionID: body.WorkspaceSelectionID,
			WorkspaceBindingVersion: body.WorkspaceBindingVersion},
		DisclosureToken: body.DisclosureToken,
	})
	if err != nil {
		writeSpeechServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

func handleDictationFrame(w http.ResponseWriter, r *http.Request) {
	service := requireDictations(w)
	if service == nil {
		return
	}
	seq, seqErr := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 64)
	offset, offsetErr := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if seqErr != nil || offsetErr != nil || seq < 0 || offset < 0 {
		writeSpeechError(w, http.StatusBadRequest, "invalid_request", "seq and offset are required")
		return
	}
	limit := service.Config().Streaming.MaxFrameBytes
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		writeSpeechError(w, http.StatusRequestEntityTooLarge, "frame_too_large", "audio frame exceeds the configured limit")
		return
	}
	if err := service.Frame(strings.TrimSpace(r.PathValue("id")), seq, offset, data); err != nil {
		writeSpeechServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleDictationStream(w http.ResponseWriter, r *http.Request) {
	service := requireDictations(w)
	if service == nil {
		return
	}
	events, ok := service.Events(strings.TrimSpace(r.PathValue("id")))
	if !ok {
		writeSpeechError(w, http.StatusNotFound, "not_found", "dictation not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	speechMu.Lock()
	streams := speechStreams
	speechMu.Unlock()
	select {
	case streams <- struct{}{}:
		defer func() { <-streams }()
	default:
		http.Error(w, "dictation stream budget exhausted", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	keepalive := time.NewTicker(time.Duration(service.Config().Streaming.KeepaliveSeconds) * time.Second)
	defer keepalive.Stop()
	var id int64
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-events:
			if !open {
				return
			}
			id++
			writeJSONSSE(w, flusher, event.Kind, id, event)
			if event.Kind == transcription.EventEnded {
				return
			}
		}
	}
}

func handleDictationFinish(w http.ResponseWriter, r *http.Request) {
	service := requireDictations(w)
	if service == nil {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	// reason is optional and informational; an empty or malformed body finishes
	// the dictation exactly like a release.
	_ = json.NewDecoder(io.LimitReader(r.Body, speechFinishBodyBytes)).Decode(&body)
	ended, err := service.Finish(strings.TrimSpace(r.PathValue("id")), body.Reason)
	if err != nil {
		writeSpeechServiceError(w, err)
		return
	}
	writeJSON(w, ended)
}

func handleDictationCancel(w http.ResponseWriter, r *http.Request) {
	service := requireDictations(w)
	if service == nil {
		return
	}
	if err := service.Cancel(strings.TrimSpace(r.PathValue("id"))); err != nil {
		writeSpeechServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type speechError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeSpeechServiceError(w http.ResponseWriter, err error) {
	var typed *transcription.Error
	switch {
	case errors.Is(err, transcription.ErrNotFound):
		writeSpeechError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, transcription.ErrBusy):
		writeSpeechError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, transcription.ErrNotListening):
		writeSpeechError(w, http.StatusConflict, "not_listening", err.Error())
	case errors.Is(err, transcription.ErrDisclosureRequired):
		writeSpeechError(w, http.StatusForbidden, "disclosure_required", err.Error())
	case errors.Is(err, transcription.ErrDisclosureInvalid):
		writeSpeechError(w, http.StatusForbidden, "disclosure_invalid", err.Error())
	case errors.Is(err, transcription.ErrNoDisclosure):
		writeSpeechError(w, http.StatusConflict, "no_disclosure", err.Error())
	case errors.As(err, &typed):
		writeSpeechError(w, http.StatusUnprocessableEntity, string(typed.State), typed.Message)
	default:
		log.Printf("dictation request failed: %v", err)
		writeSpeechError(w, http.StatusInternalServerError, "internal_error", "dictation request failed")
	}
}

func writeSpeechError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(speechError{Code: code, Message: message})
}
