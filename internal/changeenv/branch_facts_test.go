package changeenv

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A sibling worktree on its own branch: one commit ahead of main with a
// rename, a staged edit, an unstaged edit and an untracked file. Every one of
// those paths is what a merge of that branch can collide on.
func TestReadBranchFactsListsEverythingAMergeWouldBring(t *testing.T) {
	root := t.TempDir()
	liveGit(t, root, "init", "-q", "-b", "main")
	writeLive(t, root, "a.txt", "a\n")
	writeLive(t, root, "old.txt", "rename me please, enough text to keep the similarity\n")
	writeLive(t, root, "c.txt", "c\n")
	liveGit(t, root, "add", ".")
	liveGit(t, root, "commit", "-q", "-m", "base")
	sibling := filepath.Join(t.TempDir(), "elsewhere")
	liveGit(t, root, "worktree", "add", "-q", "-b", "feature", sibling)
	liveGit(t, sibling, "mv", "old.txt", "new.txt")
	liveGit(t, sibling, "commit", "-q", "-m", "rename")
	writeLive(t, sibling, "a.txt", "a staged\n")
	liveGit(t, sibling, "add", "a.txt")
	writeLive(t, sibling, "c.txt", "c unstaged\n")
	writeLive(t, sibling, "untracked.txt", "u\n")

	facts, err := ReadBranchFacts(context.Background(), sibling, LiveDiffConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if facts.Branch != "feature" || facts.Base.State != "measured" || facts.Base.Name != "main" {
		t.Fatalf("branch/base = %q %+v", facts.Branch, facts.Base)
	}
	if facts.Base.Ahead != 1 || facts.Base.Behind != 0 {
		t.Fatalf("ahead/behind = %d/%d, want 1/0", facts.Base.Ahead, facts.Base.Behind)
	}
	want := []string{"a.txt", "c.txt", "new.txt", "old.txt", "untracked.txt"}
	if facts.Changes.State != "measured" || facts.Changes.Scope != "since_base" || !reflect.DeepEqual(facts.Changes.Files, want) {
		t.Fatalf("changed files = %v (%s), want %v", facts.Changes.Files, facts.Changes.State, want)
	}
	if facts.Upstream.State != "none" {
		t.Fatalf("a branch with no upstream is not pushed: %+v", facts.Upstream)
	}
}

// With no base to find, the branch is still described, the missing base is
// said, and the uncommitted work is listed under its own label.
func TestReadBranchFactsFallsBackToUncommittedWithoutABase(t *testing.T) {
	root := t.TempDir()
	liveGit(t, root, "init", "-q", "-b", "trunk")
	writeLive(t, root, "a.txt", "a\n")
	liveGit(t, root, "add", ".")
	liveGit(t, root, "commit", "-q", "-m", "base")
	writeLive(t, root, "a.txt", "edited\n")
	writeLive(t, root, "b.txt", "new\n")
	facts, err := ReadBranchFacts(context.Background(), root, LiveDiffConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if facts.Branch != "trunk" || facts.Base.State != "unavailable" || facts.Base.Reason == "" {
		t.Fatalf("missing base not stated: %+v", facts.Base)
	}
	if facts.Changes.State != "measured" || facts.Changes.Scope != "uncommitted" ||
		!reflect.DeepEqual(facts.Changes.Files, []string{"a.txt", "b.txt"}) {
		t.Fatalf("uncommitted fallback: %+v", facts.Changes)
	}
}

// A repository whose own config names an fsmonitor program must not get it run
// by a read under WithUntrustedFolders; the same read without it would.
func TestUntrustedFolderReadsNeverRunTheRepositorysFsmonitor(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	liveGit(t, root, "init", "-q", "-b", "main")
	writeLive(t, root, "a.txt", "a\n")
	liveGit(t, root, "add", ".")
	liveGit(t, root, "commit", "-q", "-m", "base")
	hook := filepath.Join(t.TempDir(), "fsmonitor.sh")
	writeLive(t, filepath.Dir(hook), filepath.Base(hook), "#!/bin/sh\ntouch "+marker+"\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	liveGit(t, root, "config", "core.fsmonitor", hook)
	writeLive(t, root, "a.txt", "edited\n")

	if _, err := ReadBranchFacts(WithUntrustedFolders(context.Background()), root, LiveDiffConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRepositoryContext(WithUntrustedFolders(context.Background()), root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repository's fsmonitor program ran during an untrusted-folder read")
	}
	// The control: the same read without the guard does start it, so the
	// assertion above can fail.
	if _, err := ReadBranchFacts(context.Background(), root, LiveDiffConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this git did not start core.fsmonitor for these reads; the guard is unobservable here")
	}
}
