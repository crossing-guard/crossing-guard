package daemon

import (
	"os"
	"path/filepath"
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
		if err != nil || got.Prompt != "hi" || (strings.Contains(extra, "model_reasoning_effort=") && got.ThinkingEffort == nil) {
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
	const where = " (Settings → Runtimes → extra args for Codex, or the request's extra_args)"
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
			[]string{"exec", "--sandbox", "read-only", "--json", "-c", "model_reasoning_effort=low", "--", "p"}},
		{ChatRequest{Prompt: "p", ExtraArgs: "--config=model_reasoning_effort=xhigh"},
			[]string{"exec", "--sandbox", "read-only", "--json", "-c", "model_reasoning_effort=xhigh", "--", "p"}},
		{ChatRequest{Prompt: "p", SessionID: thread, ExtraArgs: "-c model_reasoning_effort=high"},
			[]string{"exec", "--sandbox", "read-only", "resume", "--json", "-c", "model_reasoning_effort=high", "--", thread, "p"}},
		{ChatRequest{Prompt: "p", SessionID: thread, ExtraArgs: "--config model_reasoning_effort=low"},
			[]string{"exec", "--sandbox", "read-only", "resume", "--json", "-c", "model_reasoning_effort=low", "--", thread, "p"}},
		{ChatRequest{Prompt: "p", SessionID: thread, ExtraArgs: "--config=model_reasoning_effort=low"},
			[]string{"exec", "--sandbox", "read-only", "resume", "--json", "-c", "model_reasoning_effort=low", "--", thread, "p"}},
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

// LocalRoute agrees with BuildCmd's own local-lane decision
// (managed-turn-profile-limits plan §4.1, VR-6, R2-7): a request is local
// exactly when the argv selects --oss with a BUILT-IN local provider. Codex
// 0.154 accepts any configured provider id after --local-provider, so any
// other provider — or none — is not a local claim.
func TestCodexLocalRouteAgreesWithTheLocalLaneBuildCmdEmits(t *testing.T) {
	bin := testChatExecutable(t)
	driver := codexChatDriver{}
	cases := []struct {
		req   ChatRequest
		local bool
	}{
		{ChatRequest{}, false},
		{ChatRequest{Model: "gpt-5.6-sol"}, false},
		{ChatRequest{Model: "local:"}, false},
		{ChatRequest{Model: "local:ollama"}, true},
		{ChatRequest{Model: "local:lmstudio"}, true},
		{ChatRequest{Model: "local:foo"}, false},
		{ChatRequest{Model: "Local:ollama"}, false},
		{ChatRequest{OSS: true}, false},
		{ChatRequest{OSS: true, LocalProvider: "ollama"}, true},
		{ChatRequest{OSS: true, LocalProvider: "foo"}, false},
		{ChatRequest{Model: "local:ollama", LocalProvider: "lmstudio"}, true},
		{ChatRequest{Model: "local:foo", LocalProvider: "ollama"}, false},
	}
	for _, tc := range cases {
		req := tc.req
		req.Binary, req.Cwd, req.Prompt = bin, t.TempDir(), "p"
		cmd, err := driver.BuildCmd(req, ChatLaunchContext{})
		if err != nil {
			t.Fatalf("%+v: %v", tc.req, err)
		}
		oss, provider := false, ""
		for index, arg := range cmd.Args {
			if arg == "--" {
				break
			}
			if arg == "--oss" {
				oss = true
			}
			if arg == "--local-provider" && index+1 < len(cmd.Args) {
				provider = cmd.Args[index+1]
			}
		}
		emittedLocal := oss && (provider == "ollama" || provider == "lmstudio")
		local, basis := driver.LocalRoute(tc.req)
		if local != tc.local || local != emittedLocal {
			t.Fatalf("%+v: LocalRoute=%v want %v; argv local lane=%v (%v)", tc.req, local, tc.local, emittedLocal, cmd.Args)
		}
		if local && !strings.Contains(basis, provider) {
			t.Fatalf("%+v: basis %q does not name the provider", tc.req, basis)
		}
	}
}

// The declared Local options are the ones LocalRoute claims, and they are the
// ones that need no vendor sign-in — one fact, three encodings, kept equal.
func TestCodexDeclaredLocalOptionsMatchLocalRoute(t *testing.T) {
	driver := codexChatDriver{}
	claimed := 0
	for _, option := range driver.ChatCapability().Models {
		if option.Custom {
			continue
		}
		local, _ := driver.LocalRoute(ChatRequest{Model: option.ID})
		needsNoSignIn := option.VendorAuthRequired != nil && !*option.VendorAuthRequired
		if local != needsNoSignIn {
			t.Fatalf("option %q: LocalRoute=%v but vendor sign-in not required=%v", option.ID, local, needsNoSignIn)
		}
		if local {
			claimed++
		}
	}
	if claimed == 0 {
		t.Fatal("Codex declares no local option; the local-route advice would never be offered")
	}
}

func TestCodexBundledBinariesNewestLayoutFirst(t *testing.T) {
	if len(codexBundledBinaries) < 2 {
		t.Fatalf("both app layouts must stay listed: %v", codexBundledBinaries)
	}
	if !strings.HasSuffix(codexBundledBinaries[0], "/codex-cli/bin/codex") {
		t.Fatalf("the declared entrypoint of the current layout must be tried first: %v", codexBundledBinaries)
	}
	if !strings.HasSuffix(codexBundledBinaries[1], "/Contents/Resources/codex") {
		t.Fatalf("the pre-2026-09-30 location must stay as the second entry: %v", codexBundledBinaries)
	}
	for _, path := range codexBundledBinaries {
		if !filepath.IsAbs(path) || filepath.Base(path) != "codex" {
			t.Errorf("bundle entry %q must be an absolute path to the codex executable", path)
		}
	}
}

// TestCodexBundlePathHasOneOwner guards the call sites: the app moved its CLI once and
// six scattered literals all broke. Only chat_codex.go may name a bundle path, only
// codexBinary may resolve "codex", and the skills probe keeps no lookup of its own.
// It is a line scan, not a parser: a path assembled from pieces, or a token after a
// "//" inside a string literal, would escape it.
func TestCodexBundlePathHasOneOwner(t *testing.T) {
	root := filepath.Join("..", "..")
	tokens := []string{"/Applications/ChatGPT", "CodexCLI.app", "Resources/codex"}
	resolves := 0
	for _, dir := range []string{"engine", "store", "harvest", "memory", "internal", "cmd", "ruledoc", "schemas", "teamwire"} {
		top := filepath.Join(root, dir)
		if _, err := os.Stat(top); os.IsNotExist(err) {
			continue // not every listed directory exists in every checkout
		}
		err := filepath.WalkDir(top, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
			owner := rel == "internal/daemon/chat_codex.go"
			for n, line := range strings.Split(string(src), "\n") {
				code, _, _ := strings.Cut(line, "//")
				for _, token := range tokens {
					if strings.Contains(code, token) && !owner {
						t.Errorf("%s:%d names a Codex bundle path (%s); codexBundledBinaries owns it", rel, n+1, token)
					}
				}
				if strings.Contains(code, `guardcli.ResolveRuntimeBinary("codex"`) {
					resolves++
					if !owner {
						t.Errorf("%s:%d resolves codex itself; call codexBinary", rel, n+1)
					}
				}
				if rel == "internal/daemon/skills_codex.go" && strings.Contains(code, "LookPath") {
					t.Errorf("%s:%d keeps a private lookup; call codexBinary", rel, n+1)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if resolves != 1 {
		t.Fatalf(`guardcli.ResolveRuntimeBinary("codex" must appear exactly once, in chat_codex.go; found %d`, resolves)
	}
}
