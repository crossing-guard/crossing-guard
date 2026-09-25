package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

func TestChangeAppendAtomicAndForeignKeys(t *testing.T) {
	ix, e := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer ix.Close()
	r := &ChangeRecord{SessionID: "s", RepositoryID: "r", CheckoutID: "c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo", Kind: "declaration", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "p", SourceDisplay: "p", SourceDigest: "sha256-v1:x", RecordedAt: 1, IntentLabel: "x", Items: []ChangeItem{{Path: "a.go"}}}
	if e := ix.AppendChange(r); e != nil {
		t.Fatal(e)
	}
	recs, total, e := ix.ChangeRecordsForSession("s", 10)
	if e != nil || total != 1 || len(recs) != 1 || len(recs[0].Items) != 1 {
		t.Fatalf("roundtrip: total=%d recs=%+v err=%v", total, recs, e)
	}
	if _, e := ix.db.Exec(`INSERT INTO change_item(record_id,ordinal,path) VALUES(999,0,'x')`); e == nil {
		t.Fatal("foreign key was not enforced")
	}
}

func TestV3MigrationFailureRollsBackStampAndDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ix.db.Exec(`DROP TRIGGER change_item_kind_insert; DROP TABLE change_item; DROP TABLE change_record; PRAGMA user_version=2`); e != nil {
		t.Fatal(e)
	}
	ix.Close()
	migrationTestHook = func(db schemaDB) error {
		_, _ = db.Exec(`CREATE TABLE should_rollback(x)`)
		return errors.New("injected migration failure")
	}
	if _, e = Open(path); e == nil {
		t.Fatal("injected migration succeeded")
	}
	migrationTestHook = nil
	raw, e := driver.Open("file:"+path, fts5.Register)
	if e != nil {
		t.Fatal(e)
	}
	defer raw.Close()
	var v int
	raw.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 2 {
		t.Fatalf("failed migration stamped v%d", v)
	}
	var n int
	raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='should_rollback'`).Scan(&n)
	if n != 0 {
		t.Fatal("failed migration DDL was not rolled back")
	}
}

func TestChangeAppendValidationAndDuplicateRollback(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	base := ChangeRecord{SessionID: "s", RepositoryID: "r", CheckoutID: "c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo", Kind: "declaration", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "p", SourceDisplay: "p", SourceDigest: "sha256-v1:x", RecordedAt: 1, IntentLabel: "x"}
	tooMany := base
	tooMany.Items = make([]ChangeItem, 10001)
	if err := ix.AppendChange(&tooMany); err == nil {
		t.Fatal("over-limit record accepted")
	}
	duplicate := base
	duplicate.Items = []ChangeItem{{Path: "a.go"}, {Path: "a.go"}}
	if err := ix.AppendChange(&duplicate); err == nil {
		t.Fatal("duplicate item transaction succeeded")
	}
	if duplicate.ID != 0 {
		t.Fatalf("failed append published uncommitted record id %d", duplicate.ID)
	}
	_, total, err := ix.ChangeRecordsForSession("s", 10)
	if err != nil || total != 0 {
		t.Fatalf("failed append left partial record: total=%d err=%v", total, err)
	}
	wrongClass := base
	wrongClass.Kind = "revision"
	wrongClass.RecordedAt = 2
	if err := ix.AppendChange(&wrongClass); err == nil {
		t.Fatal("claimed caller created observed revision tuple")
	}
}

func TestChangeEvidenceSurvivesExportAndForwardVersionIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &ChangeRecord{SessionID: "exported", RepositoryID: "r", CheckoutID: "c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo", Kind: "implementation", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "claim.md", SourceDisplay: "claim.md", SourceDigest: "sha256-v1:x", RecordedAt: 1, Items: []ChangeItem{{Path: "x.go"}}}
	if err := ix.AppendChange(r); err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(dir, "export.sqlite")
	if err := ix.Export(exported); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	copyIndex, err := OpenRO(exported)
	if err != nil {
		t.Fatal(err)
	}
	recs, total, err := copyIndex.ChangeRecordsForSession("exported", 10)
	copyIndex.Close()
	if err != nil || total != 1 || len(recs) != 1 || len(recs[0].Items) != 1 {
		t.Fatalf("export lost evidence: total=%d recs=%+v err=%v", total, recs, err)
	}

	raw, err := driver.Open("file:"+path, fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=99`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("writable open accepted future schema")
	}
	if _, err := OpenRO(path); err == nil {
		t.Fatal("read-only open accepted future schema")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatal("future-schema refusal mutated store size")
	}
	raw, err = driver.Open("file:"+path+"?mode=ro", fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 99 {
		t.Fatalf("future schema stamp changed: %d %v", version, err)
	}
}

func TestV2UpgradePreservesExistingEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "old", Verb: "read", Tool: "fixture", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP TRIGGER change_item_kind_insert; DROP TABLE change_item; DROP TABLE change_record; PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	events, err := ix.EventsForSession("old", 10)
	if err != nil || len(events) != 1 || events[0].Verb != "read" {
		t.Fatalf("v2 event lost: %+v %v", events, err)
	}
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("upgrade stamp=%d err=%v", version, err)
	}
}

func TestChangeHistoryHasExactTotalBeyondResponseLimit(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO change_record(session_id,repository_id,checkout_id,repository_identity_kind,checkout_root,kind,evidence_class,source_kind,source_ref,source_display,source_digest,recorded_at) VALUES('history','r','c','local-sha256','/repo','implementation','claimed','file','claim','claim','sha256-v1:x',?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1001; i++ {
		if _, err := stmt.Exec(i + 1); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	recs, total, err := ix.ChangeRecordsForSession("history", 1000)
	if err != nil || len(recs) != 1000 || total != 1001 {
		t.Fatalf("history cap hid total: rows=%d total=%d err=%v", len(recs), total, err)
	}
}
