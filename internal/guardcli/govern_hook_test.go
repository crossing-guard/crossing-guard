package guardcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// governTestHome sandboxes every durable surface the decision path touches
// (rules, spool, decisions log, daemon rendezvous) so the tests never read or
// write this machine's real governance state. With no daemon-addr file the
// stateful consult returns nil — the static tiers are what these tests pin.
func governTestHome(t *testing.T, rulesDoc string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(rules, []byte(rulesDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	t.Setenv("CG_GOVERN", "") // never consult a live daemon from a test
	pendingObserve = nil
	t.Cleanup(func() { pendingObserve = nil })
}

func governPreToolCall(command string) hookInput {
	return hookInput{HookEventName: "PreToolUse", SessionID: "ses_gov", Runtime: "opencode",
		ToolName: "Bash", ToolInput: toolInput{Command: commandField(command)}}
}

const governTestRules = `{"rules":[
  {"id":"deny-alpha","action":"deny","message":"alpha is restricted","if":{"tag":"command","matches":"alpha"}},
  {"id":"ask-bravo","action":"ask","message":"bravo needs a human","if":{"tag":"command","matches":"bravo"}}
]}`

func TestGovernOpenCodeDenyIsRecordedInTheSpool(t *testing.T) {
	governTestHome(t, governTestRules)
	decision := governHookPreToolUse(governPreToolCall("run alpha now"))
	if decision.Decision != "deny" || !strings.Contains(decision.Reason, "deny-alpha") {
		t.Fatalf("restricted command not denied: %+v", decision)
	}
	// The staged observation must flush WITH the decision — a denied OpenCode
	// call never reaches the vendor transcript, so the spool is the only
	// durable trace that the action ever existed.
	spool, err := os.ReadDir(observationSpoolDir())
	if err != nil || len(spool) == 0 {
		t.Fatalf("deny left no spooled observation (err=%v entries=%d)", err, len(spool))
	}
}

func TestGovernOpenCodeAskClassDeniesWithRecordedReason(t *testing.T) {
	governTestHome(t, governTestRules)
	decision := governHookPreToolUse(governPreToolCall("run bravo now"))
	if decision.Decision != "deny" {
		t.Fatalf("confirm-class rule did not fail closed: %+v", decision)
	}
	if !strings.Contains(decision.Reason, "confirm-class") || !strings.Contains(decision.Reason, "cannot prompt") {
		t.Fatalf("ask denial does not say why it could not prompt: %q", decision.Reason)
	}
}

func TestGovernOpenCodeAllowsWhenNoRuleMatches(t *testing.T) {
	governTestHome(t, governTestRules)
	decision := governHookPreToolUse(governPreToolCall("echo harmless"))
	if decision.Decision != "allow" || decision.Error != "" {
		t.Fatalf("harmless command not allowed cleanly: %+v", decision)
	}
}

func TestGovernOpenCodeHonorsEnforcementSwitch(t *testing.T) {
	governTestHome(t, governTestRules)
	if err := os.MkdirAll(filepath.Dir(enforcementFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enforcementFile(), []byte("maintenance window"), 0o644); err != nil {
		t.Fatal(err)
	}
	decision := governHookPreToolUse(governPreToolCall("run alpha now"))
	if decision.Decision != "allow" || !strings.Contains(decision.Reason, "would block") {
		t.Fatalf("enforcement-off did not report allow with the would-block reason: %+v", decision)
	}
}

func TestGovernOpenCodeFailsOpenVisiblyWhenRulesAreBroken(t *testing.T) {
	governTestHome(t, `{"rules":[{"id":"broken"`) // present-but-broken file fails the load
	decision := governHookPreToolUse(governPreToolCall("run alpha now"))
	if decision.Decision != "allow" || decision.Error == "" {
		t.Fatalf("broken rules must fail open WITH a recorded error: %+v", decision)
	}
	if !strings.Contains(decision.Reason, "governance error") {
		t.Fatalf("fail-open reason hides the governance error: %q", decision.Reason)
	}
}
