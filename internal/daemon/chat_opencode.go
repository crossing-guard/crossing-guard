package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/taskinput"
)

func init() { registerChatDriver("opencode", openCodeChatDriver{}) }

type openCodeChatDriver struct{}

// openCodeGovernanceStatus reads the installed plugin's version-gate verdict.
// Injectable so capability tests never depend on this machine's real config.
var openCodeGovernanceStatus = guardcli.OpenCodeGovernance

func (openCodeChatDriver) ChatCapability() ChatCapability {
	governanceLane, governanceNote := "", ""
	if status := openCodeGovernanceStatus(); status.Attached {
		governanceLane, governanceNote = status.Lane, status.Note
	}
	return ChatCapability{
		MessageDelivery: SessionMessageCapability{Supported: true, Boundary: "next tool call or tool result (plugin append)",
			Detail: "Delivered by the installed Crossing Guard plugin as a context-only message through the session's own instance; works for terminal and console sessions alike."},
		GovernanceLane: governanceLane, GovernanceNote: governanceNote,
		Runtime: "opencode", DisplayName: "OpenCode", CanStart: true, CanResume: true,
		CanSignIn: false, VendorAuthDefault: false, AcceptsCustomModel: true,
		ModelHint: "provider/model (for example ollama/qwen2.5-coder:7b)",
		Modes: []ChatMode{
			{ID: "", Label: "Default", Risk: "normal", Description: "OpenCode's configured agent and permissions apply; permission prompts are refused, never auto-approved."},
			{ID: openCodeVisionMode, Label: "Vision (no tools)", Risk: "normal", Description: "Uses the no-tools vision-proof agent from global OpenCode configuration; refused unless it is defined there as a primary agent. Project configuration and extra arguments are not used."},
		},
		Models: []ChatModelOption{
			{ID: "", Label: "Session / configured default", Description: "Send no model flag."},
			{ID: "custom", Label: "Exact provider/model…", Custom: true},
		},
		Inputs: []ChatInputCapability{
			{Kind: "text", MediaTypes: []string{"text/plain"}, CanStart: true, CanResume: true,
				Note: "Delivered through OpenCode's ordered --file transport."},
			{Kind: "image", MediaTypes: []string{"image/png"}, CanStart: true, CanResume: true,
				ModelConditional: true, ModeConditional: true, Note: "Requires the proved vision model and the no-tools vision-proof agent, both in global OpenCode configuration."},
		},
	}
}

func (openCodeChatDriver) ValidateChatInputs(req ChatRequest, inputs []taskinput.ResolvedInput) error {
	for _, input := range inputs {
		switch input.Kind {
		case taskinput.KindText:
		case taskinput.KindImage:
			if req.Model != "ollama/qwen2.5vl:3b" || req.Mode != openCodeVisionMode {
				return fmt.Errorf("opencode image input requires model ollama/qwen2.5vl:3b and the vision-proof mode")
			}
		default:
			return fmt.Errorf("opencode does not support task input kind %q", input.Kind)
		}
	}
	return nil
}

func (openCodeChatDriver) BuildCmd(req ChatRequest, launch ChatLaunchContext) (*exec.Cmd, error) {
	bin, err := guardcli.ResolveRuntimeBinary("opencode", req.Binary)
	if err != nil {
		return nil, err
	}
	args := []string{"run", "--format", "json", "--dir", req.Cwd}
	if req.SessionID != "" {
		args = append(args, "--session", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Mode != "" {
		args = append(args, "--agent", req.Mode)
	}
	for _, input := range launch.Inputs {
		args = append(args, "--file", input.Path)
	}
	extra, err := openCodeExtraArgs(req)
	if err != nil {
		return nil, err
	}
	args = append(args, extra...)
	// --file is variadic: without the terminator it swallows the prompt (S0-1).
	args = append(args, "--", req.Prompt)
	cmd := exec.Command(bin, args...)
	cmd.Dir = req.Cwd
	cmd.Env = os.Environ()
	if req.Mode == openCodeVisionMode {
		// The run sees only the global configuration the agent check read, so a
		// repository cannot redefine or demote the agent the mode rests on.
		cmd.Env = append(cmd.Env, openCodeGlobalConfigOnly)
	}
	return cmd, nil
}

// openCodeVisionMode is the mode that promises the no-tools agent of the same name.
const openCodeVisionMode = "vision-proof"

// openCodeGlobalConfigOnly makes OpenCode ignore project opencode.json files,
// project .opencode/ agents and project instructions (measured on 1.18.0).
const openCodeGlobalConfigOnly = "OPENCODE_DISABLE_PROJECT_CONFIG=1"

// CanonicalizeChatRequest refuses what the adapter's argv owns before admission.
// OpenCode falls back to its tool-permitting default agent, with only a stderr
// warning, when the requested agent is missing or is a subagent — so the vision
// mode is admitted only after the binary that will run confirms the agent.
func (openCodeChatDriver) CanonicalizeChatRequest(req ChatRequest) (ChatRequest, error) {
	if _, err := openCodeExtraArgs(req); err != nil {
		return ChatRequest{}, err
	}
	if req.Mode == openCodeVisionMode {
		bin, err := guardcli.ResolveRuntimeBinary("opencode", req.Binary)
		if err != nil {
			return ChatRequest{}, err
		}
		if err := openCodeAgentAvailable(bin, openCodeVisionMode); err != nil {
			return ChatRequest{}, err
		}
	}
	return req, nil
}

// openCodeExtraOptions are the only run options extra arguments may carry, and
// whether each takes a value. Measured on OpenCode 1.18.0: none of them approves
// a permission prompt or touches plugins, the server or egress. --agent picks
// among already-configured agents (so their configured permissions), and is
// refused when a mode makes it BuildCmd's own slot. An
// allowlist, because yargs accepts every option under camel-case, negated,
// dotted, `=` and repeated spellings, and 1.18 hides two aliases of --auto
// (--yolo, --dangerously-skip-permissions) that approve every permission prompt.
// Interim: provider-owned-chat-options-plan.md replaces raw extra arguments.
var openCodeExtraOptions = map[string]bool{
	"--agent":    true,
	"--variant":  true,
	"--title":    true,
	"--thinking": false,
}

// openCodeExtraValueMaxBytes bounds one option value; a validation limit, not a tunable.
const openCodeExtraValueMaxBytes = 200

// openCodeExtraArgs returns the extra-argument tokens, verbatim, when every one
// fits the allowlist. A refusal names the first misfit by position and never
// quotes it: a token such as --model contains "mode", which handleChat routes to
// the mode control.
func openCodeExtraArgs(req ChatRequest) ([]string, error) {
	tokens := splitArgs(req.ExtraArgs)
	if req.Mode == openCodeVisionMode && len(tokens) > 0 {
		// A denylist cannot hold here: --no-agent, --agent.x=y, --command and
		// --attach each move the turn to another agent after the check passed.
		return nil, errors.New("the vision-proof mode does not accept extra arguments")
	}
	const where = " (Settings → extra args for OpenCode, or the request's extra_args)"
	seen := map[string]bool{}
	for i := 0; i < len(tokens); i++ {
		position := i + 1
		if tokens[i] == "--" {
			return nil, errors.New(`extra arguments cannot contain a bare "--"; the adapter owns the prompt boundary`)
		}
		name, value, inline := strings.Cut(tokens[i], "=")
		takesValue, known := openCodeExtraOptions[name]
		switch {
		case !known || (inline && !takesValue):
			return nil, fmt.Errorf("opencode extra arguments accept only --agent, --variant, --title (each with a value) and --thinking; argument %d is not accepted"+where, position)
		case name == "--agent" && req.Mode != "":
			// BuildCmd emits its own --agent for a mode; two become an array,
			// which OpenCode cannot resolve and answers with its default agent.
			return nil, fmt.Errorf("opencode extra arguments cannot set --agent when a mode selects the agent; argument %d"+where, position)
		case seen[name]:
			return nil, fmt.Errorf("opencode extra arguments accept each option once; argument %d repeats one"+where, position)
		case takesValue && !inline:
			if i+1 == len(tokens) || strings.HasPrefix(tokens[i+1], "-") {
				return nil, fmt.Errorf("opencode extra arguments need a value after argument %d"+where, position)
			}
			i++
			value = tokens[i]
		}
		if takesValue && !openCodeExtraValueOK(value) {
			if value == "" {
				return nil, fmt.Errorf("opencode extra arguments need a value after argument %d"+where, position)
			}
			return nil, fmt.Errorf("opencode extra argument %d has a value that is too long, not UTF-8, or contains control characters"+where, position)
		}
		seen[name] = true
	}
	return tokens, nil
}

// openCodeExtraValueOK refuses before admission what exec would refuse after it
// (NUL) and what would land in session titles (terminal escapes).
func openCodeExtraValueOK(value string) bool {
	if value == "" || len(value) > openCodeExtraValueMaxBytes || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

// Bounds of the interim agent check. Test seams only, not configuration: the
// runtime model catalog plan replaces this check with its configured runner.
var (
	openCodeAgentListTimeout   = 10 * time.Second
	openCodeAgentListWaitDelay = time.Second
	openCodeAgentListMaxBytes  = 1 << 20
)

// openCodeAgentHeader matches one agent-list header line. The permission JSON
// between headers never matches (indented, quoted, or a bare bracket) and is
// never decoded.
var openCodeAgentHeader = regexp.MustCompile(`^([^\s\[\]{}"]+) \((primary|subagent|all)\)$`)

// openCodeAgentAvailable runs `agent list` from a neutral directory with global
// configuration only, and returns nil only when exactly one header names the
// agent as primary or all. Every other outcome refuses.
func openCodeAgentAvailable(bin, name string) error {
	refuse := func(reason string) error {
		return fmt.Errorf("the %s mode needs an OpenCode agent named %q in global OpenCode configuration (%s)", name, name, reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), openCodeAgentListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "agent", "list")
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), openCodeGlobalConfigOnly)
	cmd.WaitDelay = openCodeAgentListWaitDelay
	prepareTaskProcess(cmd)
	cmd.Cancel = func() error { return interruptTaskProcess(cmd) }
	// A writer rather than a pipe: Wait and WaitDelay then bound a child that
	// keeps stdout open, which a read loop of our own would wait on forever.
	stdout := &openCodeBoundedOutput{max: openCodeAgentListMaxBytes, overflowed: cancel}
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		return refuse("the agent list could not be read: start failed")
	}
	waitErr := cmd.Wait()
	switch {
	case stdout.overflow:
		return refuse("the agent list could not be read: output exceeded its bound")
	case ctx.Err() != nil:
		return refuse("the agent list could not be read: timed out")
	case errors.Is(waitErr, exec.ErrWaitDelay):
		return refuse("the agent list could not be read: its output stayed open after exit")
	case waitErr != nil:
		return refuse("the agent list could not be read: exited with an error")
	}
	modes, listed := openCodeAgentModes(stdout.buf.Bytes(), name)
	switch {
	case listed == 0:
		return refuse("the agent list could not be read: no agents listed")
	case len(modes) == 0:
		return refuse("it is not defined")
	case len(modes) > 1:
		return refuse("it is defined more than once in the agent list")
	case modes[0] == "subagent":
		return refuse("it is defined as a subagent, so OpenCode would fall back to its default agent")
	}
	return nil
}

// openCodeBoundedOutput keeps at most max bytes and stops the process at the
// first byte past it; a truncated list is never parsed.
type openCodeBoundedOutput struct {
	buf        bytes.Buffer
	max        int
	overflow   bool
	overflowed func()
}

func (out *openCodeBoundedOutput) Write(p []byte) (int, error) {
	if out.overflow {
		return len(p), nil
	}
	if room := out.max - out.buf.Len(); len(p) > room {
		out.overflow = true
		out.overflowed()
		return len(p), nil
	}
	return out.buf.Write(p)
}

// openCodeAgentModes returns the mode of every header naming the agent, and how
// many headers were listed in all.
func openCodeAgentModes(out []byte, name string) (modes []string, listed int) {
	// A split, not a scanner: nothing can stop the parse early and hide a
	// second header naming the same agent.
	for _, line := range strings.Split(string(out), "\n") {
		match := openCodeAgentHeader.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if match == nil {
			continue
		}
		listed++
		if match[1] == name {
			modes = append(modes, match[2])
		}
	}
	return modes, listed
}

func (openCodeChatDriver) ProjectEvent(obj map[string]any) []ChatEvent {
	var out []ChatEvent
	add := func(event ChatEvent) { out = append(out, event) }
	typeName := anyString(obj["type"])
	part, _ := obj["part"].(map[string]any)
	if part == nil {
		part = obj
	}
	switch typeName {
	case "step_start", "step-start":
		if id := anyString(obj["sessionID"]); id != "" {
			add(ChatEvent{"type": "session", "id": id})
		}
	case "text":
		add(ChatEvent{"type": "text", "text": anyString(part["text"])})
	case "reasoning", "thinking":
		add(ChatEvent{"type": "thinking", "text": anyString(part["text"])})
	case "tool", "tool_use":
		name := anyString(part["tool"])
		state, _ := part["state"].(map[string]any)
		input := part["input"]
		if state != nil && input == nil {
			input = state["input"]
		}
		add(ChatEvent{"type": "tool", "name": name, "text": truncate(compactJSON(input), 600)})
		if state != nil {
			result := anyString(state["output"])
			isError := false
			if result == "" {
				result = anyString(state["error"])
				isError = result != ""
			}
			if result != "" {
				add(ChatEvent{"type": "tool_result", "text": truncate(result, 600), "is_error": isError})
			}
		}
	case "tool_result":
		isError, _ := part["is_error"].(bool)
		add(ChatEvent{"type": "tool_result", "text": truncate(anyString(part["text"]), 600), "is_error": isError})
	case "step_finish", "step-finish":
		cost := obj["cost"]
		if cost == nil {
			cost = part["cost"]
		}
		add(ChatEvent{"type": "result", "cost": cost, "usage": part["tokens"]})
	case "error":
		message := anyString(obj["message"])
		if message == "" {
			message = anyString(part["error"])
		}
		if isVendorAuthFailure(message) {
			add(ChatEvent{"type": "auth_required", "runtime": "opencode", "text": "Authenticate the configured OpenCode provider and retry this turn."})
		} else {
			// This branch is OpenCode's transport error channel (stream/API
			// errors), so provider classification is legitimate here (R3).
			add(classifiedProviderError(ChatEvent{"type": "error", "text": message}, message))
		}
	default:
		add(ChatEvent{"type": "stderr", "text": "Unrecognized OpenCode event type: " + truncate(typeName, 80)})
	}
	return out
}
