package daemon

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"crossing-guard/internal/guardcli"
)

func TestChatCapabilitiesUseRegisteredDrivers(t *testing.T) {
	caps, err := chatCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(caps))
	for _, capability := range caps {
		names = append(names, capability.Runtime)
		if _, ok := chatDrivers[capability.Runtime]; !ok {
			t.Fatalf("advertised runtime %q is not dispatchable", capability.Runtime)
		}
	}
	sorted := append([]string(nil), names...)
	slices.Sort(sorted)
	if !reflect.DeepEqual(names, sorted) {
		t.Fatalf("capability runtimes are not stable-sorted: %v", names)
	}
}

type futureChatDriver struct{}

func (futureChatDriver) BuildCmd(ChatRequest, ChatLaunchContext) (*exec.Cmd, error) {
	return nil, errors.New("fixture")
}
func (futureChatDriver) ProjectEvent(map[string]any) []ChatEvent { return nil }
func (futureChatDriver) ChatCapability() ChatCapability {
	return ChatCapability{Runtime: "future", DisplayName: "Future", CanStart: true,
		Modes: []ChatMode{{Label: "Safe", Risk: "normal"}}}
}

func TestNewChatAdapterNeedsNoGenericRegistrationEdit(t *testing.T) {
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{}
	t.Cleanup(func() { chatDrivers = original })
	registerChatDriver("future", futureChatDriver{})
	caps, err := chatCapabilities()
	if err != nil || len(caps) != 1 || caps[0].Runtime != "future" {
		t.Fatalf("additive adapter was not discovered: caps=%+v err=%v", caps, err)
	}
}

func TestHandleChatCapabilities(t *testing.T) {
	rec := httptest.NewRecorder()
	handleChatCapabilities(rec, httptest.NewRequest("GET", "/api/chat/capabilities", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var caps []ChatCapability
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil || len(caps) != 3 {
		t.Fatalf("capabilities=%+v err=%v", caps, err)
	}
	for _, capability := range caps {
		if capability.Runtime == "opencode" && capability.CanSignIn {
			t.Fatal("OpenCode must not advertise Crossing Guard credential brokerage")
		}
	}
}

// TestOpenCodeCapabilityCarriesGovernanceLane pins the Slice C visibility
// contract: the version-gate verdict read from the installed artifact reaches
// the Settings projection verbatim, and an unattached runtime claims no lane.
func TestOpenCodeCapabilityCarriesGovernanceLane(t *testing.T) {
	original := openCodeGovernanceStatus
	t.Cleanup(func() { openCodeGovernanceStatus = original })

	openCodeGovernanceStatus = func() guardcli.OpenCodeGovernanceStatus {
		return guardcli.OpenCodeGovernanceStatus{Attached: true, Lane: "decision",
			Installed: "1.18.0", Interface: "1.18.0"}
	}
	capability := openCodeChatDriver{}.ChatCapability()
	if capability.GovernanceLane != "decision" || capability.GovernanceNote != "" {
		t.Fatalf("decision lane not published: %+v", capability)
	}

	openCodeGovernanceStatus = func() guardcli.OpenCodeGovernanceStatus {
		return guardcli.OpenCodeGovernanceStatus{Attached: true, Lane: "collection-only",
			Installed: "9.9.9", Interface: "1.18.0",
			Note: "installed OpenCode CLI 9.9.9 does not match the verified interface 1.18.0"}
	}
	capability = openCodeChatDriver{}.ChatCapability()
	if capability.GovernanceLane != "collection-only" || !strings.Contains(capability.GovernanceNote, "does not match") {
		t.Fatalf("collection-only fallback not visible: %+v", capability)
	}

	openCodeGovernanceStatus = func() guardcli.OpenCodeGovernanceStatus {
		return guardcli.OpenCodeGovernanceStatus{Interface: "1.18.0"}
	}
	capability = openCodeChatDriver{}.ChatCapability()
	if capability.GovernanceLane != "" || capability.GovernanceNote != "" {
		t.Fatalf("unattached runtime must claim no governance lane: %+v", capability)
	}
}

func TestOpenCodeAuthStatusKeepsCredentialsExternal(t *testing.T) {
	rec := httptest.NewRecorder()
	handleVendorAuthStatus(rec, httptest.NewRequest("GET", "/api/chat/auth?runtime=opencode", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"can_sign_in":false`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestValidateChatCapabilityRejectsBadMetadata(t *testing.T) {
	base := ChatCapability{Runtime: "fixture", DisplayName: "Fixture", Modes: []ChatMode{{Label: "Default", Risk: "normal"}}}
	if err := validateChatCapability("fixture", base); err != nil {
		t.Fatal(err)
	}
	bad := base
	bad.Modes = []ChatMode{{Label: "Default", Risk: "normal"}, {Label: "Again", Risk: "normal"}}
	if err := validateChatCapability("fixture", bad); err == nil {
		t.Fatal("duplicate default mode accepted")
	}
	bad = base
	bad.Runtime = "other"
	if err := validateChatCapability("fixture", bad); err == nil {
		t.Fatal("mismatched runtime accepted")
	}
}

func TestChatAdapterCommands(t *testing.T) {
	bin := testChatExecutable(t)
	cwd := t.TempDir()
	tests := []struct {
		name   string
		driver ChatDriver
		req    ChatRequest
		want   []string
		not    []string
	}{
		{name: "claude canonical mode", driver: claudeChatDriver{}, req: ChatRequest{Binary: bin, Cwd: cwd, Prompt: "hello", Mode: "plan", Model: "sonnet"}, want: []string{"--permission-mode", "plan", "--model", "sonnet", "--permission-prompt-tool", "--mcp-config"}},
		{name: "codex local provider", driver: codexChatDriver{}, req: ChatRequest{Binary: bin, Cwd: cwd, Prompt: "hello", Mode: "workspace-write", Model: "local:ollama"}, want: []string{"--sandbox", "workspace-write", "--oss", "--local-provider", "ollama"}, not: []string{"-m", "local:ollama", "--permission-prompt-tool", "--mcp-config"}},
		{name: "opencode safe resume", driver: openCodeChatDriver{}, req: ChatRequest{Binary: bin, Cwd: cwd, Prompt: "hello", SessionID: "ses_exact", Model: "ollama/qwen2.5-coder:7b"}, want: []string{"run", "--format", "json", "--dir", cwd, "--session", "ses_exact", "--model", "ollama/qwen2.5-coder:7b"}, not: []string{"--auto", "--permission-prompt-tool", "--mcp-config"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := tt.driver.BuildCmd(tt.req, ChatLaunchContext{TaskID: "task_fixture", DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(cmd.Args[1:], "\x00")
			for _, value := range tt.want {
				if !strings.Contains(joined, value) {
					t.Errorf("args %q missing %q", cmd.Args, value)
				}
			}
			for _, value := range tt.not {
				if strings.Contains(joined, value) {
					t.Errorf("args %q unexpectedly contain %q", cmd.Args, value)
				}
			}
		})
	}
}

func TestOpenCodeRejectsUnverifiedAutoMode(t *testing.T) {
	if err := validateChatMode(openCodeChatDriver{}, "auto"); err == nil {
		t.Fatal("unverified OpenCode auto-approval mode was accepted")
	}
}

func TestOpenCodeEventProjection(t *testing.T) {
	driver := openCodeChatDriver{}
	events := append(driver.ProjectEvent(map[string]any{"type": "step_start", "sessionID": "ses_exact",
		"part": map[string]any{"type": "step-start"}}),
		driver.ProjectEvent(map[string]any{"type": "text", "part": map[string]any{"text": "Hello"}})...)
	events = append(events, driver.ProjectEvent(map[string]any{"type": "step_finish", "part": map[string]any{
		"cost": float64(0), "tokens": map[string]any{"total": float64(10)}}})...)
	encoded, _ := json.Marshal(events)
	body := string(encoded)
	for _, expected := range []string{`"type":"session"`, `"id":"ses_exact"`, `"type":"text"`, `"text":"Hello"`, `"type":"result"`} {
		if !strings.Contains(body, expected) {
			t.Errorf("projection %q missing %q", body, expected)
		}
	}
}

func TestOpenCodeUnknownEventIsVisible(t *testing.T) {
	events := openCodeChatDriver{}.ProjectEvent(map[string]any{"type": "future_event", "secret": "bounded"})
	encoded, _ := json.Marshal(events)
	if body := string(encoded); !strings.Contains(body, `"type":"stderr"`) || !strings.Contains(body, "Unrecognized OpenCode event") {
		t.Fatalf("unknown event was silently lost: %s", body)
	}
}

func testChatExecutable(t *testing.T) string {
	t.Helper()
	name := "chat-fixture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
