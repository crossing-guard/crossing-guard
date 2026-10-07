package daemon

// memory_propose_test.go — the propose door's server-side contract (plan §5.2
// / RT-6): consent off = 404 with nothing written; consent on = a pending
// record with a session citation; unidentified callers are refused; the rate
// bound holds.

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

func proposeTestServer(t *testing.T, proposeEnabled bool) http.Handler {
	t.Helper()
	dataDir := t.TempDir()
	// setIndexPath takes the DATA dir (store.IndexPath appends index.sqlite).
	setIndexPath(dataDir)
	t.Cleanup(func() { setIndexPath("") })
	// The propose policy is the memory owner's: memory.json's propose
	// section (config-ownership plan Fix A). The per-session bound is 2 so
	// the rate test exercises the limiter.
	config := `{"format_version":1,"propose":{"enabled":` + boolJSON(proposeEnabled) + `,"per_session_max":2}}`
	if err := os.WriteFile(filepath.Join(dataDir, "memory.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	// The propose limiter is per-test so the window never leaks between cases.
	memoryProposeLimits = &proposeLimiter{counts: map[string]int{}, since: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/memory/propose", handleMemoryPropose)
	return mux
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func proposeCall(t *testing.T, mux http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/memory/propose", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestProposeRouteRefusedWithoutConsent(t *testing.T) {
	mux := proposeTestServer(t, false)
	rec := proposeCall(t, mux, `{"title":"t","body":"b","session":"claude/s1"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 without consent, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not enabled") {
		t.Fatalf("the refusal must say why: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "memory.json") {
		t.Fatalf("the refusal must name the memory owner's file: %s", rec.Body.String())
	}
}

func TestProposeRouteWritesPendingWithCitation(t *testing.T) {
	mux := proposeTestServer(t, true)
	rec := proposeCall(t, mux, `{"title":"A lesson","body":"we learned it","category":"how-to","session":"claude/sess-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "pending" {
		t.Fatalf("the route must only ever write pending, got %v", out["status"])
	}
	id, _ := out["id"].(string)

	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	r, err := ix.MemoryByID(id)
	if err != nil {
		t.Fatalf("record not written: %v", err)
	}
	if r.Status != "pending" || r.Source != "agent" {
		t.Fatalf("record shape wrong: %+v", r)
	}
	sources, err := ix.MemorySources(id)
	if err != nil || len(sources) != 1 || sources[0].Vendor != "claude" || sources[0].SessionID != "sess-1" {
		t.Fatalf("citation missing: %v %v", sources, err)
	}
}

func TestProposeRouteRefusesUnidentifiedCaller(t *testing.T) {
	mux := proposeTestServer(t, true)
	rec := proposeCall(t, mux, `{"title":"t","body":"b"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a citation-less proposal, got %d", rec.Code)
	}
}

func TestProposeRouteRateBounds(t *testing.T) {
	mux := proposeTestServer(t, true)
	for i := 0; i < 2; i++ {
		rec := proposeCall(t, mux, `{"title":"t`+string(rune('a'+i))+`","body":"b","session":"claude/sess-1"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d should pass, got %d", i, rec.Code)
		}
	}
	rec := proposeCall(t, mux, `{"title":"t-third","body":"b","session":"claude/sess-1"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the third proposal must be rate-refused, got %d", rec.Code)
	}
	// A different session is not affected by sess-1's budget.
	rec = proposeCall(t, mux, `{"title":"t-other","body":"b","session":"claude/sess-2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("another session must keep its own budget, got %d", rec.Code)
	}
}

// RT-C1's acceptance: the consent is LIVE. Flipping memory.json's propose
// section mid-run is obeyed by the next route call — no restart, no cache
// trick; the mtime stamp is the trigger.
func TestProposeConsentIsLive(t *testing.T) {
	dataDir := t.TempDir()
	setIndexPath(dataDir)
	t.Cleanup(func() { setIndexPath("") })
	writeProposeConfig := func(enabled bool) {
		body := `{"format_version":1,"propose":{"enabled":` + boolJSON(enabled) + `,"per_session_max":5}}`
		if err := os.WriteFile(filepath.Join(dataDir, "memory.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProposeConfig(false)
	memoryProposeLimits = &proposeLimiter{counts: map[string]int{}, since: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/memory/propose", handleMemoryPropose)

	first := proposeCall(t, mux, `{"title":"t","body":"b","session":"claude/sess-1"}`)
	if first.Code != http.StatusNotFound {
		t.Fatalf("consent off: expected 404, got %d", first.Code)
	}

	// The flip: same process, same store, next call.
	writeProposeConfig(true)
	// mtime granularity: ensure the stamp differs (the filesystem may share
	// a second between writes).
	time.Sleep(1100 * time.Millisecond)
	second := proposeCall(t, mux, `{"title":"t","body":"b","session":"claude/sess-1"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("consent on after a mid-run flip: expected 200, got %d: %s", second.Code, second.Body.String())
	}

	// And off again — the door closes the same way.
	writeProposeConfig(false)
	time.Sleep(1100 * time.Millisecond)
	third := proposeCall(t, mux, `{"title":"t2","body":"b","session":"claude/sess-1"}`)
	if third.Code != http.StatusNotFound {
		t.Fatalf("consent off after a mid-run flip: expected 404, got %d", third.Code)
	}
}

// Two proposals in one second were one record (the second edited the first,
// memory-create-identity plan §1.1). Back-to-back calls usually share a second;
// each proposal must be its own record either way.
func TestProposeRouteBackToBackPairIsTwoRecords(t *testing.T) {
	mux := proposeTestServer(t, true)
	ids := map[string]bool{}
	for _, title := range []string{"first", "second"} {
		rec := proposeCall(t, mux, `{"title":"`+title+`","body":"b","session":"claude/sess-1"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", title, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		ids[out["id"].(string)] = true
	}
	if len(ids) != 2 {
		t.Fatalf("two proposals must be two records, got ids %v", ids)
	}
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for id := range ids {
		r, err := ix.MemoryByID(id)
		if err != nil || r.Revision != 1 {
			t.Fatalf("%s: %+v %v", id, r, err)
		}
	}
}

// A label the store would refuse is a 400 before the rate bound spends a slot.
func TestProposeRouteBadTagsRefusedWithoutSpendingBudget(t *testing.T) {
	mux := proposeTestServer(t, true)
	for i := 0; i < 3; i++ {
		rec := proposeCall(t, mux, `{"title":"t","body":"b","tags":["a","a"],"session":"claude/sess-1"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("duplicate tags must be 400, got %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := proposeCall(t, mux, `{"title":"t","body":"b","tags":["orders"],"session":"claude/sess-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refused proposals must not spend the budget, got %d %s", rec.Code, rec.Body.String())
	}
}
