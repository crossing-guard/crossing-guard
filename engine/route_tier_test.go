package engine

import (
	"errors"
	"testing"
)

// routeRulePolicy is one pure command rule, one state rule, one route rule and one
// NEGATED route rule: the last is the shape that fires by absence on a tag set with no
// route: key, which is every hook call.
func routeRulePolicy() *Policy {
	return &Policy{Rules: []Rule{
		{ID: "command", Action: "deny", If: Predicate{Tag: CommandTagKey, Matches: "alpha"}},
		{ID: "state", Action: "deny", If: Predicate{Tag: SessionStatePrefix + "reviewed", Value: "true"}},
		{ID: "route-plain", Action: "deny", If: Predicate{Tag: RouteFactPrefix + "local", Value: "false"}},
		{ID: "route-negated", Action: "deny", If: Predicate{Not: &Predicate{Tag: RouteFactPrefix + "local", Value: "true"}}},
		{ID: "route-nested", Action: "deny", If: Predicate{All: []Predicate{
			{Tag: ProfileLocalityFactKey, Value: "local-only"},
			{Any: []Predicate{{Not: &Predicate{Tag: RouteFactPrefix + "family", Value: "inference"}}}}}}},
	}}
}

func ruleIDs(p *Policy) []string {
	ids := []string{}
	for _, r := range p.Rules {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestRouteRulesAreInNeitherHookTier(t *testing.T) {
	policy := routeRulePolicy()
	if got := ruleIDs(StaticTier(policy)); len(got) != 1 || got[0] != "command" {
		t.Fatalf("static tier = %v, want only the command rule", got)
	}
	if got := ruleIDs(RouteTier(policy)); len(got) != 3 {
		t.Fatalf("route tier = %v, want the three route rules", got)
	}
	for _, r := range policy.Rules {
		inStatic, inRoute := !ReferencesState(r.If) && !ReferencesRoute(r.If), ReferencesRoute(r.If)
		if inStatic && inRoute {
			t.Errorf("rule %s is in both the static tier and the route tier", r.ID)
		}
	}
}

// TestNegatedRouteRuleNeverFiresOnAHookTagSet is the defect the third key class exists
// for: over a hook call's tags the negated term is true by absence. The static tier
// must not hold the rule at all, so the decision is allow.
func TestNegatedRouteRuleNeverFiresOnAHookTagSet(t *testing.T) {
	policy := routeRulePolicy()
	hookTags := InvocationTags("Bash", "git push origin main")
	if d := Decide(hookTags, StaticTier(policy)); d.Decision != "allow" {
		t.Fatalf("static tier decided %q by rule %q over a hook call; a route rule must not be there", d.Decision, d.Rule)
	}
	// The same tags against the unfiltered policy DO fire the negated rule: this is what
	// the exclusion prevents, and why the test fails if the exclusion is removed.
	if d := Decide(hookTags, policy); d.Decision != "block" {
		t.Fatalf("the unfiltered policy decided %q; the negated route rule should fire by absence", d.Decision)
	}
}

func TestValidateRouteRulesRefusesAMixedRule(t *testing.T) {
	routeTerm := Predicate{Tag: RouteFactPrefix + "local", Value: "false"}
	for name, foreign := range map[string]Predicate{
		"command":  {Tag: CommandTagKey, Matches: "push"},
		"tool":     {Tag: ToolTagKey, Value: "Bash"},
		"session":  {Tag: SessionStatePrefix + "reviewed", Value: "true"},
		"target":   {Tag: TargetStatePrefix + "sensitivity", Value: "restricted"},
		"agent":    {Tag: AgentStatePrefix + "binding:tag"},
		"detector": {Tag: "data-class", Value: "secret"},
		"negated":  {Not: &Predicate{Tag: CommandTagKey, Matches: "push"}},
	} {
		policy := &Policy{Rules: []Rule{{ID: "mixed-" + name, Action: "deny", If: Predicate{All: []Predicate{routeTerm, foreign}}}}}
		var mixed *MixedRouteRuleError
		if err := ValidateRouteRules(policy); !errors.As(err, &mixed) || mixed.RuleID != "mixed-"+name {
			t.Errorf("%s: mixed rule was not refused by name: %v", name, err)
		}
	}
	if err := ValidateRouteRules(routeRulePolicy()); err != nil {
		t.Fatalf("pure route, state and command rules must validate: %v", err)
	}
}

// A coverage surface must not say the hook stops an action with a route rule: no tier
// at the action boundary evaluates one (OD-25).
func TestLiveCoverageGivesARouteRuleNoActionTier(t *testing.T) {
	for _, r := range routeRulePolicy().Rules {
		if !ReferencesRoute(r.If) {
			continue
		}
		for _, set := range []LiveRuleSet{{Kind: SetUser, Tier: StatefulArmed, HookEngine: EngineLoads}, {Kind: SetInvocation}} {
			if tiers, engine := liveTiers(r, set, LiveReach(r, set)); tiers != 0 || engine != EngineSkips {
				t.Errorf("route rule %s in set %v: tiers = %v engine = %v, want no tier", r.ID, set.Kind, tiers, engine)
			}
		}
	}
}
