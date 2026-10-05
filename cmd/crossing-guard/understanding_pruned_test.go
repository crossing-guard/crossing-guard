package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"crossing-guard/store"
)

func showPrunedOutput(t *testing.T, generation store.UnderstandingGeneration, compared, jsonOutput bool) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = writer
	code := showPrunedUnderstanding("/store/index.sqlite", generation, compared, jsonOutput)
	os.Stdout = stdout
	_ = writer.Close()
	body, err := io.ReadAll(reader)
	if err != nil || code != 0 {
		t.Fatalf("exit=%d err=%v", code, err)
	}
	return string(body)
}

// A generation whose facts retention removed is shown as that, with what the scan
// measured, and never as a scan that measured nothing.
func TestShowPrunedUnderstandingNamesRetentionAndReadsNoRows(t *testing.T) {
	generation := store.UnderstandingGeneration{ID: 7, Status: "complete",
		SnapshotDigest: "git-tree-v2-sha256:tree", ConventionState: "none",
		UnitTotal: 5, EdgeTotal: 9, FactsState: store.UnderstandingFactsPruned}

	var view understandingShow
	if err := json.Unmarshal([]byte(showPrunedOutput(t, generation, true, true)), &view); err != nil {
		t.Fatal(err)
	}
	if view.Generation.FactsState != store.UnderstandingFactsPruned || view.Generation.UnitTotal != 5 ||
		view.Units == nil || len(view.Units) != 0 || len(view.Edges) != 0 || len(view.Coverage) != 0 {
		t.Fatalf("pruned generation body: %+v", view)
	}
	if view.CandidateState != "unavailable" || !strings.Contains(view.CandidateReason, "removed by retention") {
		t.Fatalf("compare over a pruned generation: state=%q reason=%q", view.CandidateState, view.CandidateReason)
	}
	if plain := showPrunedOutput(t, generation, false, true); strings.Contains(plain, "candidate_state") {
		t.Fatalf("candidate state reported without --compare: %s", plain)
	}

	text := showPrunedOutput(t, generation, false, false)
	for _, want := range []string{"generation 7", "analysis facts were removed by retention", "5 units and 9 edges", "store: /store/index.sqlite"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text output lacks %q:\n%s", want, text)
		}
	}
}
