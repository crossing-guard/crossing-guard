package daemon

import (
	"context"
	"crossing-guard/internal/guardcli"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"crossing-guard/internal/approvalbridge"
	"crossing-guard/internal/taskinput"
)

func init() { registerChatDriver("claude", claudeChatDriver{}) }

// claudeChatDriver drives `claude -p` in stream-json mode.
type claudeChatDriver struct{}

func (claudeChatDriver) ChatCapability() ChatCapability {
	return ChatCapability{
		MessageDelivery: SessionMessageCapability{Supported: true, Boundary: "next hook boundary (prompt submit, tool call, or tool result)",
			Detail: "Delivered as hook-provided context by the installed Crossing Guard hook at the session's next boundary; works for terminal and console sessions alike. Requires the hook lane; an opted-out session lets the message expire."},
		Runtime: "claude", DisplayName: "Claude", CanStart: true, CanResume: true,
		CanSignIn: true, VendorAuthDefault: true, SupportsBaseURL: true,
		SupportsAuthToken: true, AcceptsCustomModel: true, ModelHint: "Claude model ID",
		Modes: []ChatMode{
			{ID: "", Label: "Default", Risk: "elevated", Description: "Claude permission prompts are held in the Crossing Guard approvals inbox."},
			{ID: "plan", Label: "Plan", Risk: "normal", Description: "Read-only planning mode."},
			{ID: "acceptEdits", Label: "Accept Edits", Risk: "elevated", Description: "File edits are approved; remaining Claude prompts use the approvals inbox."},
			{ID: "bypassPermissions", Label: "Bypass", Risk: "dangerous", Description: "Guarded tools run without Claude approval prompts. Trusted repositories only."},
		},
		Models: []ChatModelOption{
			{ID: "", Label: "Binary default", Description: "Send no model flag."},
			{ID: "sonnet", Label: "Sonnet"}, {ID: "opus", Label: "Opus"},
			{ID: "haiku", Label: "Haiku"}, {ID: "custom", Label: "Custom…", Custom: true},
		},
		Inputs: []ChatInputCapability{
			{Kind: "text", MediaTypes: []string{"text/plain"}, CanStart: true, CanResume: true,
				Note: "Delivered from the daemon's opaque private staging path."},
			{Kind: "image", MediaTypes: []string{"image/png"}, CanStart: true, CanResume: true,
				ModelConditional: true, Note: "Vision quality and support remain selected-model facts."},
		},
	}
}

func (claudeChatDriver) ValidateChatInputs(_ ChatRequest, inputs []taskinput.ResolvedInput) error {
	for _, input := range inputs {
		if input.Kind != taskinput.KindText && input.Kind != taskinput.KindImage {
			return fmt.Errorf("claude does not support task input kind %q", input.Kind)
		}
	}
	return nil
}

func (claudeChatDriver) CanonicalizeChatRequest(req ChatRequest) (ChatRequest, error) {
	if err := validateClaudeExtraArgs(req.ExtraArgs); err != nil {
		return ChatRequest{}, err
	}
	return req, nil
}

func validateClaudeExtraArgs(raw string) error {
	owned := []string{
		"--permission-prompt-tool", "--mcp-config", "--strict-mcp-config",
		"--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools", "--permission-mode",
		"--dangerously-skip-permissions",
	}
	for _, arg := range splitArgs(raw) {
		for _, flag := range owned {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return fmt.Errorf("extra_args cannot override Crossing Guard-owned Claude permission controls")
			}
		}
	}
	return nil
}

func claudeApprovalServerName(taskID string) (string, error) {
	if !strings.HasPrefix(taskID, "task_") || len(taskID) > 64 {
		return "", errors.New("invalid daemon task identity")
	}
	for _, char := range taskID {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return "", errors.New("invalid daemon task identity")
		}
	}
	return "cg_approval_" + taskID, nil
}

func claudeApprovalConfig(req ChatRequest, launch ChatLaunchContext) (string, string, error) {
	serverName, err := claudeApprovalServerName(launch.TaskID)
	if err != nil {
		return "", "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("resolve approval bridge executable: %w", err)
	}
	if strings.TrimSpace(launch.DataDir) == "" {
		return "", "", errors.New("daemon data directory is required for approval rendezvous")
	}
	config := map[string]any{"mcpServers": map[string]any{
		serverName: map[string]any{
			"type": "stdio", "command": executable,
			"args": []string{
				"internal-claude-approval-mcp",
				"--task-id", launch.TaskID,
				"--daemon-data-dir", launch.DataDir,
				"--catalog-session-id", req.CatalogSessionID,
				"--native-session-id", req.SessionID,
				"--timeout-ms", fmt.Sprint(activeApprovalsConfig().runtimeToolTimeout().Milliseconds()),
			},
		},
	}}
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", "", fmt.Errorf("encode approval bridge config: %w", err)
	}
	return string(encoded), "mcp__" + serverName + "__" + approvalbridge.ToolName, nil
}

func (claudeChatDriver) ProbeVendorAuth(ctx context.Context) (bool, string, string, error) {
	bin, err := guardcli.ResolveRuntimeBinary("claude", "")
	if err != nil {
		return false, "", "", err
	}
	out, runErr := exec.CommandContext(ctx, bin, "auth", "status", "--json").Output()
	return parseClaudeAuthStatus(out, runErr)
}

func parseClaudeAuthStatus(out []byte, runErr error) (bool, string, string, error) {
	var payload struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		if runErr != nil {
			return false, "", "", runErr
		}
		return false, "", "", errors.New("auth status returned an unknown response")
	}
	return payload.LoggedIn, payload.AuthMethod, payload.APIProvider, nil
}

func (claudeChatDriver) BuildVendorLogin(ctx context.Context) (*exec.Cmd, error) {
	bin, err := guardcli.ResolveRuntimeBinary("claude", "")
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, bin, "auth", "login"), nil
}

// BuildCmd: claude -p "<prompt>" --output-format stream-json --verbose
// (--verbose is required with stream-json in -p mode).
func (claudeChatDriver) BuildCmd(req ChatRequest, launch ChatLaunchContext) (*exec.Cmd, error) {
	if err := validateClaudeExtraArgs(req.ExtraArgs); err != nil {
		return nil, err
	}
	bin, err := guardcli.ResolveRuntimeBinary("claude", req.Binary)
	if err != nil {
		return nil, err
	}
	// Escape user-authored @ characters so only the daemon-owned opaque paths
	// appended here enter Claude's local file-expansion preprocessor.
	prompt := strings.ReplaceAll(req.Prompt, "@", `\@`)
	if len(launch.Inputs) > 0 {
		var attachments strings.Builder
		attachments.WriteString("Use these explicitly attached files in order:\n")
		for _, input := range launch.Inputs {
			attachments.WriteByte('@')
			attachments.WriteString(strings.ReplaceAll(input.Path, " ", `\ `))
			attachments.WriteByte('\n')
		}
		attachments.WriteString("\nUser request:\n")
		prompt = attachments.String() + prompt
	}
	// --include-partial-messages gives token-level text deltas for live streaming.
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	mode := req.Mode
	if mode == "" { // legacy API compatibility
		mode = req.PermissionMode
	}
	if mode != "" {
		args = append(args, "--permission-mode", mode)
	}
	mcpConfig, permissionTool, err := claudeApprovalConfig(req, launch)
	if err != nil {
		return nil, err
	}
	args = append(args,
		"--mcp-config", mcpConfig,
		"--permission-prompt-tool", permissionTool,
		"--allowedTools", permissionTool,
	)
	args = append(args, splitArgs(req.ExtraArgs)...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = req.Cwd
	cmd.Env = os.Environ()
	if req.BaseURL != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_BASE_URL="+req.BaseURL)
	}
	if req.AuthToken != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_AUTH_TOKEN="+req.AuthToken)
	}
	return cmd, nil
}

// ProjectEvent maps stream-json records to bounded, transport-neutral UI events.
func (claudeChatDriver) ProjectEvent(obj map[string]any) []ChatEvent {
	var out []ChatEvent
	add := func(event ChatEvent) { out = append(out, event) }
	switch obj["type"] {
	case "system":
		if obj["subtype"] == "init" {
			add(ChatEvent{
				"type": "session", "id": anyString(obj["session_id"]),
				"model": anyString(obj["model"]), "cwd": anyString(obj["cwd"]),
			})
		}
	case "stream_event":
		// token-level deltas (--include-partial-messages)
		ev, _ := obj["event"].(map[string]any)
		if ev == nil {
			return out
		}
		if ev["type"] == "content_block_delta" {
			if delta, ok := ev["delta"].(map[string]any); ok {
				switch delta["type"] {
				case "text_delta":
					add(ChatEvent{"type": "delta", "text": anyString(delta["text"])})
				case "thinking_delta":
					add(ChatEvent{"type": "thinking_delta", "text": anyString(delta["thinking"])})
				}
			}
		}
	case "assistant":
		msg, _ := obj["message"].(map[string]any)
		if msg == nil {
			return out
		}
		// The record uuid is the identity the transcript harvester publishes as
		// this block's turn anchor. Forwarded as "anchor" so the console can
		// mark what it drew live and the harvested copy is not drawn twice.
		anchor := anyString(obj["uuid"])
		if content, ok := msg["content"].([]any); ok {
			for _, item := range content {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				switch m["type"] {
				case "text":
					// final block text — client replaces the streamed bubble with this
					add(ChatEvent{"type": "text", "text": anyString(m["text"]), "anchor": anchor})
				case "thinking":
					add(ChatEvent{"type": "thinking", "text": anyString(m["thinking"]), "anchor": anchor})
				case "tool_use":
					input, _ := json.Marshal(m["input"])
					add(ChatEvent{"type": "tool", "name": anyString(m["name"]), "text": truncate(string(input), 600), "anchor": anchor})
				}
			}
		}
	case "user":
		// tool results echo back as user-role events in stream-json
		msg, _ := obj["message"].(map[string]any)
		if msg == nil {
			return out
		}
		if content, ok := msg["content"].([]any); ok {
			for _, item := range content {
				m, ok := item.(map[string]any)
				if !ok || m["type"] != "tool_result" {
					continue
				}
				// is_error must survive: a DENIED tool and a successful one are not the
				// same event, and dropping the flag made them render identically. The
				// commonest denial here is Claude Code's own permission gate, which in
				// headless `-p` mode auto-denies because there is no prompt to answer.
				isErr := false
				if b, ok := m["is_error"].(bool); ok {
					isErr = b
				}
				add(ChatEvent{"type": "tool_result", "is_error": isErr,
					"text": truncate(anyString(m["content"]), 600), "anchor": anyString(obj["uuid"])})
			}
		}
	case "result":
		add(ChatEvent{
			"type": "result", "id": anyString(obj["session_id"]),
			"cost": obj["total_cost_usd"], "ms": obj["duration_ms"],
			"is_error": obj["is_error"],
		})
	}
	return out
}
