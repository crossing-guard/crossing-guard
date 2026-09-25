package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func testWorkspaceSelection(id, subject, key, digest, checkout string, at int64) WorkspaceSelectionRecord {
	return WorkspaceSelectionRecord{
		ID: id, SubjectKind: "session", SubjectID: subject, IdempotencyKey: key,
		RequestDigest: digest, RepositoryID: "repo-1", CheckoutID: checkout,
		Root: "/repo/" + checkout, Kind: "local", SnapshotDigest: "snapshot-" + checkout,
		ObservedAt: at, CreatedAt: at, UpdatedAt: at,
	}
}

func TestWorkspaceSelectionCASAndImmutableIdempotency(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	firstIn := testWorkspaceSelection("selection-1", "session-1", "key-1", "request-1", "checkout-1", 10)
	first, created, err := ix.BindWorkspaceSelection(firstIn, 0)
	if err != nil || !created || first.BindingVersion != 1 || first.Status != "active" {
		t.Fatalf("first bind = %+v created=%v err=%v", first, created, err)
	}
	current, found, err := ix.CurrentWorkspaceSelection("session", "session-1")
	if err != nil || !found || current.ID != first.ID {
		t.Fatalf("current first = %+v found=%v err=%v", current, found, err)
	}

	secondIn := testWorkspaceSelection("selection-2", "session-1", "key-2", "request-2", "checkout-2", 20)
	second, created, err := ix.BindWorkspaceSelection(secondIn, 1)
	if err != nil || !created || second.BindingVersion != 2 {
		t.Fatalf("second bind = %+v created=%v err=%v", second, created, err)
	}
	storedFirst, found, err := ix.WorkspaceSelection(first.ID)
	if err != nil || !found || storedFirst.Status != "superseded" {
		t.Fatalf("superseded first = %+v found=%v err=%v", storedFirst, found, err)
	}

	// A delayed retry returns the immutable original outcome even though the subject
	// has since advanced to version two.
	retry, created, err := ix.BindWorkspaceSelection(firstIn, 0)
	if err != nil || created || retry.ID != first.ID || retry.BindingVersion != 1 {
		t.Fatalf("old retry = %+v created=%v err=%v", retry, created, err)
	}
	conflict := firstIn
	conflict.RequestDigest = "different"
	if _, _, err := ix.BindWorkspaceSelection(conflict, 2); !errors.Is(err, ErrWorkspaceSelectionIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	third := testWorkspaceSelection("selection-3", "session-1", "key-3", "request-3", "checkout-3", 30)
	if _, _, err := ix.BindWorkspaceSelection(third, 1); !errors.Is(err, ErrWorkspaceSelectionVersionConflict) {
		t.Fatalf("stale version conflict = %v", err)
	}
}

func TestWorkspaceSelectionLeaseBlocksRebindAndIsRetrySafe(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	selection, _, err := ix.BindWorkspaceSelection(
		testWorkspaceSelection("selection-a", "session-a", "key-a", "request-a", "checkout-a", 10), 0)
	if err != nil {
		t.Fatal(err)
	}
	lease, created, err := ix.AcquireWorkspaceSelectionLease(selection.ID, selection.BindingVersion, "task", "task-a", 11)
	if err != nil || !created || lease.Status != "active" {
		t.Fatalf("lease = %+v created=%v err=%v", lease, created, err)
	}
	retry, created, err := ix.AcquireWorkspaceSelectionLease(selection.ID, selection.BindingVersion, "task", "task-a", 12)
	if err != nil || created || retry.AcquiredAt != 11 {
		t.Fatalf("lease retry = %+v created=%v err=%v", retry, created, err)
	}
	next := testWorkspaceSelection("selection-b", "session-a", "key-b", "request-b", "checkout-b", 20)
	if _, _, err := ix.BindWorkspaceSelection(next, 1); !errors.Is(err, ErrWorkspaceSelectionLeased) {
		t.Fatalf("rebind while leased = %v", err)
	}
	leases, err := ix.ActiveWorkspaceSelectionLeases(selection.ID)
	if err != nil || len(leases) != 1 || leases[0].OwnerID != "task-a" {
		t.Fatalf("active leases = %+v err=%v", leases, err)
	}
	if released, err := ix.ReleaseWorkspaceSelectionLease(selection.ID, "task", "task-a", 21); err != nil || !released {
		t.Fatalf("release=%v err=%v", released, err)
	}
	bound, created, err := ix.BindWorkspaceSelection(next, 1)
	if err != nil || !created || bound.BindingVersion != 2 {
		t.Fatalf("post-release bind = %+v created=%v err=%v", bound, created, err)
	}
	if _, _, err := ix.AcquireWorkspaceSelectionLease(selection.ID, 1, "terminal", "terminal-a", 22); !errors.Is(err, ErrWorkspaceSelectionNotCurrent) {
		t.Fatalf("superseded lease = %v", err)
	}
}

func TestWorkspaceLeaseOwnerCannotSpanSelections(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	a, _, _ := ix.BindWorkspaceSelection(testWorkspaceSelection("selection-a", "session-a", "key-a", "request-a", "checkout-a", 10), 0)
	b, _, _ := ix.BindWorkspaceSelection(testWorkspaceSelection("selection-b", "session-b", "key-b", "request-b", "checkout-b", 10), 0)
	if _, _, err := ix.AcquireWorkspaceSelectionLease(a.ID, 1, "task", "same-task", 11); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.AcquireWorkspaceSelectionLease(b.ID, 1, "task", "same-task", 12); !errors.Is(err, ErrWorkspaceLeaseConflict) {
		t.Fatalf("cross-selection owner conflict = %v", err)
	}
}
