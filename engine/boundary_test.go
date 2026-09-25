package engine

// P-COMPILE-2 — the honest label computed mechanically (implementation-plan
// P4 gate): boundary = detection-coverage ∧ reach; a rule leaning on
// unknowable-gap detectors renders precautionary, NEVER confirmed-complete.

import (
	"strings"
	"testing"
)

func compileFixture() ([]Detector, *Policy) {
	dets := []Detector{
		{ID: "srcmap:orders", Kind: "source", Tag: tagSpec{Key: "data-class", Value: "personal-data"},
			Coverage: Coverage{Enumerable: true, Gaps: []string{"unmapped MCP servers are invisible"}}},
		{ID: "dest:allowlist", Kind: "destination",
			Coverage: Coverage{Enumerable: true, Gaps: []string{"hostname≠resolved IP at the hook seam"}}},
		{ID: "content:strategy", Kind: "content", Keywords: []string{"margin"},
			Tag:      tagSpec{Key: "data-class", Value: "business-intel"},
			Coverage: Coverage{Enumerable: false}},
	}
	pol := &Policy{Rules: []Rule{
		// the P-COMPILE-2 authored rule: IF PII AND endpoint-not-compliant THEN stop
		{ID: "pii-to-external", Mode: HardBlock, Message: "PII to a non-compliant endpoint",
			If: Predicate{All: []Predicate{
				{Tag: "data-class", Value: "personal-data"},
				{Tag: "destination-class", Value: "external"},
			}}},
		// leans ONLY on a content detector — must render precautionary
		{ID: "strategy-leak", Mode: ConfirmAndRecord, Message: "business intel leaving",
			If: Predicate{Tag: "data-class", Value: "business-intel"}},
		// mixes enumerable and unknowable terms — the weakest term governs
		{ID: "mixed", Mode: WarnAndProceed,
			If: Predicate{All: []Predicate{
				{Tag: "data-class", Value: "personal-data"},
				{Tag: "data-class", Value: "business-intel"},
			}}},
		// references a tag nothing produces — must be labeled inert
		{ID: "ghost", Mode: HardBlock,
			If: Predicate{Tag: "data-class", Value: "radioactive"}},
		// an ANY where one branch is undetectable: still fires via the other
		{ID: "either", Mode: WarnAndProceed,
			If: Predicate{Any: []Predicate{
				{Tag: "data-class", Value: "radioactive"},
				{Tag: "data-class", Value: "personal-data"},
			}}},
	}}
	return dets, pol
}

func TestBoundaryEnumerableRule(t *testing.T) {
	dets, pol := compileFixture()
	b := Boundary(pol.Rules[0], dets, ReachStop)
	if b.DetectionLabel != "enumerable (declared gaps)" || !b.CanFire {
		t.Fatalf("pii-to-external: %+v", b)
	}
	if len(b.Detection) != 2 {
		t.Fatalf("expected 2 term coverages: %+v", b.Detection)
	}
	// the destination term is produced by the dynamic-value destination detector
	if b.Detection[1].Undetectable || b.Detection[1].Detectors[0] != "dest:allowlist" {
		t.Fatalf("destination term: %+v", b.Detection[1])
	}
	// declared gaps surface per term
	if len(b.Detection[0].Gaps) == 0 || !strings.Contains(b.Detection[0].Gaps[0], "unmapped") {
		t.Fatalf("gaps not surfaced: %+v", b.Detection[0])
	}
	if !strings.Contains(b.Label, ReachStop) {
		t.Fatalf("reach missing from label: %q", b.Label)
	}
}

func TestBoundaryContentRuleIsPrecautionaryNeverComplete(t *testing.T) {
	dets, pol := compileFixture()
	for _, reach := range []string{ReachStop, ReachHarvest} {
		b := Boundary(pol.Rules[1], dets, reach)
		if b.DetectionLabel != "coverage-limited (precautionary)" {
			t.Fatalf("strategy-leak label: %+v", b)
		}
		if strings.Contains(strings.ToLower(b.Label), "complete") &&
			!strings.Contains(b.Label, "NOT claimed") {
			t.Fatalf("label claims completeness: %q", b.Label)
		}
	}
	// the closed vocabulary can never say confirmed-complete
	for _, r := range pol.Rules {
		b := Boundary(r, dets, ReachStop)
		if strings.Contains(b.DetectionLabel, "confirmed") || strings.Contains(b.DetectionLabel, "complete") {
			t.Fatalf("forbidden claim in %s: %q", r.ID, b.DetectionLabel)
		}
	}
}

func TestBoundaryWeakestTermGoverns(t *testing.T) {
	dets, pol := compileFixture()
	if b := Boundary(pol.Rules[2], dets, ReachStop); b.DetectionLabel != "coverage-limited (precautionary)" {
		t.Fatalf("mixed: %+v", b)
	}
}

func TestBoundaryInertAndAnyBranches(t *testing.T) {
	dets, pol := compileFixture()
	if b := Boundary(pol.Rules[3], dets, ReachStop); b.CanFire || b.DetectionLabel != "inert (cannot fire)" {
		t.Fatalf("ghost: %+v", b)
	}
	// ANY with one dead branch still fires via the live one, and stays honest
	if b := Boundary(pol.Rules[4], dets, ReachStop); !b.CanFire || b.DetectionLabel != "enumerable (declared gaps)" {
		t.Fatalf("either: %+v", b)
	}
	// a NOT over an unproduced tag is vacuous, not inert — and says so
	not := Rule{ID: "not-ghost", If: Predicate{Not: &Predicate{Tag: "data-class", Value: "radioactive"}}}
	if b := Boundary(not, dets, ReachStop); !b.CanFire || b.DetectionLabel != "vacuous (no detection involved)" {
		t.Fatalf("not-ghost should be vacuously fireable: %+v", b)
	}
}

// The De Morgan regressions (red-team findings): the label must agree with
// Match reachability under negated combinators, in both failure directions.
func TestBoundaryDeMorgan(t *testing.T) {
	dets, _ := compileFixture()
	ghost := Predicate{Tag: "data-class", Value: "radioactive"}
	a := Predicate{Tag: "data-class", Value: "personal-data"}

	// ¬(¬ghost ∧ a) ≡ ¬a — fires on any event where a is absent. The old
	// fold labeled this INERT while Match returned true on an empty tag set.
	firesOnAbsence := Rule{ID: "not-all", If: Predicate{Not: &Predicate{All: []Predicate{{Not: &ghost}, a}}}}
	b := Boundary(firesOnAbsence, dets, ReachStop)
	if !b.CanFire {
		t.Fatalf("inert-but-fires: %+v (Match(empty)=%v)", b, Match(firesOnAbsence.If, nil))
	}
	if !Match(firesOnAbsence.If, nil) {
		t.Fatal("premise broken: predicate should match the empty tag set")
	}

	// ¬(¬ghost ∨ a) ≡ ghost ∧ ¬a — unsatisfiable (ghost is unproducible).
	// The old fold labeled this fireable + enumerable: the bluff direction.
	dead := Rule{ID: "not-any", If: Predicate{Not: &Predicate{Any: []Predicate{{Not: &ghost}, a}}}}
	if b := Boundary(dead, dets, ReachStop); b.CanFire {
		t.Fatalf("fireable-but-dead: %+v", b)
	}

	// double negation restores polarity
	dbl := Rule{ID: "dbl", If: Predicate{Not: &Predicate{Not: &ghost}}}
	if b := Boundary(dbl, dets, ReachStop); b.CanFire {
		t.Fatalf("double-negated ghost should be inert: %+v", b)
	}
}

func TestBoundaryZeroAndVacuousPredicates(t *testing.T) {
	dets, _ := compileFixture()
	// the zero predicate matches nothing at runtime — must be inert
	if b := Boundary(Rule{ID: "blank", If: Predicate{}}, dets, ReachStop); b.CanFire {
		t.Fatalf("empty predicate labeled fireable: %+v", b)
	}
	// negated zero predicate matches everything — vacuous, fireable
	if b := Boundary(Rule{ID: "not-blank", If: Predicate{Not: &Predicate{}}}, dets, ReachStop); !b.CanFire || b.DetectionLabel != "vacuous (no detection involved)" {
		t.Fatalf("negated empty predicate: %+v", b)
	}
	// a vacuous-only conjunction fires unconditionally and must not claim
	// detector backing
	ghost1 := Predicate{Tag: "x", Value: "1"}
	ghost2 := Predicate{Tag: "x", Value: "2"}
	vac := Rule{ID: "vac", If: Predicate{All: []Predicate{{Not: &ghost1}, {Not: &ghost2}}}}
	if b := Boundary(vac, dets, ReachStop); !b.CanFire || b.DetectionLabel != "vacuous (no detection involved)" {
		t.Fatalf("vacuous conjunction: %+v", b)
	}
	// a vacuous branch inside ANY dominates: the rule fires unconditionally
	anyVac := Rule{ID: "any-vac", If: Predicate{Any: []Predicate{{Not: &ghost1}, {Tag: "data-class", Value: "business-intel"}}}}
	if b := Boundary(anyVac, dets, ReachStop); b.DetectionLabel != "vacuous (no detection involved)" {
		t.Fatalf("any-with-vacuous-branch: %+v", b)
	}
}

func TestBoundaryProducerEdgeCases(t *testing.T) {
	dets, _ := compileFixture()
	// destination-class only ever resolves to in-house|external — a typo'd
	// value has no producer and must be inert, not "enumerable"
	typo := Rule{ID: "typo", If: Predicate{Tag: "destination-class", Value: "internal"}}
	if b := Boundary(typo, dets, ReachStop); b.CanFire {
		t.Fatalf("impossible destination value labeled fireable: %+v", b)
	}
	// a detector that can never fire (pattern with no regex) is not a producer
	deadDet := append([]Detector{}, dets...)
	deadDet = append(deadDet, Detector{ID: "pattern:empty", Kind: "pattern",
		Tag: tagSpec{Key: "data-class", Value: "ssn"}, Coverage: Coverage{Enumerable: false}})
	ssn := Rule{ID: "ssn", If: Predicate{Tag: "data-class", Value: "ssn"}}
	if b := Boundary(ssn, deadDet, ReachStop); b.CanFire {
		t.Fatalf("regex-less pattern detector counted as producer: %+v", b)
	}
}

func TestCompileBoundariesCoversPolicy(t *testing.T) {
	dets, pol := compileFixture()
	bs := CompileBoundaries(pol, dets, ReachHarvest)
	if len(bs) != len(pol.Rules) {
		t.Fatalf("want %d boundaries, got %d", len(pol.Rules), len(bs))
	}
	for _, b := range bs {
		if b.Reach != ReachHarvest {
			t.Fatalf("reach not threaded: %+v", b)
		}
	}
}

// TestCommandGuardsAreNotLabelledInert pins a false label the compiler used to
// produce about the rules it ships with.
//
// The command tag is injected by the hook at decision time (CommandTagKey), not
// emitted by a detector. Counting that as "no producing detector" made every
// shipped command guard compile to INERT — "this rule can never fire" — about
// rules that block things every day. A false INERT is worse than no label at all:
// it tells a user their guards are dead when they are live.
func TestCommandGuardsAreNotLabelledInert(t *testing.T) {
	r := Rule{ID: "destructive-rm", Action: "deny",
		If: Predicate{Tag: CommandTagKey, Matches: `rm\s+-rf\b`}}
	b := Boundary(r, nil, ReachStop) // NO detectors: the command tag needs none

	if !b.CanFire {
		t.Fatal("a command guard must be able to fire — it is what the hook enforces")
	}
	if strings.Contains(b.Label, "INERT") {
		t.Errorf("label = %q, must not claim a live rule can never fire", b.Label)
	}
	// ...and it must not overclaim in the other direction either: free-text regex
	// matching can never be enumerable.
	for _, term := range b.Detection {
		if term.Tag != CommandTagKey {
			continue
		}
		if term.Undetectable {
			t.Error("the command tag IS produced — by the hook, at decision time")
		}
		if term.Enumerable {
			t.Error("a regex over free command text can never claim completeness")
		}
		if len(term.Detectors) == 0 {
			t.Error("the producer must be NAMED, or the reader cannot tell where the value comes from")
		}
	}
}

func TestExactToolGuardsNameTheirEnumerableHookProducer(t *testing.T) {
	r := Rule{ID: "deny-publication", Action: "deny",
		If: Predicate{Tag: ToolTagKey, Value: "Artifact"}}
	b := Boundary(r, nil, ReachStop)
	if !b.CanFire || strings.Contains(b.Label, "INERT") {
		t.Fatalf("exact tool guard mislabeled: %+v", b)
	}
	if len(b.Detection) != 1 {
		t.Fatalf("detection=%+v", b.Detection)
	}
	term := b.Detection[0]
	if !term.Enumerable || term.Undetectable || len(term.Detectors) != 1 ||
		!strings.Contains(term.Detectors[0], "exact bare tool identity") {
		t.Fatalf("exact tool coverage=%+v", term)
	}
}

// A user overlay may legitimately declare a detector that emits `command` tags.
// The first carve-out for the hook's eval-only producer SHORT-CIRCUITED before
// producersFor, so such a detector vanished from the coverage report — invisible
// on the one surface whose job is saying who produces what. Both producers must
// be listed.
func TestUserCommandDetectorIsNotMaskedByTheHookProducer(t *testing.T) {
	userDet := Detector{ID: "custom.command", Kind: "content",
		Keywords: []string{"sudo"}, // content detectors without keywords produce nothing
		Tag:      tagSpec{Key: CommandTagKey, Value: ""},
		Coverage: Coverage{Enumerable: true, Gaps: []string{"only exact strings"}}}
	r := Rule{ID: "x", Action: "deny", If: Predicate{Tag: CommandTagKey, Matches: "x"}}
	b := Boundary(r, []Detector{userDet}, ReachStop)

	var term *TermCoverage
	for i := range b.Detection {
		if b.Detection[i].Tag == CommandTagKey {
			term = &b.Detection[i]
		}
	}
	if term == nil {
		t.Fatal("no coverage row for the command term")
	}
	sawUser, sawHook := false, false
	for _, d := range term.Detectors {
		if d == "custom.command" {
			sawUser = true
		}
		if d == evalOnlyCommandProducer {
			sawHook = true
		}
	}
	if !sawUser {
		t.Error("the user's own command detector is missing from the producer list")
	}
	if !sawHook {
		t.Error("the hook's eval-only producer must still be named")
	}
}
