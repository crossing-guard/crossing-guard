//go:build !windows

package analyzermodule

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(process *os.Process, grace time.Duration) {
	if process == nil {
		return
	}
	_ = syscall.Kill(-process.Pid, syscall.SIGTERM)
	time.Sleep(grace)
	_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
}
