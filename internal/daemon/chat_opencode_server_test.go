package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"crossing-guard/internal/guardcli"
)

func ocEvent(kind string, props map[string]any) openCodeServerEvent {
	raw, _ := json.Marshal(props)
	return openCodeServerEvent{Type: kind, Properties: raw}
}
func ocStatus(kind string) openCodeServerEvent {
	return ocEvent("session.status", map[string]any{"sessionID": "ses_fixture", "status": map[string]any{"type": kind}})
}
func ocPermission() openCodeServerEvent {
	return ocEvent("permission.asked", map[string]any{"id": "per_fixture", "sessionID": "ses_fixture", "permission": "external_directory", "patterns": []string{"/tmp/outside/*"}})
}

func TestOpenCodeServerHoldsAndRepliesThroughCanonicalApproval(t *testing.T) {
	for _, decision := range []string{"allowed", "denied", "expired", "unavailable"} {
		t.Run(decision, func(t *testing.T) {
			frames := make(chan openCodeServerEvent, 16)
			asked := make(chan guardcli.RuntimeApprovalRequest, 1)
			release := make(chan struct{})
			replySeen := make(chan string, 1)
			var calls atomic.Int32
			subscribed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				username, password, ok := r.BasicAuth()
				if !ok || username != "opencode" || password != "test-password" {
					t.Error("missing private-server auth")
					w.WriteHeader(401)
					return
				}
				switch r.URL.Path {
				case "/global/health":
					_, _ = io.WriteString(w, `{"healthy":true,"version":"1.18.0"}`)
				case "/session":
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if len(body["permission"].([]any)) != 3 {
						t.Error("question defaults lost")
					}
					_, _ = io.WriteString(w, `{"id":"ses_fixture"}`)
				case "/event":
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					close(subscribed)
					for {
						select {
						case event := <-frames:
							raw, _ := json.Marshal(event)
							_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
							w.(http.Flusher).Flush()
						case <-r.Context().Done():
							return
						}
					}
				case "/session/ses_fixture/prompt_async":
					select {
					case <-subscribed:
					default:
						t.Error("prompt sent before subscription")
					}
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["model"] != nil {
						t.Error("default model overridden")
					}
					frames <- ocStatus("idle")
					frames <- ocStatus("busy")
					frames <- ocPermission()
					frames <- ocPermission()
					w.WriteHeader(204)
				case "/permission/per_fixture/reply":
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					replySeen <- body["reply"]
					_, _ = io.WriteString(w, "true")
					frames <- ocStatus("idle")
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			protocol := &openCodeServerProtocol{endpoint: server.URL, client: server.Client(), password: "test-password", timeout: time.Second,
				request: ChatRequest{Cwd: t.TempDir(), Prompt: "read"}, launch: ChatLaunchContext{TaskID: "task_fixture"},
				approve: func(ctx context.Context, in guardcli.RuntimeApprovalRequest) (guardcli.RuntimeApprovalResult, error) {
					calls.Add(1)
					asked <- in
					select {
					case <-release:
					case <-ctx.Done():
						return guardcli.RuntimeApprovalResult{}, ctx.Err()
					}
					if decision == "unavailable" {
						return guardcli.RuntimeApprovalResult{}, errors.New("offline")
					}
					return guardcli.RuntimeApprovalResult{Decision: decision}, nil
				}}
			done := make(chan error, 1)
			go func() { done <- protocol.converse(context.Background(), func(ChatEvent) {}) }()
			select {
			case in := <-asked:
				if in.NativeSessionID != "ses_fixture" || in.TaskID != "task_fixture" || in.Runtime != "opencode" || in.ToolCallID != "per_fixture" {
					t.Errorf("identity: %#v", in)
				}
				if in.Action != "Access paths outside the session working folder" ||
					len(in.Targets) != 1 || in.Targets[0] != "/tmp/outside/*" || in.ApprovalReason == "" ||
					!in.OfferExactRunGrant {
					t.Errorf("presentation: %#v", in)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("approval never arrived")
			}
			select {
			case err := <-done:
				t.Fatalf("turn ended while approval pending: %v", err)
			default:
			}
			close(release)
			select {
			case err := <-done:
				if (err == nil) != (decision == "allowed") {
					t.Fatalf("decision %s: %v", decision, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("turn did not settle")
			}
			want := "reject"
			if decision == "allowed" {
				want = "once"
			}
			if reply := <-replySeen; reply != want {
				t.Fatalf("reply %s, want %s", reply, want)
			}
			if calls.Load() != 1 {
				t.Fatalf("duplicate canonical approvals: %d", calls.Load())
			}
		})
	}
}

func TestOpenCodeExactRunGrantSerializesAndReusesExactPermission(t *testing.T) {
	var replies atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/permission/") && strings.HasSuffix(r.URL.Path, "/reply") {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["reply"] != "once" {
				t.Errorf("native reply = %q", body["reply"])
			}
			replies.Add(1)
			_, _ = io.WriteString(w, "true")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	firstSeen := make(chan guardcli.RuntimeApprovalRequest, 1)
	secondSeen := make(chan guardcli.RuntimeApprovalRequest, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	protocol := &openCodeServerProtocol{endpoint: server.URL, client: server.Client(), password: "test-password", timeout: time.Second,
		request: ChatRequest{Cwd: t.TempDir()}, launch: ChatLaunchContext{TaskID: "task_fixture"},
		approve: func(_ context.Context, in guardcli.RuntimeApprovalRequest) (guardcli.RuntimeApprovalResult, error) {
			if calls.Add(1) == 1 {
				firstSeen <- in
				<-release
			} else {
				secondSeen <- in
			}
			return guardcli.RuntimeApprovalResult{Decision: "allowed", GrantID: approvalGrantRunExact,
				GrantToken: "arg_" + strings.Repeat("A", 26)}, nil
		}}
	permission := func(id string) openCodePermission {
		return openCodePermission{ID: id, SessionID: "ses_fixture", Permission: "external_directory", Patterns: []string{"/tmp/outside/*"}}
	}
	done := make(chan error, 2)
	go func() {
		done <- protocol.decidePermission(context.Background(), "ses_fixture", permission("per_first"))
	}()
	first := <-firstSeen
	if first.GrantToken != "" || !first.OfferExactRunGrant {
		t.Fatalf("first request = %#v", first)
	}
	go func() {
		done <- protocol.decidePermission(context.Background(), "ses_fixture", permission("per_second"))
	}()
	select {
	case request := <-secondSeen:
		t.Fatalf("duplicate was not serialized: %#v", request)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	second := <-secondSeen
	if second.GrantToken == "" || !sameApprovalTargets(second.Targets, first.Targets) {
		t.Fatalf("remembered request = %#v", second)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 || replies.Load() != 2 {
		t.Fatalf("calls=%d replies=%d", calls.Load(), replies.Load())
	}
}

func TestOpenCodeStaleRunGrantFallsBackToInteractiveRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/permission/") || !strings.HasSuffix(r.URL.Path, "/reply") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["reply"] != "once" {
			t.Errorf("native reply = %q", body["reply"])
		}
		_, _ = io.WriteString(w, "true")
	}))
	defer server.Close()
	permission := openCodePermission{ID: "per_retry", SessionID: "ses_fixture",
		Permission: "external_directory", Patterns: []string{"/tmp/outside/*"}}
	keyBytes, _ := json.Marshal([]any{permission.Permission, permission.Patterns})
	var calls []guardcli.RuntimeApprovalRequest
	protocol := &openCodeServerProtocol{endpoint: server.URL, client: server.Client(),
		password: "test-password", timeout: time.Second, request: ChatRequest{Cwd: t.TempDir()},
		launch:      ChatLaunchContext{TaskID: "task_fixture"},
		grantTokens: map[string]string{string(keyBytes): "arg_" + strings.Repeat("A", 26)},
		approve: func(_ context.Context, in guardcli.RuntimeApprovalRequest) (guardcli.RuntimeApprovalResult, error) {
			calls = append(calls, in)
			if len(calls) == 1 {
				return guardcli.RuntimeApprovalResult{}, errors.New("stale grant")
			}
			return guardcli.RuntimeApprovalResult{Decision: "allowed", GrantID: approvalGrantRequest}, nil
		}}
	if err := protocol.decidePermission(context.Background(), "ses_fixture", permission); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].GrantToken == "" || calls[1].GrantToken != "" {
		t.Fatalf("stale grant retry = %#v", calls)
	}
	if _, ok := protocol.grantTokens[string(keyBytes)]; ok {
		t.Fatal("stale grant remained cached after one-request recovery")
	}
}

func TestOpenCodeEventsRejectForeignPermissionAndKeepUserTextOut(t *testing.T) {
	state := newOpenCodeEventState("ses_fixture")
	foreign := ocPermission()
	foreign.Properties = []byte(strings.ReplaceAll(string(foreign.Properties), "ses_fixture", "ses_foreign"))
	if _, err := state.permission(foreign.Properties); err == nil {
		t.Fatal("foreign permission accepted")
	}
	var projected []ChatEvent
	emit := func(e ChatEvent) { projected = append(projected, e) }
	for _, role := range []string{"user", "assistant"} {
		id := "msg_" + role
		_, err := state.project(ocEvent("message.updated", map[string]any{"sessionID": "ses_fixture", "info": map[string]any{"id": id, "role": role}}), emit)
		if err != nil {
			t.Fatal(err)
		}
		part := ocEvent("message.part.updated", map[string]any{"sessionID": "ses_fixture", "part": map[string]any{"id": "prt_" + role, "messageID": id, "type": "text", "text": role, "time": map[string]any{"end": 1}}})
		for range 2 {
			if _, err := state.project(part, emit); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(projected) != 1 || projected[0]["text"] != "assistant" {
		t.Fatalf("bad transcript: %#v", projected)
	}
	if done, _ := state.project(ocStatus("idle"), emit); done {
		t.Fatal("initial idle completed turn")
	}
	_, _ = state.project(ocStatus("busy"), emit)
	if done, _ := state.project(ocStatus("idle"), emit); !done {
		t.Fatal("busy -> idle failed")
	}
	_, err := state.project(ocEvent("session.error", map[string]any{"sessionID": "ses_fixture", "error": map[string]any{"name": "APIError", "data": map[string]any{"message": "provider unavailable"}}}), emit)
	if err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("error lost: %v", err)
	}
}

func TestOpenCodeServerCancellationClosesPendingApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	canceled := make(chan struct{})
	protocol := &openCodeServerProtocol{timeout: time.Minute, approve: func(ctx context.Context, _ guardcli.RuntimeApprovalRequest) (guardcli.RuntimeApprovalResult, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return guardcli.RuntimeApprovalResult{}, ctx.Err()
	}, client: &http.Client{}, endpoint: "http://127.0.0.1:1"}
	done := make(chan error, 1)
	go func() {
		done <- protocol.decidePermission(ctx, "ses_fixture", openCodePermission{ID: "per_fixture", Permission: "read"})
	}()
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("approval remained pending")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("permission handler leaked")
	}
}

func TestOpenCodePrivateEndpointAndArgumentValidation(t *testing.T) {
	for _, endpoint := range []string{"https://127.0.0.1:1234", "http://localhost:1234", "http://127.0.0.1:0", "http://127.0.0.1:1234/path", "http://user@127.0.0.1:1234", "http://127.0.0.1:1234?x=1", "http://example.com:1234"} {
		if validOpenCodeEndpoint(endpoint) {
			t.Errorf("accepted %s", endpoint)
		}
	}
	if !validOpenCodeEndpoint("http://127.0.0.1:1234") {
		t.Fatal("valid private endpoint refused")
	}
	for _, req := range []ChatRequest{{ExtraArgs: "--auto"}, {ExtraArgs: "--attach=http://x"}, {Model: "bad-model"}, {SessionID: "ses_../other"}} {
		if _, err := (openCodeChatDriver{}).CanonicalizeChatRequest(req); err == nil {
			t.Errorf("accepted %#v", req)
		}
	}
	driver := openCodeChatDriver{}
	launch := ChatLaunchContext{TaskID: "task_fixture", DataDir: t.TempDir()}
	req := ChatRequest{Binary: testChatExecutable(t), Cwd: t.TempDir(), Prompt: "private prompt"}
	first, err := driver.BuildCmd(req, launch)
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.BuildCmd(req, launch)
	if err != nil {
		t.Fatal(err)
	}
	a := driver.ProcessProtocol(req, launch, first).(*openCodeServerProtocol)
	b := driver.ProcessProtocol(req, launch, second).(*openCodeServerProtocol)
	if a.password == "" || a.password == b.password || strings.Contains(strings.Join(first.Args, " "), a.password) {
		t.Fatal("server secret not isolated")
	}
	if !reflect.DeepEqual(first.Args[1:], []string{"serve", "--hostname", "127.0.0.1", "--port", "0"}) {
		t.Fatalf("unsafe argv %q", first.Args)
	}
}

type processProtocolFixture struct{ err error }

func (p processProtocolFixture) Run(stdout io.ReadCloser, emit func(ChatEvent)) error {
	_ = stdout.Close()
	emit(ChatEvent{"type": "text", "text": "finished"})
	return p.err
}
func TestTaskProcessProtocolReapsOwnedServer(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 30")
	result := runTaskProcess(taskExecutionLaunch{cmd: cmd, protocol: processProtocolFixture{}, started: func() {}, event: func(ChatEvent) {}}, func() bool { return false })
	if result.Err != nil || cmd.ProcessState == nil {
		t.Fatalf("server not successfully reaped: %#v %v", result, cmd.ProcessState)
	}
}

func TestOpenCodeServerVersionAgentAndResumeGuards(t *testing.T) {
	for _, test := range []struct{ name, version, agent, session, directory string }{
		{name: "wrong version", version: "1.19.0"},
		{name: "missing agent", version: "1.18.0", agent: "missing"},
		{name: "wrong directory", version: "1.18.0", session: "ses_fixture", directory: t.TempDir()},
	} {
		t.Run(test.name, func(t *testing.T) {
			prompts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/global/health":
					_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": test.version})
				case "/agent":
					_, _ = io.WriteString(w, `[{"name":"build"}]`)
				case "/session/ses_fixture":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_fixture", "directory": test.directory})
				default:
					prompts++
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			protocol := &openCodeServerProtocol{endpoint: server.URL, client: server.Client(), password: "fixture", request: ChatRequest{Cwd: t.TempDir(), Mode: test.agent, SessionID: test.session}}
			if _, err := protocol.prepareSession(context.Background()); err == nil {
				t.Fatal("unsafe session admitted")
			}
			if prompts != 0 {
				t.Fatal("prompt sent before compatibility validation")
			}
		})
	}
}

func TestOpenCodeStreamBoundaries(t *testing.T) {
	for _, input := range []string{"data: broken\n\n", "data: {}\n\n", strings.Repeat("x", openCodeHTTPBound+1)} {
		frames := make(chan openCodeServerEvent, 2)
		if err := readOpenCodeEvents(context.Background(), strings.NewReader(input), frames); err == nil {
			t.Fatal("malformed stream succeeded")
		}
	}
	frames := make(chan openCodeServerEvent, 4)
	err := readOpenCodeEvents(context.Background(), strings.NewReader("data: {\"type\":\"session.status\",\n"+"data: \"properties\":{\"sessionID\":\"ses_fixture\",\"status\":{\"type\":\"busy\"}}}\n\n"), frames)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("EOF was success: %v", err)
	}
	if frame := <-frames; frame.Type != "session.status" {
		t.Fatalf("multiline frame lost: %#v", frame)
	}
}

func TestOpenCodeServerDeathCancelsProtocol(t *testing.T) {
	listening := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = io.WriteString(w, `{"healthy":true,"version":"1.18.0"}`)
		case "/session":
			_, _ = io.WriteString(w, `{"id":"ses_fixture"}`)
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/session/ses_fixture/prompt_async":
			close(listening)
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	protocol := &openCodeServerProtocol{client: server.Client(), password: "fixture", request: ChatRequest{Cwd: t.TempDir(), Prompt: "test"}}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- protocol.Run(reader, func(ChatEvent) {}) }()
	_, _ = fmt.Fprintln(writer, "opencode server listening on "+server.URL)
	select {
	case <-listening:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never dispatched")
	}
	_ = writer.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("server death reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("protocol did not cancel")
	}
}
