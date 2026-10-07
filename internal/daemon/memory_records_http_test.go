package daemon

// memory_records_http_test.go — the memory read routes the CLI and the recall
// server share (memory-reads-through-daemon plan §4.2–§4.3): the one search
// scorer, the record and records shapes, honest store errors, and the
// caller-labelled recall log.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/store"
)

// memoryReadServer seeds a scratch store and serves the three read routes.
// The recall log lands in a scratch memory dir the test can read back.
func memoryReadServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	dataDir := t.TempDir()
	memDir := t.TempDir()
	t.Setenv("CG_MEMORY_DIR", memDir)
	setIndexPath(dataDir)
	t.Cleanup(func() { setIndexPath("") })
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	seed := func(r store.MemoryRecord) {
		if r.Status == "" {
			r.Status = "active"
		}
		storeMemoryRecord(t, ix, nil, r)
	}
	seed(store.MemoryRecord{ID: "routing-update-cost", Title: "Routing update cost", Category: "bug-fix",
		Body: "Refresh recalls the price APIs from the routing screen.", Tags: []string{"orders"}})
	seed(store.MemoryRecord{ID: "alias-hit", Title: "Unrelated title", Category: "how-to",
		Body: "nothing here", Aliases: []string{"routing"}})
	seed(store.MemoryRecord{ID: "tag-hit", Title: "Another", Category: "how-to",
		Body: "plain", Tags: []string{"routing"}})
	seed(store.MemoryRecord{ID: "body-hit", Title: "Body only", Category: "gotcha",
		Body: "the routing screen hides a trap"})
	seed(store.MemoryRecord{ID: "repo-scoped", Title: "Repo routing note", Category: "convention",
		Body: "scoped", ScopeType: store.MemoryScopeRepository, ScopeID: "example-app"})
	seed(store.MemoryRecord{ID: "pending-routing", Title: "Pending routing idea", Category: "note",
		Body: "unreviewed", Status: "pending"})
	seed(store.MemoryRecord{ID: "rejected-routing", Title: "Rejected routing claim", Category: "note",
		Body: "wrong", Status: "pending"})
	if _, err := ix.RejectMemory("rejected-routing", "wrong", store.MemoryActor{AuthorType: "user", AuthorID: "tester", ActorSource: "test"}); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/memory/search", handleMemorySearch)
	mux.HandleFunc("GET /api/memory/record", handleMemoryRecord)
	mux.HandleFunc("GET /api/memory/records", handleMemoryRecords)
	return mux, memDir
}

func memoryGet(t *testing.T, mux http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func decodeSearch(t *testing.T, rec *httptest.ResponseRecorder) memorySearchResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var out memorySearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func recallOps(t *testing.T, memDir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(memDir, "recall-log.jsonl"))
	if err != nil {
		return nil
	}
	var ops []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row struct {
			Op string `json:"op"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, row.Op)
	}
	return ops
}

func TestMemorySearchScoresTitleAliasTagBody(t *testing.T) {
	mux, _ := memoryReadServer(t)
	out := decodeSearch(t, memoryGet(t, mux, "/api/memory/search?q=routing"))
	scores := map[string]int{}
	for _, h := range out.Hits {
		scores[h.ID] = h.Score
	}
	want := map[string]int{"routing-update-cost": 3, "alias-hit": 3, "tag-hit": 2, "body-hit": 1,
		"repo-scoped": 3, "pending-routing": 3}
	for id, score := range want {
		if scores[id] != score {
			t.Errorf("%s: score %d, want %d (all: %v)", id, scores[id], score, scores)
		}
	}
	if out.Hits[len(out.Hits)-1].ID != "body-hit" {
		t.Errorf("lowest score must sort last: %+v", out.Hits)
	}
}

func TestMemorySearchNeverReturnsRejectedAndDisclosesPending(t *testing.T) {
	mux, _ := memoryReadServer(t)
	out := decodeSearch(t, memoryGet(t, mux, "/api/memory/search?q=routing"))
	for _, h := range out.Hits {
		if h.ID == "rejected-routing" {
			t.Fatalf("a rejected record must never be a hit: %+v", h)
		}
		if h.ID == "pending-routing" && !h.Pending {
			t.Fatalf("a pending hit must say so: %+v", h)
		}
	}
}

func TestMemorySearchFacetsLimitAndTagOnly(t *testing.T) {
	mux, _ := memoryReadServer(t)
	if out := decodeSearch(t, memoryGet(t, mux, "/api/memory/search?q=routing&category=gotcha")); len(out.Hits) != 1 || out.Hits[0].ID != "body-hit" {
		t.Errorf("category facet: %+v", out.Hits)
	}
	if out := decodeSearch(t, memoryGet(t, mux, "/api/memory/search?q=routing&repository=example-app")); len(out.Hits) != 1 || out.Hits[0].ID != "repo-scoped" {
		t.Errorf("repository facet: %+v", out.Hits)
	}
	out := decodeSearch(t, memoryGet(t, mux, "/api/memory/search?tag=orders"))
	if len(out.Hits) != 1 || out.Hits[0].ID != "routing-update-cost" || out.Hits[0].Why[0] != "tag:orders" {
		t.Errorf("tag-only search: %+v", out.Hits)
	}
	out = decodeSearch(t, memoryGet(t, mux, "/api/memory/search?q=routing&limit=2"))
	if len(out.Hits) != 2 || !out.Truncated || out.Total != 6 {
		t.Errorf("limit: hits=%d truncated=%v total=%d", len(out.Hits), out.Truncated, out.Total)
	}
}

func TestMemorySearchExcerptIsBounded(t *testing.T) {
	mux, _ := memoryReadServer(t)
	limits, _ := consoleConfig()
	out := decodeSearch(t, memoryGet(t, mux, "/api/memory/search?q=update"))
	for _, h := range out.Hits {
		if len(h.Excerpt) > limits.Recall.MemoryExcerptBytes {
			t.Fatalf("excerpt over budget: %d > %d", len(h.Excerpt), limits.Recall.MemoryExcerptBytes)
		}
	}
}

func TestMemoryReadsLabelTheRecallLogByCaller(t *testing.T) {
	mux, memDir := memoryReadServer(t)
	memoryGet(t, mux, "/api/memory/search?q=routing")
	memoryGet(t, mux, "/api/memory/search?q=routing&via=cli")
	memoryGet(t, mux, "/api/memory/record?id=routing-update-cost")
	memoryGet(t, mux, "/api/memory/record?id=routing-update-cost&via=cli")
	memoryGet(t, mux, "/api/memory/records?status=active")
	got := strings.Join(recallOps(t, memDir), ",")
	if got != "mcp-search,cli-search,get,cli-get" {
		t.Fatalf("recall log ops = %q (the list read logs nothing)", got)
	}
	for _, path := range []string{"/api/memory/search?q=x&via=console", "/api/memory/record?id=routing-update-cost&via=x"} {
		if rec := memoryGet(t, mux, path); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", path, rec.Code)
		}
	}
}

func TestMemoryRecordShapeAndNotFound(t *testing.T) {
	mux, _ := memoryReadServer(t)
	rec := memoryGet(t, mux, "/api/memory/record?id=repo-scoped")
	if rec.Code != http.StatusOK {
		t.Fatalf("record: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["scope_type"] != "repository" || out["repository"] != "example-app" {
		t.Errorf("scope: %v / %v", out["scope_type"], out["repository"])
	}
	for _, key := range []string{"tags", "aliases", "sources"} {
		if _, ok := out[key].([]any); !ok {
			t.Errorf("%s must be an array, never null: %#v", key, out[key])
		}
	}
	for _, id := range []string{"no-such-record", "../etc"} {
		if rec := memoryGet(t, mux, "/api/memory/record?id="+id); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", id, rec.Code)
		}
	}
}

func TestMemoryRecordsListsOneStatusInTheRecordShape(t *testing.T) {
	mux, _ := memoryReadServer(t)
	counts := map[string]int{"active": 5, "pending": 1, "rejected": 1}
	for status, want := range counts {
		rec := memoryGet(t, mux, "/api/memory/records?status="+status)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", status, rec.Code, rec.Body.String())
		}
		var out struct {
			Records []map[string]any `json:"records"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Records) != want {
			t.Errorf("%s: %d records, want %d", status, len(out.Records), want)
		}
		for _, r := range out.Records {
			if r["status"] != status || r["body"] == "" || r["category"] == "" {
				t.Errorf("%s record shape: %v", status, r)
			}
			if _, has := r["sources"]; has {
				t.Errorf("the list read carries no sources: %v", r)
			}
		}
	}
	for _, status := range []string{"", "all", "active'--", "x'y"} {
		if rec := memoryGet(t, mux, "/api/memory/records?status="+status); rec.Code != http.StatusBadRequest {
			t.Errorf("status %q: %d, want 400", status, rec.Code)
		}
	}
}

// TestMemoryConfigNamesTheStoreTheDaemonReads is the PW-1 contract the CLI's
// store check relies on: the daemon reports the index it resolved at start.
func TestMemoryConfigNamesTheStoreTheDaemonReads(t *testing.T) {
	memoryReadServer(t)
	rec := httptest.NewRecorder()
	handleMemoryConfig(rec, httptest.NewRequest("GET", "/api/memory/config", nil))
	var out memoryConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Store.IndexPath == "" || out.Store.IndexPath != indexPath() {
		t.Fatalf("config must name the daemon's index: %q (daemon reads %q)", out.Store.IndexPath, indexPath())
	}
}

// TestMemoryRecordTimesAreRFC3339UTC pins the created/updated format the CLI's
// SessionStart index converts back through memory.RecordFromStore (PW-4).
func TestMemoryRecordTimesAreRFC3339UTC(t *testing.T) {
	mux, _ := memoryReadServer(t)
	rec := memoryGet(t, mux, "/api/memory/records?status=active")
	var out struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Records {
		for _, key := range []string{"created", "updated"} {
			v, _ := r[key].(string)
			if parsed, err := time.Parse(time.RFC3339, v); err != nil || parsed.Location() != time.UTC || !strings.HasSuffix(v, "Z") {
				t.Fatalf("%s %s = %q, want RFC3339 UTC", r["id"], key, v)
			}
		}
	}
}
