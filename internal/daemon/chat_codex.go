package daemon

import (
	"bytes"
	"context"
	"crossing-guard/internal/guardcli"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"crossing-guard/internal/taskinput"
)

func init() { registerChatDriver("codex", codexChatDriver{}) }

// codexChatDriver drives `codex exec --json`.
type codexChatDriver struct{}

// codexBundledBinaries are where the ChatGPT app ships the CLI, newest layout first.
// Codex is bundled inside the app, not on PATH (proven 2026-07-13), so these are
// runtime-specific fallbacks rather than search directories. The app moved the CLI on
// 2026-09-30; a move is a new entry here, not an edit at every caller.
var codexBundledBinaries = []string{
	"/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex", // codex-package.json layoutVersion 1 entrypoint
	"/Applications/ChatGPT.app/Contents/Resources/codex",               // before 2026-09-30
}

// codexBinary is the one place the Codex executable is located: a configured path,
// then PATH and the known install directories, then the app bundle.
func codexBinary(configured string) (string, error) {
	return guardcli.ResolveRuntimeBinary("codex", configured, codexBundledBinaries...)
}

func (codexChatDriver) ChatCapability() ChatCapability {
	return ChatCapability{
		MessageDelivery: codexDeliveryCapability(),
		Runtime:         "codex", DisplayName: "Codex", CanStart: true, CanResume: true,
		CanSignIn: true, VendorAuthDefault: true, AcceptsCustomModel: true,
		ModelHint: "Codex model ID",
		Modes: []ChatMode{
			{ID: "", Label: "Read Only", Risk: "normal", Description: "Read files; no writes or network."},
			{ID: "workspace-write", Label: "Workspace Write", Risk: "elevated", Description: "May write inside the working directory."},
			{ID: "danger-full-access", Label: "Full Access", Risk: "dangerous", Description: "No sandbox. Trusted tasks only."},
		},
		Models: []ChatModelOption{
			{ID: "", Label: "Binary default", Description: "Send no model flag."},
			{ID: "local:ollama", Label: "Local · Ollama", VendorAuthRequired: boolPointer(false)},
			{ID: "local:lmstudio", Label: "Local · LM Studio", VendorAuthRequired: boolPointer(false)},
			{ID: "custom", Label: "Custom…", Custom: true},
		},
		Inputs: []ChatInputCapability{
			{Kind: "text", MediaTypes: []string{"text/plain"}, CanStart: true, CanResume: true,
				Note: "Validated UTF-8 content is serialized into the prompt."},
			{Kind: "image", MediaTypes: []string{"image/png"}, CanStart: true, CanResume: true,
				ModelConditional: true, CatalogRequired: true, Note: "Proved for the configured Codex default model; custom/local model support is not inferred."},
		},
	}
}

func (codexChatDriver) ValidateChatInputs(req ChatRequest, inputs []taskinput.ResolvedInput) error {
	for _, input := range inputs {
		switch input.Kind {
		case taskinput.KindText:
		case taskinput.KindImage:
			if req.Model != "" {
				return fmt.Errorf("codex image input is not verified for the selected custom/local model")
			}
		default:
			return fmt.Errorf("codex does not support task input kind %q", input.Kind)
		}
	}
	return nil
}

func (codexChatDriver) ProbeVendorAuth(ctx context.Context) (bool, string, string, error) {
	bin, err := codexBinary("")
	if err != nil {
		return false, "", "", err
	}
	// This build writes its human status line outside stdout. The command's success
	// exit is the stable machine signal.
	if err := exec.CommandContext(ctx, bin, "login", "status").Run(); err != nil {
		return false, "", "", nil
	}
	return true, "Codex CLI login", "", nil
}

func (codexChatDriver) BuildVendorLogin(ctx context.Context) (*exec.Cmd, error) {
	bin, err := codexBinary("")
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, bin, "login"), nil
}

func (d codexChatDriver) CanonicalizeChatRequest(req ChatRequest) (ChatRequest, error) {
	return d.ParseEffort(req)
}

func (codexChatDriver) ParseEffort(req ChatRequest) (ChatRequest, error) {
	tokens, err := codexExtraArgs(req.ExtraArgs)
	if err != nil {
		return ChatRequest{}, err
	}
	keep := []string{}
	level := ""
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "-c" || token == "--config" {
			i++
			level = strings.TrimPrefix(tokens[i], "model_reasoning_effort=")
		} else if strings.HasPrefix(token, "--config=") {
			level = strings.TrimPrefix(token, "--config=model_reasoning_effort=")
		} else {
			keep = append(keep, token)
		}
	}
	req.ExtraArgs = strings.Join(keep, " ")
	return mergeLegacyEffort(req, level)
}

const codexExtraArgsWhere = "(Settings → Runtimes → extra args for Codex, or the request's extra_args)"

// codexEffortLevel is the shape of a reasoning-effort level. The provider owns the
// enum and refuses a bad word at turn time. codexExtraArgs also refuses the TOML
// keywords (true, false, inf, nan): Codex parses the value as TOML and would fail
// config load on them after admission.
var codexEffortLevel = regexp.MustCompile(`^[a-z]{1,16}$`)

// codexExtraArgs is the allowlist for operator extra arguments, measured on
// Codex CLI 0.154.0-alpha.6.2 (codex-extra-args-allowlist-plan.md §4.1). Every
// other token is refused: clap accepts hidden aliases (--yolo is the sandbox
// bypass) and -c takes any config key, so a denylist cannot be complete.
// Messages never quote a token: "model" contains "mode" and handleChat would
// route the refusal to the mode control.
func codexExtraArgs(raw string) ([]string, error) {
	tokens := splitArgs(raw)
	seen := map[string]bool{}
	once := func(name string, index int) error {
		if seen[name] {
			return fmt.Errorf("codex extra arguments accept each option once; argument %d repeats one %s", index+1, codexExtraArgsWhere)
		}
		seen[name] = true
		return nil
	}
	effort := func(value string, index int) error {
		level, ok := strings.CutPrefix(value, "model_reasoning_effort=")
		switch {
		case !ok, !codexEffortLevel.MatchString(level),
			level == "true", level == "false", level == "inf", level == "nan":
			return fmt.Errorf("codex extra argument %d must set only the reasoning-effort key to a short lowercase level %s", index+1, codexExtraArgsWhere)
		}
		return nil
	}
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		switch {
		case token == "--skip-git-repo-check":
			if err := once(token, index); err != nil {
				return nil, err
			}
		case token == "-c" || token == "--config":
			if err := once("config", index); err != nil {
				return nil, err
			}
			if index+1 == len(tokens) {
				return nil, fmt.Errorf("codex extra arguments need a value after argument %d %s", index+1, codexExtraArgsWhere)
			}
			index++
			if err := effort(tokens[index], index); err != nil {
				return nil, err
			}
		case strings.HasPrefix(token, "--config="):
			if err := once("config", index); err != nil {
				return nil, err
			}
			if err := effort(strings.TrimPrefix(token, "--config="), index); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("codex extra arguments accept only --skip-git-repo-check and one -c reasoning-effort override; argument %d is not accepted %s", index+1, codexExtraArgsWhere)
		}
	}
	return tokens, nil
}

// BuildCmd: codex exec --sandbox S [--oss ...] --json [opts] [--image p]... -- "<prompt>"
//
//	codex exec --sandbox S [--oss ...] resume --json [opts] [--image p]... -- <id> "<prompt>"
//
// --sandbox, --oss and --local-provider are exec-level options: exec resume
// rejects them (0.154), and given before "resume" they apply to the resumed
// thread (measured, codex-resume-sandbox-plan.md §2). The sandbox is always
// sent, Read Only included, because with none Codex runs workspace-write in a
// trusted project. Options come before the "--"; the session id and prompt come
// after it, so neither is ever parsed as an option or an exec subcommand
// ("review", "help"). The greedy --image list stops at "--" (proven with
// ordered images 2026-09-24).
func (codexChatDriver) BuildCmd(req ChatRequest, launch ChatLaunchContext) (*exec.Cmd, error) {
	var effortErr error
	req, effortErr = normalizeEffort(req)
	if effortErr == nil {
		req, effortErr = (codexChatDriver{}).ParseEffort(req)
	}
	if effortErr != nil {
		return nil, effortErr
	}
	extra, err := codexExtraArgs(req.ExtraArgs)
	if err != nil {
		return nil, err
	}
	bin, err := codexBinary(req.Binary)
	if err != nil {
		return nil, err
	}
	mode := req.Mode
	if mode == "" { // legacy API compatibility
		mode = req.Sandbox
	}
	if mode == "" { // the Read Only capability mode
		mode = "read-only"
	}
	args := []string{"exec", "--sandbox", mode}
	if localProvider, oss := codexLocalLane(req); oss {
		args = append(args, "--oss")
		if localProvider != "" {
			args = append(args, "--local-provider", localProvider)
		}
	}
	if req.SessionID != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json")
	if req.Model != "" && !strings.HasPrefix(req.Model, "local:") {
		args = append(args, "-m", req.Model)
	}
	args = append(args, extra...)
	if effort := req.ThinkingEffort; effort != nil && effort.Kind == "level" {
		if !codexEffortLevel.MatchString(effort.Value) {
			return nil, effortError("invalid_choice", "Invalid native effort")
		}
		args = append(args, "-c", "model_reasoning_effort="+effort.Value)
	}
	for _, input := range launch.Inputs {
		if input.Kind == taskinput.KindImage {
			args = append(args, "--image", input.Path)
		}
	}
	args = append(args, "--")
	if req.SessionID != "" {
		args = append(args, req.SessionID)
	}
	prompt := req.Prompt
	for index, input := range launch.Inputs {
		if input.Kind != taskinput.KindText {
			continue
		}
		raw, err := os.ReadFile(input.Path)
		if err != nil {
			return nil, fmt.Errorf("read prepared text input: %w", err)
		}
		prompt += fmt.Sprintf("\n\n<task-input index=%q kind=\"text\">\n%s\n</task-input>", fmt.Sprint(index+1), raw)
	}
	args = append(args, prompt)
	cmd := exec.Command(bin, args...)
	cmd.Dir = req.Cwd
	cmd.Env = chatLaunchEnv(req)
	return cmd, nil
}

// codexLocalLane is the one decision for Codex's local-provider lane, shared by
// BuildCmd (which emits --oss [--local-provider P]) and LocalRoute: a `local:P`
// model id selects the lane with provider P (the model wins over a separate
// LocalProvider), and the legacy OSS flag selects it with LocalProvider.
func codexLocalLane(req ChatRequest) (provider string, oss bool) {
	provider = strings.TrimPrefix(req.Model, "local:")
	if provider != req.Model {
		return provider, true
	}
	if req.OSS {
		return req.LocalProvider, true
	}
	return "", false
}

// codexBuiltinLocalProviders are the providers Codex ships as local model
// servers. Only they count as local: measured on 0.154.0-alpha.6.2,
// `--local-provider` accepts ANY configured provider id ("Model provider
// `bogus` not found"), and a configured provider may name a remote base URL.
// Even these two are the runtime's own configuration — CODEX_OSS_BASE_URL and
// provider settings can repoint them — so the claim is the lane, not a
// verified endpoint.
var codexBuiltinLocalProviders = map[string]bool{"ollama": true, "lmstudio": true}

// LocalRoute: a request is local when it selects the local-provider lane with
// a built-in local provider (managed-turn-profile-limits plan §4.1).
func (codexChatDriver) LocalRoute(req ChatRequest) (bool, string) {
	provider, oss := codexLocalLane(req)
	if !oss || !codexBuiltinLocalProviders[provider] {
		return false, ""
	}
	return true, "Codex local provider lane: " + provider
}

// ProjectEvent maps codex exec records without coupling the adapter to SSE.
func (codexChatDriver) ProjectEvent(obj map[string]any) []ChatEvent {
	var out []ChatEvent
	add := func(event ChatEvent) { out = append(out, event) }
	switch obj["type"] {
	case "thread.started":
		add(ChatEvent{"type": "session", "id": anyString(obj["thread_id"])})
	case "item.completed":
		item, _ := obj["item"].(map[string]any)
		if item == nil {
			return out
		}
		switch item["type"] {
		case "agent_message":
			add(ChatEvent{"type": "text", "text": anyString(item["text"])})
		case "command_execution":
			add(ChatEvent{"type": "tool", "name": "shell", "text": truncate(anyString(item["command"]), 600)})
			if out := anyString(item["aggregated_output"]); out != "" {
				add(ChatEvent{"type": "tool_result", "text": truncate(out, 600)})
			}
		case "file_change":
			add(ChatEvent{"type": "tool", "name": "edit", "text": truncate(compactJSON(item["changes"]), 600)})
		case "reasoning":
			if t := anyString(item["text"]); t != "" {
				add(ChatEvent{"type": "thinking", "text": t})
			}
		}
	case "turn.completed":
		// turn.completed.usage is a running total for the whole thread, across
		// invocations (measured on 0.154.0-alpha, plan §4 step 0), so one turn's
		// share cannot be derived neutrally. Decision D-7: live Codex usage is
		// unknown; session history carries Codex usage.
		add(ChatEvent{"type": "result"})
	case "error":
		message := anyString(obj["message"])
		if isVendorAuthFailure(message) {
			add(ChatEvent{"type": "auth_required", "runtime": "codex",
				"text": "Sign in to codex and retry this turn."})
		} else {
			// Codex's transport error channel — provider classification is
			// legitimate here (R3); the --oss local-provider lane surfaces
			// Ollama's quota/gateway wording through this same event.
			add(classifiedProviderError(ChatEvent{"type": "error", "text": message}, message))
		}
	}
	return out
}

// Codex queue requires an exact native UUID; do not use its fuzzy title lookup.
var codexSessionUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// codexDeliveryTransport selects how a message reaches a Codex thread: the
// queue verb (default — the only Codex transport with recorded consumption on
// the reference host; source-only, experimental-gated in the vendor) or the
// hook-boundary carrier shared with every hook-bearing runtime, which waits
// on the Codex turn/result hook canaries. Configured through
// delivery.runtime_options.codex.transport; the word lives only here.
var codexDeliveryTransport = "queue"

func codexDeliveryCapability() SessionMessageCapability {
	if codexDeliveryTransport == "hook" {
		return SessionMessageCapability{Supported: true, Boundary: "next hook boundary (tool call; tool result and prompt submit once their canaries record rows)",
			Detail: "Delivered as hook-provided context by the installed Crossing Guard hook; configured transport: hook (probe-gated on this host's Codex hook canaries)."}
	}
	return SessionMessageCapability{Supported: true, Boundary: "next vendor turn boundary",
		Detail: "Queues to the exact Codex thread with the vendor's queue verb (undocumented, source-only); acceptance does not prove consumption."}
}

// ConfigureDelivery is the adapter's half of delivery.runtime_options.
func (codexChatDriver) ConfigureDelivery(raw json.RawMessage) error {
	var options struct {
		Transport string `json:"transport"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil {
		return err
	}
	switch options.Transport {
	case "", "queue":
		codexDeliveryTransport = "queue"
	case "hook":
		codexDeliveryTransport = "hook"
	default:
		return fmt.Errorf("transport must be queue or hook, not %q", options.Transport)
	}
	return nil
}

func (codexChatDriver) DeliverSessionMessage(ctx context.Context, target SessionIdentity, message string) SessionMessageReceipt {
	if codexDeliveryTransport == "hook" {
		if (target.NativeID == "" && target.CatalogID == "") || strings.TrimSpace(message) == "" {
			return SessionMessageReceipt{State: "unavailable", Tier: "none", Detail: "Delivery requires an exact session identity and a nonempty message."}
		}
		return SessionMessageReceipt{State: "accepted", Tier: "queued-delivery", Carrier: sessionMessageCarrierBoundary,
			Boundary: "next hook boundary (tool call; tool result and prompt submit once their canaries record rows)"}
	}
	sessionID := target.NativeID
	if !codexSessionUUID.MatchString(sessionID) || strings.TrimSpace(message) == "" {
		return SessionMessageReceipt{State: "unavailable", Detail: "Delivery requires an exact native session UUID and a nonempty message."}
	}
	bin, err := codexBinary("")
	if err != nil {
		return SessionMessageReceipt{State: "unavailable", Detail: err.Error()}
	}
	// One `--message=` token: as a separate value, clap parses a message that
	// starts with `-` (a Markdown bullet, `--yolo`) as a flag and exits 2
	// (measured on 0.154.0-alpha.6.2).
	cmd := exec.CommandContext(ctx, bin, "queue", "--thread", sessionID, "--message="+message)
	cmd.WaitDelay = time.Second
	output := &codexDeliveryOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return SessionMessageReceipt{State: "unavailable", Detail: "Queue command could not start."}
	}
	if err := cmd.Wait(); err != nil {
		return SessionMessageReceipt{State: "unknown", Detail: "Queue command did not complete successfully; do not retry automatically."}
	}
	if output.overflow {
		return SessionMessageReceipt{State: "unknown", Detail: "Queue output exceeded the receipt bound; do not retry automatically."}
	}
	return codexQueueReceipt(sessionID, output.buf.String())
}

func codexQueueReceipt(sessionID, output string) SessionMessageReceipt {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) == 6 && fields[0] == "Queued" && fields[1] == "message" && codexSessionUUID.MatchString(fields[2]) && fields[3] == "for" && fields[4] == "thread" && fields[5] == sessionID+"." {
		return SessionMessageReceipt{State: "accepted", Tier: "queued-delivery", Boundary: "next vendor turn boundary", MessageID: fields[2], Detail: "Queued for a subsequent turn boundary; consumption is not confirmed."}
	}
	return SessionMessageReceipt{State: "unknown", Detail: "Queue acknowledgement was ambiguous; do not retry automatically."}
}

type codexDeliveryOutput struct {
	buf      bytes.Buffer
	overflow bool
}

func (out *codexDeliveryOutput) Write(p []byte) (int, error) {
	n := len(p)
	if room := 8192 - out.buf.Len(); room > 0 {
		if len(p) > room {
			out.overflow = true
			p = p[:room]
		}
		_, _ = out.buf.Write(p)
	} else if n > 0 {
		out.overflow = true
	}
	return n, nil
}

// codexModelListPagesMax bounds model/list pagination; a validation limit.
const codexModelListPagesMax = 20

// DiscoverChatModels asks the app-server's model/list (measured on
// 0.154.0-alpha, plan §4 step 0: id, displayName, description, isDefault,
// hidden, inputModalities; no limits or prices; paginated by nextCursor). The
// process runs in the framework's bounded session; the protocol is the shared
// app-server helper.
func (codexChatDriver) DiscoverChatModels(ctx context.Context, env ChatModelEnv) (ChatModelDiscovery, error) {
	discovery := ChatModelDiscovery{Scope: "Codex models available to the signed-in account (app-server model/list)"}
	bin, err := codexBinary("")
	if err != nil {
		return discovery, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	discovery.Binary = bin
	cmd := exec.Command(bin, "app-server")
	cmd.Env = chatLaunchEnv(ChatRequest{}) // a model listing: no ticket
	session, err := env.Session(cmd)
	if err != nil {
		return discovery, err
	}
	defer session.Close()
	rpc := &codexAppServer{stdin: session.Stdin, lines: session.Lines, done: ctx.Done()}
	failed := func(err error) error {
		if streamErr := session.Err(); streamErr != nil {
			return streamErr
		}
		if ctx.Err() != nil {
			return modelDiscoveryFailure(modelReasonTimedOut)
		}
		if errors.Is(err, errCodexAppServerClosed) {
			return modelDiscoveryFailure(modelReasonExited)
		}
		return modelDiscoveryFailure(modelReasonUnparseable)
	}
	params := map[string]any{}
	var defaults, others []ChatModelOption
	for page := 0; page < codexModelListPagesMax; page++ {
		var result json.RawMessage
		if page == 0 {
			result, err = rpc.initializeAndCall("crossing-guard-models", "model/list", params)
		} else {
			result, err = rpc.call("model/list", params)
		}
		if err != nil {
			return discovery, failed(err)
		}
		var listed struct {
			Data []struct {
				ID                        string   `json:"id"`
				DisplayName               string   `json:"displayName"`
				Description               string   `json:"description"`
				IsDefault                 bool     `json:"isDefault"`
				Hidden                    bool     `json:"hidden"`
				InputModalities           []string `json:"inputModalities"`
				SupportedReasoningEfforts []struct {
					Level       string `json:"reasoningEffort"`
					Description string `json:"description"`
				} `json:"supportedReasoningEfforts"`
				DefaultReasoningEffort string `json:"defaultReasoningEffort"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(result, &listed) != nil {
			return discovery, modelDiscoveryFailure(modelReasonUnparseable)
		}
		for _, model := range listed.Data {
			if model.Hidden || model.ID == "" {
				continue
			}
			label := model.DisplayName
			if label == "" {
				label = model.ID
			}
			option := ChatModelOption{ID: model.ID, Label: label, Description: model.Description, Source: chatModelSourceRuntime}
			levels := []string{}
			for _, effort := range model.SupportedReasoningEfforts {
				levels = append(levels, effort.Level)
			}
			option.Effort = nativeEffortCapability(levels, model.DefaultReasoningEffort)
			// Publish only what ValidateChatInputs would admit for a named
			// model (S-RT5): text, never image.
			if slices.Contains(model.InputModalities, string(taskinput.KindText)) {
				option.Inputs = []string{string(taskinput.KindText)}
			}
			if model.IsDefault {
				defaults = append(defaults, option)
			} else {
				others = append(others, option)
			}
		}
		if listed.NextCursor == nil || *listed.NextCursor == "" {
			discovery.Models = append(defaults, others...)
			return discovery, nil
		}
		params = map[string]any{"cursor": *listed.NextCursor}
	}
	return discovery, modelDiscoveryFailure(modelReasonTooManyEntries)
}
