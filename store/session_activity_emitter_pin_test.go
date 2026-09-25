package store

import (
	"os"
	"strings"
	"testing"
)

// I8: the natural-session emitter reads session_activity_observation by rowid
// with no kind filter. Turn rows therefore live in their own table; if this
// query ever changes shape, the reasoning in the session-status plan must be
// revisited rather than silently invalidated.
func TestSessionActivityAfterQueryIsUnfiltered(t *testing.T) {
	body, err := os.ReadFile("session_activity.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "WHERE rowid > ? ORDER BY rowid ASC LIMIT ?") {
		t.Fatal("SessionActivityAfter's unfiltered rowid read changed; the turn table's separation rationale must be re-checked")
	}
}
