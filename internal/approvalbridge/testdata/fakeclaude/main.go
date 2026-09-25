// Command fakeclaude is a compiled outer-boundary smoke fixture. It accepts the
// Claude launch shape, starts the injected MCP server from --mcp-config, requests
// one harmless tool approval, and emits Claude-compatible stream-json records.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type mcpConfig struct {
	Servers map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"mcpServers"`
}

func valueAfter(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func emit(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Println(string(encoded))
	return err
}

func run() error {
	rawConfig := valueAfter(os.Args[1:], "--mcp-config")
	permissionTool := valueAfter(os.Args[1:], "--permission-prompt-tool")
	allowedTools := valueAfter(os.Args[1:], "--allowedTools")
	if rawConfig == "" || permissionTool == "" || allowedTools != permissionTool ||
		!strings.HasSuffix(permissionTool, "__approval_prompt") {
		return errors.New("missing exact injected approval controls")
	}
	var config mcpConfig
	if err := json.Unmarshal([]byte(rawConfig), &config); err != nil || len(config.Servers) != 1 {
		return errors.New("invalid injected MCP config")
	}
	var server struct {
		Command string
		Args    []string
	}
	for _, configured := range config.Servers {
		server.Command, server.Args = configured.Command, configured.Args
	}
	if server.Command == "" {
		return errors.New("injected MCP command missing")
	}

	bridge := exec.Command(server.Command, server.Args...)
	bridge.Stderr = os.Stderr
	input, err := bridge.StdinPipe()
	if err != nil {
		return err
	}
	output, err := bridge.StdoutPipe()
	if err != nil {
		return err
	}
	if err := bridge.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(output)
	send := func(value any) (map[string]any, error) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if _, err := input.Write(append(encoded, '\n')); err != nil {
			return nil, err
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var response map[string]any
		if err := json.Unmarshal(line, &response); err != nil {
			return nil, err
		}
		return response, nil
	}
	defer func() {
		_ = input.Close()
		_ = bridge.Wait()
	}()

	if _, err := send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18"}}); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	if err := emit(map[string]any{"type": "system", "subtype": "init",
		"session_id": "native_fixture", "model": "fixture", "cwd": cwd}); err != nil {
		return err
	}
	toolInput := map[string]any{"command": "printf fixture-approved"}
	if err := emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "name": "Bash", "input": toolInput},
	}}}); err != nil {
		return err
	}
	response, err := send(map[string]any{"jsonrpc": "2.0", "id": "fixture-approval-1", "method": "tools/call",
		"params": map[string]any{"name": "approval_prompt", "arguments": map[string]any{
			"tool_name": "Bash", "tool_use_id": "fixture-tool-1", "input": toolInput,
		}}})
	if err != nil {
		return err
	}
	result, _ := response["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		return errors.New("approval bridge response missing content")
	}
	block, _ := content[0].(map[string]any)
	var decision map[string]any
	if json.Unmarshal([]byte(fmt.Sprint(block["text"])), &decision) != nil {
		return errors.New("approval bridge response malformed")
	}
	allowed := decision["behavior"] == "allow"
	message := "Fixture approval was denied."
	if allowed {
		message = "Fixture approval was allowed."
	}
	if err := emit(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "is_error": !allowed, "content": message},
	}}}); err != nil {
		return err
	}
	if err := emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "text", "text": message},
	}}}); err != nil {
		return err
	}
	return emit(map[string]any{"type": "result", "session_id": "native_fixture",
		"duration_ms": 1, "total_cost_usd": 0, "is_error": !allowed})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		os.Exit(1)
	}
}
