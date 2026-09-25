//go:build darwin

package transcription

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	sandboxExecPath = "/usr/bin/sandbox-exec"
	// sandboxWaitDelay is how long Run waits for pipes after the process is
	// killed; the process group is already dead by then.
	sandboxWaitDelay = 2 * time.Second
)

// systemReadRoots are the library and runtime locations a dynamically linked
// executable needs. Measured on 2026-09-01 against whisper-cli 1.9.2: the
// loader also stats the filesystem root itself, hence the literal "/" grant
// in the profile. File contents under the user's home are not readable;
// metadata (existence, size) is, because the loader needs unqualified
// file-read-metadata to resolve paths.
var systemReadRoots = []string{
	"/usr", "/System", "/Library", "/private/var/db", "/private/etc", "/dev",
}

type darwinSandbox struct{}

func newPlatformSandbox() Sandbox { return darwinSandbox{} }

func (darwinSandbox) Available() (bool, string) {
	if _, err := os.Stat(sandboxExecPath); err != nil {
		return false, "sandbox-exec is not present on this system"
	}
	return true, ""
}

// Command wraps the executable in a deny-by-default profile: exact process
// execution, mapping of its libraries, read access to system roots plus the
// listed paths, write access only to the listed paths, and no network. GPU
// access (iokit) is deliberately absent; granting it made whisper-cli hang in
// the 2026-09-01 probe, and CPU-only is the configured contract anyway. The
// profile is generated from paths only; no user text reaches it.
func (s darwinSandbox) Command(ctx context.Context, spec SandboxSpec) (*exec.Cmd, error) {
	if ok, reason := s.Available(); !ok {
		return nil, errors.New(reason)
	}
	if !filepath.IsAbs(spec.Executable) {
		return nil, errors.New("sandboxed executable must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(spec.Executable)
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	profile := BuildSandboxProfile(spec, resolved)
	args := append([]string{"-p", profile, spec.Executable}, spec.Args...)
	cmd := exec.CommandContext(ctx, sandboxExecPath, args...)
	cmd.Dir = spec.Dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + spec.Dir, "TMPDIR=" + spec.Dir}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A grandchild holding the output pipes must not extend the deadline.
	cmd.WaitDelay = sandboxWaitDelay
	return cmd, nil
}

// BuildSandboxProfile renders the profile text. Exported for the unit test
// that pins its shape.
func BuildSandboxProfile(spec SandboxSpec, resolved string) string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(deny default)\n(deny network*)\n")
	profile.WriteString("(allow file-map-executable)\n(allow file-read-metadata)\n(allow process-fork)\n(allow sysctl-read)\n")
	profile.WriteString("(allow signal (target self))\n")
	fmt.Fprintf(&profile, "(allow process-exec %s %s)\n", sbLiteral(spec.Executable), sbLiteral(resolved))
	profile.WriteString("(allow file-read* (literal \"/\"))\n")
	readRoots := append([]string{}, systemReadRoots...)
	readRoots = append(readRoots, executableReadRoots(resolved)...)
	readRoots = append(readRoots, spec.ReadPaths...)
	for _, root := range readRoots {
		fmt.Fprintf(&profile, "(allow file-read* %s)\n", sbSubpath(realPath(root)))
	}
	for _, path := range spec.WritePaths {
		fmt.Fprintf(&profile, "(allow file-read* file-write* %s)\n", sbSubpath(realPath(path)))
	}
	return profile.String()
}

// executableReadRoots grants the prefix that owns the executable so its dylibs
// resolve: for /opt/homebrew/bin/whisper-cli that is /opt/homebrew.
func executableReadRoots(resolved string) []string {
	dir := filepath.Dir(resolved)
	roots := []string{dir}
	for _, prefix := range []string{"/opt/homebrew", "/usr/local"} {
		if strings.HasPrefix(resolved, prefix+"/") {
			roots = append(roots, prefix)
		}
	}
	return roots
}

// realPath resolves symlinked roots such as /var and /tmp, which the sandbox
// matches only by their real /private/... paths. A path that does not exist
// yet is resolved through its longest existing ancestor.
func realPath(path string) string {
	clean := filepath.Clean(path)
	remainder := ""
	for probe := clean; probe != "/" && probe != "."; probe = filepath.Dir(probe) {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			return filepath.Join(resolved, remainder)
		}
		remainder = filepath.Join(filepath.Base(probe), remainder)
	}
	return clean
}

func sbLiteral(path string) string { return `(literal "` + sbEscape(path) + `")` }
func sbSubpath(path string) string { return `(subpath "` + sbEscape(path) + `")` }

func sbEscape(path string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path)
}
