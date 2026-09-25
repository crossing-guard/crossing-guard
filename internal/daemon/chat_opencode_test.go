package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"crossing-guard/internal/taskinput"
)

func TestOpenCodeMapsFilesInOrderAndGuardsVisionModelMode(t *testing.T) {
	inputs := []taskinput.ResolvedInput{
		{Input: taskinput.Input{Kind: taskinput.KindText}, Path: "/private/opaque/one.txt"},
		{Input: taskinput.Input{Kind: taskinput.KindImage}, Path: "/private/opaque/two.png"},
	}
	driver := openCodeChatDriver{}
	if err := driver.ValidateChatInputs(ChatRequest{Model: "ollama/qwen2.5-coder:7b"}, inputs); err == nil {
		t.Fatal("text-only model silently accepted image")
	}
	req := ChatRequest{Binary: testChatExecutable(t), Cwd: t.TempDir(), Prompt: "inspect",
		Model: "ollama/qwen2.5vl:3b", Mode: "vision-proof"}
	if err := driver.ValidateChatInputs(req, inputs); err != nil {
		t.Fatal(err)
	}
	cmd, err := driver.BuildCmd(req, ChatLaunchContext{Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	args := cmd.Args
	first := slices.Index(args, "/private/opaque/one.txt")
	second := slices.Index(args, "/private/opaque/two.png")
	// --file is variadic in OpenCode 1.18: the prompt survives only behind "--".
	if first < 1 || second != first+2 || args[len(args)-2] != "--" || args[len(args)-1] != "inspect" ||
		second != len(args)-3 || !slices.Contains(args, "vision-proof") {
		t.Fatalf("OpenCode attachment mapping changed: %q", args)
	}
	if env := cmd.Env; len(env) == 0 || env[len(env)-1] != "OPENCODE_DISABLE_PROJECT_CONFIG=1" {
		t.Fatal("the vision-proof run is not pinned to global configuration")
	}
}

func TestOpenCodeKeepsExtraArgsBeforeThePromptBoundary(t *testing.T) {
	req := ChatRequest{Binary: testChatExecutable(t), Cwd: t.TempDir(), Prompt: "-looks like a flag",
		ExtraArgs: "--variant high"}
	inputs := []taskinput.ResolvedInput{{Input: taskinput.Input{Kind: taskinput.KindText}, Path: "/private/opaque/one.txt"}}
	cmd, err := openCodeChatDriver{}.BuildCmd(req, ChatLaunchContext{Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	tail := cmd.Args[len(cmd.Args)-6:]
	if !slices.Equal(tail, []string{"--file", "/private/opaque/one.txt", "--variant", "high", "--", "-looks like a flag"}) {
		t.Fatalf("argv tail = %q", tail)
	}
	if slices.Contains(cmd.Env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") && os.Getenv("OPENCODE_DISABLE_PROJECT_CONFIG") != "1" {
		t.Fatal("a default-mode run was pinned to global configuration")
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

func TestOpenCodeRefusesExtraArgsThatOwnTheAgentOrPrompt(t *testing.T) {
	bin, log := fakeOpenCode(t, agentListing("vision-proof (primary)"))
	driver := openCodeChatDriver{}
	for _, extra := range []string{"--no-agent", "--agent.x=y", "--command review", "--attach http://127.0.0.1:4096", "--agent build", "--pure"} {
		if _, err := driver.CanonicalizeChatRequest(ChatRequest{Binary: bin, Mode: "vision-proof", ExtraArgs: extra}); err == nil ||
			!strings.Contains(err.Error(), "mode") {
			t.Fatalf("vision-proof admitted extra args %q: %v", extra, err)
		}
	}
	if _, err := driver.CanonicalizeChatRequest(ChatRequest{Binary: bin, ExtraArgs: "--variant high -- trailing"}); err == nil {
		t.Fatal("a bare -- in extra args was admitted")
	}
	if _, err := driver.CanonicalizeChatRequest(ChatRequest{Binary: bin, ExtraArgs: "--agent plan"}); err != nil {
		t.Fatalf("default mode lost its agent passthrough: %v", err)
	}
	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("the agent check ran for a refused or default-mode request: %q", calls)
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

// The auto-approve switch has three spellings in 1.18 (two hidden) and yargs
// accepts each camel-cased, negated, dotted, `=` and repeated, so only the
// allowlisted shapes may reach argv. Refusals happen before any process runs.
func TestOpenCodeExtraArgsAreAllowlisted(t *testing.T) {
	bin, log := fakeOpenCode(t, "")
	driver := openCodeChatDriver{}
	for _, extra := range []string{"", "--agent plan", "--agent=plan", "--variant high", "--variant=high",
		"--title x", "--title=--auto", "--thinking", "--agent plan --variant=high --title x --thinking"} {
		req := ChatRequest{Binary: bin, ExtraArgs: extra}
		got, err := driver.CanonicalizeChatRequest(req)
		if err != nil || !reflect.DeepEqual(got, req) {
			t.Fatalf("extra args %q: request changed or refused: %v", extra, err)
		}
	}
	refused := map[string]string{
		"--auto": "not accepted", "--auto=true": "not accepted", "--yolo": "not accepted",
		"--dangerously-skip-permissions": "not accepted", "--dangerouslySkipPermissions": "not accepted",
		"--dangerously-skip-permissions=true": "not accepted", "--no-auto": "not accepted",
		"-i": "not accepted", "-ic": "not accepted", "-h": "not accepted", "--help": "not accepted",
		"--pure": "not accepted", "--log-level DEBUG": "not accepted", "--share": "not accepted",
		"--attach http://x": "not accepted", "--port 1": "not accepted",
		"--dir /tmp": "not accepted", "--format default": "not accepted", "--model a/b": "not accepted",
		"-m a/b": "not accepted", "--session s": "not accepted", "-c": "not accepted",
		"--command x": "not accepted", "--file f": "not accepted",
		"--": `bare "--"`, "hello": "not accepted", "--agent plan hello": "argument 3 is not accepted",
		"--agent": "need a value", "--agent --auto": "need a value", "--agent --": "need a value",
		"--agent=":            "need a value",
		"--agent a --agent b": "argument 3 repeats", "--agent=a --agent b": "argument 2 repeats",
		"--thinking=true": "not accepted", "--no-thinking": "not accepted",
		"--agent.x=y": "not accepted", "--Agent plan": "not accepted",
		"--title=a\x00b":                          "control characters",
		"--title=\x1b[31mred":                     "control characters",
		"--title " + strings.Repeat("x", 201):     "too long",
		"--variant=" + string([]byte{0xff, 0xfe}): "not UTF-8",
		"--thinking --thinking":                   "argument 2 repeats",
		"--title -x":                              "need a value",
	}
	for extra, want := range refused {
		_, err := driver.CanonicalizeChatRequest(ChatRequest{Binary: bin, ExtraArgs: extra})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("extra args %q: want refusal containing %q, got %v", extra, want, err)
		}
		msg := err.Error()
		for _, routed := range []string{"mode", "task input", "working directory"} {
			if strings.Contains(msg, routed) {
				t.Fatalf("extra args %q: refusal %q would route to a console field (%q)", extra, msg, routed)
			}
		}
		if want != `bare "--"` && !strings.Contains(msg, "Settings") {
			t.Fatalf("extra args %q: refusal %q does not say where the value lives", extra, msg)
		}
		if _, buildErr := driver.BuildCmd(ChatRequest{Binary: bin, Cwd: t.TempDir(), Prompt: "p", ExtraArgs: extra}, ChatLaunchContext{}); buildErr == nil ||
			buildErr.Error() != msg {
			t.Fatalf("BuildCmd did not refuse extra args %q with the admission refusal: %v", extra, buildErr)
		}
	}
	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("a default-mode extra-args check ran the binary: %q", calls)
	}
}

func TestOpenCodeExtraArgsLandBetweenFilesAndThePrompt(t *testing.T) {
	inputs := []taskinput.ResolvedInput{
		{Input: taskinput.Input{Kind: taskinput.KindText}, Path: "/private/opaque/one.txt"},
		{Input: taskinput.Input{Kind: taskinput.KindText}, Path: "/private/opaque/two.txt"},
	}
	req := ChatRequest{Binary: testChatExecutable(t), Cwd: t.TempDir(), Prompt: "p", ExtraArgs: "--agent plan --thinking --title=--auto"}
	cmd, err := openCodeChatDriver{}.BuildCmd(req, ChatLaunchContext{Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	tail := cmd.Args[len(cmd.Args)-10:]
	want := []string{"--file", "/private/opaque/one.txt", "--file", "/private/opaque/two.txt", "--agent", "plan", "--thinking", "--title=--auto", "--", "p"}
	if !slices.Equal(tail, want) {
		t.Fatalf("argv tail = %q", tail)
	}
}

// BuildCmd enforces the mode rules itself, so a direct caller cannot put a
// second --agent beside the mode's own (OpenCode would fall back to build).
func TestOpenCodeBuildCmdRefusesExtraArgsInAgentModes(t *testing.T) {
	driver := openCodeChatDriver{}
	for _, extra := range []string{"--agent x", "--thinking"} {
		req := ChatRequest{Binary: testChatExecutable(t), Cwd: t.TempDir(), Prompt: "p", Mode: "vision-proof", ExtraArgs: extra}
		if _, err := driver.BuildCmd(req, ChatLaunchContext{}); err == nil || !strings.Contains(err.Error(), "vision-proof mode") {
			t.Fatalf("BuildCmd admitted %q in vision-proof: %v", extra, err)
		}
	}
	if _, err := openCodeExtraArgs(ChatRequest{Mode: "future-agent", ExtraArgs: "--agent x"}); err == nil ||
		!strings.Contains(err.Error(), "when a mode selects the agent") {
		t.Fatalf("--agent admitted beside a mode's own: %v", err)
	}
	if _, err := openCodeExtraArgs(ChatRequest{Mode: "future-agent", ExtraArgs: "--thinking"}); err != nil {
		t.Fatalf("a non-agent option was refused in an agent mode: %v", err)
	}
}

func TestOpenCodeDefaultModeDoesNotClaimReadOnly(t *testing.T) {
	mode := openCodeChatDriver{}.ChatCapability().Modes[0]
	if mode.ID != "" || mode.Label != "Default" || !strings.Contains(mode.Description, "never auto-approved") {
		t.Fatalf("default mode = %+v", mode)
	}
}
