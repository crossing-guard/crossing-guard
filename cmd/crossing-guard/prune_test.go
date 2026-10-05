package main

import (
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/store"
)

func TestParsePruneArgsSelectsUsageAndKeepsSeconds(t *testing.T) {
	request := parsePruneArgs([]string{"--usage", "--before", "2026-09-01", "--yes"})
	want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	if !request.usage || !request.confirm || request.cutoff != want {
		t.Fatalf("request: %+v, want cutoff %d", request, want)
	}
	if plain := parsePruneArgs([]string{"--keep-days", "3"}); plain.usage || plain.confirm {
		t.Fatalf("plain prune trims events only: %+v", plain)
	}
}

// Pruning with nothing older recorded yet must still hold the horizon: the
// backfill reads newest first, so older calls may arrive later (C-4).
func TestPruneUsageRecordsTheHorizonOnAnEmptyStore(t *testing.T) {
	index, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := pruneUsage(index, pruneRequest{cutoff: cutoff.Unix(), confirm: true, usage: true}); err != nil {
		t.Fatal(err)
	}
	horizon, err := index.UsagePruneHorizon()
	if err != nil || horizon != cutoff.UnixMilli() {
		t.Fatalf("horizon = %d, want %d (milliseconds) err=%v", horizon, cutoff.UnixMilli(), err)
	}
	older := int64(cutoff.UnixMilli() - 1)
	one := int64(1)
	result, err := index.WriteUsageSource(store.UsageSourceWrite{
		State: store.UsageSourceState{Runtime: "rt", Source: "src", SessionID: "s", Marker: "m", Reader: "r",
			Complete: true, UpdatedAtMS: 1},
		Calls: []store.UsageCallRecord{{Runtime: "rt", CallID: "c", SessionID: "s", Source: "src",
			FirstAtMS: older, AtMS: older, Output: &one, Reader: "r"}}})
	if err != nil || result.BelowHorizon != 1 || result.Written != 0 {
		t.Fatalf("an older call arriving later is dropped: %+v err=%v", result, err)
	}
	// A dry run never moves the horizon.
	if err := pruneUsage(index, pruneRequest{cutoff: cutoff.Add(48 * time.Hour).Unix(), usage: true}); err != nil {
		t.Fatal(err)
	}
	if again, _ := index.UsagePruneHorizon(); again != cutoff.UnixMilli() {
		t.Fatalf("a dry run moved the horizon to %d", again)
	}
}
