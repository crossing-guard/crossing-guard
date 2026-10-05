package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// runStdinTask runs one command through runTaskProcess and returns the text
// events it projected, bounded by a deadline so a hang fails instead of
// stalling the suite.
func runStdinTask(t *testing.T, cmd *exec.Cmd, interrupted bool) ([]string, *exec.Cmd) {
	t.Helper()
	var mu sync.Mutex
	texts := []string{}
	launch := taskExecutionLaunch{taskID: "task_stdin", runtime: "managed-fixture", cmd: cmd, driver: managedFixtureDriver{},
		started: func() {},
		event: func(event ChatEvent) {
			mu.Lock()
			defer mu.Unlock()
			if event["type"] == "text" {
				texts = append(texts, anyString(event["text"]))
			}
		}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTaskProcess(launch, func() bool { return interrupted })
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runTaskProcess did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	return texts, cmd
}

// A driver's stdin reaches the process whole, even past the pipe buffer.
func TestTaskProcessDeliversDriverStdin(t *testing.T) {
	prompt := strings.Repeat("a", 1536*1024)
	cmd := exec.Command("/bin/sh", "-c", `printf '{"text":"%s"}\n' "$(wc -c | tr -d ' ')"`)
	cmd.Stdin = strings.NewReader(prompt)
	texts, _ := runStdinTask(t, cmd, false)
	if len(texts) != 1 || texts[0] != strconv.Itoa(len(prompt)) {
		t.Fatalf("the process read %v bytes, want %d", texts, len(prompt))
	}
}

// An interrupt before the child reads its stdin returns promptly: the killed
// process closes the pipe and the copy ends instead of blocking Wait.
func TestTaskProcessInterruptBeforeStdinIsReadDoesNotHang(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.Stdin = strings.NewReader(strings.Repeat("b", 4*1024*1024))
	started := time.Now()
	runStdinTask(t, cmd, true)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("interrupt took %s", elapsed)
	}
}

// The daemon's own stdin is never handed down to a task.
func TestTaskProcessNeverHandsDownTheDaemonStdin(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `printf '{"text":"%s"}\n' "$(wc -c | tr -d ' ')"`)
	cmd.Stdin = os.Stdin
	texts, ran := runStdinTask(t, cmd, false)
	if ran.Stdin != nil || len(texts) != 1 || texts[0] != "0" {
		t.Fatalf("stdin=%v texts=%v", ran.Stdin, texts)
	}
}
