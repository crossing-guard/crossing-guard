package approvalbridge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"crossing-guard/internal/approvalchoice"
	"crossing-guard/internal/guardcli"
)

const (
	Command           = "internal-claude-approval-mcp"
	ToolName          = "approval_prompt"
	maxMCPMessage     = 256 << 10
	maxToolInput      = 128 << 10
	maxDisplaySummary = 7 << 10
	defaultTimeout    = 5 * time.Minute
)

const Usage = `
  internal-claude-approval-mcp
      provider-owned stdio permission bridge used by GUI-launched Claude tasks.
`

type Options struct {
	TaskID           string
	DaemonDataDir    string
	CatalogSessionID string
	NativeSessionID  string
	Timeout          time.Duration
}

type ApprovalRequest struct {
	Runtime          string
	TaskID           string
	CatalogSessionID string
	NativeSessionID  string
	ToolCallID       string
	ToolName         string
	Summary          string
	Prompts          []approvalchoice.ChoicePrompt
	Timeout          time.Duration
}

type ApprovalResult struct {
	Decision            string
	Reason              string
	Selections          []approvalchoice.ChoiceSelection
	PromptsCompleteness string
}

type Requester interface {
	RequestApproval(context.Context, ApprovalRequest) (ApprovalResult, error)
}

type RequesterFunc func(context.Context, ApprovalRequest) (ApprovalResult, error)

func (fn RequesterFunc) RequestApproval(ctx context.Context, request ApprovalRequest) (ApprovalResult, error) {
	return fn(ctx, request)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type server struct {
	options   Options
	requester Requester
	out       io.Writer
	writeMu   sync.Mutex
	mu        sync.Mutex
	inflight  map[string]context.CancelFunc
	wg        sync.WaitGroup
}

func Run(ctx context.Context, input io.Reader, output io.Writer, options Options, requester Requester) error {
	if options.TaskID == "" || requester == nil {
		return errors.New("task identity and approval requester are required")
	}
	if options.Timeout <= 0 || options.Timeout > 10*time.Minute {
		options.Timeout = defaultTimeout
	}
	s := &server{options: options, requester: requester, out: output,
		inflight: make(map[string]context.CancelFunc)}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), maxMCPMessage)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var request rpcRequest
		if err := json.Unmarshal(line, &request); err != nil || request.JSONRPC != "2.0" || request.Method == "" {
			s.writeError(nil, -32700, "invalid JSON-RPC request")
			continue
		}
		if request.Method == "notifications/cancelled" {
			s.cancelRequest(request.Params)
			continue
		}
		if len(request.ID) == 0 {
			// MCP lifecycle notifications carry no response.
			continue
		}
		s.handle(ctx, request)
	}
	s.cancelAll()
	s.wg.Wait()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP request: %w", err)
	}
	return nil
}

func (s *server) handle(parent context.Context, request rpcRequest) {
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2025-06-18"
		}
		s.writeResult(request.ID, map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "crossing-guard-approval", "version": "1"},
		})
	case "ping":
		s.writeResult(request.ID, map[string]any{})
	case "tools/list":
		s.writeResult(request.ID, map[string]any{"tools": []any{map[string]any{
			"name":        ToolName,
			"description": "Request a Crossing Guard approval decision for one Claude tool call.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool_name":   map[string]string{"type": "string"},
					"input":       map[string]string{"type": "object"},
					"tool_use_id": map[string]string{"type": "string"},
				},
				"required":             []string{"tool_name", "input"},
				"additionalProperties": true,
			},
		}}})
	case "tools/call":
		s.startToolCall(parent, request)
	default:
		s.writeError(request.ID, -32601, "unsupported MCP method")
	}
}

func (s *server) startToolCall(parent context.Context, request rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil || params.Name != ToolName {
		s.writeError(request.ID, -32602, "invalid approval tool call")
		return
	}
	toolName, toolCallID, toolInput, err := parseToolArguments(params.Arguments, request.ID)
	if err != nil {
		s.writePermissionDecision(request.ID, "deny", nil, err.Error())
		return
	}
	// A question tool is the one Claude shape this bridge translates. Prompts are a
	// bounded projection for the approver; the exact input never leaves this process.
	prompts, questionTexts := projectChoicePrompts(toolName, toolInput)
	key := rpcIDKey(request.ID)
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	if _, exists := s.inflight[key]; exists {
		s.mu.Unlock()
		cancel()
		s.writeError(request.ID, -32600, "duplicate JSON-RPC id")
		return
	}
	s.inflight[key] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, key)
			s.mu.Unlock()
		}()
		summary := displaySummary(toolName, toolInput)
		result, requestErr := s.requester.RequestApproval(ctx, ApprovalRequest{
			Runtime: "claude", TaskID: s.options.TaskID,
			CatalogSessionID: s.options.CatalogSessionID,
			NativeSessionID:  s.options.NativeSessionID,
			ToolCallID:       toolCallID, ToolName: toolName, Summary: summary,
			Prompts: prompts, Timeout: s.options.Timeout,
		})
		if requestErr != nil {
			// Naming the boundary that failed is the difference between a session
			// that can report the problem and one that only knows it was denied.
			// The full error also goes to stderr, which the runtime captures.
			fmt.Fprintf(os.Stderr, "[crossing-guard] approval request failed for %s: %v\n", toolName, requestErr)
			s.writePermissionDecision(request.ID, "deny", nil,
				"Approval service unavailable ("+approvalErrorClass(requestErr)+"); denied fail-closed.")
			return
		}
		if result.Decision == "allowed" {
			s.writePermissionDecision(request.ID, "allow",
				answeredInput(toolInput, prompts, questionTexts, result), "")
			return
		}
		message := "The tool request was denied."
		if result.Decision == "expired" {
			message = "The approval deadline expired; denied fail-closed."
		} else if strings.TrimSpace(result.Reason) != "" {
			message = "The tool request was denied by the user."
		}
		s.writePermissionDecision(request.ID, "deny", nil, message)
	}()
}

func parseToolArguments(raw json.RawMessage, rpcID json.RawMessage) (string, string, json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > maxToolInput {
		return "", "", nil, errors.New("approval input was missing or too large; denied fail-closed")
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", "", nil, errors.New("approval input was malformed; denied fail-closed")
	}
	stringValue := func(names ...string) string {
		for _, name := range names {
			var value string
			if json.Unmarshal(args[name], &value) == nil && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	toolName := stringValue("tool_name", "toolName")
	toolCallID := stringValue("tool_use_id", "toolUseID", "tool_call_id")
	if toolCallID == "" {
		toolCallID = rpcIDKey(rpcID)
	}
	toolInput := args["input"]
	if len(toolInput) == 0 {
		toolInput = args["tool_input"]
	}
	if toolName == "" || len(toolInput) == 0 || len(toolInput) > maxToolInput || !json.Valid(toolInput) {
		return "", "", nil, errors.New("approval input was incomplete or too large; denied fail-closed")
	}
	return toolName, toolCallID, append(json.RawMessage(nil), toolInput...), nil
}

func displaySummary(toolName string, input json.RawMessage) string {
	full := []byte(`{"tool":` + strconv.Quote(toolName) + `,"input":` + string(input) + `}`)
	if len(full) <= maxDisplaySummary {
		return string(full)
	}
	digest := sha256.Sum256(input)
	return fmt.Sprintf(`{"tool":%s,"input_preview":%s,"input_sha256":%q,"truncated":true}`,
		strconv.Quote(toolName), strconv.Quote(string(input[:maxDisplaySummary/2])), hex.EncodeToString(digest[:]))
}

func (s *server) cancelRequest(params json.RawMessage) {
	var payload struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &payload) != nil || len(payload.RequestID) == 0 {
		return
	}
	s.mu.Lock()
	cancel := s.inflight[rpcIDKey(payload.RequestID)]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *server) cancelAll() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.inflight))
	for _, cancel := range s.inflight {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func rpcIDKey(id json.RawMessage) string { return strings.TrimSpace(string(id)) }

func (s *server) writePermissionDecision(id json.RawMessage, behavior string, updatedInput json.RawMessage, message string) {
	decision := map[string]any{"behavior": behavior}
	if behavior == "allow" {
		decision["updatedInput"] = updatedInput
	} else {
		decision["message"] = message
	}
	encoded, _ := json.Marshal(decision)
	s.writeResult(id, map[string]any{"content": []any{map[string]any{
		"type": "text", "text": string(encoded),
	}}})
}

func (s *server) writeResult(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) writeError(id json.RawMessage, code int, message string) {
	payload := map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message}}
	if len(id) == 0 {
		payload["id"] = nil
	}
	s.write(payload)
}

func (s *server) write(payload any) {
	encoded, _ := json.Marshal(payload)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.out.Write(append(encoded, '\n'))
}

// Main runs the installed hidden stdio bridge command.
func Main(args []string) int {
	flags := flag.NewFlagSet(Command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options Options
	var timeoutMS int
	flags.StringVar(&options.TaskID, "task-id", "", "")
	flags.StringVar(&options.DaemonDataDir, "daemon-data-dir", "", "")
	flags.StringVar(&options.CatalogSessionID, "catalog-session-id", "", "")
	flags.StringVar(&options.NativeSessionID, "native-session-id", "", "")
	flags.IntVar(&timeoutMS, "timeout-ms", int(defaultTimeout.Milliseconds()), "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || options.TaskID == "" || options.DaemonDataDir == "" {
		return 2
	}
	options.Timeout = time.Duration(timeoutMS) * time.Millisecond
	requester := RequesterFunc(func(ctx context.Context, request ApprovalRequest) (ApprovalResult, error) {
		result, err := guardcli.RequestRuntimeApprovalFromDataDir(ctx, options.DaemonDataDir, guardcli.RuntimeApprovalRequest{
			Runtime: request.Runtime, TaskID: request.TaskID,
			CatalogSessionID: request.CatalogSessionID, NativeSessionID: request.NativeSessionID,
			ToolCallID: request.ToolCallID, ToolName: request.ToolName,
			Summary: request.Summary, Prompts: request.Prompts, Timeout: request.Timeout,
		})
		return ApprovalResult{Decision: result.Decision, Reason: result.Reason,
			Selections: result.Selections, PromptsCompleteness: result.PromptsCompleteness}, err
	})
	if err := Run(context.Background(), os.Stdin, os.Stdout, options, requester); err != nil {
		fmt.Fprintln(os.Stderr, "crossing-guard approval bridge:", err)
		return 1
	}
	return 0
}
