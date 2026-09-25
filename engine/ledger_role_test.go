package engine

import (
	"path/filepath"
	"testing"
)

// TestObserveRoleFailOpenRegression pins the C1 fix: the /observe callers build a
// role-less Event, and the shipped default library is 100% role-scoped. If Observe
// did not default the role, every detector would be skipped, the session watermark
// would never advance, and session-scoped governance would silently fail OPEN. This
// test observes a role-less Bash action against the REAL default library and asserts
// it classifies — the exact path the old ledger tests (role-less test data) missed.
func TestObserveRoleFailOpenRegression(t *testing.T) {
	dets, err := DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), dets, &Policy{})
	if err != nil {
		t.Fatal(err)
	}
	obs := l.Observe("s1", Event{Tool: "Bash"}) // role-less, as the HTTP handlers send
	if len(obs.Tags) == 0 {
		t.Fatal("role-less tool_call classified to ZERO tags — session governance fails OPEN")
	}
	if obs.WeakNeg {
		t.Error("session watermark did not advance on a real observed action")
	}
}
