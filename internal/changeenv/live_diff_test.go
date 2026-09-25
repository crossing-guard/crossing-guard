package changeenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func liveTestConfig() LiveDiffConfig {
	return LiveDiffConfig{MaxFiles: 100, MaxFileBytes: 1 << 20, ContextLines: 3, MaxRefs: 20}
}

func liveGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeLive(t *testing.T, root, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileByPath(files []LiveDiffFile, path string) (LiveDiffFile, bool) {
	for _, file := range files {
		if file.Path == path {
			return file, true
		}
	}
	return LiveDiffFile{}, false
}

// liveCorpus builds one repository that exercises every typed row: a modified
// tracked file, a staged file, an untracked text file, an untracked binary, a
// mode-only change, a rename with no text change, and a branch ahead of main.
func liveCorpus(t *testing.T) string {
	root := testRepo(t)
	liveGit(t, root, "branch", "-M", "main")
	writeLive(t, root, "src/keep.go", "package keep\n")
	writeLive(t, root, "src/mode.sh", "echo hi\n")
	writeLive(t, root, "src/old-name.txt", "same bytes\n")
	liveGit(t, root, "add", ".")
	liveGit(t, root, "commit", "-q", "-m", "second")
	liveGit(t, root, "checkout", "-q", "-b", "feature")
	writeLive(t, root, "base.txt", "base\ncommitted on feature\n")
	liveGit(t, root, "commit", "-q", "-am", "feature work")
	// Working tree state after the commit:
	writeLive(t, root, "src/keep.go", "package keep\n\nfunc Keep() {}\n") // modified, unstaged
	writeLive(t, root, "src/staged.go", "package staged\n")               // staged addition
	liveGit(t, root, "add", "src/staged.go")
	writeLive(t, root, "notes.md", "one\ntwo\n") // untracked text
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0, 1, 2, 255}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "src/mode.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	liveGit(t, root, "mv", "src/old-name.txt", "src/new-name.txt") // staged rename-only
	return root
}

func TestLiveDiffScopesListTypedRowsWithFreshness(t *testing.T) {
	root := liveCorpus(t)
	ctx := context.Background()
	working, err := LiveDiff(ctx, root, "working", "", liveTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if working.Branch != "feature" || working.Head == "" || working.ObservedAt == 0 {
		t.Fatalf("checkout facts=%+v", working)
	}
	want := map[string]string{"src/keep.go": "text", "src/staged.go": "text", "notes.md": "text", "blob.bin": "binary", "src/mode.sh": "mode", "src/new-name.txt": "rename"}
	for path, kind := range want {
		file, found := fileByPath(working.Files, path)
		if !found || file.Kind != kind || file.Freshness == "" {
			t.Fatalf("%s: found=%v file=%+v", path, found, file)
		}
	}
	if file, _ := fileByPath(working.Files, "notes.md"); !file.Untracked || file.Added != 2 || file.Status != "?" {
		t.Fatalf("untracked row=%+v", file)
	}
	if file, _ := fileByPath(working.Files, "src/new-name.txt"); file.OldPath != "src/old-name.txt" || file.Status != "R" {
		t.Fatalf("rename row=%+v", file)
	}
	if file, _ := fileByPath(working.Files, "src/keep.go"); file.Added != 2 || file.Removed != 0 {
		t.Fatalf("modified counts=%+v", file)
	}
	staged, err := LiveDiff(ctx, root, "staged", "", liveTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, found := fileByPath(staged.Files, "src/keep.go"); found {
		t.Fatal("staged scope listed an unstaged change")
	}
	if _, found := fileByPath(staged.Files, "src/staged.go"); !found {
		t.Fatal("staged scope missed the staged addition")
	}
	if _, found := fileByPath(staged.Files, "notes.md"); found {
		t.Fatal("staged scope listed an untracked file")
	}
	unstaged, err := LiveDiff(ctx, root, "unstaged", "", liveTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, found := fileByPath(unstaged.Files, "notes.md"); found {
		t.Fatal("unstaged scope listed an untracked file")
	}
	if _, found := fileByPath(unstaged.Files, "src/keep.go"); !found {
		t.Fatal("unstaged scope missed the worktree change")
	}
}

func TestLiveDiffBranchScopesUseTheMergeBaseAndRefuseOptionShapedBases(t *testing.T) {
	root := liveCorpus(t)
	ctx := context.Background()
	branch, err := LiveDiff(ctx, root, "branch", "", liveTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if branch.Base != "main" || branch.MergeBase == "" || len(branch.Files) != 1 || branch.Files[0].Path != "base.txt" {
		t.Fatalf("branch scope=%+v", branch)
	}
	since, err := LiveDiff(ctx, root, "since-base", "main", liveTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"base.txt", "src/keep.go", "notes.md", "src/staged.go"} {
		if _, found := fileByPath(since.Files, path); !found {
			t.Fatalf("since-base missed %s in %+v", path, since.Files)
		}
	}
	sink := filepath.Join(t.TempDir(), "sink")
	for _, bad := range []string{"--output=" + sink, "-v", "nope-ref"} {
		_, err := LiveDiff(ctx, root, "branch", bad, liveTestConfig())
		var typed *LiveDiffError
		if err == nil || !asLiveError(err, &typed) || typed.Code != "base-ref-missing" {
			t.Fatalf("base %q: err=%v", bad, err)
		}
	}
	if _, err := os.Stat(sink); err == nil {
		t.Fatal("an option-shaped base reached git")
	}
	if _, err := LiveDiff(ctx, root, "sideways", "", liveTestConfig()); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

func asLiveError(err error, target **LiveDiffError) bool {
	typed, ok := err.(*LiveDiffError)
	if ok {
		*target = typed
	}
	return ok
}

func TestLiveDiffPatchIsBoundedAndFreshnessMovesWithContent(t *testing.T) {
	root := liveCorpus(t)
	ctx := context.Background()
	cfg := liveTestConfig()
	patch, err := LiveDiffPatchFor(ctx, root, "working", "", "src/keep.go", cfg)
	if err != nil || patch.Kind != "text" || !strings.Contains(patch.Patch, "+func Keep() {}") || patch.Truncated {
		t.Fatalf("patch=%+v err=%v", patch, err)
	}
	before := patch.Freshness
	writeLive(t, root, "src/keep.go", "package keep\n\nfunc Keep() {}\n\nfunc More() {}\n")
	after, err := LiveDiffPatchFor(ctx, root, "working", "", "src/keep.go", cfg)
	if err != nil || after.Freshness == before {
		t.Fatalf("freshness did not move: before=%s after=%+v err=%v", before, after, err)
	}
	untracked, err := LiveDiffPatchFor(ctx, root, "working", "", "notes.md", cfg)
	if err != nil || !strings.Contains(untracked.Patch, "+one") || !strings.Contains(untracked.Patch, "--- /dev/null") {
		t.Fatalf("untracked patch=%+v err=%v", untracked, err)
	}
	binary, err := LiveDiffPatchFor(ctx, root, "working", "", "blob.bin", cfg)
	if err != nil || binary.Kind != "binary" || binary.Patch != "" {
		t.Fatalf("binary row=%+v err=%v", binary, err)
	}
	var typed *LiveDiffError
	if _, err := LiveDiffPatchFor(ctx, root, "working", "", "../escape", cfg); !asLiveError(err, &typed) || typed.Code != "invalid-path" {
		t.Fatalf("unsafe path err=%v", err)
	}
	if _, err := LiveDiffPatchFor(ctx, root, "staged", "", "src/keep.go", cfg); !asLiveError(err, &typed) || typed.Code != "file-not-in-scope" {
		t.Fatalf("out-of-scope err=%v", err)
	}
	// Revisions must precede the pathspec: a staged-only file in the working
	// scope and a committed file in the branch scope both have patches.
	staged, err := LiveDiffPatchFor(ctx, root, "working", "", "src/staged.go", cfg)
	if err != nil || !strings.Contains(staged.Patch, "+package staged") {
		t.Fatalf("staged file in working scope patch=%+v err=%v", staged, err)
	}
	branch, err := LiveDiffPatchFor(ctx, root, "branch", "", "base.txt", cfg)
	if err != nil || !strings.Contains(branch.Patch, "+committed on feature") {
		t.Fatalf("branch scope patch=%+v err=%v", branch, err)
	}
	since, err := LiveDiffPatchFor(ctx, root, "since-base", "main", "src/keep.go", cfg)
	if err != nil || !strings.Contains(since.Patch, "+func Keep() {}") {
		t.Fatalf("since-base patch=%+v err=%v", since, err)
	}
	small := cfg
	small.MaxFileBytes = 64
	bounded, err := LiveDiffPatchFor(ctx, root, "working", "", "src/keep.go", small)
	if err != nil || !bounded.Truncated || len(bounded.Patch) > 64 || !strings.HasSuffix(bounded.Patch, "\n") {
		t.Fatalf("bounded patch=%+v err=%v", bounded, err)
	}
	many := cfg
	many.MaxFiles = 2
	list, err := LiveDiff(ctx, root, "working", "", many)
	if err != nil || list.Truncated == nil || list.Truncated.Cap != "max_files" || len(list.Files) != 2 || list.Truncated.Files < 4 {
		t.Fatalf("truncated list=%+v err=%v", list, err)
	}
}

func TestLiveDiffConflictIsATypedRowNeverAPatch(t *testing.T) {
	root := testRepo(t)
	liveGit(t, root, "branch", "-M", "main")
	liveGit(t, root, "checkout", "-q", "-b", "other")
	writeLive(t, root, "base.txt", "other side\n")
	liveGit(t, root, "commit", "-q", "-am", "other")
	liveGit(t, root, "checkout", "-q", "main")
	writeLive(t, root, "base.txt", "main side\n")
	liveGit(t, root, "commit", "-q", "-am", "main")
	cmd := exec.Command("git", "-C", root, "merge", "other")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("merge did not conflict: %s", out)
	}
	ctx := context.Background()
	list, err := LiveDiff(ctx, root, "working", "", liveTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	file, found := fileByPath(list.Files, "base.txt")
	if !found || file.Kind != "conflict" {
		t.Fatalf("conflict row=%+v found=%v", file, found)
	}
	patch, err := LiveDiffPatchFor(ctx, root, "working", "", "base.txt", liveTestConfig())
	if err != nil || patch.Kind != "conflict" || strings.Contains(patch.Patch, "<<<<<<<") || patch.Patch != "" {
		t.Fatalf("conflict patch=%+v err=%v", patch, err)
	}
}

func TestLiveDiffRefsAndDeadline(t *testing.T) {
	root := liveCorpus(t)
	refs, err := LiveDiffRefsFor(context.Background(), root, liveTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	current := ""
	for _, branch := range refs.Branches {
		names[branch.Name] = true
		if branch.Current {
			current = branch.Name
		}
	}
	if !names["main"] || !names["feature"] || current != "feature" || len(refs.Commits) < 3 || refs.Commits[0].Subject != "feature work" {
		t.Fatalf("refs=%+v", refs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	_, err = LiveDiff(ctx, root, "working", "", liveTestConfig())
	var typed *LiveDiffError
	if err == nil || !asLiveError(err, &typed) || typed.Code != "git-timeout" {
		t.Fatalf("deadline err=%v", err)
	}
	empty := t.TempDir()
	liveGit(t, empty, "init", "-q")
	_, err = LiveDiff(context.Background(), empty, "working", "", liveTestConfig())
	if err == nil || !asLiveError(err, &typed) || typed.Code != "unborn-head" {
		t.Fatalf("unborn err=%v", err)
	}
	if isRepo, err := IsRepository(context.Background(), empty); err != nil || !isRepo {
		t.Fatalf("empty repository: isRepo=%v err=%v", isRepo, err)
	}
	if isRepo, err := IsRepository(context.Background(), t.TempDir()); err != nil || isRepo {
		t.Fatalf("plain folder: isRepo=%v err=%v", isRepo, err)
	}
	// git never lists a FIFO, but a tracked path can be replaced by one between
	// the listing and the read; the reader refuses it instead of blocking on open.
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan worktreeContent, 1)
	go func() { content, _ := worktreeFacts(root, "pipe", 1<<20); done <- content }()
	select {
	case content := <-done:
		if !content.special {
			t.Fatalf("fifo read as content: %+v", content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
	small := liveTestConfig()
	small.MaxStatusEntries = 2
	capped, err := LiveDiff(context.Background(), root, "working", "", small)
	if err != nil || capped.Truncated == nil || capped.Truncated.Cap != "max_status_entries" || len(capped.Files) != 2 {
		t.Fatalf("status cap=%+v err=%v", capped, err)
	}
}
