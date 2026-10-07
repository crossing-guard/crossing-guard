package store

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Real-magnitude clocks, on purpose: session_checkpoint.capture_ended_at is unix
// SECONDS and understanding_generation.started_at is unix NANOSECONDS. A keep rule
// that mixes the two passes any test seeded with small numbers on both sides.
const (
	retentionNowSeconds = int64(1_791_000_000)
	retentionDay        = int64(86_400)
	retentionSecond     = int64(1_000_000_000)
)

func retentionKeepSince() int64  { return retentionNowSeconds - 14*retentionDay }
func retentionGraceSince() int64 { return (retentionNowSeconds - retentionDay) * retentionSecond }

func retentionGeneration(t *testing.T, ix *Index, checkoutID, digest string, startedSeconds int64, convention string) int64 {
	t.Helper()
	g := completeUnderstanding()
	g.CheckoutID, g.CheckoutRoot = checkoutID, "/repo/"+checkoutID
	g.SnapshotDigest = "git-tree-v2-sha256:" + digest
	g.StartedAt, g.EndedAt = startedSeconds*retentionSecond, startedSeconds*retentionSecond+1
	if convention == "explicit" {
		g.ConventionState, g.ConventionSourceRef, g.ConventionSourceDigest = "explicit", "conventions.json", "sha256-v1:conventions"
	}
	g.Units[0].DescriptorJSON = fmt.Sprintf(`{"path":"a.go","digest":%q}`, digest)
	if err := ix.AppendUnderstanding(&g); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

func retentionCheckpoint(t *testing.T, ix *Index, sessionID, checkoutID, kind, digest string, endedSeconds int64) {
	t.Helper()
	appendUnderstandingCheckpointFixture(t, ix, sessionID, "repo", checkoutID, kind,
		fmt.Sprintf("%s-%s-%s-%d", sessionID, checkoutID, kind, endedSeconds),
		"git-tree-v2-sha256:"+digest, endedSeconds-1, endedSeconds)
}

// retentionFixture seeds one store with every case the keep rule names. The
// expected outcome is written here, by hand, and never computed by the store.
type retentionFixture struct {
	ix                                              *Index
	liveBaseline, liveIntermediate, liveCurrent     int64
	quietBaseline, quietCurrent                     int64
	orphan, young, otherCheckoutNewest, explicitNew int64
	failed                                          int64
	// A live session whose first checkpoint is not an opening one, whose newest
	// two checkpoints share a second, and which also touched a second checkout.
	oddSettledFirst, oddBaseline, oddTieLoser, oddCurrent int64
	secondCheckoutBoundary, secondCheckoutNewest          int64
}

func newRetentionFixture(t *testing.T) retentionFixture {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	now := retentionNowSeconds
	f := retentionFixture{ix: ix}
	// A session still active two days ago whose baseline is thirty days old.
	retentionCheckpoint(t, ix, "live", "checkout", "attachment", "live-base", now-30*retentionDay)
	retentionCheckpoint(t, ix, "live", "checkout", "settled", "live-mid", now-20*retentionDay)
	retentionCheckpoint(t, ix, "live", "checkout", "settled", "live-current", now-2*retentionDay)
	f.liveBaseline = retentionGeneration(t, ix, "checkout", "live-base", now-30*retentionDay, "none")
	f.liveIntermediate = retentionGeneration(t, ix, "checkout", "live-mid", now-20*retentionDay, "none")
	f.liveCurrent = retentionGeneration(t, ix, "checkout", "live-current", now-2*retentionDay, "none")
	// A session quiet for twenty-five days.
	retentionCheckpoint(t, ix, "quiet", "checkout", "attachment", "quiet-base", now-40*retentionDay)
	retentionCheckpoint(t, ix, "quiet", "checkout", "settled", "quiet-current", now-25*retentionDay)
	f.quietBaseline = retentionGeneration(t, ix, "checkout", "quiet-base", now-40*retentionDay, "none")
	f.quietCurrent = retentionGeneration(t, ix, "checkout", "quiet-current", now-25*retentionDay, "none")
	// Scans with no checkpoint: one old, one inside the grace (and newest of the checkout).
	f.orphan = retentionGeneration(t, ix, "checkout", "orphan", now-10*retentionDay, "none")
	f.young = retentionGeneration(t, ix, "checkout", "young", now-3600, "none")
	// The only generation of another checkout, and the only explicit-convention one.
	f.otherCheckoutNewest = retentionGeneration(t, ix, "other", "other-only", now-50*retentionDay, "none")
	f.explicitNew = retentionGeneration(t, ix, "checkout", "explicit-only", now-35*retentionDay, "explicit")
	failed := UnderstandingGeneration{RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo/checkout",
		Status: "failed", ConventionState: "none", StartedAt: (now - 45*retentionDay) * retentionSecond,
		EndedAt: (now-45*retentionDay)*retentionSecond + 1, LimitationCode: "source_changed", Limitation: "changed"}
	if err := ix.AppendUnderstanding(&failed); err != nil {
		t.Fatal(err)
	}
	f.failed = failed.ID
	// "odd": settled first, attachment later (the baseline is the first OPENING
	// checkpoint, not the first checkpoint), then two checkpoints in the same second
	// (the higher id is current).
	retentionCheckpoint(t, ix, "odd", "checkout", "settled", "odd-settled-first", now-12*retentionDay)
	retentionCheckpoint(t, ix, "odd", "checkout", "attachment", "odd-baseline", now-11*retentionDay)
	retentionCheckpoint(t, ix, "odd", "checkout", "pre-verification", "odd-tie-loser", now-3*retentionDay)
	retentionCheckpoint(t, ix, "odd", "checkout", "settled", "odd-current", now-3*retentionDay)
	f.oddSettledFirst = retentionGeneration(t, ix, "checkout", "odd-settled-first", now-12*retentionDay, "none")
	f.oddBaseline = retentionGeneration(t, ix, "checkout", "odd-baseline", now-11*retentionDay, "none")
	f.oddTieLoser = retentionGeneration(t, ix, "checkout", "odd-tie-loser", now-3*retentionDay, "none")
	f.oddCurrent = retentionGeneration(t, ix, "checkout", "odd-current", now-3*retentionDay, "none")
	// Its one checkpoint in a second checkout is baseline and current there; a newer
	// scan of that checkout means only K1 keeps it.
	retentionCheckpoint(t, ix, "odd", "second", "attachment", "second-boundary", now-9*retentionDay)
	f.secondCheckoutBoundary = retentionGeneration(t, ix, "second", "second-boundary", now-9*retentionDay, "none")
	f.secondCheckoutNewest = retentionGeneration(t, ix, "second", "second-newest", now-8*retentionDay, "none")
	// A checkpoint that never completed pins nothing and makes no session live.
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "quiet", ScopeKey: "checkout",
		Kind: "settled", RequestID: "quiet-pending", WorkingDirectory: "/repo/checkout", RepositoryID: "repo",
		CheckoutID: "checkout", CheckoutRoot: "/repo/checkout", Status: "pending", BoundaryClass: "settled",
		RequestedAt: now - 60}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

type retentionChildren struct {
	Units    []UnderstandingUnit
	Edges    []UnderstandingEdge
	Coverage []UnderstandingCoverage
}

func readRetentionChildren(t *testing.T, ix *Index, generationID int64) retentionChildren {
	t.Helper()
	units, _, err := ix.UnderstandingUnits(generationID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	edges, _, err := ix.UnderstandingEdges(generationID, "", "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := ix.UnderstandingCoverage(generationID)
	if err != nil {
		t.Fatal(err)
	}
	return retentionChildren{Units: units, Edges: edges, Coverage: coverage}
}

func retentionChildRows(t *testing.T, ix *Index, generationID int64) int {
	t.Helper()
	var rows int
	if err := ix.db.QueryRow(`SELECT (SELECT COUNT(*) FROM understanding_unit WHERE generation_id=?1)
		+(SELECT COUNT(*) FROM understanding_edge WHERE generation_id=?1)
		+(SELECT COUNT(*) FROM understanding_coverage WHERE generation_id=?1)`, generationID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// pruneFully retires a generation and drains its rows in deliberately small
// transactions, the way the retention worker does.
func pruneFully(t *testing.T, ix *Index, generationID int64) bool {
	t.Helper()
	pruned, err := ix.PruneUnderstandingFacts(generationID, retentionKeepSince(), retentionGraceSince())
	if err != nil {
		t.Fatal(err)
	}
	for more := pruned; more; {
		if more, err = ix.DeletePrunedUnderstandingFacts(generationID, 1); err != nil {
			t.Fatal(err)
		}
	}
	return pruned
}

func retentionFactsState(t *testing.T, ix *Index, generationID int64) string {
	t.Helper()
	generation, found, err := ix.UnderstandingGenerationByID(generationID)
	if err != nil || !found {
		t.Fatalf("generation %d found=%v err=%v", generationID, found, err)
	}
	return generation.FactsState
}

func TestUnderstandingRetentionPrunesExactlyTheUnkeptGenerations(t *testing.T) {
	f := newRetentionFixture(t)
	ix := f.ix
	kept := []int64{f.liveBaseline, f.liveCurrent, f.young, f.otherCheckoutNewest, f.explicitNew,
		f.oddBaseline, f.oddCurrent, f.secondCheckoutBoundary, f.secondCheckoutNewest}
	want := []int64{f.quietBaseline, f.quietCurrent, f.liveIntermediate, f.oddSettledFirst, f.orphan, f.oddTieLoser} // oldest first
	before := map[int64]retentionChildren{}
	for _, id := range kept {
		before[id] = readRetentionChildren(t, ix, id)
	}
	var generationsBefore int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_generation`).Scan(&generationsBefore); err != nil {
		t.Fatal(err)
	}
	candidates, err := ix.PrunableUnderstandingGenerations(retentionKeepSince(), retentionGraceSince())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("candidates=%v want=%v", candidates, want)
	}
	// The transaction decides for itself: asked about every generation, it prunes
	// exactly the same set the list named.
	all := append(append([]int64{f.failed}, kept...), want...)
	pruned := []int64{}
	for _, id := range all {
		if pruneFully(t, ix, id) {
			pruned = append(pruned, id)
		} else if _, err := ix.DeletePrunedUnderstandingFacts(id, 100); err == nil {
			t.Fatalf("the row delete accepted generation %d, which is not pruned", id)
		}
	}
	sort.Slice(pruned, func(i, j int) bool { return pruned[i] < pruned[j] })
	sortedWant := append([]int64{}, want...)
	sort.Slice(sortedWant, func(i, j int) bool { return sortedWant[i] < sortedWant[j] })
	if !reflect.DeepEqual(pruned, sortedWant) {
		t.Fatalf("pruned=%v want=%v", pruned, sortedWant)
	}
	for _, id := range want {
		if rows := retentionChildRows(t, ix, id); rows != 0 || retentionFactsState(t, ix, id) != UnderstandingFactsPruned {
			t.Fatalf("pruned generation %d rows=%d state=%s", id, rows, retentionFactsState(t, ix, id))
		}
		generation, _, _ := ix.UnderstandingGenerationByID(id)
		if generation.UnitTotal != 1 || generation.EdgeTotal != 1 {
			t.Fatalf("pruned generation %d lost its measured totals: %+v", id, generation)
		}
	}
	for _, id := range kept {
		if retentionFactsState(t, ix, id) != UnderstandingFactsPresent || !reflect.DeepEqual(before[id], readRetentionChildren(t, ix, id)) {
			t.Fatalf("kept generation %d changed", id)
		}
	}
	if state := retentionFactsState(t, ix, f.failed); state != UnderstandingFactsPresent {
		t.Fatalf("failed generation state=%s", state)
	}
	var generationsAfter int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_generation`).Scan(&generationsAfter); err != nil || generationsAfter != generationsBefore {
		t.Fatalf("generation rows %d -> %d err=%v", generationsBefore, generationsAfter, err)
	}
	again, err := ix.PrunableUnderstandingGenerations(retentionKeepSince(), retentionGraceSince())
	if err != nil || len(again) != 0 {
		t.Fatalf("second pass candidates=%v err=%v", again, err)
	}
}

func TestUnderstandingPruneRechecksKeepRuleInsideTheTransaction(t *testing.T) {
	f := newRetentionFixture(t)
	ix := f.ix
	candidates, err := ix.PrunableUnderstandingGenerations(retentionKeepSince(), retentionGraceSince())
	if err != nil || len(candidates) != 6 {
		t.Fatalf("candidates=%v err=%v", candidates, err)
	}
	// The quiet session resumes after the list was taken: its baseline is now the
	// baseline of a live session and must survive, although the list named it.
	retentionCheckpoint(t, ix, "quiet", "checkout", "settled", "quiet-resumed", retentionNowSeconds-60)
	baseline := readRetentionChildren(t, ix, f.quietBaseline)
	ok, err := ix.PruneUnderstandingFacts(f.quietBaseline, retentionKeepSince(), retentionGraceSince())
	if err != nil || ok {
		t.Fatalf("resumed session's baseline pruned=%v err=%v", ok, err)
	}
	if retentionFactsState(t, ix, f.quietBaseline) != UnderstandingFactsPresent ||
		!reflect.DeepEqual(baseline, readRetentionChildren(t, ix, f.quietBaseline)) {
		t.Fatal("resumed session's baseline changed")
	}
	// Its old current checkpoint is now an intermediate one and goes.
	ok, err = ix.PruneUnderstandingFacts(f.quietCurrent, retentionKeepSince(), retentionGraceSince())
	if err != nil || !ok {
		t.Fatalf("superseded current pruned=%v err=%v", ok, err)
	}
}

func TestUnderstandingPrunedSnapshotIsRemeasuredAsANewGeneration(t *testing.T) {
	f := newRetentionFixture(t)
	ix := f.ix
	// Retired but not yet drained: the re-measure below must not depend on the old
	// rows being gone, and the leftover sweep must find them.
	if ok, err := ix.PruneUnderstandingFacts(f.orphan, retentionKeepSince(), retentionGraceSince()); err != nil || !ok {
		t.Fatalf("prune ok=%v err=%v", ok, err)
	}
	if leftovers, err := ix.PrunedUnderstandingGenerationsWithFacts(); err != nil || !reflect.DeepEqual(leftovers, []int64{f.orphan}) {
		t.Fatalf("leftovers=%v err=%v", leftovers, err)
	}
	if units, total, err := ix.UnderstandingUnits(f.orphan, 0, 10); err != nil || total != 1 || len(units) != 1 {
		t.Fatalf("undrained rows total=%d err=%v", total, err)
	}
	digest := "git-tree-v2-sha256:orphan"
	exists, err := ix.PrunedUnderstandingGenerationExists("repo", "checkout", digest, "codemap-v1", "sha256-v1:bundle", "none", "")
	if err != nil || !exists {
		t.Fatalf("pruned identity exists=%v err=%v", exists, err)
	}
	// One failed attempt lands between the prune and the successful re-measure.
	failed := UnderstandingGeneration{RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo/checkout",
		Status: "failed", SnapshotProtocol: "git-tree-v2", SnapshotDigest: digest, StructuralSchema: "codemap-v1",
		AnalyzerBundleDigest: "sha256-v1:bundle", ConventionState: "none",
		StartedAt: (retentionNowSeconds - 30) * retentionSecond, EndedAt: (retentionNowSeconds - 29) * retentionSecond,
		LimitationCode: "scan_timeout", Limitation: "timed out"}
	if err := ix.AppendUnderstanding(&failed); err != nil {
		t.Fatal(err)
	}
	remeasure := func(startedSeconds int64) UnderstandingGeneration {
		g := completeUnderstanding()
		g.CheckoutRoot, g.SnapshotDigest = "/repo/checkout", digest
		g.StartedAt, g.EndedAt = startedSeconds*retentionSecond, startedSeconds*retentionSecond+1
		if err := ix.AppendUnderstanding(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	fresh := remeasure(retentionNowSeconds - 10)
	if fresh.ID == f.orphan || fresh.ID == failed.ID || fresh.FactsState != UnderstandingFactsPresent {
		t.Fatalf("re-measure did not append a new present generation: %+v", fresh)
	}
	if retentionFactsState(t, ix, f.orphan) != UnderstandingFactsPruned || retentionChildRows(t, ix, f.orphan) != 2 {
		t.Fatal("the pruned generation was rewritten")
	}
	for more := true; more; {
		var err error
		if more, err = ix.DeletePrunedUnderstandingFacts(f.orphan, 1); err != nil {
			t.Fatal(err)
		}
	}
	if leftovers, err := ix.PrunedUnderstandingGenerationsWithFacts(); err != nil || len(leftovers) != 0 || retentionChildRows(t, ix, f.orphan) != 0 {
		t.Fatalf("drain left rows: leftovers=%v err=%v", leftovers, err)
	}
	exact, found, err := ix.UnderstandingForSnapshot("repo", "checkout", digest, "codemap-v1", "sha256-v1:bundle", "none", "")
	if err != nil || !found || exact.ID != fresh.ID {
		t.Fatalf("readers resolve id=%d found=%v err=%v, want %d", exact.ID, found, err, fresh.ID)
	}
	// A replay of the same scan returns the present row, never the pruned one, and
	// raises no identity collision.
	if replay := remeasure(retentionNowSeconds - 5); replay.ID != fresh.ID {
		t.Fatalf("replay id=%d want=%d", replay.ID, fresh.ID)
	}
	if retentionChildRows(t, ix, fresh.ID) != 3 {
		t.Fatalf("re-measured generation rows=%d", retentionChildRows(t, ix, fresh.ID))
	}
	// The new generation is inside the grace and is not a candidate.
	candidates, err := ix.PrunableUnderstandingGenerations(retentionKeepSince(), retentionGraceSince())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range candidates {
		if id == fresh.ID {
			t.Fatal("the re-measured generation is a prune candidate")
		}
	}
}

func TestUnderstandingRecoveryIgnoresAPrunedGeneration(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	now := retentionNowSeconds
	retentionCheckpoint(t, ix, "session", "checkout", "settled", "tree", now-60)
	id := retentionGeneration(t, ix, "checkout", "tree", now-60*retentionDay, "none")
	needing := func() int {
		checkpoints, err := ix.RecentCheckpointsNeedingUnderstanding(now-retentionDay, 0, "codemap-v1", "sha256-v1:bundle", 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(checkpoints)
	}
	if needing() != 0 {
		t.Fatal("a present generation did not suppress recovery")
	}
	// It is the newest generation of its checkout, so retention would keep it; the
	// state is forced here to test the suppressor alone.
	for _, statement := range []string{`DELETE FROM understanding_coverage WHERE generation_id=?`,
		`DELETE FROM understanding_edge WHERE generation_id=?`, `DELETE FROM understanding_unit WHERE generation_id=?`,
		`UPDATE understanding_generation SET facts_state='pruned' WHERE id=?`} {
		if _, err := ix.db.Exec(statement, id); err != nil {
			t.Fatal(err)
		}
	}
	if needing() != 1 {
		t.Fatal("a pruned generation still suppressed recovery")
	}
}

func understandingGenerationLayout(t *testing.T, ix *Index) ([]string, string) {
	t.Helper()
	rows, err := ix.db.Query(`SELECT name||'|'||type||'|'||"notnull"||'|'||COALESCE(dflt_value,'')||'|'||pk
		FROM pragma_table_info('understanding_generation') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	// The identity index and every trigger on the understanding tables, as one string.
	var definitions string
	if err := ix.db.QueryRow(`SELECT group_concat(sql, char(10)) FROM (SELECT sql FROM sqlite_master
		WHERE (type='index' AND name='understanding_generation_complete_identity')
		OR (type='trigger' AND tbl_name LIKE 'understanding_%') ORDER BY type,name)`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	return columns, definitions
}

func TestV43UpgradeAddsFactsStateAndNarrowsTheIdentityIndex(t *testing.T) {
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	freshColumns, freshIndex := understandingGenerationLayout(t, fresh)
	if last := freshColumns[len(freshColumns)-1]; last != "facts_state|TEXT|1|'present'|0" {
		t.Fatalf("fresh last column=%q", last)
	}

	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := completeUnderstanding()
	if err := ix.AppendUnderstanding(&g); err != nil {
		t.Fatal(err)
	}
	// Back to the schema-42 layout: no column, the un-narrowed index.
	if _, err := ix.db.Exec(`DROP INDEX understanding_generation_complete_identity;
		ALTER TABLE understanding_generation DROP COLUMN facts_state;
		CREATE UNIQUE INDEX understanding_generation_complete_identity
		  ON understanding_generation(repository_id,checkout_id,snapshot_digest,structural_schema,
		    analyzer_bundle_digest,convention_state,convention_source_digest)
		  WHERE status='complete';
		PRAGMA user_version=42`); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	columns, indexSQL := understandingGenerationLayout(t, ix)
	if !reflect.DeepEqual(columns, freshColumns) || indexSQL != freshIndex {
		t.Fatalf("migrated layout differs from fresh:\ncolumns %v\n     vs %v\nindex %q\n   vs %q", columns, freshColumns, indexSQL, freshIndex)
	}
	if state := retentionFactsState(t, ix, g.ID); state != UnderstandingFactsPresent {
		t.Fatalf("existing generation migrated to %q", state)
	}
	if rows := retentionChildRows(t, ix, g.ID); rows != 3 {
		t.Fatalf("migration changed child rows: %d", rows)
	}
}

// The schema-43 note's rollback, run as written: it must succeed with foreign keys
// on, leave no pruned row for a schema-42 binary to misread as "measured, empty",
// and a later upgrade must be clean.
func TestV43RollbackProcedureRunsAndReupgrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	kept := retentionGeneration(t, ix, "checkout", "kept", retentionNowSeconds-3600, "none")
	gone := retentionGeneration(t, ix, "checkout", "gone", retentionNowSeconds-60*retentionDay, "none")
	if ok, err := ix.PruneUnderstandingFacts(gone, retentionKeepSince(), retentionGraceSince()); err != nil || !ok {
		t.Fatalf("prune ok=%v err=%v", ok, err)
	}
	// Retired but not drained, the state a stopped daemon leaves behind. The SQL is the
	// schema note's own constant, not a copy of it.
	if _, err := ix.db.Exec(understandingFactsStateV43RollbackSQL); err != nil {
		t.Fatalf("rollback procedure: %v", err)
	}
	ix.Close()
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, found, err := ix.UnderstandingGenerationByID(gone); err != nil || found {
		t.Fatalf("pruned row survived the rollback: found=%v err=%v", found, err)
	}
	if rows := retentionChildRows(t, ix, kept); rows != 3 {
		t.Fatalf("kept generation rows=%d", rows)
	}
	if _, indexSQL := understandingGenerationLayout(t, ix); !strings.Contains(indexSQL, "facts_state") {
		t.Fatalf("re-upgrade did not narrow the index: %q", indexSQL)
	}
}

// A prune that cannot take the write lock returns the driver's busy error, which
// IsBusyError recognises: the retention worker counts it as a skip and moves on.
func TestUnderstandingPruneReportsABusyStore(t *testing.T) {
	f := newRetentionFixture(t)
	var path string
	if err := f.ix.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	holder, err := other.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	pruned, err := f.ix.PruneUnderstandingFacts(f.orphan, retentionKeepSince(), retentionGraceSince())
	if pruned || !IsBusyError(err) {
		t.Fatalf("prune beside a held write lock: pruned=%v err=%v", pruned, err)
	}
	if retentionFactsState(t, f.ix, f.orphan) != UnderstandingFactsPresent {
		t.Fatal("a busy prune changed the generation")
	}
}
