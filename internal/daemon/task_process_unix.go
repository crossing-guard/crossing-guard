//go:build !windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareTaskProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func interruptTaskProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.EPERM) {
		// Nothing in the group took the signal. On macOS that is a group whose
		// members have all ended and are not reaped yet, and on every platform a
		// group the daemon may not signal. The launched process tells them apart:
		// it accepts the signal or is done in the first case and refuses it in
		// the second. Nil therefore means the launched process is stopped, not
		// that no other member is left.
		err = cmd.Process.Kill()
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
