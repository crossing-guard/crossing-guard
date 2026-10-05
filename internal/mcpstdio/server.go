// Package mcpstdio is the one newline-delimited JSON-RPC loop behind every stdio
// MCP server Crossing Guard runs (recall-mcp-v1-plan §3.5). It owns the protocol:
// initialize, ping, tools/list, tools/call dispatch, cancellation and the line cap.
// A Server owns only what its tools mean.
package mcpstdio

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// defaultProtocolVersion answers a client that names none.
const defaultProtocolVersion = "2025-06-18"

// ServerInfo is what initialize reports about the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool is one tools/list entry. Annotations are the MCP behavior hints
// (readOnlyHint and friends); nil omits them.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
	Annotations any    `json:"annotations,omitempty"`
}

// Call is one tools/call request. Meta is the request's params._meta, which some
// clients use to say which session and turn the call belongs to.
type Call struct {
	ID        json.RawMessage
	Name      string
	Arguments json.RawMessage
	Meta      json.RawMessage
}

// Error is a JSON-RPC error reply.
type Error struct {
	Code    int
	Message string
}

// Reply answers one tools/call: exactly one of Result or Err.
type Reply struct {
	Result any
	Err    *Error
}

// Server is what a stdio MCP server supplies.
type Server interface {
	Info() ServerInfo
	Tools() []Tool
	// Prepare checks one tools/call on the read loop, before the duplicate-id
	// check. A non-nil immediate reply is sent without starting work; otherwise
	// work runs concurrently under a context that notifications/cancelled and
	// end of input cancel.
	Prepare(call Call) (work func(context.Context) Reply, immediate *Reply)
}

// TextResult is the tools/call result carrying one text block.
func TextResult(text string, isError bool) map[string]any {
	result := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	if isError {
		result["isError"] = true
	}
	return result
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type loop struct {
	server   Server
	out      io.Writer
	writeMu  sync.Mutex
	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	wg       sync.WaitGroup
}

// Run serves one client until input ends, then cancels and waits for every
// call in flight. maxMessage bounds one request line.
func Run(ctx context.Context, input io.Reader, output io.Writer, server Server, maxMessage int) error {
	if maxMessage <= 0 {
		return fmt.Errorf("maxMessage must be positive, got %d", maxMessage)
	}
	l := &loop{server: server, out: output, inflight: make(map[string]context.CancelFunc)}
	scanner := bufio.NewScanner(input)
	// The cap is the larger of maxMessage and the initial buffer, so the buffer
	// must not start above the cap.
	scanner.Buffer(make([]byte, min(64<<10, maxMessage)), maxMessage)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var request rpcRequest
		if err := json.Unmarshal(line, &request); err != nil || request.JSONRPC != "2.0" || request.Method == "" {
			l.writeError(nil, -32700, "invalid JSON-RPC request")
			continue
		}
		if request.Method == "notifications/cancelled" {
			l.cancelRequest(request.Params)
			continue
		}
		if len(request.ID) == 0 {
			// MCP lifecycle notifications carry no response.
			continue
		}
		l.handle(ctx, request)
	}
	l.cancelAll()
	l.wg.Wait()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP request: %w", err)
	}
	return nil
}

func (l *loop) handle(parent context.Context, request rpcRequest) {
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = defaultProtocolVersion
		}
		l.writeResult(request.ID, map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      l.server.Info(),
		})
	case "ping":
		l.writeResult(request.ID, map[string]any{})
	case "tools/list":
		l.writeResult(request.ID, map[string]any{"tools": l.server.Tools()})
	case "tools/call":
		l.startToolCall(parent, request)
	default:
		l.writeError(request.ID, -32601, "unsupported MCP method")
	}
}

func (l *loop) startToolCall(parent context.Context, request rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      json.RawMessage `json:"_meta"`
	}
	call := Call{ID: request.ID}
	if err := json.Unmarshal(request.Params, &params); err == nil {
		call.Name, call.Arguments, call.Meta = params.Name, params.Arguments, params.Meta
	}
	work, immediate := l.server.Prepare(call)
	if immediate != nil {
		l.writeReply(request.ID, *immediate)
		return
	}
	if work == nil {
		l.writeError(request.ID, -32603, "the server prepared no work for this call")
		return
	}
	key := rpcIDKey(request.ID)
	ctx, cancel := context.WithCancel(parent)
	l.mu.Lock()
	if _, exists := l.inflight[key]; exists {
		l.mu.Unlock()
		cancel()
		l.writeError(request.ID, -32600, "duplicate JSON-RPC id")
		return
	}
	l.inflight[key] = cancel
	l.mu.Unlock()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer cancel()
		defer func() {
			l.mu.Lock()
			delete(l.inflight, key)
			l.mu.Unlock()
		}()
		l.writeReply(request.ID, work(ctx))
	}()
}

func (l *loop) cancelRequest(params json.RawMessage) {
	var payload struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &payload) != nil || len(payload.RequestID) == 0 {
		return
	}
	l.mu.Lock()
	cancel := l.inflight[rpcIDKey(payload.RequestID)]
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (l *loop) cancelAll() {
	l.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(l.inflight))
	for _, cancel := range l.inflight {
		cancels = append(cancels, cancel)
	}
	l.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func rpcIDKey(id json.RawMessage) string { return strings.TrimSpace(string(id)) }

func (l *loop) writeReply(id json.RawMessage, reply Reply) {
	switch {
	case reply.Err != nil:
		l.writeError(id, reply.Err.Code, reply.Err.Message)
	case reply.Result == nil:
		l.writeError(id, -32603, "the server returned neither a result nor an error")
	default:
		l.writeResult(id, reply.Result)
	}
}

func (l *loop) writeResult(id json.RawMessage, result any) {
	l.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (l *loop) writeError(id json.RawMessage, code int, message string) {
	payload := map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message}}
	if len(id) == 0 {
		payload["id"] = nil
	}
	l.write(payload)
}

func (l *loop) write(payload any) {
	encoded, _ := json.Marshal(payload)
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	_, _ = l.out.Write(append(encoded, '\n'))
}
