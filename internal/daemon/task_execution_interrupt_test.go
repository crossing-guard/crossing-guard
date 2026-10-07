package daemon

import (
	"os/exec"
	"sync"
	"testing"
	"time"
)

// taskTestDeadline bounds a wait for a process state these tests cause.
const taskTestDeadline = 10 * time.Second

// scheduleInterruptible schedules one long-running process on the registry and
// returns the channel its outcome arrives on. admit runs on the registry's run
// goroutine immediately before the process is started.
func scheduleInterruptible(t *testing.T, registry *TaskExecutionRegistry, taskID string, admit func()) <-chan executionOutcome {
	t.Helper()
	reservation, err := registry.Reserve("managed-fixture")
	if err != nil {
		t.Fatal(err)
	}
	outcome := make(chan executionOutcome, 1)
	registry.Schedule(reservation, taskExecutionLaunch{taskID: taskID, runtime: "managed-fixture",
		cmd: exec.Command("/bin/sh", "-c", "sleep 30"), driver: managedFixtureDriver{},
		started: func() {}, event: func(ChatEvent) {},
		done:  func(result executionOutcome) { outcome <- result },
		admit: func() error { admit(); return nil }})
	return outcome
}

func wantInterruptedOutcome(t *testing.T, outcome <-chan executionOutcome) {
	t.Helper()
	select {
	case result := <-outcome:
		if !result.Interrupted {
			t.Fatalf("outcome %+v, want interrupted", result)
		}
	case <-time.After(taskTestDeadline):
		t.Fatal("the interrupted process was not stopped")
	}
}

// Interrupts that arrive while the registry is starting a task's process never
// touch the command the start is writing, and the process is still stopped.
// Under -race this fails when Interrupt reads the command during its start.
func TestTaskExecutionInterruptDuringProcessStart(t *testing.T) {
	registry := NewTaskExecutionRegistry()
	for range 20 {
		interruptWhileStarting(t, registry)
	}
}

func interruptWhileStarting(t *testing.T, registry *TaskExecutionRegistry) {
	t.Helper()
	stop := make(chan struct{})
	var interrupters sync.WaitGroup
	// The interrupters end with this launch even when it fails.
	defer interrupters.Wait()
	defer close(stop)
	outcome := scheduleInterruptible(t, registry, "task_start_race", func() {
		for range 4 {
			interrupters.Add(1)
			go func() {
				defer interrupters.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					// Most of these land on a process that is already killed and
					// not yet reaped: stopping it again is not a failure.
					if _, err := registry.Interrupt("task_start_race"); err != nil {
						t.Errorf("interrupt of a starting or dying task: %v", err)
						return
					}
				}
			}()
		}
	})
	wantInterruptedOutcome(t, outcome)
}

// An interrupt accepted before the process exists is not lost: the process is
// stopped as soon as it has started.
func TestTaskExecutionInterruptBeforeProcessStartIsHonored(t *testing.T) {
	registry := NewTaskExecutionRegistry()
	outcome := scheduleInterruptible(t, registry, "task_pending_interrupt", func() {
		accepted := make(chan bool)
		go func() {
			interrupted, err := registry.Interrupt("task_pending_interrupt")
			accepted <- interrupted && err == nil
		}()
		if !<-accepted {
			t.Error("an admitted task refused the interrupt")
		}
	})
	wantInterruptedOutcome(t, outcome)
}
