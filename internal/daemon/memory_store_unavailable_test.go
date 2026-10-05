package daemon

// memory-store-unavailable-reads plan §6: a store that cannot be read is a 503
// with its reason on every memory read (never an empty list), a store not
// created yet reads as empty, and session enrichment says why it is missing.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"crossing-guard/store"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// memoryReadRoutes is every GET memory read, each with a well-formed request.
var memoryReadRoutes = []string{
	"/api/memory", "/api/memory/pending",
	"/api/memory/search?q=routing", "/api/memory/record?id=routing-update-cost",
	"/api/memory/records?status=active", "/api/memory/tags", "/api/memory/by-tag?key=k&value=v",
}

// memoryReadMux is the memory read routes as main.go wires them.
func memoryReadMux() http.Handler {
	st := NewStore("") // the console list reads the store, never its own directory
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/memory", func(w http.ResponseWriter, _ *http.Request) { handleConsoleMemories(w, st.ListMemories) })
	mux.HandleFunc("GET /api/memory/pending", func(w http.ResponseWriter, _ *http.Request) { handleConsoleMemories(w, ListPendingMemories) })
	mux.HandleFunc("GET /api/memory/search", handleMemorySearch)
	mux.HandleFunc("GET /api/memory/record", handleMemoryRecord)
	mux.HandleFunc("GET /api/memory/records", handleMemoryRecords)
	mux.HandleFunc("GET /api/memory/tags", handleMemoryTagUses)
	mux.HandleFunc("GET /api/memory/by-tag", handleMemoryByTag)
	return mux
}

// withoutGovernor runs the test as a process whose governor never opened the
// store (the fresh-install and degraded start-up case).
func withoutGovernor(t *testing.T) {
	t.Helper()
	saved := governor
	governor = nil
	t.Cleanup(func() { governor = saved })
}

// useIndexDir points the daemon at dir's index for this test, then restores the
// previous index and scan memo, rather than repointing later tests at the real
// home the way setIndexPath("") would.
func useIndexDir(t *testing.T, dir string) {
	t.Helper()
	savedPath, savedScan := resolvedIndexPath, sessionScanCoalescer
	setIndexPath(dir)
	t.Cleanup(func() { resolvedIndexPath, sessionScanCoalescer = savedPath, savedScan })
}

// stampSchema rewrites the store's version, the way a newer binary leaves it.
func stampSchema(t *testing.T, path string, version int) {
	t.Helper()
	raw, err := driver.Open("file:"+path, fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("PRAGMA user_version=" + strconv.Itoa(version)); err != nil {
		t.Fatal(err)
	}
}

// A1: a newer store answers 503 naming the store and its reason on every read.
func TestMemoryReadsReportStoreFailure(t *testing.T) {
	memoryReadServer(t)
	withoutGovernor(t)
	stampSchema(t, indexPath(), 99)
	mux := memoryReadMux()
	for _, path := range memoryReadRoutes {
		rec := memoryGet(t, mux, path)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "v99") {
			t.Errorf("%s: %d %q, want 503 naming the v99 refusal", path, rec.Code, rec.Body.String())
		}
	}
	// Every memory read carries the shared prefix.
	for _, path := range memoryReadRoutes {
		if rec := memoryGet(t, mux, path); !strings.Contains(rec.Body.String(), "memory store unavailable") {
			t.Errorf("%s: %q, want the memory store unavailable prefix", path, rec.Body.String())
		}
	}
}

// A2: a store not created yet, in a process that never opened it, is empty.
func TestMemoryReadsOnAStoreNotCreatedYetAreEmpty(t *testing.T) {
	t.Setenv("CG_MEMORY_DIR", t.TempDir())
	useIndexDir(t, t.TempDir()) // no index.sqlite
	withoutGovernor(t)
	mux := memoryReadMux()
	want := map[string]struct {
		code int
		body string
	}{
		"/api/memory":                               {200, `[]`},
		"/api/memory/pending":                       {200, `[]`},
		"/api/memory/search?q=routing":              {200, `"hits":[]`},
		"/api/memory/record?id=routing-update-cost": {404, `not found`},
		"/api/memory/records?status=active":         {200, `{"records":[]}`},
		"/api/memory/tags":                          {200, `{"tags":[]}`},
		"/api/memory/by-tag?key=k&value=v":          {200, `"records":[]`},
	}
	for _, path := range memoryReadRoutes {
		rec := memoryGet(t, mux, path)
		w := want[path]
		if rec.Code != w.code || !strings.Contains(rec.Body.String(), w.body) {
			t.Errorf("%s: %d %q, want %d containing %q", path, rec.Code, rec.Body.String(), w.code, w.body)
		}
	}
	if _, err := os.Stat(indexPath()); !os.IsNotExist(err) {
		t.Fatalf("a read must not create the store: %v", err)
	}
}

// A2b: a missing store is empty only while no governor holds it, and a dangling
// symlink is unavailable, not empty.
func TestMemoryReadsOnAMissingStoreUnderAGovernorOrDanglingLinkFail(t *testing.T) {
	t.Setenv("CG_MEMORY_DIR", t.TempDir())
	dir := t.TempDir()
	useIndexDir(t, dir)
	mux := memoryReadMux()

	saved := governor
	governor = &Governor{}
	t.Cleanup(func() { governor = saved })
	for _, path := range memoryReadRoutes {
		rec := memoryGet(t, mux, path)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "is missing") {
			t.Errorf("governor running, %s: %d %q, want 503 naming the missing index", path, rec.Code, rec.Body.String())
		}
	}

	governor = nil
	if err := os.Symlink(filepath.Join(dir, "gone", "index.sqlite"), indexPath()); err != nil {
		t.Fatal(err)
	}
	for _, path := range memoryReadRoutes {
		if rec := memoryGet(t, mux, path); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("dangling link, %s: %d %q, want 503", path, rec.Code, rec.Body.String())
		}
	}
}

// A4: enrichment against an unreadable store keeps the vendor rows and says why
// the change-evidence rows are missing; a readable or not-created store says
// nothing; the coalescer keeps the reason with the rows it qualifies.
func TestSessionEnrichmentReportsAnUnreadableStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no vendor session stores
	withoutGovernor(t)
	dir := t.TempDir()
	useIndexDir(t, dir)

	if _, problem := scanSessionsUncoalesced(); problem != "" {
		t.Fatalf("a store not created yet is no problem, got %q", problem)
	}
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	ix.Close()
	if _, problem := scanSessionsUncoalesced(); problem != "" {
		t.Fatalf("a readable store is no problem, got %q", problem)
	}
	stampSchema(t, indexPath(), 99)
	if _, problem := scanSessionsUncoalesced(); !strings.Contains(problem, "v99") {
		t.Fatalf("an unreadable store must say why, got %q", problem)
	}

	c := &scanCoalescer{}
	rows := c.get(func() ([]SessionSummary, string) { return []SessionSummary{{ID: "vendor-row"}}, "store schema is v99" })
	if len(rows) != 1 || c.changeEvidenceProblem() != "store schema is v99" {
		t.Fatalf("the coalescer must keep the rows and their problem together: %v %q", rows, c.changeEvidenceProblem())
	}
}

// A4: the rail answers carry the problem; a clean scan leaves the field out.
func TestSessionRailCarriesTheChangeEvidenceProblem(t *testing.T) {
	f := newOrganizationFixture(t) // a real store, so the organized rail can read tags
	scan := f.scan
	presence := func(time.Time) presenceOpenSet { return unknownPresence("test") }
	routes := map[string]http.HandlerFunc{
		"/api/sessions?view=rail": func(w http.ResponseWriter, r *http.Request) { handleSessionsWith(w, r, scan, presence) },
		// The organized rail (a filter or grouping) builds its own answer.
		"/api/sessions?view=rail&group_by=runtime": func(w http.ResponseWriter, r *http.Request) { handleSessionsWith(w, r, scan, presence) },
		"/api/sessions/peers?cwd=" + t.TempDir() + "&cwd_source=test&scope=all": func(w http.ResponseWriter, r *http.Request) {
			handleSessionPeersWith(w, r, scan, presence)
		},
	}
	for path, handler := range routes {
		for _, problem := range []string{"", "store schema is v99"} {
			sessionScanCoalescer = &scanCoalescer{problem: problem}
			rec := memoryGet(t, handler, path)
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
			}
			got, present := out["change_evidence_problem"]
			if problem == "" && present {
				t.Errorf("%s: a clean scan must omit the field, got %v", path, got)
			}
			if problem != "" && got != problem {
				t.Errorf("%s must carry %q, got %v", path, problem, got)
			}
		}
	}
}

// Postwork RT-P4: a session no transcript matches is looked up in the store; a
// store that cannot be read is said, not answered as "session not found".
func TestLoadSessionFallbackNamesAnUnreadableIndex(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no vendor session stores
	withoutGovernor(t)
	useIndexDir(t, t.TempDir())
	if _, err := LoadSession("claude", "absent"); err == nil || !strings.HasPrefix(err.Error(), "session not found: ") {
		t.Fatalf("a store not created yet: want session not found, got %v", err)
	}
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	ix.Close()
	stampSchema(t, indexPath(), 99)
	if _, err := LoadSession("claude", "absent"); err == nil || !strings.HasPrefix(err.Error(), "session store unreadable: ") || !strings.Contains(err.Error(), "v99") {
		t.Fatalf("an unreadable store must be named, got %v", err)
	}
}
