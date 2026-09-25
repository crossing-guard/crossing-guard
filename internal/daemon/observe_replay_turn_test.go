package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/observation"
)

// A turn record that could not be delivered live is spooled by the hook and
// replayed at boot; replay must admit the trn_ prefix (SSA journey 8). A record
// it cannot validate is reported by name and quarantined with its bytes kept —
// the contract every other prefix keeps — while the valid records still land.
func TestReplayAdmitsSpooledTurnRecords(t *testing.T) {
	g := foldHarness(t)
	defer swapSessionStatusRefold(func(string, string) {})()
	data := t.TempDir()
	dir := filepath.Join(data, "observation-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	good := observation.SessionTurnEnvelope{
		Schema: observation.SessionTurnSchemaV1, ObservationID: "trn_feedfacefeedfacefeedfacefeedface",
		CollectorID: observation.CollectorSessionTurn, Runtime: "claude", SessionID: "ses-spooled",
		Kind: "turn.ended", ObservedAt: 1_800_000_000, QueuedAt: 1_800_000_000, DeliveryAttempts: 1, DeliveryMode: "direct", // spooled as sent; replay restamps it
	}
	bad := good
	bad.ObservationID = "trn_ffffffffffffffffffffffffffffffff" // sorts after the good one
	bad.Kind = "Stop"                                          // a vendor event name is not one of our kinds
	for _, envelope := range []observation.SessionTurnEnvelope{good, bad} {
		b, _ := json.Marshal(envelope)
		if err := os.WriteFile(filepath.Join(dir, envelope.ObservationID+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := replayObservationSpoolOnce(t.Context(), data, g)
	if err == nil || !strings.Contains(err.Error(), bad.ObservationID) {
		t.Fatalf("the invalid record must be reported by name, got %v", err)
	}
	rows, err := g.ix.SessionTurnsFor("claude", "ses-spooled", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != "turn.ended" || rows[0].DeliveryMode != "replay" || rows[0].DeliveryAttempts != 2 {
		t.Fatalf("the good record must land once and only it: %+v (replay err %v)", rows, err)
	}
	if _, err := os.Stat(filepath.Join(dir, good.ObservationID+".json")); !os.IsNotExist(err) {
		t.Fatal("a replayed record must leave the spool")
	}
	if _, err := os.Stat(filepath.Join(dir, bad.ObservationID+".json")); !os.IsNotExist(err) {
		t.Fatal("an invalid record must be quarantined out of the spool, not retried every boot")
	}
}
