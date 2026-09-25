package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/codemap"
	"crossing-guard/internal/understanding"
	"crossing-guard/store"
)

func understandingCoordinatorFixture(t *testing.T) (*Governor, *understandingScanCoordinator) {
	t.Helper()
	index, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	governor := NewGovernor(index, nil)
	coordinator := newUnderstandingScanCoordinator(governor)
	governor.understanding = coordinator
	return governor, coordinator
}

func understandingWorkFixture(checkout string, id, ended int64, kind string) store.UnderstandingCheckpoint {
	digest := fmt.Sprintf("git-tree-v2-sha256:%s-%d", checkout, id)
	root := filepath.Join("/tmp", "understanding-"+checkout)
	return store.UnderstandingCheckpoint{
		Checkpoint: store.SessionCheckpoint{ID: id, SessionID: "session", Kind: kind,
			Status: "complete", CheckoutID: checkout, CheckoutRoot: root,
			ChangeRecordID: id, CaptureEndedAt: ended},
		Change: store.ChangeRecord{ID: id, SessionID: "session", RepositoryID: "repo-" + checkout,
			CheckoutID: checkout, CheckoutRoot: root, SnapshotDigest: digest,
			BaseRevision: "base", HeadRevision: "head"},
	}
}

func appendCoordinatorCheckpointFixture(t *testing.T, index *store.Index, source store.UnderstandingCheckpoint) {
	t.Helper()
	checkpoint := source.Checkpoint
	transaction, err := index.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := transaction.EnsureSessionCheckpoint(store.SessionCheckpoint{
		SessionID: checkpoint.SessionID, ScopeKey: checkpoint.CheckoutID, Kind: checkpoint.Kind,
		RequestID: fmt.Sprintf("coordinator-%d", checkpoint.ID), WorkingDirectory: checkpoint.CheckoutRoot,
		RepositoryID: source.Change.RepositoryID, CheckoutID: checkpoint.CheckoutID,
		CheckoutRoot: checkpoint.CheckoutRoot, Status: "pending", BoundaryClass: "settled",
		RequestedAt: checkpoint.CaptureEndedAt - 1,
	})
	if err == nil {
		err = transaction.Commit()
	} else {
		_ = transaction.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := index.ClaimSessionCheckpoint(stored.ID, checkpoint.CaptureEndedAt-1, 0, "settled"); err != nil || !won {
		t.Fatalf("claim won=%v err=%v", won, err)
	}
	record := source.Change
	record.ID = 0
	record.SessionID = checkpoint.SessionID
	record.Kind = "revision"
	record.EvidenceClass = "observed"
	record.SourceKind = "git"
	record.SourceRef = "HEAD"
	record.SourceDisplay = "Git"
	record.SourceDigest = record.SnapshotDigest
	record.RepositoryIdentityKind = "local-sha256"
	record.RecordedAt = checkpoint.CaptureEndedAt
	record.CaptureStartedAt = checkpoint.CaptureEndedAt - 1
	record.CaptureEndedAt = checkpoint.CaptureEndedAt
	record.CaptureAttempts = 1
	if err := index.CompleteSessionCheckpoint(stored.ID, &record, checkpoint.CaptureEndedAt); err != nil {
		t.Fatal(err)
	}
}

func TestUnderstandingCoordinatorCoalescesRapidCheckpointOffers(t *testing.T) {
	_, coordinator := understandingCoordinatorFixture(t)
	now := time.Unix(100, 0)
	coordinator.now = func() time.Time { return now }
	for i := int64(1); i <= 342; i++ {
		coordinator.offer(understandingWorkFixture("checkout", i, i, "settled"))
	}
	if len(coordinator.items) != 1 || coordinator.stats.offered.Load() != 1 ||
		coordinator.stats.coalesced.Load() != 341 {
		t.Fatalf("pending=%d offered=%d coalesced=%d", len(coordinator.items),
			coordinator.stats.offered.Load(), coordinator.stats.coalesced.Load())
	}
	if _, ready := coordinator.nextReady(now); ready {
		t.Fatal("quiet settled work ran immediately")
	}
	work, ready := coordinator.nextReady(now.Add(understandingQuietWindow))
	if !ready || work.source.Checkpoint.ID != 342 {
		t.Fatalf("latest coalesced work ready=%v work=%+v", ready, work)
	}
}

func TestUnderstandingCoordinatorRunsOneImmediateBoundary(t *testing.T) {
	_, coordinator := understandingCoordinatorFixture(t)
	now := time.Unix(100, 0)
	coordinator.now = func() time.Time { return now }
	coordinator.offer(understandingWorkFixture("checkout", 1, 1, "attachment"))
	work, ready := coordinator.nextReady(now)
	if !ready {
		t.Fatal("attachment baseline was not admitted immediately")
	}
	calls := 0
	coordinator.scan = func(ctx context.Context, index *store.Index, input understanding.ScanInput) (understanding.ScanResult, error) {
		calls++
		if input.Expected == nil || input.Expected.SnapshotDigest != work.source.Change.SnapshotDigest ||
			input.Base != work.source.Change.BaseRevision {
			t.Fatalf("scan input=%+v", input)
		}
		return understanding.ScanResult{Outcome: "complete", Recorded: true}, nil
	}
	coordinator.runWork(context.Background(), work)
	if calls != 1 || coordinator.stats.completed.Load() != 1 || coordinator.stats.active.Load() != 0 {
		t.Fatalf("calls=%d completed=%d active=%d", calls,
			coordinator.stats.completed.Load(), coordinator.stats.active.Load())
	}
}

func TestUnderstandingCoordinatorSuppressesExactCompleteAndRecentFailure(t *testing.T) {
	governor, coordinator := understandingCoordinatorFixture(t)
	now := time.Unix(100, 0)
	coordinator.now = func() time.Time { return now }
	completeWork := understandingWorkFixture("complete", 1, 1, "settled")
	complete := store.UnderstandingGeneration{RepositoryID: completeWork.Change.RepositoryID,
		CheckoutID: completeWork.Change.CheckoutID, CheckoutRoot: completeWork.Change.CheckoutRoot,
		Status: "complete", SnapshotProtocol: "git-tree-v2",
		SnapshotDigest: completeWork.Change.SnapshotDigest, BaseRevision: "base", HeadRevision: "head",
		StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: governor.analyzerAssembly.Digest(),
		ConventionState: "none", StartedAt: 1, EndedAt: 2}
	if err := governor.ix.AppendUnderstanding(&complete); err != nil {
		t.Fatal(err)
	}
	coordinator.offer(completeWork)
	failedWork := understandingWorkFixture("failed", 2, 2, "settled")
	failed := store.UnderstandingGeneration{RepositoryID: failedWork.Change.RepositoryID,
		CheckoutID: failedWork.Change.CheckoutID, CheckoutRoot: failedWork.Change.CheckoutRoot,
		Status: "failed", SnapshotProtocol: "git-tree-v2",
		SnapshotDigest: failedWork.Change.SnapshotDigest, BaseRevision: "base", HeadRevision: "head",
		StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: governor.analyzerAssembly.Digest(),
		ConventionState: "none", StartedAt: now.UnixNano() - 2, EndedAt: now.UnixNano() - 1,
		LimitationCode: "fixture", Limitation: "fixture"}
	if err := governor.ix.AppendUnderstanding(&failed); err != nil {
		t.Fatal(err)
	}
	coordinator.offer(failedWork)
	if len(coordinator.items) != 0 || coordinator.stats.coalesced.Load() != 2 {
		t.Fatalf("suppressed pending=%d coalesced=%d", len(coordinator.items), coordinator.stats.coalesced.Load())
	}
}

func TestUnderstandingCoordinatorBoundsDistinctCheckoutPressure(t *testing.T) {
	_, coordinator := understandingCoordinatorFixture(t)
	coordinator.now = func() time.Time { return time.Unix(100, 0) }
	for i := 0; i < understandingPendingCapacity+1; i++ {
		checkout := fmt.Sprintf("checkout-%02d", i)
		coordinator.offer(understandingWorkFixture(checkout, int64(i+1), int64(i+1), "settled"))
	}
	if len(coordinator.items) != understandingPendingCapacity || coordinator.stats.overflow.Load() != 1 {
		t.Fatalf("pending=%d overflow=%d", len(coordinator.items), coordinator.stats.overflow.Load())
	}
}

func TestUnderstandingCoordinatorKeysAndPendingStatusUseRepositoryAndCheckout(t *testing.T) {
	_, coordinator := understandingCoordinatorFixture(t)
	now := time.Unix(100, 0)
	coordinator.now = func() time.Time { return now }
	first := understandingWorkFixture("same-checkout", 1, 1, "settled")
	second := understandingWorkFixture("same-checkout", 2, 2, "settled")
	second.Change.RepositoryID = "different-repository"
	coordinator.offer(first)
	coordinator.offer(second)
	if len(coordinator.items) != 2 {
		t.Fatalf("repository identities coalesced into %d pending entries", len(coordinator.items))
	}
	if !coordinator.pending(first.Change.RepositoryID, first.Change.CheckoutID, first.Change.SnapshotDigest) ||
		!coordinator.pending(second.Change.RepositoryID, second.Change.CheckoutID, second.Change.SnapshotDigest) ||
		coordinator.pending("wrong-repository", first.Change.CheckoutID, first.Change.SnapshotDigest) {
		t.Fatal("pending status did not require full repository/checkout/snapshot identity")
	}
}

func TestUnderstandingRecoveryCountsCandidatesNotEmptyPolls(t *testing.T) {
	governor, coordinator := understandingCoordinatorFixture(t)
	now := time.Unix(1_000, 0)
	coordinator.now = func() time.Time { return now }
	if coordinator.recoverOne(now) {
		t.Fatal("empty recovery poll consumed an admission")
	}
	source := understandingWorkFixture("recovery", 1, now.Unix()-1, "settled")
	appendCoordinatorCheckpointFixture(t, governor.ix, source)
	if !coordinator.recoverOne(now) {
		t.Fatal("eligible recovery checkpoint did not consume an admission")
	}
	if coordinator.stats.recovered.Load() != 1 || len(coordinator.items) != 1 {
		t.Fatalf("recovered=%d pending=%d", coordinator.stats.recovered.Load(), len(coordinator.items))
	}
}

func TestStopUnderstandingSchedulingClosesAndDrainsPendingWork(t *testing.T) {
	governor, coordinator := understandingCoordinatorFixture(t)
	coordinator.now = func() time.Time { return time.Unix(100, 0) }
	coordinator.offer(understandingWorkFixture("shutdown", 1, 1, "settled"))
	stopUnderstandingScheduling(governor)
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if !coordinator.closed || len(coordinator.items) != 0 {
		t.Fatalf("closed=%v pending=%d", coordinator.closed, len(coordinator.items))
	}
}
