package harvest

import "testing"

// Which runtime serves each session-scoped kind is published per adapter and
// measured, never assumed (helper-session-attachment plan D1/B2): Claude's
// hooks write all three, OpenCode's plugin writes tool results (idle is
// probe-gated), and Codex publishes none until its canaries record rows.
func TestSessionScopedActivityEvidencePerRuntime(t *testing.T) {
	claude := ActivityEvidence("claude")
	for _, kind := range []string{"session.turn-started", "session.tool-completed", "session.turn-ended"} {
		if claude[kind] != "hook-exact" {
			t.Fatalf("claude %s = %q", kind, claude[kind])
		}
	}
	opencode := ActivityEvidence("opencode")
	if opencode["session.tool-completed"] != "hook-exact" || opencode["session.turn-ended"] != "probe-gated" || opencode["session.turn-started"] != "" {
		t.Fatalf("opencode: %v", opencode)
	}
	codex := ActivityEvidence("codex")
	for _, kind := range []string{"session.turn-started", "session.tool-completed", "session.turn-ended"} {
		if _, served := codex[kind]; served {
			t.Fatalf("codex publishes %s before its canary recorded rows", kind)
		}
	}
}
