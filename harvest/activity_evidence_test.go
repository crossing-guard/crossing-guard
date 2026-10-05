package harvest

import "testing"

// Which runtime serves each session-scoped kind is published per adapter and
// measured, never assumed (helper-session-attachment plan D1/B2): Claude's
// hooks write all three, OpenCode's 1.18 plugin writes tool results and
// transition-deduped session.status turn boundaries (hook-exact), and Codex
// publishes none until its canaries record rows.
func TestSessionScopedActivityEvidencePerRuntime(t *testing.T) {
	claude := ActivityEvidence("claude")
	for _, kind := range []string{"session.turn-started", "session.tool-completed", "session.turn-ended"} {
		if claude[kind] != "hook-exact" {
			t.Fatalf("claude %s = %q", kind, claude[kind])
		}
	}
	opencode := ActivityEvidence("opencode")
	for _, kind := range []string{"session.turn-started", "session.tool-completed", "session.turn-ended"} {
		if opencode[kind] != "hook-exact" {
			t.Fatalf("opencode %s = %q", kind, opencode[kind])
		}
	}
	codex := ActivityEvidence("codex")
	for _, kind := range []string{"session.turn-started", "session.tool-completed", "session.turn-ended"} {
		if _, served := codex[kind]; served {
			t.Fatalf("codex publishes %s before its canary recorded rows", kind)
		}
	}
	anti := ActivityEvidence("antigravity")
	for _, kind := range []string{"session.started", "session.tool-completed", "turn.ended", "session.turn-ended"} {
		if anti[kind] != "hook-exact" {
			t.Fatalf("antigravity %s = %q", kind, anti[kind])
		}
	}
	for _, kind := range []string{"session.ended", "turn.started", "session.turn-started", "input.requested"} {
		if _, served := anti[kind]; served {
			t.Fatalf("antigravity invents %s", kind)
		}
	}
	if !(antigravityRuntime{}).ActivityProcess("/fixture/bin/agy") || (antigravityRuntime{}).ActivityProcess("not-agy") {
		t.Fatal("antigravity process evidence does not match its binary")
	}
}
