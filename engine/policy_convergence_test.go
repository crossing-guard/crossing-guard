package engine

import (
	"errors"
	"testing"
)

// These pin the ADR 0025 §2/§2a primitives: a regex predicate term, the raw-command
// evaluation tag, and the authored `action` → Mode mapping. They are what lets a
// format-2 regex guard live in the one predicate language without a second evaluator.

// TestRegexPredicateTermFiresOverCommandTag is the format-2 conversion in miniature:
// a rule authored as {"tag":"command","matches":"rm -rf"} must deny when the raw
// command carries it, and stay silent otherwise.
func TestRegexPredicateTermFiresOverCommandTag(t *testing.T) {
	pol := &Policy{Rules: []Rule{{
		ID: "destructive-rm", Action: "deny",
		If: Predicate{Tag: CommandTagKey, Matches: `\brm\s+-rf\b`},
	}}}
	if err := CompilePredicates(pol); err != nil {
		t.Fatalf("good pattern rejected: %v", err)
	}

	hit := Decide([]Tag{{Key: CommandTagKey, Value: "rm -rf /tmp/x"}}, pol)
	if hit.Decision != "block" || hit.Rule != "destructive-rm" {
		t.Fatalf("regex guard did not deny: %+v", hit)
	}
	miss := Decide([]Tag{{Key: CommandTagKey, Value: "ls -la"}}, pol)
	if miss.Decision != "allow" {
		t.Fatalf("regex guard fired on a clean command: %+v", miss)
	}
}

// TestActionMapsToMode pins §2a: rules author a flat action; the engine resolves it
// to the severity ladder so the tested menu system survives one authored field.
func TestActionMapsToMode(t *testing.T) {
	cases := map[string]struct {
		action string
		dec    string
		mode   Mode
	}{
		"deny blocks hard":          {"deny", "block", HardBlock},
		"ask blocks overridable":    {"ask", "block", ConfirmAndRecord},
		"redact blocks overridable": {"redact", "block", ConfirmAndRecord},
		"observe proceeds":          {"observe", "allow", SilentLog},
		"allow proceeds":            {"allow", "allow", SilentLog},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			pol := &Policy{Rules: []Rule{{ID: "r", Action: c.action,
				If: Predicate{Tag: "x", Value: "y"}}}}
			d := Decide([]Tag{{Key: "x", Value: "y"}}, pol)
			if d.Decision != c.dec {
				t.Fatalf("action %q → decision %q, want %q", c.action, d.Decision, c.dec)
			}
			// The reported mode is the resolved one, so the console/menu see the ladder.
			if c.dec != "allow" && d.Mode != c.mode {
				t.Fatalf("action %q → mode %q, want %q", c.action, d.Mode, c.mode)
			}
		})
	}
}

// TestDirectModeStillWins keeps ladder-authored policies working: an explicit Mode
// is honored over Action, so existing engine/testdata policies are unaffected.
func TestDirectModeStillWins(t *testing.T) {
	r := Rule{ID: "r", Mode: HardBlock, Action: "observe", If: Predicate{Tag: "x"}}
	if got := r.effectiveMode(); got != HardBlock {
		t.Fatalf("direct Mode not honored: %q", got)
	}
}

// TestBadRegexIsRejectedNotSilent pins the fail-loud contract: an uncompilable
// pattern must be a load error, never a rule that quietly never fires — that silent
// stop-enforcing is precisely what ADR 0025 says a format change must not do.
func TestBadRegexIsRejectedNotSilent(t *testing.T) {
	pol := &Policy{Rules: []Rule{{ID: "broken",
		If: Predicate{Tag: CommandTagKey, Matches: `rm(-rf`}}}} // unbalanced paren
	err := CompilePredicates(pol)
	if err == nil {
		t.Fatal("bad regex accepted — it would silently never fire")
	}
	var bad *BadPatternError
	if !errors.As(err, &bad) || bad.RuleID != "broken" {
		t.Fatalf("error does not name the offending rule: %v", err)
	}
	// A bad pattern at MATCH time (if it ever slips past load) fails safe: matches
	// nothing, never everything.
	if Match(Predicate{Tag: CommandTagKey, Matches: `rm(-rf`},
		[]Tag{{Key: CommandTagKey, Value: "rm -rf /"}}) {
		t.Fatal("uncompilable pattern matched — a bad regex must fail safe, not open")
	}
}

// TestUnknownActionIsRejected pins the red-team HIGH: a rule with an unknown or typo'd
// action must be a LOUD load error, never a silent SilentLog→proceed (which stops it
// enforcing). Valid actions still load.
func TestUnknownActionIsRejected(t *testing.T) {
	bad := &Policy{Rules: []Rule{{ID: "typo", Action: "dney", If: Predicate{Tag: "command", Matches: "x"}}}}
	err := CompilePredicates(bad)
	if err == nil {
		t.Fatal("unknown action 'dney' accepted — the rule would silently never enforce")
	}
	if b, ok := err.(*BadActionError); !ok || b.RuleID != "typo" {
		t.Fatalf("error does not name the offending rule/action: %v", err)
	}
	// "block" is a plausible synonym for deny but is NOT in the vocabulary — reject it.
	if CompilePredicates(&Policy{Rules: []Rule{{ID: "b", Action: "block", If: Predicate{Tag: "c"}}}}) == nil {
		t.Error("'block' accepted — it maps to no Mode and would not enforce")
	}
	for _, a := range []string{"", "deny", "ask", "allow", "observe", "redact"} {
		if err := CompilePredicates(&Policy{Rules: []Rule{{ID: "r", Action: a, If: Predicate{Tag: "c"}}}}); err != nil {
			t.Errorf("valid action %q rejected: %v", a, err)
		}
	}
}
