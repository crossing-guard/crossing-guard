package guardcli

// runtime_executable.go — resolve a runtime binary to an ABSOLUTE path before exec.
//
// Why this exists: the daemon normally runs as a launchd user agent, and launchd
// gives an agent a minimal default PATH (/usr/bin:/bin:/usr/sbin:/sbin). Every
// common install location for `claude` and `codex` — Homebrew, nvm, asdf,
// ~/.local/bin — is outside it, so an ambient-PATH lookup fails for the managed
// service while working perfectly when the daemon is started by hand from an
// interactive shell. That asymmetry is what makes the bug easy to misdiagnose.
//
// exec.Command resolves a bare name via LookPath against the CURRENT process's
// environment, at Command() time. Setting cmd.Env afterwards changes only the
// CHILD's environment and has no effect on that lookup — so a corrected PATH on
// cmd.Env alone does not fix this, and looks like it should.
//
// service.go also gives the agent a real PATH. Both halves are kept: the plist
// fixes the environment for everything the daemon spawns, while explicit
// resolution keeps chat working even when the daemon was started some other way
// (a hand-run binary, a different supervisor, a future container).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// extraBinDirs are the install prefixes launchd's default PATH omits. Ordered
// most-common-first; ~ is expanded at call time.
var extraBinDirs = []string{
	"/opt/homebrew/bin",
	"/usr/local/bin",
	"~/.local/bin",
	"~/.bun/bin",
	"~/.volta/bin",
	"~/go/bin",
}

// nvmGlobs are node version managers, where the binary lives under a
// version-specific directory that cannot be named literally.
var nvmGlobs = []string{
	"~/.nvm/versions/node/*/bin",
	"~/.asdf/installs/nodejs/*/bin",
	"~/Library/pnpm",
}

func home() string { h, _ := os.UserHomeDir(); return h }

// RuntimeAgentPATH is the PATH given to the launchd agent. launchd's default omits
// every common install prefix, so the managed service could not spawn `claude`
// or `codex` at all while a hand-started daemon could. Changing this string
// changes the plist, which the `string(cur) == want` idempotency check in
// installService correctly reads as drift and re-bootstraps — no migration.
func RuntimeAgentPATH() string {
	dirs := []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	for _, d := range extraBinDirs {
		dirs = append(dirs, expandHome(d))
	}
	for _, g := range nvmGlobs {
		if matches, err := filepath.Glob(expandHome(g)); err == nil {
			dirs = append(dirs, matches...)
		}
	}
	return strings.Join(dirs, ":")
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	return filepath.Join(home(), p[2:])
}

// searchedDirs reports every directory ResolveRuntimeBinary would look in, for error
// messages. A "not found" that does not say where it looked is the failure mode
// docs/STYLE.md calls out.
func searchedDirs() []string {
	var dirs []string
	if p := os.Getenv("PATH"); p != "" {
		dirs = append(dirs, strings.Split(p, string(os.PathListSeparator))...)
	}
	for _, d := range extraBinDirs {
		dirs = append(dirs, expandHome(d))
	}
	for _, g := range nvmGlobs {
		if matches, err := filepath.Glob(expandHome(g)); err == nil {
			dirs = append(dirs, matches...)
		}
	}
	return dirs
}

func executable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// ResolveRuntimeBinary turns a runtime name into an absolute path.
//
// Order: an explicitly configured binary wins (the user said so); then the
// ambient PATH; then the known install prefixes. `fallbacks` carries
// runtime-specific locations that are not directories on PATH at all — the
// codex binary bundled inside the ChatGPT app is the standing example.
//
// The error names the runtime and every directory searched, so the reader can
// act on it without reading this file.
func ResolveRuntimeBinary(name, configured string, fallbacks ...string) (string, error) {
	if configured != "" {
		// An absolute/relative path is taken at face value; a bare name still
		// needs resolving.
		if strings.ContainsRune(configured, os.PathSeparator) {
			if executable(configured) {
				return configured, nil
			}
			return "", fmt.Errorf("configured %s binary %q is not an executable file", name, configured)
		}
		name = configured
	}

	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range searchedDirs() {
		candidate := filepath.Join(dir, name)
		if executable(candidate) {
			return candidate, nil
		}
	}
	for _, fb := range fallbacks {
		if executable(fb) {
			return fb, nil
		}
	}

	return "", fmt.Errorf(
		"%s: executable %q not found; searched: %s; "+
			"if it is installed elsewhere, set the binary path in the console's Advanced drawer; "+
			"note: the daemon runs under launchd with a minimal PATH, so a shell that finds %q may not reflect what the service sees",
		name, name, strings.Join(searchedDirs(), ", "), name)
}
