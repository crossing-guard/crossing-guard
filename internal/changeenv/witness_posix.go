//go:build !windows

package changeenv

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func processCleanupCapability() string { return "process-group" }

func terminateProcess(cmd *exec.Cmd) string {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return "process-group"
	}
	_ = cmd.Process.Kill()
	return "direct-process"
}

func processExitStatus(err *exec.ExitError) (int, string, bool) {
	if status, ok := err.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		sig := status.Signal()
		return 128 + int(sig), sig.String(), true
	}
	return err.ExitCode(), "", false
}
