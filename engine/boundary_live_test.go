package engine

import (
	"strings"
	"testing"
)

var staticReaches = []string{ReachStopUnarmed, ReachStopNotLoaded, ReachStopStatefulDown}

// At a static reach no tier evaluates a rule that reads state (stateful-tier reach plan
// D-2): the hook's static tiers skip every state-referencing rule (RT-2's fix,
// engine.StaticTier) and no stateful tier runs it there. So the rule is INERT whether
// its state term is positive or negated — these negated rows read VACUOUS before that
// fix landed and were flipped on purpose. Never UNVERIFIED, even for a caller that read
// no producer source: the surface knows nothing produces state here.
func TestStaticReachStateTerms(t *testing.T) {
	dets := stateChannelFixture()
	declared := stateProducerFixture()
	bash := Predicate{Tag: ToolTagKey, Value: "Bash"}
	for _, reach := range staticReaches {
		for _, sp := range []StateProducers{{}, known, declared} {
			cases := []struct {
				name      string
				p         Predicate
				canFire   bool
				detection string
			}{
				{"positive session", Predicate{All: []Predicate{bash, {Tag: "session:vcs", Value: "push-force"}}}, false, "inert (cannot fire)"},
				{"positive declared", Predicate{Tag: "session:work", Value: "uncommitted"}, false, "inert (cannot fire)"},
				{"positive agent", Predicate{Tag: "agent:b1:reviewed"}, false, "inert (cannot fire)"},
				{"positive target", Predicate{Tag: "target:secret", Value: "aws-key"}, false, "inert (cannot fire)"},
				{"negated agent alone", Predicate{Not: &Predicate{Tag: "agent:b1:reviewed"}}, false, "inert (cannot fire)"},
				// The whole rule is skipped, so a stateless sibling term does not make it fire.
				{"negated agent with tool", Predicate{All: []Predicate{bash, {Not: &Predicate{Tag: "agent:b1:reviewed"}}}}, false, "inert (cannot fire)"},
			}
			for _, c := range cases {
				b := Boundary(Rule{ID: c.name, Action: "deny", If: c.p}, dets, sp, reach)
				if b.CanFire != c.canFire || b.Unverified || b.DetectionLabel != c.detection || b.Reach != reach {
					t.Errorf("%s @ %q: got can_fire=%v unverified=%v %q reach %q", c.name, reach, b.CanFire, b.Unverified, b.DetectionLabel, b.Reach)
				}
				for _, tc := range b.Detection {
					if IsStateTag(tc.Tag) && (!tc.Undetectable || tc.Unverified || tc.Note != staticStateNote || len(tc.Detectors) != 0) {
						t.Errorf("%s @ %q: state row %+v", c.name, reach, tc)
					}
				}
			}
		}
	}
}

// A rule with no state term labels at a static reach exactly as at ReachStop: the same
// hook tiers decide it, with the same live role filter.
func TestStaticReachLeavesStatelessRulesAlone(t *testing.T) {
	dets := stateChannelFixture()
	for _, p := range []Predicate{
		{Tag: "vcs", Value: "push-force"},
		{Tag: "agent", Value: "pushback"}, // assistant-only: not produced live
		{Tag: CommandTagKey, Matches: "rm"},
		{Tag: ToolTagKey, Value: "Bash"},
	} {
		r := Rule{ID: "r", Action: "deny", If: p}
		want := Boundary(r, dets, known, ReachStop)
		for _, reach := range staticReaches {
			got := Boundary(r, dets, known, reach)
			if got.CanFire != want.CanFire || got.DetectionLabel != want.DetectionLabel ||
				strings.Join(got.Detection[0].Detectors, ",") != strings.Join(want.Detection[0].Detectors, ",") {
				t.Errorf("%s @ %q: %+v, want %+v", p.Tag, reach, got, want)
			}
		}
	}
}

// LiveReach is the one place a live surface's reach is chosen (plan D-1/D-1a).
func TestLiveReach(t *testing.T) {
	state := Predicate{Tag: "session:vcs", Value: "push-force"}
	stateless := Predicate{Tag: CommandTagKey, Matches: "rm"}
	type set = LiveRuleSet
	cases := []struct {
		name string
		p    Predicate
		mode Mode
		set  set
		want string
	}{
		{"stateless anywhere", stateless, HardBlock, set{Kind: SetTeam}, ReachStop},
		{"stateless silent", stateless, SilentLog, set{Kind: SetUser, Tier: StatefulUnarmed}, ReachStop},
		{"gating armed user", state, HardBlock, set{Kind: SetUser, Tier: StatefulArmed}, ReachStop},
		{"ask unarmed user", state, ConfirmAndRecord, set{Kind: SetUser, Tier: StatefulUnarmed}, ReachStopUnarmed},
		{"gating down user", state, HardBlock, set{Kind: SetUser, Tier: StatefulDown}, ReachStopStatefulDown},
		// The stateful tier loads the adopted team layers too (layered-policy plan).
		{"gating team armed", state, HardBlock, set{Kind: SetTeam, Tier: StatefulArmed}, ReachStop},
		{"gating team unarmed", state, HardBlock, set{Kind: SetTeam, Tier: StatefulUnarmed}, ReachStopUnarmed},
		// The hook's invocation file never reaches the stateful tier.
		{"gating invocation", state, HardBlock, set{Kind: SetInvocation}, ReachStopNotLoaded},
		// A non-gating state rule in the user rulebook acts only in the audit, armed or
		// not: the hook skips state rules and the stateful tier drops the verdict.
		{"silent user armed", state, SilentLog, set{Kind: SetUser, Tier: StatefulArmed}, ReachHarvest},
		{"silent user unarmed", state, SilentLog, set{Kind: SetUser, Tier: StatefulUnarmed}, ReachHarvest},
		{"warn user armed", state, WarnAndProceed, set{Kind: SetUser, Tier: StatefulArmed}, ReachHarvest},
		{"warn user unarmed", state, WarnAndProceed, set{Kind: SetUser, Tier: StatefulUnarmed}, ReachHarvest},
		// The audit does not load team layers: a non-gating team state rule keeps its
		// tier's reach and is labeled INERT there (no tier acts on it).
		{"silent team", state, SilentLog, set{Kind: SetTeam, Tier: StatefulArmed}, ReachStop},
		{"warn team", state, WarnAndProceed, set{Kind: SetTeam, Tier: StatefulUnarmed}, ReachStopUnarmed},
	}
	for _, c := range cases {
		r := Rule{ID: c.name, Mode: c.mode, If: c.p}
		if got := LiveReach(r, c.set); got != c.want {
			t.Errorf("%s: LiveReach = %q, want %q", c.name, got, c.want)
		}
		// Boundary keeps the chosen reach (its own override applies at ReachStop only).
		if b := CompileLiveBoundaries(&Policy{Rules: []Rule{r}}, nil, known, c.set)[0]; b.Reach != c.want {
			t.Errorf("%s: compiled reach %q, want %q", c.name, b.Reach, c.want)
		}
	}
}

// A report-only state rule nothing evaluates is never labeled able to fire, and a
// non-gating state rule labeled by a dry-run caller still lands at harvest (postwork
// PW-1, PW-10).
func TestNotEvaluatedAndDryRunReaches(t *testing.T) {
	silent := Rule{ID: "s", Mode: SilentLog, If: Predicate{Tag: "session:vcs", Value: "push-force"}}
	b := CompileLiveBoundaries(&Policy{Rules: []Rule{silent}}, stateChannelFixture(), known,
		LiveRuleSet{Kind: SetTeam, Tier: StatefulArmed})[0]
	if b.CanFire || b.Unverified || b.Reach != ReachStop || !strings.HasPrefix(b.Label, "INERT — no live tier evaluates this rule here") {
		t.Errorf("silent team rule: %+v", b)
	}
	if b := Boundary(silent, stateChannelFixture(), known, ReachDryRun); b.Reach != ReachHarvest {
		t.Errorf("dry-run caller: reach %q, want harvest", b.Reach)
	}
}
