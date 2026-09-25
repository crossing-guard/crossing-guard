package daemon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"crossing-guard/internal/taskinput"
)

func TestParseClaudeAuthStatusAcceptsLoggedOutJSONWithNonzeroExit(t *testing.T) {
	ready, method, provider, err := parseClaudeAuthStatus(
		[]byte(`{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`),
		errors.New("exit status 1"),
	)
	if err != nil || ready || method != "none" || provider != "firstParty" {
		t.Fatalf("got ready=%v method=%q provider=%q err=%v", ready, method, provider, err)
	}
}

func TestClaudeCommandInjectsTaskScopedApprovalBridge(t *testing.T) {
	bin := testChatExecutable(t)
	dataDir := t.TempDir()
	cmd, err := claudeChatDriver{}.BuildCmd(ChatRequest{
		Binary: bin, Cwd: t.TempDir(), Prompt: "continue",
		SessionID: "native_1", CatalogSessionID: "catalog_1",
	}, ChatLaunchContext{TaskID: "task_0123456789abcdef", DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	valueAfter := func(flag string) string {
		for index, arg := range cmd.Args {
			if arg == flag && index+1 < len(cmd.Args) {
				return cmd.Args[index+1]
			}
		}
		return ""
	}
	tool := valueAfter("--permission-prompt-tool")
	if tool != "mcp__cg_approval_task_0123456789abcdef__approval_prompt" ||
		valueAfter("--allowedTools") != tool {
		t.Fatalf("permission tool args = %q", cmd.Args)
	}
	var config struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(valueAfter("--mcp-config")), &config); err != nil {
		t.Fatal(err)
	}
	server, ok := config.MCPServers["cg_approval_task_0123456789abcdef"]
	if !ok || server.Command == "" {
		t.Fatalf("MCP config = %#v", config)
	}
	joined := strings.Join(server.Args, "\x00")
	for _, expected := range []string{"internal-claude-approval-mcp", "task_0123456789abcdef", dataDir, "catalog_1", "native_1"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("bridge args %q missing %q", server.Args, expected)
		}
	}
}

func TestClaudeRejectsConflictingRawPermissionArgs(t *testing.T) {
	for _, raw := range []string{
		"--permission-prompt-tool other", "--mcp-config={}", "--strict-mcp-config",
		"--allowedTools Bash", "--permission-mode bypassPermissions",
		"--dangerously-skip-permissions",
	} {
		if _, err := (claudeChatDriver{}).CanonicalizeChatRequest(ChatRequest{ExtraArgs: raw}); err == nil {
			t.Fatalf("conflicting extra args accepted: %q", raw)
		}
	}
}

func TestClaudeMapsOnlyOpaqueAttachmentsAndEscapesUserMentions(t *testing.T) {
	driver := claudeChatDriver{}
	inputs := []taskinput.ResolvedInput{
		{Input: taskinput.Input{Kind: taskinput.KindImage}, Path: "/private/opaque one/input.png"},
		{Input: taskinput.Input{Kind: taskinput.KindText}, Path: "/private/opaque/two.txt"},
	}
	if err := driver.ValidateChatInputs(ChatRequest{}, inputs); err != nil {
		t.Fatal(err)
	}
	cmd, err := driver.BuildCmd(ChatRequest{Binary: testChatExecutable(t), Cwd: t.TempDir(), Prompt: "ask @project/secret"},
		ChatLaunchContext{TaskID: "task_0123456789abcdef", DataDir: t.TempDir(), Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	prompt := ""
	for index, arg := range cmd.Args {
		if arg == "-p" && index+1 < len(cmd.Args) {
			prompt = cmd.Args[index+1]
		}
	}
	if !strings.Contains(prompt, `@/private/opaque\ one/input.png`) || !strings.Contains(prompt, "@/private/opaque/two.txt") ||
		!strings.Contains(prompt, `ask \@project/secret`) || strings.Contains(prompt, "ask @project/secret") {
		t.Fatalf("Claude attachment prompt is unsafe: %q", prompt)
	}
}

func TestClaudeProjectsObservedThinkingEnvelopeWithoutOpaqueSignature(t *testing.T) {
	driver := claudeChatDriver{}
	emptyDelta := driver.ProjectEvent(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_delta", "delta": map[string]any{
			"type": "thinking_delta", "thinking": "",
		},
	}})
	emptyCompleted := driver.ProjectEvent(map[string]any{"type": "assistant", "message": map[string]any{
		"content": []any{map[string]any{"type": "thinking", "thinking": "", "signature": "opaque"}},
	}})
	for _, events := range [][]ChatEvent{emptyDelta, emptyCompleted} {
		if len(events) != 1 || events[0]["text"] != "" {
			t.Fatalf("empty thinking activity envelope changed: %+v", events)
		}
		if _, copied := events[0]["signature"]; copied {
			t.Fatalf("opaque thinking signature leaked into task event: %+v", events[0])
		}
	}

	delta := driver.ProjectEvent(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_delta", "delta": map[string]any{
			"type": "thinking_delta", "thinking": "  inspect evidence  ",
		},
	}})
	completed := driver.ProjectEvent(map[string]any{"type": "assistant", "message": map[string]any{
		"content": []any{map[string]any{"type": "thinking", "thinking": "final reasoning"}},
	}})
	if len(delta) != 1 || delta[0]["type"] != "thinking_delta" || delta[0]["text"] != "  inspect evidence  " {
		t.Fatalf("visible thinking delta changed: %+v", delta)
	}
	if len(completed) != 1 || completed[0]["type"] != "thinking" || completed[0]["text"] != "final reasoning" {
		t.Fatalf("visible thinking completion changed: %+v", completed)
	}
}

// The owned stream carries the vendor's record uuid on completed blocks and
// tool results; it is forwarded as "anchor" — the same identity the harvester
// publishes as the block's turn anchor — so the console can mark what it drew
// live. Streamed deltas have no record yet and carry none.
func TestClaudeForwardsTheRecordIdentityAsAnchor(t *testing.T) {
	driver := claudeChatDriver{}
	completed := driver.ProjectEvent(map[string]any{"type": "assistant", "uuid": "rec-1", "message": map[string]any{
		"content": []any{
			map[string]any{"type": "thinking", "thinking": ""},
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "ls"}},
		},
	}})
	if len(completed) != 3 {
		t.Fatalf("three blocks expected: %+v", completed)
	}
	for _, event := range completed {
		if event["anchor"] != "rec-1" {
			t.Fatalf("block %v must carry the record identity: %+v", event["type"], event)
		}
	}
	result := driver.ProjectEvent(map[string]any{"type": "user", "uuid": "rec-2", "message": map[string]any{
		"content": []any{map[string]any{"type": "tool_result", "content": "done"}},
	}})
	if len(result) != 1 || result[0]["anchor"] != "rec-2" {
		t.Fatalf("tool results carry their record identity too: %+v", result)
	}
	delta := driver.ProjectEvent(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "he"},
	}})
	if len(delta) != 1 || delta[0]["anchor"] != nil {
		t.Fatalf("a streamed chunk has no record yet and must carry no anchor: %+v", delta)
	}
}
