package daemon

import (
	"path/filepath"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// TestNormalizeIsTheOneTranslation pins R7. Live capture and the Phase 4 importer must
// derive the SAME canonical action from the same observation. Because tags are frozen
// at observe time, a disagreement between the two is permanent and undetectable after
// the fact — replayed state would silently differ from live state.
//
// The real guarantee is structural (one function, one caller path); this pins the
// behaviour so a second implementation shows up as a failure rather than as drift.
func TestNormalizeIsTheOneTranslation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		o          Observation
		wantVerb   string
		wantTarget string
		wantText   string
	}{
		{"read a file", Observation{Tool: "Read", FilePath: "/repo/.env"},
			"read", "file:/repo/.env", ""},
		{"edit carries the body as text", Observation{Tool: "Edit", FilePath: "/repo/a.go", Content: "secret=x"},
			"write", "file:/repo/a.go", "secret=x"},
		{"command beats content", Observation{Tool: "Bash", Command: "git commit", Content: "ignored"},
			"exec", "", "git commit"},
		{"fetch targets the url host", Observation{Tool: "WebFetch", URL: "https://evil.example.com/x"},
			"egress", "url:evil.example.com", ""},
		{"mcp tool is its own entity", Observation{Tool: "mcp__github__createIssue"},
			"use", "mcp:createIssue", ""},
		{"file wins over url when both present", Observation{Tool: "Read", FilePath: "/repo/a", URL: "https://x.test/y"},
			"read", "file:/repo/a", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := Normalize(tc.o)
			if n.Verb != tc.wantVerb {
				t.Errorf("verb = %q, want %q", n.Verb, tc.wantVerb)
			}
			if n.TargetID != tc.wantTarget {
				t.Errorf("target = %q, want %q", n.TargetID, tc.wantTarget)
			}
			if n.Event.Text != tc.wantText {
				t.Errorf("text channel = %q, want %q", n.Event.Text, tc.wantText)
			}
			if n.Event.Role != "tool_call" {
				t.Errorf("role = %q — a non-tool_call role silently disables every role-scoped detector", n.Event.Role)
			}
		})
	}
}

// TestObserveUsesTheNormalizer proves the LIVE path routes through it rather than
// keeping a private copy: what lands in the event log must equal what Normalize said.
func TestObserveUsesTheNormalizer(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	obs := []Observation{
		{SessionID: "s", Tool: "Read", FilePath: "/repo/.env", TS: 1, Decision: "allow"},
		{SessionID: "s", Tool: "WebFetch", URL: "https://evil.example.com/x", TS: 2, Decision: "allow"},
		{SessionID: "s", Tool: "Bash", Command: "git push", TS: 3, Decision: "allow"},
	}
	for _, o := range obs {
		if err := g.Observe(o); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := ix.EventsForSession("s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != len(obs) {
		t.Fatalf("got %d events, want %d", len(evs), len(obs))
	}
	for i, o := range obs {
		want := Normalize(o)
		if evs[i].Verb != want.Verb {
			t.Errorf("event %d verb = %q, normalizer said %q — the live path has its own copy",
				i, evs[i].Verb, want.Verb)
		}
		if evs[i].TargetEntityID != want.TargetID {
			t.Errorf("event %d target = %q, normalizer said %q",
				i, evs[i].TargetEntityID, want.TargetID)
		}
	}
}
