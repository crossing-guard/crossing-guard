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

func TestSessionNewestActionAtRuntimeQualified(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	seedActionEvent(t, ix, "claude", "shared-ses", 1_000)
	seedActionEvent(t, ix, "opencode", "shared-ses", 2_000)
	ts, ok, err := ix.SessionNewestActionAt("claude", "shared-ses")
	if err != nil || !ok || ts != 1_000 {
		t.Fatalf("claude newest action: ts=%d ok=%t err=%v", ts, ok, err)
	}
	ts, ok, err = ix.SessionNewestActionAt("opencode", "shared-ses")
	if err != nil || !ok || ts != 2_000 {
		t.Fatalf("opencode newest action: ts=%d ok=%t err=%v", ts, ok, err)
	}
}

func TestSessionActionCandidatesBoundedAndDeduped(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	seedActionEvent(t, ix, "opencode", "ses-a", 1_000)
	seedActionEvent(t, ix, "opencode", "ses-a", 1_500)
	seedActionEvent(t, ix, "opencode", "ses-b", 2_000)
	seedActionEvent(t, ix, "claude", "ses-c", 3_000)
	candidates, err := ix.SessionActionCandidates(500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d: %+v", len(candidates), candidates)
	}
	if candidates[0].Runtime != "claude" || candidates[0].SessionID != "ses-c" || candidates[0].NewestActionAt != 3_000 {
		t.Fatalf("newest candidate mismatch: %+v", candidates[0])
	}
	if candidates[1].Runtime != "opencode" || candidates[1].SessionID != "ses-b" || candidates[1].NewestActionAt != 2_000 {
		t.Fatalf("second candidate mismatch: %+v", candidates[1])
	}
	if candidates[2].Runtime != "opencode" || candidates[2].SessionID != "ses-a" || candidates[2].NewestActionAt != 1_500 {
		t.Fatalf("third candidate mismatch: %+v", candidates[2])
	}
	limited, err := ix.SessionActionCandidates(500, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limited candidates: %d err=%v", len(limited), err)
	}
}

func seedActionEvent(t *testing.T, ix *Index, runtime, session string, ts int64) {
	t.Helper()
	gov, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if err := gov.EnsureSessionRoot(runtime, session, "/tmp/"+session+".jsonl", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := gov.tx.Exec(`INSERT INTO event(ts, session_id, runtime, verb, tool, origin)
		VALUES(?,?,?, 'action', 'Read', 'live')`, ts, session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := gov.Commit(); err != nil {
		t.Fatal(err)
	}
}
