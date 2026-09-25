package approvalbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

type mcpHarness struct {
	input  *io.PipeWriter
	output *bufio.Reader
	done   chan error
}

func newMCPHarness(t *testing.T, requester Requester) *mcpHarness {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), inputReader, outputWriter,
			Options{TaskID: "task_0123456789abcdef", CatalogSessionID: "catalog_1",
				NativeSessionID: "native_1", Timeout: time.Second}, requester)
		_ = outputWriter.Close()
	}()
	h := &mcpHarness{input: inputWriter, output: bufio.NewReader(outputReader), done: done}
	t.Cleanup(func() {
		_ = inputWriter.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("MCP bridge did not stop")
		}
	})
	return h
}

func (h *mcpHarness) send(t *testing.T, payload any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.input.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (h *mcpHarness) receive(t *testing.T) map[string]any {
	t.Helper()
	line, err := h.output.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func permissionDecision(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result: %#v", response)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("missing content: %#v", response)
	}
	block, _ := content[0].(map[string]any)
	var decision map[string]any
	if err := json.Unmarshal([]byte(block["text"].(string)), &decision); err != nil {
		t.Fatal(err)
	}
	return decision
}

func TestMCPInitializeAndToolList(t *testing.T) {
	h := newMCPHarness(t, RequesterFunc(func(context.Context, ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{}, errors.New("not called")
	}))
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18"}})
	initialize := h.receive(t)
	result := initialize["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize response = %#v", initialize)
	}
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	listed := h.receive(t)
	tools := listed["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != ToolName {
		t.Fatalf("tool list = %#v", listed)
	}
}

func TestMCPAllowPreservesOriginalInputAndIdentity(t *testing.T) {
	requests := make(chan ApprovalRequest, 1)
	h := newMCPHarness(t, RequesterFunc(func(_ context.Context, request ApprovalRequest) (ApprovalResult, error) {
		requests <- request
		return ApprovalResult{Decision: "allowed"}, nil
	}))
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": "call-1", "method": "tools/call",
		"params": map[string]any{"name": ToolName, "arguments": map[string]any{
			"tool_name": "Bash", "tool_use_id": "toolu_1",
			"input": map[string]any{"command": "printf hello", "nested": map[string]any{"value": 7}},
		}}})
	decision := permissionDecision(t, h.receive(t))
	if decision["behavior"] != "allow" {
		t.Fatalf("decision = %#v", decision)
	}
	updated := decision["updatedInput"].(map[string]any)
	if updated["command"] != "printf hello" || updated["nested"].(map[string]any)["value"] != float64(7) {
		t.Fatalf("updated input = %#v", updated)
	}
	request := <-requests
	if request.Runtime != "claude" || request.TaskID != "task_0123456789abcdef" ||
		request.CatalogSessionID != "catalog_1" || request.NativeSessionID != "native_1" ||
		request.ToolCallID != "toolu_1" || request.ToolName != "Bash" {
		t.Fatalf("request identity = %#v", request)
	}
	if !json.Valid([]byte(request.Summary)) {
		t.Fatalf("summary is not JSON: %q", request.Summary)
	}
}

func TestMCPRequesterFailureDeniesFailClosed(t *testing.T) {
	h := newMCPHarness(t, RequesterFunc(func(context.Context, ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{}, errors.New("daemon unavailable")
	}))
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": ToolName, "arguments": map[string]any{
			"tool_name": "Write", "input": map[string]any{"file_path": "/tmp/x"},
		}}})
	decision := permissionDecision(t, h.receive(t))
	if decision["behavior"] != "deny" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestMCPDenyAndExpiryRemainDenied(t *testing.T) {
	for _, decision := range []string{"denied", "expired"} {
		t.Run(decision, func(t *testing.T) {
			h := newMCPHarness(t, RequesterFunc(func(context.Context, ApprovalRequest) (ApprovalResult, error) {
				return ApprovalResult{Decision: decision}, nil
			}))
			h.send(t, map[string]any{"jsonrpc": "2.0", "id": decision, "method": "tools/call",
				"params": map[string]any{"name": ToolName, "arguments": map[string]any{
					"tool_name": "Bash", "input": map[string]any{"command": "true"},
				}}})
			if got := permissionDecision(t, h.receive(t)); got["behavior"] != "deny" {
				t.Fatalf("decision = %#v", got)
			}
		})
	}
}

func TestMCPRejectsUnknownMethodToolAndMalformedArguments(t *testing.T) {
	called := make(chan struct{}, 1)
	h := newMCPHarness(t, RequesterFunc(func(context.Context, ApprovalRequest) (ApprovalResult, error) {
		called <- struct{}{}
		return ApprovalResult{}, errors.New("unexpected requester call")
	}))
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	if response := h.receive(t); response["error"] == nil {
		t.Fatalf("unknown method response = %#v", response)
	}
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "another_tool", "arguments": map[string]any{}}})
	if response := h.receive(t); response["error"] == nil {
		t.Fatalf("unknown tool response = %#v", response)
	}
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": ToolName, "arguments": map[string]any{"tool_name": "Bash"}}})
	if response := permissionDecision(t, h.receive(t)); response["behavior"] != "deny" {
		t.Fatalf("malformed argument response = %#v", response)
	}
	select {
	case <-called:
		t.Fatal("invalid protocol input reached requester")
	default:
	}
}

func TestMCPRejectsOversizedMessage(t *testing.T) {
	input := bytes.NewBuffer(bytes.Repeat([]byte{'x'}, maxMCPMessage+1))
	output := new(bytes.Buffer)
	err := Run(context.Background(), input, output, Options{TaskID: "task_oversize"},
		RequesterFunc(func(context.Context, ApprovalRequest) (ApprovalResult, error) {
			t.Fatal("oversized message reached requester")
			return ApprovalResult{}, nil
		}))
	if err == nil {
		t.Fatal("oversized message unexpectedly succeeded")
	}
}

func TestMCPCancellationCancelsMatchingApproval(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	h := newMCPHarness(t, RequesterFunc(func(ctx context.Context, _ ApprovalRequest) (ApprovalResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ApprovalResult{}, ctx.Err()
	}))
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": ToolName, "arguments": map[string]any{
			"tool_name": "Bash", "input": map[string]any{"command": "true"},
		}}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("approval did not start")
	}
	h.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 9}})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("approval context was not cancelled")
	}
	decision := permissionDecision(t, h.receive(t))
	if decision["behavior"] != "deny" {
		t.Fatalf("cancel decision = %#v", decision)
	}
}
