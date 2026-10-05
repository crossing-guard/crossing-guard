package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"crossing-guard/store"
)

// realDir is dir's symlink-resolved spelling. On macOS t.TempDir() is itself a
// symlinked spelling (/var/folders → /private/var/folders), which is the
// defect's shape: the same folder written two ways.
func realDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectRootKeyFollowsSymlinks(t *testing.T) {
	base := realDir(t, t.TempDir())
	repo := mkdir(t, filepath.Join(base, "repo"))
	sibling := mkdir(t, filepath.Join(base, "sibling"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if projectRootKey(link) != projectRootKey(repo) {
		t.Fatalf("symlink %q and target %q differ: %q vs %q", link, repo, projectRootKey(link), projectRootKey(repo))
	}
	if projectRootKey(repo+"/") != projectRootKey(repo) {
		t.Fatalf("a trailing slash changed the key")
	}
	if projectRootKey(sibling) == projectRootKey(repo) {
		t.Fatalf("a sibling folder shares the key %q", projectRootKey(repo))
	}
}

func TestProjectRootKeySystemAlias(t *testing.T) {
	logical := t.TempDir()
	physical := realDir(t, logical)
	if logical == physical {
		t.Logf("temp dir %q has one spelling on this system; still asserting one key", logical)
	}
	if projectRootKey(logical) != projectRootKey(physical) {
		t.Fatalf("%q and %q are one folder but have keys %q and %q", logical, physical, projectRootKey(logical), projectRootKey(physical))
	}
	if runtime.GOOS != "darwin" {
		return
	}
	firmlinked := filepath.Join("/System/Volumes/Data", physical)
	if _, err := os.Stat(firmlinked); err != nil {
		t.Logf("no data-volume spelling for %q (%v); firmlink branch skipped", physical, err)
		return
	}
	if projectRootKey(firmlinked) != projectRootKey(physical) {
		t.Fatalf("firmlink %q and %q differ: %q vs %q", firmlinked, physical, projectRootKey(firmlinked), projectRootKey(physical))
	}
}

func TestProjectRootKeyEmptyRelativeAndMissing(t *testing.T) {
	for _, path := range []string{"", ".", "relative/path"} {
		if key := projectRootKey(path); key != "" {
			t.Fatalf("projectRootKey(%q) = %q, want no key", path, key)
		}
	}
	if key := projectRootKey("/nonexistent-crossing-guard/a/../b"); key != "/nonexistent-crossing-guard/b" {
		t.Fatalf("missing folder key = %q, want its cleaned spelling", key)
	}
	// A file is not a folder: the key falls back to its resolved spelling.
	file := filepath.Join(realDir(t, t.TempDir()), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if key := projectRootKey(file); key != file {
		t.Fatalf("file key = %q, want %q", key, file)
	}
}

// A FIFO named as a root or cwd must never block the single-goroutine pump:
// O_DIRECTORY refuses it before a blocking open (a plain O_RDONLY open of a
// FIFO waits for a writer forever).
func TestProjectRootKeyNeverBlocksOnAFIFO(t *testing.T) {
	fifo := filepath.Join(realDir(t, t.TempDir()), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFO support here: %v", err)
	}
	done := make(chan string, 1)
	go func() { done <- projectRootKey(fifo) }()
	select {
	case key := <-done:
		if key != fifo {
			t.Fatalf("FIFO key = %q, want its resolved spelling %q", key, fifo)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("projectRootKey blocked on a FIFO")
	}
}

func TestProjectRootKeyCase(t *testing.T) {
	base := realDir(t, t.TempDir())
	mixed := mkdir(t, filepath.Join(base, "Repo"))
	upper := filepath.Join(base, "REPO")
	a, errA := os.Stat(mixed)
	b, errB := os.Stat(upper)
	if errA == nil && errB == nil && os.SameFile(a, b) {
		if runtime.GOOS != "darwin" {
			t.Skipf("case-insensitive volume on %s: only darwin asks the file system for the on-disk case", runtime.GOOS)
		}
		t.Log("case-insensitive volume: every spelling has the on-disk key")
		for _, spelling := range []string{upper, filepath.Join(base, "repo")} {
			if projectRootKey(spelling) != mixed {
				t.Fatalf("projectRootKey(%q) = %q, want the on-disk %q", spelling, projectRootKey(spelling), mixed)
			}
		}
		return
	}
	t.Log("case-sensitive volume: a case variant is another folder")
	lower := mkdir(t, filepath.Join(base, "repo"))
	if projectRootKey(lower) == projectRootKey(mixed) {
		t.Fatalf("distinct folders %q and %q share a key", lower, mixed)
	}
}

// Red-team PR-1: Go lowercases İ (U+0130) to ASCII i, but APFS keeps İx and Ix
// as two folders. A key that folds case would merge them.
func TestProjectRootKeyNeverMergesUnicodeCase(t *testing.T) {
	base := realDir(t, t.TempDir())
	dotted := mkdir(t, filepath.Join(base, "İx"))
	plain := filepath.Join(base, "Ix")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Skipf("this volume will not hold both %q and %q: %v", dotted, plain, err)
	}
	if projectRootKey(dotted) == projectRootKey(plain) {
		t.Fatalf("distinct folders %q and %q share the key %q", dotted, plain, projectRootKey(plain))
	}
}

// symlinkedFolder makes base/repo, a symlink base/link to it and a sibling
// base/sibling; repo is returned in its resolved spelling, which is what a
// vendor records for a session working there. Shared by the matcher, host,
// natural-lane and HTTP tests.
func symlinkedFolder(t *testing.T, base string) (repo, link, sibling string) {
	t.Helper()
	repo = mkdir(t, filepath.Join(realDir(t, base), "repo"))
	sibling = mkdir(t, filepath.Join(realDir(t, base), "sibling"))
	link = filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	return repo, link, sibling
}

// scopeFolders adds a child of repo to symlinkedFolder's shape.
type scopeFolders struct{ repo, link, sibling, child string }

func newScopeFolders(t *testing.T) scopeFolders {
	t.Helper()
	repo, link, sibling := symlinkedFolder(t, t.TempDir())
	return scopeFolders{repo: repo, link: link, sibling: sibling, child: mkdir(t, filepath.Join(repo, "child"))}
}

func TestBindingScopeMatchesFolderNotSpelling(t *testing.T) {
	folders := newScopeFolders(t)
	cases := []struct {
		name    string
		binding store.ManagedBinding
		task    RuntimeTask
		want    bool
	}{
		{"symlinked root, resolved cwd", store.ManagedBinding{ProjectRoot: folders.link}, RuntimeTask{WorkingDirectory: folders.repo}, true},
		{"resolved root, symlinked cwd", store.ManagedBinding{ProjectRoot: folders.repo}, RuntimeTask{WorkingDirectory: folders.link}, true},
		{"equal spellings", store.ManagedBinding{ProjectRoot: folders.repo}, RuntimeTask{WorkingDirectory: folders.repo + "/"}, true},
		{"runtime scope still refuses", store.ManagedBinding{ProjectRoot: folders.link, ScopeRuntime: "codex"}, RuntimeTask{Runtime: "claude", WorkingDirectory: folders.repo}, false},
		{"session scope still refuses", store.ManagedBinding{ProjectRoot: folders.link, ScopeSession: "s-1"}, RuntimeTask{NativeSessionID: "s-2", WorkingDirectory: folders.repo}, false},
		{"sibling folder", store.ManagedBinding{ProjectRoot: folders.link}, RuntimeTask{WorkingDirectory: folders.sibling}, false},
		{"subdirectory", store.ManagedBinding{ProjectRoot: folders.link}, RuntimeTask{WorkingDirectory: folders.child}, false},
		{"parent folder", store.ManagedBinding{ProjectRoot: folders.child}, RuntimeTask{WorkingDirectory: folders.link}, false},
		{"empty cwd", store.ManagedBinding{ProjectRoot: folders.repo}, RuntimeTask{}, false},
		{"empty root", store.ManagedBinding{}, RuntimeTask{WorkingDirectory: folders.repo}, false},
	}
	for _, tc := range cases {
		if got := bindingScopeMatches(tc.binding, tc.task, newFolderScope(tc.task.WorkingDirectory)); got != tc.want {
			t.Errorf("%s: bindingScopeMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNaturalScopeMatchesFolderNotSpelling(t *testing.T) {
	folders := newScopeFolders(t)
	cases := []struct {
		name    string
		binding store.ManagedBinding
		signal  naturalSessionSignal
		want    bool
	}{
		{"symlinked root, recorded physical cwd", store.ManagedBinding{ProjectRoot: folders.link}, naturalSessionSignal{ProjectRoot: folders.repo}, true},
		{"equal spellings", store.ManagedBinding{ProjectRoot: folders.repo}, naturalSessionSignal{ProjectRoot: folders.repo}, true},
		{"runtime scope still refuses", store.ManagedBinding{ProjectRoot: folders.link, ScopeRuntime: "codex"}, naturalSessionSignal{Runtime: "claude", ProjectRoot: folders.repo}, false},
		{"session scope still refuses", store.ManagedBinding{ProjectRoot: folders.link, ScopeSession: "s-1"}, naturalSessionSignal{CatalogSessionID: "s-2", NativeSessionID: "s-3", ProjectRoot: folders.repo}, false},
		{"sibling folder", store.ManagedBinding{ProjectRoot: folders.link}, naturalSessionSignal{ProjectRoot: folders.sibling}, false},
		{"subdirectory", store.ManagedBinding{ProjectRoot: folders.link}, naturalSessionSignal{ProjectRoot: folders.child}, false},
		{"cwd-less session", store.ManagedBinding{ProjectRoot: folders.repo}, naturalSessionSignal{}, false},
		{"empty root", store.ManagedBinding{}, naturalSessionSignal{ProjectRoot: folders.repo}, false},
	}
	for _, tc := range cases {
		if got := naturalScopeMatches(tc.binding, tc.signal, newFolderScope(tc.signal.ProjectRoot)); got != tc.want {
			t.Errorf("%s: naturalScopeMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// One routing pass resolves its working directory once, and only when a root
// is spelled differently (plan §3.2).
func TestFolderScopeResolvesOnlyWhenSpellingsDiffer(t *testing.T) {
	folders := newScopeFolders(t)
	scope := newFolderScope(folders.repo)
	if !scope.matchesRoot(folders.repo) || scope.resolved {
		t.Fatalf("an equal spelling must match without resolving (resolved=%v)", scope.resolved)
	}
	if !scope.matchesRoot(folders.link) || !scope.resolved {
		t.Fatalf("a symlinked spelling must match by key (resolved=%v)", scope.resolved)
	}
	if scope.matchesRoot(folders.sibling) {
		t.Fatal("a sibling folder matched")
	}
	if newFolderScope("relative").Key() != "" || newFolderScope("").matchesRoot(folders.repo) {
		t.Fatal("a relative or empty directory must match nothing")
	}
}
