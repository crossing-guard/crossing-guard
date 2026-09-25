package workspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/store"
)

func workspaceTestRepo(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "base.txt")
	run("-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid", "commit", "-q", "-m", "base")
	return root
}

func workspaceTestConfig(t *testing.T, allowed, managed string) Config {
	t.Helper()
	path := filepath.Join(filepath.Dir(allowed), "workspace.json")
	if err := os.WriteFile(path, []byte(validWorkspaceConfigJSON(allowed, managed)), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestSelectionServiceListsBindsAndRevalidatesOnlyAllowedCheckout(t *testing.T) {
	base := t.TempDir()
	repo := workspaceTestRepo(t, filepath.Join(base, "repo"))
	outside := workspaceTestRepo(t, filepath.Join(base, "outside"))
	config := workspaceTestConfig(t, repo, filepath.Join(base, "managed"))
	index, err := store.Open(filepath.Join(base, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	service := NewSelectionService(config, index)
	service.now = func() time.Time { return time.UnixMilli(100) }
	ids := []string{"selection-1", "selection-2"}
	service.newID = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}

	set, err := service.List(Subject{Kind: "session", ID: "session-1"}, []string{outside})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepo, _ := filepath.EvalSymlinks(repo)
	if len(set.Candidates) != 1 || set.Candidates[0].Root != canonicalRepo || set.Candidates[0].Source != "configured" {
		t.Fatalf("candidate set = %+v", set)
	}
	selection, created, err := service.Bind(BindRequest{SubjectKind: "session", SubjectID: "session-1",
		CandidateID: set.Candidates[0].ID, ExpectedVersion: 0, IdempotencyKey: "bind-1"}, []string{outside})
	if err != nil || !created || selection.Version != 1 || selection.Candidate.Root != canonicalRepo {
		t.Fatalf("selection = %+v created=%v err=%v", selection, created, err)
	}
	retry, created, err := service.Bind(BindRequest{SubjectKind: "session", SubjectID: "session-1",
		CandidateID: set.Candidates[0].ID, ExpectedVersion: 0, IdempotencyKey: "bind-1"}, nil)
	if err != nil || created || retry.ID != selection.ID {
		t.Fatalf("retry = %+v created=%v err=%v", retry, created, err)
	}
	fresh, err := service.Revalidate(selection.ID, 1)
	if err != nil || fresh.CheckoutID != selection.Candidate.CheckoutID || fresh.Freshness != "live" {
		t.Fatalf("revalidate = %+v err=%v", fresh, err)
	}
	if _, err := service.Revalidate(selection.ID, 2); !errors.Is(err, store.ErrWorkspaceSelectionNotCurrent) {
		t.Fatalf("stale version = %v", err)
	}
}

func TestSelectionServiceRejectsBrowserInventedCandidate(t *testing.T) {
	base := t.TempDir()
	repo := workspaceTestRepo(t, filepath.Join(base, "repo"))
	config := workspaceTestConfig(t, repo, filepath.Join(base, "managed"))
	index, err := store.Open(filepath.Join(base, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	service := NewSelectionService(config, index)
	if _, _, err := service.Bind(BindRequest{SubjectKind: "task", SubjectID: "task-1",
		CandidateID: "/tmp/not-an-opaque-candidate", ExpectedVersion: 0, IdempotencyKey: "key"}, nil); err == nil {
		t.Fatal("raw path candidate was accepted")
	}
}
