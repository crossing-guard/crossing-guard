//go:build windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
)

func prepareTaskProcess(*exec.Cmd) {}

func interruptTaskProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
