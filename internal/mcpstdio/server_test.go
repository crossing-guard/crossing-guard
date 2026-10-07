package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeServer struct {
	gotMeta chan json.RawMessage
}

func (f *fakeServer) Info() ServerInfo { return ServerInfo{Name: "fake", Version: "0"} }

func (f *fakeServer) Tools() []Tool {
	return []Tool{{Name: "echo", Description: "d", InputSchema: map[string]any{"type": "object"},
		Annotations: map[string]any{"readOnlyHint": true}}}
}

func (f *fakeServer) Prepare(call Call) (func(context.Context) Reply, *Reply) {
	switch call.Name {
	case "echo":
		return func(context.Context) Reply {
			if f.gotMeta != nil {
				f.gotMeta <- call.Meta
			}
			return Reply{Result: TextResult("args="+string(call.Arguments), false)}
		}, nil
	case "wait":
		return func(ctx context.Context) Reply {
			<-ctx.Done()
			return Reply{Result: TextResult("cancelled", true)}
		}, nil
	}
	return nil, &Reply{Err: &Error{Code: -32602, Message: "unknown tool"}}
}

func run(t *testing.T, server Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, server, 1<<20); err != nil {
		t.Fatal(err)
	}
	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var reply map[string]any
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			t.Fatalf("reply %q: %v", line, err)
		}
		replies = append(replies, reply)
	}
	return replies
}

func TestListCallAndMeta(t *testing.T) {
	server := &fakeServer{gotMeta: make(chan json.RawMessage, 1)}
	replies := run(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1},"_meta":{"threadId":"t-1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`,
	)
	if len(replies) != 5 {
		t.Fatalf("want 5 replies (the notification gets none), got %d: %v", len(replies), replies)
	}
	byID := map[float64]map[string]any{}
	for _, reply := range replies {
		byID[reply["id"].(float64)] = reply
	}
	if got := byID[1]["result"].(map[string]any)["protocolVersion"]; got != "2025-11-25" {
		t.Fatalf("initialize echoed %v", got)
	}
	tool := byID[2]["result"].(map[string]any)["tools"].([]any)[0].(map[string]any)
	if tool["inputSchema"] == nil || tool["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatalf("tools/list lost the schema or annotations: %v", tool)
	}
	text := byID[3]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != `args={"a":1}` {
		t.Fatalf("call text %v", text)
	}
	if meta := <-server.gotMeta; string(meta) != `{"threadId":"t-1"}` {
		t.Fatalf("meta %s", meta)
	}
	if code := byID[4]["error"].(map[string]any)["code"]; code != float64(-32602) {
		t.Fatalf("unknown tool code %v", code)
	}
	if code := byID[5]["error"].(map[string]any)["code"]; code != float64(-32601) {
		t.Fatalf("unknown method code %v", code)
	}
}

func TestCancelledCallIsAnsweredAndEndOfInputCancelsTheRest(t *testing.T) {
	replies := run(t, &fakeServer{},
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait"}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"wait"}}`,
	)
	if len(replies) != 2 {
		t.Fatalf("want both waits answered, got %v", replies)
	}
}

func TestOversizeLineEndsTheLoopWithAnError(t *testing.T) {
	var out bytes.Buffer
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":"` + strings.Repeat("x", 2048) + `"}}`
	if err := Run(context.Background(), strings.NewReader(line+"\n"), &out, &fakeServer{}, 1024); err == nil {
		t.Fatal("an over-cap line must end the loop with an error")
	}
}

func TestInvalidJSONGetsParseError(t *testing.T) {
	replies := run(t, &fakeServer{}, `not json`)
	if replies[0]["error"].(map[string]any)["code"] != float64(-32700) || replies[0]["id"] != nil {
		t.Fatalf("parse error reply %v", replies[0])
	}
}

type noWorkServer struct{ *fakeServer }

func (noWorkServer) Prepare(Call) (func(context.Context) Reply, *Reply) { return nil, nil }

func TestAServerThatPreparesNothingGetsAnErrorNotAPanic(t *testing.T) {
	replies := run(t, noWorkServer{&fakeServer{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`)
	if replies[0]["error"].(map[string]any)["code"] != float64(-32603) {
		t.Fatalf("reply %v", replies[0])
	}
	if err := Run(context.Background(), strings.NewReader(""), &bytes.Buffer{}, noWorkServer{&fakeServer{}}, 0); err == nil {
		t.Fatal("a non-positive line cap must be refused")
	}
}
