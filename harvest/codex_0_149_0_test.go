package harvest

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const currentCodexFixture = `{"timestamp":"2026-08-28T10:00:00Z","type":"session_meta","payload":{"id":"11111111-1111-1111-1111-111111111111","session_id":"11111111-1111-1111-1111-111111111111","cwd":"/work/repo"}}
{"timestamp":"2026-08-28T10:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"hidden developer instructions"}]}}
{"timestamp":"2026-08-28T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hidden injected environment"}]}}
{"timestamp":"2026-08-28T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"actual human prompt"}]}}}
{"timestamp":"2026-08-28T10:00:04Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[{"type":"Text","text":"useful assistant note"}],"phase":"commentary"}}}
{"timestamp":"2026-08-28T10:00:05Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","summary_text":[],"encrypted_content":"never-display-this"}}}
{"timestamp":"2026-08-28T10:00:06Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","summary_text":[{"text":"public reasoning summary"}],"encrypted_content":"still-never-display-this"}}}
{"timestamp":"2026-08-28T10:00:07Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/zsh","-lc","pwd"],"cwd":"file:///work/repo","status":"completed","stdout":"/work/repo\n","stderr":""}}}
{"timestamp":"2026-08-28T10:00:08Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"McpToolCall","server":"memory","tool":"search","arguments":{"query":"session"},"status":"completed","result":{"content":[{"type":"text","text":"one result"}]}}}}
{"timestamp":"2026-08-28T10:00:09Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":30},"last_token_usage":{"input_tokens":40,"output_tokens":10},"model_context_window":1000}}}
{"timestamp":"2026-08-28T10:00:10Z","type":"event_msg","payload":{"type":"thread_settings_applied"}}
{"timestamp":"2026-08-28T10:00:11Z","type":"event_msg","payload":{"type":"task_started"}}
{"timestamp":"2026-08-28T10:00:12Z","type":"event_msg","payload":{"type":"task_complete"}}
{"timestamp":"2026-08-28T10:00:13Z","type":"turn_context","payload":{"model":"gpt-test"}}
{"timestamp":"2026-08-28T10:00:14Z","type":"world_state","payload":{"type":"snapshot","private":"known non-display transport"}}
{"timestamp":"2026-08-28T10:00:15Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"FutureWidget","body":"unknown"}}}
{"timestamp":"2026-08-28T10:00:16Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"opaque protocol reasoning"}}
{"timestamp":"2026-08-28T10:00:17Z","type":"event_msg","payload":`

const legacyCodexFixture = `{"timestamp":"2026-08-28T09:00:00Z","payload":{"type":"session_meta","payload":{"session_id":"22222222-2222-2222-2222-222222222222","cwd":"/legacy/repo"}}}
{"timestamp":"2026-08-28T09:00:01Z","payload":{"type":"event_msg","payload":{"type":"user_message","message":"legacy user"}}}
{"timestamp":"2026-08-28T09:00:02Z","payload":{"type":"event_msg","payload":{"type":"agent_message","message":"legacy assistant"}}}
{"timestamp":"2026-08-28T09:00:03Z","payload":{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":12,"cached_input_tokens":2,"output_tokens":3},"last_token_usage":{"input_tokens":7,"output_tokens":3},"model_context_window":100}}}}
{"timestamp":"2026-08-28T09:00:04Z","payload":{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":\"pwd\"}"}}}
{"timestamp":"2026-08-28T09:00:05Z","payload":{"type":"response_item","payload":{"type":"function_call_output","output":"/legacy/repo"}}}
{"timestamp":"2026-08-28T09:00:06Z","payload":{"type":"response_item","payload":{"type":"reasoning","summary":[{"text":"legacy public reasoning"}],"encrypted_content":"opaque"}}}
`

func TestDecodeCodexRecordSupportsFlatAndNestedEnvelopes(t *testing.T) {
	flat, ok := decodeCodexRecord(map[string]any{
		"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{}},
	})
	if !ok || !flat.Flat || flat.Envelope != "event_msg" || flat.Kind != "token_count" {
		t.Fatalf("flat=%+v ok=%v", flat, ok)
	}
	nested, ok := decodeCodexRecord(map[string]any{
		"payload": map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call"}},
	})
	if !ok || nested.Flat || nested.Envelope != "response_item" || nested.Kind != "function_call" {
		t.Fatalf("nested=%+v ok=%v", nested, ok)
	}
}

func TestNormalizeCodexCurrentPresentationLane(t *testing.T) {
	events, unparsed, usage, err := normalizeCodexReader(strings.NewReader(currentCodexFixture), transcriptCaps)
	if err != nil {
		t.Fatal(err)
	}
	if unparsed != 2 {
		t.Fatalf("unparsed=%d want 2 (unknown item + malformed line)", unparsed)
	}
	wantKinds := []string{"user", "assistant", "thinking", "tool_call", "tool_result", "tool_call", "tool_result"}
	gotKinds := make([]string, len(events))
	for i := range events {
		gotKinds[i] = events[i].Kind
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("kinds=%v\nevents=%+v", gotKinds, events)
	}
	joined := ""
	for _, event := range events {
		joined += "\n" + event.Text
	}
	for _, forbidden := range []string{"hidden developer", "hidden injected", "never-display-this", "token_count", "thread_settings"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("canonical transcript leaked %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{"actual human prompt", "useful assistant note", "public reasoning summary", "/work/repo", "one result"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("canonical transcript missing %q: %s", required, joined)
		}
	}
	if events[3].Name != "Bash" || events[5].Name != "memory.search" {
		t.Fatalf("tool names=%q,%q", events[3].Name, events[5].Name)
	}
	if usage == nil || usage.Model != "gpt-test" || usage.Turns != 1 || usage.InputTokens != 80 ||
		usage.CacheRead != 20 || usage.OutputTokens != 30 || usage.Context != 50 || usage.ContextWindow != 1000 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestCodexMetaCurrentCountsPublicTurnsNotTokenUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-current.jsonl")
	if err := os.WriteFile(path, []byte(currentCodexFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, usage, days := codexMeta(path)
	if meta.threadID != "11111111-1111-1111-1111-111111111111" || meta.cwd != "/work/repo" ||
		meta.title != "actual human prompt" || meta.userTurns != 1 {
		t.Fatalf("meta=%+v", meta)
	}
	if usage == nil || usage.Turns != 1 || usage.Model != "gpt-test" || usage.InputTokens != 80 {
		t.Fatalf("usage=%+v", usage)
	}
	if day := days["2026-08-28"]; day == nil || day.Turns != 1 || day.Input != 80 || day.CacheRead != 20 || day.Output != 30 {
		t.Fatalf("day=%+v", day)
	}
}

func TestNormalizeCodexLegacyNestedCompatibility(t *testing.T) {
	events, unparsed, usage, err := normalizeCodexReader(strings.NewReader(legacyCodexFixture), transcriptCaps)
	if err != nil {
		t.Fatal(err)
	}
	if unparsed != 0 {
		t.Fatalf("unparsed=%d events=%+v", unparsed, events)
	}
	wantKinds := []string{"user", "assistant", "tool_call", "tool_result", "thinking"}
	gotKinds := make([]string, len(events))
	for i := range events {
		gotKinds[i] = events[i].Kind
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("kinds=%v events=%+v", gotKinds, events)
	}
	if usage == nil || usage.Turns != 1 || usage.InputTokens != 10 || usage.CacheRead != 2 || usage.OutputTokens != 3 {
		t.Fatalf("usage=%+v", usage)
	}
	if strings.Contains(events[len(events)-1].Text, "opaque") || events[len(events)-1].Text != "legacy public reasoning" {
		t.Fatalf("reasoning=%q", events[len(events)-1].Text)
	}
}

func TestNormalizeCodexCurrentProtocolOnlyIsVisibleCoverageGap(t *testing.T) {
	fixture := `{"timestamp":"2026-08-28T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"injected context must stay hidden"}]}}` + "\n"
	events, unparsed, _, err := normalizeCodexReader(strings.NewReader(fixture), transcriptCaps)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || unparsed != 1 {
		t.Fatalf("events=%+v unparsed=%d want no leaked events and one compatibility gap", events, unparsed)
	}
}

func TestCodexCurrentProjectionAggregatesPresentationSegments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "28")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	thread := "33333333-3333-3333-3333-333333333333"
	base := time.Unix(1_700_000_000, 0)
	write := func(name, message string, modified time.Time) {
		body := `{"timestamp":"2026-08-28T10:00:00Z","type":"session_meta","payload":{"session_id":"` + thread + `","cwd":"/work/repo"}}` + "\n" +
			`{"timestamp":"2026-08-28T10:00:01Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"` + message + `"}]}}}` + "\n"
		path := filepath.Join(dir, name+".jsonl")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	write("rollout-a", "first current segment", base)
	write("rollout-b", "second current segment", base.Add(time.Second))

	runtime := codexRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	snapshot, err := runtime.ReadTranscriptProjection(context.Background(), discovery.Sessions[0], ProjectionReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Unparsed != 0 || len(snapshot.Events) != 2 ||
		snapshot.Events[0].Event.Text != "first current segment" ||
		snapshot.Events[1].Event.Text != "second current segment" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}
