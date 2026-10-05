package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/ruledoc"
)

func tierRules(t *testing.T, doc string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // rulebook.Load bootstraps an install profile under HOME
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
}

// TestStatefulTierStillFiresNegatedStateTerm: the static tier now leaves a negated
// state rule alone, so the daemon must still own it — it fires where the state is
// absent (the rule's meaning, over a complete tag set) and stops once it folds.
func TestStatefulTierStillFiresNegatedStateTerm(t *testing.T) {
	tierRules(t, `{"rules":[
	  {"id":"egress-needs-personal-review","action":"deny","message":"not yet classified",
	   "if":{"all":[
	     {"tag":"command","matches":"EGRESS_CANARY"},
	     {"not":{"tag":"session:data-class","value":"personal"}}
	   ]}}
	]}`)
	g := statefulGovernor(t)
	d, _, err := g.DecideStateful(Observation{SessionID: "fresh", Tool: "Bash",
		Command: "run EGRESS_CANARY now", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Rule != "egress-needs-personal-review" {
		t.Fatalf("stateful tier did not fire the negated state rule for a fresh session: %+v", d)
	}

	if err := g.Observe(Observation{SessionID: "s", Tool: "Write",
		Content: "reach the admin at alice@example.com", TS: 10, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	d, _, err = g.DecideStateful(Observation{SessionID: "s", Tool: "Bash",
		Command: "run EGRESS_CANARY now", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("negated state rule fired although the state is folded: %+v", d)
	}
}

// TestStatefulTierHasTheToolTag (red-team RT-1): a mixed tool+state rule now lives only
// in the stateful tier, so the daemon must see the same `tool` fact the hook does —
// otherwise `tool=` never matches and `not: tool=` is vacuous here instead.
func TestStatefulTierHasTheToolTag(t *testing.T) {
	tierRules(t, `{"rules":[
	  {"id":"bash-in-fresh-session","action":"deny",
	   "if":{"all":[{"tag":"tool","value":"Bash"},{"not":{"tag":"session:data-class"}}]}},
	  {"id":"not-bash-in-fresh-session","action":"deny",
	   "if":{"all":[{"not":{"tag":"tool","value":"Bash"}},{"not":{"tag":"session:data-class"}},
	     {"tag":"command","matches":"NOT_BASH_CANARY"}]}}
	]}`)
	g := statefulGovernor(t)
	d, _, err := g.DecideStateful(Observation{SessionID: "fresh", Tool: "Bash", Command: "ls", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Rule != "bash-in-fresh-session" {
		t.Fatalf("a tool=Bash term did not match in the stateful tier: %+v", d)
	}
	d, _, err = g.DecideStateful(Observation{SessionID: "fresh", Tool: "Bash",
		Command: "NOT_BASH_CANARY", TS: 21})
	if err != nil {
		t.Fatal(err)
	}
	if d != nil && d.Rule == "not-bash-in-fresh-session" {
		t.Fatalf("`not: tool=Bash` was vacuous for a Bash call: %+v", d)
	}
}

// TestTiersPartitionThePolicy: every rule is judged by exactly one tier — nothing
// enforced twice, nothing dropped by the split — over every shipped rulebook and a
// generated set of nested predicates.
func TestTiersPartitionThePolicy(t *testing.T) {
	var policies []*engine.Policy
	for name, raw := range map[string][]byte{
		"default":          ruledoc.DefaultRules(),
		"safety-starter":   ruledoc.SafetyStarterRules(),
		"security-observe": ruledoc.SecurityObserveRules(),
	} {
		pol, err := ruledoc.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		policies = append(policies, pol)
	}
	leaves := []engine.Predicate{
		{Tag: engine.CommandTagKey, Matches: "x"}, {Tag: engine.ToolTagKey, Value: "Bash"},
		{Tag: "data-class"}, {Tag: "session:phase"}, {Tag: "target:owner"}, {Tag: "agent:b:t"},
	}
	var generated engine.Policy
	for i, a := range leaves {
		for j, b := range leaves {
			a, b := a, b
			for k, p := range []engine.Predicate{
				{All: []engine.Predicate{a, {Not: &b}}},
				{Any: []engine.Predicate{{Not: &engine.Predicate{All: []engine.Predicate{a}}}, b}},
				{Not: &engine.Predicate{Any: []engine.Predicate{a, b}}},
			} {
				generated.Rules = append(generated.Rules, engine.Rule{ID: fmt.Sprintf("g-%d-%d-%d", i, j, k), If: p})
			}
		}
	}
	policies = append(policies, &generated)

	for _, pol := range policies {
		static, stateful := engine.StaticTier(pol).Rules, statefulRules(pol).Rules
		if len(static)+len(stateful) != len(pol.Rules) {
			t.Fatalf("split lost or doubled rules: %d static + %d stateful != %d",
				len(static), len(stateful), len(pol.Rules))
		}
		seen := map[string]bool{}
		for _, r := range append(append([]engine.Rule{}, static...), stateful...) {
			if seen[r.ID] {
				t.Errorf("rule %s is in both tiers", r.ID)
			}
			seen[r.ID] = true
		}
	}
}

// TestPolicyCheckSaysWhatItCouldNotEvaluate (owner D-5): the console's dry run cannot
// judge a state rule, so its answer names how many it left to the daemon — and the
// field is absent when there are none.
func TestPolicyCheckSaysWhatItCouldNotEvaluate(t *testing.T) {
	check := func() map[string]any {
		rec := httptest.NewRecorder()
		handlePolicyCheck(rec, httptest.NewRequest(http.MethodPost, "/api/policy/check",
			strings.NewReader(`{"command":"git pu`+`sh origin main"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	tierRules(t, `{"rules":[
	  {"id":"push-needs-review","action":"deny",
	   "if":{"all":[{"tag":"command","matches":"pu`+`sh"},{"not":{"tag":"agent:b1:reviewed"}}]}}
	]}`)
	res := check()
	if res["decision"] != "allow" || res["state_rules_not_evaluated"] != float64(1) {
		t.Fatalf("check over a negated state rule: %v", res)
	}
	tierRules(t, `{"rules":[{"id":"pure","action":"deny","if":{"tag":"command","matches":"zzz"}}]}`)
	if res := check(); res["state_rules_not_evaluated"] != nil {
		t.Fatalf("field present with no state rules: %v", res)
	}
	// The shipped default carries three OBSERVE state rules: they never block, so a
	// preview has nothing to warn about.
	tierRules(t, string(ruledoc.DefaultRules()))
	if res := check(); res["state_rules_not_evaluated"] != nil {
		t.Fatalf("observe-only state rules counted as unevaluated gates: %v", res)
	}
}
