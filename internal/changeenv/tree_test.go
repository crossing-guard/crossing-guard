package changeenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"crossing-guard/internal/observation"
)

func treeTestConfig() TreeConfig { return TreeConfig{MaxEntriesPerDir: 100, MaxReadBytes: 64} }

// treeCorpus: tracked files in two folders, an ignored file, an untracked
// directory, a symlink, a nested repository, a deleted tracked file, a binary,
// and a file over the read cap.
func treeCorpus(t *testing.T) string {
	root := testRepo(t)
	writeLive(t, root, "a/b/f.txt", "one\ntwo\n")
	writeLive(t, root, "c/g.txt", "gee\n")
	writeLive(t, root, "gone.txt", "bye\n")
	writeLive(t, root, ".gitignore", "ignored.txt\nignored-dir/\n")
	liveGit(t, root, "add", "-A")
	liveGit(t, root, "commit", "-q", "-m", "tree")
	writeLive(t, root, "a/b/ignored.txt", "ig\n")
	writeLive(t, root, "ignored-dir/x.txt", "ig\n")
	writeLive(t, root, "a/untracked_dir/u.txt", "zed\n")
	writeLive(t, root, "big.txt", strings.Repeat("line\n", 40))
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0, 1, 2, 255}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	liveGit(t, nested, "init", "-q")
	return root
}

func entryByName(entries []TreeEntry, name string) (TreeEntry, bool) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return TreeEntry{}, false
}

func TestListTreeFoldsGitsPopulationIntoChildren(t *testing.T) {
	root := treeCorpus(t)
	ctx := context.Background()
	rootListing, err := ListTree(ctx, root, "", treeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range rootListing.Entries {
		names = append(names, entry.Name+":"+entry.Kind)
	}
	want := []string{"a:dir", "c:dir", "nested:dir", ".gitignore:file", "base.txt:file", "big.txt:file", "blob.bin:file", "gone.txt:missing"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("root entries=%v want %v", names, want)
	}
	if _, found := entryByName(rootListing.Entries, "ignored-dir"); found {
		t.Fatal("an ignored directory was listed")
	}
	if nested, _ := entryByName(rootListing.Entries, "nested"); !nested.Untracked {
		t.Fatalf("nested repository should be an untracked dir at the root: %+v", nested)
	}
	if big, _ := entryByName(rootListing.Entries, "big.txt"); big.Size != 200 || big.ModTime == 0 {
		t.Fatalf("size/mtime not stated: %+v", big)
	}
	a, err := ListTree(ctx, root, "a", treeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if link, _ := entryByName(a.Entries, "link"); link.Kind != "symlink" || !link.Untracked {
		t.Fatalf("symlink entry=%+v", link)
	}
	if untracked, _ := entryByName(a.Entries, "untracked_dir"); untracked.Kind != "dir" || !untracked.Untracked {
		t.Fatalf("untracked dir entry=%+v", untracked)
	}
	if b, _ := entryByName(a.Entries, "b"); b.Kind != "dir" || b.Untracked {
		t.Fatalf("tracked dir entry=%+v", b)
	}
	ab, err := ListTree(ctx, root, "a/b", treeTestConfig())
	if err != nil || len(ab.Entries) != 1 || ab.Entries[0].Name != "f.txt" {
		t.Fatalf("a/b listing=%+v err=%v (ignored.txt must be absent)", ab, err)
	}
}

func TestListTreeStatesAndCaps(t *testing.T) {
	root := treeCorpus(t)
	ctx := context.Background()
	for dir, state := range map[string]string{"ignored-dir": "ignored", "does-not-exist": "not-in-checkout", "nested": "nested-repository"} {
		listing, err := ListTree(ctx, root, dir, treeTestConfig())
		if err != nil || listing.State != state || len(listing.Entries) != 0 {
			t.Fatalf("%s: listing=%+v err=%v want state %s", dir, listing, err, state)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "hollow"), 0o700); err != nil {
		t.Fatal(err)
	}
	if listing, err := ListTree(ctx, root, "hollow", treeTestConfig()); err != nil || listing.State != "empty" {
		t.Fatalf("empty dir listing=%+v err=%v", listing, err)
	}
	for _, bad := range []string{"../x", ".git", ".git/config", "a/../c"} {
		_, err := ListTree(ctx, root, bad, treeTestConfig())
		var typed *LiveDiffError
		if err == nil || !asLiveError(err, &typed) || typed.Code != "invalid-path" {
			t.Fatalf("dir %q: err=%v", bad, err)
		}
	}
	// Pathspec magic is literal: a glob or a colon prefix names a path that does not exist.
	for _, magic := range []string{"*", ":/a", ":(top)a"} {
		listing, err := ListTree(ctx, root, magic, treeTestConfig())
		if err != nil || listing.State != "not-in-checkout" || len(listing.Entries) != 0 {
			t.Fatalf("magic %q: listing=%+v err=%v", magic, listing, err)
		}
	}
	// A wholly untracked directory lists its own contents when asked, and a
	// subdirectory folded from that listing is untracked by the same rule.
	writeLive(t, root, "a/untracked_dir/deeper/d.txt", "d\n")
	untracked, err := ListTree(ctx, root, "a/untracked_dir", treeTestConfig())
	if err != nil || len(untracked.Entries) != 2 || untracked.Entries[0].Name != "deeper" || !untracked.Entries[0].Untracked || !untracked.Entries[1].Untracked {
		t.Fatalf("untracked dir contents=%+v err=%v", untracked, err)
	}
	// A directory with any tracked path beneath it is not untracked.
	if a, _ := ListTree(ctx, root, "", treeTestConfig()); a.Entries[0].Name != "a" || a.Entries[0].Untracked {
		t.Fatalf("root 'a' entry=%+v", a.Entries[0])
	}
	// Listing a file, not a directory, is named as such.
	var typed *LiveDiffError
	_, err = ListTree(ctx, root, "a/b/f.txt", treeTestConfig())
	if err == nil || !asLiveError(err, &typed) || typed.Code != "not-a-directory" {
		t.Fatalf("file as dir err=%v", err)
	}
	capped, err := ListTree(ctx, root, "", TreeConfig{MaxEntriesPerDir: 2, MaxReadBytes: 64})
	if err != nil || !capped.Truncated || len(capped.Entries) != 2 || capped.Dropped < 6 {
		t.Fatalf("capped listing=%+v err=%v", capped, err)
	}
}

func TestReadTreeFileServesOnlyListedPathsBounded(t *testing.T) {
	root := treeCorpus(t)
	ctx := context.Background()
	cfg := treeTestConfig()
	file, err := ReadTreeFile(ctx, root, "a/b/f.txt", cfg)
	if err != nil || file.Kind != "text" || file.Text != "one\ntwo\n" || file.Truncated || file.Size != 8 || file.ModTime == 0 {
		t.Fatalf("text file=%+v err=%v", file, err)
	}
	big, err := ReadTreeFile(ctx, root, "big.txt", cfg)
	if err != nil || !big.Truncated || big.Bytes > 64 || !strings.HasSuffix(big.Text, "\n") || big.Size != 200 {
		t.Fatalf("bounded file=%+v err=%v", big, err)
	}
	binary, err := ReadTreeFile(ctx, root, "blob.bin", cfg)
	if err != nil || binary.Kind != "binary" || binary.Text != "" || binary.Size != 4 {
		t.Fatalf("binary file=%+v err=%v", binary, err)
	}
	link, err := ReadTreeFile(ctx, root, "a/link", cfg)
	if err != nil || link.Kind != "symlink" || link.Target != "/etc/hosts" || link.Text != "" {
		t.Fatalf("symlink file=%+v err=%v", link, err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "c", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	// git never lists a FIFO; a listed path replaced by one is reported special, not opened.
	read, err := statAndReadBounded(root, "c/pipe", 64)
	if err != nil || !read.Mode.IsRegular() == false || read.Body != nil {
		t.Fatalf("fifo read=%+v err=%v", read, err)
	}
	var typed *LiveDiffError
	for path, code := range map[string]string{"a/b/ignored.txt": "file-not-in-tree", ".git/config": "invalid-path", ".GIT/config": "invalid-path", "a/.git/x": "invalid-path", "../etc/passwd": "invalid-path", "gone.txt": "not-a-file", "a": "file-not-in-tree"} {
		_, err := ReadTreeFile(ctx, root, path, cfg)
		if err == nil || !asLiveError(err, &typed) || typed.Code != code {
			t.Fatalf("%s: err=%v want %s", path, err, code)
		}
	}
}

// The checkpoint reader beside the new seam keeps its own semantics: a small
// file yields its bytes and the literal digest, a file over the checkpoint cap
// yields no body and the bounded flag, a symlink is refused.
func TestCheckpointReaderUnchangedBesideTheBoundedSeam(t *testing.T) {
	root := treeCorpus(t)
	body, total, digest, bounded, err := readCheckpointRegular(root, "a/b/f.txt")
	if err != nil || string(body) != "one\ntwo\n" || total != 8 || bounded ||
		digest != "sha256-v1:"+observation.DigestBytes([]byte("one\ntwo\n"))[len("sha256-v1:"):] {
		t.Fatalf("checkpoint read=%q total=%d digest=%q bounded=%v err=%v", body, total, digest, bounded, err)
	}
	writeLive(t, root, "huge.txt", strings.Repeat("x", maxCheckpointPathBytes+1))
	body, total, digest, bounded, err = readCheckpointRegular(root, "huge.txt")
	if err != nil || body != nil || !bounded || digest == "" || total != maxCheckpointPathBytes+1 {
		t.Fatalf("over-cap checkpoint read=%d total=%d digest=%q bounded=%v err=%v", len(body), total, digest, bounded, err)
	}
	// The bounded reader reports truncation from what it read, not from a stat.
	read, err := statAndReadBounded(root, "huge.txt", 10)
	if err != nil || len(read.Body) != 10 || !read.Truncated {
		t.Fatalf("bounded read=%d truncated=%v err=%v", len(read.Body), read.Truncated, err)
	}
	if _, err := statAndReadBounded(root, "a/b/f.txt", 0); err == nil {
		t.Fatal("a zero byte budget was accepted")
	}
	if _, _, _, _, err := readCheckpointRegular(root, "a/link"); err == nil {
		t.Fatal("checkpoint reader followed a symlink")
	}
	read, err = statAndReadBounded(root, "a/link", 64)
	if err != nil || read.LinkTarget != "/etc/hosts" {
		t.Fatalf("bounded reader symlink=%+v err=%v", read, err)
	}
	// A symlinked directory component never redirects a read through either reader.
	if err := os.Symlink("/etc", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := statAndReadBounded(root, "escape/hosts", 64); err == nil {
		t.Fatal("bounded reader followed a symlinked directory")
	}
	if _, err := worktreeFacts(root, "escape/hosts", 64); err == nil {
		t.Fatal("the diff pane's worktree read followed a symlinked directory")
	}
}
