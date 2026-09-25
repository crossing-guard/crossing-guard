package harvest

import (
	"strings"
	"testing"
)

// Context a hook added is a transcript row; the vendor's other attachments
// stay counted as unparsed. Record shapes measured on Claude Code 2.1.270
// (session 2bacab41, 2026-09-15).
func TestClaudeHookContextBecomesAContextRow(t *testing.T) {
	long := strings.Repeat("r", transcriptCaps.tool+50)
	lines := strings.Join([]string{
		`{"type":"user","sessionId":"s1","uuid":"u1","timestamp":"2026-09-15T15:50:50Z","message":{"role":"user","content":"commit only my files"}}`,
		`{"type":"attachment","sessionId":"s1","uuid":"a1","timestamp":"2026-09-15T15:50:54Z","attachment":{"type":"hook_additional_context","content":["[helper run r1]","We discussed this before."],"hookName":"UserPromptSubmit","hookEvent":"UserPromptSubmit"}}`,
		`{"type":"attachment","sessionId":"s1","uuid":"a2","timestamp":"2026-09-15T15:50:54Z","attachment":{"type":"date","date":"2026-09-15"}}`,
		`{"type":"attachment","sessionId":"s1","uuid":"a3","timestamp":"2026-09-15T15:51:00Z","attachment":{"type":"hook_additional_context","content":["` + long + `"],"hookName":"PreToolUse:Bash","hookEvent":"PreToolUse"}}`,
	}, "\n")
	events, unparsed, _, err := normalizeClaudeReader("/projects/x/s1.jsonl", strings.NewReader(lines), transcriptCaps)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || unparsed != 1 {
		t.Fatalf("events %+v unparsed %d", events, unparsed)
	}
	context := events[1]
	if context.Kind != "context" || context.Name != "UserPromptSubmit" || context.Text != "[helper run r1]\nWe discussed this before." ||
		context.TurnAnchor != "a1" || context.Seq != 2 || context.Ts != "2026-09-15T15:50:54Z" {
		t.Fatalf("context row: %+v", context)
	}
	clipped := events[2]
	if clipped.Kind != "context" || clipped.Name != "PreToolUse" || clipped.FullLen != len(long) || len(clipped.Text) >= len(long) {
		t.Fatalf("a long context row is clipped and declares its size: kind=%s name=%s full_len=%d text=%d", clipped.Kind, clipped.Name, clipped.FullLen, len(clipped.Text))
	}
}
