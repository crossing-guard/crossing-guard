package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/codemap"
	"crossing-guard/store"
)

func retentionCoordinatorGeneration(t *testing.T, governor *Governor, work store.UnderstandingCheckpoint, started time.Time) store.UnderstandingGeneration {
	t.Helper()
	generation := store.UnderstandingGeneration{RepositoryID: work.Change.RepositoryID,
		CheckoutID: work.Change.CheckoutID, CheckoutRoot: work.Change.CheckoutRoot,
		Status: "complete", SnapshotProtocol: "git-tree-v2",
		SnapshotDigest: work.Change.SnapshotDigest, BaseRevision: "base", HeadRevision: "head",
		StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: governor.analyzerAssembly.Digest(),
		ConventionState: "none", StartedAt: started.UnixNano(), EndedAt: started.UnixNano() + 1,
		Units: []store.UnderstandingUnit{{Path: "a.go", SourceHash: "sha256-v1:a",
			DescriptorJSON: fmt.Sprintf(`{"path":"a.go","snapshot":%q}`, work.Change.SnapshotDigest)}}}
	if err := governor.ix.AppendUnderstanding(&generation); err != nil {
		t.Fatal(err)
	}
	return generation
}

// holdRetentionSettings pins the configuration a pass re-reads before every write
// to the test's own settings, enabled.
func holdRetentionSettings(coordinator *understandingScanCoordinator) {
	enabled := true
	coordinator.retentionSettings = retentionSettingsFixed(retentionTestSettings(), &enabled)
}

func retentionTestSettings() understandingRetentionSettings {
	return understandingRetentionSettings{keep: 14 * 24 * time.Hour, grace: 24 * time.Hour, deleteRows: 1, deleteDescriptors: 1}
}

func TestUnderstandingRetentionPassPrunesUnkeptGenerationsAndCounts(t *testing.T) {
	governor, coordinator := understandingCoordinatorFixture(t)
	holdRetentionSettings(coordinator)
	now := time.Unix(1_791_000_000, 0)
	coordinator.now = func() time.Time { return now }
	// A session quiet for sixty days left two scans of one checkout; a scan from an
	// hour ago is the newest of that checkout.
	old := understandingWorkFixture("checkout", 1, now.Add(-60*24*time.Hour).Unix(), "attachment")
	appendCoordinatorCheckpointFixture(t, governor.ix, old)
	oldGeneration := retentionCoordinatorGeneration(t, governor, old, now.Add(-60*24*time.Hour))
	young := understandingWorkFixture("checkout", 2, now.Add(-time.Hour).Unix(), "settled")
	young.Checkpoint.SessionID = "other"
	youngGeneration := retentionCoordinatorGeneration(t, governor, young, now.Add(-time.Hour))

	coordinator.retentionPass(context.Background(), retentionTestSettings())

	stored, _, err := governor.ix.UnderstandingGenerationByID(oldGeneration.ID)
	if err != nil || stored.FactsState != store.UnderstandingFactsPruned {
		t.Fatalf("old generation state=%q err=%v", stored.FactsState, err)
	}
	if leftovers, err := governor.ix.PrunedUnderstandingGenerationsWithFacts(); err != nil || len(leftovers) != 0 {
		t.Fatalf("the pass left rows behind: %v err=%v", leftovers, err)
	}
	// The retired generation's descriptor lost its last reference and is gone; the
	// kept generation still reads its own.
	if more, err := governor.ix.DeleteUnreferencedUnderstandingDescriptors(1); err != nil || more {
		t.Fatalf("descriptor sweep more=%v err=%v", more, err)
	}
	if units, _, err := governor.ix.UnderstandingUnits(youngGeneration.ID, 0, 10); err != nil || len(units) != 1 {
		t.Fatalf("kept generation units=%+v err=%v", units, err)
	}
	if units, total, err := governor.ix.UnderstandingUnits(oldGeneration.ID, 0, 10); err != nil || total != 0 || len(units) != 0 {
		t.Fatalf("retired generation units total=%d err=%v", total, err)
	}
	if unfinished := coordinator.stats.pruneUnfinished.Load(); unfinished != 0 {
		t.Fatalf("unfinished=%d after a drained pass", unfinished)
	}
	kept, _, err := governor.ix.UnderstandingGenerationByID(youngGeneration.ID)
	if err != nil || kept.FactsState != store.UnderstandingFactsPresent {
		t.Fatalf("young generation state=%q err=%v", kept.FactsState, err)
	}
	if coordinator.stats.factsPruned.Load() != 1 || coordinator.stats.pruneCandidates.Load() != 0 ||
		coordinator.stats.retentionLastPass.Load() != now.Unix() {
		t.Fatalf("pruned=%d candidates=%d lastPass=%d", coordinator.stats.factsPruned.Load(),
			coordinator.stats.pruneCandidates.Load(), coordinator.stats.retentionLastPass.Load())
	}
	// A second pass has nothing to do and prunes nothing.
	coordinator.retentionPass(context.Background(), retentionTestSettings())
	if coordinator.stats.factsPruned.Load() != 1 {
		t.Fatalf("second pass pruned again: %d", coordinator.stats.factsPruned.Load())
	}
	health, err := governor.Health()
	if err != nil || health.UnderstandingFactsPruned != 1 || health.UnderstandingRetentionLastPass != now.Unix() ||
		!health.UnderstandingRetentionEnabled {
		t.Fatalf("health record: %+v", health)
	}
}

// retentionBacklog seeds count old generations of one checkout that nothing keeps,
// plus the newest one that K2 does keep.
func retentionBacklog(t *testing.T, governor *Governor, now time.Time, count int) {
	t.Helper()
	for id := int64(1); id <= int64(count)+1; id++ {
		work := understandingWorkFixture("checkout", id, now.Unix(), "settled")
		retentionCoordinatorGeneration(t, governor, work, now.Add(-60*24*time.Hour+time.Duration(id)*time.Second))
	}
}

func retentionSettingsFixed(settings understandingRetentionSettings, enabled *bool) func() (understandingRetentionSettings, bool) {
	return func() (understandingRetentionSettings, bool) { return settings, *enabled }
}

// The pause is a wait the coordinator's stop ends at once; a pass must not sleep
// through it.
func TestUnderstandingRetentionPassStopsInsideItsPause(t *testing.T) {
	governor, coordinator := understandingCoordinatorFixture(t)
	now := time.Unix(1_791_000_000, 0)
	coordinator.now = func() time.Time { return now }
	retentionBacklog(t, governor, now, 3)
	settings := retentionTestSettings()
	settings.pause = time.Hour
	enabled := true
	coordinator.retentionSettings = retentionSettingsFixed(settings, &enabled)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { coordinator.retentionPass(ctx, settings); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for coordinator.stats.factsPruned.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the pass never retired a generation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel() // the pass is now inside its one-hour pause
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stopped coordinator's pass slept through its pause")
	}
	if pruned := coordinator.stats.factsPruned.Load(); pruned != 1 {
		t.Fatalf("pass retired %d generations before stopping", pruned)
	}
}

// Turning retention off, or changing a horizon, ends a running pass at its next
// write: a first pass on a large store runs for hours and a delete cannot be undone.
func TestUnderstandingRetentionPassEndsWhenConfigurationChanges(t *testing.T) {
	for name, change := range map[string]func(*understandingRetentionSettings, *bool){
		"disabled":        func(_ *understandingRetentionSettings, enabled *bool) { *enabled = false },
		"horizon changed": func(settings *understandingRetentionSettings, _ *bool) { settings.keep = 60 * 24 * time.Hour },
	} {
		t.Run(name, func(t *testing.T) {
			governor, coordinator := understandingCoordinatorFixture(t)
			now := time.Unix(1_791_000_000, 0)
			coordinator.now = func() time.Time { return now }
			retentionBacklog(t, governor, now, 3)
			pass := retentionTestSettings()
			current, enabled := pass, true
			coordinator.retentionSettings = func() (understandingRetentionSettings, bool) {
				// The owner edits the file once the first generation is retired and
				// its rows are gone, so the next write would be the second retire.
				if coordinator.stats.factsPruned.Load() >= 1 {
					if leftovers, err := governor.ix.PrunedUnderstandingGenerationsWithFacts(); err == nil && len(leftovers) == 0 {
						change(&current, &enabled)
					}
				}
				return current, enabled
			}
			coordinator.retentionPass(context.Background(), pass)
			if pruned := coordinator.stats.factsPruned.Load(); pruned != 1 {
				t.Fatalf("the pass retired %d generations after the configuration changed", pruned)
			}
			if leftovers, err := governor.ix.PrunedUnderstandingGenerationsWithFacts(); err != nil || len(leftovers) != 0 {
				t.Fatalf("the first generation was not drained: %v err=%v", leftovers, err)
			}
		})
	}
}

// A store another writer holds makes every prune a counted skip, never an error
// that ends the pass or a retry loop.
func TestUnderstandingRetentionPassCountsABusyStoreAsASkip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	index, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	governor := NewGovernor(index, nil)
	coordinator := newUnderstandingScanCoordinator(governor)
	now := time.Unix(1_791_000_000, 0)
	coordinator.now = func() time.Time { return now }
	retentionBacklog(t, governor, now, 1)
	settings := retentionTestSettings()
	enabled := true
	coordinator.retentionSettings = retentionSettingsFixed(settings, &enabled)
	other, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	holder, err := other.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	coordinator.retentionPass(context.Background(), settings)
	if skipped, pruned := coordinator.stats.pruneSkippedBusy.Load(), coordinator.stats.factsPruned.Load(); skipped != 1 || pruned != 0 {
		t.Fatalf("busy store: skipped=%d pruned=%d", skipped, pruned)
	}
	if candidates := coordinator.stats.pruneCandidates.Load(); candidates != 1 {
		t.Fatalf("a skipped generation left the backlog count at %d", candidates)
	}
	// A generation already retired whose row delete meets a busy store stays counted
	// as unfinished: the count is what "retention has drained" is read from.
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	candidates, err := index.PrunableUnderstandingGenerations(now.Add(-settings.keep).Unix(), now.Add(-settings.grace).UnixNano())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%v err=%v", candidates, err)
	}
	if pruned, err := index.PruneUnderstandingFacts(candidates[0], now.Add(-settings.keep).Unix(), now.Add(-settings.grace).UnixNano()); err != nil || !pruned {
		t.Fatalf("retire pruned=%v err=%v", pruned, err)
	}
	if holder, err = other.BeginGov(); err != nil {
		t.Fatal(err)
	}
	coordinator.retentionPass(context.Background(), settings)
	if unfinished := coordinator.stats.pruneUnfinished.Load(); unfinished != 1 {
		t.Fatalf("after a busy row delete unfinished=%d, want 1", unfinished)
	}
}

func TestUnderstandingCoordinatorRemeasuresAPrunedSnapshot(t *testing.T) {
	governor, coordinator := understandingCoordinatorFixture(t)
	now := time.Unix(1_791_000_000, 0)
	coordinator.now = func() time.Time { return now }
	work := understandingWorkFixture("checkout", 1, now.Unix(), "settled")
	generation := retentionCoordinatorGeneration(t, governor, work, now.Add(-60*24*time.Hour))
	later := understandingWorkFixture("checkout", 2, now.Unix(), "settled")
	retentionCoordinatorGeneration(t, governor, later, now.Add(-50*24*time.Hour))

	coordinator.offer(work)
	if len(coordinator.items) != 0 || coordinator.stats.coalesced.Load() != 1 {
		t.Fatalf("a present generation did not answer the offer: pending=%d", len(coordinator.items))
	}
	if pruned, err := governor.ix.PruneUnderstandingFacts(generation.ID, now.Unix(), now.UnixNano()); err != nil || !pruned {
		t.Fatalf("prune pruned=%v err=%v", pruned, err)
	}
	coordinator.offer(work)
	key := understandingCheckoutKey(work.Change.RepositoryID, work.Change.CheckoutID)
	queued, ok := coordinator.items[key]
	if !ok || !queued.remeasure {
		t.Fatalf("a pruned generation answered the offer: queued=%v work=%+v", ok, queued)
	}
	// A failed attempt on top of the pruned generation: inside the backoff the offer
	// waits; after it, the scan is still a re-measure of pruned history.
	delete(coordinator.items, key)
	failed := store.UnderstandingGeneration{RepositoryID: work.Change.RepositoryID,
		CheckoutID: work.Change.CheckoutID, CheckoutRoot: work.Change.CheckoutRoot,
		Status: "failed", SnapshotProtocol: "git-tree-v2", SnapshotDigest: work.Change.SnapshotDigest,
		StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: governor.analyzerAssembly.Digest(),
		ConventionState: "none", StartedAt: now.UnixNano() - 2, EndedAt: now.UnixNano() - 1,
		LimitationCode: "fixture", Limitation: "fixture"}
	if err := governor.ix.AppendUnderstanding(&failed); err != nil {
		t.Fatal(err)
	}
	coordinator.offer(work)
	if _, ok := coordinator.items[key]; ok {
		t.Fatal("a recent failure did not hold the offer back")
	}
	now = now.Add(2 * understandingFailureBackoff)
	coordinator.offer(work)
	if queued, ok := coordinator.items[key]; !ok || !queued.remeasure {
		t.Fatalf("after the backoff: queued=%v remeasure=%v", ok, queued.remeasure)
	}
}

func writeRetentionConfig(t *testing.T, dataDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dataDir, "daemon.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateConsoleConfig()
}

func TestUnderstandingRetentionConfigurationFailsClosed(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	settings, enabled := understandingRetentionSettingsNow()
	if !enabled || settings.keep != 14*24*time.Hour || settings.grace != 24*time.Hour ||
		settings.startDelay != 2*time.Minute || settings.interval != 30*time.Minute || settings.pause != 2*time.Second || settings.deleteRows != 10000 || settings.deleteDescriptors != 500 {
		t.Fatalf("shipped defaults: enabled=%v settings=%+v", enabled, settings)
	}

	writeRetentionConfig(t, dataDir, `{"understanding_retention":{"enabled":false}}`)
	if _, enabled := understandingRetentionSettingsNow(); enabled {
		t.Fatal("enabled:false did not stop retention")
	}

	// A horizon below the scan recovery window turns retention off, names the
	// problem, and leaves the rest of the file in force.
	writeRetentionConfig(t, dataDir, `{"edits_page_size":7,"understanding_retention":{"keep_days":0}}`)
	snapshot := consoleConfigSnapshot()
	if _, enabled := understandingRetentionSettingsNow(); enabled {
		t.Fatal("a horizon below the recovery window still prunes")
	}
	if snapshot.Config.EditsPageSize != 7 || len(snapshot.Problems) != 1 ||
		!strings.Contains(snapshot.Problems[0], "understanding_retention.keep_days") ||
		!strings.Contains(snapshot.Problems[0], "understanding retention is off") {
		t.Fatalf("section problem: origin=%q edits=%d problems=%v", snapshot.Origin, snapshot.Config.EditsPageSize, snapshot.Problems)
	}
	writeRetentionConfig(t, dataDir, `{"understanding_retention":{"intermediate_grace_hours":1}}`)
	if _, enabled := understandingRetentionSettingsNow(); enabled {
		t.Fatal("a grace below the recovery window still prunes")
	}

	// A file that stops loading keeps the last good value for everything else, but
	// never prunes: the value in force is not what the owner wrote.
	writeRetentionConfig(t, dataDir, `{"understanding_retention":{"enabled":true}}`)
	if _, enabled := understandingRetentionSettingsNow(); !enabled {
		t.Fatal("a valid file did not enable retention")
	}
	writeRetentionConfig(t, dataDir, `{"understanding_retention":`)
	if _, origin := consoleConfig(); origin != "invalid" && origin != "last-good" {
		t.Fatalf("broken file origin=%q", origin)
	}
	if _, enabled := understandingRetentionSettingsNow(); enabled {
		t.Fatal("a file that failed to load still prunes")
	}
}

// A daemon stopped between retiring a generation and deleting its rows leaves
// rows no reader resolves; the next pass finishes them before anything else.
func TestUnderstandingRetentionPassFinishesUnfinishedDeletes(t *testing.T) {
	governor, coordinator := understandingCoordinatorFixture(t)
	holdRetentionSettings(coordinator)
	now := time.Unix(1_791_000_000, 0)
	coordinator.now = func() time.Time { return now }
	old := understandingWorkFixture("checkout", 1, now.Add(-60*24*time.Hour).Unix(), "settled")
	generation := retentionCoordinatorGeneration(t, governor, old, now.Add(-60*24*time.Hour))
	later := understandingWorkFixture("checkout", 2, now.Unix(), "settled")
	retentionCoordinatorGeneration(t, governor, later, now.Add(-time.Hour))
	if pruned, err := governor.ix.PruneUnderstandingFacts(generation.ID, now.Unix(), now.UnixNano()); err != nil || !pruned {
		t.Fatalf("retire pruned=%v err=%v", pruned, err)
	}
	if leftovers, err := governor.ix.PrunedUnderstandingGenerationsWithFacts(); err != nil || len(leftovers) != 1 {
		t.Fatalf("fixture leftovers=%v err=%v", leftovers, err)
	}
	coordinator.retentionPass(context.Background(), retentionTestSettings())
	if leftovers, err := governor.ix.PrunedUnderstandingGenerationsWithFacts(); err != nil || len(leftovers) != 0 {
		t.Fatalf("leftovers after the pass=%v err=%v", leftovers, err)
	}
}

// The dangerous origin: a file broken at the daemon's FIRST load. The value in
// force is then the built-in defaults, which have retention on.
func TestUnderstandingRetentionStaysOffWhenTheFirstLoadFails(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	writeRetentionConfig(t, dataDir, `{"understanding_retention":`)
	config, origin := consoleConfig()
	if origin != "invalid" || !config.UnderstandingRetention.Enabled {
		t.Fatalf("fixture: origin=%q enabled=%v, want the defaults under origin invalid", origin, config.UnderstandingRetention.Enabled)
	}
	if _, enabled := understandingRetentionSettingsNow(); enabled {
		t.Fatal("a daemon.json that never loaded turned retention on")
	}
}

// Every accepted value must become a positive duration: an unbounded keep_days
// wraps negative and would prune what it was meant to keep forever.
func TestUnderstandingRetentionBoundsKeepDurationsPositive(t *testing.T) {
	largest := ConsoleUnderstandingRetention{Enabled: true, KeepDays: understandingRetentionKeepDaysMax,
		IntermediateGraceHours: understandingRetentionKeepDaysMax * 24,
		PassIntervalSeconds:    understandingRetentionIntervalSecondsMax,
		PauseMS:                understandingRetentionPauseMSMax, DeleteRowsPerTransaction: understandingRetentionDeleteRowsMax,
		StartDelaySeconds: understandingRetentionIntervalSecondsMax, DeleteDescriptorsPerTransaction: understandingRetentionDeleteRowsMax}
	if err := largest.validate(); err != nil {
		t.Fatalf("the largest values are refused: %v", err)
	}
	keep := time.Duration(largest.KeepDays) * 24 * time.Hour
	grace := time.Duration(largest.IntermediateGraceHours) * time.Hour
	if keep <= 0 || grace <= 0 || time.Duration(largest.PassIntervalSeconds)*time.Second <= 0 {
		t.Fatalf("largest accepted values overflow: keep=%s grace=%s", keep, grace)
	}
	for name, mutate := range map[string]func(*ConsoleUnderstandingRetention){
		"keep_days forever":       func(r *ConsoleUnderstandingRetention) { r.KeepDays = 999999 },
		"grace too large":         func(r *ConsoleUnderstandingRetention) { r.IntermediateGraceHours = 3_000_000 },
		"interval negative":       func(r *ConsoleUnderstandingRetention) { r.PassIntervalSeconds = -1 },
		"interval too large":      func(r *ConsoleUnderstandingRetention) { r.PassIntervalSeconds = 1 << 40 },
		"pause too large":         func(r *ConsoleUnderstandingRetention) { r.PauseMS = 1 << 40 },
		"delete budget zero":      func(r *ConsoleUnderstandingRetention) { r.DeleteRowsPerTransaction = 0 },
		"delete budget too large": func(r *ConsoleUnderstandingRetention) { r.DeleteRowsPerTransaction = 1 << 40 },
		"start delay negative":    func(r *ConsoleUnderstandingRetention) { r.StartDelaySeconds = -1 },
		"descriptor budget zero":  func(r *ConsoleUnderstandingRetention) { r.DeleteDescriptorsPerTransaction = 0 },
	} {
		value := defaultConsoleConfig().UnderstandingRetention
		mutate(&value)
		if err := value.validate(); err == nil {
			t.Fatalf("%s was accepted: %+v", name, value)
		}
	}
}

// The first pass follows the start delay, not the interval, and later passes follow
// the interval; a disabled configuration runs none.
func TestUnderstandingRetentionRunsAfterTheStartDelayThenEachInterval(t *testing.T) {
	governor, coordinator := understandingCoordinatorFixture(t)
	now := time.Unix(1_791_000_000, 0)
	coordinator.now = func() time.Time { return now }
	retentionBacklog(t, governor, now, 1)
	settings := retentionTestSettings()
	settings.startDelay, settings.interval = 20*time.Millisecond, time.Hour
	enabled := false
	coordinator.retentionSettings = retentionSettingsFixed(settings, &enabled)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coordinator.runRetention(ctx); close(done) }()
	time.Sleep(200 * time.Millisecond)
	if coordinator.stats.retentionLastPass.Load() != 0 {
		t.Fatal("a disabled configuration ran a pass")
	}
	cancel()
	<-done

	enabled = true
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go coordinator.runRetention(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for coordinator.stats.factsPruned.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no pass ran after the start delay, with a one-hour interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
