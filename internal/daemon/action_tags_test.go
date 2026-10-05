package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// A shell command resolves no single target, so there is no entity whose state could
// answer a target: term: the rule is undecided and counted, it does not fire. (It fired
// on every shell call while the absent target state read as "not present".) An action
// that does resolve a target is judged over that target's state.
func TestStatefulTargetTermIsUndecidedWithoutATarget(t *testing.T) {
	tierRules(t, `{"rules":[
	  {"id":"shell-unscanned","action":"deny",
	   "if":{"all":[{"tag":"command","matches":"zzq"},{"not":{"tag":"target:secret","value":"aws-key"}}]}},
	  {"id":"write-unscanned","action":"deny",
	   "if":{"all":[{"tag":"tool","value":"Write"},{"not":{"tag":"target:secret","value":"aws-key"}}]}}
	]}`)
	g := statefulGovernor(t)
	d, undecided, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash", Command: "zzq now", TS: 10})
	if err != nil || d != nil || undecided != 1 {
		t.Fatalf("shell call: decision=%+v undecided=%d err=%v", d, undecided, err)
	}
	d, undecided, err = g.DecideStateful(Observation{SessionID: "s", Tool: "Write", FilePath: "/tmp/plain.txt", Content: "hello", TS: 11})
	if err != nil || d == nil || d.Rule != "write-unscanned" || undecided != 0 {
		t.Fatalf("file call: decision=%+v undecided=%d err=%v", d, undecided, err)
	}
}

func TestGovernDecideReportsUndecided(t *testing.T) {
	raw, err := json.Marshal(StatefulDecision{Decision: "allow", Evaluated: true, Undecided: 2})
	if err != nil || !strings.Contains(string(raw), `"undecided":2`) {
		t.Fatalf("%s %v", raw, err)
	}
	if raw, _ := json.Marshal(StatefulDecision{Decision: "allow"}); strings.Contains(string(raw), "undecided") {
		t.Fatalf("zero must be omitted: %s", raw)
	}
}

// The audit judges a whole past session: it has no single command, tool or target, and
// cannot read a session fact the daemon writes live only. A rule decided by such a term
// is never a finding, a decidable branch beside it still reports, and the rule is named
// as not fully audited.
func TestAuditLeavesUnreadableTermsUndecided(t *testing.T) {
	blind := engine.UnknownAtHarvest(store.StateProducersFor(nil, 0))
	held := engine.Predicate{Tag: "session:vcs", Value: "push-force"}
	target := engine.Predicate{Tag: "target:secret", Value: "aws-key"}
	command := engine.Predicate{Tag: engine.CommandTagKey, Matches: "push"}
	rules := []engine.Rule{
		{ID: "not-target", Action: "observe", If: engine.Predicate{Not: &target}},
		{ID: "command-and-state", Action: "observe", If: engine.Predicate{All: []engine.Predicate{command, held}}},
		{ID: "not-command", Action: "observe", If: engine.Predicate{All: []engine.Predicate{held, {Not: &command}}}},
		{ID: "either", Action: "observe", If: engine.Predicate{Any: []engine.Predicate{target, held}}},
		// Decided by the readable half of the negated conjunction: it reports, and only the
		// term the audit actually found absent is named.
		{ID: "not-both", Action: "observe", If: engine.Predicate{Not: &engine.Predicate{All: []engine.Predicate{target, {Tag: "session:area", Value: "docs"}}}}},
		{ID: "readable", Action: "observe", If: engine.Predicate{All: []engine.Predicate{held, {Not: &engine.Predicate{Tag: "session:area", Value: "docs"}}}}},
	}
	tags := []engine.Tag{{Key: "session:vcs", Value: "push-force"}}
	var fired []string
	for _, f := range livePolicyFindings(rules, tags, blind, SessionSummary{ID: "s"}) {
		fired = append(fired, f.Rule)
		if f.Rule == "not-both (armed)" && strings.Join(f.Absent, ",") != "session:area=docs" {
			t.Errorf("not-both absent = %v", f.Absent)
		}
		for _, absent := range f.Absent {
			if strings.HasPrefix(absent, "target:") || strings.HasPrefix(absent, engine.CommandTagKey) {
				t.Errorf("%s lists an unreadable term as absent: %v", f.Rule, f.Absent)
			}
		}
	}
	if got := strings.Join(fired, ","); got != "either (armed),not-both (armed),readable (armed)" {
		t.Fatalf("findings = %s", got)
	}
	if got := strings.Join(notFullyAudited(rules, blind), ","); got != "not-target,command-and-state,not-command,either,not-both" {
		t.Fatalf("not fully audited = %s", got)
	}
	// Unreadable agent keys are undecided too, for that session only.
	agentRule := []engine.Rule{{ID: "unreviewed", Action: "observe", If: engine.Predicate{Not: &engine.Predicate{Tag: "agent:b1:reviewed"}}}}
	if got := livePolicyFindings(agentRule, tags, auditUnknownFor(blind, auditAgentUnavailable), SessionSummary{}); len(got) != 0 {
		t.Fatalf("an unread agent key read as absent: %+v", got)
	}
	if got := livePolicyFindings(agentRule, tags, auditUnknownFor(blind, auditAgentRead), SessionSummary{}); len(got) != 1 {
		t.Fatalf("a read, absent agent key must report: %+v", got)
	}
}

// The agent claim producers are declared for the harvest channel, because the audit
// reads them: the coverage label and the evaluator agree there.
func TestAgentClaimsAreHarvestProducers(t *testing.T) {
	g := statefulGovernor(t)
	sp := store.StateProducersFor(g.ix, 100)
	for _, p := range sp.Producers {
		if strings.HasPrefix(p.Tag, engine.AgentStatePrefix) && !p.Harvest {
			t.Errorf("%s is not a harvest producer", p.Tag)
		}
	}
	if engine.UnknownAtHarvest(sp)(engine.Predicate{Tag: "agent:b1:reviewed"}) {
		t.Fatal("the audit reads agent: keys; they are not unknown there")
	}
}

// The console's dry run names no tool: a deny rule that needs one is counted as
// undecided, and the field is absent when every rule was decided.
func TestPolicyCheckCountsUndecidedRules(t *testing.T) {
	check := func(command string) map[string]any {
		rec := httptest.NewRecorder()
		handlePolicyCheck(rec, httptest.NewRequest(http.MethodPost, "/api/policy/check",
			strings.NewReader(`{"command":"`+command+`"}`)))
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
	  {"id":"bash-zeta","action":"deny","if":{"all":[{"tag":"tool","value":"Bash"},{"tag":"command","matches":"zeta"}]}},
	  {"id":"pure","action":"deny","if":{"tag":"command","matches":"omega"}}
	]}`)
	if res := check("echo zeta"); res["decision"] != "allow" || res["rules_undecided"] != float64(1) {
		t.Fatalf("tool-dependent rule: %v", res)
	}
	if res := check("echo omega"); res["decision"] != "deny" || res["rules_undecided"] != nil {
		t.Fatalf("a decided command: %v", res)
	}
}
