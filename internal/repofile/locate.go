// Package repofile locates a checkout's root by statting for a `.git` entry —
// never by spawning git (team plan §5.16.3, R14: the hook path has no git). The
// walk is modeled on internal/daemon/chat_models.go's checkWorkDir (the real
// stat-only precedent): innermost first, Lstat per level, so the `.git`-is-a-file
// worktree case falls out of Lstat's stat-any-entry.
package repofile

import (
	"os"
	"path/filepath"
)

// Locate returns the innermost ancestor directory of cwd that holds a `.git`
// entry (a directory, or the worktree's .git FILE) — the checkout root a
// repository layer's staging is keyed by. ok is false when cwd is not inside a
// checkout, or when cwd itself cannot be stated. It never spawns git and never
// follows the `.git` entry's contents.
func Locate(cwd string) (root string, ok bool) {
	dir := filepath.Clean(cwd)
	if _, err := os.Lstat(dir); err != nil {
		return "", false
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // reached the filesystem root without a .git
		}
		dir = parent
	}
}
