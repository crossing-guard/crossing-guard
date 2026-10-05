package harvest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type projectionCapableStub struct{ stubRuntime }

func (projectionCapableStub) Name() string { return "projection-capable-stub" }
func (projectionCapableStub) DiscoverTranscriptProjections(context.Context,
	ProjectionReadLimits) (ProjectionDiscovery, error) {
	return ProjectionDiscovery{Sessions: []ProjectionSession{}, Complete: true,
		Limitations: []ProjectionLimitation{}}, nil
}
func (projectionCapableStub) ReadTranscriptProjection(context.Context, ProjectionSession,
	ProjectionReadLimits) (ProjectionSnapshot, error) {
	return ProjectionSnapshot{}, nil
}
func (projectionCapableStub) TranscriptProjectionGeneration(context.Context, ProjectionSession,
	ProjectionReadLimits) (string, error) {
	return "fixture", nil
}

func TestTranscriptProjectionCapabilityIsAdditive(t *testing.T) {
	var _ TranscriptProjectionSource = claudeRuntime{}
	var _ TranscriptProjectionSource = codexRuntime{}
	var _ TranscriptProjectionSource = opencodeRuntime{}
	if _, ok := any(stubRuntime{}).(TranscriptProjectionSource); ok {
		t.Fatal("a runtime without the optional capability unexpectedly satisfies it")
	}

	stub := projectionCapableStub{}
	register(stub)
	t.Cleanup(func() { delete(runtimes, stub.Name()) })
	var found bool
	for _, runtime := range Runtimes() {
		if runtime.Name() != stub.Name() {
			continue
		}
		_, found = runtime.(TranscriptProjectionSource)
	}
	if !found {
		t.Fatal("newly registered capable runtime was not discoverable without a generic switch")
	}
}

func writeClaudeProjectionFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "11111111-1111-1111-1111-111111111111"
	dir := filepath.Join(home, ".claude", "projects", "-work-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	body := `{"type":"user","sessionId":"` + id + `","timestamp":"2026-08-28T10:00:00Z","message":{"role":"user","content":"first prompt"}}` + "\n" +
		`{"type":"assistant","sessionId":"` + id + `","timestamp":"2026-08-28T10:00:01Z","message":{"role":"assistant","model":"claude-test","content":[{"type":"text","text":"first answer"}],"usage":{"input_tokens":3,"output_tokens":2}}}` + "\n" +
		`{"type":"custom-title","customTitle":"Sprint retro fixture"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, id
}

func TestClaudeProjectionReusesParserAndRejectsMutation(t *testing.T) {
	path, id := writeClaudeProjectionFixture(t)
	runtime := claudeRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || !discovery.Complete || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	session := discovery.Sessions[0]
	if session.ID != id || session.Summary.Title != "Sprint retro fixture" || len(session.Segments) != 1 {
		t.Fatalf("session=%+v", session)
	}
	snapshot, err := runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	direct, directUnparsed, _, err := normalizeClaude(path, transcriptCaps)
	if err != nil {
		t.Fatal(err)
	}
	projected := make([]CanonicalEvent, 0, len(snapshot.Events))
	lineage := map[string]bool{}
	for index, event := range snapshot.Events {
		projected = append(projected, event.Event)
		if event.Ordinal != index || event.Lineage == "" || lineage[event.Lineage] {
			t.Fatalf("unstable projection event: %+v", event)
		}
		lineage[event.Lineage] = true
	}
	if !reflect.DeepEqual(projected, direct) || snapshot.Unparsed != directUnparsed {
		t.Fatalf("projection parser drift\nprojected=%+v\ndirect=%+v", projected, direct)
	}
	if generation, err := runtime.TranscriptProjectionGeneration(context.Background(), session,
		ProjectionReadLimits{}); err != nil || generation != session.Generation {
		t.Fatalf("generation=%q want=%q err=%v", generation, session.Generation, err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(`{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"changed"}}` + "\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append=%v close=%v", writeErr, closeErr)
	}
	_, err = runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	assertProjectionLimitation(t, err, ProjectionSourceMutated)
}

func writeCodexProjectionSegment(t *testing.T, dir, name, thread, message string, modified time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name+".jsonl")
	body := `{"timestamp":"2026-08-28T10:00:00Z","payload":{"type":"session_meta","payload":{"session_id":"` + thread + `","cwd":"/work/repo"}}}` + "\n" +
		`{"timestamp":"2026-08-28T10:00:01Z","payload":{"type":"event_msg","payload":{"type":"user_message","message":"` + message + `"}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexProjectionAggregatesAllResumeSegmentsDeterministically(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "28")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	thread := "22222222-2222-2222-2222-222222222222"
	base := time.Unix(1_700_000_000, 0)
	firstPath := writeCodexProjectionSegment(t, dir, "rollout-a", thread, "first segment", base)
	secondPath := writeCodexProjectionSegment(t, dir, "rollout-b", thread, "second segment", base.Add(time.Second))

	runtime := codexRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || !discovery.Complete || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	session := discovery.Sessions[0]
	if session.ID != thread || len(session.Segments) != 2 || session.Summary.UserTurns != 2 {
		t.Fatalf("aggregate=%+v", session)
	}
	snapshot, err := runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, event := range snapshot.Events {
		if event.Event.Kind == "user" {
			users = append(users, event.Event.Text)
		}
	}
	if !reflect.DeepEqual(users, []string{"first segment", "second segment"}) {
		t.Fatalf("segment order=%v", users)
	}
	var direct []CanonicalEvent
	for _, path := range []string{firstPath, secondPath} {
		events, _, _, normalizeErr := normalizeCodex(path, transcriptCaps)
		if normalizeErr != nil {
			t.Fatal(normalizeErr)
		}
		direct = append(direct, events...)
	}
	projected := make([]CanonicalEvent, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		projected = append(projected, event.Event)
	}
	if !reflect.DeepEqual(projected, direct) {
		t.Fatalf("codex projection parser drift\nprojected=%+v\ndirect=%+v", projected, direct)
	}
	again, err := runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for index := range snapshot.Events {
		if snapshot.Events[index].Lineage != again.Events[index].Lineage {
			t.Fatalf("lineage changed at %d: %q != %q", index,
				snapshot.Events[index].Lineage, again.Events[index].Lineage)
		}
	}
	firstInfo, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	aggregateLimit := max(firstInfo.Size(), secondInfo.Size()) + 1
	limited, err := runtime.DiscoverTranscriptProjections(context.Background(),
		ProjectionReadLimits{MaxSourceBytes: aggregateLimit})
	if err != nil || limited.Complete || len(limited.Sessions) != 1 || len(limited.Limitations) != 1 {
		t.Fatalf("aggregate-limited discovery=%+v err=%v", limited, err)
	}
	if got := limited.Limitations[0]; got.Kind != ProjectionSourceTooLarge || got.SessionID != thread ||
		got.ObservedBytes != firstInfo.Size()+secondInfo.Size() || got.LimitBytes != aggregateLimit {
		t.Fatalf("aggregate limitation=%+v", got)
	}
}

func TestProjectionGenerationIncludesCatalogAndResumeIdentity(t *testing.T) {
	base := ProjectionSession{Runtime: "codex", ID: "canonical", Summary: SessionSummary{
		ID: "rollout-a", ResumeID: "canonical", Title: "same", Cwd: "/repo",
	}}
	first := projectionGeneration(base)
	base.Summary.ID = "rollout-b"
	if second := projectionGeneration(base); second == first {
		t.Fatal("catalog identity change did not change projection generation")
	}
	base.Summary.ID = "rollout-a"
	base.Summary.ResumeID = "different-resume"
	if third := projectionGeneration(base); third == first {
		t.Fatal("resume identity change did not change projection generation")
	}
}

func TestFileProjectionEnforcesDefaultAggregateInputLimitBeforeParsing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "-work-large")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "33333333-3333-3333-3333-333333333333.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(DefaultProjectionMaxSourceBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	discovery, err := (claudeRuntime{}).DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || discovery.Complete || len(discovery.Sessions) != 0 || len(discovery.Limitations) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	if got := discovery.Limitations[0]; got.Kind != ProjectionSourceTooLarge ||
		got.LimitBytes != DefaultProjectionMaxSourceBytes || got.ObservedBytes != DefaultProjectionMaxSourceBytes+1 {
		t.Fatalf("limitation=%+v", got)
	}
}

func TestProjectionCancellationIsTyped(t *testing.T) {
	_, _ = writeClaudeProjectionFixture(t)
	runtime := claudeRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runtime.ReadTranscriptProjection(ctx, discovery.Sessions[0], ProjectionReadLimits{})
	assertProjectionLimitation(t, err, ProjectionReadCancelled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
}

func TestFileProjectionDoesNotSwallowCancellationDuringParserRead(t *testing.T) {
	_, _ = writeClaudeProjectionFixture(t)
	runtime := claudeRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = readFileTranscriptProjection(ctx, runtime, discovery.Sessions[0], ProjectionReadLimits{},
		func(source string, reader io.Reader) ([]CanonicalEvent, int, *SessionUsage, error) {
			var first [1]byte
			if _, readErr := io.ReadFull(reader, first[:]); readErr != nil {
				return nil, 0, nil, readErr
			}
			cancel()
			return normalizeClaudeReader(source, reader, transcriptCaps)
		})
	assertProjectionLimitation(t, err, ProjectionReadCancelled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
}

func TestFileProjectionDetectsMutationAfterBoundedRead(t *testing.T) {
	path, _ := writeClaudeProjectionFixture(t)
	runtime := claudeRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	_, err = readFileTranscriptProjection(context.Background(), runtime, discovery.Sessions[0],
		ProjectionReadLimits{}, func(source string, reader io.Reader) ([]CanonicalEvent, int, *SessionUsage, error) {
			events, unparsed, usage, normalizeErr := normalizeClaudeReader(source, reader, transcriptCaps)
			if normalizeErr != nil {
				return nil, 0, nil, normalizeErr
			}
			file, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				return nil, 0, nil, openErr
			}
			_, writeErr := file.WriteString("\n")
			closeErr := file.Close()
			if writeErr != nil {
				return nil, 0, nil, writeErr
			}
			if closeErr != nil {
				return nil, 0, nil, closeErr
			}
			return events, unparsed, usage, nil
		})
	assertProjectionLimitation(t, err, ProjectionSourceMutated)
}

func TestFileProjectionReportsUnreadableWithoutPartialSnapshot(t *testing.T) {
	path, _ := writeClaudeProjectionFixture(t)
	runtime := claudeRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.ReadTranscriptProjection(context.Background(), discovery.Sessions[0],
		ProjectionReadLimits{})
	assertProjectionLimitation(t, err, ProjectionSourceUnreadable)
	if len(snapshot.Events) != 0 || snapshot.Generation != "" {
		t.Fatalf("unreadable source returned partial snapshot: %+v", snapshot)
	}
}

func TestOpenCodeProjectionUsesLogicalSessionBytesAndGeneration(t *testing.T) {
	_, id := openCodeFixture(t)
	runtime := opencodeRuntime{}
	discovery, err := runtime.DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil || !discovery.Complete || len(discovery.Sessions) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	session := discovery.Sessions[0]
	if session.ID != id || session.SourceBytes <= 0 || session.SourceBytes >= DefaultProjectionMaxSourceBytes {
		t.Fatalf("logical source accounting=%+v", session)
	}
	snapshot, err := runtime.ReadTranscriptProjection(context.Background(), session, ProjectionReadLimits{})
	if err != nil || len(snapshot.Events) != 5 || snapshot.SourceBytes != session.SourceBytes {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	segment := session.Segments[0]
	direct, _, _, err := runtime.NormalizeSession(SessionRef{Runtime: runtime.Name(), ID: session.ID,
		Source: segment.SourceRef, Segment: segment.ID, UpdateMarker: segment.UpdateMarker}, false)
	if err != nil {
		t.Fatal(err)
	}
	projected := make([]CanonicalEvent, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		projected = append(projected, event.Event)
	}
	if !reflect.DeepEqual(projected, direct) {
		t.Fatalf("OpenCode projection parser drift\nprojected=%+v\ndirect=%+v", projected, direct)
	}
	_, err = runtime.ReadTranscriptProjection(context.Background(), session,
		ProjectionReadLimits{MaxSourceBytes: session.SourceBytes - 1})
	assertProjectionLimitation(t, err, ProjectionSourceTooLarge)
}

func assertProjectionLimitation(t *testing.T, err error, want ProjectionLimitationKind) {
	t.Helper()
	var projectionErr *ProjectionError
	if !errors.As(err, &projectionErr) || projectionErr.Limitation.Kind != want {
		t.Fatalf("error=%v limitation=%+v want=%s", err, projectionErr, want)
	}
}
