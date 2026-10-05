package store

import (
	"path/filepath"
	"testing"
)

func TestSchemaV46AddsRouteColumnsAndHandoffTablesOnAFreshStore(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for _, table := range []string{"orchestration_managed_binding", "orchestration_review_binding"} {
		cols, err := columnSet(ix.db, table)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"route_id", "route_revision_digest", "route_problem", "adoption_key"} {
			if !cols[name] {
				t.Errorf("%s lacks %s", table, name)
			}
		}
	}
	for _, table := range []string{"handoff", "team_member", "handoff_open", "handoff_runtime_readiness"} {
		cols, err := columnSet(ix.db, table)
		if err != nil || len(cols) == 0 {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	if err := migrateTeamRestOfReleaseV46(ix.db); err != nil {
		t.Fatalf("the step must be idempotent: %v", err)
	}
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 46 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
}
