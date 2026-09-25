package store

import (
	"path/filepath"
	"testing"
)

func seedTurn(t *testing.T, ix *Index, id, runtime, session, kind string, atMS int64) int64 {
	t.Helper()
	gov, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if err := gov.EnsureSessionRoot(runtime, session, "/tmp/"+session+".jsonl", "/tmp"); err != nil {
		t.Fatal(err)
	}
	_, rowID, err := gov.AppendSessionTurn(SessionTurnObservation{
		ObservationID: id, Runtime: runtime, SessionID: session, Kind: kind,
		ObservedAt: atMS / 1000, ReceivedAtMS: atMS, EvidenceDigest: "d-" + id,
		CollectorID: "test", DeliveryAttempts: 1, DeliveryMode: "direct",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gov.Commit(); err != nil {
		t.Fatal(err)
	}
	return rowID
}

func TestSessionTurnsNewestFirstAndBounded(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	seedTurn(t, ix, "t1", "claude", "s", "turn.started", 1_000)
	seedTurn(t, ix, "t2", "claude", "s", "turn.ended", 2_000)
	seedTurn(t, ix, "t3", "claude", "s", "turn.started", 3_000)
	rows, err := ix.SessionTurnsFor("claude", "s", 2)
	if err != nil || len(rows) != 2 || rows[0].Kind != "turn.started" || rows[0].ReceivedAtMS != 3_000 || rows[1].ReceivedAtMS != 2_000 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].RowID <= rows[1].RowID {
		t.Fatalf("rowid must be monotonic: %+v", rows)
	}
}

func TestSessionTurnCollisionAndRetention(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	seedTurn(t, ix, "same", "claude", "s", "turn.ended", 5_000)
	gov, _ := ix.BeginGov()
	if _, _, err := gov.AppendSessionTurn(SessionTurnObservation{ObservationID: "same", Runtime: "claude", SessionID: "s",
		Kind: "turn.started", ObservedAt: 5, ReceivedAtMS: 5_000, EvidenceDigest: "other", CollectorID: "test",
		DeliveryAttempts: 2, DeliveryMode: "replay"}); err != ErrSessionTurnCollision {
		t.Fatalf("want collision, got %v", err)
	}
	_ = gov.Rollback()
	seedTurn(t, ix, "old", "claude", "s2", "turn.ended", 100)
	pruned, err := ix.PruneSessionTurns(1_000)
	if err != nil || pruned != 1 {
		t.Fatalf("pruned=%d err=%v", pruned, err)
	}
	recent, err := ix.SessionsWithRecentTurns(4_000, 10)
	if err != nil || len(recent) != 1 || recent[0].SessionID != "s" {
		t.Fatalf("recent=%+v err=%v", recent, err)
	}
}
