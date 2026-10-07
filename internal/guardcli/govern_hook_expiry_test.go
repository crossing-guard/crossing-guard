package guardcli

import (
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/rulebook"
	"crossing-guard/ruledoc"
)

// Criterion 91 on the hook path (team rest-of-release plan §4.2): a device kept past
// the expiry of a fail-closed bundle blocks governed tool calls with a reason naming
// the organization and the date; a refresh lifts the block with no action; Un-adopt
// lifts it at once. A fail-open bundle stops applying its rules instead. The hook
// reads layers.json and the staged rule document only: no request is made.
func TestGovernHookBlocksPastAFailClosedExpiryNamingTheOrganizationAndDate(t *testing.T) {
	governTestHome(t, `{"rules":[]}`)
	storeDir := dataDir()
	rules := []byte(`{"rules":[{"id":"team-deny","action":"deny","message":"team rule","if":{"tag":"command","matches":"team-bad"}}]}`)
	digest := ruledoc.ContentDigest(rules)
	if _, err := rulebook.StageLayer(storeDir, digest, rules); err != nil {
		t.Fatal(err)
	}
	expired := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	adopt := func(mode string, expires time.Time) {
		t.Helper()
		if err := rulebook.AdoptBundle(storeDir, rulebook.AdoptedBundle{OrganizationID: "org_1", OrganizationName: "Acme",
			Scope: rulebook.ScopeOrganization, BundleID: "bnd_1", Revision: 1, FailureMode: mode, ExpiresAt: expires,
			RulebookDigest: digest, SignedDigest: "sha256:s"}); err != nil {
			t.Fatal(err)
		}
	}
	call := func(command string) GovernDecision {
		pendingObserve = nil
		return governHookPreToolUse(governPreToolCall(command))
	}

	adopt(rulebook.FailClosed, expired)
	blocked := call("ls -la")
	if blocked.Decision != "deny" || !strings.Contains(blocked.Reason, "Acme") || !strings.Contains(blocked.Reason, "30 September 2026") ||
		!strings.Contains(blocked.Reason, "block until renewed") {
		t.Fatalf("past a fail-closed expiry every governed call is blocked, naming the organization and the date: %+v", blocked)
	}

	// Red-team H1: the block is every governed tool call, not shell commands only. A
	// call with no command (a write, an edit, a fetch, a connector tool) carries an
	// empty command value, and the expiry rule must still match it.
	for _, in := range nonShellGovernedCalls() {
		pendingObserve = nil
		if d := governHookPreToolUse(in); d.Decision != "deny" || !strings.Contains(d.Reason, "Acme") ||
			!strings.Contains(d.Reason, "block until renewed") {
			t.Fatalf("past a fail-closed expiry a %s call must be blocked too: %+v", in.ToolName, d)
		}
	}

	// A refresh moves the expiry in the record; the next call is decided by the
	// bundle's own rules again, with no other act.
	if err := rulebook.RefreshTeamLayer(storeDir, "org_1", rulebook.ScopeOrganization,
		rulebook.LayerRefresh{BundleID: "bnd_2", Revision: 2, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if lifted := call("ls -la"); lifted.Decision != "allow" {
		t.Fatalf("a refresh must lift the block with no action: %+v", lifted)
	}
	if own := call("run team-bad now"); own.Decision != "deny" || !strings.Contains(own.Reason, "team-deny") {
		t.Fatalf("the refreshed bundle's own rules decide: %+v", own)
	}

	// Expired again, then Un-adopt: lifted at once.
	adopt(rulebook.FailClosed, expired)
	if again := call("ls -la"); again.Decision != "deny" {
		t.Fatalf("expired again must block: %+v", again)
	}
	if _, err := rulebook.UnadoptBundle(storeDir, "org_1", rulebook.ScopeOrganization); err != nil {
		t.Fatal(err)
	}
	if lifted := call("ls -la"); lifted.Decision != "allow" {
		t.Fatalf("un-adopt must lift the block at once: %+v", lifted)
	}

	// fail-open: past expiry the rules stop applying and nothing is blocked.
	adopt(rulebook.FailOpen, expired)
	if open := call("run team-bad now"); open.Decision != "allow" {
		t.Fatalf("an expired fail-open bundle's rules must stop applying: %+v", open)
	}
}

// nonShellGovernedCalls is one governed call of each kind that carries no shell
// command: a file write, an edit, a web fetch and a connector (MCP) tool.
func nonShellGovernedCalls() []hookInput {
	call := func(tool string, in toolInput) hookInput {
		return hookInput{HookEventName: "PreToolUse", SessionID: "ses_gov", Runtime: "opencode", ToolName: tool, ToolInput: in}
	}
	return []hookInput{
		call("Write", toolInput{FilePath: "/tmp/notes.txt", Content: "hello"}),
		call("Edit", toolInput{FilePath: "/tmp/notes.txt", OldString: "a", NewString: "b"}),
		call("WebFetch", toolInput{URL: "https://example.com/"}),
		call("mcp__files__read_file", toolInput{Path: "/tmp/notes.txt"}),
	}
}
