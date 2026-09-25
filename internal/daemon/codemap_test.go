package daemon

// The codemap surface is reached from HTTP handlers, so it is exercised
// concurrently by construction. This file tests that property, because the
// failure mode is not a wrong answer — it is a dead daemon.

import (
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// repoRootForTest locates the module root from this file's own path, so the
// test does not depend on the working directory a runner happens to use.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(self), "..", ".."))
}

// TestCodemapDescriptorIsConcurrencySafe is a regression test for a FATAL bug,
// not a nice-to-have.
//
// The language adapter is registered once per process and its lazily-populated
// module cache was an unsynchronised map written from every Analyze call. Those
// calls arrive on HTTP handler goroutines, so two descriptor requests in flight
// at once raced it — and a concurrent map read/write in Go is an unrecoverable
// runtime fatal, not a panic something can catch. Two reference clicks in the
// console, or two open tabs, were enough to take the daemon down.
//
// Run under -race, which is why `go test -race ./...` belongs in the check
// script: without the adapter's mutex this reports a data race, and with
// unlucky timing it kills the test binary outright.
func TestCodemapDescriptorIsConcurrencySafe(t *testing.T) {
	root := repoRootForTest(t)
	// Several files from the same package and several goroutines per file: the
	// repeats race the descriptor cache, the spread races the module cache and
	// the shared project graph.
	paths := []string{
		"codemap/describe.go", "codemap/role.go", "codemap/port.go",
		"codemap/descriptor.go", "internal/daemon/refindex.go",
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel := paths[i%len(paths)]
			r := httptest.NewRequest("GET", "/api/codemap/descriptor?cwd="+root+"&path="+rel, nil)
			w := httptest.NewRecorder()
			handleCodemapDescriptor(w, r)
			if w.Code != 200 {
				t.Errorf("%s: status %d: %s", rel, w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
}

// TestEntityCandidatesBridgesRelativeAndAbsolute pins the fix for the bug that
// made cross-session backlinks return zero for every file in every project.
//
// The reference index speaks repo-relative paths; the governor's event log
// records absolute ones. Nothing reconciled them, so the panel said "no session
// has touched this" about files the currently-open session had just created.
func TestEntityCandidatesBridgesRelativeAndAbsolute(t *testing.T) {
	root := "/Users/x/proj"
	got := entityCandidates(root, "store/govern.go")
	if len(got) == 0 || got[0] != "/Users/x/proj/store/govern.go" {
		t.Fatalf("relative target must resolve against the root first, got %v", got)
	}
	// macOS records /tmp and /private/tmp for the same tree; both spellings
	// must be tried or a project under either misses for the same reason.
	if !contains(got, "/private/Users/x/proj/store/govern.go") {
		t.Errorf("missing the /private spelling: %v", got)
	}
	// An id that is not a path at all must survive untouched.
	if got := entityCandidates(root, "file:mcp:some_tool"); len(got) != 1 {
		t.Errorf("an explicit file: id must not be rewritten, got %v", got)
	}
	// An absolute target is already in the store's vocabulary.
	abs := entityCandidates(root, "/elsewhere/a.go")
	if len(abs) != 1 || abs[0] != "/elsewhere/a.go" {
		t.Errorf("absolute target must pass through, got %v", abs)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
