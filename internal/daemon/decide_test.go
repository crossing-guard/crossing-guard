package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// statefulTestPolicy writes a rules file with ONE stateful rule (session:data-class ∧
// a command match) and points CG_RULES at it, so DecideStateful evaluates it rather
// than the live canary file.
func statefulTestPolicy(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules.json")
	doc := `{"rules":[
	  {"id":"no-egress-after-personal","action":"deny",
	   "message":"session touched personal data, then tried to egress",
	   "if":{"all":[
	     {"tag":"session:data-class","value":"personal"},
	     {"tag":"command","matches":"EGRESS_CANARY"}
	   ]}},
	  {"id":"static-rm","action":"deny","if":{"tag":"command","matches":"PURE_STATIC"}}
	]}`
	if err := os.WriteFile(rules, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
}

func statefulGovernor(t *testing.T) *Governor {
	t.Helper()
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)
	// Pin the platform gate to a demonstrated platform so these tests assert the STATEFUL
	// logic on any GOOS — otherwise they'd only run on darwin and go vacuously green
	// (gate closed → rule never evaluated) on the linux CI.
	g.platform = PlatformSupportFor("darwin")
	return g
}

// TestStatefulTierGatedByPlatform pins that the gate itself works: on an undemonstrated
// GOOS the stateful tier never fires, regardless of rules or state.
func TestStatefulTierGatedByPlatform(t *testing.T) {
	statefulTestPolicy(t)
	g := statefulGovernor(t)
	g.platform = PlatformSupportFor("linux") // untested → must not arm
	if err := g.Observe(Observation{SessionID: "s", Tool: "Write",
		Content: "alice@example.com", TS: 10, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	d, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash",
		Command: "run EGRESS_CANARY now", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("stateful tier armed on an undemonstrated platform: %+v", d)
	}
}

// TestStatefulRuleFiresOverAccumulatedState pins the core of Phase 3: a rule that only
// the daemon can evaluate (it references session:data-class) fires once the session has
// accumulated that state and the action matches.
func TestStatefulRuleFiresOverAccumulatedState(t *testing.T) {
	statefulTestPolicy(t)
	g := statefulGovernor(t)

	// Prior action establishes session:data-class=personal (data.email over the body).
	if err := g.Observe(Observation{SessionID: "s", Tool: "Write",
		Content: "reach the admin at alice@example.com", TS: 10, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}

	// The egress action, in the SAME session, is denied by the stateful rule.
	d, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash",
		Command: "run EGRESS_CANARY now", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Decision != "block" || d.Rule != "no-egress-after-personal" {
		t.Fatalf("stateful rule did not fire: %+v", d)
	}
}

// TestFailOpenWhenNoAccumulatedState is the owner's hard requirement, at the daemon
// layer: the SAME action in a session that has NOT accumulated the state does not fire
// — no session:data-class tag exists, so the stateful rule cannot match. nil = proceed.
func TestFailOpenWhenNoAccumulatedState(t *testing.T) {
	statefulTestPolicy(t)
	g := statefulGovernor(t)

	// No prior action → the session has no folded state.
	d, err := g.DecideStateful(Observation{SessionID: "fresh", Tool: "Bash",
		Command: "run EGRESS_CANARY now", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("a session with no accumulated state must not trigger a stateful rule: %+v", d)
	}
}

// TestPureCommandRulesAreNotStateful keeps the tiers separate: a rule that references
// only the command tag is the hook's static tier and must be EXCLUDED from the daemon's
// stateful evaluation, so it is never enforced twice.
func TestPureCommandRulesAreNotStateful(t *testing.T) {
	statefulTestPolicy(t)
	g := statefulGovernor(t)

	// PURE_STATIC matches the static-only rule. The daemon must NOT report it — even
	// though it would "match", it is not a stateful rule.
	d, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash",
		Command: "run PURE_STATIC now", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("a pure command rule must be left to the static tier, got: %+v", d)
	}
}

// TestStatefulRuleDetection pins the classifier that splits the tiers.
func TestStatefulRuleDetection(t *testing.T) {
	stateful := engine.Predicate{All: []engine.Predicate{
		{Tag: "session:data-class", Value: "secrets"},
		{Tag: "net", Value: "egress-external"},
	}}
	if !predicateReferencesState(stateful) {
		t.Error("a predicate over session:* must be detected as stateful")
	}
	pure := engine.Predicate{Tag: "command", Matches: "rm"}
	if predicateReferencesState(pure) {
		t.Error("a pure command predicate must NOT be detected as stateful")
	}
	target := engine.Predicate{Not: &engine.Predicate{Tag: "target:data-class", Value: "public"}}
	if !predicateReferencesState(target) {
		t.Error("a predicate over target:* (even negated) must be detected as stateful")
	}
}

// TestFailOpenOnStateReadError pins the red-team HIGH (corroborated by my own pass): a
// state-read ERROR must fail OPEN — DecideStateful returns an error the handler maps to
// allow/evaluated=false, never a block over a tag set it knows is incomplete.
func TestFailOpenOnStateReadError(t *testing.T) {
	statefulTestPolicy(t)
	g := statefulGovernor(t)
	// Close the store so SessionState errors — simulating a degraded/failing daemon.
	g.ix.Close()

	d, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash",
		Command: "run EGRESS_CANARY now", TS: 20})
	if err == nil {
		t.Fatal("a state-read error must surface (→ handler fails open), not be swallowed into a decision")
	}
	if d != nil {
		t.Fatalf("a degraded state read must NEVER return a block: %+v", d)
	}
}

// TestWarnStatefulRuleDoesNotBlock pins the other HIGH: a warn-mode stateful rule must
// proceed, not collapse into a deny. Authored via a direct Mode (warn has no action).
func TestWarnStatefulRuleDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules.json")
	doc := `{"rules":[{"id":"warn-egress","mode":"warn-and-proceed",
	  "if":{"all":[{"tag":"session:data-class","value":"personal"},{"tag":"command","matches":"EGRESS_CANARY"}]}}]}`
	if err := os.WriteFile(rules, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	g := statefulGovernor(t)
	if err := g.Observe(Observation{SessionID: "s", Tool: "Write",
		Content: "alice@example.com", TS: 10, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	d, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash",
		Command: "run EGRESS_CANARY now", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("a warn-mode stateful rule became a block: %+v", d)
	}
}

// TestAskStatefulRuleStaysOverridable pins that a confirm-and-record (ask) stateful
// rule surfaces as Mode ConfirmAndRecord, so the handler maps it to an overridable ask
// rather than a hard deny.
func TestAskStatefulRuleStaysOverridable(t *testing.T) {
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules.json")
	doc := `{"rules":[{"id":"ask-egress","action":"ask",
	  "if":{"all":[{"tag":"session:data-class","value":"personal"},{"tag":"command","matches":"EGRESS_CANARY"}]}}]}`
	if err := os.WriteFile(rules, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	g := statefulGovernor(t)
	if err := g.Observe(Observation{SessionID: "s", Tool: "Write",
		Content: "alice@example.com", TS: 10, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	d, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash",
		Command: "run EGRESS_CANARY now", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Mode != engine.ConfirmAndRecord {
		t.Fatalf("ask rule did not surface as ConfirmAndRecord (overridable): %+v", d)
	}
}
