package memcli

import (
	"crossing-guard/store"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The import writes lowercased repository slugs, so its --project filter must
// accept the directory's own spelling (memory-repository-case-fix-plan.md).
func TestImportClaudeMemoryProjectFilterIgnoresCase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, ".claude", "projects",
		"-Users-fixture-Documents-Sites-Example-Other-Project", "memory")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	topic := "---\ndescription: Example shop supplier returns\ntype: note\n---\n\nSupplier returns take two weeks.\n"
	if err := os.WriteFile(filepath.Join(project, "supplier-returns.md"), []byte(topic), 0o644); err != nil {
		t.Fatal(err)
	}

	matched, err := importClaudeMemory(filepath.Join(home, "matched"), "Example-Other-Project", false)
	if err != nil {
		t.Fatal(err)
	}
	if matched.Projects != 1 || matched.Imported != 1 {
		t.Fatalf("--project Example-Other-Project: %+v, want 1 project and 1 imported", matched)
	}

	other, err := importClaudeMemory(filepath.Join(home, "other"), "example-app", false)
	if err != nil {
		t.Fatal(err)
	}
	if other.Projects != 0 || other.Imported != 0 {
		t.Fatalf("--project example-app: %+v, want nothing imported", other)
	}
}

// PW-H1 (team item 5 decision 14): on a LINKED device, importing a vendor's auto-memory
// from a checkout with one origin remote mints the remote identity (decision 18) but
// shares nothing — the import runs unattended, so its records queue nothing for the team.
func TestImportOnALinkedDeviceMintsIdentityAndQueuesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	checkout := filepath.Join(home, "work", "svc")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://git.example.com/acme/svc.git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = checkout
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	resolved, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := store.Open(indexDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tx.EnsureSessionCheckpoint(store.SessionCheckpoint{Runtime: "claude", SessionID: "claude/s", ScopeKey: resolved, Kind: "attachment",
		WorkingDirectory: resolved, RepositoryID: "rep", CheckoutID: "chk", CheckoutRoot: resolved, Status: "pending", BoundaryClass: "unconfirmed", RequestedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()
	project := filepath.Join(home, ".claude", "projects", claudeProjectEscape.ReplaceAllString(resolved, "-"), "memory")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "deploy-notes.md"), []byte("---\ndescription: deploy notes\ntype: note\n---\n\nDeploy on Tuesdays.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, ids, err := ImportAutoMemory(filepath.Join(home, "mirror"), "", false)
	if err != nil || st.Imported != 1 || len(ids) != 1 {
		t.Fatalf("import: %+v %v %v", st, ids, err)
	}
	ix, err = store.Open(indexDB())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	rec, err := ix.MemoryByID(ids[0])
	if err != nil || rec.RepositoryIdentity != "remote-sha256" || rec.Status != "active" || rec.ShareState != "unshared" {
		t.Fatalf("minted, active, and NOT shared: %+v %v", rec, err)
	}
	if sum, err := ix.OutboxPendingSummary(); err != nil || sum.ByKind[store.OutboxMemory] != 0 {
		t.Fatalf("an import queues nothing for the team: %+v %v", sum, err)
	}
}
