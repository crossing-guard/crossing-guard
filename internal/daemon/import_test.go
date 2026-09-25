package daemon

import (
	"path/filepath"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/store"
)

// TestImportSkipsSessionsAlreadyCapturedLive exercises the never-clobber GUARD directly:
// importOneSession must return -1 (skip) for a session that already has live events,
// BEFORE it ever reads the vendor file — so live truth is never overwritten by a lossy
// import. (The sibling test above pins labelling; this one pins the skip the name promises.)
func TestImportSkipsSessionsAlreadyCapturedLive(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	s := harvest.SessionSummary{Runtime: "claude", ID: "live-session", Path: "/does/not/exist.jsonl"}
	sid := harvest.CanonicalID(s)
	// Seed a LIVE event for it. The path is bogus on purpose: if the skip works, the
	// importer returns before ever trying to read it.
	if err := g.Observe(Observation{SessionID: sid, Tool: "Read", Command: "x", TS: 10}); err != nil {
		t.Fatal(err)
	}
	n, _, err := importOneSession(g, ix, s, &ImportResult{})
	if err != nil {
		t.Fatalf("skip must not error (it must not read the file): %v", err)
	}
	if n != -1 {
		t.Fatalf("a session with live events must be SKIPPED (-1), got %d", n)
	}
	if imp, _ := ix.CountEventsByOrigin(sid, "imported"); imp != 0 {
		t.Errorf("skipped session gained imported rows: %d", imp)
	}
}

// TestImportedEventsAreLabelledAndNeverClobberLive pins the Phase 4 invariants: an
// imported event is stamped origin=imported (never confused with live truth), and the
// live observe path always writes origin=live regardless of what a caller asks.
func TestImportedEventsAreLabelledAndNeverClobberLive(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	// An imported observation is stamped imported.
	if err := g.Observe(Observation{SessionID: "hist", Tool: "Read", Command: "x", TS: 100, Origin: "imported"}); err != nil {
		t.Fatal(err)
	}
	imp, _ := ix.CountEventsByOrigin("hist", "imported")
	if imp != 1 {
		t.Fatalf("imported event not labelled: got %d imported rows", imp)
	}

	// An observation with no origin defaults to live.
	if err := g.Observe(Observation{SessionID: "hist", Tool: "Read", Command: "y", TS: 200}); err != nil {
		t.Fatal(err)
	}
	live, _ := ix.CountEventsByOrigin("hist", "live")
	if live != 1 {
		t.Fatalf("default origin is not live: got %d live rows", live)
	}
}

// TestReimportReplacesOnlyImportedRows pins idempotency + safety: DeleteImportedForSession
// removes imported rows so a re-import does not duplicate, while leaving LIVE truth
// untouched.
func TestReimportReplacesOnlyImportedRows(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	if err := g.Observe(Observation{SessionID: "s", Tool: "Read", Command: "live-truth", TS: 10}); err != nil {
		t.Fatal(err) // live
	}
	if err := g.Observe(Observation{SessionID: "s", Tool: "Read", Command: "old-import", TS: 5, Origin: "imported"}); err != nil {
		t.Fatal(err)
	}
	if err := g.Observe(Observation{SessionID: "s", Runtime: "codex", Tool: "exec",
		Content: "outer-call", TS: 8, Origin: "transcript"}); err != nil {
		t.Fatal(err)
	}

	deleted, err := ix.DeleteImportedForSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("delete removed %d, want 1 imported row", deleted)
	}
	if live, _ := ix.CountEventsByOrigin("s", "live"); live != 1 {
		t.Errorf("delete-imported removed a LIVE row; %d left", live)
	}
	if imp, _ := ix.CountEventsByOrigin("s", "imported"); imp != 0 {
		t.Errorf("imported rows remain after delete: %d", imp)
	}
	if transcript, _ := ix.CountEventsByOrigin("s", "transcript"); transcript != 1 {
		t.Errorf("delete-imported removed transcript truth; %d left", transcript)
	}
}

// TestParseEventTime accepts the vendor timestamp shapes and refuses the rest, so an
// undatable event is skipped rather than folded at a fabricated time.
func TestParseEventTime(t *testing.T) {
	good := []string{"2026-07-20T10:00:00Z", "2026-07-20T10:00:00.123Z", "2026-07-20T10:00:00+00:00"}
	for _, s := range good {
		if _, ok := parseEventTime(s); !ok {
			t.Errorf("should parse %q", s)
		}
	}
	for _, s := range []string{"", "not-a-time", "1721469600"} {
		if _, ok := parseEventTime(s); ok {
			t.Errorf("should NOT parse %q", s)
		}
	}
}
