package store

import (
	"path/filepath"
	"testing"
)

func TestRuntimeEventCursorUsesExactLiveRuntimeBoundary(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []EventRecord{
		{TS: 1, SessionID: "old", Runtime: "cursor", Tool: "Read", Decision: "allow", Origin: "live", Tags: `[]`},
		{TS: 2, SessionID: "other", Runtime: "codex", Tool: "Read", Decision: "allow", Origin: "live", Tags: `[]`},
		{TS: 3, SessionID: "imported", Runtime: "cursor", Tool: "Read", Decision: "allow", Origin: "imported", Tags: `[]`},
	}
	for _, event := range fixtures {
		if _, err := tx.AppendEvent(event, nil); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cursor, err := ix.RuntimeEventCursor("cursor")
	if err != nil || cursor != 1 {
		t.Fatalf("cursor=%d err=%v, want live Cursor id 1", cursor, err)
	}

	tx, err = ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	newID, err := tx.AppendEvent(EventRecord{TS: 4, SessionID: "new", Runtime: "cursor",
		Verb: "exec", Tool: "Shell", Decision: "deny", Reason: "canary", Origin: "live",
		Tags: `[{"key":"command","value":"synthetic-marker"}]`}, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	events, err := ix.RuntimeEventsAfter("cursor", cursor, 25)
	if err != nil || len(events) != 1 || events[0].ID != newID || events[0].SessionID != "new" || events[0].Decision != "deny" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if events[0].Tags == "" {
		t.Fatal("server-side tags needed for exact canary recognition are missing")
	}
}
