//go:build !darwin

package transcription

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
)

type unavailableSandbox struct{}

func newPlatformSandbox() Sandbox { return unavailableSandbox{} }

func (unavailableSandbox) Available() (bool, string) {
	return false, "local dictation is not proved on " + runtime.GOOS + "; no process sandbox is available"
}

func (s unavailableSandbox) Command(context.Context, SandboxSpec) (*exec.Cmd, error) {
	_, reason := s.Available()
	return nil, errors.New(reason)
}
