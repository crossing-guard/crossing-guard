package daemon

import (
	"crossing-guard/engine"
	"strings"
	"testing"
)

func TestSecurityCatalogFeedsReportOnlyRule(t *testing.T) {
	dets, err := engine.SecurityObserveDetectors()
	if err != nil {
		t.Fatal(err)
	}
	floor, err := engine.StructuralDetectors()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := engine.LoadPolicy("../../ruledoc/rules.security-observe.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, text, dest string
		floor, want      bool
	}{
		{"aws", "AKIA" + strings.Repeat("A", 16), "https://example.com/upload", false, true},
		{"pem", "-----BEGIN PRIVATE KEY-----", "https://example.com/upload", false, true},
		{"github", "ghp_" + strings.Repeat("a", 36), "https://example.com/upload", false, true},
		{"bearer", "Authorization: Bearer synthetic-token", "https://example.com/upload", false, true},
		{"no credential", "ordinary text", "https://example.com/upload", false, false},
		{"private destination", "AKIA" + strings.Repeat("A", 16), "http://172.16.2.3/upload", false, false},
		{"ipv6 loopback", "AKIA" + strings.Repeat("A", 16), "http://[::1]/upload", false, false},
		{"floor only", "AKIA" + strings.Repeat("A", 16), "https://example.com/upload", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected := dets
			if tc.floor {
				selected = floor
			}
			tags := engine.Classify(engine.Event{Role: "tool_result", Text: tc.text}, selected)
			tags = append(tags, engine.Classify(engine.Event{Role: "tool_call", Destination: tc.dest}, selected)...)
			decision := engine.Decide(withSessionScope(tags), policy)
			found := false
			for _, id := range decision.AllFired {
				if id == "observe-credential-external-egress" {
					found = true
				}
			}
			if found != tc.want || decision.Decision != "allow" {
				t.Fatalf("observation=%v want=%v decision=%+v", found, tc.want, decision)
			}
		})
	}
}
