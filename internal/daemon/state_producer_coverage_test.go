package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/internal/detectorselection"
	"crossing-guard/store"
)

// The coverage route and the stateful tier agree on the two non-detector producers
// (state-producer-declarations plan §7): a model-claimed tag from a saved binding and
// the daemon's uncommitted-work fact are labeled able to fire, and DecideStateful fires
// on them. Before the declarations both read INERT while the tier fired.
func TestCoverageRouteAgreesWithStatefulTierOnDeclaredProducers(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "rules.json")
	doc := `{"rules":[
	  {"id":"ask-claimed","action":"ask","message":"claimed","if":{"all":[
	    {"tag":"command","matches":"CLAIM_CANARY"},{"tag":"agent:managed-follower:reviewed"}]}},
	  {"id":"ask-wip","action":"ask","message":"wip","if":{"all":[
	    {"tag":"command","matches":"WIP_CANARY"},{"tag":"session:work","value":"uncommitted"}]}},
	  {"id":"ask-ghost","action":"ask","message":"ghost","if":{"tag":"agent:nobody:reviewed"}}
	]}`
	if err := os.WriteFile(rules, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	g := statefulGovernor(t)

	binding := store.ManagedBinding{BindingID: "managed-follower", State: "enabled", Role: "follower", ProjectRoot: "/repo",
		ProfileID: "follower", ProfileSourceDigest: "sha256-v1:source", ProfileBundleDigest: "sha256-v1:bundle",
		Runtime: "codex", Authority: []string{"draft-reply"}, AllowedProfiles: []store.ManagedProfileRef{},
		DeclaredTags: []string{"reviewed"}}
	saved, err := g.ix.PutManagedBinding(binding, store.ManagedBindingAbsentToken(binding.BindingID), 1)
	if err != nil {
		t.Fatal(err)
	}
	// The route reloads the rulebook itself (stateful-tier reach plan D-5).
	priorGov, priorDets := governor, policyDetectorRuntime.governor
	governor = g
	policyDetectorRuntime.governor = &detectorselection.LoadedDetectors{Detectors: g.dets}
	t.Cleanup(func() { governor, policyDetectorRuntime.governor = priorGov, priorDets })

	labels := func() map[string]engine.RuleBoundary {
		rec := httptest.NewRecorder()
		handlePolicyCoverage(rec, httptest.NewRequest("GET", "/api/policy/coverage", nil))
		var body struct {
			Engine []engine.RuleBoundary `json:"engine"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]engine.RuleBoundary{}
		for _, b := range body.Engine {
			out[b.Rule] = b
		}
		return out
	}
	byRule := labels()
	for rule, canFire := range map[string]bool{"ask-claimed": true, "ask-wip": true, "ask-ghost": false} {
		b := byRule[rule]
		if b.CanFire != canFire || b.Unverified {
			t.Errorf("%s: can_fire=%v unverified=%v label=%q, want can_fire=%v", rule, b.CanFire, b.Unverified, b.Label, canFire)
		}
	}

	// The evaluator fires where the label said it can.
	group := store.ManagedGroup{GroupID: "org_c", BindingID: saved.BindingID, State: "active", RootTaskID: "t",
		RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 2, UpdatedAt: 2}
	run := store.ManagedRun{RunID: "run_c", IdempotencyKey: "idem_c", GroupID: group.GroupID, BindingID: saved.BindingID,
		BindingStateToken: saved.StateToken, Role: "follower", ProfileID: saved.ProfileID,
		ProfileSourceDigest: saved.ProfileSourceDigest, ProfileBundleDigest: saved.ProfileBundleDigest,
		SourceTaskID: "t", SourceEventID: 1, AdmittedAt: 2, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := g.ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	if err := g.ix.PutOrchestrationTags([]store.OrchestrationTag{{TagID: "tag_c", RunID: run.RunID,
		BindingID: saved.BindingID, AgentKey: "agent:managed-follower:reviewed", Tag: "reviewed", SessionID: "s", AppliedAt: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := g.ix.UpsertSessionStateDirect("s", store.FactUncommittedWork, "change_record=1", 2); err != nil {
		t.Fatal(err)
	}
	for command, rule := range map[string]string{"run CLAIM_CANARY": "ask-claimed", "run WIP_CANARY": "ask-wip"} {
		d, _, err := g.DecideStateful(Observation{SessionID: "s", Tool: "Bash", Command: command, TS: 20})
		if err != nil {
			t.Fatal(err)
		}
		if d == nil || d.Rule != rule || d.Mode != engine.ConfirmAndRecord {
			t.Errorf("%s: decision %+v, want %s ask", command, d, rule)
		}
	}

	// With no governor no stateful tier runs, so no state reaches either rule: both are
	// labeled at the "not running" static reach and cannot fire there (stateful-tier
	// reach plan D-5) — known, not unverified.
	governor = nil
	byRule = labels()
	for _, rule := range []string{"ask-claimed", "ask-wip"} {
		if b := byRule[rule]; b.CanFire || b.Unverified || b.Reach != engine.ReachStopStatefulDown {
			t.Errorf("%s without a governor: %+v", rule, b)
		}
	}
}
