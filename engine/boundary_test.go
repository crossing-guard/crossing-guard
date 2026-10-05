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
	b := Boundary(pol.Rules[0], dets, known, ReachStop)
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
		b := Boundary(pol.Rules[1], dets, known, reach)
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
		b := Boundary(r, dets, known, ReachStop)
		if strings.Contains(b.DetectionLabel, "confirmed") || strings.Contains(b.DetectionLabel, "complete") {
			t.Fatalf("forbidden claim in %s: %q", r.ID, b.DetectionLabel)
		}
	}
}

func TestBoundaryWeakestTermGoverns(t *testing.T) {
	dets, pol := compileFixture()
	if b := Boundary(pol.Rules[2], dets, known, ReachStop); b.DetectionLabel != "coverage-limited (precautionary)" {
		t.Fatalf("mixed: %+v", b)
	}
}

func TestBoundaryInertAndAnyBranches(t *testing.T) {
	dets, pol := compileFixture()
	if b := Boundary(pol.Rules[3], dets, known, ReachStop); b.CanFire || b.DetectionLabel != "inert (cannot fire)" {
		t.Fatalf("ghost: %+v", b)
	}
	// ANY with one dead branch still fires via the live one, and stays honest
	if b := Boundary(pol.Rules[4], dets, known, ReachStop); !b.CanFire || b.DetectionLabel != "enumerable (declared gaps)" {
		t.Fatalf("either: %+v", b)
	}
	// a NOT over an unproduced tag is vacuous, not inert — and says so
	not := Rule{ID: "not-ghost", If: Predicate{Not: &Predicate{Tag: "data-class", Value: "radioactive"}}}
	if b := Boundary(not, dets, known, ReachStop); !b.CanFire || b.DetectionLabel != "vacuous (no detection involved)" {
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
	b := Boundary(firesOnAbsence, dets, known, ReachStop)
	if !b.CanFire {
		t.Fatalf("inert-but-fires: %+v (Match(empty)=%v)", b, Match(firesOnAbsence.If, nil))
	}
	if !Match(firesOnAbsence.If, nil) {
		t.Fatal("premise broken: predicate should match the empty tag set")
	}

	// ¬(¬ghost ∨ a) ≡ ghost ∧ ¬a — unsatisfiable (ghost is unproducible).
	// The old fold labeled this fireable + enumerable: the bluff direction.
	dead := Rule{ID: "not-any", If: Predicate{Not: &Predicate{Any: []Predicate{{Not: &ghost}, a}}}}
	if b := Boundary(dead, dets, known, ReachStop); b.CanFire {
		t.Fatalf("fireable-but-dead: %+v", b)
	}

	// double negation restores polarity
	dbl := Rule{ID: "dbl", If: Predicate{Not: &Predicate{Not: &ghost}}}
	if b := Boundary(dbl, dets, known, ReachStop); b.CanFire {
		t.Fatalf("double-negated ghost should be inert: %+v", b)
	}
}

func TestBoundaryZeroAndVacuousPredicates(t *testing.T) {
	dets, _ := compileFixture()
	// the zero predicate matches nothing at runtime — must be inert
	if b := Boundary(Rule{ID: "blank", If: Predicate{}}, dets, known, ReachStop); b.CanFire {
		t.Fatalf("empty predicate labeled fireable: %+v", b)
	}
	// negated zero predicate matches everything — vacuous, fireable
	if b := Boundary(Rule{ID: "not-blank", If: Predicate{Not: &Predicate{}}}, dets, known, ReachStop); !b.CanFire || b.DetectionLabel != "vacuous (no detection involved)" {
		t.Fatalf("negated empty predicate: %+v", b)
	}
	// a vacuous-only conjunction fires unconditionally and must not claim
	// detector backing
	ghost1 := Predicate{Tag: "x", Value: "1"}
	ghost2 := Predicate{Tag: "x", Value: "2"}
	vac := Rule{ID: "vac", If: Predicate{All: []Predicate{{Not: &ghost1}, {Not: &ghost2}}}}
	if b := Boundary(vac, dets, known, ReachStop); !b.CanFire || b.DetectionLabel != "vacuous (no detection involved)" {
		t.Fatalf("vacuous conjunction: %+v", b)
	}
	// a vacuous branch inside ANY dominates: the rule fires unconditionally
	anyVac := Rule{ID: "any-vac", If: Predicate{Any: []Predicate{{Not: &ghost1}, {Tag: "data-class", Value: "business-intel"}}}}
	if b := Boundary(anyVac, dets, known, ReachStop); b.DetectionLabel != "vacuous (no detection involved)" {
		t.Fatalf("any-with-vacuous-branch: %+v", b)
	}
}

func TestBoundaryProducerEdgeCases(t *testing.T) {
	dets, _ := compileFixture()
	// destination-class only ever resolves to in-house|external — a typo'd
	// value has no producer and must be inert, not "enumerable"
	typo := Rule{ID: "typo", If: Predicate{Tag: "destination-class", Value: "internal"}}
	if b := Boundary(typo, dets, known, ReachStop); b.CanFire {
		t.Fatalf("impossible destination value labeled fireable: %+v", b)
	}
	// a detector that can never fire (pattern with no regex) is not a producer
	deadDet := append([]Detector{}, dets...)
	deadDet = append(deadDet, Detector{ID: "pattern:empty", Kind: "pattern",
		Tag: tagSpec{Key: "data-class", Value: "ssn"}, Coverage: Coverage{Enumerable: false}})
	ssn := Rule{ID: "ssn", If: Predicate{Tag: "data-class", Value: "ssn"}}
	if b := Boundary(ssn, deadDet, known, ReachStop); b.CanFire {
		t.Fatalf("regex-less pattern detector counted as producer: %+v", b)
	}
}

func TestCompileBoundariesCoversPolicy(t *testing.T) {
	dets, pol := compileFixture()
	bs := CompileBoundaries(pol, dets, known, ReachHarvest)
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
	b := Boundary(r, nil, known, ReachStop) // NO detectors: the command tag needs none

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
	b := Boundary(r, nil, known, ReachStop)
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
		Tag:      tagSpec{Key: CommandTagKey, Value: "sudo"},
		Coverage: Coverage{Enumerable: true, Gaps: []string{"only exact strings"}}}
	// The pattern must accept the value the detector emits, or it is not a producer of
	// this term at all (the matches filter, stateful-rule-coverage plan D-4b).
	r := Rule{ID: "x", Action: "deny", If: Predicate{Tag: CommandTagKey, Matches: "sudo"}}
	b := Boundary(r, []Detector{userDet}, known, ReachStop)

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

// stateChannelFixture is one detector per reach class: a tool_call-only session fact, a
// role-less resource fact, and an assistant-only chat fact.
func stateChannelFixture() []Detector {
	return []Detector{
		{ID: "vcs.push.force", Kind: "pattern", Regex: "push --force", Roles: []string{"tool_call"},
			Tag: tagSpec{Key: "vcs", Value: "push-force"}, Coverage: Coverage{Enumerable: true}},
		{ID: "secret.any", Kind: "pattern", Regex: "AKIA", Scope: "resource",
			Tag: tagSpec{Key: "secret", Value: "aws-key"}, Coverage: Coverage{Gaps: []string{"novel"}}},
		{ID: "agent.pushback", Kind: "content", Keywords: []string{"no"}, Roles: []string{"assistant"},
			Tag: tagSpec{Key: "agent", Value: "pushback"}, Coverage: Coverage{Gaps: []string{"phrasing"}}},
	}
}

// A state term is produced by its bare key's detectors, restricted to those whose facts
// reach that fold on the rule's channel (stateful-rule-coverage plan D-4). Before the
// fix every session:/target: term was UNDETECTABLE, so every stateful rule read INERT.
func TestStateTermProducersFollowTheChannel(t *testing.T) {
	dets := stateChannelFixture()
	cases := []struct {
		reach, tag, value string
		want              []string // producer ids; nil = undetectable
	}{
		{ReachStop, "session:vcs", "push-force", []string{"vcs.push.force"}},
		{ReachStop, "target:vcs", "push-force", nil},                    // not resource-scoped
		{ReachStop, "target:secret", "aws-key", []string{"secret.any"}}, // resource-scoped
		{ReachStop, "session:agent", "pushback", nil},                   // assistant-only never folds live
		{ReachStop, "agent", "pushback", nil},                           // the same on a bare live term
		{ReachStop, "session:command", "", nil},                         // eval-only, never folded
		{ReachStop, "session:tool", "", nil},
		{ReachStop, "session:secret", "nonexistent", nil},
		{ReachHarvest, "session:agent", "pushback", []string{"agent.pushback"}},
		{ReachHarvest, "target:secret", "aws-key", nil}, // the audit never exposes target:
		{ReachDryRun, "agent", "pushback", []string{"agent.pushback"}},
	}
	for _, c := range cases {
		r := Rule{ID: "r", Action: "deny", If: Predicate{Tag: c.tag, Value: c.value}}
		b := Boundary(r, dets, known, c.reach)
		if len(b.Detection) != 1 {
			t.Fatalf("%s %s: %d coverage rows", c.reach, c.tag, len(b.Detection))
		}
		tc := b.Detection[0]
		if c.want == nil {
			if !tc.Undetectable || b.CanFire {
				t.Errorf("%s %s=%s: want undetectable/inert, got %+v can_fire=%v", c.reach, c.tag, c.value, tc, b.CanFire)
			}
			continue
		}
		if tc.Undetectable || strings.Join(tc.Detectors, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s %s=%s: producers %v, want %v", c.reach, c.tag, c.value, tc.Detectors, c.want)
		}
	}
}

// A matches term keeps a producer only when the pattern accepts a value that producer
// can emit — the same test the value branch applies (D-4b).
func TestMatchesTermFiltersProducers(t *testing.T) {
	dets := []Detector{
		{ID: "data.email", Kind: "pattern", Regex: "@", Tag: tagSpec{Key: "data-class", Value: "personal-data"},
			Coverage: Coverage{Gaps: []string{"obfuscated"}}},
		{ID: "net.dest", Kind: "destination", Coverage: Coverage{Enumerable: true}},
	}
	cases := []struct {
		tag, matches string
		canFire      bool
	}{
		{"session:data-class", "^credential", false},
		{"session:data-class", "^personal", true},
		{"session:destination-class", "^ext", true},
		{"session:destination-class", "^nowhere$", false},
		{"session:data-class", "(", false}, // uncompilable matches nothing, as in the evaluator
	}
	for _, c := range cases {
		r := Rule{ID: "r", Action: "deny", If: Predicate{Tag: c.tag, Matches: c.matches}}
		if got := Boundary(r, dets, known, ReachStop).CanFire; got != c.canFire {
			t.Errorf("%s ~ %q: can_fire=%v, want %v", c.tag, c.matches, got, c.canFire)
		}
	}
}

// The label says what the rule does. A report-only stateful rule never acts live — the
// stateful tier drops a verdict that does not gate — so it is labeled at harvest reach
// and never claims to block (D-4c).
func TestLabelFollowsTheEffectiveMode(t *testing.T) {
	dets := stateChannelFixture() // secret.any is non-enumerable: a PRECAUTIONARY label names the verb
	cases := []struct {
		action  string
		term    string
		mode    Mode
		reach   string
		verb    string
		notVerb string
	}{
		{"observe", "session:secret", SilentLog, ReachHarvest, "never blocks", "blocks on"},
		{"deny", "session:secret", HardBlock, ReachStop, "blocks on", "never blocks"},
		{"ask", "session:secret", ConfirmAndRecord, ReachStop, "asks on", "blocks on"},
		{"observe", "secret", SilentLog, ReachStop, "never blocks", "blocks on"}, // static: live hook logs it
	}
	for _, c := range cases {
		r := Rule{ID: "r", Action: c.action, If: Predicate{Tag: c.term}}
		b := Boundary(r, dets, known, ReachStop)
		if b.Mode != c.mode || b.Reach != c.reach {
			t.Errorf("%s %s: mode=%s reach=%q, want %s %q", c.action, c.term, b.Mode, b.Reach, c.mode, c.reach)
		}
		if !strings.Contains(b.Label, c.verb) || strings.Contains(b.Label, c.notVerb) {
			t.Errorf("%s %s: label %q must say %q and not %q", c.action, c.term, b.Label, c.verb, c.notVerb)
		}
	}
}

// known is a caller that has read every producer source and found no declarations:
// a producer-less term is then genuinely INERT, never merely unverified.
var known = StateProducers{SessionFactsKnown: true, AgentClaimsKnown: true}

// stateProducerFixture declares one model-claim producer and one daemon session fact,
// the two producer classes no detector covers (state-producer-declarations plan).
func stateProducerFixture() StateProducers {
	return StateProducers{
		SessionFactsKnown: true, AgentClaimsKnown: true,
		Producers: []StateProducer{
			{Tag: "agent:b1:reviewed", Values: []string{"model-claimed"}, Source: "binding:b1",
				Gaps: []string{"model judgment"}, Live: true},
			{Tag: "session:work", Values: []string{"uncommitted"}, Source: "daemon:checkpoint-settle",
				Enumerable: true, Gaps: []string{"written only at settle"}, Live: true},
			{Tag: "target:work", Values: []string{"uncommitted"}, Source: "ignored", Enumerable: true, Live: true},
		},
	}
}

// A declared producer makes a live state term fire; an undeclared key or value stays
// INERT once the caller has read every source.
func TestDeclaredStateProducersLabelLiveTerms(t *testing.T) {
	sp := stateProducerFixture()
	cases := []struct {
		action, tag, value string
		label              string
		producer           string
	}{
		{"ask", "agent:b1:reviewed", "", "coverage-limited (precautionary)", "binding:b1"},
		{"ask", "agent:b1:reviewed", "model-claimed", "coverage-limited (precautionary)", "binding:b1"},
		{"ask", "agent:b1:reviewed", "true", "inert (cannot fire)", ""},
		{"ask", "agent:b2:reviewed", "", "inert (cannot fire)", ""},
		{"deny", "session:work", "uncommitted", "enumerable (declared gaps)", "daemon:checkpoint-settle"},
		{"deny", "session:work", "committed", "inert (cannot fire)", ""},
		{"deny", "target:work", "uncommitted", "inert (cannot fire)", ""},     // declarations never produce target:
		{"observe", "session:work", "uncommitted", "inert (cannot fire)", ""}, // harvest never reads it
		{"observe", "agent:b1:reviewed", "", "inert (cannot fire)", ""},
	}
	for _, c := range cases {
		r := Rule{ID: "r", Action: c.action, If: Predicate{Tag: c.tag, Value: c.value}}
		b := Boundary(r, nil, sp, ReachStop)
		if b.DetectionLabel != c.label || b.Unverified {
			t.Errorf("%s %s=%s: label %q unverified=%v, want %q", c.action, c.tag, c.value, b.DetectionLabel, b.Unverified, c.label)
			continue
		}
		if got := strings.Join(b.Detection[0].Detectors, ","); got != c.producer {
			t.Errorf("%s %s=%s: producers %q, want %q", c.action, c.tag, c.value, got, c.producer)
		}
	}
	if b := Boundary(Rule{ID: "r", Action: "deny", If: Predicate{Tag: "session:work", Value: "uncommitted"}}, nil, sp, ReachStop); strings.Join(b.Detection[0].Gaps, ";") != "written only at settle" {
		t.Errorf("declared gaps must reach the term: %+v", b.Detection[0])
	}
	// an unknowable producer's gaps are not a complete list, but they are still shown
	if b := Boundary(Rule{ID: "r", Action: "ask", If: Predicate{Tag: "agent:b1:reviewed"}}, nil, sp, ReachStop); strings.Join(b.Detection[0].Limits, ";") != "model judgment" || len(b.Detection[0].Gaps) != 0 {
		t.Errorf("claim limits must reach the term: %+v", b.Detection[0])
	}
}

// Unknown is not inert, and unknown is not "cannot fire" under a NOT. A caller that
// could not read the binding set must not label a positive agent: term INERT, nor claim
// it can fire; a negated one fires either way.
func TestUnknownStateProducersAreUnverified(t *testing.T) {
	unknown := StateProducers{SessionFactsKnown: true, AgentClaimsNote: "no governance store at /x"}
	pos := Rule{ID: "pos", Action: "ask", If: Predicate{Tag: "agent:b1:reviewed"}}
	b := Boundary(pos, nil, unknown, ReachStop)
	if b.CanFire || !b.Unverified || b.DetectionLabel != "unverified (producers not visible here)" ||
		!strings.HasPrefix(b.Label, "UNVERIFIED") {
		t.Fatalf("positive unknown agent term: %+v", b)
	}
	tc := b.Detection[0]
	if tc.Undetectable || !tc.Unverified || !strings.Contains(tc.Note, "no governance store at /x") {
		t.Fatalf("term must be unverified with the caller's note, never undetectable: %+v", tc)
	}
	neg := Rule{ID: "neg", Action: "deny", If: Predicate{All: []Predicate{
		{Tag: ToolTagKey, Value: "Bash"}, {Not: &Predicate{Tag: "agent:b1:reviewed"}}}}}
	// Only the stateful tier evaluates a rule that reads state (the hook's tiers skip
	// it), and that tier has the tool identity: the rule can fire there whether or not
	// the claim exists, so an unknown binding set never makes it unverified.
	b = Boundary(neg, nil, unknown, ReachStop)
	if !b.CanFire || b.Unverified {
		t.Fatalf("negated unknown agent term fires either way: %+v", b)
	}
	if len(b.Tiers) != 1 || b.Tiers[0].Tier != "daemon-stateful" || !b.Tiers[0].CanFire {
		t.Fatalf("the stateful tier alone evaluates it: %+v", b.Tiers)
	}
	// harvest never holds agent: facts, so there it is INERT whatever the caller knows
	obs := Rule{ID: "obs", Action: "observe", If: Predicate{Tag: "agent:b1:reviewed"}}
	if b := Boundary(obs, nil, unknown, ReachStop); b.CanFire || b.Unverified {
		t.Fatalf("harvest agent term: %+v", b)
	}
	// the zero value knows no daemon facts either: session:work is unverified, not INERT
	work := Rule{ID: "work", Action: "ask", If: Predicate{Tag: "session:work", Value: "uncommitted"}}
	if b := Boundary(work, nil, StateProducers{}, ReachStop); !b.Unverified || b.CanFire {
		t.Fatalf("zero-value session fact: %+v", b)
	}
}

// Unverified folds explicitly: inert still kills a conjunction, unverified poisons the
// rest of it, and in a disjunction it decides only when no branch can fire.
func TestUnverifiedFolds(t *testing.T) {
	unknown := StateProducers{SessionFactsKnown: true}
	dets := []Detector{{ID: "vcs.push.force", Kind: "pattern", Regex: "x", Tag: tagSpec{Key: "vcs", Value: "push-force"},
		Coverage: Coverage{Enumerable: true}}}
	unv := Predicate{Tag: "agent:b1:reviewed"}
	fires := Predicate{Tag: "vcs", Value: "push-force"}
	dead := Predicate{Tag: "vcs", Value: "nonexistent"}
	cases := []struct {
		name       string
		p          Predicate
		canFire    bool
		unverified bool
	}{
		{"all[unverified,fires]", Predicate{All: []Predicate{unv, fires}}, false, true},
		{"all[unverified,dead]", Predicate{All: []Predicate{unv, dead}}, false, false},
		{"any[unverified,fires]", Predicate{Any: []Predicate{unv, fires}}, true, false},
		{"any[unverified,dead]", Predicate{Any: []Predicate{unv, dead}}, false, true},
		{"not any[unverified,fires]", Predicate{Not: &Predicate{Any: []Predicate{unv, fires}}}, true, false},
	}
	for _, c := range cases {
		b := Boundary(Rule{ID: c.name, Action: "deny", If: c.p}, dets, unknown, ReachStop)
		if b.CanFire != c.canFire || b.Unverified != c.unverified {
			t.Errorf("%s: can_fire=%v unverified=%v, want %v %v", c.name, b.CanFire, b.Unverified, c.canFire, c.unverified)
		}
	}
}

// A report-only rule that reads state is labeled at harvest reach, where nothing produces
// the hook's evaluation-only command/tool tags: a command term there must not make it
// look able to fire (postwork PW-2). On the live channel the same term keeps its hook
// producer.
func TestEvalOnlyProducersExistOnlyLive(t *testing.T) {
	dets := stateChannelFixture()
	report := Rule{ID: "r", Action: "observe", If: Predicate{All: []Predicate{
		{Tag: "session:secret"}, {Tag: CommandTagKey, Matches: "git"}}}}
	if b := Boundary(report, dets, known, ReachStop); b.CanFire || b.Reach != ReachHarvest {
		t.Fatalf("report-only stateful rule with a command term: can_fire=%v reach=%q label=%q", b.CanFire, b.Reach, b.Label)
	}
	for _, tag := range []string{CommandTagKey, ToolTagKey} {
		live := Rule{ID: "l", Action: "deny", If: Predicate{Tag: tag}}
		if b := Boundary(live, dets, known, ReachStop); !b.CanFire {
			t.Errorf("live %s rule lost its hook producer: %s", tag, b.Label)
		}
		if b := Boundary(live, dets, known, ReachHarvest); b.CanFire {
			t.Errorf("harvest %s rule claims a hook producer: %s", tag, b.Label)
		}
	}
}

// The remaining verbs and the negated target term on harvest (postwork PW-5).
func TestLabelVerbsAndHarvestTargetNegation(t *testing.T) {
	dets := stateChannelFixture()
	warn := Boundary(Rule{ID: "w", Action: "", Mode: WarnAndProceed, If: Predicate{Tag: "secret"}}, dets, known, ReachStop)
	if !strings.Contains(warn.Label, "warns on a matched detector, never blocks") || warn.Reach != ReachStop {
		t.Errorf("static warn rule: reach=%q label=%q", warn.Reach, warn.Label)
	}
	allow := Boundary(Rule{ID: "a", Action: "allow", If: Predicate{Tag: "secret"}}, dets, known, ReachStop)
	if allow.Mode != SilentLog || !strings.Contains(allow.Label, "never blocks") {
		t.Errorf("allow rule: mode=%s label=%q", allow.Mode, allow.Label)
	}
	notTarget := Boundary(Rule{ID: "n", Action: "observe", If: Predicate{Not: &Predicate{Tag: "target:secret"}}}, dets, known, ReachStop)
	// The audit cannot read a target: fact, so the evaluator leaves the term undecided and
	// the rule never fires there: INERT, not VACUOUS (it was vacuous while absence read as No).
	if notTarget.Reach != ReachHarvest || notTarget.DetectionLabel != "inert (cannot fire)" {
		t.Errorf("not target: on harvest: reach=%q detection=%q", notTarget.Reach, notTarget.DetectionLabel)
	}
}
