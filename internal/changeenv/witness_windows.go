//go:build windows

package changeenv

import "os/exec"

func configureProcessGroup(_ *exec.Cmd) {}

func processCleanupCapability() string { return "direct-process" }

func terminateProcess(cmd *exec.Cmd) string {
	_ = cmd.Process.Kill()
	return "direct-process"
}

func processExitStatus(err *exec.ExitError) (int, string, bool) {
	return err.ExitCode(), "", false
}
