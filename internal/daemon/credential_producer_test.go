package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/ruledoc"
)

// awsKeySample is a synthetic AWS access key id shape; it matches secret.aws-key and
// data.credential and is not a real key.
const awsKeySample = "AKIAIOSFODNN7EXAMPLE"

// credentialAskPolicy arms public recipe 3 (docs/public/how-to/recipes.md): ask on an
// external destination after the session accumulated credential-material evidence.
func credentialAskPolicy(t *testing.T) {
	t.Helper()
	rules := filepath.Join(t.TempDir(), "rules.json")
	doc := `{"rules":[{"id":"review-external-after-credential-evidence","action":"ask",
	  "message":"credential evidence, then an external destination",
	  "if":{"all":[
	    {"tag":"session:data-class","value":"credential-material"},
	    {"tag":"destination-class","value":"external"}]}}]}`
	if err := os.WriteFile(rules, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
}

// With the starter, writing a credential folds data-class=credential-material onto the
// session (evidence redacted), and recipe 3 then asks on an external fetch. Before
// data.credential no shipped detector produced that fact, so the rule never fired.
func TestCredentialEvidenceFoldsAndArmsRecipeThree(t *testing.T) {
	credentialAskPolicy(t)
	g := statefulGovernor(t)
	if err := g.Observe(Observation{SessionID: "s", Tool: "Write", FilePath: "/tmp/p/.env",
		Content: "AWS_ACCESS_KEY_ID=" + awsKeySample, TS: 10, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	state, err := g.SessionState("s")
	if err != nil {
		t.Fatal(err)
	}
	folded := false
	for _, s := range state {
		if strings.Contains(s.Evidence, awsKeySample) {
			t.Fatalf("raw key stored in session state: %+v", s)
		}
		if s.Key == "data-class" && s.Value == "credential-material" && s.Detector == "data.credential" {
			folded = true
		}
	}
	if !folded {
		t.Fatalf("credential-material not folded: %+v", state)
	}

	d, _, err := g.DecideStateful(Observation{SessionID: "s", Tool: "WebFetch",
		URL: "https://paste.example.net/upload", TS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Mode != engine.ConfirmAndRecord || d.Rule != "review-external-after-credential-evidence" {
		t.Fatalf("recipe 3 did not ask: %+v", d)
	}

	// Failure paths: an in-house destination in the same session, and an external one in
	// a session that never saw a credential, both proceed.
	for _, o := range []Observation{
		{SessionID: "s", Tool: "WebFetch", URL: "http://127.0.0.1:8080/x", TS: 30},
		{SessionID: "clean", Tool: "WebFetch", URL: "https://paste.example.net/upload", TS: 30},
	} {
		if d, _, err := g.DecideStateful(o); err != nil || d != nil {
			t.Fatalf("%s %s: want proceed, got %+v err=%v", o.SessionID, o.URL, d, err)
		}
	}
}

// The shipped observe-credential-external-egress rule reports in the audit dry-run for a
// transcript that wrote a key and fetched an external URL, and stays quiet without the key.
func TestAuditReportsShippedCredentialEgressRule(t *testing.T) {
	detectors, err := engine.DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	if err := initAudit(detectors); err != nil {
		t.Fatal(err)
	}
	pol, err := ruledoc.Parse(ruledoc.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	rules := statefulRules(pol).Rules
	fetch := harvest.CanonicalEvent{Kind: "tool_call", Name: "WebFetch", Text: `{"url":"https://paste.example.net/upload"}`}
	findings := func(write string) []string {
		detail := &SessionDetail{}
		detail.Events = []harvest.CanonicalEvent{
			{Kind: "tool_call", Name: "Write", Text: `{"file_path":"/tmp/p/.env","content":"` + write + `"}`},
			fetch,
		}
		var out []string
		for _, f := range livePolicyFindings(rules, sessionTags(detail), nil, SessionSummary{}) {
			out = append(out, f.Rule)
		}
		return out
	}
	const want = "observe-credential-external-egress (armed)"
	if got := findings("KEY=" + awsKeySample); !slices.Contains(got, want) {
		t.Fatalf("findings %v, want %s", got, want)
	}
	if got := findings("KEY=placeholder"); slices.Contains(got, want) {
		t.Fatalf("finding without credential evidence: %v", got)
	}
}
