package guardcli

import (
	"testing"

	"crossing-guard/internal/observation"
)

func TestSessionEntryEnvelopeKeepsNativeSourceAndUsesGenericKind(t *testing.T) {
	entry, err := buildSessionEntryEnvelope(hookInput{Runtime: "claude", SessionID: "native",
		HookEventName: "SessionStart", Source: "compact", TranscriptPath: "/tmp/session.jsonl", Cwd: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Schema != observation.SessionEntrySchemaV1 || entry.EntryKind != "context-compact" ||
		entry.NativeSource != "compact" || entry.Runtime != "claude" || entry.SessionID != "native" {
		t.Fatalf("entry=%+v", entry)
	}
	entry, err = buildSessionEntryEnvelope(hookInput{Runtime: "future-runtime", SessionID: "native",
		HookEventName: "SessionStart", Source: "brand-new"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.EntryKind != "unknown" || entry.NativeSource != "brand-new" {
		t.Fatalf("unknown source was guessed: %+v", entry)
	}
}

func TestClaudeSessionStartMatcherIsExact(t *testing.T) {
	current := claudeMatcherIsCurrent("SessionStart")
	if !current(map[string]any{"matcher": claudeSessionEntryMatcher}) {
		t.Fatal("documented SessionStart matcher was not current")
	}
	for _, entry := range []map[string]any{{}, {"matcher": "*"}, {"matcher": "startup"}} {
		if current(entry) {
			t.Fatalf("overbroad or partial SessionStart matcher accepted: %+v", entry)
		}
	}
}
