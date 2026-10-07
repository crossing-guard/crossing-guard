// Package recallmcp is the recall MCP server agent sessions call
// (recall-mcp-v1-plan). It owns no store handle: every tool is one or two
// authenticated GETs against the daemon of record, and a daemon it cannot
// reach is an error result, never an empty answer.
package recallmcp

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"crossing-guard/internal/mcpstdio"
)

// Command is the verb that runs the server.
const Command = "mcp"

// ServerName is the name every runtime registers the server under.
const ServerName = "crossing-guard"

// maxMessage bounds one request line; recall tool arguments are a few words.
const maxMessage = 64 << 10

// Server serves the recall tools for one runtime.
type Server struct {
	runtime  string
	identity CallerIdentity
	client   *client
}

// NewServer returns the server for one registered runtime.
func NewServer(runtime string, locate Locator) (*Server, error) {
	identity, ok := identities[runtime]
	if !ok {
		return nil, fmt.Errorf("unknown runtime %q (known: %s)", runtime, strings.Join(Runtimes(), ", "))
	}
	return &Server{runtime: runtime, identity: identity, client: &client{locate: locate}}, nil
}

func (s *Server) Info() mcpstdio.ServerInfo {
	return mcpstdio.ServerInfo{Name: ServerName, Version: "1"}
}

func (s *Server) Tools() []mcpstdio.Tool {
	out := make([]mcpstdio.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, mcpstdio.Tool{Name: t.name, Description: t.description, InputSchema: t.schema, Annotations: t.annotation()})
	}
	return out
}

func (s *Server) Prepare(call mcpstdio.Call) (func(context.Context) mcpstdio.Reply, *mcpstdio.Reply) {
	var selected *tool
	for index := range tools {
		if tools[index].name == call.Name {
			selected = &tools[index]
		}
	}
	if selected == nil {
		return nil, &mcpstdio.Reply{Err: &mcpstdio.Error{Code: -32602, Message: "unknown tool " + call.Name}}
	}
	place, placeSource := callerPlace(s.identity)
	who := caller{runtime: s.runtime, id: s.identity.SessionID(call.Meta), place: place, placeSource: placeSource}
	return func(ctx context.Context) mcpstdio.Reply {
		result, err := selected.run(ctx, s, toolCall{args: call.Arguments, caller: who})
		if err != nil {
			return mcpstdio.Reply{Result: mcpstdio.TextResult(err.Error(), true)}
		}
		limit := 0
		if config, err := s.client.settings(ctx); err == nil {
			limit = config.MaxResultBytes
		}
		// The write-shaped doors are not recall: a proposal's or a send
		// receipt's own framing serves, never the recalled-data notice (RT-6).
		if selected.name == "propose_memory" || selected.name == "send_to_session" {
			return mcpstdio.Reply{Result: mcpstdio.TextResult(renderBare(result, limit), false)}
		}
		return mcpstdio.Reply{Result: mcpstdio.TextResult(render(result, limit), false)}
	}, nil
}

// place asks the daemon where the caller works, without reading any peers.
func (s *Server) place(ctx context.Context, call toolCall) (map[string]any, error) {
	query := callerQuery(call)
	query.Set("place_only", "1")
	var out struct {
		Caller map[string]any `json:"caller"`
	}
	if err := s.client.get(ctx, "/api/sessions/peers", query, &out); err != nil {
		return nil, err
	}
	return out.Caller, nil
}

// render is the notice line, then the result as JSON. Over the byte limit,
// whole items are dropped from the end of the result's longest top-level
// list and truncated_to_fit says so; JSON is never cut mid-value. A result
// with no list left to shrink stays whole and says over_limit instead.
func render(result map[string]any, limit int) string {
	budget := limit - len(notice) - 1
	encoded, _ := json.Marshal(result)
	if limit > 0 && len(encoded) > budget {
		var generic map[string]any // typed slices become []any, so any list can shrink
		if json.Unmarshal(encoded, &generic) == nil {
			for len(encoded) > budget && dropLastItem(generic) {
				generic["truncated_to_fit"] = true
				encoded, _ = json.Marshal(generic)
			}
			if len(encoded) > budget {
				generic["over_limit"] = true
				encoded, _ = json.Marshal(generic)
			}
		}
	}
	return notice + "\n" + string(encoded)
}

// renderBare is render without the recalled-data notice — the one path for
// results that are not recall (the propose door's own framing serves).
func renderBare(result map[string]any, limit int) string {
	encoded, _ := json.Marshal(result)
	if limit > 0 && len(encoded) > limit {
		var generic map[string]any
		if json.Unmarshal(encoded, &generic) == nil {
			for len(encoded) > limit && dropLastItem(generic) {
				generic["truncated_to_fit"] = true
				encoded, _ = json.Marshal(generic)
			}
			if len(encoded) > limit {
				generic["over_limit"] = true
				encoded, _ = json.Marshal(generic)
			}
		}
	}
	return string(encoded)
}

// dropLastItem removes the last element of the longest top-level list.
func dropLastItem(result map[string]any) bool {
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	longest, size := "", 0
	for _, key := range keys {
		if list, ok := result[key].([]any); ok && len(list) > size {
			longest, size = key, len(list)
		}
	}
	if size == 0 {
		return false
	}
	result[longest] = result[longest].([]any)[:size-1]
	return true
}

// Main runs the server on stdin/stdout. A daemon that cannot be located is a
// per-call error, never an exit: the session keeps its tools and they work as
// soon as the daemon does.
func Main(args []string, locate Locator) int {
	flags := flag.NewFlagSet(Command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	runtime := flags.String("runtime", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *runtime == "" {
		fmt.Fprintf(os.Stderr, "usage: crossing-guard mcp --runtime <%s>\n", strings.Join(Runtimes(), "|"))
		return 2
	}
	server, err := NewServer(*runtime, locate)
	if err != nil {
		fmt.Fprintln(os.Stderr, "crossing-guard mcp:", err)
		return 2
	}
	if err := mcpstdio.Run(context.Background(), os.Stdin, os.Stdout, server, maxMessage); err != nil {
		fmt.Fprintln(os.Stderr, "crossing-guard mcp:", err)
		return 1
	}
	return 0
}
