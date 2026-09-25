package daemon

import (
	"strings"
	"testing"

	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

func testTurnEnvelope(kind string) observation.SessionTurnEnvelope {
	return observation.SessionTurnEnvelope{
		Schema: observation.SessionTurnSchemaV1, ObservationID: "trn_" + strings.Repeat("a", 32),
		CollectorID: observation.CollectorSessionTurn, Runtime: "claude", SessionID: "ses-turn-1",
		Kind: kind, NativeSource: "Stop", ObservedAt: 1_800_000_000, QueuedAt: 1_800_000_000,
		DeliveryAttempts: 1, DeliveryMode: "direct",
	}
}

func TestSessionTurnIngestStoresRowAndPublishes(t *testing.T) {
	ix, err := store.Open(t.TempDir() + "/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := NewGovernor(ix, nil)
	published := ""
	defer swapSessionStatusRefold(func(runtime, sessionID string) { published = runtime + "/" + sessionID })()

	receipt, err := ingestSessionTurnV1(g, testTurnEnvelope("turn.ended"))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Duplicate || receipt.Schema != observation.SessionTurnSchemaV1 {
		t.Fatalf("receipt=%+v", receipt)
	}
	if published != "claude/ses-turn-1" {
		t.Fatalf("a landed turn must publish a refold, got %q", published)
	}
	rows, err := ix.SessionTurnsFor("claude", "ses-turn-1", 10)
	if err != nil || len(rows) != 1 || rows[0].Kind != "turn.ended" || rows[0].ReceivedAtMS == 0 || rows[0].RowID == 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	// A retry of the same fact is a duplicate, not a second row, and does
	// not publish again.
	published = ""
	again, err := ingestSessionTurnV1(g, testTurnEnvelope("turn.ended"))
	if err != nil || !again.Duplicate || published != "" {
		t.Fatalf("again=%+v err=%v published=%q", again, err, published)
	}
	// The same identity carrying a different fact is a collision.
	other := testTurnEnvelope("turn.started")
	if _, err := ingestSessionTurnV1(g, other); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("collision not reported: %v", err)
	}
}

func TestSessionTurnRejectsForeignVocabulary(t *testing.T) {
	// The kind on the wire is OUR word. A provider's event name is not a kind.
	for _, bad := range []string{"Stop", "session.idle", "", "turn.finished"} {
		envelope := testTurnEnvelope(bad)
		if err := validateSessionTurnEnvelope(envelope); err == nil {
			t.Fatalf("kind %q must be rejected", bad)
		}
	}
	for _, good := range []string{"turn.started", "turn.ended", "input.requested", "subagent.ended", "context.compacted"} {
		if err := validateSessionTurnEnvelope(testTurnEnvelope(good)); err != nil {
			t.Fatalf("kind %q must be accepted: %v", good, err)
		}
	}
}
