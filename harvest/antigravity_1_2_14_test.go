package harvest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const antigravityTestID = "abcdef00-aaaa-4bbb-8ccc-000000000014"

func antigravityTestPath(t *testing.T, home, id string) string {
	t.Helper()
	path := filepath.Join(home, ".gemini", "antigravity-cli", "brain", id,
		".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func antigravityTestRow(t *testing.T, source, kind, content string) string {
	t.Helper()
	row, err := json.Marshal(map[string]any{"source": source, "type": kind,
		"status": "DONE", "created_at": "2026-10-01T10:00:00-05:00", "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return string(row) + "\n"
}

func antigravityTestToolRow(t *testing.T, content string, calls any) string {
	t.Helper()
	row, err := json.Marshal(map[string]any{"source": "MODEL", "type": "PLANNER_RESPONSE",
		"status": "DONE", "created_at": "2026-10-01T10:00:01-05:00",
		"content": content, "tool_calls": calls})
	if err != nil {
		t.Fatal(err)
	}
	return string(row) + "\n"
}

func TestAntigravityNoToolAndResumeDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := antigravityTestPath(t, home, antigravityTestID)
	rows := antigravityTestRow(t, "USER_EXPLICIT", "USER_INPUT",
		"<USER_REQUEST>What changed?</USER_REQUEST><ADDITIONAL_METADATA>private setting</ADDITIONAL_METADATA>") +
		antigravityTestRow(t, "MODEL", "PLANNER_RESPONSE", "Nothing changed.") +
		antigravityTestRow(t, "USER_EXPLICIT", "USER_INPUT",
			"<USER_REQUEST>And now?</USER_REQUEST><ADDITIONAL_METADATA>more private metadata</ADDITIONAL_METADATA>") +
		antigravityTestRow(t, "MODEL", "PLANNER_RESPONSE", "Still nothing.")
	if err := os.WriteFile(path, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := (antigravityRuntime{}).Collect()
	if len(jobs) != 1 {
		t.Fatalf("collected %d sessions, want one resumed conversation", len(jobs))
	}
	sum, ok := (antigravityRuntime{}).Summarize(jobs[0])
	if !ok || sum.ID != antigravityTestID || sum.Title != "What changed?" ||
		sum.TitleSource != "prompt" || sum.UserTurns != 2 || sum.Cwd != "" || sum.Project != "" {
		t.Fatalf("summary: %+v ok=%v", sum, ok)
	}
	events, unparsed, _, err := (antigravityRuntime{}).Normalize(path)
	if err != nil || unparsed != 0 || len(events) != 4 || events[0].Kind != "user" ||
		events[0].Text != "What changed?" || events[1].Kind != "assistant" || events[3].Text != "Still nothing." {
		t.Fatalf("events=%+v unparsed=%d err=%v", events, unparsed, err)
	}
	if key := RepositoryGroupKey(sum); key != NoProjectKey {
		t.Fatalf("invented project: %s", key)
	}
}

func TestAntigravityUnknownAndPartialRecordsStayCounted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := antigravityTestPath(t, home, antigravityTestID)
	rows := antigravityTestRow(t, "USER_EXPLICIT", "USER_INPUT",
		"<USER_REQUEST>Read a file</USER_REQUEST>") +
		antigravityTestRow(t, "MODEL", "GENERIC", "Tool-looking prose is not a verified tool result") +
		antigravityTestRow(t, "SYSTEM", "SYSTEM_MESSAGE", "private system text") +
		`{"source":"MODEL","type":"PLANNER_RESPONSE","status":"ACTIVE","content":"unfinished"}` + "\n" +
		`{"source":"MODEL","type":"PLANNER_RESPONSE","content":`
	if err := os.WriteFile(path, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	events, unparsed, _, err := (antigravityRuntime{}).Normalize(path)
	if err != nil || unparsed != 4 || len(events) != 5 {
		t.Fatalf("events=%+v unparsed=%d err=%v", events, unparsed, err)
	}
	for _, event := range events[1:] {
		if event.Kind != "other" || strings.Contains(event.Text, "private") || strings.Contains(event.Text, "Tool-looking") {
			t.Fatalf("fabricated or leaked unknown record: %+v", event)
		}
	}
	sum, ok := (antigravityRuntime{}).Summarize((antigravityRuntime{}).Collect()[0])
	if !ok || sum.Title != "Read a file" {
		t.Fatalf("partial transcript lost valid prefix: %+v %v", sum, ok)
	}
}

func TestAntigravitySourceRejectsSymlinksAndOtherPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := antigravityBrainRoot()
	outside := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	if err := os.WriteFile(outside, []byte(`{"source":"USER_EXPLICIT"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, antigravityTestID, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, "transcript_full.jsonl")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	if jobs := (antigravityRuntime{}).Collect(); len(jobs) != 0 {
		t.Fatalf("followed a transcript symlink: %+v", jobs)
	}
	if _, _, _, err := (antigravityRuntime{}).Normalize(linked); err == nil {
		t.Fatal("normalized a symlink")
	}
	if _, _, _, err := (antigravityRuntime{}).Normalize(outside); err == nil {
		t.Fatal("normalized an unrelated path")
	}
}

func TestAntigravityTranscriptProjectionAndLimits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := antigravityTestPath(t, home, antigravityTestID)
	rows := antigravityTestRow(t, "USER_EXPLICIT", "USER_INPUT",
		"<USER_REQUEST>orbit phrase</USER_REQUEST><ADDITIONAL_METADATA>hidden metadata</ADDITIONAL_METADATA>") +
		antigravityTestRow(t, "MODEL", "PLANNER_RESPONSE", "gravity reply") +
		antigravityTestRow(t, "MODEL", "GENERIC", "secret generic prose")
	if err := os.WriteFile(path, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := antigravityRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || !discovery.Complete || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	session := discovery.Sessions[0]
	if session.ID != antigravityTestID || session.Summary.Title != "orbit phrase" ||
		len(session.Segments) != 1 || session.Generation == "" {
		t.Fatalf("session=%+v", session)
	}
	legacy := session
	legacy.Segments = append([]ProjectionSegment(nil), session.Segments...)
	legacy.Segments[0].UpdateMarker = fileProjectionMarker(legacy.Segments[0].Modified,
		legacy.Segments[0].SourceBytes)
	if session.Generation == projectionGeneration(legacy) {
		t.Fatal("parser revision did not change the source generation")
	}
	snapshot, err := runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	if err != nil || len(snapshot.Events) != 3 || snapshot.Unparsed != 1 ||
		snapshot.Events[0].Event.Text != "orbit phrase" ||
		snapshot.Events[1].Event.Text != "gravity reply" ||
		snapshot.Events[2].Event.Kind != "other" ||
		strings.Contains(snapshot.Events[2].Event.Text, "secret") {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	again, err := runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	if err != nil || again.Events[0].Lineage != snapshot.Events[0].Lineage {
		t.Fatalf("lineage changed: %+v err=%v", again, err)
	}
	limited, err := runtime.DiscoverTranscriptProjections(context.Background(),
		ProjectionReadLimits{MaxSourceBytes: 1})
	if err != nil || limited.Complete || len(limited.Limitations) != 1 ||
		limited.Limitations[0].Kind != ProjectionSourceTooLarge {
		t.Fatalf("limited=%+v err=%v", limited, err)
	}
	if err := os.WriteFile(path, []byte(rows+antigravityTestRow(t, "MODEL", "PLANNER_RESPONSE", "new")), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	if limitation, ok := projectionLimitation(err); !ok || limitation.Kind != ProjectionSourceMutated {
		t.Fatalf("mutation limitation=%+v err=%v", limitation, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runtime.DiscoverTranscriptProjections(cancelled, ProjectionReadLimits{})
	if limitation, ok := projectionLimitation(err); !ok || limitation.Kind != ProjectionReadCancelled {
		t.Fatalf("cancellation limitation=%+v err=%v", limitation, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	unsafeDiscovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || unsafeDiscovery.Complete || len(unsafeDiscovery.Sessions) != 0 ||
		len(unsafeDiscovery.Limitations) != 1 ||
		unsafeDiscovery.Limitations[0].Kind != ProjectionSourceUnreadable {
		t.Fatalf("unsafe discovery=%+v err=%v", unsafeDiscovery, err)
	}
	_, err = runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	if limitation, ok := projectionLimitation(err); !ok || limitation.Kind != ProjectionSourceUnreadable {
		t.Fatalf("symlink limitation=%+v err=%v", limitation, err)
	}
}

func TestAntigravityStructuredToolCallsStayOrderedAndBounded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := antigravityTestPath(t, home, antigravityTestID)
	largeCommand := strings.Repeat("x", 3000)
	rows := antigravityTestRow(t, "USER_EXPLICIT", "USER_INPUT",
		"<USER_REQUEST>Inspect the fixture</USER_REQUEST>") +
		antigravityTestToolRow(t, "I will check it.", []any{
			map[string]any{"name": "view_file", "args": map[string]any{"AbsolutePath": "/fixture/readme"}},
			map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": largeCommand}},
		}) +
		antigravityTestRow(t, "MODEL", "GENERIC", "Tool-looking result prose is not verified output") +
		antigravityTestToolRow(t, "", []any{42})
	if err := os.WriteFile(path, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	events, unparsed, _, err := (antigravityRuntime{}).Normalize(path)
	if err != nil || unparsed != 2 || len(events) != 6 {
		t.Fatalf("events=%+v unparsed=%d err=%v", events, unparsed, err)
	}
	wantKinds := []string{"user", "assistant", "tool_call", "tool_call", "other", "other"}
	for i, event := range events {
		if event.Seq != i+1 || event.Kind != wantKinds[i] {
			t.Fatalf("event %d=%+v", i, event)
		}
		if event.Kind == "tool_result" || strings.Contains(event.Text, "Tool-looking") {
			t.Fatalf("invented result or leaked opaque prose: %+v", event)
		}
	}
	if events[2].Name != "view_file" || !strings.Contains(events[2].Text, "/fixture/readme") ||
		events[3].Name != "run_command" || events[3].FullLen <= len(events[3].Text) ||
		len(events[3].Text) > transcriptCaps.tool+3 {
		t.Fatalf("tool events=%+v", events[2:4])
	}
	runtime := antigravityRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	first, err := runtime.ReadTranscriptProjection(context.Background(), discovery.Sessions[0], ProjectionReadLimits{})
	if err != nil || len(first.Events) != len(events) || first.Events[2].Event.Name != "view_file" {
		t.Fatalf("projection=%+v err=%v", first, err)
	}
	second, err := runtime.ReadTranscriptProjection(context.Background(), discovery.Sessions[0], ProjectionReadLimits{})
	if err != nil || first.Events[3].Lineage != second.Events[3].Lineage {
		t.Fatalf("unstable tool lineage: first=%+v second=%+v err=%v", first.Events[3], second.Events[3], err)
	}
}
