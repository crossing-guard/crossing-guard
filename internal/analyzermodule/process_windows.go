//go:build windows

package analyzermodule

import (
	"os"
	"os/exec"
	"time"
)

func configureProcess(_ *exec.Cmd) {}

func terminateProcess(process *os.Process, _ time.Duration) {
	if process != nil {
		_ = process.Kill()
	}
}
