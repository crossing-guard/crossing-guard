package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/internal/detectorselection"
	"crossing-guard/internal/platform"
)

type coverageBody struct {
	Engine        []engine.RuleBoundary `json:"engine"`
	Error         string                `json:"error"`
	Note          string                `json:"note"`
	Rulebook      map[string]any        `json:"rulebook"`
	Compatibility map[string]any        `json:"compatibility"`
	Legacy        any                   `json:"legacy"`
}

func getCoverage(t *testing.T) (coverageBody, map[string]engine.RuleBoundary) {
	t.Helper()
	rec := httptest.NewRecorder()
	handlePolicyCoverage(rec, httptest.NewRequest("GET", "/api/policy/coverage", nil))
	var body coverageBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	by := map[string]engine.RuleBoundary{}
	for _, b := range body.Engine {
		by[b.Rule] = b
	}
	return body, by
}

// swapCoverageGlobals isolates the route's package state and HOME (the rulebook's
// install profile lives there; CG_RULES names the document).
func swapCoverageGlobals(t *testing.T, g *Governor, dets *detectorselection.LoadedDetectors, policyPath string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	priorGov, priorDets, priorPath := governor, policyDetectorRuntime.governor, daemonPolicyPath
	governor, policyDetectorRuntime.governor, daemonPolicyPath = g, dets, policyPath
	t.Cleanup(func() { governor, policyDetectorRuntime.governor, daemonPolicyPath = priorGov, priorDets, priorPath })
}

// The route labels the rulebook the stateful tier loads NOW, not a copy frozen at
// start-up, and reports the daemon's compatibility file without labeling it
// (stateful-tier reach plan D-5, red-team RT-3).
func TestPolicyCoverageLabelsTheLiveRulebook(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "rules.json")
	write := func(body string) {
		if err := os.WriteFile(rules, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"rules":[{"id":"first","action":"deny","if":{"tag":"command","matches":"x"}}]}`)
	t.Setenv("CG_RULES", rules)
	g := statefulGovernor(t)
	compat := filepath.Join(t.TempDir(), "policy-engine.json")
	if err := os.WriteFile(compat, []byte(`{"rules":[{"id":"ledger-only","action":"deny","if":{"tag":"command","matches":"y"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	swapCoverageGlobals(t, g, nil, compat)

	body, by := getCoverage(t)
	if _, ok := by["first"]; !ok || body.Rulebook["path"] != rules {
		t.Fatalf("rulebook not labeled: %+v", body)
	}
	if _, ok := by["ledger-only"]; ok || body.Compatibility["labeled"] != false || body.Compatibility["path"] != compat {
		t.Fatalf("compatibility file must be reported, not labeled: %+v", body)
	}
	if body.Legacy != nil {
		t.Fatalf("the stale legacy block is gone: %+v", body.Legacy)
	}

	write(`{"rules":[{"id":"second","action":"ask","if":{"tag":"session:vcs","value":"push-force"}}]}`)
	_, by = getCoverage(t)
	if _, ok := by["second"]; !ok || len(by) != 1 {
		t.Fatalf("an edited rulebook must be labeled without a restart: %+v", by)
	}
	if b := by["second"]; !b.CanFire || b.Reach != engine.ReachStop {
		t.Fatalf("armed governor: gating state rule at STOP reach, got %+v", b)
	}
}

// On a platform whose stateful tier is not armed, the label and the evaluator agree:
// DecideStateful returns nil, and the rule is labeled at the unarmed static reach,
// where no rule that reads state can fire, positive or negated (RT-6).
func TestPolicyCoverageUnarmedPlatform(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(rules, []byte(`{"rules":[
	  {"id":"positive","action":"deny","if":{"tag":"session:vcs","value":"push-force"}},
	  {"id":"negated","action":"ask","if":{"not":{"tag":"agent:b1:reviewed"}}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	g := statefulGovernor(t)
	g.platform = platform.For("linux")
	swapCoverageGlobals(t, g, nil, "")

	if d, _, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash", Command: "git push --force", TS: 1}); err != nil || d != nil {
		t.Fatalf("unarmed stateful tier must not decide: %+v %v", d, err)
	}
	_, by := getCoverage(t)
	if b := by["positive"]; b.CanFire || b.Unverified || b.Reach != engine.ReachStopUnarmed {
		t.Errorf("positive state rule on an unarmed platform: %+v", b)
	}
	// The static tiers skip every rule that reads state, so a negated one cannot fire
	// there either (it read as firing before that fix landed).
	if b := by["negated"]; b.CanFire || b.Unverified || b.Reach != engine.ReachStopUnarmed {
		t.Errorf("negated state rule on an unarmed platform: %+v", b)
	}
}

// No governor and no resolved governor detector set: never label with no detectors
// (every detector-backed rule would read a false INERT) — report the error instead
// (red-team RT-2). A broken rulebook is reported, not labeled.
func TestPolicyCoverageRefusesToGuess(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(rules, []byte(`{"rules":[{"id":"r","action":"deny","if":{"tag":"vcs","value":"push-force"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	swapCoverageGlobals(t, nil, nil, "")
	if body, _ := getCoverage(t); body.Engine != nil || body.Error == "" {
		t.Fatalf("no detectors: %+v", body)
	}
	// With detectors resolved, a broken rulebook is its own error, not the detector one.
	dets, err := engine.DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	policyDetectorRuntime.governor = &detectorselection.LoadedDetectors{Detectors: dets}
	if err := os.WriteFile(rules, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if body, _ := getCoverage(t); body.Engine != nil || !strings.HasPrefix(body.Error, "rulebook: ") {
		t.Fatalf("broken rulebook: %+v", body)
	}
}

// O-3 on the route: a rule only the hook's engine tier could fire is UNVERIFIED here —
// the daemon cannot see a hook's invocation file — never INERT and never can-fire. A
// tool ∧ state rule is the stateful tier's alone, and that tier has the tool identity
// (TestStatefulTierHasTheToolTag proves the evaluator fires it), so it can fire here.
func TestPolicyCoverageLabelsPerTier(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(rules, []byte(`{"rules":[
	  {"id":"detector-only","action":"deny","if":{"tag":"vcs","value":"push-force"}},
	  {"id":"cross","action":"deny","if":{"all":[{"tag":"tool","value":"Bash"},{"tag":"session:vcs","value":"push-force"}]}},
	  {"id":"stateful","action":"deny","if":{"all":[{"tag":"command","matches":"push"},{"tag":"session:vcs","value":"push-force"}]}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	swapCoverageGlobals(t, statefulGovernor(t), nil, "")
	body, by := getCoverage(t)
	b := by["detector-only"]
	// Both hook tiers carry the detector tags: the rule fires at the standalone tier
	// whatever the engine tier loads, so the label no longer waits on a file this
	// surface cannot see.
	if !b.CanFire || b.Unverified || len(b.Tiers) != 2 ||
		b.Tiers[0].Tier != "hook-engine" || b.Tiers[0].Loads != "unknown" || !b.Tiers[0].CanFire ||
		b.Tiers[1].Tier != "hook-standalone" || !b.Tiers[1].CanFire {
		t.Errorf("detector-only user rule: %+v", b)
	}
	if b := by["cross"]; !b.CanFire || b.Unverified || b.Reach != engine.ReachStop ||
		len(b.Tiers) != 1 || b.Tiers[0].Tier != "daemon-stateful" {
		t.Errorf("tool ∧ state fires in the stateful tier alone: %+v", b)
	}
	if b := by["stateful"]; !b.CanFire || b.Reach != engine.ReachStop {
		t.Errorf("command ∧ state fires in the stateful tier: %+v", b)
	}
	if !strings.Contains(body.Note, "cannot see that file") {
		t.Errorf("route note must say the engine tier's loading is unseen: %q", body.Note)
	}
}
