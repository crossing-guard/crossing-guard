package daemon

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// The console's WATER_ORDER is a copy of the engine ladder for chip colours; it must
// not drift from engine.WaterOrder (data-class-personal-ladder-plan.md §5).
func TestConsoleWaterOrderMirrorsTheEngineLadder(t *testing.T) {
	core, err := staticFS.ReadFile("static/js/core.js")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)const WATER_ORDER = \[(.*?)\];`).FindSubmatch(core)
	if block == nil {
		t.Fatal("could not find WATER_ORDER in core.js")
	}
	var console []string
	for _, m := range regexp.MustCompile(`'([^']*)'`).FindAllSubmatch(block[1], -1) {
		console = append(console, string(m[1]))
	}
	if !slices.Equal(console, engine.WaterOrder()) {
		t.Fatalf("console WATER_ORDER %v != engine ladder %v", console, engine.WaterOrder())
	}
}

// A store folded by a build that emitted the legacy "personal" spelling is repaired at
// start, after which a stateful rule on the ladder spelling fires on that session.
func TestLegacyPersonalFoldIsRepairedAndGates(t *testing.T) {
	statefulTestPolicy(t) // session:data-class=personal-data ∧ command EGRESS_CANARY
	g := statefulGovernor(t)
	tx, err := g.ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.UpsertSessionState("old", store.StateRow{Key: engine.DataClassKey, Value: "personal",
		Detector: "data.email", Provenance: "observed"}, 10); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	egress := Observation{SessionID: "old", Tool: "Bash", Command: "run EGRESS_CANARY now", TS: 20}

	// Before the repair the matcher already reads the legacy fold through the alias.
	if d, _, err := g.DecideStateful(egress); err != nil || d == nil || d.Rule != "no-egress-after-personal" {
		t.Fatalf("alias-aware match before repair: %+v %v", d, err)
	}

	if problem := repairLegacyDataClassFolds(g.ix); problem != "" {
		t.Fatalf("repair reported a problem: %s", problem)
	}
	state, err := g.ix.SessionState("old")
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || state[0].Value != "personal-data" {
		t.Fatalf("fold not repaired: %+v", state)
	}
	if d, _, err := g.DecideStateful(egress); err != nil || d == nil || d.Rule != "no-egress-after-personal" {
		t.Fatalf("stateful rule must fire after repair: %+v %v", d, err)
	}

	// A new observation folds the ladder spelling into the same row, not a second one.
	if err := g.Observe(Observation{SessionID: "old", Tool: "Write", Content: "alice@example.com", TS: 30, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	state, _ = g.ix.SessionState("old")
	var classes []string
	for _, s := range state {
		if s.Key == engine.DataClassKey {
			classes = append(classes, s.Value)
		}
	}
	if !slices.Equal(classes, []string{"personal-data"}) {
		t.Fatalf("one canonical data-class row expected: %v", classes)
	}
	for _, s := range state {
		if s.Key == engine.DataClassKey && (s.FirstSeen != 10 || s.LastSeen != 30) {
			t.Fatalf("the new observation must merge into the repaired row (10..30): %+v", s)
		}
	}
}

// A failed repair is not fatal and is reported on the governor health (postwork PW-2).
func TestFoldRepairFailureIsReportedOnHealth(t *testing.T) {
	g := statefulGovernor(t)
	tx, err := g.ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.UpsertSessionState("old", store.StateRow{Key: engine.DataClassKey, Value: "personal",
		Detector: "data.email", Provenance: "observed"}, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`CREATE TRIGGER block_repair BEFORE DELETE ON session_state
		BEGIN SELECT RAISE(ABORT,'repair blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	problem := repairLegacyDataClassFolds(g.ix)
	if !strings.Contains(problem, "repair blocked") {
		t.Fatalf("failure must be returned for health: %q", problem)
	}
	g.foldRepairError = problem
	if h, err := g.Health(); err != nil || h.FoldRepairError != problem {
		t.Fatalf("health must carry the repair failure: %+v", h)
	}
	state, _ := g.ix.SessionState("old")
	if len(state) != 1 || state[0].Value != "personal" {
		t.Fatalf("a failed repair leaves the fold untouched: %+v", state)
	}
}
