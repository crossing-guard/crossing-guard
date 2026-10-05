package guardcli

import (
	"net/http"
	"net/http/httptest"
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

const governWarnRules = `{"rules":[
  {"id":"warn-unreviewed","mode":"warn-and-proceed","message":"always warns (a non-state term: the static tier never evaluates state rules)","if":{"not":{"tag":"command","matches":"^never-a-real-command$"}}},
  {"id":"warn-echo","mode":"warn-and-proceed","message":"echo seen","if":{"tag":"command","matches":"^echo warnme"}},
  {"id":"deny-alpha","action":"deny","message":"alpha is restricted","if":{"tag":"command","matches":"alpha"}},
  {"id":"ask-bravo","action":"ask","message":"bravo needs a human","if":{"tag":"command","matches":"bravo"}}
]}`

// governWarnHome is governTestHome with no invocation policy, so only the standalone
// tier sees the warn rule.
func governWarnHome(t *testing.T) {
	governTestHome(t, governWarnRules)
	t.Setenv("CG_POLICY", filepath.Join(t.TempDir(), "absent-policy.json"))
}

// A warn rule proceeds in the headless lane and is named in the recorded reason;
// before, any non-allow standalone decision denied as "confirm-class".
func TestGovernWarnRuleProceedsNamingTheRule(t *testing.T) {
	governWarnHome(t)
	decision := governHookPreToolUse(governPreToolCall("ls -la"))
	if decision.Decision != "allow" || decision.Error != "" {
		t.Fatalf("warn-only match did not proceed: %+v", decision)
	}
	if decision.Reason != "warn-and-proceed: rule warn-unreviewed" {
		t.Fatalf("allow reason does not name the warn: %q", decision.Reason)
	}
}

func TestGovernEveryFiredWarnIsNamed(t *testing.T) {
	governWarnHome(t)
	d := governHookPreToolUse(governPreToolCall("echo warnme"))
	if d.Decision != "allow" || d.Reason != "warn-and-proceed: rule warn-unreviewed, warn-echo" {
		t.Fatalf("two warns not both named: %+v", d)
	}
}

// With an invocation policy the engine tier warns the user rulebook's rule first; the
// reason names it once, not once per tier.
func TestGovernEngineTierWarnNamedOnce(t *testing.T) {
	governWarnHome(t)
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"rules":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_POLICY", policy)
	d := governHookPreToolUse(governPreToolCall("ls -la"))
	if d.Decision != "allow" || d.Reason != "warn-and-proceed: rule warn-unreviewed" {
		t.Fatalf("engine+static warn not named exactly once: %+v", d)
	}
}

func TestGovernWarnDoesNotMaskAGatingRule(t *testing.T) {
	governWarnHome(t)
	if d := governHookPreToolUse(governPreToolCall("run bravo")); d.Decision != "deny" ||
		!strings.Contains(d.Reason, "confirm-class rule ask-bravo") {
		t.Fatalf("ask beside a warn did not gate: %+v", d)
	}
	pendingObserve = nil
	if d := governHookPreToolUse(governPreToolCall("run alpha")); d.Decision != "deny" ||
		!strings.Contains(d.Reason, "deny-alpha") {
		t.Fatalf("deny beside a warn did not gate: %+v", d)
	}
}

// A warn never ends the pipeline: the stateful consult still runs and its deny wins.
func TestGovernWarnStillConsultsTheStatefulTier(t *testing.T) {
	governWarnHome(t)
	consulted := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/govern/decide" {
			http.NotFound(w, r) // observation posts: tolerated, not under test
			return
		}
		consulted++
		_, _ = w.Write([]byte(`{"decision":"deny","rule":"stateful-unreviewed","message":"not reviewed yet","evaluated":true}`))
	}))
	defer daemon.Close()
	t.Setenv("CG_GOVERN", strings.TrimPrefix(daemon.URL, "http://"))
	d := governHookPreToolUse(governPreToolCall("ls -la"))
	if consulted != 1 {
		t.Fatalf("stateful tier consulted %d times after a warn, want 1", consulted)
	}
	if d.Decision != "deny" || !strings.Contains(d.Reason, "stateful-unreviewed") {
		t.Fatalf("stateful deny after a warn did not gate: %+v", d)
	}
}
