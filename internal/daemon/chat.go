package daemon

// Fallback chat: spawn the official vendor binaries with custom settings and
// stream normalized events back over SSE. This is
// the ONLY surface where we drive. Custom base URL / token here means the
// GLM-class or local backend — never extract or front vendor OAuth.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"crossing-guard/internal/observation"
	"crossing-guard/internal/taskinput"
	"crossing-guard/store"
)

type ChatRequest struct {
	ThinkingEffort       *store.ThinkingEffort `json:"thinking_effort,omitempty"`
	SessionEffortToken   string                `json:"session_effort_token,omitempty"`
	effortDisplayLabel   string
	effortSource         string
	effortSessionID      string
	effortMapping        string
	effortLegacy         bool
	effortCatalogDigest  string
	Runtime              string `json:"runtime"`
	Prompt               string `json:"prompt"`
	SessionID            string `json:"session_id,omitempty"`         // vendor resume handle, if set
	CatalogSessionID     string `json:"catalog_session_id,omitempty"` // exact existing session row selected in the console
	Model                string `json:"model,omitempty"`
	Mode                 string `json:"mode,omitempty"`            // canonical adapter-owned execution mode
	BaseURL              string `json:"base_url,omitempty"`        // ANTHROPIC_BASE_URL (claude)
	AuthToken            string `json:"auth_token,omitempty"`      // ANTHROPIC_AUTH_TOKEN (claude)
	PermissionMode       string `json:"permission_mode,omitempty"` // claude --permission-mode
	Sandbox              string `json:"sandbox,omitempty"`         // codex --sandbox
	OSS                  bool   `json:"oss,omitempty"`             // codex --oss
	LocalProvider        string `json:"local_provider,omitempty"`  // codex --local-provider
	Binary               string `json:"binary,omitempty"`          // override binary path
	ExtraArgs            string `json:"extra_args,omitempty"`
	Cwd                  string `json:"cwd,omitempty"` // compatibility path when no workspace selection is supplied
	WorkspaceSelectionID string `json:"workspace_selection_id,omitempty"`
	// AllowSharedSession is the person's explicit override of the ownership
	// rule: send even though another process is using the session (Part A).
	AllowSharedSession bool     `json:"allow_shared_session,omitempty"`
	InputScopeID       string   `json:"input_scope_id,omitempty"`
	InputIDs           []string `json:"input_ids,omitempty"`
	IdempotencyKey     string   `json:"idempotency_key,omitempty"`
	// HandoffTicket marks the first turn of a console Open of a handoff (team
	// rest-of-release plan §6.3): the ticket the open route returned. The task
	// service admits it through the handoff owner, and only this launch's
	// process carries it in its environment (chatLaunchEnv).
	HandoffTicket string `json:"handoff_ticket,omitempty"`
}

// chatLaunchEnv is the ONE place a runtime process's environment is built (team
// rest-of-release plan §6.4): every chat_*.go site that gives a process an
// environment goes through it — session launches, model listings and the
// OpenCode sites alike. It starts from the daemon's own environment with any
// inherited handoff ticket removed, so a daemon started from inside an opened
// session stamps no launch, and adds the ticket only for the first turn of an
// Open, which is the one request that carries one. Launches that assign no
// environment inherit the daemon's, which start-up cleared of the same
// variable (dropInheritedHandoffTicket).
func chatLaunchEnv(req ChatRequest) []string {
	ticketPrefix := observation.HandoffTicketEnv + "="
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+1)
	for _, entry := range inherited {
		if !strings.HasPrefix(entry, ticketPrefix) {
			env = append(env, entry)
		}
	}
	if req.HandoffTicket != "" {
		env = append(env, ticketPrefix+req.HandoffTicket)
	}
	return env
}

// withoutEnv returns env without the entries of the named variables.
func withoutEnv(env []string, names ...string) []string {
	kept := env[:0:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(names, name) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// dropInheritedHandoffTicket removes a handoff ticket from the daemon's own
// environment, once, at start-up (plan §17.1 F-6): several runtime launches
// assign no environment and inherit the daemon's, so a daemon started from
// inside an opened session would otherwise hand its ticket to every one of them.
func dropInheritedHandoffTicket() error {
	return os.Unsetenv(observation.HandoffTicketEnv)
}

type sseWriter struct {
	w  http.ResponseWriter
	f  http.Flusher
	mu sync.Mutex
}

func (s *sseWriter) send(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(v)
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.f.Flush()
}

// ChatDriver is one vendor's fallback-chat behavior: build the subprocess and
// map its streamed events to UI events. Implementations live in chat_<vendor>.go
// and self-register via init(); handleChat never names a vendor.
// BuildCmd returns an error rather than a command that will fail at Start():
// binary resolution is the one step whose failure needs its own message (which
// runtime, where we looked), and exec's "file not found in $PATH" cannot carry
// that. See binpath.go.
type ChatDriver interface {
	BuildCmd(req ChatRequest, launch ChatLaunchContext) (*exec.Cmd, error)
	ProjectEvent(obj map[string]any) []ChatEvent
}

// A server-backed runtime can supply its own wire protocol while the existing
// task process runner retains ownership of start, stop, stderr and process wait.
type chatProcessProtocolFactory interface {
	ProcessProtocol(ChatRequest, ChatLaunchContext, *exec.Cmd) chatProcessProtocol
}

type chatProcessProtocol interface {
	Run(io.ReadCloser, func(ChatEvent)) error
}

// A one-shot protocol consumes stdout through EOF so its process can be waited
// naturally. Server-backed protocols keep the existing terminate-and-reap path.
type chatProtocolNaturalExit interface {
	WaitForNaturalExit() bool
}

// SessionIdentity is the canonical address of one session: the runtime that
// owns it, its catalog id, and its native id. Delivery is addressed by this,
// never by a task (helper-session-attachment plan D5).
type SessionIdentity struct {
	Runtime   string `json:"runtime"`
	CatalogID string `json:"catalog_id,omitempty"`
	NativeID  string `json:"native_id,omitempty"`
}

// sessionMessageDeliverer is an optional port on the existing runtime driver.
// The adapter answers with the strongest tier it can prove for THIS session
// now: a direct vendor verb it already performed, or the boundary carrier
// (Carrier == sessionMessageCarrierBoundary) — in which case the host records
// one pending message the session's own next hook/plugin boundary consumes.
// A queued receipt is not evidence of consumption. No resume fallback is implied.
type sessionMessageDeliverer interface {
	DeliverSessionMessage(context.Context, SessionIdentity, string) SessionMessageReceipt
}

// sessionInbox is one target session's inbox resolved from the runtime's own
// registry: a live socket path plus the registry facts that prove the path
// belongs to the session the caller named. The generic layer never reads a
// vendor registry; the runtime's resolver owns the shape.
type sessionInbox struct {
	SocketPath  string
	RegistryPID int
	SessionID   string // the registry row's own session id, cross-checked by the resolver
	Cwd         string
	ProcStart   string
	Status      string
}

// sessionInboxResolver is an optional port (session-message-layer plan §5.1):
// the runtime's adapter resolves one canonical identity to its live inbox. The
// bool means safe to use — a row was found and its path is vetted by the
// vendor's own rules; the string carries the reason when the bool is false
// (no live inbox, unvettable path, ambiguous identity, stale registry row, or
// a path the vendor moved aside). A zero inbox with a reason is the
// not-found form. Detection of registry ambiguity lives in the resolver (it
// can see the rows); the generic layer only refuses to choose.
type sessionInboxResolver interface {
	ResolveSessionInbox(identity SessionIdentity) (sessionInbox, bool, string)
}

// sessionMessageCarrierBoundary marks a receipt whose transport is the target
// session's own next boundary; the host owns the pending record.
const sessionMessageCarrierBoundary = "boundary"

type SessionMessageReceipt struct {
	State     string `json:"state"`              // pending | started | not_requested | accepted | delivered | expired | unavailable | unknown
	Tier      string `json:"tier,omitempty"`     // queued-delivery | socket-post | none
	Boundary  string `json:"boundary,omitempty"` // where the message lands, in the vendor's own terms
	Carrier   string `json:"carrier,omitempty"`  // "" (adapter performed it) | boundary | socket
	MessageID string `json:"message_id,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// ReasonClass is the structural cause of an unavailable receipt on an
	// acting claim (escalation-delivery plan §4): attended_session,
	// grant:<refusal>, dry_run, resume_error, interrupted, and the rest.
	ReasonClass string `json:"reason_class,omitempty"`
}

// sessionMessageCarrierSocket marks a receipt whose transport was a direct post
// to the target session's own inbox. No pending record exists for a socket
// post: the receipt on the run is the whole of the evidence, and the vendor's
// inbound controls may hold, drop, or expire the message after transport
// acceptance (session-message-layer plan D5, red-team RT-3).
const sessionMessageCarrierSocket = "socket"

// ChatLaunchContext carries daemon-owned identity that exists only after task
// admission. It is not part of ChatRequest, its JSON contract, or its idempotency
// digest. Providers may ignore fields they do not need.
type ChatLaunchContext struct {
	TaskID  string
	DataDir string
	Inputs  []taskinput.ResolvedInput
}

// chatRequestCanonicalizer is an optional semantic-validation seam on the same
// registered driver. There is deliberately no second validator registry.
type chatRequestCanonicalizer interface {
	CanonicalizeChatRequest(ChatRequest) (ChatRequest, error)
}

// chatInputValidator lets the existing provider adapter reject an unproved
// kind/model/mode combination before any provider process starts.
type chatInputValidator interface {
	ValidateChatInputs(ChatRequest, []taskinput.ResolvedInput) error
}

var chatDrivers = map[string]ChatDriver{}

func registerChatDriver(name string, d ChatDriver) {
	if name == "" || d == nil {
		panic("chat driver registration requires a name and driver")
	}
	if _, exists := chatDrivers[name]; exists {
		panic("duplicate chat driver registration: " + name)
	}
	chatDrivers[name] = d
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := decodeManagedJSON(w, r, &req, 0); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sse := &sseWriter{w: w, f: flusher}
	if runtimeTasks == nil {
		sse.send(map[string]string{"type": "error", "text": runtimeTasksUnavailable("runtime task service unavailable")})
		sse.send(map[string]string{"type": "done"})
		return
	}
	key := req.IdempotencyKey
	if key == "" {
		key = r.Header.Get("Idempotency-Key")
	}
	if key == "" {
		key = newTaskID()
	}
	task, _, err := runtimeTasks.Create(req, key)
	if err != nil {
		field := ""
		var inputErr *taskinput.Error
		var effortErr *EffortError
		// Typed input rejections are classified first: adapter messages such as
		// "not verified for the selected model" contain "mode" and would otherwise
		// highlight the wrong control.
		if errors.As(err, &effortErr) || errors.Is(err, store.ErrSessionEffortConflict) {
			field = "thinking_effort"
		} else if errors.As(err, &inputErr) || strings.Contains(err.Error(), "task input") {
			field = "attachments"
		} else if strings.Contains(err.Error(), "working directory") {
			field = "cwd"
		} else if strings.Contains(err.Error(), "mode") {
			field = "mode"
		}
		if field != "" {
			sse.send(map[string]string{"type": "input_error", "field": field, "text": err.Error()})
		} else {
			sse.send(map[string]string{"type": "error", "text": err.Error()})
		}
		sse.send(map[string]string{"type": "done"})
		return
	}
	sse.send(map[string]any{"type": "task", "task_id": task.ID, "lifecycle": task.Lifecycle})
	subscription, replay, err := runtimeTasks.Subscribe(task.ID, 0)
	if err != nil {
		sse.send(map[string]string{"type": "error", "text": err.Error()})
		sse.send(map[string]string{"type": "done"})
		return
	}
	defer runtimeTasks.Unsubscribe(subscription.ID)
	lastSequence := int64(0)
	forward := func(event TaskEvent) bool {
		if event.Sequence <= lastSequence {
			return false
		}
		lastSequence = event.Sequence
		sse.send(event.Payload)
		if strings.HasPrefix(event.Kind, "task.") && terminalTaskLifecycle(TaskLifecycle(strings.TrimPrefix(event.Kind, "task."))) {
			if anyString(event.Payload["type"]) != "done" {
				sse.send(map[string]string{"type": "done"})
			}
			return true
		}
		return false
	}
	for _, event := range replay {
		if forward(event) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return // subscriber lifetime is not process lifetime
		case event, ok := <-subscription.Events:
			if !ok {
				return
			}
			if forward(event) {
				return
			}
		}
	}
}

func validateChatMode(driver ChatDriver, mode string) error {
	if mode == "" {
		return nil
	}
	provider, ok := driver.(chatCapabilityProvider)
	if !ok {
		return fmt.Errorf("runtime does not accept a canonical mode")
	}
	for _, supported := range provider.ChatCapability().Modes {
		if supported.ID == mode {
			return nil
		}
	}
	return fmt.Errorf("selected mode is not supported by this runtime")
}

func validateChatCwd(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("choose a working directory before sending")
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("working directory must be an absolute path")
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("working directory is unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory")
	}
	return clean, nil
}

// splitArgs splits a raw extra-args string on whitespace. (Quoting is not
// supported: ExtraArgs is an operator convenience, not a shell.)
func splitArgs(s string) []string { return strings.Fields(s) }
