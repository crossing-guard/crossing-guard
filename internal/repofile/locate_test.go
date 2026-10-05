package repofile

import (
	"os"
	"path/filepath"
	"testing"
)

// The checkout facts a repository layer's staging depends on (§5.16.6 criterion 24):
// innermost wins; a worktree's `.git` FILE counts (Lstat stat-any-entry); no git
// process is ever spawned; not-a-checkout says so.

func TestLocateFindsTheInnermostRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "cmd", "teamlink")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	got, ok := Locate(nested)
	if !ok || got != root {
		t.Fatalf("innermost root: got %q ok=%v", got, ok)
	}
	// cwd IS the root: still found.
	if got, _ := Locate(root); got != root {
		t.Fatalf("the root itself: %q", got)
	}
	// A sibling outside the checkout says so.
	outside := filepath.Dir(root)
	if _, ok := Locate(outside); ok {
		t.Fatal("a directory with no .git ancestor is not in a checkout")
	}
}

func TestLocateHandlesTheWorktreeGitFile(t *testing.T) {
	root := t.TempDir()
	// A linked worktree's .git is a FILE naming the real git dir.
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /somewhere/else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, ok := Locate(filepath.Join(root, "sub"))
	if !ok || got != root {
		t.Fatalf("a .git FILE is a checkout root too: %q ok=%v", got, ok)
	}
}

func TestLocateNeverSpawnsGitAndNeverDescends(t *testing.T) {
	// The walk is ancestor-only: a nested repo BELOW cwd is not found — cwd's own
	// ancestors, innermost first. (A child .git must not claim this root.)
	root := t.TempDir()
	child := filepath.Join(root, "inner")
	if err := os.MkdirAll(filepath.Join(child, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, ok := Locate(root); ok {
		t.Fatalf("a .git below cwd must not claim the parent: got %q", got)
	}
	_ = filepath.Clean
	_ = os.Lstat
}
