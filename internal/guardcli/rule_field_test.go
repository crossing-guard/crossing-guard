package guardcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"crossing-guard/internal/observation"
)

// spooledEnvelopes returns every observation the decision path wrote, oldest first.
func spooledEnvelopes(t *testing.T) []observation.Envelope {
	t.Helper()
	entries, err := os.ReadDir(observationSpoolDir())
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	var out []observation.Envelope
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(observationSpoolDir(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var e observation.Envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].QueuedAt < out[j].QueuedAt })
	return out
}

func TestTheDecidingRuleIsAFieldOnTheObservation(t *testing.T) {
	for _, tc := range []struct {
		name, command, wantDecision, wantRule string
	}{
		{"hard deny", "run alpha now", "deny", "deny-alpha"},
		{"confirm-class deny in a lane that cannot prompt", "run bravo now", "deny", "ask-bravo"},
		{"no rule", "echo harmless", "allow", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			governTestHome(t, governTestRules)
			governHookPreToolUse(governPreToolCall(tc.command))
			got := spooledEnvelopes(t)
			if len(got) != 1 {
				t.Fatalf("want one spooled observation, got %d", len(got))
			}
			if got[0].Decision != tc.wantDecision || got[0].Rule != tc.wantRule {
				t.Fatalf("decision=%q rule=%q, want %q/%q (reason %q)", got[0].Decision, got[0].Rule,
					tc.wantDecision, tc.wantRule, got[0].Reason)
			}
		})
	}
}

// Standing enforcement down records an ALLOW whose reason says what would have been
// blocked. The rule that would have blocked it is exactly what "what did turning it off
// cost me?" needs as a field.
func TestAWouldBlockAllowStillNamesItsRule(t *testing.T) {
	governTestHome(t, governTestRules)
	if err := os.MkdirAll(filepath.Dir(enforcementFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enforcementFile(), []byte("maintenance window"), 0o644); err != nil {
		t.Fatal(err)
	}
	governHookPreToolUse(governPreToolCall("run alpha now"))
	got := spooledEnvelopes(t)
	if len(got) != 1 || got[0].Decision != "allow" || got[0].Rule != "deny-alpha" {
		t.Fatalf("want allow carrying deny-alpha: %+v", got)
	}
}

// The ask flow writes TWO events: the hold, then its resolution. The resolution's prose
// ("confirmed by user", "override: …") names no rule, so before this field an allowed
// override was unattributable. Both flushes must carry the rule.
func TestAskThenResolutionBothCarryTheRule(t *testing.T) {
	governTestHome(t, governTestRules)
	observeAttempt(governPreToolCall("run bravo now"))
	stageRule("ask-bravo")
	observeAsk("held for human: rule ask-bravo")
	observeDecision("allow", "confirmed by user")
	got := spooledEnvelopes(t)
	if len(got) != 2 {
		t.Fatalf("want the hold and the resolution, got %d", len(got))
	}
	decisions := map[string]observation.Envelope{}
	for _, e := range got {
		if e.Rule != "ask-bravo" {
			t.Fatalf("%s event lost its rule: %+v", e.Decision, e)
		}
		decisions[e.Decision] = e
	}
	resolution, ok := decisions["allow"]
	if _, held := decisions["ask"]; !ok || !held {
		t.Fatalf("want one ask and one allow: %+v", got)
	}
	if strings.Contains(resolution.Reason, "ask-bravo") {
		t.Fatal("test premise: the resolution's prose does not name the rule — the field is the only carrier")
	}
}

// An envelope without a rule must serialize exactly as it did before the field existed,
// or every spool file waiting to replay changes digest and is quarantined as a collision.
func TestAnEnvelopeWithoutARuleKeepsItsDigest(t *testing.T) {
	e := observation.Envelope{Schema: "v1", ObservationID: "obs_1", CollectorID: "c", SessionID: "s", Tool: "Bash",
		Decision: "allow", Reason: "no rule matched", TS: 5, ToolInputCompleteness: "unavailable", QueuedAt: 5}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"rule"`) {
		t.Fatalf("an empty rule must be omitted: %s", raw)
	}
	before, _ := e.Digest()
	e.Rule = "deny-alpha"
	after, _ := e.Digest()
	if before == after {
		t.Fatal("the rule is evidence and must be inside the digest")
	}
}
