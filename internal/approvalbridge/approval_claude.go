package approvalbridge

import (
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
	"time"

	"crossing-guard/internal/approvalchoice"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/mcpstdio"
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
	Action           string
	Targets          []string
	ApprovalReason   string
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

// bridge is the approval tool behind the shared stdio loop.
type bridge struct {
	options   Options
	requester Requester
}

func Run(ctx context.Context, input io.Reader, output io.Writer, options Options, requester Requester) error {
	if options.TaskID == "" || requester == nil {
		return errors.New("task identity and approval requester are required")
	}
	if options.Timeout <= 0 || options.Timeout > 10*time.Minute {
		options.Timeout = defaultTimeout
	}
	return mcpstdio.Run(ctx, input, output, &bridge{options: options, requester: requester}, maxMCPMessage)
}

func (b *bridge) Info() mcpstdio.ServerInfo {
	return mcpstdio.ServerInfo{Name: "crossing-guard-approval", Version: "1"}
}

func (b *bridge) Tools() []mcpstdio.Tool {
	return []mcpstdio.Tool{{
		Name:        ToolName,
		Description: "Request a Crossing Guard approval decision for one Claude tool call.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_name":   map[string]string{"type": "string"},
				"input":       map[string]string{"type": "object"},
				"tool_use_id": map[string]string{"type": "string"},
			},
			"required":             []string{"tool_name", "input"},
			"additionalProperties": true,
		},
	}}
}

// Prepare validates the call before the loop's duplicate-id check, as the bridge
// always has: a malformed call is denied without ever reaching the requester.
func (b *bridge) Prepare(call mcpstdio.Call) (func(context.Context) mcpstdio.Reply, *mcpstdio.Reply) {
	if call.Name != ToolName {
		return nil, &mcpstdio.Reply{Err: &mcpstdio.Error{Code: -32602, Message: "invalid approval tool call"}}
	}
	toolName, toolCallID, toolInput, err := parseToolArguments(call.Arguments, call.ID)
	if err != nil {
		return nil, &mcpstdio.Reply{Result: permissionDecisionResult("deny", nil, err.Error())}
	}
	// A question tool is the one Claude shape this bridge translates. Prompts are a
	// bounded projection for the approver; the exact input never leaves this process.
	prompts, questionTexts := projectChoicePrompts(toolName, toolInput)
	return func(ctx context.Context) mcpstdio.Reply {
		summary := displaySummary(toolName, toolInput)
		result, requestErr := b.requester.RequestApproval(ctx, ApprovalRequest{
			Runtime: "claude", TaskID: b.options.TaskID,
			CatalogSessionID: b.options.CatalogSessionID,
			NativeSessionID:  b.options.NativeSessionID,
			ToolCallID:       toolCallID, ToolName: toolName, Summary: summary,
			Action: "Use the “" + toolName + "” tool", Targets: approvalTargets(toolInput),
			ApprovalReason: "Claude requires approval before it can run this tool.",
			Prompts:        prompts, Timeout: b.options.Timeout,
		})
		if requestErr != nil {
			// Naming the boundary that failed is the difference between a session
			// that can report the problem and one that only knows it was denied.
			// The full error also goes to stderr, which the runtime captures.
			fmt.Fprintf(os.Stderr, "[crossing-guard] approval request failed for %s: %v\n", toolName, requestErr)
			return mcpstdio.Reply{Result: permissionDecisionResult("deny", nil,
				"Approval service unavailable ("+approvalErrorClass(requestErr)+"); denied fail-closed.")}
		}
		if result.Decision == "allowed" {
			return mcpstdio.Reply{Result: permissionDecisionResult("allow",
				answeredInput(toolInput, prompts, questionTexts, result), "")}
		}
		message := "The tool request was denied."
		if result.Decision == "expired" {
			message = "The approval deadline expired; denied fail-closed."
		} else if strings.TrimSpace(result.Reason) != "" {
			message = "The tool request was denied by the user."
		}
		return mcpstdio.Reply{Result: permissionDecisionResult("deny", nil, message)}
	}, nil
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

// approvalTargets selects the concrete scalar a person is most likely deciding
// about. The complete bounded input remains available under Raw held call.
func approvalTargets(input json.RawMessage) []string {
	var values map[string]json.RawMessage
	if json.Unmarshal(input, &values) != nil {
		return nil
	}
	for _, key := range []string{"command", "file_path", "filePath", "path", "url", "query"} {
		var value string
		if json.Unmarshal(values[key], &value) == nil && strings.TrimSpace(value) != "" {
			return []string{strings.TrimSpace(value)}
		}
	}
	return nil
}

func rpcIDKey(id json.RawMessage) string { return strings.TrimSpace(string(id)) }

// permissionDecisionResult is the text result Claude's --permission-prompt-tool reads.
func permissionDecisionResult(behavior string, updatedInput json.RawMessage, message string) map[string]any {
	decision := map[string]any{"behavior": behavior}
	if behavior == "allow" {
		decision["updatedInput"] = updatedInput
	} else {
		decision["message"] = message
	}
	encoded, _ := json.Marshal(decision)
	return map[string]any{"content": []any{map[string]any{
		"type": "text", "text": string(encoded),
	}}}
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
			Summary: request.Summary, Action: request.Action, Targets: request.Targets,
			ApprovalReason: request.ApprovalReason, Prompts: request.Prompts, Timeout: request.Timeout,
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
