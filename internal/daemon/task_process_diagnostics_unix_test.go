//go:build !windows

package daemon

import (
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

type diagnosticFailureProtocolFixture struct {
	entered, returning chan struct{}
	input              *io.PipeWriter
}

func (p diagnosticFailureProtocolFixture) Run(stdout io.ReadCloser, _ func(ChatEvent)) error {
	<-p.entered
	if _, err := p.input.Write([]byte("release\n")); err != nil {
		return err
	}
	if err := p.input.Close(); err != nil {
		return err
	}
	_, err := io.ReadAll(stdout)
	close(p.returning)
	if err != nil {
		return err
	}
	return errors.New("diagnostic protocol failure")
}

func TestTaskProcessProtocolFailurePreservesLateStderr(t *testing.T) {
	entered, returning, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	input, writer := io.Pipe()
	cmd := exec.Command("/bin/sh", "-c", "printf 'initial diagnostic\\n' >&2; IFS= read -r release; printf '429 Too Many Requests\\n' >&2; printf done")
	cmd.Stdin = input
	started := make(chan int, 1)
	done := make(chan struct{})
	var events []ChatEvent
	var outcome executionOutcome
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	first := true
	go func() {
		defer close(done)
		outcome = runTaskProcess(taskExecutionLaunch{cmd: cmd,
			protocol: diagnosticFailureProtocolFixture{entered: entered, returning: returning, input: writer},
			started:  func() { started <- cmd.Process.Pid }, event: func(event ChatEvent) {
				if first {
					first = false
					close(entered)
					<-release
				}
				events = append(events, event)
			}}, func() bool { return false })
	}()
	pid := 0
	t.Cleanup(func() {
		unblock()
		_ = writer.Close()
		_ = input.Close()
		select {
		case <-done:
			return
		default:
		}
		if pid != 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("process owner did not settle during cleanup")
		}
	})
	select {
	case pid = <-started:
	case <-done:
		t.Fatalf("process failed to start: %+v", outcome)
	case <-time.After(3 * time.Second):
		t.Fatal("process did not start")
	}
	select {
	case <-returning:
	case <-time.After(3 * time.Second):
		t.Fatal("protocol did not finish")
	}
	// The broken owner reaps while the first callback is paused, closing the
	// pipe with the quota line still unread. The correct owner cannot reap
	// until release. Detect premature reap, bounded by one second.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("process owner did not finish")
	}
	if outcome.Err == nil || outcome.Err.Error() != "diagnostic protocol failure" || cmd.ProcessState == nil {
		t.Fatalf("protocol error or reaping lost: %+v", outcome)
	}
	for _, event := range events {
		if event["error_class"] == providerErrorQuota {
			return
		}
	}
	t.Fatalf("late provider classification lost: %v", events)
}
