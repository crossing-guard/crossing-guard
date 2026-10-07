package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/store"
)

// A collector may legitimately observe a call without knowing its runtime (the
// natural-smoke guardcli records carried only a session label). Such an envelope is
// valid governance evidence; it just cannot assert a canonical session root, so the
// activity fact is skipped rather than failing the whole append. Before this guard,
// one such spooled record failed "session root requires runtime and native session
// identity" and retried every five seconds forever.
func TestRuntimelessObservationIngestsWithoutSessionActivity(t *testing.T) {
	repo := v1TestRepo(t)
	data := t.TempDir()
	ix, err := store.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() { _ = ix.Close() })
	e := testV1Envelope(t, repo)
	e.Runtime = ""
	e.SessionID = "orchestration-natural-smoke"
	receipt, err := ingestObservationV1(t.Context(), g, e)
	if err != nil {
		t.Fatalf("runtime-less envelope must ingest as evidence: %v", err)
	}
	if receipt.EventID == 0 {
		t.Fatal("expected a recorded event for the runtime-less observation")
	}
	// And end-to-end through the spool: the exact shape that used to poison replay.
	spooled := testV1Envelope(t, repo)
	spooled.Runtime = ""
	spooled.SessionID = "orchestration-natural-smoke"
	spooled.ObservationID = "obs_feedfacefeedfacefeedfacefeedface"
	spooled.ActionID = "act_feedfacefeedfacefeedfacefeedface"
	b, err := json.Marshal(spooled)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, "observation-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, spooled.ObservationID+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replayObservationSpoolOnce(t.Context(), data, g); err != nil {
		t.Fatalf("replay of runtime-less spool record must succeed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("delivered spool record must be removed, stat err=%v", err)
	}
}

// A spooled record whose delivery-attempt budget is exhausted is quarantined with
// its bytes preserved and a collection issue recorded — never retried forever.
func TestReplayGivesUpAfterMaxDeliveryAttempts(t *testing.T) {
	repo := v1TestRepo(t)
	data := t.TempDir()
	ix, err := store.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() { _ = ix.Close() })
	e := testV1Envelope(t, repo)
	e.DeliveryAttempts = replayMaxDeliveryAttempts + 50
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, "observation-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, e.ObservationID+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replayObservationSpoolOnce(t.Context(), data, g); err != nil {
		t.Fatalf("give-up pass must not report an error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("exhausted record must leave the live spool, stat err=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".rejected") {
			preserved = true
		}
	}
	if !preserved {
		t.Fatal("exhausted record must be preserved as a .rejected quarantine file")
	}
	events, err := ix.EventsForSession(e.SessionID, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("quarantined record must not ingest, events=%d err=%v", len(events), err)
	}
}

// Concurrent ScanSessions callers share one underlying scan; a completed result is
// reused only within the short burst window, and every caller gets an isolated
// top-level slice.
func TestScanCoalescerSharesOneScanWithinWindow(t *testing.T) {
	var c scanCoalescer
	calls := 0
	scan := func() ([]SessionSummary, string) {
		calls++
		time.Sleep(20 * time.Millisecond)
		return []SessionSummary{{ID: "one"}}, ""
	}
	type result struct{ out []SessionSummary }
	results := make(chan result, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- result{c.get(scan)} }()
	}
	for i := 0; i < 8; i++ {
		r := <-results
		if len(r.out) != 1 || r.out[0].ID != "one" {
			t.Fatalf("unexpected shared result %+v", r.out)
		}
	}
	if calls != 1 {
		t.Fatalf("concurrent callers ran %d scans, want 1", calls)
	}
	first := c.get(scan)
	first[0].ID = "mutated"
	second := c.get(scan)
	if second[0].ID != "one" {
		t.Fatal("callers must receive isolated slice copies")
	}
	if calls != 1 {
		t.Fatalf("burst window reuse ran %d scans, want still 1", calls)
	}
	c.done = time.Now().Add(-2 * scanReuseWindow)
	_ = c.get(scan)
	if calls != 2 {
		t.Fatalf("expired window ran %d scans, want 2", calls)
	}
}
