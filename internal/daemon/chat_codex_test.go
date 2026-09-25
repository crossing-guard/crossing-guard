package daemon

import (
	"os"
	"slices"
	"strings"
	"testing"

	"crossing-guard/internal/taskinput"
)

func TestCodexMapsOrderedPreparedInputsWithoutSilentModelFallback(t *testing.T) {
	bin := testChatExecutable(t)
	textPath := t.TempDir() + "/input.txt"
	imagePath := t.TempDir() + "/input.png"
	if err := os.WriteFile(textPath, []byte("TEXT-MARKER-7319"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("normalized"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs := []taskinput.ResolvedInput{
		{Input: taskinput.Input{Kind: taskinput.KindText}, Path: textPath},
		{Input: taskinput.Input{Kind: taskinput.KindImage}, Path: imagePath},
	}
	driver := codexChatDriver{}
	if err := driver.ValidateChatInputs(ChatRequest{}, inputs); err != nil {
		t.Fatal(err)
	}
	if err := driver.ValidateChatInputs(ChatRequest{Model: "custom"}, inputs); err == nil {
		t.Fatal("custom model silently accepted image")
	}
	secondImage := t.TempDir() + "/second.png"
	if err := os.WriteFile(secondImage, []byte("normalized"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, taskinput.ResolvedInput{Input: taskinput.Input{Kind: taskinput.KindImage}, Path: secondImage})
	for _, sessionID := range []string{"", "019f701f-0000-7000-8000-000000000001"} {
		cmd, err := driver.BuildCmd(ChatRequest{Binary: bin, Cwd: t.TempDir(), Prompt: "inspect", SessionID: sessionID}, ChatLaunchContext{Inputs: inputs})
		if err != nil {
			t.Fatal(err)
		}
		args := cmd.Args
		boundary := slices.Index(args, "--")
		want := len(args) - 2
		if sessionID != "" {
			want = len(args) - 3
			if args[len(args)-2] != sessionID {
				t.Fatalf("session id is not between the boundary and the prompt: %q", args)
			}
		}
		if boundary != want || !strings.Contains(args[len(args)-1], "TEXT-MARKER-7319") {
			t.Fatalf("prompt is not the last positional after the boundary: %q", args)
		}
		var images []string
		for index, value := range args {
			if value == "--image" {
				if index > boundary {
					t.Fatalf("image flag after the boundary: %q", args)
				}
				images = append(images, args[index+1])
			}
		}
		if !slices.Equal(images, []string{imagePath, secondImage}) {
			t.Fatalf("Codex attachment order is unsafe: %q", args)
		}
	}
}

func TestCodexExtraArgsAllowlist(t *testing.T) {
	driver := codexChatDriver{}
	for _, extra := range []string{
		"", "--skip-git-repo-check", "-c model_reasoning_effort=high",
		"--config model_reasoning_effort=low", "--config=model_reasoning_effort=xhigh",
		"--skip-git-repo-check -c model_reasoning_effort=medium",
	} {
		req := ChatRequest{Runtime: "codex", Prompt: "hi", ExtraArgs: extra}
		got, err := driver.CanonicalizeChatRequest(req)
		if err != nil || got.ExtraArgs != extra || got.Prompt != "hi" {
			t.Fatalf("%q: accepted request changed or refused: %+v %v", extra, got, err)
		}
	}
	const (
		notAccepted = "is not accepted"
		repeats     = "repeats one"
		needsValue  = "need a value after"
		badEffort   = "reasoning-effort key"
	)
	cases := map[string]string{
		"--yolo": notAccepted, "--dangerously-bypass-approvals-and-sandbox": notAccepted,
		"--approve-for-me": notAccepted, "--not-so-yolo": notAccepted, "--full-auto": notAccepted,

		"-c sandbox_mode=danger-full-access": badEffort, "-c approval_policy=never": badEffort,
		"--config=sandbox_mode=x": badEffort, "-c profiles.p.model_reasoning_effort=high": badEffort,
		`-c model_reasoning_effort="high"`: badEffort, `-c 'model_reasoning_effort=low'`: badEffort,
		"-c model_reasoning_effort=HIGH": badEffort, "-c model_reasoning_effort=": badEffort,
		"-c model_reasoning_effort=-x": badEffort, "-c model_reasoning_effort=true": badEffort,
		"-c model_reasoning_effort=false": badEffort, "-c model_reasoning_effort=inf": badEffort,
		"-c model_reasoning_effort=nan": badEffort, "-c model_reasoning_effort=abcdefghijklmnopq": badEffort,
		"-c --yolo": badEffort, "-c --": badEffort, "--config=": badEffort,
		"-cmodel_reasoning_effort=high": notAccepted, "-c=model_reasoning_effort=low": notAccepted,
		"model_reasoning_effort=low": notAccepted, "-c": needsValue, "--config": needsValue,

		"--sandbox danger-full-access": notAccepted, "-s": notAccepted, "-sdanger-full-access": notAccepted,
		"--sandbox=read-only": notAccepted, "-p x": notAccepted, "--profile=x": notAccepted,

		"--add-dir /tmp": notAccepted, "-C /tmp": notAccepted, "--cd=/tmp": notAccepted,
		"--worktree": notAccepted, "--ignore-user-config": notAccepted,
		"--dangerously-bypass-hook-trust": notAccepted, "--ignore-rules": notAccepted,
		"--enable x": notAccepted, "--disable x": notAccepted, "--strict-config": notAccepted,
		"--ephemeral": notAccepted, "-o f": notAccepted, "-o=f": notAccepted,
		"--output-last-message f": notAccepted, "--output-schema f": notAccepted,
		"-i f": notAccepted, "--image f": notAccepted,

		"--json": notAccepted, "--experimental-json": notAccepted, "-m x": notAccepted,
		"--oss": notAccepted, "--local-provider ollama": notAccepted, "--color never": notAccepted,
		"--thread-source x": notAccepted, "--last": notAccepted, "--all": notAccepted,
		"-h": notAccepted, "--help": notAccepted, "-V": notAccepted, "--version": notAccepted,

		"--": notAccepted, "hello": notAccepted,
		"--skip-git-repo-check=true": notAccepted, "--Skip-git-repo-check": notAccepted,

		"--skip-git-repo-check --skip-git-repo-check":                        repeats,
		"-c model_reasoning_effort=low -c model_reasoning_effort=low":        repeats,
		"-c model_reasoning_effort=low --config model_reasoning_effort=high": repeats,
		"--config=model_reasoning_effort=low -c model_reasoning_effort=high": repeats,
		"--skip-git-repo-check -c model_reasoning_effort=low --yolo":         notAccepted,
		"-c model_reasoning_effort=low -c":                                   repeats,
		"-c model_reasoning_effort=low -c sandbox_mode=x":                    repeats,
	}
	bin := testChatExecutable(t)
	for extra, want := range cases {
		_, err := driver.CanonicalizeChatRequest(ChatRequest{Runtime: "codex", Prompt: "hi", ExtraArgs: extra})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: want refusal containing %q, got %v", extra, want, err)
		}
		message := err.Error()
		if !strings.Contains(message, "Settings") {
			t.Fatalf("%q: refusal does not say where to fix it: %s", extra, message)
		}
		for _, routed := range []string{"mode", "task input", "working directory"} {
			if strings.Contains(message, routed) {
				t.Fatalf("%q: refusal would route to a console field (%q): %s", extra, routed, message)
			}
		}
		if _, buildErr := driver.BuildCmd(ChatRequest{Binary: bin, Cwd: t.TempDir(), Prompt: "hi", ExtraArgs: extra}, ChatLaunchContext{}); buildErr == nil || buildErr.Error() != message {
			t.Fatalf("%q: BuildCmd refusal %v differs from admission refusal %q", extra, buildErr, message)
		}
	}
}

func TestCodexExtraArgsRefusalNamesThePosition(t *testing.T) {
	const where = " (Settings → extra args for Codex, or the request's extra_args)"
	cases := map[string]string{
		"--skip-git-repo-check --yolo":                  "codex extra arguments accept only --skip-git-repo-check and one -c reasoning-effort override; argument 2 is not accepted" + where,
		"--skip-git-repo-check --skip-git-repo-check":   "codex extra arguments accept each option once; argument 2 repeats one" + where,
		"--skip-git-repo-check -c":                      "codex extra arguments need a value after argument 2" + where,
		"--skip-git-repo-check -c sandbox_mode=x":       "codex extra argument 3 must set only the reasoning-effort key to a short lowercase level" + where,
		"--skip-git-repo-check --config=sandbox_mode=x": "codex extra argument 2 must set only the reasoning-effort key to a short lowercase level" + where,
		"-c model_reasoning_effort=low --config=x=y":    "codex extra arguments accept each option once; argument 3 repeats one" + where,
	}
	for extra, want := range cases {
		if _, err := codexExtraArgs(extra); err == nil || err.Error() != want {
			t.Fatalf("%q: got %v, want %q", extra, err, want)
		}
	}
}

func TestCodexArgvKeepsPositionalsAfterBoundary(t *testing.T) {
	bin := testChatExecutable(t)
	driver := codexChatDriver{}
	const thread = "019f701f-0000-7000-8000-000000000001"
	cases := []struct {
		req  ChatRequest
		tail []string
	}{
		{ChatRequest{Prompt: "p", ExtraArgs: "--skip-git-repo-check -c model_reasoning_effort=medium"},
			[]string{"exec", "--sandbox", "read-only", "--json", "--skip-git-repo-check", "-c", "model_reasoning_effort=medium", "--", "p"}},
		{ChatRequest{Prompt: "p", ExtraArgs: "--config model_reasoning_effort=low"},
			[]string{"exec", "--sandbox", "read-only", "--json", "--config", "model_reasoning_effort=low", "--", "p"}},
		{ChatRequest{Prompt: "p", ExtraArgs: "--config=model_reasoning_effort=xhigh"},
			[]string{"exec", "--sandbox", "read-only", "--json", "--config=model_reasoning_effort=xhigh", "--", "p"}},
		{ChatRequest{Prompt: "p", SessionID: thread, ExtraArgs: "-c model_reasoning_effort=high"},
			[]string{"exec", "--sandbox", "read-only", "resume", "--json", "-c", "model_reasoning_effort=high", "--", thread, "p"}},
		{ChatRequest{Prompt: "p", SessionID: thread, ExtraArgs: "--config model_reasoning_effort=low"},
			[]string{"exec", "--sandbox", "read-only", "resume", "--json", "--config", "model_reasoning_effort=low", "--", thread, "p"}},
		{ChatRequest{Prompt: "p", SessionID: thread, ExtraArgs: "--config=model_reasoning_effort=low"},
			[]string{"exec", "--sandbox", "read-only", "resume", "--json", "--config=model_reasoning_effort=low", "--", thread, "p"}},
		{ChatRequest{Prompt: "p", SessionID: thread, ExtraArgs: "--skip-git-repo-check -c model_reasoning_effort=medium"},
			[]string{"exec", "--sandbox", "read-only", "resume", "--json", "--skip-git-repo-check", "-c", "model_reasoning_effort=medium", "--", thread, "p"}},
		{ChatRequest{Prompt: "-h", SessionID: "--yolo"},
			[]string{"exec", "--sandbox", "read-only", "resume", "--json", "--", "--yolo", "-h"}},
		{ChatRequest{Prompt: "--yolo"}, []string{"exec", "--sandbox", "read-only", "--json", "--", "--yolo"}},
		{ChatRequest{Prompt: "review"}, []string{"exec", "--sandbox", "read-only", "--json", "--", "review"}},
		{ChatRequest{Prompt: "help"}, []string{"exec", "--sandbox", "read-only", "--json", "--", "help"}},
	}
	for _, tc := range cases {
		tc.req.Binary, tc.req.Cwd = bin, t.TempDir()
		cmd, err := driver.BuildCmd(tc.req, ChatLaunchContext{})
		if err != nil {
			t.Fatal(err)
		}
		if got := cmd.Args[1:]; !slices.Equal(got, tc.tail) {
			t.Fatalf("argv = %q, want %q", got, tc.tail)
		}
	}
}

func TestCodexExecOptionsPrecedeResume(t *testing.T) {
	bin := testChatExecutable(t)
	driver := codexChatDriver{}
	const thread = "019f701f-0000-7000-8000-000000000001"
	cases := []struct {
		req  ChatRequest
		head []string // argv between the binary and "--json"
	}{
		{ChatRequest{}, []string{"exec", "--sandbox", "read-only"}},
		{ChatRequest{Mode: "workspace-write"}, []string{"exec", "--sandbox", "workspace-write"}},
		{ChatRequest{Mode: "danger-full-access"}, []string{"exec", "--sandbox", "danger-full-access"}},
		{ChatRequest{Sandbox: "workspace-write"}, []string{"exec", "--sandbox", "workspace-write"}},
		{ChatRequest{Mode: "danger-full-access", Sandbox: "workspace-write"}, []string{"exec", "--sandbox", "danger-full-access"}},
		{ChatRequest{Model: "local:ollama"}, []string{"exec", "--sandbox", "read-only", "--oss", "--local-provider", "ollama"}},
		{ChatRequest{Mode: "workspace-write", Model: "local:lmstudio"}, []string{"exec", "--sandbox", "workspace-write", "--oss", "--local-provider", "lmstudio"}},
		{ChatRequest{OSS: true, LocalProvider: "ollama"}, []string{"exec", "--sandbox", "read-only", "--oss", "--local-provider", "ollama"}},
		{ChatRequest{OSS: true}, []string{"exec", "--sandbox", "read-only", "--oss"}},
		{ChatRequest{Model: "local:"}, []string{"exec", "--sandbox", "read-only", "--oss"}},
		{ChatRequest{Model: "local:ollama", LocalProvider: "lmstudio"}, []string{"exec", "--sandbox", "read-only", "--oss", "--local-provider", "ollama"}},
	}
	for _, tc := range cases {
		for _, resume := range []bool{false, true} {
			req := tc.req
			req.Binary, req.Cwd, req.Prompt = bin, t.TempDir(), "p"
			want := slices.Clone(tc.head)
			if resume {
				req.SessionID = thread
				want = append(want, "resume")
			}
			want = append(want, "--json", "--")
			if tc.req.Model != "" && !strings.HasPrefix(tc.req.Model, "local:") {
				t.Fatalf("table rows use no hosted model: %+v", tc.req)
			}
			if resume {
				want = append(want, thread)
			}
			want = append(want, "p")
			cmd, err := driver.BuildCmd(req, ChatLaunchContext{})
			if err != nil {
				t.Fatal(err)
			}
			if got := cmd.Args[1:]; !slices.Equal(got, want) {
				t.Fatalf("%+v resume=%v: argv = %q, want %q", tc.req, resume, got, want)
			}
			assertCodexExecOptionsBeforeResume(t, cmd.Args, req.Model)
		}
	}
	// Legacy oss with a named model keeps both flags: valid Codex usage.
	cmd, err := driver.BuildCmd(ChatRequest{Binary: bin, Cwd: t.TempDir(), Prompt: "p", SessionID: thread, OSS: true, Model: "gpt-oss:20b"}, ChatLaunchContext{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"exec", "--sandbox", "read-only", "--oss", "resume", "--json", "-m", "gpt-oss:20b", "--", thread, "p"}; !slices.Equal(cmd.Args[1:], want) {
		t.Fatalf("argv = %q, want %q", cmd.Args[1:], want)
	}
	assertCodexExecOptionsBeforeResume(t, cmd.Args, "gpt-oss:20b")
	// A prompt or session id that spells an exec option stays positional.
	cmd, err = driver.BuildCmd(ChatRequest{Binary: bin, Cwd: t.TempDir(), Prompt: "--sandbox", SessionID: "--oss", Mode: "workspace-write", Model: "gpt-x"}, ChatLaunchContext{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--sandbox", "workspace-write", "resume", "--json", "-m", "gpt-x", "--", "--oss", "--sandbox"}
	if got := cmd.Args[1:]; !slices.Equal(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	assertCodexExecOptionsBeforeResume(t, cmd.Args, "gpt-x")
}

// assertCodexExecOptionsBeforeResume checks, over the tokens before the
// adapter's "--": exactly one --sandbox, at Args[2]; nothing exec-only after
// "resume"; no -m for a local:* model.
func assertCodexExecOptionsBeforeResume(t *testing.T, args []string, model string) {
	t.Helper()
	options := args[:slices.Index(args, "--")]
	sandboxes := 0
	afterResume := false
	for _, token := range options {
		switch token {
		case "--sandbox":
			sandboxes++
		case "resume":
			afterResume = true
			continue
		}
		if afterResume && (token == "--sandbox" || token == "--oss" || token == "--local-provider") {
			t.Fatalf("exec-only option %s after resume: %q", token, args)
		}
	}
	if sandboxes != 1 || options[2] != "--sandbox" {
		t.Fatalf("want exactly one --sandbox at Args[2]: %q", args)
	}
	if strings.HasPrefix(model, "local:") && slices.Contains(options, "-m") {
		t.Fatalf("model flag sent for a local model: %q", args)
	}
}
