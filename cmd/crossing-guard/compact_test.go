package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/store"
)

// compactFixture is a data directory whose store holds a megabyte of free pages.
func compactFixture(t *testing.T) (string, int64) {
	t.Helper()
	// runCompact points SQLite's temporary directory at the data directory; restore
	// it when the test ends, or later tests inherit a deleted directory.
	t.Setenv("SQLITE_TMPDIR", os.Getenv("SQLITE_TMPDIR"))
	dir := t.TempDir()
	path := filepath.Join(dir, "index.sqlite")
	index, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	generation := store.UnderstandingGeneration{RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo",
		Status: "complete", SnapshotProtocol: "git-tree-v2", SnapshotDigest: "git-tree-v2-sha256:a",
		StructuralSchema: "codemap-v1", AnalyzerBundleDigest: "sha256-v1:bundle", ConventionState: "none", StartedAt: 1, EndedAt: 2}
	for i := 0; i < 300; i++ {
		generation.Units = append(generation.Units, store.UnderstandingUnit{Path: strings.Repeat("p", i+1), SourceHash: "sha256-v1:s",
			DescriptorJSON: strings.Repeat("x", 4000) + strings.Repeat("y", i)})
	}
	if err := index.AppendUnderstanding(&generation); err != nil {
		t.Fatal(err)
	}
	newer := generation
	newer.Units, newer.SnapshotDigest, newer.StartedAt, newer.EndedAt = nil, "git-tree-v2-sha256:b", 3, 4
	if err := index.AppendUnderstanding(&newer); err != nil {
		t.Fatal(err)
	}
	if pruned, err := index.PruneUnderstandingFacts(generation.ID, 1<<40, 1<<62); err != nil || !pruned {
		t.Fatalf("prune pruned=%v err=%v", pruned, err)
	}
	for more := true; more; {
		if more, err = index.DeletePrunedUnderstandingFacts(generation.ID, 1000); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := index.DeleteUnreferencedUnderstandingDescriptors(1000); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return dir, info.Size()
}

func storeSize(t *testing.T, dir string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestCompactShrinksAStoreNoDaemonHolds(t *testing.T) {
	dir, before := compactFixture(t)
	if code := runCompact([]string{"--data", dir}); code != 0 {
		t.Fatalf("compact exit=%d", code)
	}
	if after := storeSize(t, dir); before-after < 1<<20 {
		t.Fatalf("store %d -> %d bytes", before, after)
	}
	// A stale daemon-addr (nothing listening) is not a daemon.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stale := listener.Addr().String()
	listener.Close()
	if err := os.WriteFile(filepath.Join(dir, "daemon-addr"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runCompact([]string{"--data", dir}); code != 0 {
		t.Fatalf("compact beside a stale daemon-addr exit=%d", code)
	}
}

func TestCompactRefusesWhileADaemonAnswers(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"healthy":        func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
		"rejected token": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
	} {
		t.Run(name, func(t *testing.T) {
			dir, before := compactFixture(t)
			server := httptest.NewServer(handler)
			defer server.Close()
			if err := os.WriteFile(filepath.Join(dir, "daemon-addr"), []byte(strings.TrimPrefix(server.URL, "http://")), 0o600); err != nil {
				t.Fatal(err)
			}
			if code := runCompact([]string{"--data", dir}); code == 0 {
				t.Fatal("compact ran beside an answering daemon")
			}
			if after := storeSize(t, dir); after != before {
				t.Fatalf("a refused compact changed the store: %d -> %d", before, after)
			}
		})
	}
}

// A process that holds the store while no daemon-addr names it is invisible to the
// probe; the exclusive open is what stops compact, and the store is left as it was.
func TestCompactFailsBusyBesideAHolderTheProbeCannotSee(t *testing.T) {
	dir, before := compactFixture(t)
	holder, err := store.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if code := runCompact([]string{"--data", dir}); code == 0 {
		t.Fatal("compact ran beside another holder of the store")
	}
	if after := storeSize(t, dir); after != before {
		t.Fatalf("a failed compact changed the store: %d -> %d", before, after)
	}
}
