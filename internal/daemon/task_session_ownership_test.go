package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Part A: a session another process is using is refused at creation unless the
// person overrides; the refusal is typed so the console can offer "send anyway".
func TestTaskCreationRefusesASessionInUseElsewhere(t *testing.T) {
	service := installTestRuntimeTasks(t)
	prev := taskSessionInUse
	calls := 0
	taskSessionInUse = func(_ time.Time, runtime, catalogID, nativeID string) (bool, error) {
		calls++
		return nativeID == "busy-session", nil
	}
	defer func() { taskSessionInUse = prev }()

	// A binary that cannot exist: an admitted send must never spawn a real
	// vendor process from a unit test, and the task must be terminal before
	// the test's store closes.
	base := ChatRequest{Runtime: "claude", Prompt: "hello", Cwd: t.TempDir(), Binary: filepath.Join(t.TempDir(), "no-such-binary")}
	waitTerminal := func(task RuntimeTask) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			current, _, err := service.Task(task.ID)
			if err == nil && terminalTaskLifecycle(current.Lifecycle) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task %s never reached a terminal state", task.ID)
	}
	busy := base
	busy.SessionID = "busy-session"
	if _, _, err := service.Create(busy, "idempotency-key-1"); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("a session in use elsewhere must be refused with the typed reason, got %v", err)
	}
	override := busy
	override.AllowSharedSession = true
	if task, _, err := service.Create(override, "idempotency-key-2"); errors.Is(err, ErrSessionInUse) {
		t.Fatal("the person's override must admit the send")
	} else if err == nil {
		waitTerminal(task)
	}
	free := base
	free.SessionID = "quiet-session"
	if task, _, err := service.Create(free, "idempotency-key-3"); errors.Is(err, ErrSessionInUse) {
		t.Fatal("a session nobody else is using must be admitted")
	} else if err == nil {
		waitTerminal(task)
	}
	if calls < 2 {
		t.Fatalf("the rule must be consulted for every non-overridden send, consulted %d times", calls)
	}
}

// Production wires the launch-time re-check: the rule says "free" when the
// task is created and "busy" when the launch actually happens, and the task
// ends failed with the ownership reason — no vendor process is started.
func TestCreateWiresTheLaunchTimeOwnershipCheck(t *testing.T) {
	service := installTestRuntimeTasks(t)
	prev := taskSessionInUse
	calls := 0
	taskSessionInUse = func(time.Time, string, string, string) (bool, error) {
		calls++
		return calls > 1, nil // first ask (creation) free; every later ask (launch) busy
	}
	defer func() { taskSessionInUse = prev }()
	// An executable that exists, so creation reaches the launch; the admit
	// hook refuses before anything is spawned, so /usr/bin/true never runs.
	task, created, err := service.Create(ChatRequest{Runtime: "claude", Prompt: "hello", Cwd: t.TempDir(),
		SessionID: "later-busy", Binary: "/usr/bin/true"}, "idempotency-key-launch")
	if err != nil || !created {
		t.Fatalf("creation must be admitted: created=%v err=%v", created, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, _, err := service.Task(task.ID)
		if err == nil && terminalTaskLifecycle(current.Lifecycle) {
			if current.Lifecycle != TaskFailed || !strings.Contains(current.ErrorText, "another process is using this session") {
				t.Fatalf("launch must fail with the ownership reason, got %s %q", current.Lifecycle, current.ErrorText)
			}
			if calls < 2 {
				t.Fatalf("the rule must be asked at launch as well as at creation, asked %d times", calls)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task never reached a terminal state")
}

// The registry's queued path: reservations beyond the running cap wait, and a
// promoted launch is re-admitted before it spawns. Slots are released on a
// refusal, so the registry is usable afterwards.
func TestQueuedLaunchIsReAdmittedOnPromotion(t *testing.T) {
	registry := NewTaskExecutionRegistry()
	release := make(chan struct{})
	holdDone := make(chan executionOutcome, taskMaxRunning)
	// Fill every running slot with launches parked in their admit hook.
	for i := 0; i < taskMaxRunning; i++ {
		reservation, err := registry.Reserve("claude")
		if err != nil {
			t.Fatal(err)
		}
		registry.Schedule(reservation, taskExecutionLaunch{taskID: "hold", runtime: "claude",
			done:  func(outcome executionOutcome) { holdDone <- outcome },
			admit: func() error { <-release; return ErrSessionInUse }})
	}
	queuedReservation, err := registry.Reserve("claude")
	if err != nil {
		t.Fatal(err)
	}
	queuedDone := make(chan executionOutcome, 1)
	registry.Schedule(queuedReservation, taskExecutionLaunch{taskID: "queued", runtime: "claude",
		done:  func(outcome executionOutcome) { queuedDone <- outcome },
		admit: func() error { return ErrSessionInUse }})
	select {
	case <-queuedDone:
		t.Fatal("a queued launch must not run before a slot frees")
	case <-time.After(100 * time.Millisecond):
	}
	close(release) // the held launches refuse and free their slots
	select {
	case outcome := <-queuedDone:
		if !errors.Is(outcome.Err, ErrSessionInUse) {
			t.Fatalf("the promoted launch must be re-admitted and refused, got %v", outcome.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the queued launch was never promoted")
	}
	for i := 0; i < taskMaxRunning; i++ {
		<-holdDone
	}
	if _, err := registry.Reserve("claude"); err != nil {
		t.Fatalf("slots must be released after refusals: %v", err)
	}
}
