package daemon

// "No code without a plan", end to end — and the point of this test is what it
// does NOT contain.
//
// There is no new detector, no new tag, no new schema, and no framework change.
// The session already accretes `area=docs` when it touches a document and
// `fs=edit|write` when it changes a file, both from the shipped detection
// library. So the rule is one predicate over state that already exists, and it
// is authored the same way a user would author it: policy JSON.
//
// That is the claim console-understanding-graph.md §4 makes — "a control rule is
// config (detector + policy JSON), never framework code" — under test rather
// than asserted. If this file ever needs a Go change to express a new governance
// rule, the seam has leaked.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/store"
)

type harvestEvent = harvest.CanonicalEvent
type harvestSummary = harvest.SessionSummary

// planRuleJSON is the whole feature. A write, in a session that has not engaged
// with any document, is held for confirmation with a reason that says why.
//
// `ask` rather than `deny` deliberately: "you have not opened a plan" is a
// process smell, not a safety violation, and a gate that blocks an urgent
// one-line fix teaches people to disable the gate. An overridable hold that
// states its reason is the honest strength.
const planRuleJSON = `{"rules":[{
  "id": "code-without-a-plan",
  "action": "ask",
  "if": { "all": [
    { "tag": "fs", "matches": "edit|write" },
    { "not": { "tag": "session:phase", "value": "plan" } }
  ]},
  "reason": "This session has not opened a design doc or plan. Confirm the change is intentional, or open the plan item it implements."
}]}`

func planPolicy(t *testing.T) *engine.Policy {
	t.Helper()
	var pol engine.Policy
	if err := json.Unmarshal([]byte(planRuleJSON), &pol); err != nil {
		t.Fatalf("the rule must be valid policy JSON a user could paste: %v", err)
	}
	if err := engine.CompilePredicates(&pol); err != nil {
		t.Fatalf("the rule must survive the same validation the console applies: %v", err)
	}
	return &pol
}

// armPlanRule installs the rule as the ACTIVE policy for this test only, via the
// same CG_RULES path the hook and daemon resolve. This drives the REAL
// enforcement path rather than a parallel one — a governance test that evaluates
// its own copy of the evaluator proves nothing about what ships.
func armPlanRule(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(planRuleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
}

func planTestGovernor(t *testing.T) (*Governor, func()) {
	t.Helper()
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	dets, err := engine.LoadLayered("") // the shipped library, unmodified
	if err != nil {
		ix.Close()
		t.Fatal(err)
	}
	return NewGovernor(ix, dets), func() { ix.Close() }
}

// TestCodeWithoutAPlanIsHeld is the positive case: a session that goes straight
// to code is stopped and told why.
func TestCodeWithoutAPlanIsHeld(t *testing.T) {
	g, done := planTestGovernor(t)
	defer done()
	sid := "claude/no-plan"

	armPlanRule(t)
	planPolicy(t) // the rule must also be valid as authored

	// The session writes code, having read nothing.
	write := Observation{SessionID: sid, Tool: "Edit", FilePath: "/repo/internal/daemon/thing.go", TS: 10}
	if err := g.Observe(write); err != nil {
		t.Fatal(err)
	}

	d, err := g.DecideStateful(write)
	if err != nil {
		t.Fatalf("a stateful evaluation must not error on a healthy store: %v", err)
	}
	if d == nil {
		t.Fatal("expected the rule to fire on a write with no doc engagement")
	}
	if d.Mode != engine.ConfirmAndRecord {
		t.Errorf("an `ask` rule must be an OVERRIDABLE hold, not a hard block; got mode %v", d.Mode)
	}
	// A denial that cannot explain itself trains people to bypass it.
	if d.Message == "" && d.Rule == "" {
		t.Error("the hold must name the rule or carry its reason")
	}
}

// TestCodeAfterAPlanProceeds is the negative case, and the one that actually
// matters: a rule that fires on everything is not governance, it is noise.
func TestCodeAfterAPlanProceeds(t *testing.T) {
	g, done := planTestGovernor(t)
	defer done()
	sid := "claude/with-plan"

	armPlanRule(t)

	// Same session, but it reads the design doc first — which is all "having a
	// plan" means here, and it is observable rather than asserted.
	for _, o := range []Observation{
		{SessionID: sid, Tool: "Read", FilePath: "/repo/docs/design/thing.md", TS: 1},
		{SessionID: sid, Tool: "Edit", FilePath: "/repo/internal/daemon/thing.go", TS: 2},
	} {
		if err := g.Observe(o); err != nil {
			t.Fatal(err)
		}
	}

	write := Observation{SessionID: sid, Tool: "Edit", FilePath: "/repo/internal/daemon/thing.go", TS: 3}
	d, err := g.DecideStateful(write)
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		rows, _ := g.SessionState(sid)
		t.Errorf("a session that consulted a doc must proceed; got %v. session state: %v", d.Mode, rows)
	}
}

// TestPlanRuleIsPureConfig guards the property the whole exercise is meant to
// demonstrate: the rule reads session state, so it is a rule only the daemon can
// evaluate, and it is expressible without touching the engine.
func TestPlanRuleIsPureConfig(t *testing.T) {
	pol := planPolicy(t)
	if !predicateReferencesState(pol.Rules[0].If) {
		t.Fatal("the rule must reference session state, or the hook's static tier " +
			"would evaluate it and the daemon's fold would be bypassed")
	}
	if got := statefulRules(pol); len(got.Rules) != 1 {
		t.Fatalf("the rule must be selected into the stateful tier, got %d rules", len(got.Rules))
	}
}

// TestPlanRuleFiresInAuditDryRun is the OTHER half of "author once, arm once".
//
// The exact same rule JSON that the live gate enforces (planRuleJSON, which
// reads `session:phase`) must also fire in the post-hoc audit — the dry-run over real
// history a user does BEFORE arming. Before this, the dry-run path exposed only
// bare tags (`area`), so a stateful rule authored for the live gate silently
// matched nothing in the audit, and the two surfaces disagreed about the same
// rule. If this test needs a different rule string from the live test, the
// namespaces have diverged again.
func TestPlanRuleFiresInAuditDryRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	detectors, _ := engine.DefaultDetectors()
	if err := initAudit(detectors); err != nil {
		// Audit uses the live rulebook; no second report-rule file is required.
	}
	armPlanRule(t) // installs planRuleJSON as the active policy via CG_RULES
	live, _, err := livePolicyRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("expected the armed plan rule to load, got %d rules", len(live))
	}

	sum := func(id string) harvestSummary { return harvestSummary{Runtime: "claude", ID: id} }

	// A session that wrote code and never opened a doc — must be flagged.
	blind := &SessionDetail{}
	blind.Events = []harvestEvent{
		{Kind: "tool_call", Name: "Edit", Text: `{"file_path":"/repo/internal/daemon/thing.go"}`},
	}
	if got := livePolicyFindings(live, sessionTags(blind, 0), sum("blind")); len(got) != 1 {
		t.Errorf("a code-only session must be flagged by the armed rule in dry-run, got %d findings", len(got))
	}

	// A session that also read a design doc — must NOT be flagged, or the rule is noise.
	planned := &SessionDetail{}
	planned.Events = []harvestEvent{
		{Kind: "tool_call", Name: "Read", Text: `{"file_path":"/repo/docs/design/thing.md"}`},
		{Kind: "tool_call", Name: "Edit", Text: `{"file_path":"/repo/internal/daemon/thing.go"}`},
	}
	if got := livePolicyFindings(live, sessionTags(planned, 0), sum("planned")); len(got) != 0 {
		t.Errorf("a session that opened a doc must pass the armed rule in dry-run, got %d findings: %+v",
			len(got), got)
	}
}
