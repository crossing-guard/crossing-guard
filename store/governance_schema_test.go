package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGovernanceSchema drives the real GovTx API (not inline SQL): schema applies,
// is idempotent across a daemon-restart re-open, and one atomic observation
// round-trips. Phase-0/1a verification.
func TestGovernanceSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := ix.db.Exec(schema); err != nil { // re-run (second daemon start) must be clean
		t.Fatalf("schema not idempotent: %v", err)
	}

	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	fileID := "file:/repo/.env"
	if err := tx.UpsertEntity(fileID, "file", "/repo/.env", 100); err != nil {
		t.Fatal(err)
	}
	if err := tx.UpsertEntityState(fileID, StateRow{Key: "data-class", Value: "secrets",
		Detector: "area.secrets", Provenance: "observed", Evidence: "path=.env"}, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 100, SessionID: "claude/s1", Verb: "read", Tool: "Read",
		TargetEntityID: fileID, Tags: `[{"key":"data-class","value":"secrets"}]`,
		Decision: "deny", Reason: "READ on secrets file", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var verb, target, origin string
	if err := ix.db.QueryRow(
		`SELECT verb, target_entity_id, origin FROM event WHERE session_id=?`, "claude/s1").
		Scan(&verb, &target, &origin); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if verb != "read" || target != fileID || origin != "live" {
		t.Fatalf("event round-trip mismatch: verb=%q target=%q origin=%q", verb, target, origin)
	}
	es, err := ix.EntityState(fileID)
	if err != nil || len(es) != 1 || es[0].Value != "secrets" {
		t.Fatalf("entity state: %v %v", es, err)
	}

	ix.Close()
	ix2, err := Open(path) // daemon restart
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ix2.Close()
	var n int
	if err := ix2.db.QueryRow(`SELECT count(*) FROM event`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("event not durable across reopen: n=%d err=%v", n, err)
	}
}

func TestOneEventCanOwnManyExactResources(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(EventRecord{TS: 10, SessionID: "multi", Runtime: "codex", Verb: "write", Tool: "apply_patch", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{"/repo/a.go", "/repo/b.go"} {
		id := "file:" + path
		if err := tx.UpsertEntity(id, "file", path, 10); err != nil {
			t.Fatal(err)
		}
		if err := tx.AppendEventResource(eventID, i, id, "tool_input.command"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var events, resources int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM event WHERE session_id='multi'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM event_resource WHERE event_id=?`, eventID).Scan(&resources); err != nil {
		t.Fatal(err)
	}
	if events != 1 || resources != 2 {
		t.Fatalf("actions=%d resources=%d, want 1/2", events, resources)
	}
	touches, err := ix.SessionFileTouches("multi", 10)
	if err != nil {
		t.Fatal(err)
	}
	if touches.EventTotal != 1 || touches.DistinctFiles != 2 || touches.NonFileEvents != 0 || len(touches.Touches) != 2 {
		t.Fatalf("touch projection = %+v", touches)
	}
	eventsOut, err := ix.EventsForSession("multi", 10)
	if err != nil || len(eventsOut) != 1 || eventsOut[0].ID != eventID {
		t.Fatalf("returned event identity = %+v err=%v", eventsOut, err)
	}
	resourceOut, resourceTotal, err := ix.EventResourcesForSession("multi", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if resourceTotal != 2 || len(resourceOut) != 2 || resourceOut[0].EventID != eventID || resourceOut[0].Ordinal != 0 || resourceOut[0].Identity != "/repo/a.go" || resourceOut[1].Ordinal != 1 || resourceOut[1].Identity != "/repo/b.go" {
		t.Fatalf("ordered event resources = %+v", resourceOut)
	}
}

func TestEventResourceRollbackAndLegacyFallback(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, _ := ix.BeginGov()
	eventID, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "rolled", Verb: "write", Tool: "apply_patch", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.UpsertEntity("file:/repo/rolled", "file", "/repo/rolled", 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventResource(eventID, 0, "file:/repo/rolled", "tool_input.command"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM event WHERE session_id='rolled'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback left %d events: %v", count, err)
	}

	tx, _ = ix.BeginGov()
	if err := tx.UpsertEntity("file:/repo/legacy", "file", "/repo/legacy", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 2, SessionID: "legacy", Verb: "write", Tool: "Write", TargetEntityID: "file:/repo/legacy", Origin: "imported"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	touches, err := ix.SessionFileTouches("legacy", 10)
	if err != nil || touches.DistinctFiles != 1 || len(touches.Touches) != 1 {
		t.Fatalf("legacy touches=%+v err=%v", touches, err)
	}
}

// TestFoldOrderIndependent pins C2: folding the SAME fact in two different arrival
// orders must yield identical (first_seen,last_seen). MIN/MAX makes the fold
// order-independent, which is what lets live-accumulated state equal a ts-ordered
// replay (model doc R2). Before the fix, last_seen took the last-arriving ts and
// could move backward.
func TestFoldOrderIndependent(t *testing.T) {
	fold := func(order [][2]int64) StateRow { // each pair = (ts) folded in sequence
		ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer ix.Close()
		for _, o := range order {
			tx, _ := ix.BeginGov()
			if err := tx.UpsertEntityState("e1", StateRow{Key: "k", Value: "v", Detector: "d",
				Provenance: "observed", Evidence: "x"}, o[0]); err != nil {
				t.Fatal(err)
			}
			tx.Commit()
		}
		rows, _ := ix.EntityState("e1")
		if len(rows) != 1 {
			t.Fatalf("expected one folded row, got %d", len(rows))
		}
		return rows[0]
	}
	ascending := fold([][2]int64{{90}, {100}, {110}})
	descending := fold([][2]int64{{110}, {100}, {90}})
	if ascending.FirstSeen != descending.FirstSeen || ascending.LastSeen != descending.LastSeen {
		t.Fatalf("fold is order-sensitive: asc(%d,%d) != desc(%d,%d)",
			ascending.FirstSeen, ascending.LastSeen, descending.FirstSeen, descending.LastSeen)
	}
	if ascending.FirstSeen != 90 || ascending.LastSeen != 110 {
		t.Fatalf("window wrong: want (90,110), got (%d,%d)", ascending.FirstSeen, ascending.LastSeen)
	}
}

// TestGovernanceSchemaOnRealIndexCopy — migration applies to a real populated index
// without disturbing existing tables. Point CG_REAL_INDEX_COPY at a COPY.
func TestGovernanceSchemaOnRealIndexCopy(t *testing.T) {
	src := os.Getenv("CG_REAL_INDEX_COPY")
	if src == "" {
		t.Skip("set CG_REAL_INDEX_COPY to a copy of a real index to verify migration on real data")
	}
	ix, err := Open(src)
	if err != nil {
		t.Fatalf("migrate real index: %v", err)
	}
	defer ix.Close()
	var sessions, events int
	if err := ix.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("existing sessions table disturbed: %v", err)
	}
	if err := ix.db.QueryRow(`SELECT count(*) FROM event`).Scan(&events); err != nil {
		t.Fatalf("new event table missing after migrate: %v", err)
	}
	t.Logf("real-index migrate OK: sessions=%d preserved, event rows=%d", sessions, events)
}

// TestMigrateRenamesLegacyProvenanceColumn pins the upgrade path. An index created
// by an older build has event.provenance; the current binary writes event.origin.
// Without migration, CREATE TABLE IF NOT EXISTS leaves the old column in place and
// EVERY observation fails at runtime while the daemon reports healthy — which is
// exactly what happened on the first real deploy.
func TestMigrateRenamesLegacyProvenanceColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// simulate the OLD shape: drop the new table, recreate it the legacy way
	if _, err := ix.db.Exec(`DROP TABLE event;
		CREATE TABLE event(id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
		  session_id TEXT NOT NULL, verb TEXT, tool TEXT, target_entity_id TEXT,
		  tags TEXT, decision TEXT, reason TEXT, provenance TEXT)`); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	ix2, err := Open(path) // reopening must migrate it
	if err != nil {
		t.Fatalf("open on a legacy index must migrate, got: %v", err)
	}
	defer ix2.Close()
	cols, err := columnSet(ix2.db, "event")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["origin"] || cols["provenance"] {
		t.Fatalf("event column not migrated to origin: %v", cols)
	}
	// and a real write must now succeed through the shipped API
	tx, err := ix2.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "s", Verb: "read", Origin: "live"}, nil); err != nil {
		t.Fatalf("append after migration failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateAddsRuntimeToAnExistingLog pins the v1→v2 upgrade. The event log is
// PRIMARY TRUTH and is never rebuilt, so adding "which runtime did this" has to
// land on a database full of rows that predate the question. Those rows must
// survive and read back as UNKNOWN — an empty runtime — because there is no
// honest way to infer the agent behind an event recorded before the hook carried
// it, and a plausible back-fill would be a guess written into the only record
// that a blocked action ever happened.
func TestMigrateAddsRuntimeToAnExistingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a v1 store: the event table as it was before event.runtime, holding
	// one row from before the upgrade.
	if _, err := ix.db.Exec(`DROP TABLE event;
		CREATE TABLE event(id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
		  session_id TEXT NOT NULL, verb TEXT, tool TEXT, target_entity_id TEXT,
		  tags TEXT, decision TEXT, reason TEXT, origin TEXT);
		INSERT INTO event(ts,session_id,verb,tool,target_entity_id,tags,decision,reason,origin)
		  VALUES(1,'old-session','exec','Bash','','[]','deny','canary','live');
		PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	ix2, err := Open(path)
	if err != nil {
		t.Fatalf("opening a v1 store must migrate it, got: %v", err)
	}
	defer ix2.Close()

	cols, err := columnSet(ix2.db, "event")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["runtime"] {
		t.Fatalf("event.runtime missing after migrate: %v", cols)
	}
	// The pre-existing row survived, and its runtime is UNKNOWN rather than guessed.
	old, err := ix2.EventsForSession("old-session", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 {
		t.Fatalf("pre-upgrade events = %d, want 1 — PRIMARY TRUTH must survive a migration", len(old))
	}
	if old[0].Runtime != "" {
		t.Errorf("runtime = %q, want empty: an event from before the field must read as UNKNOWN, never back-filled", old[0].Runtime)
	}
	if old[0].Decision != "deny" {
		t.Errorf("decision = %q, want \"deny\" — the row's other columns must be untouched", old[0].Decision)
	}
	// And a new write round-trips the runtime.
	tx, err := ix2.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 2, SessionID: "new-session", Runtime: "claude",
		Verb: "exec", Tool: "Bash", Decision: "allow", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := ix2.EventsForSession("new-session", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Runtime != "claude" {
		t.Fatalf("new event runtime = %+v, want claude", got)
	}
}

// TestMigrateRepairsANullableRuntimeColumn covers the repair branch the first
// review left untested: a store whose runtime column was added NULLABLE (by an
// intermediate build, or by the shipped DDL before it was fixed to match the
// migration) holds NULL runtimes that fail every string scan. Open() must
// normalise them to ” — and must do it without rewriting stores that need no
// repair (the probe-first discipline; asserted here only by the repair working).
func TestMigrateRepairsANullableRuntimeColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nullable.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the intermediate layout: nullable runtime, one NULL row in it.
	if _, err := ix.db.Exec(`DROP TABLE event;
		CREATE TABLE event(id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
		  session_id TEXT NOT NULL, runtime TEXT, verb TEXT, tool TEXT,
		  target_entity_id TEXT, tags TEXT, decision TEXT, reason TEXT, origin TEXT);
		INSERT INTO event(ts,session_id,runtime,verb,tool,target_entity_id,tags,decision,reason,origin)
		  VALUES(1,'s',NULL,'exec','Bash','','[]','deny','r','live')`); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	ix2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix2.Close()
	got, err := ix2.EventsForSession("s", 10)
	if err != nil {
		t.Fatalf("reading a repaired store must not error: %v", err)
	}
	if len(got) != 1 || got[0].Runtime != "" {
		t.Fatalf("NULL runtime must read back as empty after repair, got %+v", got)
	}
}
