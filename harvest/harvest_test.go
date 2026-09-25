package harvest

import (
	"os"
	"testing"
)

// TestScanRealCorpus is the M2 slice-1 gate run against the real vendor dirs
// on this machine (the project's probe discipline: gates pass on real data).
// It skips cleanly on machines without a corpus.
func TestScanRealCorpus(t *testing.T) {
	if _, err := os.Stat(claudeRoot()); err != nil {
		t.Skip("no ~/.claude/projects on this machine")
	}
	sums := ScanSessions()
	counts := map[string]int{}
	for _, s := range sums {
		counts[s.Runtime]++
	}
	t.Logf("scan: %d sessions (claude=%d codex=%d)", len(sums), counts["claude"], counts["codex"])
	if counts["claude"] == 0 {
		t.Fatal("expected >0 claude sessions on this corpus")
	}
	// newest-first ordering
	for i := 1; i < len(sums); i++ {
		if sums[i].Modified.After(sums[i-1].Modified) {
			t.Fatalf("ordering violated at %d", i)
		}
	}
	// normalize the newest claude session end-to-end
	for _, s := range sums {
		if s.Runtime != "claude" {
			continue
		}
		d, err := Load(s.Runtime, s.ID)
		if err != nil {
			t.Fatalf("Load(%s/%s): %v", s.Runtime, s.ID, err)
		}
		if len(d.Events) == 0 && d.Unparsed == 0 {
			t.Fatalf("session %s produced no events and no unparsed count", s.ID)
		}
		t.Logf("newest claude session: %d events, %d unparsed", len(d.Events), d.Unparsed)
		break
	}
}
