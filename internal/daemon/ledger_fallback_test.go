package daemon

// initLedger's two arms are the honesty contract for the dev ledger: an
// ABSENT policy-engine.json falls back to the ACTIVE enforcement rules (so the
// ledger decides over what is actually enforced), and a PRESENT-but-broken file
// fails loudly (a bad edit must never silently downgrade to different rules).
// Both were comment-only claims until now; the absent arm additionally hangs on
// error unwrapping (errors.Is, not os.IsNotExist), which is exactly the kind of
// invariant that dies quietly in a refactor.

import (
	"os"
	"path/filepath"
	"testing"

	"crossing-guard/engine"
)

func TestInitLedgerFallsBackToActiveRulesWhenConfigAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// No policy-engine.json at this path; guardcli falls back to its embedded
	// default ruleset, so the ledger must come up with THOSE rules.
	detectors, _ := engine.DefaultDetectors()
	err := initLedger(filepath.Join(dir, "ledger.jsonl"), detectors,
		filepath.Join(dir, "no-policy-engine.json"))
	if err != nil {
		t.Fatalf("an absent engine-ledger config must fall back to the active rules, got: %v", err)
	}
	if enginePolicy == nil || len(enginePolicy.Rules) == 0 {
		t.Fatal("fallback produced no rules — the dev ledger would decide over nothing")
	}
	found := false
	for _, r := range enginePolicy.Rules {
		if r.ID == "destructive-rm" {
			found = true
		}
	}
	if !found {
		t.Error("fallback rules are not the active enforcement set (destructive-rm missing)")
	}
}

func TestInitLedgerFailsLoudOnABrokenConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	broken := filepath.Join(dir, "policy-engine.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	detectors, _ := engine.DefaultDetectors()
	err := initLedger(filepath.Join(dir, "ledger.jsonl"), detectors, broken)
	if err == nil {
		t.Fatal("a present-but-broken config must be a loud error, never a silent fallback to other rules")
	}
}
