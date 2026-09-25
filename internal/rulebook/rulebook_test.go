package rulebook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
)

func decide(policy *engine.Policy, command string) engine.Decision {
	return engine.Decide([]engine.Tag{{Key: engine.CommandTagKey, Value: command}}, policy)
}

func TestMissingFileUsesSafeShippedDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("CG_RULES", path)
	loaded, err := LoadDocument()
	if err != nil {
		t.Fatal(err)
	}
	policy := loaded.Policy
	if loaded.Path != path || loaded.Origin != "embedded-catalog" || loaded.Selection != "legacy-implicit" || !loaded.Active {
		t.Fatalf("wrong embedded provenance: %+v", loaded)
	}
	if !strings.HasPrefix(loaded.Digest, "sha256:") || len(loaded.Raw) == 0 {
		t.Fatalf("missing content evidence: digest=%q bytes=%d", loaded.Digest, len(loaded.Raw))
	}
	if len(policy.Rules) == 0 {
		t.Fatal("shipped default is empty")
	}
	if got := decide(policy, "git status").Decision; got != "allow" {
		t.Fatalf("benign command = %s, want allow", got)
	}
	if got := decide(policy, "rm -rf /tmp/build").Decision; got == "allow" {
		t.Fatal("shipped default allowed destructive recursive delete")
	}
}

func TestFreshInstallUsesEmptyMechanismBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_RULES", "")
	loaded, err := LoadDocument()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InstallCohort != "c5c-mechanism-first" || loaded.Origin != "mechanism-floor" ||
		loaded.Selection != "unselected-baseline" || loaded.Selected || len(loaded.Policy.Rules) != 0 {
		t.Fatalf("fresh baseline = %+v rules=%d", loaded, len(loaded.Policy.Rules))
	}
	if _, err := os.Stat(filepath.Join(home, ".crossing-guard", "installation.json")); err != nil {
		t.Fatalf("installation profile not recorded: %v", err)
	}
}

func TestFreshPreviewDoesNotWriteInstallationProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_RULES", "")
	loaded, err := LoadDocumentPreview()
	if err != nil || loaded.Origin != "mechanism-floor" {
		t.Fatalf("preview = %+v, %v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".crossing-guard")); !os.IsNotExist(err) {
		t.Fatalf("preview wrote data root: %v", err)
	}
}

func TestPresentEnvironmentFileReportsValidatedContentEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "rules.json")
	raw := []byte(`{"rules":[{"id":"custom","action":"deny","if":{"tag":"command","matches":"alpha"}}]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	loaded, err := LoadDocument()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Path != path || loaded.Origin != "environment-file" || loaded.Selection != "invocation-path" || !loaded.Active || !loaded.Selected {
		t.Fatalf("wrong file provenance: %+v", loaded)
	}
	if string(loaded.Raw) != string(raw) {
		t.Fatalf("raw document changed: got %q want %q", loaded.Raw, raw)
	}
	if got := decide(loaded.Policy, "alpha").Decision; got != "block" {
		t.Fatalf("validated loaded policy did not enforce: %s", got)
	}
}

func TestPresentMalformedFileFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	if _, err := Load(); err == nil {
		t.Fatal("present malformed file fell back silently")
	}
}

func TestShippedCanaryContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", filepath.Join(t.TempDir(), "absent.json"))
	policy, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	decision := decide(policy, "echo "+CanaryMarker)
	if decision.Rule != "canary-deny" {
		t.Fatalf("canary fired %q, want canary-deny", decision.Rule)
	}
	if got := decide(policy, "echo canary in a coal mine").Decision; got != "allow" {
		t.Fatalf("near-miss = %s, want allow", got)
	}
}

func TestShippedObserveRulesAreStatefulReportOnlyAndSelective(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", filepath.Join(t.TempDir(), "absent.json"))
	policy, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]engine.Rule{}
	for _, rule := range policy.Rules {
		byID[rule.ID] = rule
	}
	cases := []struct {
		id       string
		positive []engine.Tag
		negative []engine.Tag
	}{
		{"observe-code-without-plan",
			[]engine.Tag{{Key: "session:fs", Value: "write"}},
			[]engine.Tag{{Key: "session:fs", Value: "write"}, {Key: "session:phase", Value: "plan"}}},
		{"observe-force-push-without-red-team",
			[]engine.Tag{{Key: "session:vcs", Value: "push-force"}},
			[]engine.Tag{{Key: "session:vcs", Value: "push-force"}, {Key: "session:phase", Value: "red-team"}}},
		{"observe-credential-external-egress",
			[]engine.Tag{{Key: "session:data-class", Value: "credential-material"}, {Key: "session:destination-class", Value: "external"}},
			[]engine.Tag{{Key: "session:data-class", Value: "credential-material"}}},
	}
	for _, tc := range cases {
		rule, ok := byID[tc.id]
		if !ok {
			t.Errorf("missing shipped rule %s", tc.id)
			continue
		}
		if rule.Action != "observe" || !strings.HasPrefix(firstPredicateTag(rule.If), "session:") {
			t.Errorf("%s is not stateful observe-only: %+v", tc.id, rule)
		}
		if !engine.Match(rule.If, tc.positive) {
			t.Errorf("%s missed positive tags", tc.id)
		}
		if engine.Match(rule.If, tc.negative) {
			t.Errorf("%s matched negative tags", tc.id)
		}
	}
}

func firstPredicateTag(predicate engine.Predicate) string {
	if predicate.Tag != "" {
		return predicate.Tag
	}
	for _, child := range predicate.All {
		if tag := firstPredicateTag(child); tag != "" {
			return tag
		}
	}
	for _, child := range predicate.Any {
		if tag := firstPredicateTag(child); tag != "" {
			return tag
		}
	}
	if predicate.Not != nil {
		return firstPredicateTag(*predicate.Not)
	}
	return ""
}
