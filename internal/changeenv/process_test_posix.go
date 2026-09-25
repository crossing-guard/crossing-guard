//go:build !windows

package changeenv

import (
	"errors"
	"syscall"
)

func processAlive(pid int) error { return syscall.Kill(pid, 0) }
func processGone(err error) bool { return errors.Is(err, syscall.ESRCH) }
