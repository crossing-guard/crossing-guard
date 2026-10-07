package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/internal/taskinput"
	"crossing-guard/internal/workspace"
	"crossing-guard/store"
)

func installTestRuntimeTasks(t *testing.T) *TaskApplicationService {
	t.Helper()
	dataDir := t.TempDir()
	index, err := store.Open(filepath.Join(dataDir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewTaskApplicationService(taskStoreRepository{index: index},
		NewTaskExecutionRegistry(), NewTaskSubscriberHub(), registeredTaskRuntime,
		func(string, string) (bool, error) { return true, nil }, dataDir)
	previousService, previousIndex := runtimeTasks, runtimeTaskIndex
	runtimeTasks, runtimeTaskIndex = service, index
	t.Cleanup(func() {
		runtimeTasks, runtimeTaskIndex = previousService, previousIndex
		_ = index.Close()
	})
	return service
}

func TestTaskStateMachineKeepsTerminalStatesMonotonic(t *testing.T) {
	machine := TaskStateMachine{}
	if err := machine.Validate(TaskQueued, TaskStarting); err != nil {
		t.Fatal(err)
	}
	for _, terminal := range []TaskLifecycle{TaskCompleted, TaskInterrupted, TaskFailed, TaskUnknown} {
		if err := machine.Validate(terminal, TaskRunning); err == nil {
			t.Errorf("terminal %s returned to running", terminal)
		}
	}
}

type lifecycleFixtureDriver struct{ command string }

func (driver lifecycleFixtureDriver) BuildCmd(ChatRequest, ChatLaunchContext) (*exec.Cmd, error) {
	return exec.Command("/bin/sh", "-c", driver.command), nil
}

func (lifecycleFixtureDriver) ProjectEvent(object map[string]any) []ChatEvent {
	return []ChatEvent{{"type": "text", "text": anyString(object["text"])}}
}

type buildFailureDriver struct{}

func (buildFailureDriver) BuildCmd(ChatRequest, ChatLaunchContext) (*exec.Cmd, error) {
	return nil, errors.New("fixture command unavailable")
}

func (buildFailureDriver) ProjectEvent(map[string]any) []ChatEvent { return nil }

type workspaceFixture struct {
	root     string
	acquired []string
	released []string
}

func (fixture *workspaceFixture) AcquireForConsumer(selectionID, ownerKind, ownerID string) (workspace.Candidate, int64, error) {
	fixture.acquired = append(fixture.acquired, selectionID+":"+ownerKind+":"+ownerID)
	return workspace.Candidate{Root: fixture.root}, 7, nil
}

func (fixture *workspaceFixture) ReleaseConsumer(selectionID, ownerKind, ownerID string) error {
	fixture.released = append(fixture.released, selectionID+":"+ownerKind+":"+ownerID)
	return nil
}

type contextualFixtureDriver struct{ launches chan ChatLaunchContext }

func (driver contextualFixtureDriver) CanonicalizeChatRequest(req ChatRequest) (ChatRequest, error) {
	req.ExtraArgs = ""
	return req, nil
}

func (driver contextualFixtureDriver) BuildCmd(_ ChatRequest, launch ChatLaunchContext) (*exec.Cmd, error) {
	driver.launches <- launch
	return exec.Command("/bin/sh", "-c", "true"), nil
}

func (contextualFixtureDriver) ProjectEvent(map[string]any) []ChatEvent { return nil }

type inputFixtureDriver struct{ launches chan ChatLaunchContext }

func (driver inputFixtureDriver) ValidateChatInputs(_ ChatRequest, inputs []taskinput.ResolvedInput) error {
	if len(inputs) != 1 || inputs[0].Kind != taskinput.KindText {
		return errors.New("fixture input mismatch")
	}
	return nil
}

func (driver inputFixtureDriver) BuildCmd(_ ChatRequest, launch ChatLaunchContext) (*exec.Cmd, error) {
	driver.launches <- launch
	return exec.Command("/bin/sh", "-c", "true"), nil
}

func (inputFixtureDriver) ProjectEvent(map[string]any) []ChatEvent { return nil }

func TestTaskLaunchContextIsDaemonOnlyAndCanonicalizationPrecedesDigest(t *testing.T) {
	service := installTestRuntimeTasks(t)
	launches := make(chan ChatLaunchContext, 1)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"fixture": contextualFixtureDriver{launches: launches}}
	t.Cleanup(func() { chatDrivers = original })
	req := ChatRequest{Runtime: "fixture", Prompt: "work", Cwd: t.TempDir(), ExtraArgs: "legacy-alias"}
	task, created, err := service.Create(req, "fixture-context-key")
	if err != nil || !created {
		t.Fatalf("task=%+v created=%v err=%v", task, created, err)
	}
	select {
	case launch := <-launches:
		if launch.TaskID != task.ID || launch.DataDir != service.launchDataDir {
			t.Fatalf("launch context = %+v want task=%q data=%q", launch, task.ID, service.launchDataDir)
		}
	case <-time.After(time.Second):
		t.Fatal("launch context not delivered")
	}
	retry := req
	retry.ExtraArgs = ""
	second, created, err := service.Create(retry, "fixture-context-key")
	if err != nil || created || second.ID != task.ID {
		t.Fatalf("canonical retry=%+v created=%v err=%v", second, created, err)
	}
}

func TestTaskInputsPreflightClaimLaunchEventCleanupAndIdempotentRetry(t *testing.T) {
	dataDir := t.TempDir()
	inputs, err := taskinput.NewService(filepath.Join(dataDir, "inputs"), daemonTaskInputConfig(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	scope, input, err := inputs.Stage(context.Background(), "", taskinput.SourcePicker, "notes.txt", bytes.NewBufferString("marker 7319"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.Open(filepath.Join(dataDir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	launches := make(chan ChatLaunchContext, 1)
	driver := inputFixtureDriver{launches: launches}
	service := NewTaskApplicationServiceWithInputs(taskStoreRepository{index: index}, NewTaskExecutionRegistry(),
		NewTaskSubscriberHub(), func(runtime string) (ChatDriver, bool) { return driver, runtime == "fixture" },
		func(string, string) (bool, error) { return true, nil }, inputs, dataDir)
	req := ChatRequest{Runtime: "fixture", Prompt: "inspect", Cwd: dataDir,
		InputScopeID: scope.ID, InputIDs: []string{input.ID}}
	task, created, err := service.Create(req, "fixture-input-key")
	if err != nil || !created {
		t.Fatalf("create=%+v created=%v err=%v", task, created, err)
	}
	select {
	case launch := <-launches:
		if len(launch.Inputs) != 1 || launch.Inputs[0].Path == "" {
			t.Fatalf("launch inputs=%+v", launch.Inputs)
		}
	case <-time.After(time.Second):
		t.Fatal("input launch not observed")
	}
	wantTaskState(t, service, task.ID, TaskCompleted, time.Second)
	if _, err := inputs.List(scope.ID); err == nil {
		t.Fatal("terminal task did not consume private inputs")
	}
	retry, created, err := service.Create(req, "fixture-input-key")
	if err != nil || created || retry.ID != task.ID {
		t.Fatalf("retry=%+v created=%v err=%v", retry, created, err)
	}
	events, err := service.Events(task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events)
	if !strings.Contains(string(encoded), `"type":"inputs"`) || strings.Contains(string(encoded), scope.ID) || strings.Contains(string(encoded), filepath.Join(dataDir, "inputs")) {
		t.Fatalf("public input event missing or leaked private authority: %s", encoded)
	}
}

func TestTaskSurvivesSubscriberDisconnectAndCanBeInterrupted(t *testing.T) {
	service := installTestRuntimeTasks(t)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"fixture": lifecycleFixtureDriver{command: "sleep 0.15; printf '%s\\n' '{\"text\":\"finished\"}'"}}
	t.Cleanup(func() { chatDrivers = original })
	cwd := t.TempDir()

	task, created, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "work", Cwd: cwd}, "fixture-send-1")
	if err != nil || !created {
		t.Fatalf("create=%+v created=%v err=%v", task, created, err)
	}
	subscription, _, err := service.Subscribe(task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	service.Unsubscribe(subscription.ID)
	wantTaskState(t, service, task.ID, TaskCompleted, 2*time.Second)

	chatDrivers["fixture"] = lifecycleFixtureDriver{command: "sleep 10; printf '%s\\n' '{\"text\":\"too-late\"}'"}
	second, _, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "wait", Cwd: cwd}, "fixture-send-2")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, service, second.ID, TaskRunning, time.Second)
	interruptStarted := time.Now()
	if _, err := service.Interrupt(second.ID); err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, service, second.ID, TaskInterrupted, time.Second)
	if elapsed := time.Since(interruptStarted); elapsed > time.Second {
		t.Fatalf("interrupt waited for child process: %s", elapsed)
	}
}

func TestTaskCreateIsIdempotent(t *testing.T) {
	service := installTestRuntimeTasks(t)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"fixture": lifecycleFixtureDriver{command: "sleep 0.05"}}
	t.Cleanup(func() { chatDrivers = original })
	req := ChatRequest{Runtime: "fixture", Prompt: "same", Cwd: t.TempDir()}
	first, created, err := service.Create(req, "fixture-same-key")
	if err != nil || !created {
		t.Fatalf("first created=%v err=%v", created, err)
	}
	second, created, err := service.Create(req, "fixture-same-key")
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("retry=%+v created=%v err=%v", second, created, err)
	}
	changed := req
	changed.Prompt = "different"
	if _, _, err := service.Create(changed, "fixture-same-key"); !errors.Is(err, store.ErrRuntimeTaskIdempotencyConflict) {
		t.Fatalf("conflict=%v", err)
	}
}

func TestTaskCatalogIdentityValidationAndQueuedPublication(t *testing.T) {
	service := installTestRuntimeTasks(t)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"fixture": lifecycleFixtureDriver{command: "sleep 10"}}
	t.Cleanup(func() { chatDrivers = original })
	cwd := t.TempDir()
	service.catalog = func(runtime, id string) (bool, error) {
		return runtime == "fixture" && id == "catalog-1", nil
	}
	if _, _, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "bad", Cwd: cwd,
		CatalogSessionID: "missing"}, "fixture-missing-catalog"); err == nil {
		t.Fatal("accepted an unverified catalog identity")
	}
	task, created, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "queued", Cwd: cwd,
		CatalogSessionID: "catalog-1", SessionID: "native-1"}, "fixture-valid-catalog")
	if err != nil || !created || task.CatalogSessionID != "catalog-1" {
		t.Fatalf("task=%+v created=%v err=%v", task, created, err)
	}
	events, err := service.Events(task.ID, 0, 10)
	if err != nil || len(events) == 0 || events[0].Kind != "task.queued" || events[0].Sequence != 1 {
		t.Fatalf("queued event=%+v err=%v", events, err)
	}
	_, _ = service.Interrupt(task.ID)
}

func TestQueuedTaskCanBeInterruptedBeforeVendorStarts(t *testing.T) {
	service := installTestRuntimeTasks(t)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"fixture": lifecycleFixtureDriver{command: "sleep 10"}}
	t.Cleanup(func() { chatDrivers = original })
	cwd := t.TempDir()
	var tasks []RuntimeTask
	for index, key := range []string{"fixture-queue-1", "fixture-queue-2", "fixture-queue-3"} {
		task, created, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "wait", Cwd: cwd}, key)
		if err != nil || !created {
			t.Fatalf("create %d created=%v err=%v", index, created, err)
		}
		tasks = append(tasks, task)
	}
	wantTaskState(t, service, tasks[0].ID, TaskRunning, time.Second)
	wantTaskState(t, service, tasks[1].ID, TaskRunning, time.Second)
	wantTaskState(t, service, tasks[2].ID, TaskQueued, time.Second)
	if _, err := service.Interrupt(tasks[2].ID); err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, service, tasks[2].ID, TaskInterrupted, time.Second)
	for _, task := range tasks[:2] {
		_, _ = service.Interrupt(task.ID)
	}
}

func TestBuildCommandFailureStopsBeforeVendorTurn(t *testing.T) {
	service := installTestRuntimeTasks(t)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"fixture": buildFailureDriver{}}
	t.Cleanup(func() { chatDrivers = original })
	task, created, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "work", Cwd: t.TempDir()}, "fixture-build-failure")
	if err != nil || !created || task.Lifecycle != TaskFailed {
		t.Fatalf("task=%+v created=%v err=%v", task, created, err)
	}
	events, err := service.Events(task.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "task.queued" || events[1].Kind != "task.failed" {
		t.Fatalf("events=%+v", events)
	}
	if got := anyString(events[1].Payload["type"]); got != "error" {
		t.Fatalf("failed payload type=%q", got)
	}
}

func TestWorkspaceSelectionOwnsTaskCWDAndLeaseLifecycle(t *testing.T) {
	service := installTestRuntimeTasks(t)
	selectedRoot := t.TempDir()
	selection := &workspaceFixture{root: selectedRoot}
	service.SetWorkspaceSelectionService(selection)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"fixture": buildFailureDriver{}}
	t.Cleanup(func() { chatDrivers = original })

	if _, _, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "work", Cwd: t.TempDir(),
		WorkspaceSelectionID: "selection-1"}, "workspace-cwd-conflict"); err == nil {
		t.Fatal("raw cwd was accepted alongside workspace selection")
	}
	task, created, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "work",
		WorkspaceSelectionID: "selection-1"}, "workspace-selected-task")
	if err != nil || !created || task.WorkingDirectory != selectedRoot ||
		task.WorkspaceSelectionID != "selection-1" || task.WorkspaceSelectionVersion != 7 {
		t.Fatalf("workspace task=%+v created=%v err=%v", task, created, err)
	}
	if len(selection.acquired) != 1 || len(selection.released) != 1 {
		t.Fatalf("workspace lease acquired=%v released=%v", selection.acquired, selection.released)
	}
	retry, created, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "work",
		WorkspaceSelectionID: "selection-1"}, "workspace-selected-task")
	if err != nil || created || retry.ID != task.ID || len(selection.acquired) != 1 {
		t.Fatalf("workspace retry=%+v created=%v err=%v acquired=%v", retry, created, err, selection.acquired)
	}
}

func TestTaskRecoveryDropsUnprovenControlAuthority(t *testing.T) {
	service := installTestRuntimeTasks(t)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"fixture": lifecycleFixtureDriver{command: "sleep 10"}}
	t.Cleanup(func() { chatDrivers = original })
	task, _, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "wait", Cwd: t.TempDir()}, "fixture-recover-key")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, service, task.ID, TaskRunning, time.Second)
	// A fresh service has no access to the old registry's process handle.
	recovered := NewTaskApplicationService(service.repository, NewTaskExecutionRegistry(),
		NewTaskSubscriberHub(), registeredTaskRuntime, service.catalog, service.launchDataDir)
	if err := recovered.RecoverLostTasks(); err != nil {
		t.Fatal(err)
	}
	unknown := wantTaskState(t, recovered, task.ID, TaskUnknown, time.Second)
	if unknown.Controllable || unknown.Freshness != "unknown" {
		t.Fatalf("recovered task retained unproven live/control state: %+v", unknown)
	}
	_, _ = service.executions.Interrupt(task.ID)
}

func TestTaskDeltaBufferCoalescesChunks(t *testing.T) {
	flushed := make(chan ChatEvent, 2)
	buffer := NewTaskDeltaBuffer(func(_, _, _ string, payload ChatEvent) { flushed <- payload })
	buffer.Add("task", "fixture", "message.delta", ChatEvent{"type": "delta", "text": "one"})
	buffer.Add("task", "fixture", "message.delta", ChatEvent{"type": "delta", "text": " two"})
	buffer.FlushTask("task")
	select {
	case payload := <-flushed:
		if payload["text"] != "one two" {
			t.Fatalf("coalesced payload=%+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("coalesced delta did not flush")
	}
	select {
	case extra := <-flushed:
		t.Fatalf("delta flushed more than once: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestTaskIngestFlushesDraftsBeforeCompletedTextAndTools(t *testing.T) {
	service := installTestRuntimeTasks(t)
	task, _, _, err := service.repository.Create(taskCreateRecord{ID: "ordered-stream-task",
		ConsoleScope: "fixture", IdempotencyKey: "ordered-stream-key", RequestDigest: "fixture",
		Runtime: "future-fixture", WorkingDirectory: t.TempDir(), CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []ChatEvent{
		{"type": "delta", "text": "a"}, {"type": "delta", "text": " b"},
		{"type": "text", "text": "a b"}, {"type": "thinking_delta", "text": "thinking"},
		{"type": "tool", "name": "fixture-tool"}, {"type": "delta", "text": "tail"},
		{"type": "text", "text": "final"},
	} {
		service.ingest(task.ID, task.Runtime, event)
	}
	service.deltas.FlushTask(task.ID)
	events, err := service.Events(task.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"task.queued", "message.delta", "message.completed", "reasoning.delta", "tool.started", "message.delta", "message.completed"}
	if len(events) != len(want) {
		t.Fatalf("events=%+v", events)
	}
	for i, kind := range want {
		if events[i].Kind != kind {
			t.Fatalf("event %d=%s want=%s", i, events[i].Kind, kind)
		}
	}
	if events[1].Payload["text"] != "a b" || events[5].Payload["text"] != "tail" {
		t.Fatalf("coalesced drafts=%+v", events)
	}
}

func TestTaskDeltaFlushWaitsForInFlightTimerPublication(t *testing.T) {
	entered, release, settled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	buffer := NewTaskDeltaBuffer(func(_, _, _ string, _ ChatEvent) {
		close(entered)
		<-release
	})
	buffer.Add("task", "fixture", "message.delta", ChatEvent{"type": "delta", "text": "draft"})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("timer did not publish")
	}
	go func() { buffer.FlushTask("task"); close(settled) }()
	select {
	case <-settled:
		t.Fatal("flush returned before timer publication settled")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-settled:
	case <-time.After(3 * time.Second):
		t.Fatal("flush did not settle")
	}
}

func TestTaskDeltaExpiredTimerCannotFlushReplacementDraft(t *testing.T) {
	var texts []string
	buffer := NewTaskDeltaBuffer(func(_, _, _ string, event ChatEvent) { texts = append(texts, anyString(event["text"])) })
	key := "task\x00message.delta"
	buffer.Add("task", "fixture", "message.delta", ChatEvent{"type": "delta", "text": strings.Repeat("a", taskDeltaFlushBytes)})
	buffer.mu.Lock()
	old := buffer.entries[key]
	old.timer.Stop()
	buffer.mu.Unlock()
	buffer.Add("task", "fixture", "message.delta", ChatEvent{"type": "delta", "text": "new"})
	buffer.flushKey(key, old)
	buffer.FlushTask("task")
	if len(texts) != 2 || len(texts[0]) != taskDeltaFlushBytes || texts[1] != "new" {
		t.Fatalf("replacement draft was lost: count=%d", len(texts))
	}
}

func wantTaskState(t *testing.T, service *TaskApplicationService, id string, want TaskLifecycle, timeout time.Duration) RuntimeTask {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, found, err := service.Task(id)
		if err != nil {
			t.Fatal(err)
		}
		if found && task.Lifecycle == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _, _ := service.Task(id)
	t.Fatalf("task %s state=%s want=%s", id, task.Lifecycle, want)
	return RuntimeTask{}
}
