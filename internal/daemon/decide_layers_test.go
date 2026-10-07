package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/rulebook"
	"crossing-guard/ruledoc"
)

// The stateful tier evaluates the LAYERED policy (stateful-tier-layered-policy-plan.md):
// a state rule distributed in a repository or organization layer is decided by the
// daemon, with the loader's own layer stamps and precedence, and fails open when its
// layer cannot be read.

const negatedRepoRule = `{"rules":[{"id":"repo-needs-personal","action":"deny",
  "message":"repository rule: egress only from a personal-data session",
  "if":{"all":[{"tag":"command","matches":"EGRESS_CANARY"},
    {"not":{"tag":"session:data-class","value":"personal"}}]}}]}`

// emptyUserRulebook points CG_RULES at a rulebook with no rules, so the team layer
// under test is the only source of stateful rules.
func emptyUserRulebook(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(p, []byte(`{"rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", p)
}

// adoptLayer stages and adopts one team layer through the rulebook's own writers,
// unexpired, and returns the staged document's path.
func adoptLayer(t *testing.T, storeDir, scope, doc string) string {
	t.Helper()
	raw := []byte(doc)
	digest := ruledoc.ContentDigest(raw)
	path, err := rulebook.StageLayer(storeDir, digest, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := rulebook.AdoptBundle(storeDir, rulebook.AdoptedBundle{OrganizationID: "org_1", Scope: scope, RulebookDigest: digest,
		AdoptedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour), FailureMode: "fail-open"}); err != nil {
		t.Fatal(err)
	}
	return path
}

// repoLayerFixture: a checkout (with .git) resolved to repo_1, whose repository layer
// holds doc. Returns the governor (store wired), the checkout and the staged document.
func repoLayerFixture(t *testing.T, doc string) (*Governor, string, string) {
	t.Helper()
	g := statefulGovernor(t)
	g.layerStore = t.TempDir()
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rulebook.SetRepositoryResolution(g.layerStore, checkout, "repo_1"); err != nil {
		t.Fatal(err)
	}
	staged := adoptLayer(t, g.layerStore, "repository:repo_1", doc)
	return g, checkout, staged
}

func egress(session, cwd string) Observation {
	return Observation{SessionID: session, Tool: "Bash", Command: "run EGRESS_CANARY now", Cwd: cwd, TS: 20}
}

func mustBlockRepo(t *testing.T, g *Governor, o Observation) {
	t.Helper()
	d, _, err := g.DecideStateful(o)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Decision != "block" || d.Rule != "repo-needs-personal" || d.Layer != engine.LayerRepository {
		t.Fatalf("the repository layer's state rule must fire and name its tier: %+v", d)
	}
}

func mustProceed(t *testing.T, g *Governor, o Observation, why string) {
	t.Helper()
	d, _, err := g.DecideStateful(o)
	if err != nil {
		t.Fatalf("%s: %v", why, err)
	}
	if d != nil {
		t.Fatalf("%s: %+v", why, d)
	}
}

// T1 — a repository-layer negated state rule fires for a fresh session and stops
// firing once the session has folded the state it names.
func TestStatefulTierEvaluatesRepositoryLayer(t *testing.T) {
	emptyUserRulebook(t)
	g, checkout, _ := repoLayerFixture(t, negatedRepoRule)

	mustBlockRepo(t, g, egress("fresh", checkout))

	if err := g.Observe(Observation{SessionID: "fresh", Tool: "Write",
		Content: "reach the admin at alice@example.com", TS: 10, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	mustProceed(t, g, egress("fresh", checkout), "the folded state must satisfy the negated term")
}

// T2 — a layer that cannot be read contributes nothing (fail-open); a user-layer load
// error surfaces as an error the handler maps to allow/unevaluated.
func TestStatefulTierLayerLoadErrorFailsOpen(t *testing.T) {
	emptyUserRulebook(t)
	g, checkout, staged := repoLayerFixture(t, negatedRepoRule)
	mustBlockRepo(t, g, egress("s", checkout)) // positive control

	if err := os.WriteFile(staged, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustProceed(t, g, egress("s", checkout), "an unloadable staged layer must not decide")

	if err := os.WriteFile(staged, []byte(negatedRepoRule), 0o600); err != nil {
		t.Fatal(err)
	}
	mustBlockRepo(t, g, egress("s", checkout)) // restored: the block is back

	layersDoc, _, _ := rulebook.LayersPathsIn(g.layerStore)
	if err := os.WriteFile(layersDoc, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustProceed(t, g, egress("s", checkout), "unreadable adoption records must not decide")

	bad := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(bad, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", bad)
	d, _, err := g.DecideStateful(egress("s", checkout))
	if err == nil || d != nil {
		t.Fatalf("a user-layer load error must surface (handler fails open), got d=%+v err=%v", d, err)
	}
}

// T3 — only an absolute cwd selects a repository layer. The test runs from inside the
// checkout, so a relative lookup does find the checkout on disk — but its root is spelled
// relatively, and the repository index holds only the absolute roots the daemon
// resolved, so no layer applies.
func TestStatefulTierRelativeCwdSelectsNoRepositoryLayer(t *testing.T) {
	emptyUserRulebook(t)
	g, checkout, _ := repoLayerFixture(t, negatedRepoRule)
	if err := os.MkdirAll(filepath.Join(checkout, "rel", "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(checkout)
	mustBlockRepo(t, g, egress("s", checkout)) // positive control

	mustProceed(t, g, egress("s", ""), "an empty cwd must select no repository layer")
	mustProceed(t, g, egress("s", "rel/dir"), "a relative cwd must select no repository layer")

	// The organization layer is not checkout-scoped: it still decides with no cwd.
	org := `{"rules":[{"id":"org-needs-personal","action":"deny",
	  "if":{"all":[{"tag":"command","matches":"EGRESS_CANARY"},
	    {"not":{"tag":"session:data-class","value":"personal"}}]}}]}`
	adoptLayer(t, g.layerStore, "organization", org)
	d, _, err := g.DecideStateful(egress("s", ""))
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Rule != "org-needs-personal" || d.Layer != engine.LayerOrganization {
		t.Fatalf("the organization layer must still decide without a cwd: %+v", d)
	}
}

// T4 — precedence is the loader's: at equal rank the user layer's rule wins, exactly as
// engine.Decide over the layered policy.
func TestStatefulTierPrecedenceMatchesLoader(t *testing.T) {
	ask := func(id string) string {
		b, _ := json.Marshal(map[string]any{"rules": []any{map[string]any{"id": id, "action": "ask",
			"if": map[string]any{"all": []any{
				map[string]any{"tag": "command", "matches": "EGRESS_CANARY"},
				map[string]any{"not": map[string]any{"tag": "session:data-class", "value": "personal"}}}}}}})
		return string(b)
	}
	user := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(user, []byte(ask("user-ask")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", user)
	g, checkout, _ := repoLayerFixture(t, ask("repo-ask"))

	d, _, err := g.DecideStateful(egress("s", checkout))
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Rule != "user-ask" || d.Layer != engine.LayerUser || d.Mode != engine.ConfirmAndRecord {
		t.Fatalf("the user layer speaks first at equal rank: %+v", d)
	}
	if len(d.AllFired) != 2 {
		t.Fatalf("both layers' rules must be evaluated: %v", d.AllFired)
	}
	pol, _, err := rulebook.LoadLayered(checkout, g.layerStore)
	if err != nil {
		t.Fatal(err)
	}
	want := engine.Decide([]engine.Tag{{Key: engine.CommandTagKey, Value: "run EGRESS_CANARY now"}}, statefulRules(pol))
	if want.Rule != d.Rule || want.Layer != d.Layer {
		t.Fatalf("stateful precedence diverged from the loader's: got %s/%s want %s/%s", d.Rule, d.Layer, want.Rule, want.Layer)
	}
}
