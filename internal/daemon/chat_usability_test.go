package daemon

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateChatCwd(t *testing.T) {
	dir := t.TempDir()
	if got, err := validateChatCwd(dir + string(os.PathSeparator) + "."); err != nil || got != filepath.Clean(dir) {
		t.Fatalf("valid cwd: got %q, %v", got, err)
	}
	for name, cwd := range map[string]string{
		"empty":    "",
		"relative": "relative/path",
		"missing":  filepath.Join(dir, "missing"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateChatCwd(cwd); err == nil {
				t.Fatalf("validateChatCwd(%q) succeeded", cwd)
			}
		})
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateChatCwd(file); err == nil {
		t.Fatal("file accepted as cwd")
	}
}

func TestHandleChatRejectsCwdBeforeSpawn(t *testing.T) {
	installTestRuntimeTasks(t)
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"runtime":"codex","prompt":"hello","cwd":"relative"}`))
	rec := httptest.NewRecorder()
	handleChat(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, `"type":"input_error"`) || !strings.Contains(body, `"field":"cwd"`) {
		t.Fatalf("missing structured cwd recovery: %s", body)
	}
}

func TestVendorAuthFailureClassification(t *testing.T) {
	for _, line := range []string{
		"Failed to authenticate: OAuth session expired and could not be refreshed",
		"authentication required",
		"Not logged in",
	} {
		if !isVendorAuthFailure(line) {
			t.Errorf("did not classify %q", line)
		}
	}
	if isVendorAuthFailure("connection refused") {
		t.Fatal("network failure misclassified as vendor auth")
	}
}

func TestVendorAuthStatusRejectsUnknownRuntime(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/chat/auth?runtime=other", nil)
	rec := httptest.NewRecorder()
	handleVendorAuthStatus(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestConsoleSessionRailAndAuthRecoveryContract(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	sessions := read("js/views/sessions.js")
	for _, required := range []string{"launch_cwd", "projnew", "projtoggle", "projmode", "projname", "RAIL_MODES", "SESSION_PAGE_SIZE = 15", "view=repository", "aria-expanded", "aria-controls", "sessionRailRenderGeneration", "++loadGeneration", "preserveRailHeader(head, load, focusNode, isCurrent)", "head.isConnected", "search reaches all sessions", "fresh: true, cwd: group.launch_cwd", "Selected session · outside this page", "el('button', 'sess')"} {
		if !bytes.Contains(sessions, []byte(required)) {
			t.Errorf("session rail lost %q", required)
		}
	}
	chat := read("js/views/chat.js")
	taskAPI := read("js/task/task-api.js")
	taskSemantics := read("js/task/task-event-semantics.js")
	for _, required := range []string{"S.chatCwd", "chat-capabilities.js", "/api/chat/auth?runtime=", "/api/chat/auth/start", "auth_required", "This turn was not replayed", "input_error", "mode: selMode.value"} {
		if !bytes.Contains(chat, []byte(required)) {
			t.Errorf("chat recovery lost %q", required)
		}
	}
	for _, forbidden := range []string{"const MODES", "const MODELS", "runtime === 'claude'", "runtime === 'codex'", "permission_mode:", "sandbox:"} {
		if bytes.Contains(chat, []byte(forbidden)) {
			t.Errorf("chat view regained runtime-specific behavior %q", forbidden)
		}
	}
	settings := read("js/views/settings-runtimes.js")
	if !bytes.Contains(settings, []byte("capabilityPairs(capabilities)")) {
		t.Error("settings runtime choices must come from chat capabilities")
	}
	if bytes.Contains(sessions, []byte("d.runtime === 'claude'")) || bytes.Contains(sessions, []byte("d.runtime === 'codex'")) {
		t.Error("session resume behavior must not branch on a runtime name")
	}
	for _, required := range []string{"task/task-projection-store.js", "task/task-api.js", "createRuntimeTask", "taskProjectionStore.subscribe(drain)", "catalog_session_id: chatState.origin?.harvestId"} {
		if !bytes.Contains(chat, []byte(required)) {
			t.Errorf("chat stream seam lost %q", required)
		}
	}
	for _, required := range []string{"'/api/v1/runtime-tasks'", "'Idempotency-Key'", "'Content-Type': 'application/json'", "createRuntimeTask", "interruptRuntimeTask"} {
		if !bytes.Contains(taskAPI, []byte(required)) {
			t.Errorf("task API seam lost %q", required)
		}
	}
	for _, forbidden := range []string{"res.body.getReader()", "new TextDecoder()", "fetch('/api/chat'", "aborter?.abort()"} {
		if bytes.Contains(chat, []byte(forbidden)) {
			t.Errorf("chat view still owns stream framing %q", forbidden)
		}
	}
	for _, required := range []string{"isVendorTurnEvidence", "VENDOR_TURN_EVENT_TYPES"} {
		if !bytes.Contains(chat, []byte(required)) && !bytes.Contains(taskSemantics, []byte(required)) {
			t.Errorf("vendor turn accounting seam lost %q", required)
		}
	}
	for _, source := range [][]byte{chat, sessions, taskSemantics} {
		if !bytes.Contains(source, []byte("hasRenderableText")) {
			t.Error("live, historical, and semantic owners must share the renderable-text rule")
		}
	}
	for _, forbidden := range []string{"!['input_error', 'auth_required', 'done'].includes(ev.type)"} {
		if bytes.Contains(chat, []byte(forbidden)) {
			t.Errorf("daemon lifecycle events can again count as vendor turns: %q", forbidden)
		}
	}
	state := read("js/state.js")
	app := read("js/app.js")
	if bytes.Contains(state, []byte("chatAbort")) || bytes.Contains(chat, []byte("S.chatAbort")) {
		t.Error("task interruption must not use cross-view global state")
	}
	for _, required := range []string{"cg:task-active", "root: wrap", "activeTask?.root?.isConnected && activeTask.interrupt()", "!running || !activeTaskID"} {
		if !bytes.Contains(app, []byte(required)) && !bytes.Contains(chat, []byte(required)) {
			t.Errorf("active-task interrupt seam lost %q", required)
		}
	}
	for _, required := range []string{"EventStreamClient", "eventStreamClient.start()", "task-projection-store.js"} {
		if !bytes.Contains(app, []byte(required)) {
			t.Errorf("app task bootstrap lost %q", required)
		}
	}
	for _, forbidden := range []string{"/api/runtime-tasks/stream", "readSSE("} {
		if bytes.Contains(app, []byte(forbidden)) {
			t.Errorf("app regained task transport logic %q", forbidden)
		}
	}
	if bytes.Contains(sessions, []byte(`from "./chat.js"`)) {
		t.Error("sessions view must not import chat view; that recreates the ESM cycle")
	}
	// The task→session identity join moved to Go with the status decider
	// (session-status signal plan, 2026-09-01); the view's seam is now the
	// daemon's per-session activity item rendered through the one renderer.
	for _, required := range []string{"renderSessionStatus(sessionActivityStore.activity("} {
		if !bytes.Contains(sessions, []byte(required)) {
			t.Errorf("session status seam lost %q", required)
		}
	}
	if bytes.Contains(sessions, []byte("visibleForSession(")) {
		t.Error("session view regained a browser-side task identity join; that ranking belongs to the daemon")
	}
	for _, required := range []string{"cg:mount-chat", "renderChat(d.container", "d.log.isConnected"} {
		if !bytes.Contains(app, []byte(required)) && !bytes.Contains(sessions, []byte(required)) {
			t.Errorf("session/chat composition seam lost %q", required)
		}
	}
	css := read("css/app.css")
	for _, required := range []string{".projhead .projnew", ".projhead .projtoggle", ".projhead .projmode", ".session-pager", ".selected-outside-label", ".sess:focus-visible", ".auth-gate", ".session-foot::after", "top: 100%; height: 20px", "background: var(--bg); pointer-events: none", ".taskfeedstatus"} {
		if !bytes.Contains(css, []byte(required)) {
			t.Errorf("console styling lost %q", required)
		}
	}
}
