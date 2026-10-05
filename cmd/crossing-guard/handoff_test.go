package main

import (
	"bytes"
	"strings"
	"testing"
)

// Criterion 88's fail path (OD-19): there is no `handoff open` verb — opening is the
// console's act — and the usage names none.
func TestHandoffHasNoOpenVerb(t *testing.T) {
	if _, ok := handoffVerbs["open"]; ok {
		t.Fatal("handoff open must not exist: a terminal never picks a handoff up")
	}
	if code := handoffCmd([]string{"open", "hnd_x"}); code != 2 {
		t.Fatalf("handoff open is a usage error, got exit %d", code)
	}
	for _, line := range strings.Split(handoffUsage, "\n") {
		if strings.Contains(line, "crossing-guard handoff") && strings.Contains(line, " open") {
			t.Fatalf("the usage offers an open verb: %q", line)
		}
	}
	for _, verb := range []string{"send", "list", "show", "decline", "withdraw", "close", "clean"} {
		if _, ok := handoffVerbs[verb]; !ok {
			t.Fatalf("handoff %s is missing", verb)
		}
	}
	if !strings.Contains(topLevelUsage, "handoff send|list|show|decline|withdraw|close|clean") {
		t.Fatal("the top-level usage names the handoff verbs")
	}
}

func TestHandoffSendFlags(t *testing.T) {
	o, err := parseHandoffSend([]string{"--session", "claude/abc", "--to", "Teammate", "--title", "T",
		"--remaining", "one", "--remaining", "two", "--include-conversation"})
	if err != nil || o.runtime != "claude" || o.sessionID != "abc" || o.to != "Teammate" || o.title != "T" ||
		len(o.remaining) != 2 || !o.includeConversation || o.local {
		t.Fatalf("parsed: %+v err=%v", o, err)
	}
	if o, err := parseHandoffSend([]string{"--session", "codex/thread-1", "--local"}); err != nil || !o.local || o.to != "" {
		t.Fatalf("local: %+v err=%v", o, err)
	}
	for _, bad := range [][]string{
		{"--to", "Teammate"},                                // no session
		{"--session", "claude", "--to", "Teammate"},         // not runtime/id
		{"--session", "claude/abc"},                         // neither --to nor --local
		{"--session", "claude/abc", "--to", "T", "--local"}, // both
		{"--session", "claude/abc", "--to", "T", "extra"},   // stray argument
		{"--session", "claude/abc", "--to", "T", "--open"},  // unknown flag
	} {
		if _, err := parseHandoffSend(bad); err == nil {
			t.Fatalf("%v must be refused", bad)
		}
	}
}

func TestHandoffRowsWordWhatTheDeviceHolds(t *testing.T) {
	never := handoffRow{ID: "hnd_1", State: "expired"}
	never.Peer.DisplayName = "Teammate"
	if line := never.line(); !strings.Contains(line, "(expired before it reached this device)") || !strings.Contains(line, "Teammate") {
		t.Fatalf("never held: %q", line)
	}
	other := handoffRow{ID: "hnd_2", State: "sent", FromAnotherDevice: true}
	if line := other.line(); !strings.Contains(line, "(sent from another of your devices)") {
		t.Fatalf("sender's other device: %q", line)
	}
	refused := handoffRow{ID: "hnd_3", State: "refused", RefusalCode: "rate_limited", Title: "T", EverHeld: true, HasText: true}
	if line := refused.line(); !strings.Contains(line, "refused (rate_limited)") {
		t.Fatalf("a refusal is shown by name: %q", line)
	}
	var out bytes.Buffer
	shown := handoffShown{Title: "T", BodyMarkdown: "B", Remaining: []string{"one"}, Bytes: 120}
	shown.Checks.RedactionCount, shown.Checks.AbsolutePaths = 2, 1
	printHandoffShown(&out, shown, true)
	for _, want := range []string{"Title: T", "- one", "B", "120 bytes leave", "2 secret pattern matches", "1 paths"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the shown content lacks %q: %s", want, out.String())
		}
	}
}
