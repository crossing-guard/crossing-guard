package store

import (
	"path/filepath"
	"testing"
)

func openUsageStore(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func count(value int64) *int64 { return &value }

// usageCall is one call from source in session, completing at atMS.
func usageCall(source, session, id string, atMS, output int64) UsageCallRecord {
	return UsageCallRecord{Runtime: "rt", CallID: id, SessionID: session, Source: source, FirstAtMS: atMS,
		AtMS: atMS, Model: "model-a", Input: count(10), CacheRead: count(90), CacheWrite: count(0),
		Output: count(output), Reader: "rt/usage-1"}
}

func writeSource(t *testing.T, ix *Index, source, session string, born *int64, restart, complete bool,
	calls ...UsageCallRecord) UsageWriteResult {
	t.Helper()
	for index := range calls {
		calls[index].SourceBornMS = born
	}
	result, err := ix.WriteUsageSource(UsageSourceWrite{Restart: restart, Calls: calls,
		State: UsageSourceState{Runtime: "rt", Source: source, SessionID: session, Marker: "m",
			Reader: "rt/usage-1", Complete: complete, UpdatedAtMS: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func usageOwnerOf(t *testing.T, ix *Index, id string) (string, string, int64) {
	t.Helper()
	var source, session string
	var output int64
	if err := ix.db.QueryRow(`SELECT source,session_id,COALESCE(output,0) FROM usage_call WHERE runtime='rt' AND call_id=?`,
		id).Scan(&source, &session, &output); err != nil {
		return "", "", -1
	}
	return source, session, output
}

func totalCalls(t *testing.T, ix *Index) int64 {
	t.Helper()
	groups, err := ix.UsageGroups(UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		return 0
	}
	return groups[0].Calls
}

func TestUsageCopiesCountOnceAndTheEarliestBornSourceOwns(t *testing.T) {
	ix := openUsageStore(t)
	original, fork := count(100), count(200)
	// The fork is read first; the original, born earlier, takes the call over.
	writeSource(t, ix, "src-fork", "session-fork", fork, false, true, usageCall("src-fork", "session-fork", "c1", 1000, 5))
	writeSource(t, ix, "src-orig", "session-orig", original, false, true, usageCall("src-orig", "session-orig", "c1", 1000, 5))
	if source, session, _ := usageOwnerOf(t, ix, "c1"); source != "src-orig" || session != "session-orig" {
		t.Fatalf("owner = %s/%s, want the earliest-born source", source, session)
	}
	// Re-reading the fork again never moves it back.
	result := writeSource(t, ix, "src-fork", "session-fork", fork, false, true, usageCall("src-fork", "session-fork", "c1", 1000, 5))
	if result.Copies != 1 || totalCalls(t, ix) != 1 {
		t.Fatalf("a copy counts once: %+v calls=%d", result, totalCalls(t, ix))
	}
	if source, _, _ := usageOwnerOf(t, ix, "c1"); source != "src-orig" {
		t.Fatalf("owner moved to %s", source)
	}
}

func TestUsageOwnerReReadUpdatesFiguresAndKeepsFirstLine(t *testing.T) {
	ix := openUsageStore(t)
	partial := usageCall("src", "s", "c1", 1000, 2)
	writeSource(t, ix, "src", "s", nil, false, false, partial)
	complete := usageCall("src", "s", "c1", 1900, 90)
	complete.FirstAtMS = 1900 // a later read sees only the later line
	writeSource(t, ix, "src", "s", nil, false, true, complete)
	var firstAt, atMS, output int64
	if err := ix.db.QueryRow(`SELECT first_at_ms,at_ms,output FROM usage_call WHERE call_id='c1'`).Scan(&firstAt, &atMS, &output); err != nil {
		t.Fatal(err)
	}
	if firstAt != 1000 || atMS != 1900 || output != 90 {
		t.Fatalf("streaming snapshot: first=%d at=%d output=%d", firstAt, atMS, output)
	}
}

func TestUsageRestartNeverDipsAndPromotesCopies(t *testing.T) {
	ix := openUsageStore(t)
	originalBorn, forkBorn := count(100), count(200)
	writeSource(t, ix, "src-a", "session-a", originalBorn, false, true,
		usageCall("src-a", "session-a", "shared", 1000, 5), usageCall("src-a", "session-a", "only-a", 1100, 7),
		usageCall("src-a", "session-a", "kept", 1200, 9))
	writeSource(t, ix, "src-b", "session-b", forkBorn, false, true, usageCall("src-b", "session-b", "shared", 1000, 5))
	// src-a is rewritten: the re-read so far holds only "kept". Nothing dips.
	result := writeSource(t, ix, "src-a", "session-a", originalBorn, true, false, usageCall("src-a", "session-a", "kept", 1200, 9))
	if result.Epoch != 2 || totalCalls(t, ix) != 3 {
		t.Fatalf("during a re-read the old rows still count: %+v calls=%d", result, totalCalls(t, ix))
	}
	// The re-read completes without "shared" and "only-a".
	result = writeSource(t, ix, "src-a", "session-a", originalBorn, false, true)
	if result.Removed != 1 || result.Promoted != 1 || totalCalls(t, ix) != 2 {
		t.Fatalf("completion: %+v calls=%d", result, totalCalls(t, ix))
	}
	if source, session, output := usageOwnerOf(t, ix, "shared"); source != "src-b" || session != "session-b" || output != 5 {
		t.Fatalf("the copy takes the call over with its figures: %s/%s/%d", source, session, output)
	}
	if source, _, _ := usageOwnerOf(t, ix, "only-a"); source != "" {
		t.Fatal("a call no live source holds is removed")
	}
}

func TestUsageRewrittenHolderIsNeverResurrected(t *testing.T) {
	ix := openUsageStore(t)
	writeSource(t, ix, "src-a", "session-a", count(100), false, true, usageCall("src-a", "session-a", "x", 1000, 5))
	writeSource(t, ix, "src-b", "session-b", count(200), false, true, usageCall("src-b", "session-b", "x", 1000, 5))
	// The holder is rewritten and no longer holds x: its copy row goes.
	writeSource(t, ix, "src-b", "session-b", count(200), true, true)
	// Then the owner is rewritten without x: nobody holds it any more.
	result := writeSource(t, ix, "src-a", "session-a", count(100), true, true)
	if result.Promoted != 0 || result.Removed != 1 || totalCalls(t, ix) != 0 {
		t.Fatalf("a stale copy must not bring x back: %+v", result)
	}
}

func TestUsagePruneHorizonHoldsAndCoversCopies(t *testing.T) {
	ix := openUsageStore(t)
	writeSource(t, ix, "src-a", "session-a", count(100), false, true,
		usageCall("src-a", "session-a", "old", 1000, 5), usageCall("src-a", "session-a", "new", 5000, 7))
	writeSource(t, ix, "src-b", "session-b", count(200), false, true, usageCall("src-b", "session-b", "old", 1000, 5))
	if before, _ := ix.CountUsageCallsBefore(3000); before != 1 {
		t.Fatalf("dry run counts calls, not copies: %d", before)
	}
	if deleted, err := ix.PruneUsageCallsBefore(3000); err != nil || deleted != 1 {
		t.Fatalf("prune: %d %v", deleted, err)
	}
	var copies int
	_ = ix.db.QueryRow(`SELECT COUNT(*) FROM usage_call_copy`).Scan(&copies)
	if copies != 0 {
		t.Fatal("prune covers remembered copies too")
	}
	// A later, longer horizon never reopens the window.
	if _, err := ix.PruneUsageCallsBefore(2000); err != nil {
		t.Fatal(err)
	}
	if horizon, _ := ix.UsagePruneHorizon(); horizon != 3000 {
		t.Fatalf("horizon = %d, want the maximum", horizon)
	}
	// A re-read after a rewrite cannot bring pruned history back.
	result := writeSource(t, ix, "src-a", "session-a", count(100), true, true,
		usageCall("src-a", "session-a", "old", 1000, 5), usageCall("src-a", "session-a", "new", 5000, 7))
	if result.BelowHorizon != 1 || totalCalls(t, ix) != 1 {
		t.Fatalf("pruned calls stay pruned: %+v", result)
	}
}

func TestUsageGroupsStateCoverageAndBuckets(t *testing.T) {
	ix := openUsageStore(t)
	monday := int64(1789430400000) // 2026-09-14 00:00 UTC, a Monday
	day := int64(24 * 3600 * 1000)
	stated := usageCall("src", "s", "c1", monday+2*day, 100)
	stated.Reasoning = count(40)
	stated.Parts = `[{"id":"cache-1h","label":"1-hour cache","of":"cache-write","count":6}]`
	stated.Cost = &UsageCost{Amount: 0.5, Unit: "USD", Basis: "runtime"}
	unstated := usageCall("src", "s", "c2", monday+9*day, 60)
	unstated.Model = "model-b"
	writeSource(t, ix, "src", "s", nil, false, true, stated, unstated)
	groups, err := ix.UsageGroups(UsageQuery{GroupBy: []string{UsageDimModel}, Bucket: UsageBucketWeek})
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups: %+v err=%v", groups, err)
	}
	byModel := map[string]UsageGroup{}
	for _, group := range groups {
		byModel[group.Key.Model] = group
	}
	a, b := byModel["model-a"], byModel["model-b"]
	if a.Key.Bucket != "2026-09-14" || b.Key.Bucket != "2026-09-21" {
		t.Fatalf("a week is the Monday on or before the call: %q %q", a.Key.Bucket, b.Key.Bucket)
	}
	if a.Reasoning.Sum != 40 || a.Reasoning.Stated != 1 || a.ShareReasoning != 40 || a.ShareOutput != 100 {
		t.Fatalf("reasoning share over calls stating both: %+v", a)
	}
	if b.Reasoning.Stated != 0 || b.ShareCalls != 0 {
		t.Fatalf("an unstated class is unknown, not zero: %+v", b)
	}
	if len(a.Parts) != 1 || a.Parts[0].Count != 6 || a.Parts[0].Of != "cache-write" || len(a.Cost) != 1 ||
		a.Cost[0].Amount != 0.5 || len(a.Readers) != 1 {
		t.Fatalf("details: parts=%+v cost=%+v readers=%+v", a.Parts, a.Cost, a.Readers)
	}
	if a.ContextCalls != 1 || a.ContextSum != 100 {
		t.Fatalf("context per call: %d/%d", a.ContextSum, a.ContextCalls)
	}
	if _, err := ix.UsageGroups(UsageQuery{GroupBy: []string{"bogus"}}); err == nil {
		t.Fatal("an unknown dimension is refused")
	}
	work := testWorkIndex(t, ix, nil)
	top, byContext, err := ix.UsageRootTotals(UsageQuery{}, work, 5)
	if err != nil || len(top) != 1 || top[0].Main.Calls != 2 || top[0].Total != 2*100+100+60 || top[0].LastModel != "model-b" ||
		len(byContext) != 1 || byContext[0].LastContext != 100 {
		t.Fatalf("top sessions: %+v %+v err=%v", top, byContext, err)
	}
	var outbox int
	_ = ix.db.QueryRow(`SELECT COUNT(*) FROM sync_outbox`).Scan(&outbox)
	if outbox != 0 {
		t.Fatal("usage rows never enter sync_outbox")
	}
}

func TestUsageSchemaIsAdditiveAndRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// The documented rollback step (plan §3.6): drop the four tables, stamp 33.
	if _, err := ix.db.Exec(`DROP TABLE usage_call; DROP TABLE usage_call_copy; DROP TABLE usage_source;
		DROP TABLE usage_prune; PRAGMA user_version=33;`); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var tables, version int
	_ = ix.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'usage_%'`).Scan(&tables)
	_ = ix.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	// Relative assertion: later schema bumps (v36 memory tables) must not
	// re-break this test — the rollback under test is "older store re-migrates
	// to CURRENT", whatever current is (the RT-1 fold's rule).
	if tables != 4 || version != SchemaVersion || SchemaVersion < 35 {
		t.Fatalf("tables=%d version=%d", tables, version)
	}
}

// Schema 35's rollback (session usage breakdown plan §5.2): drop the role
// column and stamp 34; opening again adds it back.
func TestUsageRoleColumnRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`ALTER TABLE usage_source DROP COLUMN role; PRAGMA user_version=34;`); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	cols, err := columnSet(ix.db, "usage_source")
	if err != nil || !cols["role"] {
		t.Fatalf("role column after reopen: %v err=%v", cols, err)
	}
}

// An owner whose later read no longer starts at the call's first line keeps
// the earliest first line it stated, so the call stays with it (code red-team
// C-9; the copy path takes the same minimum from the holder's copy row).
func TestUsageOwnerKeepsItsEarliestFirstLineAcrossReads(t *testing.T) {
	ix := openUsageStore(t)
	writeSource(t, ix, "src-a", "session-a", count(100), false, true, usageCall("src-a", "session-a", "x", 1000, 5))
	first := usageCall("src-b", "session-b", "x", 1000, 5)
	first.FirstAtMS = 500 // src-b's first line of x is earlier: it owns x
	writeSource(t, ix, "src-a", "session-a", count(100), false, true, usageCall("src-a", "session-a", "x", 1000, 5))
	writeSource(t, ix, "src-b", "session-b", count(200), false, false, first)
	later := usageCall("src-b", "session-b", "x", 1000, 5)
	later.FirstAtMS = 900 // a later read of src-b starts after that first line
	writeSource(t, ix, "src-b", "session-b", count(200), false, true, later)
	if source, _, _ := usageOwnerOf(t, ix, "x"); source != "src-b" {
		t.Fatalf("owner = %s, want the source with the earliest first line", source)
	}
}

// Two calls completing in the same millisecond: the latest-call levels come
// from the larger call id, every time (code red-team C-11).
func TestUsageTopSessionsBreaksATimeTieByCallID(t *testing.T) {
	ix := openUsageStore(t)
	first := usageCall("src", "s", "call-a", 5000, 1)
	first.Model, first.Input = "model-a", count(1)
	second := usageCall("src", "s", "call-b", 5000, 1)
	second.Model, second.Input = "model-b", count(2)
	writeSource(t, ix, "src", "s", nil, false, true, second, first)
	_, byContext, err := ix.UsageRootTotals(UsageQuery{}, testWorkIndex(t, ix, nil), 1)
	if err != nil || len(byContext) != 1 || byContext[0].LastModel != "model-b" || byContext[0].LastAtMS != 5000 ||
		byContext[0].LastContext != 2+90 {
		t.Fatalf("latest call: %+v err=%v", byContext, err)
	}
}
