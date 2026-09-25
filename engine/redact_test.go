package engine

import (
	"strings"
	"testing"
)

// TestSecretEvidenceNeverEchoesTheMatch pins D8. Evidence is written into the
// append-only event log and the state fold — permanently. The previous rule leaked
// in BOTH directions: a match of <=8 bytes was stored verbatim, and a longer one
// kept its first 4 characters, which is enough to identify a provider from an
// "sk-", "ghp_" or "AKIA" prefix and sometimes the account.
func TestSecretEvidenceNeverEchoesTheMatch(t *testing.T) {
	dets, err := LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	// Fake-but-realistically-shaped credentials. None of these is a live secret.
	for _, tc := range []struct{ name, text, mustNotContain string }{
		{"aws key", "export AWS=AKIAIOSFODNN7EXAMPLE", "AKIA"},
		// 36-char body: the shipped regex requires exactly that, which is the
		// classic ghp_ shape. (Fine-grained "github_pat_…" tokens are longer and
		// differently shaped — an uncovered case, not one this test asserts.)
		{"github token", "ghp_" + strings.Repeat("a", 36), "ghp_"},
		{"bearer", "Authorization: Bearer sk-abcdefghijklmnop", "sk-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tags := Classify(Event{Tool: "Bash", Text: tc.text, Role: "tool_call"}, dets)
			var fired int
			for _, tag := range tags {
				if !strings.HasPrefix(tag.Detector, "secret.") && tag.Key != "data-class" {
					continue
				}
				fired++
				if strings.Contains(tag.Evidence, tc.mustNotContain) {
					t.Fatalf("%s leaked into evidence: %q", tc.mustNotContain, tag.Evidence)
				}
				if !strings.Contains(tag.Evidence, "redacted") {
					t.Fatalf("secret evidence not redacted: %q", tag.Evidence)
				}
			}
			if fired == 0 {
				t.Fatalf("no secret detector fired on %q — the redaction test proves nothing", tc.text)
			}
		})
	}
}

// TestShortMatchIsStillRedacted is the half the old rule got exactly backwards:
// len<=8 returned the match VERBATIM, so the shortest secrets were the least
// protected.
func TestShortMatchIsStillRedacted(t *testing.T) {
	if got := redact("abc"); strings.Contains(got, "abc") {
		t.Fatalf("short match echoed verbatim: %q", got)
	}
}

// TestEvidenceModeDefaultsToRedacted pins the fail-safe default: a detector that
// does not declare "raw" — including one from a user overlay nobody reviewed —
// must not store what it matched.
func TestEvidenceModeDefaultsToRedacted(t *testing.T) {
	undeclared := Detector{ID: "x", Kind: "pattern"}
	if got := patternEvidence(undeclared, "hunter2hunter2"); strings.Contains(got, "hunter2") {
		t.Fatalf("undeclared evidence mode leaked the match: %q", got)
	}
	typo := Detector{ID: "x", Kind: "pattern", Evidence: "RAW"} // not the literal "raw"
	if got := patternEvidence(typo, "hunter2hunter2"); strings.Contains(got, "hunter2") {
		t.Fatalf("a typo in the overlay turned on raw capture: %q", got)
	}
	declared := Detector{ID: "x", Kind: "pattern", Evidence: "raw"}
	if got := patternEvidence(declared, "git commit -m x"); got != "git commit -m x" {
		t.Fatalf("raw evidence lost for a benign detector: %q", got)
	}
}
