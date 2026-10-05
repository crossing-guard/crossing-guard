package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var pathReconciliationIndexes = []string{"path_reconciliation_current", "path_reconciliation_session"}

func requirePathReconciliationIndexes(t *testing.T, ix *Index, when string) {
	t.Helper()
	for _, name := range pathReconciliationIndexes {
		var table string
		err := ix.db.QueryRow(`SELECT tbl_name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&table)
		if err != nil || table != "path_reconciliation" {
			t.Fatalf("%s: index %s on table %q err=%v", when, name, table, err)
		}
	}
}

// requireQueryPlanSearches fails on any full scan of path_reconciliation: the
// supersede lookup ran once per path under the daemon's write lock, and a SCAN
// of the installed table held that lock for minutes (observe-hook-latency plan
// §2.4, §15.1).
func requireQueryPlanSearches(t *testing.T, ix *Index, query, index string, args ...any) {
	t.Helper()
	requireQueryPlanUses(t, ix, query, index, args...)
	rows, err := ix.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(detail, "SCAN path_reconciliation") {
			t.Fatalf("query plan scans path_reconciliation: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestPathReconciliationLookupsUseIndexes(t *testing.T) {
	ix := openResultTestIndex(t)
	requirePathReconciliationIndexes(t, ix, "fresh store")
	requireQueryPlanSearches(t, ix, pathReconciliationSupersedeLookup, "path_reconciliation_current", 1, "a.go", "v1")
	requireQueryPlanSearches(t, ix, `SELECT id FROM path_reconciliation
		WHERE session_id=? ORDER BY reconciled_at,id LIMIT ?`, "path_reconciliation_session", "s", 10)
	requireQueryPlanSearches(t, ix, `SELECT COUNT(*) FROM path_reconciliation WHERE session_id=?`,
		"path_reconciliation_session", "s")
	requireQueryPlanSearches(t, ix, `SELECT id FROM path_reconciliation
		WHERE session_id=? AND (path IN (?) OR old_path IN (?)) ORDER BY reconciled_at,id LIMIT ?`,
		"path_reconciliation_session", "s", "a.go", "a.go", 10)
}

// A characterization test: it passes with and without the indexes, and pins the
// append behaviour the indexed lookup must keep.
func TestPathReconciliationSupersedeChainIsUnchangedByIndexes(t *testing.T) {
	ix := openResultTestIndex(t)
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "s",
		ScopeKey: "checkout:c", Kind: "settled", RequestID: "reconcile-fixture",
		WorkingDirectory: "/repo", Status: "pending", BoundaryClass: "settled", RequestedAt: 1})
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	fact := PathReconciliation{SessionID: "s", CheckoutID: "c", CurrentCheckpointID: checkpoint.ID,
		Path: "a.go", Classification: "unattributed", GitEffectDigest: "sha256-v1:one",
		Algorithm: "v1", ReconciledAt: 10}
	for _, step := range []PathReconciliation{fact, fact} {
		if err := ix.AppendPathReconciliation(step); err != nil {
			t.Fatal(err)
		}
	}
	other := fact
	other.Path = "b.go"
	if err := ix.AppendPathReconciliation(other); err != nil {
		t.Fatal(err)
	}
	changed := fact
	changed.Classification, changed.ReconciledAt = "session-associated", 11
	if err := ix.AppendPathReconciliation(changed); err != nil {
		t.Fatal(err)
	}
	facts, err := ix.PathReconciliationsForSession("s", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("an unchanged fact must not be appended twice: %+v", facts)
	}
	if facts[0].Path != "a.go" || facts[1].Path != "b.go" || facts[2].Path != "a.go" ||
		facts[2].SupersedesID != facts[0].ID || facts[1].SupersedesID != 0 {
		t.Fatalf("supersede chain=%+v", facts)
	}
	counts, err := ix.FactCountsForSession("s")
	if err != nil || counts.PathReconciliations != 3 {
		t.Fatalf("fact counts=%+v err=%v", counts, err)
	}
}

func TestPathReconciliationIndexesSurviveReopenAndForeignKeyRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	requirePathReconciliationIndexes(t, ix, "first open")
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	if ix, err = Open(path); err != nil {
		t.Fatal(err)
	}
	requirePathReconciliationIndexes(t, ix, "second open")
	// The v8 repair renames the table, recreates it and drops the renamed one,
	// which takes the indexes with it. They must be back by the end of Open.
	if _, err := ix.db.Exec(strings.Replace(`DROP TABLE path_reconciliation;
CREATE TABLE path_reconciliation(
  id INTEGER PRIMARY KEY,
  session_id TEXT NOT NULL,
  checkout_id TEXT NOT NULL,
  predecessor_checkpoint_id INTEGER REFERENCES PARENT(id) ON DELETE RESTRICT,
  current_checkpoint_id INTEGER NOT NULL REFERENCES PARENT(id) ON DELETE RESTRICT,
  event_id INTEGER REFERENCES event(id) ON DELETE SET NULL,
  result_id INTEGER REFERENCES result_observation(id) ON DELETE SET NULL,
  path TEXT NOT NULL,
  old_path TEXT NOT NULL DEFAULT '',
  classification TEXT NOT NULL,
  competing_actor TEXT NOT NULL DEFAULT '',
  runtime_effect_digest TEXT NOT NULL DEFAULT '',
  git_effect_digest TEXT NOT NULL DEFAULT '',
  algorithm TEXT NOT NULL,
  reconciled_at INTEGER NOT NULL,
  supersedes_id INTEGER REFERENCES path_reconciliation(id) ON DELETE RESTRICT
);
CREATE INDEX path_reconciliation_current ON path_reconciliation(current_checkpoint_id,path,algorithm,id);
CREATE INDEX path_reconciliation_session ON path_reconciliation(session_id,reconciled_at,id);`,
		"PARENT", "session_checkpoint_v7", -1)); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	if ix, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var stale int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list('path_reconciliation')
		WHERE "table"='session_checkpoint_v7'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("the fixture must go through the repair: stale=%d err=%v", stale, err)
	}
	requirePathReconciliationIndexes(t, ix, "after the v8 repair")
}

// linkedResultEffectsOracle is the query shape both readers had before they
// became session-scoped: every result's latest reconciliation, store-wide, then
// the session filter. The rewrite must return exactly these rows.
const linkedResultEffectsOracle = `WITH current AS (
		SELECT r.id,r.result_id FROM result_reconciliation r JOIN (
			SELECT result_id,MAX(id) id FROM result_reconciliation GROUP BY result_id
		) latest ON latest.id=r.id WHERE r.join_class='exact'
	), matched AS (SELECT o.id result_id,cand.event_id,e.ordinal,o.completed_at,e.raw_identity,e.operation,e.evidence_source,
		e.content_digest,e.diff_digest,e.completeness,
		CASE WHEN e.content_payload IS NULL THEN 0 ELSE 1 END content_measured,
		CASE WHEN e.diff_payload IS NULL THEN 0 ELSE 1 END diff_measured
		FROM result_observation o JOIN current c ON c.result_id=o.id
		JOIN result_reconciliation_candidate cand ON cand.reconciliation_id=c.id AND cand.selected=1
		JOIN result_effect e ON e.result_id=o.id WHERE o.session_id=?`

func linkedResultEffectsFromOracle(t *testing.T, ix *Index, tail string, args ...any) []LinkedResultEffect {
	t.Helper()
	rows, err := ix.db.Query(linkedResultEffectsOracle+tail, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []LinkedResultEffect{}
	for rows.Next() {
		var effect LinkedResultEffect
		if err := rows.Scan(&effect.ResultID, &effect.EventID, &effect.Ordinal, &effect.CompletedAt,
			&effect.RawIdentity, &effect.Operation, &effect.EvidenceSource,
			&effect.ContentDigest, &effect.DiffDigest, &effect.Completeness,
			&effect.ContentMeasured, &effect.DiffMeasured); err != nil {
			t.Fatal(err)
		}
		out = append(out, effect)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func appendLinkedEffectResult(t *testing.T, ix *Index, session, nativeID string, completedAt int64, identities ...string) int64 {
	t.Helper()
	return appendLinkedEffectResultInState(t, ix, session, nativeID, "success", completedAt, identities...)
}

func appendLinkedEffectResultInState(t *testing.T, ix *Index, session, nativeID, state string, completedAt int64, identities ...string) int64 {
	t.Helper()
	effects := []ResultEffect{}
	for ordinal, identity := range identities {
		effects = append(effects, ResultEffect{Ordinal: ordinal, RawIdentity: identity, Operation: "update",
			EvidenceSource: "runtime-result", SourceField: "changes", Completeness: "complete", DiffBytes: 4,
			DiffDigest: retainedBodyDigest([]byte("diff")), DiffCompleteness: "complete", DiffPayload: []byte("diff")})
	}
	id, duplicate, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res-" + nativeID,
		SessionID: session, Runtime: "codex", Tool: "apply_patch", NativeCallID: nativeID,
		NativeCallKind: "tool_use_id", SourceKind: "vendor-patch-result", SourceDigest: "sha256-v1:" + nativeID,
		State: state, CompletedAt: completedAt, Completeness: "metadata-only",
		PayloadDigest: "sha256-v1:payload-" + nativeID}, effects)
	if err != nil || duplicate {
		t.Fatalf("append result %s: duplicate=%v err=%v", nativeID, duplicate, err)
	}
	return id
}

func requireJoinClass(t *testing.T, ix *Index, resultID, at int64, want string) {
	t.Helper()
	got, err := ix.ReconcileResultObservation(resultID, at)
	if err != nil || got.JoinClass != want {
		t.Fatalf("reconcile result %d: class=%q want %q err=%v", resultID, got.JoinClass, want, err)
	}
}

func TestLinkedResultEffectsMatchStoreWideOracle(t *testing.T) {
	ix := openResultTestIndex(t)

	// Exact from the start; two effects, so ordinal order is exercised.
	appendResultTestEvent(t, ix, "s", "apply_patch", "exact", 10)
	exact := appendLinkedEffectResult(t, ix, "s", "exact", 15, "src/a.go", "src/b.go")
	requireJoinClass(t, ix, exact, 16, "exact")

	// Exact, then superseded by a later non-exact reconciliation that still
	// carries a selected candidate, so only the join class excludes it.
	appendResultTestEvent(t, ix, "s", "apply_patch", "demoted", 11)
	demoted := appendLinkedEffectResult(t, ix, "s", "demoted", 30, "src/a.go")
	requireJoinClass(t, ix, demoted, 31, "exact")
	if _, err := ix.db.Exec(`INSERT INTO result_reconciliation(result_id,join_class,algorithm,reconciled_at,supersedes_id)
		SELECT result_id,'ambiguous',algorithm,32,id FROM result_reconciliation WHERE result_id=?`, demoted); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`INSERT INTO result_reconciliation_candidate(reconciliation_id,event_id,candidate_reason,selected)
		SELECT (SELECT MAX(id) FROM result_reconciliation WHERE result_id=?),c.event_id,c.candidate_reason,1
		FROM result_reconciliation_candidate c JOIN result_reconciliation r ON r.id=c.reconciliation_id
		WHERE r.result_id=? AND c.selected=1`, demoted, demoted); err != nil {
		t.Fatal(err)
	}

	// Unjoined first, exact once its action arrives: included.
	promoted := appendLinkedEffectResult(t, ix, "s", "promoted", 20, "src/a.go")
	requireJoinClass(t, ix, promoted, 21, "unjoined")
	appendResultTestEvent(t, ix, "s", "apply_patch", "promoted", 22)
	requireJoinClass(t, ix, promoted, 23, "exact")

	// Never reconciled, and never exact: both excluded.
	appendLinkedEffectResult(t, ix, "s", "unreconciled", 50, "src/a.go")
	unjoined := appendLinkedEffectResult(t, ix, "s", "unjoined", 60, "src/a.go")
	requireJoinClass(t, ix, unjoined, 61, "unjoined")

	// Exact but not successful: the linked readers never filtered on state.
	appendResultTestEvent(t, ix, "s", "apply_patch", "errored", 13)
	errored := appendLinkedEffectResultInState(t, ix, "s", "errored", "failure", 70, "src/a.go")
	requireJoinClass(t, ix, errored, 71, "exact")

	// Another session's exact result on the same path must not leak in.
	appendResultTestEvent(t, ix, "other", "apply_patch", "foreign", 12)
	foreign := appendLinkedEffectResult(t, ix, "other", "foreign", 25, "src/a.go")
	requireJoinClass(t, ix, foreign, 26, "exact")

	const order = ` ORDER BY completed_at,result_id,ordinal,event_id`
	want := linkedResultEffectsFromOracle(t, ix, `) SELECT * FROM matched`+order, "s")
	got, err := ix.LinkedResultEffectsForSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 4 || want[0].ResultID != exact || want[1].RawIdentity != "src/b.go" ||
		want[2].ResultID != promoted || want[3].ResultID != errored {
		t.Fatalf("fixture no longer exercises supersession: oracle=%+v", want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session effects differ from the oracle:\n got=%+v\nwant=%+v", got, want)
	}

	// The third probe matches both effects of the first-sorted result and cuts
	// between them at limit=1. SQLite happens to return them in ordinal order
	// without the tie-break too, so this pins the result, not the mechanism; the
	// order constant is pinned below.
	for _, probe := range []struct {
		identities []string
		limit      int
		total      int
	}{
		{[]string{"src/a.go", "src/missing.go"}, 1, 3},
		{[]string{"src/a.go", "src/missing.go"}, 10, 3},
		{[]string{"src/b.go", "src/a.go"}, 1, 4},
		{[]string{"src/b.go", "src/a.go"}, 3, 4},
		{[]string{"src/b.go"}, 1, 1},
	} {
		args := []any{"s"}
		for _, identity := range probe.identities {
			args = append(args, identity)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(probe.identities)), ",")
		wantFile := linkedResultEffectsFromOracle(t, ix,
			` AND e.raw_identity IN (`+placeholders+`)) SELECT * FROM matched`+order+` LIMIT ?`,
			append(args, probe.limit)...)
		gotFile, total, err := ix.LinkedResultEffectsForFile("s", probe.identities, probe.limit)
		if err != nil {
			t.Fatal(err)
		}
		if total != probe.total || len(gotFile) == 0 || !reflect.DeepEqual(gotFile, wantFile) {
			t.Fatalf("file effects %+v total=%d differ from the oracle:\n got=%+v\nwant=%+v", probe, total, gotFile, wantFile)
		}
	}
	if linkedResultEffectsOrder != order {
		t.Fatalf("linked effects order=%q, want the total order %q", linkedResultEffectsOrder, order)
	}
	if effects, err := ix.LinkedResultEffectsForSession("other"); err != nil || len(effects) != 1 || effects[0].ResultID != foreign {
		t.Fatalf("other session effects=%+v err=%v", effects, err)
	}
}

// A regression to resolving every result's latest reconciliation store-wide
// would return the same rows, so only the plan can catch it.
func TestLinkedResultEffectsReadOnlyTheSessionsReconciliations(t *testing.T) {
	ix := openResultTestIndex(t)
	file := linkedResultEffectsForSessionCTE + ` AND e.raw_identity IN (?,?))`
	for name, probe := range map[string]struct {
		query string
		args  []any
	}{
		"session":    {linkedResultEffectsForSessionCTE + `)` + linkedResultEffectColumns + linkedResultEffectsOrder, []any{"s"}},
		"file count": {file + ` SELECT COUNT(*) FROM matched`, []any{"s", "a.go", "b.go"}},
		"file rows":  {file + linkedResultEffectColumns + linkedResultEffectsOrder + ` LIMIT ?`, []any{"s", "a.go", "b.go", 10}},
	} {
		requireQueryPlanUses(t, ix, probe.query, "result_observation_session", probe.args...)
		requireQueryPlanUses(t, ix, probe.query, "result_reconciliation_current", probe.args...)
		rows, err := ix.db.Query("EXPLAIN QUERY PLAN "+probe.query, probe.args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(detail, "SCAN") {
				t.Fatalf("%s: linked effects plan scans a table: %s", name, detail)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
}
