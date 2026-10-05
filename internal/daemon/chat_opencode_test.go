package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"crossing-guard/internal/taskinput"
)

func TestOpenCodeMapsFilesInOrderAndGuardsVisionModelMode(t *testing.T) {
	inputs := []taskinput.ResolvedInput{
		{Input: taskinput.Input{Kind: taskinput.KindText, Name: "one.txt", MediaType: "text/plain"}, Path: "/private/opaque/one.txt"},
		{Input: taskinput.Input{Kind: taskinput.KindImage, Name: "two.png", MediaType: "image/png"}, Path: "/private/opaque/two.png"},
	}
	driver := openCodeChatDriver{}
	if err := driver.ValidateChatInputs(ChatRequest{Model: "ollama/qwen2.5-coder:7b"}, inputs); err == nil {
		t.Fatal("text-only model silently accepted image")
	}
	req := ChatRequest{Cwd: t.TempDir(), Prompt: "inspect", Model: "ollama/qwen2.5vl:3b", Mode: openCodeVisionMode}
	if err := driver.ValidateChatInputs(req, inputs); err != nil {
		t.Fatal(err)
	}
	protocol := &openCodeServerProtocol{request: req, launch: ChatLaunchContext{Inputs: inputs}}
	body := protocol.promptBody()
	parts := body["parts"].([]map[string]any)
	if len(parts) != 3 || parts[0]["url"] != "file:///private/opaque/one.txt" || parts[1]["url"] != "file:///private/opaque/two.png" || parts[2]["text"] != "inspect" || body["agent"] != openCodeVisionMode {
		t.Fatalf("attachment mapping changed: %#v", body)
	}
	if got := body["model"]; !reflect.DeepEqual(got, map[string]string{"providerID": "ollama", "modelID": "qwen2.5vl:3b"}) {
		t.Fatalf("model = %#v", got)
	}
}

// fakeOpenCode writes an executable shell script standing in for the vendor CLI.
// Every invocation appends its arguments, working directory and project-config
// switch to calls.log beside it.
func fakeOpenCode(t *testing.T, body string) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fakes need a POSIX shell")
	}
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "opencode"), filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		`echo "$* | $(pwd -P) | ${OPENCODE_DISABLE_PROJECT_CONFIG:-unset}" >> '` + log + "'\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func readCalls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// agentListing is the measured 1.18 shape: a header at column 0, then permission
// JSON whose first line is indented and whose closing bracket is not.
func agentListing(headers ...string) string {
	var b strings.Builder
	for _, header := range headers {
		b.WriteString("printf '%s\\n' '" + header + "' '  [' '  {' '    \"permission\": \"*\",' '    \"pattern\": \"vision-proof (primary)\"' '  }' ']'\n")
	}
	return b.String()
}

func TestOpenCodeVisionModeRequiresAPrimaryAgent(t *testing.T) {
	for _, tc := range []struct {
		name, body, refusal string
	}{
		{"primary", agentListing("build (primary)", "vision-proof (primary)"), ""},
		{"all", agentListing("build (primary)", "vision-proof (all)"), ""},
		{"crlf", "printf 'build (primary)\\r\\nvision-proof (primary)\\r\\n'", ""},
		{"subagent", agentListing("build (primary)", "vision-proof (subagent)"), "defined as a subagent"},
		{"absent", agentListing("build (primary)", "plan (primary)"), "it is not defined"},
		{"forged duplicate", agentListing("vision-proof (primary)", "vision-proof (subagent)"), "more than once"},
		{"only in JSON and stderr", agentListing("build (primary)") + "echo 'vision-proof (primary)' >&2", "it is not defined"},
		{"exit status", agentListing("vision-proof (primary)") + "exit 3", "exited with an error"},
		{"empty", "exit 0", "no agents listed"},
		{"prefix is not a name", agentListing("vision-proof-x (primary)", "x-vision-proof (primary)"), "it is not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, log := fakeOpenCode(t, tc.body)
			_, err := openCodeChatDriver{}.CanonicalizeChatRequest(ChatRequest{Binary: bin, Mode: "vision-proof"})
			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("refused a usable agent: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.refusal) || !strings.Contains(err.Error(), "mode") {
				t.Fatalf("err = %v, want a mode refusal containing %q", err, tc.refusal)
			}
			calls := readCalls(t, log)
			if len(calls) != 1 {
				t.Fatalf("calls = %q", calls)
			}
			fields := strings.Split(calls[0], " | ")
			neutral, _ := filepath.EvalSymlinks(os.TempDir())
			if fields[0] != "agent list" || filepath.Clean(fields[1]) != filepath.Clean(neutral) || fields[2] != "1" {
				t.Fatalf("agent check ran as %q, want `agent list` in %s with project config disabled", calls[0], neutral)
			}
		})
	}
}

func TestOpenCodeAgentCheckIsBounded(t *testing.T) {
	previousTimeout, previousDelay, previousMax := openCodeAgentListTimeout, openCodeAgentListWaitDelay, openCodeAgentListMaxBytes
	t.Cleanup(func() {
		openCodeAgentListTimeout, openCodeAgentListWaitDelay, openCodeAgentListMaxBytes = previousTimeout, previousDelay, previousMax
	})
	openCodeAgentListTimeout, openCodeAgentListWaitDelay, openCodeAgentListMaxBytes = 2*time.Second, 200*time.Millisecond, 64

	bin, _ := fakeOpenCode(t, agentListing("vision-proof (primary)", "build (primary)", "plan (primary)"))
	if _, err := (openCodeChatDriver{}).CanonicalizeChatRequest(ChatRequest{Binary: bin, Mode: "vision-proof"}); err == nil ||
		!strings.Contains(err.Error(), "exceeded its bound") {
		t.Fatalf("oversized list: err = %v", err)
	}

	// An endless writer is stopped at the cap, not at the deadline.
	bin, _ = fakeOpenCode(t, "while :; do echo 'build (primary)'; done")
	started := time.Now()
	if _, err := (openCodeChatDriver{}).CanonicalizeChatRequest(ChatRequest{Binary: bin, Mode: "vision-proof"}); err == nil ||
		!strings.Contains(err.Error(), "exceeded its bound") {
		t.Fatalf("endless list: err = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= openCodeAgentListTimeout {
		t.Fatalf("endless list ran %s, until the deadline instead of the cap", elapsed)
	}

	// A background child keeps stdout open after the parent is killed; the
	// process group and WaitDelay must still end the check.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	bin, _ = fakeOpenCode(t, "sleep 60 &\necho $! > '"+pidFile+"'\nsleep 60")
	started = time.Now()
	_, err := openCodeChatDriver{}.CanonicalizeChatRequest(ChatRequest{Binary: bin, Mode: "vision-proof"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("hung list: err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Fatalf("hung list took %s to refuse", elapsed)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		child, _ := os.FindProcess(pid)
		if child.Signal(syscall.Signal(0)) != nil {
			break // the whole process group was killed, not only the direct child
		}
		if time.Now().After(deadline) {
			_ = child.Kill()
			t.Fatal("the agent check left its child process running")
		}
	}
}

func TestOpenCodeRefusesAllExtraArgsBeforeSpawn(t *testing.T) {
	bin, log := fakeOpenCode(t, agentListing("vision-proof (primary)"))
	driver := openCodeChatDriver{}
	for _, extra := range []string{"--variant high", "--agent plan", "--thinking", "--auto", "--attach http://127.0.0.1:4096", "--pure"} {
		req := ChatRequest{Binary: bin, Cwd: t.TempDir(), Prompt: "p", ExtraArgs: extra}
		_, err := driver.CanonicalizeChatRequest(req)
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("extra args %q: %v", extra, err)
		}
		if _, buildErr := driver.BuildCmd(req, ChatLaunchContext{}); buildErr == nil || buildErr.Error() != err.Error() {
			t.Fatalf("BuildCmd did not preserve refusal for %q: %v", extra, buildErr)
		}
	}
	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("a refused request ran OpenCode: %q", calls)
	}
}

func TestHandleChatRefusesVisionModeWithoutTheAgentBeforeSpawn(t *testing.T) {
	service := installTestRuntimeTasks(t)
	bin, log := fakeOpenCode(t, `[ "$1" = agent ] && { `+agentListing("build (primary)")+` exit 0; }`)
	body := `{"runtime":"opencode","prompt":"describe","mode":"vision-proof","cwd":` + jsonString(t, t.TempDir()) + `,"binary":` + jsonString(t, bin) + `}`
	rec := httptest.NewRecorder()
	handleChat(rec, httptest.NewRequest("POST", "/api/chat", strings.NewReader(body)))
	if got := rec.Body.String(); !strings.Contains(got, `"type":"input_error"`) || !strings.Contains(got, `"field":"mode"`) ||
		!strings.Contains(got, "not defined") {
		t.Fatalf("missing structured mode refusal: %s", got)
	}
	if calls := readCalls(t, log); len(calls) != 1 || !strings.HasPrefix(calls[0], "agent list") {
		t.Fatalf("OpenCode was invoked beyond the agent check: %q", calls)
	}
	if tasks, err := service.List("opencode", "", 10); err != nil || len(tasks) != 0 {
		t.Fatalf("a refused request created tasks: %v %v", tasks, err)
	}
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestOpenCodeDefaultModeDoesNotClaimReadOnly(t *testing.T) {
	mode := openCodeChatDriver{}.ChatCapability().Modes[0]
	if mode.ID != "" || mode.Label != "Default" || !strings.Contains(mode.Description, "approvals inbox") {
		t.Fatalf("default mode = %+v", mode)
	}
}

// Events built from a nested part carry that part's id as their anchor, the
// identity the stored transcript carries too. An event with no nested part
// (the old flat tool_result shape) carries none: a guessed anchor could hide
// a row the console never drew.
func TestOpenCodeLiveEventsAnchorOnTheirPartID(t *testing.T) {
	driver := openCodeChatDriver{}
	anchorOf := func(events []ChatEvent, kind string) (string, bool) {
		for _, event := range events {
			if event["type"] == kind {
				anchor, ok := event["anchor"].(string)
				return anchor, ok
			}
		}
		t.Fatalf("no %s event in %+v", kind, events)
		return "", false
	}
	text := driver.ProjectEvent(map[string]any{"type": "text", "sessionID": "ses_x",
		"part": map[string]any{"id": "prt_text", "type": "text", "text": "hello"}})
	if anchor, _ := anchorOf(text, "text"); anchor != "prt_text" {
		t.Fatalf("text anchor = %q", anchor)
	}
	thinking := driver.ProjectEvent(map[string]any{"type": "reasoning",
		"part": map[string]any{"id": "prt_think", "type": "reasoning", "text": "hm"}})
	if anchor, _ := anchorOf(thinking, "thinking"); anchor != "prt_think" {
		t.Fatalf("thinking anchor = %q", anchor)
	}
	tool := driver.ProjectEvent(map[string]any{"type": "tool_use", "part": map[string]any{"id": "prt_tool", "type": "tool",
		"tool": "bash", "state": map[string]any{"status": "completed", "input": map[string]any{"command": "ls"}, "output": "a.txt"}}})
	call, _ := anchorOf(tool, "tool")
	result, _ := anchorOf(tool, "tool_result")
	if call != "prt_tool" || result != "prt_tool" {
		t.Fatalf("a tool call and its result share the part id: call=%q result=%q", call, result)
	}
	flat := driver.ProjectEvent(map[string]any{"type": "tool_result", "id": "not-a-part", "text": "out"})
	if _, ok := anchorOf(flat, "tool_result"); ok {
		t.Fatalf("an event with no nested part must carry no anchor: %+v", flat)
	}
	bare := driver.ProjectEvent(map[string]any{"type": "text", "id": "evt_1", "text": "flat"})
	if _, ok := anchorOf(bare, "text"); ok {
		t.Fatalf("a flat text event must carry no anchor: %+v", bare)
	}
}
