//go:build !windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startTaskGroup starts script as the leader of its own process group, the way
// runTaskProcess does, and reaps it when the test ends.
func startTaskGroup(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	prepareTaskProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // the test may have reaped it already
	})
	return cmd
}

// processState returns the ps state letters of pid, or "" once it is gone.
func processState(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Skipf("ps is not usable here: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func awaitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(taskTestDeadline)
	for {
		state := processState(t, pid)
		if strings.HasPrefix(state, "Z") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d is in state %q, want ended and unreaped", pid, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A task process that has ended and has not been reaped yet is already stopped:
// stopping it is not a failure, whether a stop or the process itself ended it.
// macOS answers a signal to such a group with EPERM, which is also its answer
// for a group the daemon may not signal.
func TestInterruptTaskProcessTreatsUnreapedLeaderAsStopped(t *testing.T) {
	for name, script := range map[string]string{
		"killed":         "exec sleep 30",
		"exited by self": "exit 0",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := startTaskGroup(t, script)
			if name == "killed" {
				if err := interruptTaskProcess(cmd); err != nil {
					t.Fatalf("first stop: %v", err)
				}
			}
			awaitZombie(t, cmd.Process.Pid)
			if err := interruptTaskProcess(cmd); err != nil {
				t.Fatalf("stop of an ended, unreaped process: %v", err)
			}
			_ = cmd.Wait() // killed: the exit status is not the subject
			if err := interruptTaskProcess(cmd); err != nil {
				t.Fatalf("stop of a reaped process: %v", err)
			}
		})
	}
}

// The raw answers the fallback depends on: the leader of a group that holds
// only itself, ended and unreaped, never refuses the signal, and on macOS the
// group does. A platform change shows here and not as a wrong task outcome.
func TestUnreapedGroupSignalAnswer(t *testing.T) {
	cmd := startTaskGroup(t, "exit 0")
	pid := cmd.Process.Pid
	awaitZombie(t, pid)
	group := syscall.Kill(-pid, syscall.SIGKILL)
	leader := cmd.Process.Signal(syscall.SIGKILL)
	if runtime.GOOS == "darwin" && group != syscall.EPERM {
		t.Fatalf("group signal to an unreaped leader: %v, want EPERM", group)
	}
	if group != nil && group != syscall.EPERM {
		t.Fatalf("group signal to an unreaped leader: %v, want nil or EPERM", group)
	}
	if leader != nil && leader != os.ErrProcessDone {
		t.Fatalf("leader signal to an unreaped leader: %v, want nil or done", leader)
	}
}
