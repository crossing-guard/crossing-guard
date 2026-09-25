package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaVersionStampedOnOpen pins the D12 forward-compat guard: a fresh store is
// stamped with this binary's SchemaVersion, so a later binary can tell how old it is.
func TestSchemaVersionStampedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var v int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion {
		t.Fatalf("fresh store stamped v%d, want v%d", v, SchemaVersion)
	}
}

// TestNewerStoreIsRefusedNotCorrupted is the heart of D12: an OLDER binary opening a
// database written by a NEWER one must REFUSE, not silently write against a layout it
// does not understand. Simulated by stamping the store one version ahead.
func TestNewerStoreIsRefusedNotCorrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Stamp it as if a future binary wrote it, then close and reopen with THIS binary.
	if _, err := ix.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("opening a newer-schema store must fail, not silently proceed")
	}
	// The message must name the mismatch so a human knows to upgrade.
	if !strings.Contains(err.Error(), "newer") && !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("refusal does not explain the version mismatch: %v", err)
	}
}

// TestExportIsAConsistentStore pins D15: the export is itself a valid, self-contained
// store carrying the same rows, and refuses to overwrite an existing file.
func TestExportIsAConsistentStore(t *testing.T) {
	dir := t.TempDir()
	ix, err := Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// Write one governance event so the export has something to prove it carried.
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 100, SessionID: "s", Verb: "read", Tool: "Read", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "backup", "snap.sqlite")
	if err := ix.Export(dest); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	// The export must have NO -wal/-shm siblings — it is one self-contained file.
	if _, err := os.Stat(dest + "-wal"); !os.IsNotExist(err) {
		t.Error("export left a -wal sibling; it should be self-contained")
	}
	// Reopen the snapshot as a store and confirm the event is there.
	snap, err := Open(dest)
	if err != nil {
		t.Fatalf("export is not a valid store: %v", err)
	}
	defer snap.Close()
	stat, err := snap.EventLogStat()
	if err != nil {
		t.Fatal(err)
	}
	if stat.Total != 1 || stat.LastEventTS != 100 {
		t.Fatalf("export did not carry the rows: %+v", stat)
	}

	// A second export to the same path must REFUSE rather than clobber a backup.
	snap2, _ := Open(filepath.Join(dir, "index.sqlite"))
	defer snap2.Close()
	if err := snap2.Export(dest); err == nil {
		t.Error("export overwrote an existing backup; it must refuse")
	}
}

// TestNewerStoreIsNotMutatedByRefusal pins the red-team HIGH: refusing a newer-schema
// store must happen BEFORE any DDL/migration writes, so the file the guard swore not to
// corrupt is byte-identical after the failed Open.
func TestNewerStoreIsNotMutatedByRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("opening a newer-schema store must fail")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("refused-open MUTATED the store: %d bytes → %d bytes", len(before), len(after))
	}
}
