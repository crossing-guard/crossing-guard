package ruledoc

import (
	"testing"

	"crossing-guard/engine"
)

// Every shipped rulebook as `crossing-guard coverage` and /api/policy/coverage
// label it through the live-set API, on an armed and an unarmed host (stateful-tier
// reach plan, red-team RT-12). The report-only observe rules act in the audit either
// way; the command rules are decided by the hook's static tiers either way. Nothing in
// the shipped set may change meaning on an unarmed platform.
func TestShippedDefaultLiveReachArmedAndUnarmed(t *testing.T) {
	starter, err := engine.DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range [][]byte{DefaultRules(), SafetyStarterRules(), SecurityObserveRules()} {
		pol, err := Parse(doc)
		if err != nil {
			t.Fatal(err)
		}
		checkShippedLiveReach(t, pol, starter)
	}
}

func checkShippedLiveReach(t *testing.T, pol *engine.Policy, starter []engine.Detector) {
	t.Helper()
	for _, tier := range []engine.StatefulTier{engine.StatefulArmed, engine.StatefulUnarmed} {
		set := engine.LiveRuleSet{Kind: engine.SetUser, Tier: tier}
		for _, b := range engine.CompileLiveBoundaries(pol, starter, allSourcesRead, set) {
			want := engine.ReachStop
			if b.Mode == engine.SilentLog {
				want = engine.ReachHarvest
			}
			if b.Reach != want || !b.CanFire {
				t.Errorf("tier %d %s: can_fire=%v reach=%q, want %q", tier, b.Rule, b.CanFire, b.Reach, want)
			}
		}
	}
}
