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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"crossing-guard/internal/taskinput"
)

type ChatRequest struct {
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

// sessionMessageCarrierBoundary marks a receipt whose transport is the target
// session's own next boundary; the host owns the pending record.
const sessionMessageCarrierBoundary = "boundary"

type SessionMessageReceipt struct {
	State     string `json:"state"`              // accepted | delivered | expired | unavailable | unknown
	Tier      string `json:"tier,omitempty"`     // queued-delivery | none
	Boundary  string `json:"boundary,omitempty"` // where the message lands, in the vendor's own terms
	Carrier   string `json:"carrier,omitempty"`  // "" (adapter performed it) | boundary
	MessageID string `json:"message_id,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		sse.send(map[string]string{"type": "error", "text": "runtime task service unavailable"})
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
		// Typed input rejections are classified first: adapter messages such as
		// "not verified for the selected model" contain "mode" and would otherwise
		// highlight the wrong control.
		if errors.As(err, &inputErr) || strings.Contains(err.Error(), "task input") {
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
