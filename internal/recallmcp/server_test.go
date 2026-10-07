package recallmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"crossing-guard/internal/mcpstdio"
)

type fakeDaemon struct {
	paths       []string
	answers     map[string]any
	posts       []string
	postAnswers map[string]any
	getError    error
}

func (f *fakeDaemon) Addr() string { return "127.0.0.1:1" }

func (f *fakeDaemon) GetJSON(path string, out any) error {
	return f.GetJSONContext(context.Background(), path, out)
}

func (f *fakeDaemon) GetJSONContext(_ context.Context, path string, out any) error {
	f.paths = append(f.paths, path)
	if f.getError != nil {
		return f.getError
	}
	route, _, _ := strings.Cut(path, "?")
	answer, ok := f.answers[route]
	if !ok {
		return errors.New(route + ": 404 Not Found")
	}
	raw, _ := json.Marshal(answer)
	return json.Unmarshal(raw, out)
}

// postAnswers records POST bodies; a POST the fake does not expect is an
// error, mirroring the real daemon's route table.
func (f *fakeDaemon) PostJSON(path string, body, out any) error {
	f.posts = append(f.posts, path)
	route, _, _ := strings.Cut(path, "?")
	answer, ok := f.postAnswers[route]
	if !ok {
		return errors.New(route + ": 404 Not Found")
	}
	raw, _ := json.Marshal(answer)
	return json.Unmarshal(raw, out)
}

func recallConfig() map[string]any {
	return map[string]any{"config": map[string]any{"recall": map[string]any{
		"request_timeout_ms": 1000, "max_result_bytes": 60000, "memory_body_max_bytes": 10, "memory_search_limit_max": 5}}}
}

func serve(t *testing.T, runtime string, daemon *fakeDaemon, locateErr error, lines ...string) []map[string]any {
	t.Helper()
	server, err := NewServer(runtime, func() (Daemon, error) {
		if locateErr != nil {
			return nil, locateErr
		}
		return daemon, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := mcpstdio.Run(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, server, maxMessage); err != nil {
		t.Fatal(err)
	}
	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var reply map[string]any
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, reply)
	}
	return replies
}

func toolText(t *testing.T, reply map[string]any) (string, bool) {
	t.Helper()
	result := reply["result"].(map[string]any)
	isError, _ := result["isError"].(bool)
	return result["content"].([]any)[0].(map[string]any)["text"].(string), isError
}

// Every READ tool is annotated read-only; the one write-shaped door is
// annotated honestly as NOT read-only (RT-6). The annotation surface is a
// consent contract — this test pins both halves.
func TestToolAnnotationsAreHonest(t *testing.T) {
	replies := serve(t, "opencode", &fakeDaemon{}, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listed := replies[0]["result"].(map[string]any)["tools"].([]any)
	if len(listed) != len(tools) {
		t.Fatalf("listed %d tools, want %d", len(listed), len(tools))
	}
	for _, entry := range listed {
		tool := entry.(map[string]any)
		hints := tool["annotations"].(map[string]any)
		name, _ := tool["name"].(string)
		if name == "propose_memory" || name == "send_to_session" {
			if hints["readOnlyHint"] != false || hints["idempotentHint"] != false {
				t.Fatalf("%s must not claim read-only or idempotent: %v", name, hints)
			}
			continue
		}
		if hints["readOnlyHint"] != true || hints["destructiveHint"] != false {
			t.Fatalf("%v is not annotated read-only: %v", name, hints)
		}
	}
}

func TestCodexCallerIdentityComesFromEachCallsMeta(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig(),
		"/api/sessions/peers": map[string]any{"peers": []any{}}}}
	replies := serve(t, "codex", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"active_sessions","arguments":{},"_meta":{"threadId":"thread-a"}}}`)
	if text, isError := toolText(t, replies[0]); isError || !strings.HasPrefix(text, notice+"\n") {
		t.Fatalf("reply %q error=%v", text, isError)
	}
	var peersPath string
	for _, path := range daemon.paths {
		if strings.HasPrefix(path, "/api/sessions/peers") {
			peersPath = path
		}
	}
	query, _ := url.ParseQuery(strings.SplitN(peersPath, "?", 2)[1])
	if query.Get("runtime") != "codex" || query.Get("id") != "thread-a" || query.Get("cwd_source") != "process_cwd" {
		t.Fatalf("peers query %v", query)
	}
}

func TestUnreachableDaemonIsAnErrorResultNeverEmpty(t *testing.T) {
	replies := serve(t, "claude", nil, errors.New("no service installed"),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"x"}}}`)
	text, isError := toolText(t, replies[0])
	if !isError || !strings.Contains(text, "could not be located") {
		t.Fatalf("reply %q error=%v", text, isError)
	}
	down := &fakeDaemon{getError: &url.Error{Op: "Get", URL: "http://127.0.0.1:1", Err: errors.New("connection refused")}}
	replies = serve(t, "claude", down, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_skills","arguments":{}}}`)
	if text, isError := toolText(t, replies[0]); !isError || !strings.Contains(text, "127.0.0.1:1") {
		t.Fatalf("reply %q error=%v", text, isError)
	}
}

func TestSearchSessionsNeverForwardsTheCoverageSessionList(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig(),
		"/api/search": map[string]any{"event_limit": 20,
			"coverage": map[string]any{"state": "incomplete", "indexed_sessions": 1, "sessions": []any{strings.Repeat("x", 5000)}},
			"hits": []any{
				map[string]any{"runtime": "codex", "id": "a", "snippet": "s"},
				map[string]any{"runtime": "claude", "id": "b", "snippet": "s"},
			}}}}
	replies := serve(t, "claude", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"x","runtime":"codex"}}}`)
	text, _ := toolText(t, replies[0])
	if strings.Contains(text, strings.Repeat("x", 100)) {
		t.Fatal("coverage.sessions was forwarded")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(text, notice+"\n")), &result); err != nil {
		t.Fatal(err)
	}
	if result["scanned_events"] != float64(20) || result["filters_applied_after_scan"] != true ||
		len(result["sessions"].([]any)) != 1 || !strings.Contains(result["coverage_reading"].(string), "NOT") {
		t.Fatalf("result %v", result)
	}
}

func TestGetMemoryCutsTheBodyAndStatesPending(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig(),
		"/api/memory/record": map[string]any{"id": "m", "body": "0123456789abcdef", "pending": true}}}
	replies := serve(t, "claude", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_memory","arguments":{"id":"m"}}}`)
	text, _ := toolText(t, replies[0])
	if !strings.Contains(text, `"body":"0123456789"`) || !strings.Contains(text, `"body_truncated":true`) || !strings.Contains(text, `"pending":true`) {
		t.Fatalf("memory %s", text)
	}
}

func TestRenderDropsWholeItemsToFit(t *testing.T) {
	items := []string{}
	for i := 0; i < 50; i++ {
		items = append(items, strings.Repeat("y", 100))
	}
	text := render(map[string]any{"hits": items, "total": 50}, 1200)
	if len(text) > 1200 {
		t.Fatalf("rendered %d bytes over the 1200 limit", len(text))
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(text, notice+"\n")), &result); err != nil {
		t.Fatalf("truncation broke the JSON: %v", err)
	}
	if result["truncated_to_fit"] != true || result["total"] != float64(50) {
		t.Fatalf("result %v", result)
	}
}

func TestUnknownRuntimeAndArgumentsAreRefused(t *testing.T) {
	if _, err := NewServer("nope", nil); err == nil {
		t.Fatal("an unknown runtime must be refused")
	}
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig()}}
	replies := serve(t, "claude", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_memory","arguments":{"id":"m","extra":1}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"drop_tables","arguments":{}}}`)
	byID := map[float64]map[string]any{}
	for _, reply := range replies {
		byID[reply["id"].(float64)] = reply
	}
	if _, isError := toolText(t, byID[1]); !isError {
		t.Fatal("an unknown argument must be an error result")
	}
	if byID[2]["error"] == nil {
		t.Fatal("an unknown tool must be a protocol error")
	}
}

// A daemon that stops answering is located afresh on the next call, so a
// restarted daemon is found without restarting the vendor session.
func TestTransportFailureRelocatesOnTheNextCall(t *testing.T) {
	down := &fakeDaemon{getError: &url.Error{Op: "Get", URL: "http://127.0.0.1:1", Err: errors.New("connection refused")}}
	up := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig(),
		"/api/skills": map[string]any{"skills": []any{}}}}
	locates := 0
	server, err := NewServer("claude", func() (Daemon, error) {
		locates++
		if locates == 1 {
			return down, nil
		}
		return up, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	call := `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"list_skills","arguments":{}}}`
	var out bytes.Buffer
	input := strings.NewReader(fmt.Sprintf(call, 1) + "\n")
	if err := mcpstdio.Run(context.Background(), input, &out, server, maxMessage); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := mcpstdio.Run(context.Background(), strings.NewReader(fmt.Sprintf(call, 2)+"\n"), &out, server, maxMessage); err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	_ = json.Unmarshal(out.Bytes(), &reply)
	if text, isError := toolText(t, reply); isError || locates != 2 {
		t.Fatalf("second call: %q error=%v locates=%d", text, isError, locates)
	}
}

// A JSON null body from the daemon is an empty result, never a crash.
func TestNullBodyIsAnEmptyResult(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig(), "/api/memory/search": nil}}
	replies := serve(t, "claude", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memories","arguments":{"query":"x"}}}`)
	if _, isError := toolText(t, replies[0]); isError {
		t.Fatal("a null body must not be an error")
	}
}

func TestRenderSaysOverLimitWhenNothingCanShrink(t *testing.T) {
	text := render(map[string]any{"body": strings.Repeat("z", 2000)}, 500)
	if !strings.Contains(text, `"over_limit":true`) {
		t.Fatalf("an oversized result with no list must say so: %s", text[:120])
	}
}

func recallConfigWithPropose(enabled bool) map[string]any {
	// The propose consent moved to memory.json's owner: the fake daemon
	// serves /api/memory/config, which the tool reads per invocation.
	return map[string]any{"config": map[string]any{"recall": map[string]any{
		"request_timeout_ms": 1000, "max_result_bytes": 60000, "memory_body_max_bytes": 10,
		"memory_search_limit_max": 5}}}
}

func memoryConfigWithPropose(enabled bool) map[string]any {
	return map[string]any{"propose": map[string]any{"enabled": enabled, "per_session_max": 5}}
}

// The propose door is off by default: the tool says so and posts nothing.
func TestProposeMemoryRefusedWithoutConsent(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfigWithPropose(false),
		"/api/memory/config": memoryConfigWithPropose(false)}}
	replies := serve(t, "claude", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"propose_memory","arguments":{"title":"t","body":"b"}}}`)
	text, isError := toolText(t, replies[0])
	if !isError || !strings.Contains(text, "not enabled") {
		t.Fatalf("expected a consent refusal, got %q (error=%v)", text, isError)
	}
	if len(daemon.posts) != 0 {
		t.Fatalf("no POST may happen without consent, got %v", daemon.posts)
	}
}

// With consent the tool posts to the propose route and frames the result as
// a proposal, never as recall.
func TestProposeMemoryPostsWithConsent(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-123")
	daemon := &fakeDaemon{
		answers:     map[string]any{"/api/console/config": recallConfigWithPropose(true), "/api/memory/config": memoryConfigWithPropose(true)},
		postAnswers: map[string]any{"/api/memory/propose": map[string]any{"id": "proposal-x", "status": "pending"}},
	}
	replies := serve(t, "claude", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"propose_memory","arguments":{"title":"t","body":"b"}}}`)
	text, isError := toolText(t, replies[0])
	if isError {
		t.Fatalf("expected a proposal result, got error %q", text)
	}
	if !strings.Contains(text, "a human promotes it") {
		t.Fatalf("result must carry the proposal framing, got %q", text)
	}
	if strings.Contains(text, "Recall context") {
		t.Fatalf("the propose result must NOT carry the recalled-data notice, got %q", text)
	}
	if len(daemon.posts) != 1 || daemon.posts[0] != "/api/memory/propose" {
		t.Fatalf("expected one propose POST, got %v", daemon.posts)
	}
}

// The write door advertises itself honestly: not read-only.
func TestProposeMemoryAnnotationIsNotReadOnly(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig()}}
	replies := serve(t, "claude", daemon, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools, _ := replies[0]["result"].(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		annotations, _ := tool["annotations"].(map[string]any)
		readOnlyHint, _ := annotations["readOnlyHint"].(bool)
		if name == "propose_memory" && readOnlyHint {
			t.Fatal("propose_memory must not advertise readOnlyHint")
		}
		if name == "get_memory" && !readOnlyHint {
			t.Fatal("read tools keep readOnlyHint")
		}
	}
}

// An unidentified caller cannot propose: no citation is possible.
func TestProposeMemoryRequiresIdentifiedCaller(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfigWithPropose(true),
		"/api/memory/config": memoryConfigWithPropose(true)}}
	// opencode is the runtime that does not identify its caller sessions.
	replies := serve(t, "opencode", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"propose_memory","arguments":{"title":"t","body":"b"}}}`)
	text, isError := toolText(t, replies[0])
	if !isError || !strings.Contains(text, "cannot cite") {
		t.Fatalf("expected an unidentified-caller refusal, got %q (error=%v)", text, isError)
	}
	if len(daemon.posts) != 0 {
		t.Fatalf("no POST may happen without a citable session, got %v", daemon.posts)
	}
}

// unidentified caller, never guesses (recall-mcp-v1 F13).
func TestSendToSessionPostsCallerIdentityOnTheQuery(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig()},
		postAnswers: map[string]any{"/api/session-message/send": map[string]any{"invocation_id": "oinv_x", "state": "pending",
			"tier": "queued-delivery", "boundary": "the session's next hook boundary", "detail": "Pending."}}}
	replies := serve(t, "codex", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_to_session","arguments":{"runtime":"claude","session_id":"abc-123","message":"hello"},"_meta":{"threadId":"thread-9"}}}`)
	text, isError := toolText(t, replies[0])
	if isError {
		t.Fatalf("send reply errored: %s", text)
	}
	if !strings.Contains(text, `"invocation_id":"oinv_x"`) || !strings.Contains(text, `"state":"pending"`) {
		t.Fatalf("receipt passthrough incomplete: %s", text)
	}
	var sendPath string
	for _, path := range daemon.posts {
		if strings.HasPrefix(path, "/api/session-message/send") {
			sendPath = path
		}
	}
	if sendPath == "" {
		t.Fatal("no POST path was recorded")
	}
	query, _ := url.ParseQuery(strings.SplitN(sendPath, "?", 2)[1])
	if query.Get("caller_runtime") != "codex" || query.Get("caller_id") != "thread-9" {
		t.Fatalf("send query must carry caller_runtime/caller_id (the route's names): %v", query)
	}
}

// A refusal or error from the daemon is the tool's answer, never a retry.
func TestSendToSessionPassesRefusalsThrough(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig()},
		postAnswers: map[string]any{"/api/session-message/send": map[string]any{"state": "refused",
			"detail": "deliver_attended is not granted"}}}
	replies := serve(t, "claude", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_to_session","arguments":{"runtime":"codex","session_id":"t1","message":"hi"}}}`)
	text, isError := toolText(t, replies[0])
	if isError {
		t.Fatalf("a daemon refusal is a typed result, not a tool error: %s", text)
	}
	if !strings.Contains(text, `"state":"refused"`) || !strings.Contains(text, "deliver_attended is not granted") {
		t.Fatalf("refusal passthrough incomplete: %s", text)
	}
}

// The send tool refuses locally when a required argument is empty: nothing is
// posted at all.
func TestSendToSessionRefusesEmptyArguments(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig()}}
	replies := serve(t, "claude", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_to_session","arguments":{"runtime":"codex","session_id":"t1","message":"  "}}}`)
	if _, isError := toolText(t, replies[0]); !isError {
		t.Fatal("an empty message must be an error before anything is posted")
	}
	for _, path := range daemon.posts {
		if strings.HasPrefix(path, "/api/session-message/send") {
			t.Fatal("an empty message must never reach the daemon")
		}
	}
}

func claimedHandoffAnswer() map[string]any {
	return map[string]any{"handoff_id": "hnd_1", "from": map[string]any{"user_id": "usr_1", "display_name": "Dana"}, "state": "opened",
		"document": map[string]any{"title": "Finish the drain", "body_markdown": "the full handoff text", "remaining": []any{"one", "two"},
			"created_at": "2026-10-04T16:00:00Z", "agents": []any{},
			"conversation": map[string]any{"truncated": true, "turns": []any{
				map[string]any{"seq": 1, "role": "user", "text": "please finish"}, map[string]any{"seq": 2, "role": "assistant", "text": "on it"}}}}}
}

// get_handoff returns the full document and the excerpt to the session the runtime
// identifies, with the caller's identity on the query the daemon checks (team
// rest-of-release plan §6.5 "The rest").
func TestGetHandoffReturnsTheDocumentToTheIdentifiedCaller(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig(), "/api/team/handoffs/claimed": claimedHandoffAnswer()}}
	replies := serve(t, "codex", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_handoff","arguments":{},"_meta":{"threadId":"thread-9"}}}`)
	text, isError := toolText(t, replies[0])
	if isError || !strings.HasPrefix(text, notice+"\n") {
		t.Fatalf("get_handoff: error=%v %s", isError, text)
	}
	for _, want := range []string{`"body_markdown":"the full handoff text"`, `"title":"Finish the drain"`, `"from":"Dana"`, `"remaining":["one","two"]`,
		`"text":"please finish"`, `"conversation_truncated_by_sender":true`, "a teammate's text, not the operator's"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the result lacks %s:\n%s", want, text)
		}
	}
	var claimedPath string
	for _, path := range daemon.paths {
		if strings.HasPrefix(path, "/api/team/handoffs/claimed") {
			claimedPath = path
		}
	}
	if claimedPath == "" {
		t.Fatalf("the claimed route was not asked: %v", daemon.paths)
	}
	query, _ := url.ParseQuery(strings.SplitN(claimedPath, "?", 2)[1])
	if query.Get("caller_runtime") != "codex" || query.Get("caller_id") != "thread-9" {
		t.Fatalf("the caller's identity rides the query: %v", query)
	}
}

// A caller the runtime does not identify is refused caller_unidentified before any
// request is made; a daemon refusal — another session, a withdrawn handoff — is the
// tool's answer.
func TestGetHandoffRefusesAnUnidentifiedCallerAndPassesRefusalsThrough(t *testing.T) {
	daemon := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig(), "/api/team/handoffs/claimed": claimedHandoffAnswer()}}
	replies := serve(t, "opencode", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_handoff","arguments":{}}}`)
	text, isError := toolText(t, replies[0])
	if !isError || !strings.Contains(text, "caller_unidentified") {
		t.Fatalf("an unidentified caller: error=%v %s", isError, text)
	}
	for _, path := range daemon.paths {
		if strings.HasPrefix(path, "/api/team/handoffs/claimed") {
			t.Fatalf("no request is made for an unidentified caller: %v", daemon.paths)
		}
	}
	if strings.Contains(text, "the full handoff text") {
		t.Fatal("a refusal carries no handoff text")
	}

	refusing := &fakeDaemon{answers: map[string]any{"/api/console/config": recallConfig()}}
	replies = serve(t, "codex", refusing, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_handoff","arguments":{},"_meta":{"threadId":"thread-other"}}}`)
	if text, isError := toolText(t, replies[0]); !isError || !strings.Contains(text, "no handoff was returned to this session") {
		t.Fatalf("a daemon refusal: error=%v %s", isError, text)
	}
	replies = serve(t, "codex", daemon, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_handoff","arguments":{"id":"hnd_other"},"_meta":{"threadId":"thread-9"}}}`)
	if text, isError := toolText(t, replies[0]); !isError {
		t.Fatalf("get_handoff takes no arguments — a session cannot name another handoff: %s", text)
	}
}

// The names other packages put in a sentence to a session (the handoff brief) are the
// names in the registered table, each exactly once.
func TestExportedToolNamesAreRegistered(t *testing.T) {
	for _, name := range []string{ToolSearchMemories, ToolGetMemory, ToolGetHandoff} {
		registered := 0
		for _, tool := range tools {
			if tool.name == name {
				registered++
			}
		}
		if registered != 1 {
			t.Fatalf("%q is registered %d times, want once", name, registered)
		}
	}
}
