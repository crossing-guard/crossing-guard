package daemon

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type taskHTTPRepositoryStub struct {
	order       []string
	events      []TaskEvent
	subscribers *TaskSubscriberHub
	minimum     int64
	maximum     int64
}

func (r *taskHTTPRepositoryStub) Create(taskCreateRecord) (RuntimeTask, TaskEvent, bool, error) {
	return RuntimeTask{}, TaskEvent{}, false, nil
}
func (r *taskHTTPRepositoryStub) ByID(string) (RuntimeTask, bool, error) {
	return RuntimeTask{}, false, nil
}
func (r *taskHTTPRepositoryStub) ByIdempotency(string, string) (RuntimeTask, bool, error) {
	return RuntimeTask{}, false, nil
}
func (r *taskHTTPRepositoryStub) List(string, string, int) ([]RuntimeTask, error) {
	r.order = append(r.order, "list")
	return []RuntimeTask{}, nil
}
func (r *taskHTTPRepositoryStub) ListActive() ([]RuntimeTask, error) { return nil, nil }
func (r *taskHTTPRepositoryStub) Transition(string, TaskLifecycle, TaskLifecycle, int64, string) (RuntimeTask, bool, error) {
	return RuntimeTask{}, false, nil
}
func (r *taskHTTPRepositoryStub) TransitionWithEvent(string, TaskLifecycle, TaskLifecycle, int64, string, string, string, map[string]any) (RuntimeTask, TaskEvent, bool, error) {
	return RuntimeTask{}, TaskEvent{}, false, nil
}
func (r *taskHTTPRepositoryStub) SetNativeSession(string, string, int64) (bool, error) {
	return true, nil
}
func (r *taskHTTPRepositoryStub) Append(string, string, string, map[string]any, int64) (TaskEvent, error) {
	return TaskEvent{}, nil
}
func (r *taskHTTPRepositoryStub) Events(string, int64, int) ([]TaskEvent, error) { return nil, nil }
func (r *taskHTTPRepositoryStub) EventsAfter(after int64, limit int) ([]TaskEvent, error) {
	page := make([]TaskEvent, 0, limit)
	for _, event := range r.events {
		if event.EventID <= after || len(page) >= limit {
			continue
		}
		page = append(page, event)
		if r.subscribers != nil {
			r.subscribers.Publish(event) // force snapshot/replay attachment overlap
		}
	}
	return page, nil
}
func (r *taskHTTPRepositoryStub) Watermark() (int64, error) {
	r.order = append(r.order, "watermark")
	return r.maximum, nil
}
func (r *taskHTTPRepositoryStub) EventRange() (int64, int64, error) {
	return r.minimum, r.maximum, nil
}
func (r *taskHTTPRepositoryStub) DeleteExpired(int64) (int64, error) { return 0, nil }

func installTaskHTTPStub(t *testing.T, repository *taskHTTPRepositoryStub) {
	t.Helper()
	hub := NewTaskSubscriberHub()
	repository.subscribers = hub
	service := NewTaskApplicationService(repository, NewTaskExecutionRegistry(), hub,
		registeredTaskRuntime, func(string, string) (bool, error) { return true, nil }, "")
	previous := runtimeTasks
	runtimeTasks = service
	t.Cleanup(func() { runtimeTasks = previous })
}

func TestRuntimeTaskSnapshotCapturesWatermarkBeforeProjection(t *testing.T) {
	repository := &taskHTTPRepositoryStub{maximum: 7}
	installTaskHTTPStub(t, repository)
	recorder := httptest.NewRecorder()
	handleRuntimeTaskList(recorder, httptest.NewRequest("GET", "/api/runtime-tasks", nil))
	if recorder.Code != 200 || len(repository.order) < 2 || repository.order[0] != "watermark" || repository.order[1] != "list" {
		t.Fatalf("status=%d order=%v body=%s", recorder.Code, repository.order, recorder.Body.String())
	}
}

func TestRuntimeTaskStreamDeduplicatesReplayAttachmentRace(t *testing.T) {
	event := TaskEvent{SchemaVersion: 1, EventID: 1, TaskID: "task", Runtime: "fixture",
		Sequence: 1, Kind: "task.started", SourceKind: "fixture", EvidenceClass: "observed",
		Freshness: "live", Payload: map[string]any{"type": "spawn"}}
	repository := &taskHTTPRepositoryStub{events: []TaskEvent{event}, minimum: 1, maximum: 1}
	installTaskHTTPStub(t, repository)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/api/runtime-tasks/stream?after=0", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handleRuntimeTaskStream(recorder, request); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	body := recorder.Body.String()
	if strings.Count(body, "id: 1\n") != 1 || !strings.Contains(body, "event: task\n") {
		t.Fatalf("stream did not deduplicate replay/live overlap: %q", body)
	}
}

func TestRuntimeTaskStreamResetsImpossibleCursor(t *testing.T) {
	repository := &taskHTTPRepositoryStub{minimum: 1, maximum: 2}
	installTaskHTTPStub(t, repository)
	recorder := httptest.NewRecorder()
	handleRuntimeTaskStream(recorder, httptest.NewRequest("GET", "/api/runtime-tasks/stream?after=99", nil))
	if body := recorder.Body.String(); !strings.Contains(body, "event: reset") || !strings.Contains(body, `"through_event_id":2`) {
		t.Fatalf("missing explicit reset: %q", body)
	}
}

func TestRuntimeTaskStreamResetsReplayBeyondBound(t *testing.T) {
	events := make([]TaskEvent, 2001)
	for index := range events {
		events[index] = TaskEvent{SchemaVersion: 1, EventID: int64(index + 1),
			TaskID: "task", Sequence: int64(index + 1), Kind: "task.activity"}
	}
	repository := &taskHTTPRepositoryStub{events: events, minimum: 1, maximum: 2001}
	installTaskHTTPStub(t, repository)
	recorder := httptest.NewRecorder()
	handleRuntimeTaskStream(recorder, httptest.NewRequest("GET", "/api/runtime-tasks/stream?after=0", nil))
	if body := recorder.Body.String(); !strings.Contains(body, "event: reset") ||
		!strings.Contains(body, "replay exceeds the bounded window") || strings.Contains(body, "event: task") {
		t.Fatalf("oversized replay was not reset atomically: %q", body)
	}
}
