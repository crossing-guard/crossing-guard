package store

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// storeLayout is everything a store's schema is: each object's kind, name, table and
// SQL, and the version stamp.
func storeLayout(t *testing.T, ix *Index) ([]string, int) {
	t.Helper()
	rows, err := ix.db.Query(`SELECT type || '|' || name || '|' || tbl_name || '|' || COALESCE(sql,'') FROM sqlite_master`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	layout := []string{}
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			t.Fatal(err)
		}
		layout = append(layout, object)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(layout)
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return layout, version
}

// perStoreTables are the tables whose rows are one store's own and so differ between
// any two stores: the device identity, minted at open.
var perStoreTables = map[string]bool{"sync_device": true}

// storeRows is every row of every table except perStoreTables, as table → sorted rows.
// A table with no rows is present with an empty list, so a table missing on one side
// shows.
func storeRows(t *testing.T, ix *Index) map[string][]string {
	t.Helper()
	names, err := ix.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	names.Close()
	all := map[string][]string{}
	for _, table := range tables {
		if perStoreTables[table] {
			continue
		}
		all[table] = tableRows(t, ix, table)
	}
	return all
}

// tableRows is one table's rows, each rendered whole, sorted.
func tableRows(t *testing.T, ix *Index, table string) []string {
	t.Helper()
	rows, err := ix.db.Query(`SELECT * FROM "` + table + `"`)
	if err != nil {
		// A virtual table's shadow may refuse a plain read; nothing here does today.
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for rows.Next() {
		values := make([]any, len(cols))
		into := make([]any, len(cols))
		for i := range values {
			into[i] = &values[i]
		}
		if err := rows.Scan(into...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%q", values))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// Two seeded stores are two devices: each open mints its own device id over its copy,
// and a reopened store keeps the one it has.
func TestSeededFreshStoresAreDifferentDevices(t *testing.T) {
	stop, err := SeedFreshStores()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	dir := t.TempDir()
	deviceOf := func(name string) string {
		ix, err := Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer ix.Close()
		var id string
		var devices int
		if err := ix.db.QueryRow(`SELECT count(*), COALESCE(min(id),'') FROM sync_device`).Scan(&devices, &id); err != nil {
			t.Fatal(err)
		}
		if devices != 1 || id == "" {
			t.Fatalf("%s: %d device rows, id %q", name, devices, id)
		}
		return id
	}
	first, second := deviceOf("first.sqlite"), deviceOf("second.sqlite")
	if first == second {
		t.Fatalf("two seeded stores are the same device: %s", first)
	}
	if again := deviceOf("first.sqlite"); again != first {
		t.Fatalf("a reopened seeded store changed device: %s then %s", first, again)
	}
}

// A seeded test binary's new store is the ladder's own output: the same objects, the
// same SQL, the same version, in WAL, writable, and empty. A path that already holds a
// file is opened as it is, never replaced. stop puts the ordinary open back.
func TestSeededFreshStoreIsTheLaddersStore(t *testing.T) {
	dir := t.TempDir()
	laddered, err := Open(filepath.Join(dir, "laddered.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer laddered.Close()
	wantLayout, wantVersion := storeLayout(t, laddered)
	if wantVersion != SchemaVersion || len(wantLayout) == 0 {
		t.Fatalf("the laddered store: version %d, %d objects", wantVersion, len(wantLayout))
	}

	stop, err := SeedFreshStores()
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	if freshStoreSeed.Load() == nil {
		t.Fatal("the seed is not in force")
	}
	seededPath := filepath.Join(dir, "nested", "seeded.sqlite")
	seeded, err := Open(seededPath)
	if err != nil {
		t.Fatal(err)
	}
	defer seeded.Close()
	gotLayout, gotVersion := storeLayout(t, seeded)
	if gotVersion != wantVersion || !reflect.DeepEqual(gotLayout, wantLayout) {
		t.Fatalf("a seeded store's layout differs from the ladder's: version %d want %d; %d objects want %d", gotVersion, wantVersion, len(gotLayout), len(wantLayout))
	}
	if got, want := storeRows(t, seeded), storeRows(t, laddered); !reflect.DeepEqual(got, want) {
		t.Fatalf("a seeded store's rows differ from the ladder's:\n seeded   %v\n laddered %v", got, want)
	}
	var mode string
	var events int
	if err := seeded.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("a seeded store is in WAL: %q %v", mode, err)
	}
	if err := seeded.db.QueryRow(`SELECT count(*) FROM event`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("a seeded store is empty: %d %v", events, err)
	}
	tx, err := seeded.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "s", Runtime: "codex", Verb: "read", Tool: "Read", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, "nested", ".store-seed-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("the staged copy is removed: %v %v", leftovers, err)
	}
	if info, err := os.Stat(seededPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("a seeded store is owner-only: %v %v", info, err)
	}

	// The written event stayed in its own store: a second new store is empty, and
	// reopening the first finds its row — an existing path is opened, not re-seeded.
	if err := seeded.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(seededPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.db.QueryRow(`SELECT count(*) FROM event`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("an existing store is opened as it is: %d events %v", events, err)
	}
	other, err := Open(filepath.Join(dir, "other.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.db.QueryRow(`SELECT count(*) FROM event`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("another new store shares nothing with the first: %d events %v", events, err)
	}

	// A file that is not a store is not replaced by the seed: the open says so.
	foreign := filepath.Join(dir, "foreign.sqlite")
	if err := os.WriteFile(foreign, []byte("not a database, and long enough to be read as a header"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ix, err := Open(foreign); err == nil {
		ix.Close()
		t.Fatal("a file that is not a store was replaced by the seed")
	}

	stop()
	stopped = true
	if freshStoreSeed.Load() != nil {
		t.Fatal("stop leaves the seed in force")
	}
}
