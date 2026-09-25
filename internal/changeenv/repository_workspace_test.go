package changeenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInspectCheckoutReportsLayeredDirtyFactsAndSnapshot(t *testing.T) {
	repo := testRepo(t)
	clean, err := InspectCheckout(repo)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Dirty || clean.Head == "" || clean.Repository.Root == "" || clean.SnapshotDigest == "" || clean.ObservedAt <= 0 {
		t.Fatalf("clean checkout = %+v", clean)
	}
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "add", "staged.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("unstaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, err := InspectCheckout(filepath.Join(repo, "."))
	if err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty || dirty.Staged != 1 || dirty.Unstaged != 1 || dirty.Untracked != 1 || dirty.Conflicted != 0 {
		t.Fatalf("dirty checkout = %+v", dirty)
	}
	if dirty.SnapshotDigest == clean.SnapshotDigest || dirty.Repository.CheckoutID != clean.Repository.CheckoutID {
		t.Fatalf("snapshot/identity mismatch: clean=%+v dirty=%+v", clean, dirty)
	}
}
