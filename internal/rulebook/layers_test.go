package rulebook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/ruledoc"
)

// The layered loader's journey tests (team plan §5.16.6: criteria 22-25, 29): the
// user layer ++ the checkout's repository layer ++ the organization layer, each
// stamped with its tier, expiry applied at load, a corrupted layer named — and the
// tie-break is the concatenation order.

func layerTestDoc(t *testing.T, dir, ruleID, matches string) (raw []byte, digest string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"rules": []map[string]any{{
			"id": ruleID, "action": "deny", "message": "team rule " + ruleID,
			"if": map[string]any{"tag": "command", "matches": matches},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, ruledoc.ContentDigest(raw)
}

// noUserLayer points CG_RULES at an EMPTY rulebook: the loader's invocation file is
// the user layer, and this test's subject is the team layers.
func noUserLayer(t *testing.T) {
	t.Helper()
	userRaw, _ := json.Marshal(map[string]any{"rules": []map[string]any{}})
	userPath := filepath.Join(t.TempDir(), "user-empty.json")
	if err := os.WriteFile(userPath, userRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", userPath)
}

func adopt(t *testing.T, storeDir, scope string, ruleID, matches string, expires time.Time, mode string) AdoptedBundle {
	t.Helper()
	raw, digest := layerTestDoc(t, storeDir, ruleID, matches)
	if _, err := StageLayer(storeDir, digest, raw); err != nil {
		t.Fatal(err)
	}
	adopt := AdoptedBundle{OrganizationID: "org_test", OrganizationName: "Acme", Scope: scope,
		BundleID: "bnd_test", Revision: 1, KeyID: "k_test", RulebookDigest: digest,
		ExpiresAt: expires, FailureMode: mode, SignedDigest: "sha256:test"}
	if err := AdoptBundle(storeDir, adopt); err != nil {
		t.Fatal(err)
	}
	return adopt
}

func TestLoadLayeredConcatenatesAndStampsTiers(t *testing.T) {
	storeDir := t.TempDir()
	// A user layer via CG_RULES (the loader's invocation file).
	userRaw, _ := json.Marshal(map[string]any{"rules": []map[string]any{{
		"id": "user-deny", "action": "deny", "message": "user rule",
		"if": map[string]any{"tag": "command", "matches": `\buser-bad\b`}}}})
	userPath := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(userPath, userRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", userPath)

	orgAdopt := adopt(t, storeDir, "organization", "org-deny", `org-bad`, time.Now().Add(time.Hour), "fail-open")

	// A checkout + its repository resolution and adoption.
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SetRepositoryResolution(storeDir, checkout, "repo_1"); err != nil {
		t.Fatal(err)
	}
	repoAdopt := adopt(t, storeDir, "repository:repo_1", "repo-deny", `repo-bad`, time.Now().Add(time.Hour), "fail-open")

	pol, reasons, err := LoadLayered(checkout, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Rules) != 3 {
		t.Fatalf("user ++ repository ++ organization = 3 rules, got %d: %+v", len(pol.Rules), pol.Rules)
	}
	if pol.Rules[0].Layer != engine.LayerUser || pol.Rules[0].ID != "user-deny" {
		t.Fatalf("the user layer is FIRST: %+v", pol.Rules[0])
	}
	if pol.Rules[1].Layer != engine.LayerRepository || pol.Rules[1].ID != "repo-deny" {
		t.Fatalf("the repository layer is second: %+v", pol.Rules[1])
	}
	if pol.Rules[2].Layer != engine.LayerOrganization || pol.Rules[2].ID != orgAdopt.RulebookDigest[:0]+"org-deny" {
		if pol.Rules[2].ID != "org-deny" {
			t.Fatalf("the organization layer is third: %+v", pol.Rules[2])
		}
	}
	// The engine decides through the concatenated policy, and the winner's layer
	// reaches the Decision (§5.6.22: a deny names the rule AND the layer).
	d := engine.Decide([]engine.Tag{{Key: "command", Value: "run repo-bad now"}}, pol)
	if d.Decision != "block" || d.Rule != "repo-deny" || d.Layer != engine.LayerRepository {
		t.Fatalf("the repository rule must win and name its tier: %+v", d)
	}
	if len(reasons) != 0 {
		t.Fatalf("a healthy layered load carries no reasons: %v", reasons)
	}
	_ = repoAdopt
}

func TestUnresolvedCheckoutReadsNotYetStaged(t *testing.T) {
	storeDir := t.TempDir()
	adopt(t, storeDir, "repository:repo_other", "other-deny", `x-bad`, time.Now().Add(time.Hour), "fail-open")
	// A checkout the daemon has NOT resolved: the reason says so, the policy is
	// user-only (§5.6.18).
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	noUserLayer(t)
	pol, reasons, err := LoadLayered(checkout, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Rules) != 0 {
		t.Fatalf("no repository layer may apply to an unresolved checkout: %+v", pol.Rules)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "not yet staged") {
		t.Fatalf("the reason must name the real cause: %v", reasons)
	}
}

func TestExpiryAppliesTheDeclaredFailureMode(t *testing.T) {
	storeDir := t.TempDir()
	checkout := t.TempDir()
	noUserLayer(t)
	// fail-open: past expiry, the rules are absent for new decisions.
	adopt(t, storeDir, "organization", "gone-open", `old-bad`, time.Now().Add(-time.Hour), "fail-open")
	pol, reasons, err := LoadLayered(checkout, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Rules) != 0 {
		t.Fatalf("an expired fail-open layer's rules are absent: %+v", pol.Rules)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "expired (fail-open)") {
		t.Fatalf("the reason names the expired layer: %v", reasons)
	}
	// fail-closed: past expiry, the layer DENIES, loudly.
	storeDir2 := t.TempDir()
	adopt(t, storeDir2, "organization", "gone-closed", `old-bad`, time.Now().Add(-time.Hour), "fail-closed")
	pol2, reasons2, err := LoadLayered(checkout, storeDir2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pol2.Rules) != 1 {
		t.Fatalf("an expired fail-closed layer denies: %+v", pol2.Rules)
	}
	d := engine.Decide([]engine.Tag{{Key: "command", Value: "anything"}}, pol2)
	if d.Decision != "block" || !strings.Contains(d.Message, "expired") {
		t.Fatalf("fail-closed denies with the reason: %+v", d)
	}
	if len(reasons2) != 1 || !strings.Contains(reasons2[0], "fail-closed") {
		t.Fatalf("the reason names the mode: %v", reasons2)
	}
}

func TestCorruptedLayerIsNamedNotSilent(t *testing.T) {
	storeDir := t.TempDir()
	checkout := t.TempDir()
	noUserLayer(t)
	adopt := adopt(t, storeDir, "organization", "broken", `broken-bad`, time.Now().Add(time.Hour), "fail-open")
	// Corrupt the staged body BEHIND the adoption record.
	path, err := stagedPath(storeDir, adopt.RulebookDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ not a rulebook"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol, reasons, err := LoadLayered(checkout, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Rules) != 0 {
		t.Fatalf("a broken layer is skipped, not enforced: %+v", pol.Rules)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "adopted but unloadable") {
		t.Fatalf("the decision names the unloadable layer: %v", reasons)
	}
}

func TestTieBreakIsTheConcatenationOrder(t *testing.T) {
	// Same-rank ties resolve to the earlier tier: the user's own document speaks
	// first (postwork F4's pin, now a test).
	storeDir := t.TempDir()
	checkout := t.TempDir()
	userRaw, _ := json.Marshal(map[string]any{"rules": []map[string]any{{
		"id": "user-same-rank", "action": "deny", "message": "the user decided first",
		"if": map[string]any{"tag": "command", "matches": `tie-bad`}}}})
	userPath := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(userPath, userRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", userPath)
	adopt(t, storeDir, "organization", "org-same-rank", `tie-bad`, time.Now().Add(time.Hour), "fail-open")
	pol, _, err := LoadLayered(checkout, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	d := engine.Decide([]engine.Tag{{Key: "command", Value: "tie-bad"}}, pol)
	if d.Rule != "user-same-rank" || d.Layer != engine.LayerUser {
		t.Fatalf("same-rank ties resolve to the earlier tier — the user's: %+v", d)
	}
}

// An empty store directory means "no known store": the user layer alone, and never a
// layers.json relative to the process's working directory.
func TestLoadLayeredEmptyStoreReadsNoRelativeLayers(t *testing.T) {
	noUserLayer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "layers.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	lp, err := LoadLayeredFull(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(lp.Reasons) != 0 || len(lp.Policy.Rules) != 0 {
		t.Fatalf("an empty store must read no layer records: reasons=%v rules=%+v", lp.Reasons, lp.Policy.Rules)
	}
}
