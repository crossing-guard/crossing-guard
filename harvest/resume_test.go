package harvest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResumeCopiesAreNotCountedTwice pins D7. Resuming a Claude Code session copies
// the prior conversation into the NEW file, and the copied records keep their
// ORIGINAL sessionId. Attributing by filename counted the same turns and tokens
// under two sessions — measured at 5 of 507 files and 4,758 records (~1.8%) on the
// real corpus, with one session resumed twice into two separate files.
//
// The rule: a record belongs to the session whose id it CARRIES. Copied history
// still renders (it is real context); it is never re-counted.
func TestResumeCopiesAreNotCountedTwice(t *testing.T) {
	dir := t.TempDir()
	resumed := "bbbbbbbb-0000-0000-0000-000000000000"
	path := filepath.Join(dir, resumed+".jsonl")

	// 2 copied assistant turns from session "aaaa…", then 1 turn genuinely ours.
	line := func(sid string, out int) string {
		return `{"type":"assistant","sessionId":"` + sid + `","timestamp":"2026-07-16T10:00:00.000Z",` +
			`"message":{"role":"assistant","model":"claude-opus-4-8",` +
			`"content":[{"type":"text","text":"reply"}],"usage":{"input_tokens":10,` +
			`"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":` +
			string(rune('0'+out)) + `}}}`
	}
	body := line("aaaaaaaa-0000-0000-0000-000000000000", 5) + "\n" +
		line("aaaaaaaa-0000-0000-0000-000000000000", 5) + "\n" +
		line(resumed, 7) + "\n" +
		`{"type":"user","sessionId":"aaaaaaaa-0000-0000-0000-000000000000",` +
		`"message":{"role":"user","content":"copied prompt"}}` + "\n" +
		`{"type":"user","sessionId":"` + resumed + `","message":{"role":"user","content":"my prompt"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	scan := claudeTitle(path)
	if scan.Usage == nil {
		t.Fatal("no usage parsed")
	}
	if scan.Usage.Turns != 1 {
		t.Fatalf("counted %d assistant turns; copied history was re-counted (want 1)", scan.Usage.Turns)
	}
	if scan.Usage.OutputTokens != 7 {
		t.Fatalf("output tokens = %d, want 7 — copied tokens leaked into this session's total",
			scan.Usage.OutputTokens)
	}
	if scan.UserTurns != 1 {
		t.Fatalf("counted %d user turns, want 1", scan.UserTurns)
	}
	// The title must come from OUR prompt, not the copied one.
	if scan.Title != "my prompt" {
		t.Fatalf("title taken from copied history: %q", scan.Title)
	}

	// Display still shows everything: copied history is real context.
	events, _, usage, err := normalizeClaude(path, transcriptCaps)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 5 {
		t.Fatalf("copied history was dropped from the transcript: %d events", len(events))
	}
	if usage.Turns != 1 {
		t.Fatalf("detail usage re-counted copies: %d turns, want 1", usage.Turns)
	}
}

func TestClaudeSyntheticFailureDoesNotReplaceConcreteModel(t *testing.T) {
	dir := t.TempDir()
	sid := "cccccccc-0000-0000-0000-000000000000"
	path := filepath.Join(dir, sid+".jsonl")
	body := `{"type":"assistant","sessionId":"` + sid + `","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":2}}}` + "\n" +
		`{"type":"assistant","sessionId":"` + sid + `","error":"authentication_failed","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"Failed to authenticate"}],"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	scan := claudeTitle(path)
	if scan.Usage == nil {
		t.Fatal("no summary usage parsed")
	}
	if scan.Usage.Model != "claude-opus-5" {
		t.Fatalf("summary model = %q, want last concrete model", scan.Usage.Model)
	}
	_, _, usage, err := normalizeClaude(path, transcriptCaps)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Model != "claude-opus-5" {
		t.Fatalf("detail model = %q, want last concrete model", usage.Model)
	}
}
