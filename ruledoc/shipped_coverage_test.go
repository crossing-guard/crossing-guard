package ruledoc

import (
	"strings"
	"testing"

	"crossing-guard/engine"
)

// The reverse invariant (stateful-rule-coverage plan §5 inv. 2): a shipped rule never
// names a fact its paired shipped detector document cannot produce. The rule that
// motivated it, observe-credential-external-egress, needed data-class=credential-material
// for weeks while no shipped detector emitted it.
//
// Each rulebook is checked against the detector document it ships with. The legacy
// default rulebook pairs with the legacy starter. The safety starter has only command
// terms, so it holds against the structural floor too. security-observe is meant to pair
// with a security detector assembly that is not built yet (configuration.md); against the
// floor its credential rule is INERT, and that is pinned here so building the assembly
// flips it on purpose rather than silently.
func TestShippedRulesHaveShippedProducers(t *testing.T) {
	starter, err := engine.DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	floor, err := engine.StructuralDetectors()
	if err != nil {
		t.Fatal(err)
	}
	pairings := []struct {
		name      string
		rules     []byte
		detectors []engine.Detector
		inert     map[string]bool // rule ids expected INERT for this pairing
	}{
		{"default+starter", DefaultRules(), starter, nil},
		{"safety-starter+starter", SafetyStarterRules(), starter, nil},
		{"safety-starter+floor", SafetyStarterRules(), floor, nil},
		{"security-observe+starter", SecurityObserveRules(), starter, nil},
		{"security-observe+floor", SecurityObserveRules(), floor,
			map[string]bool{"observe-credential-external-egress": true}},
	}
	for _, p := range pairings {
		pol, err := Parse(p.rules)
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		// Live reach for every rule; harvest reach only for the rules the audit replays
		// (the stateful ones). A command/tool rule has no harvest producer by design.
		stateful := &engine.Policy{}
		for _, r := range pol.Rules {
			if engine.ReferencesState(r.If) {
				stateful.Rules = append(stateful.Rules, r)
			}
		}
		for _, run := range []struct {
			pol   *engine.Policy
			reach string
		}{{pol, engine.ReachStop}, {stateful, engine.ReachHarvest}} {
			reach := run.reach
			for _, b := range engine.CompileBoundaries(run.pol, p.detectors, allSourcesRead, reach) {
				if p.inert[b.Rule] {
					if b.CanFire {
						t.Errorf("%s %s: expected INERT (paired assembly not built), got %s", p.name, b.Rule, b.Label)
					}
					continue
				}
				for _, tc := range b.Detection {
					if tc.Undetectable {
						t.Errorf("%s @ %s: rule %s term %s=%s has no shipped producer", p.name, reach, b.Rule, tc.Tag, tc.Value)
					}
				}
			}
		}
	}
}

// The shipped default rules' labels against the legacy starter, as a user sees them from
// `crossing-guard coverage` (ReachStop). The three observe rules are report-only and
// stateful: the live tier drops their verdict, so they are labeled at harvest reach and
// never claim to block.
func TestShippedDefaultRuleLabels(t *testing.T) {
	starter, err := engine.DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := Parse(DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	byRule := map[string]engine.RuleBoundary{}
	for _, b := range engine.CompileBoundaries(pol, starter, allSourcesRead, engine.ReachStop) {
		byRule[b.Rule] = b
	}
	for _, id := range []string{"observe-code-without-plan", "observe-force-push-without-red-team",
		"observe-credential-external-egress"} {
		b, ok := byRule[id]
		if !ok {
			t.Fatalf("shipped rule %s missing", id)
		}
		if !b.CanFire || b.Mode != engine.SilentLog || b.Reach != engine.ReachHarvest ||
			strings.Contains(b.Label, "blocks on") {
			t.Errorf("%s: can_fire=%v mode=%s reach=%q label=%q", id, b.CanFire, b.Mode, b.Reach, b.Label)
		}
	}
	producers := map[string]string{}
	for _, tc := range byRule["observe-credential-external-egress"].Detection {
		producers[tc.Tag] = strings.Join(tc.Detectors, ",")
	}
	if producers["session:data-class"] != "data.credential" || producers["session:destination-class"] != "net.dest" {
		t.Errorf("credential rule producers %v, want data.credential and net.dest", producers)
	}
	for _, id := range []string{"destructive-rm", "curl-pipe-shell", "canary-deny"} {
		if b := byRule[id]; b.Mode != engine.HardBlock || b.Reach != engine.ReachStop || !strings.Contains(b.Label, "blocks on") {
			t.Errorf("%s: mode=%s reach=%q label=%q", id, b.Mode, b.Reach, b.Label)
		}
	}
	for _, id := range []string{"git-force-push", "git-hard-reset", "team-link-change"} {
		if b := byRule[id]; b.Mode != engine.ConfirmAndRecord || b.Reach != engine.ReachStop || !strings.Contains(b.Label, "asks on") {
			t.Errorf("%s: mode=%s reach=%q label=%q", id, b.Mode, b.Reach, b.Label)
		}
	}
}

// allSourcesRead deliberately withholds the daemon-fact catalog while marking every
// source known: a shipped rule must be backed by its paired detector document alone, so
// any term those detectors cannot produce is reported undetectable, never unverified
// and never rescued by a daemon fact.
var allSourcesRead = engine.StateProducers{SessionFactsKnown: true, AgentClaimsKnown: true}
