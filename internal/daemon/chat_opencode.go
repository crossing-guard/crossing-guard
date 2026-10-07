package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"crossing-guard/harvest"
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
			{ID: "", Label: "Default", Risk: "normal", Description: "OpenCode's configured agent and permissions apply; permission requests wait in the Crossing Guard approvals inbox."},
			{ID: openCodeVisionMode, Label: "Vision (no tools)", Risk: "normal", Description: "Uses the no-tools vision-proof agent from global OpenCode configuration; refused unless it is defined there as a primary agent. Project configuration and extra arguments are not used."},
		},
		Models: []ChatModelOption{
			{ID: "", Label: "Session / configured default", Description: "Send no model flag."},
			{ID: "custom", Label: "Exact provider/model…", Custom: true},
		},
		Inputs: []ChatInputCapability{
			{Kind: "text", MediaTypes: []string{"text/plain"}, CanStart: true, CanResume: true,
				Note: "Delivered through OpenCode's ordered file parts."},
			{Kind: "image", MediaTypes: []string{"image/png"}, CanStart: true, CanResume: true,
				ModelConditional: true, CatalogRequired: true, ModeConditional: true, Note: "Requires a model OpenCode reports as accepting images, and the no-tools vision-proof agent in global OpenCode configuration."},
		},
	}
}

func (openCodeChatDriver) ValidateChatInputs(req ChatRequest, inputs []taskinput.ResolvedInput) error {
	for _, input := range inputs {
		switch input.Kind {
		case taskinput.KindText:
		case taskinput.KindImage:
			// Which models accept images is OpenCode's fact, checked against its
			// own model list by the framework before this runs (design §5.3).
			// The adapter keeps its safety rule: a concrete model and the
			// no-tools agent.
			if req.Model == "" || req.Mode != openCodeVisionMode {
				return fmt.Errorf("opencode image input requires a model chosen from OpenCode's list and the vision-proof mode")
			}
		default:
			return fmt.Errorf("opencode does not support task input kind %q", input.Kind)
		}
	}
	return nil
}

func (openCodeChatDriver) BuildCmd(req ChatRequest, launch ChatLaunchContext) (*exec.Cmd, error) {
	if err := validateOpenCodeTransportRequest(req); err != nil {
		return nil, err
	}
	if launch.TaskID == "" || launch.DataDir == "" {
		return nil, fmt.Errorf("OpenCode approvals require daemon task identity and data directory")
	}
	bin, err := guardcli.ResolveRuntimeBinary("opencode", req.Binary)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("prepare OpenCode server authentication: %w", err)
	}
	cmd := exec.Command(bin, "serve", "--hostname", "127.0.0.1", "--port", "0")
	cmd.Dir = req.Cwd
	// A handoff cannot be opened in OpenCode in this release (OD-7b), so this
	// launch is built with no ticket whatever the request carries.
	cmd.Env = withoutEnv(chatLaunchEnv(ChatRequest{}), "OPENCODE_SERVER_PASSWORD", "OPENCODE_SERVER_USERNAME")
	cmd.Env = append(cmd.Env, "OPENCODE_SERVER_USERNAME=opencode", "OPENCODE_SERVER_PASSWORD="+hex.EncodeToString(secret))
	if req.Mode == openCodeVisionMode {
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
	if err := validateOpenCodeTransportRequest(req); err != nil {
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

func validateOpenCodeTransportRequest(req ChatRequest) error {
	if strings.TrimSpace(req.ExtraArgs) != "" {
		return errors.New("OpenCode GUI extra_args are unsupported by the interactive approval transport")
	}
	if req.Model != "" {
		provider, model, ok := strings.Cut(req.Model, "/")
		if !ok || provider == "" || model == "" {
			return errors.New("OpenCode model must be provider/model")
		}
	}
	if req.SessionID != "" && !openCodeWireID(req.SessionID, "ses_") {
		return errors.New("invalid OpenCode session id")
	}
	return nil
}

// Bounds of the agent check. Test seams only, not configuration: a per-request
// check whose limits are safety bounds, not tunables.
var (
	openCodeAgentListTimeout   = 10 * time.Second
	openCodeAgentListWaitDelay = time.Second
	openCodeAgentListMaxBytes  = 1 << 20
)

// openCodeAgentHeader matches one agent-list header line. The permission JSON
// between headers never matches (indented, quoted, or a bare bracket) and is
// never decoded.
var openCodeAgentHeader = regexp.MustCompile(`^([^\s\[\]{}"]+) \((primary|subagent|all)\)$`)

// openCodeAgentAvailable runs `agent list` through the framework's bounded
// runner, from a neutral directory with global configuration only, and returns
// nil only when exactly one header names the agent as primary or all. Every
// other outcome refuses. It checks the binary the request will run, so it stays
// per request rather than reading the model-discovery cache.
func openCodeAgentAvailable(bin, name string) error {
	refuse := func(reason string) error {
		return fmt.Errorf("the %s mode needs an OpenCode agent named %q in global OpenCode configuration (%s)", name, name, reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), openCodeAgentListTimeout)
	defer cancel()
	runner := boundedRunner{ctx: ctx, workDir: os.TempDir(), maxBytes: int64(openCodeAgentListMaxBytes),
		waitDelay: openCodeAgentListWaitDelay}
	cmd := exec.Command(bin, "agent", "list")
	cmd.Env = chatLaunchEnv(ChatRequest{})
	cmd.Env = append(cmd.Env, openCodeGlobalConfigOnly)
	out, err := runner.Run(cmd)
	if err != nil {
		switch modelDiscoveryReason(err) {
		case modelReasonTooLarge:
			return refuse("the agent list could not be read: output exceeded its bound")
		case modelReasonTimedOut:
			return refuse("the agent list could not be read: timed out")
		case modelReasonExited:
			return refuse("the agent list could not be read: exited with an error")
		case modelReasonWorkDirInRepo:
			return refuse("the agent list could not be read: the temporary directory is inside a repository")
		default:
			return refuse("the agent list could not be read: start failed")
		}
	}
	modes, listed := openCodeAgentModes(out, name)
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
	nested, _ := obj["part"].(map[string]any)
	part := nested
	if part == nil {
		part = obj
	}
	// The part id is the record identity OpenCode's stored transcript carries
	// too, so the console can skip the stored copy of what it drew live. Only
	// a real nested part has one.
	anchored := func(event ChatEvent) ChatEvent {
		if id := anyString(nested["id"]); id != "" {
			event["anchor"] = id
		}
		return event
	}
	switch typeName {
	case "step_start", "step-start":
		if id := anyString(obj["sessionID"]); id != "" {
			add(ChatEvent{"type": "session", "id": id})
		}
	case "text":
		add(anchored(ChatEvent{"type": "text", "text": anyString(part["text"])}))
	case "reasoning", "thinking":
		add(anchored(ChatEvent{"type": "thinking", "text": anyString(part["text"])}))
	case "tool", "tool_use":
		name := anyString(part["tool"])
		state, _ := part["state"].(map[string]any)
		input := part["input"]
		if state != nil && input == nil {
			input = state["input"]
		}
		add(anchored(ChatEvent{"type": "tool", "name": name, "text": truncate(compactJSON(input), 600)}))
		if state != nil {
			result := anyString(state["output"])
			isError := false
			if result == "" {
				result = anyString(state["error"])
				isError = result != ""
			}
			if result != "" {
				add(anchored(ChatEvent{"type": "tool_result", "text": truncate(result, 600), "is_error": isError}))
			}
		}
	case "tool_result":
		isError, _ := part["is_error"].(bool)
		add(ChatEvent{"type": "tool_result", "text": truncate(anyString(part["text"]), 600), "is_error": isError})
	case "step_finish", "step-finish":
		add(usageEvent(openCodeStepUsage(obj, part)))
	case "error":
		message := anyString(obj["message"])
		if message == "" {
			message = openCodeErrorText(obj["error"])
			if message == "" {
				message = openCodeErrorText(part["error"])
			}
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

// DiscoverChatModels lists the models OpenCode reports as usable under global
// configuration (plan §2.2). Everything OpenCode-shaped stays here: the
// command, its switches, the header-then-object output of --verbose (1.18.0),
// the provider/model id grammar, and zero meaning "stated as zero" (D-6).
func (openCodeChatDriver) DiscoverChatModels(_ context.Context, env ChatModelEnv) (ChatModelDiscovery, error) {
	discovery := ChatModelDiscovery{Scope: "OpenCode global configuration (project configuration and plugins excluded)"}
	bin, err := guardcli.ResolveRuntimeBinary("opencode", "")
	if err != nil {
		return discovery, modelDiscoveryFailure(modelReasonNotInstalled)
	}
	discovery.Binary = bin
	cmd := exec.Command(bin, "models", "--verbose", "--pure")
	cmd.Env = chatLaunchEnv(ChatRequest{}) // a model listing: no ticket
	cmd.Env = append(cmd.Env, openCodeGlobalConfigOnly, "OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1")
	out, err := env.Run(cmd)
	if err != nil {
		return discovery, err
	}
	discovery.Models, discovery.Rejected, err = parseOpenCodeVerboseModels(out)
	return discovery, err
}

// openCodeVerboseModel declares only the fields the adapter maps. The api,
// headers and options objects, which can hold credentials, are skipped by the
// decoder and never kept.
type openCodeVerboseModel struct {
	Variants   map[string]json.RawMessage `json:"variants"`
	ID         string                     `json:"id"`
	ProviderID string                     `json:"providerID"`
	Name       string                     `json:"name"`
	Limit      struct {
		Context int64 `json:"context"`
		Input   int64 `json:"input"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	Cost *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
		Cache  struct {
			Read  float64 `json:"read"`
			Write float64 `json:"write"`
		} `json:"cache"`
	} `json:"cost"`
	Capabilities struct {
		Reasoning *bool           `json:"reasoning"`
		Input     map[string]bool `json:"input"`
	} `json:"capabilities"`
}

// parseOpenCodeVerboseModels reads `<provider>/<model>` header lines, each
// followed by one pretty-printed JSON object that ends at a "}" line. A header
// whose object does not decode, or does not name the header's model, is
// counted as rejected; output that is not this shape at all is unparseable.
func parseOpenCodeVerboseModels(out []byte) ([]ChatModelOption, int, error) {
	lines := strings.Split(string(out), "\n")
	models, rejected := []ChatModelOption{}, 0
	for i := 0; i < len(lines); i++ {
		header := strings.TrimSpace(lines[i])
		if header == "" {
			continue
		}
		if strings.HasPrefix(header, "{") || strings.HasPrefix(header, "}") || !strings.Contains(header, "/") ||
			i+1 >= len(lines) || strings.TrimRight(lines[i+1], "\r") != "{" {
			return nil, 0, modelDiscoveryFailure(modelReasonUnparseable)
		}
		end := i + 1
		for end < len(lines) && strings.TrimRight(lines[end], "\r") != "}" {
			end++
		}
		if end == len(lines) {
			return nil, 0, modelDiscoveryFailure(modelReasonUnparseable)
		}
		var raw openCodeVerboseModel
		if err := json.Unmarshal([]byte(strings.Join(lines[i+1:end+1], "\n")), &raw); err != nil ||
			raw.ProviderID+"/"+raw.ID != header {
			rejected++
			i = end
			continue
		}
		models = append(models, openCodeModelOption(header, raw))
		i = end
	}
	if len(models) == 0 && rejected == 0 {
		return nil, 0, modelDiscoveryFailure(modelReasonUnparseable)
	}
	return models, rejected, nil
}

func openCodeModelOption(id string, raw openCodeVerboseModel) ChatModelOption {
	label := raw.Name
	if label == "" {
		label = raw.ID
	}
	option := ChatModelOption{ID: id, Label: label, Source: chatModelSourceRuntime,
		Group: raw.ProviderID, GroupLabel: raw.ProviderID, Effort: openCodeEffort(raw.Variants, raw.Capabilities.Reasoning)}
	positive := func(value int64) *int64 {
		if value <= 0 {
			return nil // OpenCode writes 0 for a limit it does not know
		}
		return &value
	}
	if limits := (ChatModelLimits{ContextTokens: positive(raw.Limit.Context), InputTokens: positive(raw.Limit.Input),
		OutputTokens: positive(raw.Limit.Output)}); limits != (ChatModelLimits{}) {
		option.Limits = &limits
	}
	for _, kind := range []taskinput.Kind{taskinput.KindText, taskinput.KindImage} {
		if raw.Capabilities.Input[string(kind)] {
			option.Inputs = append(option.Inputs, string(kind))
		}
	}
	if cost := raw.Cost; cost != nil {
		// D-6: OpenCode's stated rates, zero included; USD per 1M tokens.
		option.Price = &ChatModelPrice{Unit: "USD", PerTokens: 1_000_000, Rates: []ChatModelRate{
			{Class: "input", Amount: cost.Input}, {Class: "output", Amount: cost.Output},
			{Class: "cache-read", Amount: cost.Cache.Read}, {Class: "cache-write", Amount: cost.Cache.Write}}}
	}
	return option
}

// openCodeStepUsage maps one step_finish into the neutral classes. Measured on
// 1.18.0 (plan §4 step 0): each step reports only its own tokens (additive),
// and OpenCode counts reasoning apart from output, so the neutral output is
// their sum. Occupancy is this one call's input plus cache. Run events carry no
// model id. Cost is OpenCode's stated figure, zero included (D-6).
func openCodeStepUsage(obj, part map[string]any) ChatUsage {
	tokens, _ := part["tokens"].(map[string]any)
	cache, _ := tokens["cache"].(map[string]any)
	input, output, reasoning := statedCount(tokens["input"]), statedCount(tokens["output"]), statedCount(tokens["reasoning"])
	cacheRead, cacheWrite := statedCount(cache["read"]), statedCount(cache["write"])
	usage := ChatUsage{Accumulation: usageAdditive, TokenClasses: harvest.TokenClasses{Input: input,
		CacheRead: cacheRead, CacheWrite: cacheWrite, Output: sumStated(output, reasoning), Reasoning: reasoning},
		ContextUsed: sumStated(input, cacheRead, cacheWrite)}
	cost := obj["cost"]
	if cost == nil {
		cost = part["cost"]
	}
	if amount, ok := statedAmount(cost); ok {
		usage.Cost = &harvest.Cost{Amount: amount, Unit: "USD", Basis: harvest.CostBasisRuntime}
	}
	return usage
}
