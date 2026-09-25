package store

import (
	"path/filepath"
	"testing"
)

// TestPruneTrimsOldEventsButKeepsState pins D10/R8: prune deletes events older than a
// cutoff and reports the count, while leaving the folded state (the CURRENT belief)
// intact — retention trades the ability to replay old history, not today's state.
func TestPruneTrimsOldEventsButKeepsState(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	// Three events across time; the state fold lands on the entity/session.
	for _, ev := range []struct {
		ts   int64
		path string
	}{
		{100, "/repo/old.md"},
		{200, "/repo/mid.md"},
		{300, "/repo/new.md"},
	} {
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.AppendEvent(EventRecord{TS: ev.ts, SessionID: "s", Verb: "read",
			Tool: "Read", TargetEntityID: "file:" + ev.path, Origin: "live"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := tx.UpsertSessionState("s", StateRow{Key: "area", Value: "docs",
			Detector: "area.docs", Provenance: "observed"}, ev.ts); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	// Dry-run count: everything strictly before ts=250 is the two oldest.
	n, err := ix.CountEventsBefore(250)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("CountEventsBefore(250) = %d, want 2", n)
	}

	deleted, err := ix.PruneEventsBefore(250)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("pruned %d, want 2", deleted)
	}

	// Only the newest event survives.
	stat, err := ix.EventLogStat()
	if err != nil {
		t.Fatal(err)
	}
	if stat.Total != 1 || stat.LastEventTS != 300 {
		t.Fatalf("after prune: %+v, want 1 event at ts=300", stat)
	}

	// The folded session state is UNTOUCHED — pruning events is not pruning belief.
	state, err := ix.SessionState("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(state) == 0 {
		t.Fatal("prune deleted folded state; it must only delete raw events")
	}
}

// TestPruneNoOpWhenNothingOldEnough: a cutoff before every event deletes nothing.
func TestPruneNoOpWhenNothingOldEnough(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, _ := ix.BeginGov()
	_, _ = tx.AppendEvent(EventRecord{TS: 500, SessionID: "s", Verb: "read", Tool: "Read", Origin: "live"}, nil)
	_ = tx.Commit()

	n, err := ix.CountEventsBefore(100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("nothing is older than the cutoff, got count %d", n)
	}
	deleted, err := ix.PruneEventsBefore(100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("deleted %d with nothing old enough", deleted)
	}
}
