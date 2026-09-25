package daemon

import (
	"bufio"
	"encoding/json"
	"strings"
	"sync"
)

const (
	taskStdoutRecordMax = 16 * 1024 * 1024
	taskStderrRecordMax = 1024 * 1024
)

// runTaskProcess owns byte framing and process wait for one already-admitted launch.
// Admission, task lifecycle, persistence, and subscriber delivery remain outside it.
func runTaskProcess(launch taskExecutionLaunch, interrupted func() bool) executionOutcome {
	stdout, err := launch.cmd.StdoutPipe()
	if err != nil {
		return executionOutcome{Err: err}
	}
	stderr, err := launch.cmd.StderrPipe()
	if err != nil {
		return executionOutcome{Err: err}
	}
	launch.cmd.Stdin = nil
	prepareTaskProcess(launch.cmd)
	if err := launch.cmd.Start(); err != nil {
		return executionOutcome{Err: err}
	}
	launch.started()
	if interrupted() {
		_ = interruptTaskProcess(launch.cmd)
	}

	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), taskStderrRecordMax)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.Contains(line, "Reading additional input from stdin") {
				continue
			}
			if isVendorAuthFailure(line) {
				launch.event(ChatEvent{"type": "auth_required", "runtime": launch.runtime,
					"text": "Sign in to " + launch.runtime + " and retry this turn."})
				continue
			}
			// The provider CLI's own stderr is a transport channel (R3):
			// classified lines keep their stderr kind — additive field, no
			// rendering change — so the orchestration layer can read the
			// provider class without a second detection pass.
			launch.event(classifiedProviderError(ChatEvent{"type": "stderr", "text": truncate(line, 500)}, line))
		}
		if err := scanner.Err(); err != nil {
			launch.event(ChatEvent{"type": "stderr", "text": "Runtime diagnostics stream ended: " + truncate(err.Error(), 300)})
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), taskStdoutRecordMax)
	for scanner.Scan() {
		var object map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &object); err != nil {
			launch.event(ChatEvent{"type": "stderr", "text": "Unrecognized runtime event was skipped."})
			continue
		}
		for _, event := range launch.driver.ProjectEvent(object) {
			launch.event(event)
		}
	}
	if err := scanner.Err(); err != nil {
		launch.event(ChatEvent{"type": "stderr", "text": "Runtime output stream ended: " + truncate(err.Error(), 300)})
	}
	// Cmd.Wait closes StderrPipe. Drain diagnostics first so provider failures
	// are classified before process completion can discard the unread tail.
	readers.Wait()
	waitErr := launch.cmd.Wait()
	return executionOutcome{Err: waitErr, Interrupted: interrupted()}
}
