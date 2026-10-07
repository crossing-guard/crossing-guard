package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"crossing-guard/internal/observation"
	"crossing-guard/internal/taskinput"
)

const antigravityChatTestID = "abcdef00-aaaa-4bbb-8ccc-0000000000a1"

func TestAntigravityChatCapabilityAndCommand(t *testing.T) {
	driver := antigravityChatDriver{}
	capability := driver.ChatCapability()
	if err := validateChatCapability("antigravity", capability); err != nil {
		t.Fatal(err)
	}
	if !capability.CanStart || !capability.CanResume || capability.MessageDelivery.Supported ||
		len(capability.Inputs) != 1 || capability.Inputs[0].Kind != "text" {
		t.Fatalf("capability overclaims or omits text: %+v", capability)
	}
	bin := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	req := ChatRequest{Runtime: "antigravity", Prompt: "literal --dangerously-skip-permissions",
		SessionID: antigravityChatTestID, Binary: bin, Cwd: t.TempDir()}
	cmd, err := driver.BuildCmd(req, ChatLaunchContext{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{bin, "-p", req.Prompt, "--output-format", "stream-json",
		"--disable-slash-commands", "--sandbox", "--print-timeout", "5m",
		"--conversation", antigravityChatTestID}
	if !reflect.DeepEqual(cmd.Args, want) || cmd.Dir != req.Cwd {
		t.Fatalf("argv=%q dir=%q", cmd.Args, cmd.Dir)
	}
	t.Setenv(observation.HandoffTicketEnv, "tkt_inherited")
	for _, tc := range []struct {
		name, session, ticket string
	}{
		{name: "normal"},
		{name: "attributed", ticket: "tkt_current"},
		{name: "resume", session: antigravityChatTestID},
	} {
		launchReq := req
		launchReq.SessionID, launchReq.HandoffTicket = tc.session, tc.ticket
		built, err := driver.BuildCmd(launchReq, ChatLaunchContext{})
		if err != nil {
			t.Fatal(err)
		}
		var tickets []string
		for _, entry := range built.Env {
			if strings.HasPrefix(entry, observation.HandoffTicketEnv+"=") {
				tickets = append(tickets, entry)
			}
		}
		var expected []string
		if tc.ticket != "" {
			expected = []string{observation.HandoffTicketEnv + "=" + tc.ticket}
		}
		if !reflect.DeepEqual(tickets, expected) {
			t.Fatalf("%s launch attribution = %q; want %q", tc.name, tickets, expected)
		}
	}
	for _, bad := range []ChatRequest{
		{SessionID: "--continue"},
		{Mode: "plan"}, {Mode: "accept-edits"}, {Model: "two tokens"}, {Model: "--dangerously-skip-permissions"},
		{ExtraArgs: "--dangerously-skip-permissions"}, {AuthToken: "secret"},
		{BaseURL: "https://example.test"}, {PermissionMode: "bypassPermissions"},
		{Sandbox: "danger-full-access"}, {OSS: true}, {LocalProvider: "ollama"},
	} {
		if _, err := driver.CanonicalizeChatRequest(bad); err == nil {
			t.Fatalf("accepted unsupported request: %+v", bad)
		}
	}
	if err := driver.ValidateChatInputs(req, []taskinput.ResolvedInput{{Input: taskinput.Input{Kind: taskinput.KindText}}}); err == nil {
		t.Fatal("accepted a separate task input")
	}
}

func TestAntigravityModelsUseExistingRunnerAndLiteralSelection(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(observation.HandoffTicketEnv, "tkt_inherited")
	driver := antigravityChatDriver{}
	called := false
	discovery, err := driver.DiscoverChatModels(context.Background(), ChatModelEnv{Run: func(cmd *exec.Cmd) ([]byte, error) {
		called = true
		if !reflect.DeepEqual(cmd.Args, []string{bin, "models"}) || cmd.Dir != "" {
			t.Fatalf("discovery bypasses runner or prompts model: %+v", cmd)
		}
		for _, entry := range cmd.Env {
			if strings.HasPrefix(entry, observation.HandoffTicketEnv+"=") {
				t.Fatalf("model discovery carries a handoff ticket: %q", entry)
			}
		}
		return []byte("fixture-model\tFixture model\n"), nil
	}})
	if err != nil || !called || len(discovery.Models) != 1 || discovery.Models[0].ID != "fixture-model" {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	model := "literal;$(not-a-command)"
	cmd, err := driver.BuildCmd(ChatRequest{Binary: bin, Model: model}, ChatLaunchContext{})
	if err != nil || !reflect.DeepEqual(cmd.Args[len(cmd.Args)-2:], []string{"--model", model}) {
		t.Fatalf("selection was not literal: cmd=%+v err=%v", cmd, err)
	}
}

func TestAntigravityModelsRejectMalformedEmptyAndDuplicateRows(t *testing.T) {
	models, rejected, err := parseAntigravityModels([]byte("fixture\tFixture (High)\r\nfixture\tDuplicate\n{\"token\":\"secret\"}\n--flag\tFlag\nbad id\tLabel\nother\t\tLabel\ncustom\tReserved\n"))
	if err != nil || rejected != 5 || len(models) != 2 || models[0].Limits != nil || models[0].Price != nil || models[0].Effort != nil || len(models[0].Inputs) != 0 {
		t.Fatalf("models=%+v rejected=%d err=%v", models, rejected, err)
	}
	for _, raw := range []string{"", "\n", "{\"token\":\"secret\"}\n", "model\t\n"} {
		_, _, err := parseAntigravityModels([]byte(raw))
		if err == nil || modelDiscoveryReason(err) != modelReasonUnparseable || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe or fresh empty result: %v", err)
		}
	}
}

func antigravityRunFixture(t *testing.T, raw string, requested string) ([]ChatEvent, error) {
	t.Helper()
	protocol := (antigravityChatDriver{}).ProcessProtocol(ChatRequest{SessionID: requested}, ChatLaunchContext{}, nil)
	events := []ChatEvent{}
	err := protocol.Run(io.NopCloser(strings.NewReader(raw)), func(event ChatEvent) {
		events = append(events, event)
	})
	return events, err
}

func TestAntigravityStreamProjectsMeasuredToolAndOneFinalReply(t *testing.T) {
	raw := `{"event":"init","conversation_id":"` + antigravityChatTestID + `","init":{"permission_mode":"request-review"}}` + "\n" +
		`{"event":"step_update","step_update":{"conversation_id":"` + antigravityChatTestID + `","step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"view_file","tool_info":{"name":"view_file","parameters":{"AbsolutePath":"/fixture/readme"}}}}` + "\n" +
		`{"event":"step_update","step_update":{"conversation_id":"` + antigravityChatTestID + `","step_index":2,"state":"DONE","step_type":"tool","tool_name":"view_file","tool_info":{"name":"view_file","parameters":{"AbsolutePath":"/fixture/readme"},"output":"file contents"}}}` + "\n" +
		`{"event":"step_update","step_update":{"conversation_id":"` + antigravityChatTestID + `","step_index":2,"state":"DONE","step_type":"tool","tool_name":"view_file","tool_info":{"output":"file contents"}}}` + "\n" +
		`{"event":"step_update","step_update":{"conversation_id":"` + antigravityChatTestID + `","step_index":3,"state":"ACTIVE","step_type":"agent_response","text_delta":"answer"}}` + "\n" +
		`{"event":"step_update","step_update":{"conversation_id":"` + antigravityChatTestID + `","step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"\n"}}` + "\n" +
		`{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"SUCCESS","response":"answer"}}` + "\n"
	events, err := antigravityRunFixture(t, raw, antigravityChatTestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("events=%+v", events)
	}
	for i, kind := range []string{"session", "tool", "tool_result", "delta", "delta", "text", "result"} {
		if events[i]["type"] != kind {
			t.Fatalf("event %d=%+v", i, events[i])
		}
	}
	if events[0]["id"] != antigravityChatTestID || events[1]["name"] != "view_file" ||
		events[3]["text"] != "answer" || events[4]["text"] != "\n" ||
		events[5]["text"] != "answer" || events[6]["id"] != antigravityChatTestID {
		t.Fatalf("wrong identity or response: %+v", events)
	}
}

func TestAntigravityResponseStreamsBeforeTerminalResult(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	protocol := (antigravityChatDriver{}).ProcessProtocol(ChatRequest{}, ChatLaunchContext{}, nil)
	events := make(chan ChatEvent, 8)
	done := make(chan error, 1)
	go func() { done <- protocol.Run(reader, func(event ChatEvent) { events <- event }) }()
	raw := `{"event":"init","conversation_id":"` + antigravityChatTestID + `"}` + "\n" +
		`{"event":"step_update","step_update":{"conversation_id":"` + antigravityChatTestID + `","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"hello "}}` + "\n"
	if _, err := writer.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"session", "delta"} {
		select {
		case event := <-events:
			if event["type"] != kind || kind == "delta" && event["text"] != "hello " {
				t.Fatalf("event=%v", event)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("response did not stream before the result")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("partial text ended the turn: %v", err)
	default:
	}
	if _, err := writer.Write([]byte(`{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"SUCCESS","response":"hello world"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not finish")
	}
	if event := <-events; event["type"] != "text" || event["text"] != "hello world" {
		t.Fatalf("terminal text did not replace fragments: %v", event)
	}
}

func TestAntigravityResponseFragmentStatesAndBounds(t *testing.T) {
	init := `{"event":"init","conversation_id":"` + antigravityChatTestID + `"}` + "\n"
	step := func(index any, state string, text any) string {
		raw, err := json.Marshal(map[string]any{"event": "step_update", "step_update": map[string]any{
			"conversation_id": antigravityChatTestID, "step_index": index,
			"step_type": "agent_response", "state": state, "text_delta": text}})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	}
	result := `{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"SUCCESS","response":"final"}}` + "\n"
	for _, tc := range []struct{ name, updates, want, wantErr string }{
		{"repeated active and done", step(1, "ACTIVE", "ha") + step(1, "ACTIVE", "ha") + step(1, "DONE", "\n") + step(1, "DONE", "ignored"), "haha\n", ""},
		{"unknown state", step(1, "QUEUED", "ignored"), "", ""},
		{"negative index", step(-1, "ACTIVE", "bad"), "", "step index"},
		{"fractional index", step(1.5, "ACTIVE", "bad"), "", "step index"},
		{"string index", step("1", "ACTIVE", "bad"), "", "step index"},
		{"invalid delta", step(1, "ACTIVE", true), "", "text delta"},
		{"active after done", step(1, "DONE", "ok") + step(1, "ACTIVE", "bad"), "ok", "completed assistant"},
		{"utf8 aggregate budget", step(1, "ACTIVE", strings.Repeat("a", 64*1024-1)) + step(1, "ACTIVE", "🦦") + step(1, "DONE", "xmore"), strings.Repeat("a", 64*1024-1) + "x", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, err := antigravityRunFixture(t, init+tc.updates+result, "")
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error=%v want=%q", err, tc.wantErr)
			}
			var fragments string
			for _, event := range events {
				if event["type"] == "delta" {
					text := anyString(event["text"])
					if !utf8.ValidString(text) {
						t.Fatal("invalid UTF-8 fragment")
					}
					fragments += text
				}
				if tc.wantErr != "" && event["type"] == "result" {
					t.Fatal("partial text was promoted to success")
				}
			}
			if fragments != tc.want {
				t.Fatalf("fragment lengths got=%d want=%d", len(fragments), len(tc.want))
			}
		})
	}
	var updates strings.Builder
	for i := 0; i <= 4096; i++ {
		updates.WriteString(step(i, "DONE", ""))
	}
	if _, err := antigravityRunFixture(t, init+updates.String()+result, ""); err == nil || !strings.Contains(err.Error(), "too many assistant steps") {
		t.Fatalf("unbounded assistant state: %v", err)
	}
	emptyResult := strings.Replace(result, `"response":"final"`, `"response":""`, 1)
	events, err := antigravityRunFixture(t, init+step(1, "ACTIVE", "partial")+emptyResult, "")
	if err == nil || len(events) != 2 || events[1]["type"] != "delta" {
		t.Fatalf("partial text falsely succeeded: events=%v err=%v", events, err)
	}
}

func TestAntigravityStreamRefusesFalseSuccessAndWrongIdentity(t *testing.T) {
	init := `{"event":"init","conversation_id":"` + antigravityChatTestID + `"}` + "\n"
	other := "00000000-0000-0000-0000-000000000001"
	cases := []struct {
		name      string
		raw       string
		requested string
		want      string
	}{
		{"empty success", init + `{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"SUCCESS","response":""}}` + "\n", "", "without a response"},
		{"quota error", init + `{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"ERROR","error":{"message":"RESOURCE_EXHAUSTED"}}}` + "\n", "", "quota exhausted"},
		{"missing result", init, "", "before a terminal result"},
		{"resume mismatch", init, other, "different conversation"},
		{"result mismatch", init + `{"event":"result","result":{"conversation_id":"` + other + `","status":"SUCCESS","response":"wrong"}}` + "\n", "", "changed conversation"},
		{"duplicate init", init + init, "", "duplicate stream init"},
		{"duplicate result", init + `{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"SUCCESS","response":"answer"}}` + "\n" + `{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"SUCCESS","response":"again"}}` + "\n", "", "after the terminal result"},
		{"malformed", init + "{", "", "malformed stream record"},
		{"oversized", init + strings.Repeat("x", taskStdoutRecordMax+1) + "\n", "", "output stream failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := antigravityRunFixture(t, tc.raw, tc.requested)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestAntigravityStreamReportsToolDenialWithoutInventingReply(t *testing.T) {
	raw := `{"event":"init","conversation_id":"` + antigravityChatTestID + `"}` + "\n" +
		`{"event":"step_update","step_update":{"conversation_id":"` + antigravityChatTestID + `","step_index":4,"state":"ERROR","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"echo safe"},"error":{"type":"permission","message":"Denied by headless permission policy"}}}}` + "\n" +
		`{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"SUCCESS","response":""}}` + "\n"
	events, err := antigravityRunFixture(t, raw, "")
	if err == nil || !strings.Contains(err.Error(), "without a response") || len(events) != 3 ||
		events[2]["type"] != "tool_result" || events[2]["is_error"] != true {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestAntigravityOneShotProcessWaitsForNaturalExitAndPropagatesStatus(t *testing.T) {
	stream := `{"event":"init","conversation_id":"` + antigravityChatTestID + `"}` + "\n" +
		`{"event":"result","result":{"conversation_id":"` + antigravityChatTestID + `","status":"SUCCESS","response":"answer"}}` + "\n"
	for _, exitCode := range []int{0, 7} {
		cmd := exec.Command("/bin/sh", "-c", "printf '%s' '"+stream+"'; exit "+string(rune('0'+exitCode)))
		events := []ChatEvent{}
		driver := antigravityChatDriver{}
		launch := taskExecutionLaunch{cmd: cmd, driver: driver, runtime: "antigravity",
			protocol: driver.ProcessProtocol(ChatRequest{}, ChatLaunchContext{}, cmd),
			started:  func() {}, event: func(event ChatEvent) { events = append(events, event) }}
		outcome := runTaskProcess(launch, func() bool { return false })
		if (exitCode == 0 && outcome.Err != nil) || (exitCode != 0 && outcome.Err == nil) ||
			len(events) != 3 || events[1]["text"] != "answer" {
			t.Fatalf("exit %d outcome=%+v events=%+v", exitCode, outcome, events)
		}
	}
}
