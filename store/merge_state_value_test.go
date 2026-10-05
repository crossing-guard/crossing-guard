package store

import (
	"path/filepath"
	"testing"
)

// MergeStateValue repairs folds written under a legacy spelling
// (data-class-personal-ladder-plan.md D-3): both fold tables, the fold's own MIN/MAX
// merge into an existing row, idempotent, and no write when there is nothing to repair.
func TestMergeStateValueRepairsBothFolds(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	legacy := func() StateRow {
		return StateRow{Key: "data-class", Value: "personal", Detector: "data.email", Provenance: "observed", Evidence: "old"}
	}
	for _, step := range []func() error{
		// s1 has only the legacy row → it moves.
		func() error { return tx.UpsertSessionState("s1", legacy(), 10) },
		// s2 has both spellings → they merge to MIN(first)/MAX(last).
		func() error { return tx.UpsertSessionState("s2", legacy(), 5) },
		func() error {
			return tx.UpsertSessionState("s2", StateRow{Key: "data-class", Value: "personal-data", Detector: "data.email", Provenance: "observed", Evidence: "new"}, 20)
		},
		// an unrelated key with the same value is left alone.
		func() error {
			return tx.UpsertSessionState("s1", StateRow{Key: "role", Value: "personal", Detector: "x", Provenance: "observed"}, 10)
		},
		func() error { return tx.UpsertEntity("file:/a", "file", "/a", 7) },
		func() error { return tx.UpsertEntityState("file:/a", legacy(), 7) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	moved, err := ix.MergeStateValue("data-class", "personal", "personal-data")
	if err != nil || moved != 3 {
		t.Fatalf("moved=%d err=%v, want the 3 legacy rows", moved, err)
	}
	value := func(rows []StateRow, key string) []StateRow {
		var out []StateRow
		for _, r := range rows {
			if r.Key == key {
				out = append(out, r)
			}
		}
		return out
	}
	s1, _ := ix.SessionState("s1")
	if got := value(s1, "data-class"); len(got) != 1 || got[0].Value != "personal-data" || got[0].FirstSeen != 10 {
		t.Fatalf("s1 fold: %+v", got)
	}
	if got := value(s1, "role"); len(got) != 1 || got[0].Value != "personal" {
		t.Fatalf("another key must be untouched: %+v", got)
	}
	s2, _ := ix.SessionState("s2")
	if got := value(s2, "data-class"); len(got) != 1 || got[0].Value != "personal-data" || got[0].FirstSeen != 5 || got[0].LastSeen != 20 {
		t.Fatalf("s2 must merge to one row spanning 5..20: %+v", got)
	}
	ent, _ := ix.EntityState("file:/a")
	if len(ent) != 1 || ent[0].Value != "personal-data" {
		t.Fatalf("entity fold: %+v", ent)
	}

	if moved, err := ix.MergeStateValue("data-class", "personal", "personal-data"); err != nil || moved != 0 {
		t.Fatalf("second run must be a no-op: moved=%d err=%v", moved, err)
	}
	if _, err := ix.MergeStateValue("data-class", "same", "same"); err == nil {
		t.Fatal("from == to is refused")
	}
}

// With nothing to repair, the merge must not take the write lock: a start-up that finds
// another writer holding it answers 0 at once instead of waiting out busy_timeout.
func TestMergeStateValueTakesNoWriteLockWhenClean(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	holder, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if moved, err := ix.MergeStateValue("data-class", "personal", "personal-data"); err != nil || moved != 0 {
		t.Fatalf("clean store must be a lock-free no-op: moved=%d err=%v", moved, err)
	}
}
