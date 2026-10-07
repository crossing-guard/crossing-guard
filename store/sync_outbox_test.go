package store

import (
	"testing"

	"crossing-guard/engine"
)

func newCheckpoint(t *testing.T, ix *Index, session string) SessionCheckpoint {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: session, ScopeKey: "checkout:" + session, Kind: "settled",
		RequestID: session, WorkingDirectory: "/repo", RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo",
		Status: "pending", BoundaryClass: "settled", RequestedAt: 1})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, won, err := ix.ClaimSessionCheckpoint(c.ID, 2, 0, "settled"); err != nil || !won {
		t.Fatalf("claim: %v %v", won, err)
	}
	return c
}

// Item 4 criterion 31: a checkpoint reaching a terminal state enqueues its fact in the
// same transaction, only while linked, and the drain finds the checkpoint by its wire id.
func TestCheckpointFactEnqueuesOnlyWhileLinkedAndResolves(t *testing.T) {
	ix := openResultTestIndex(t)
	before := newCheckpoint(t, ix, "before-link")
	if err := ix.FailSessionCheckpoint(before.ID, 3, "failed", "settled", "timeout", "d1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := ix.OutboxPendingSummary(); s.Pending != 0 {
		t.Fatalf("unlinked: nothing enqueued, got %+v", s)
	}
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	c := newCheckpoint(t, ix, "linked")
	record := &ChangeRecord{SessionID: "linked", RepositoryID: "repo", CheckoutID: "checkout", RepositoryIdentityKind: "local-sha256",
		CheckoutRoot: "/repo", Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceDigest: "snapshot",
		RecordedAt: 2, CaptureStartedAt: 1, CaptureEndedAt: 2, BaseRevision: "base", HeadRevision: "head", SnapshotDigest: "snapshot"}
	if err := ix.CompleteSessionCheckpoint(c.ID, record, 3); err != nil {
		t.Fatal(err)
	}
	rows, err := ix.OutboxBatch(10, nil)
	if err != nil || len(rows) != 1 || rows[0].Kind != OutboxCheckpointFact || rows[0].Scope != "linked" || rows[0].EnqueuedAt != 3 {
		t.Fatalf("one checkpoint fact row: %+v err=%v", rows, err)
	}
	deviceID, _, _ := ix.Device()
	got, ok, err := ix.CheckpointForFact(deviceID, "linked", rows[0].GlobalID)
	if err != nil || !ok || got.ID != c.ID || got.Status != "complete" {
		t.Fatalf("resolve: %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := ix.CheckpointForFact(deviceID, "before-link", rows[0].GlobalID); ok {
		t.Fatal("a wire id resolves only within its own session")
	}
}

// The batch is oldest-first, leaves parked kinds out entirely (the wedge fix), acks
// idempotently, and the summary normalizes pre-item-4 nanosecond memory rows.
func TestOutboxBatchSkipsParkedKindsAndAcksIdempotently(t *testing.T) {
	ix := openResultTestIndex(t)
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	// A pre-item-4 memory row (nanoseconds) at the head, then events.
	if _, err := ix.db.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at) VALUES('memory','mem_X','h','repository:r',?)`, int64(1_700_000_000)*1_000_000_000); err != nil {
		t.Fatal(err)
	}
	a := engine.NewGenesisAnchor("s")
	for _, v := range []string{"e1", "e2", "e3"} {
		appendChained(t, ix, "s", a, v)
	}
	head, _ := ix.OutboxBatch(2, nil)
	if len(head) != 2 || head[0].Kind != OutboxMemory || head[0].EnqueuedAt != 1_700_000_000 {
		t.Fatalf("oldest first: %+v", head)
	}
	rows, err := ix.OutboxBatch(10, []string{OutboxMemory})
	if err != nil || len(rows) != 3 {
		t.Fatalf("parked memory is left out and never occupies the head: %+v err=%v", rows, err)
	}
	for _, r := range rows {
		if r.Kind != OutboxEvent {
			t.Fatalf("kind %s", r.Kind)
		}
	}
	s, _ := ix.OutboxPendingSummary()
	if s.Pending != 4 || s.ByKind[OutboxMemory] != 1 || s.OldestAt != 1000 {
		t.Fatalf("summary in seconds: %+v", s)
	}
	seqs := []int64{rows[0].Seq, rows[1].Seq}
	if err := ix.OutboxAttempted(seqs); err != nil {
		t.Fatal(err)
	}
	if err := ix.OutboxAck(seqs, 99); err != nil {
		t.Fatal(err)
	}
	if err := ix.OutboxAck(seqs, 100); err != nil {
		t.Fatal(err)
	}
	var acked int64
	_ = ix.db.QueryRow(`SELECT acked_at FROM sync_outbox WHERE seq = ?`, seqs[0]).Scan(&acked)
	if acked != 99 {
		t.Fatalf("a second ack does not move the first: %d", acked)
	}
	left, _ := ix.OutboxBatch(10, []string{OutboxMemory})
	if len(left) != 1 || left[0].Attempts != 0 {
		t.Fatalf("remaining: %+v", left)
	}
	in, err := ix.EventWireInputsByGlobalID([]string{left[0].GlobalID, "evt_missing"})
	if err != nil || len(in) != 1 || in[left[0].GlobalID].Session != "s" {
		t.Fatalf("wire inputs by id: %+v err=%v", in, err)
	}
}
