package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

func downgradeWorkspaceSchemaToV25(t *testing.T, ix *Index) {
	t.Helper()
	if _, err := ix.db.Exec(`
ALTER TABLE runtime_task DROP COLUMN workspace_selection_version;
ALTER TABLE runtime_task DROP COLUMN workspace_selection_id;
DROP TABLE workspace_mutation;
DROP TABLE workspace_selection_lease;
DROP TABLE workspace_binding;
DROP TABLE workspace_selection;
DROP TABLE workspace_worktree;
PRAGMA user_version=25;`); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceV26MigrationPreservesPrimaryFactsAndIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "preserved", Verb: "read", Tool: "Read", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	downgradeWorkspaceSchemaToV25(t, ix)
	ix.Close()

	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var events int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM event WHERE session_id='preserved'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("preserved events=%d err=%v", events, err)
	}
	for _, table := range []string{"workspace_worktree", "workspace_selection", "workspace_binding", "workspace_selection_lease", "workspace_mutation"} {
		var found int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found); err != nil || found != 1 {
			t.Fatalf("table %s found=%d err=%v", table, found, err)
		}
	}
	var check string
	if err := ix.db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check=%q err=%v", check, err)
	}
	var violations int
	rows, err := ix.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		violations++
	}
	rows.Close()
	if violations != 0 {
		t.Fatalf("foreign key violations=%d", violations)
	}
}

func TestWorkspaceV26MigrationFailureRollsBackDDLAndStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	downgradeWorkspaceSchemaToV25(t, ix)
	ix.Close()
	migrationTestHook = func(db schemaDB) error {
		_, _ = db.Exec(`CREATE TABLE workspace_v26_should_rollback(x)`)
		return errors.New("injected workspace migration failure")
	}
	t.Cleanup(func() { migrationTestHook = nil })
	if _, err := Open(path); err == nil {
		t.Fatal("injected workspace migration succeeded")
	}
	migrationTestHook = nil
	raw, err := driver.Open("file:"+path, fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 25 {
		t.Fatalf("failed migration stamped version=%d err=%v", version, err)
	}
	var workspaceTables, injected int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'workspace_%'`).Scan(&workspaceTables); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='workspace_v26_should_rollback'`).Scan(&injected); err != nil {
		t.Fatal(err)
	}
	if workspaceTables != 0 || injected != 0 {
		t.Fatalf("failed migration retained workspace tables=%d injected=%d", workspaceTables, injected)
	}
}
