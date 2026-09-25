package transcription

import (
	"context"
	"os/exec"
)

// SandboxSpec describes one confined process launch: the exact executable,
// its arguments, the paths it may read beyond the platform's own libraries,
// and the paths it may write. Network access is always denied.
type SandboxSpec struct {
	Executable string
	Args       []string
	ReadPaths  []string
	WritePaths []string
	Dir        string
}

// Sandbox is the port through which the local backend launches its process.
// The daemon has no other process-confinement mechanism, so this is a new
// owner, named as such in the design's red-team record.
type Sandbox interface {
	// Available reports whether confinement exists on this platform and, when
	// it does not, a reason safe to show in Settings.
	Available() (bool, string)
	Command(ctx context.Context, spec SandboxSpec) (*exec.Cmd, error)
}

// NewStrictSandbox returns the platform's deny-by-default sandbox.
func NewStrictSandbox() Sandbox { return newPlatformSandbox() }
