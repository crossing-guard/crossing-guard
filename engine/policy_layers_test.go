package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// The tighten-only property (team plan §5.6.3, criterion 23; §5.16): concatenated
// layers user ++ repository ++ organization can never make an effective decision
// WEAKER than the user layer alone. This is the acceptance spine for item 3 — the
// property is what lets a team distribute policy without being able to loosen a
// developer's own rules. It holds by construction (strongest severity wins,
// authored allow → SilentLog rank 3), and this test pins it so no future loader or
// merge path can silently break it.

// randomRule builds one rule with a random action over one of a fixed tag space, so
// every run explores different overlaps between the three layers.
func randomRule(rng *rand.Rand, id string, layered bool) Rule {
	actions := []string{"deny", "ask", "allow", "observe"}
	verbs := []string{"bash", "edit", "read", "write", "fetch"}
	r := Rule{
		ID:     id,
		Action: actions[rng.Intn(len(actions))],
		If:     Predicate{Tag: "tool", Matches: fmt.Sprintf(`\b%s\b`, verbs[rng.Intn(len(verbs))])},
	}
	if layered {
		switch rng.Intn(3) {
		case 0:
			r.Layer = LayerUser
		case 1:
			r.Layer = LayerRepository
		default:
			r.Layer = LayerOrganization
		}
	}
	return r
}

// rankOf is the strength ordering the property speaks in: 0 is the strongest
// (hard-block) and 3 the weakest (silent-log). The effective rank of a policy over a
// tag set is the rank of its winning rule's mode; a policy with no matching rule is
// the floor (allow, rank 3).
func rankOf(d Decision) int {
	if d.Decision != "allow" {
		return d.Mode.rank()
	}
	return 3
}

func TestLayeredConcatenationIsTightenOnly(t *testing.T) {
	rng := rand.New(rand.NewSource(38))
	for run := 0; run < 300; run++ {
		user := &Policy{}
		repo := &Policy{}
		org := &Policy{}
		for i := 0; i < 1+rng.Intn(4); i++ {
			user.Rules = append(user.Rules, randomRule(rng, fmt.Sprintf("u%d", i), true))
		}
		if rng.Intn(2) == 0 {
			for i := 0; i < 1+rng.Intn(4); i++ {
				repo.Rules = append(repo.Rules, randomRule(rng, fmt.Sprintf("r%d", i), true))
			}
		}
		if rng.Intn(2) == 0 {
			for i := 0; i < 1+rng.Intn(4); i++ {
				org.Rules = append(org.Rules, randomRule(rng, fmt.Sprintf("o%d", i), true))
			}
		}
		if err := CompilePredicates(user); err != nil {
			t.Fatalf("user policy: %v", err)
		}
		if err := CompilePredicates(repo); err != nil {
			t.Fatalf("repository policy: %v", err)
		}
		if err := CompilePredicates(org); err != nil {
			t.Fatalf("organization policy: %v", err)
		}

		// The layered policy is the concatenation, in load order.
		layered := &Policy{Rules: append(append(append([]Rule{}, user.Rules...), repo.Rules...), org.Rules...)}

		// Probe over every verb so both matching and non-matching paths are explored.
		for _, verb := range []string{"bash", "edit", "read", "write", "fetch", "unmatched"} {
			tags := []Tag{{Key: "tool", Value: "run " + verb}}
			alone := Decide(tags, user)
			together := Decide(tags, layered)

			if rankOf(together) > rankOf(alone) {
				t.Fatalf("run %d verb %q: layered decision %q (mode %s, rank %d) is WEAKER than user-only %q (mode %s, rank %d)",
					run, verb, together.Decision, together.Mode, rankOf(together), alone.Decision, alone.Mode, rankOf(alone))
			}
		}
	}
}

// TestDecisionCarriesTheWinningLayersLayer pins the report side of the same design:
// the Decision names the tier the winning rule arrived by, so the event and the
// chain can say `layer: organization` — and an unmatched allow carries no layer at
// all, because no rule decided it.
func TestDecisionCarriesTheWinningLayersLayer(t *testing.T) {
	user := &Policy{Rules: []Rule{{ID: "u-deny", Action: "deny", If: Predicate{Tag: "tool", Matches: `\bbash\b`}, Layer: LayerUser}}}
	org := &Policy{Rules: []Rule{{ID: "o-ask", Action: "ask", If: Predicate{Tag: "tool", Matches: `\bbash\b`}, Layer: LayerOrganization}}}
	for _, pol := range []*Policy{user, org} {
		if err := CompilePredicates(pol); err != nil {
			t.Fatalf("compile: %v", err)
		}
	}

	bash := []Tag{{Key: "tool", Value: "run bash"}}
	// User deny outranks org ask: tighten-only, and the Decision still names the
	// USER layer because that is the rule that won.
	if d := Decide(bash, user); d.Decision != "block" || d.Layer != LayerUser {
		t.Fatalf("user-only deny: %+v", d)
	}
	layered := &Policy{Rules: append(append([]Rule{}, user.Rules...), org.Rules...)}
	if d := Decide(bash, layered); d.Decision != "block" || d.Rule != "u-deny" || d.Layer != LayerUser {
		t.Fatalf("layered: the user deny must win and say its layer: %+v", d)
	}
	// The org rule alone asks, and says the organization layer.
	if d := Decide(bash, org); d.Decision != "block" || d.Rule != "o-ask" || d.Layer != LayerOrganization {
		t.Fatalf("org-only ask: %+v", d)
	}
	// A probe nothing matches allows with no rule and no layer — never a guessed tier.
	if d := Decide([]Tag{{Key: "tool", Value: "ls"}}, layered); d.Decision != "allow" || d.Rule != "" || d.Layer != "" {
		t.Fatalf("unmatched probe must carry no layer: %+v", d)
	}
}
