package engine

import (
	"strings"
	"testing"
)

// tierFixture: an unknowable credential pattern (no roles, so it produces live) plus the
// state-channel fixture's enumerable vcs detector.
func tierFixture() []Detector {
	return append(stateChannelFixture(),
		Detector{ID: "data.credential", Kind: "pattern", Regex: "BEGIN PRIVATE KEY",
			Tag: tagSpec{Key: "data-class", Value: "credential-material"}, Coverage: Coverage{Gaps: []string{"novel formats"}}})
}

func liveLabel(t *testing.T, p Predicate, set LiveRuleSet) RuleBoundary {
	t.Helper()
	return CompileLiveBoundaries(&Policy{Rules: []Rule{{ID: "r", Action: "deny", If: p}}}, tierFixture(), stateProducerFixture(), set)[0]
}

func tierNames(vs []TierVerdict) string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Tier+"="+v.Loads)
	}
	return strings.Join(out, ",")
}

var (
	credTerm = Predicate{Tag: "data-class", Value: "credential-material"}
	bashTerm = Predicate{Tag: ToolTagKey, Value: "Bash"}
	workTerm = Predicate{Tag: "session:work", Value: "uncommitted"}
)

// The hook's two tiers decide over the same facts (engine.ActionTags), so a user-rulebook
// rule on a detector tag can fire at the standalone tier wherever the engine tier stands:
// the label is the same for every engine load, and only the engine row's "loads" differs.
func TestDetectorRuleFiresAtEitherHookTier(t *testing.T) {
	rows := map[EngineLoad]string{
		EngineSkips:   "hook-engine=not here,hook-standalone=yes",
		EngineLoads:   "hook-engine=yes,hook-standalone=yes",
		EngineUnknown: "hook-engine=unknown,hook-standalone=yes",
	}
	for _, tier := range []StatefulTier{StatefulArmed, StatefulUnarmed, StatefulDown} {
		for eng, want := range rows {
			b := liveLabel(t, credTerm, LiveRuleSet{Kind: SetUser, Tier: tier, HookEngine: eng})
			if !b.CanFire || b.Unverified || b.DetectionLabel != "coverage-limited (precautionary)" || tierNames(b.Tiers) != want {
				t.Errorf("tier %d engine %d: %+v", tier, eng, b)
			}
			if tc := b.Detection[0]; tc.Undetectable || !strings.Contains(strings.Join(tc.Tiers, ","), "hook-standalone") {
				t.Errorf("tier %d engine %d row: %+v", tier, eng, tc)
			}
		}
	}
}

// A rule that reads state is the stateful tier's alone, and that tier has the raw
// command and the exact tool identity as well as the folded state
// (engine.InvocationTags), so tool ∧ state and command ∧ state both can fire there —
// whatever the hook's engine tier loads, since the hook skips the rule.
func TestToolAndStateMeetInTheStatefulTier(t *testing.T) {
	for _, eng := range []EngineLoad{EngineUnknown, EngineLoads, EngineSkips} {
		set := LiveRuleSet{Kind: SetUser, Tier: StatefulArmed, HookEngine: eng}
		for name, p := range map[string]Predicate{
			"tool ∧ session":    {All: []Predicate{bashTerm, workTerm}},
			"command ∧ session": {All: []Predicate{{Tag: CommandTagKey, Matches: "git"}, workTerm}},
		} {
			b := liveLabel(t, p, set)
			if !b.CanFire || b.Unverified || b.Reach != ReachStop || tierNames(b.Tiers) != "daemon-stateful=yes" {
				t.Errorf("engine %d: %s: %+v", eng, name, b)
			}
		}
	}
}

// Every hook tier has the tool identity: `not: tool=Bash` is a real test there, never
// satisfied by absence, whatever the engine tier loads. (It was VACUOUS under the engine
// tier while that tier decided over detector tags alone.)
func TestNegatedToolIsDetectedAtEveryHookTier(t *testing.T) {
	notBash := Predicate{Not: &bashTerm}
	for _, eng := range []EngineLoad{EngineSkips, EngineLoads, EngineUnknown} {
		b := liveLabel(t, notBash, LiveRuleSet{Kind: SetUser, Tier: StatefulArmed, HookEngine: eng})
		if !b.CanFire || b.DetectionLabel != "enumerable (declared gaps)" {
			t.Errorf("engine %d: %+v", eng, b)
		}
		for _, v := range b.Tiers {
			if v.DetectionLabel != "enumerable (declared gaps)" {
				t.Errorf("engine %d tier %s: %+v", eng, v.Tier, v)
			}
		}
	}
}

// A rule spanning an invocation fact and a detector fact is labeled the same wherever
// the engine tier stands, and a conjunction of the two can fire (it could fire at no
// tier while each tier held half the facts).
func TestMixedRuleLabelIgnoresTheEngineLoad(t *testing.T) {
	either := Predicate{Any: []Predicate{bashTerm, credTerm}}
	both := Predicate{All: []Predicate{{Tag: CommandTagKey, Matches: "curl"}, credTerm}}
	for _, eng := range []EngineLoad{EngineSkips, EngineLoads, EngineUnknown} {
		set := LiveRuleSet{Kind: SetUser, Tier: StatefulArmed, HookEngine: eng}
		if b := liveLabel(t, either, set); !b.CanFire || b.DetectionLabel != "coverage-limited (precautionary)" {
			t.Errorf("any, engine %d: %+v", eng, b)
		}
		if b := liveLabel(t, both, set); !b.CanFire || b.Unverified {
			t.Errorf("all, engine %d: %+v", eng, b)
		}
	}
}

// Only the engine tier loads the invocation file, and it has every action fact: its
// tool, command and detector rules can all fire. Its state rules reach no tier.
func TestInvocationSetIsTheEngineTierOnly(t *testing.T) {
	set := LiveRuleSet{Kind: SetInvocation, Tier: StatefulArmed, HookEngine: EngineSkips} // both ignored
	for _, p := range []Predicate{bashTerm, {Tag: CommandTagKey, Matches: "rm"}, credTerm} {
		if b := liveLabel(t, p, set); !b.CanFire || b.Unverified || b.Reach != ReachStop || tierNames(b.Tiers) != "hook-engine=yes" {
			t.Errorf("%s: %+v", p.Tag, b)
		}
	}
	if b := liveLabel(t, workTerm, set); b.CanFire || b.Reach != ReachStopNotLoaded || b.Detection[0].Note != staticStateNote {
		t.Errorf("state rule: %+v", b)
	}
}

// A negated state rule is the stateful tier's alone: the hook's tiers skip every rule
// that reads state (it read as firing on every call in the standalone tier before that
// fix). Armed, the stateful tier evaluates it; unarmed, no tier does and it is INERT.
func TestNegatedStateRuleIsTheStatefulTiersAlone(t *testing.T) {
	negated := Predicate{Not: &Predicate{Tag: "agent:b1:reviewed"}}
	armed := liveLabel(t, negated, LiveRuleSet{Kind: SetUser, Tier: StatefulArmed, HookEngine: EngineSkips})
	if !armed.CanFire || armed.Reach != ReachStop || tierNames(armed.Tiers) != "daemon-stateful=yes" {
		t.Errorf("armed: %+v", armed)
	}
	unarmed := liveLabel(t, negated, LiveRuleSet{Kind: SetUser, Tier: StatefulUnarmed, HookEngine: EngineLoads})
	if unarmed.CanFire || unarmed.Unverified || unarmed.Reach != ReachStopUnarmed || len(unarmed.Tiers) != 0 ||
		!strings.HasPrefix(unarmed.Label, "INERT — no live tier evaluates this rule here") {
		t.Errorf("unarmed: %+v", unarmed)
	}
}

// The hook's fired-rule label names the one tier that fired it, over that tier's facts:
// the tool identity and the detector tags both.
func TestBoundaryOnTheEngineTier(t *testing.T) {
	r := Rule{ID: "x", Action: "deny", If: Predicate{Any: []Predicate{bashTerm, credTerm}}}
	b := BoundaryOn(r, tierFixture(), known, ReachStop, TierEngine)
	if !b.CanFire || b.DetectionLabel != "coverage-limited (precautionary)" || tierNames(b.Tiers) != "hook-engine=yes" {
		t.Fatalf("%+v", b)
	}
	if tc := b.Detection[0]; tc.Tag != ToolTagKey || tc.Undetectable {
		t.Fatalf("the engine tier has the tool identity: %+v", tc)
	}
}

// Harvest and dry-run labels list no tiers and keep their single channel.
func TestHarvestHasNoTiers(t *testing.T) {
	// No hook-only tool or command producer exists on the harvest channel (PW-2), so the
	// rule here is a state term alone.
	r := Rule{ID: "x", Action: "deny", If: Predicate{Tag: "session:vcs", Value: "push-force"}}
	for _, reach := range []string{ReachHarvest, ReachDryRun} {
		if b := Boundary(r, tierFixture(), known, reach); len(b.Tiers) != 0 || len(b.Detection[0].Tiers) != 0 || !b.CanFire {
			t.Errorf("%s: %+v", reach, b)
		}
	}
}

// Harvest and dry-run INERT keep their single-channel wording (postwork PW-3).
func TestHarvestInertWordingUnchanged(t *testing.T) {
	b := Boundary(Rule{ID: "x", Action: "deny", If: Predicate{Tag: "data-class", Value: "radioactive"}}, tierFixture(), known, ReachDryRun)
	if b.Label != "INERT — a required term has no producer (detector or declared state producer); this rule can never fire ("+ReachDryRun+")" {
		t.Errorf("%q", b.Label)
	}
}
