package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/workspace"
	"crossing-guard/store"
)

func daemonWorkspaceRepo(t *testing.T, root string) string {
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

func daemonWorkspaceConfig(root, managed string) []byte {
	value := map[string]any{
		"format_version": 1, "allowed_roots": []string{root},
		"worktrees": map[string]any{"root": managed, "max_managed": 8,
			"max_aggregate_bytes": 10737418240, "max_replay_files": 2000,
			"max_replay_bytes": 268435456, "max_replay_file_bytes": 20971520,
			"unused_warning_hours": 168, "failed_create_recovery_hours": 24,
			"include_untracked_default": false, "copy_ignored_default": false},
		"review": map[string]any{"max_files": 500, "max_rendered_file_bytes": 2097152,
			"max_rendered_total_bytes": 8388608, "max_hunks": 2000, "max_rendered_line_bytes": 32768},
		"mutation": map[string]any{"max_patch_bytes": 2097152, "max_patch_lines": 1000,
			"max_paths": 500, "preview_ttl_seconds": 300, "approval_ttl_seconds": 600,
			"max_active_per_selection": 1, "max_recovery_bytes": 268435456,
			"recovery_retention_hours": 24},
	}
	body, _ := json.Marshal(value)
	return body
}

func TestWorkspaceHTTPListsAndBindsOpaqueCandidate(t *testing.T) {
	data := t.TempDir()
	repo := daemonWorkspaceRepo(t, filepath.Join(data, "repo"))
	if err := os.WriteFile(filepath.Join(data, "workspace.json"), daemonWorkspaceConfig(repo, filepath.Join(data, "managed")), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := newWorkspaceHost(data, filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer host.close()
	mux := http.NewServeMux()
	registerWorkspaceRoutes(mux, host)

	list := httptest.NewRecorder()
	mux.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/workspaces?session_id=session-1", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var set struct {
		Candidates []struct {
			ID   string `json:"id"`
			Root string `json:"root"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &set); err != nil || len(set.Candidates) != 1 || set.Candidates[0].ID == "" {
		t.Fatalf("candidate set=%+v err=%v", set, err)
	}
	body, _ := json.Marshal(map[string]any{"subject_kind": "session", "subject_id": "session-1",
		"candidate_id": set.Candidates[0].ID, "expected_version": 0, "idempotency_key": "workspace-bind-1"})
	bind := httptest.NewRecorder()
	mux.ServeHTTP(bind, httptest.NewRequest(http.MethodPost, "/api/workspace-selections", bytes.NewReader(body)))
	if bind.Code != http.StatusCreated || bind.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("bind status=%d content-type=%q body=%s", bind.Code, bind.Header().Get("Content-Type"), bind.Body.String())
	}
	var selection map[string]any
	if err := json.Unmarshal(bind.Body.Bytes(), &selection); err != nil || selection["id"] == "" || selection["version"] != float64(1) {
		t.Fatalf("selection=%+v err=%v", selection, err)
	}
}

func TestWorkspaceHTTPRejectsAmbiguousSubjectAndRawPathCandidate(t *testing.T) {
	data := t.TempDir()
	repo := daemonWorkspaceRepo(t, filepath.Join(data, "repo"))
	if err := os.WriteFile(filepath.Join(data, "workspace.json"), daemonWorkspaceConfig(repo, filepath.Join(data, "managed")), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := newWorkspaceHost(data, filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer host.close()
	mux := http.NewServeMux()
	registerWorkspaceRoutes(mux, host)

	badList := httptest.NewRecorder()
	mux.ServeHTTP(badList, httptest.NewRequest(http.MethodGet, "/api/workspaces?task_id=t&session_id=s", nil))
	if badList.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous subject status=%d body=%s", badList.Code, badList.Body.String())
	}
	body := []byte(`{"subject_kind":"task","subject_id":"task-1","candidate_id":"/tmp/raw","expected_version":0,"idempotency_key":"workspace-bind-1"}`)
	bind := httptest.NewRecorder()
	mux.ServeHTTP(bind, httptest.NewRequest(http.MethodPost, "/api/workspace-selections", bytes.NewReader(body)))
	if bind.Code != http.StatusConflict || !bytes.Contains(bind.Body.Bytes(), []byte("candidate_unavailable")) {
		t.Fatalf("raw candidate status=%d body=%s", bind.Code, bind.Body.String())
	}
}

func TestWorkspaceHTTPUnavailableIsCapabilityLocal(t *testing.T) {
	mux := http.NewServeMux()
	registerWorkspaceRoutes(mux, nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces?task_id=t", nil))
	if response.Code != http.StatusServiceUnavailable || !bytes.Contains(response.Body.Bytes(), []byte("workspace_unavailable")) {
		t.Fatalf("unavailable status=%d body=%s", response.Code, response.Body.String())
	}
}

// ---- live diff routes (workspace-panes plan §4.1) ---------------------------------

type liveDiffFixture struct {
	mux   *http.ServeMux
	index *store.Index
	repo  string
}

// newLiveDiffFixture has no workspace.json on purpose (owner decision O-G):
// the diff routes read the folder a session recorded and nothing else.
func newLiveDiffFixture(t *testing.T) liveDiffFixture {
	t.Helper()
	data := t.TempDir()
	repo := daemonWorkspaceRepo(t, filepath.Join(data, "repo"))
	index, err := store.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	mux := http.NewServeMux()
	review := workspace.NewReviewService(reviewBudgets(defaultConsoleConfig()), index)
	registerWorkspaceDiffRoutes(mux, review)
	registerWorkspaceFilesRoutes(mux, review)
	return liveDiffFixture{mux: mux, index: index, repo: repo}
}

func (f liveDiffFixture) session(t *testing.T, id, cwd string) {
	t.Helper()
	if err := govTx(t, f.index, func(tx *store.GovTx) error { return tx.EnsureSessionRoot("claude", id, "", cwd) }); err != nil {
		t.Fatal(err)
	}
}

func (f liveDiffFixture) get(t *testing.T, path string, out any) int {
	t.Helper()
	response := httptest.NewRecorder()
	f.mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: %v: %s", path, err, response.Body.String())
		}
	}
	return response.Code
}

func TestWorkspaceDiffRoutesRenderTypedProblemsAndScopes(t *testing.T) {
	f := newLiveDiffFixture(t)
	f.session(t, "in-repo", f.repo)
	if err := os.WriteFile(filepath.Join(f.repo, "new.txt"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "base.txt"), []byte("base\nmore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(f.repo) // the resolver reports the symlink-free root
	var checkout workspace.ReviewCheckoutResponse
	if code := f.get(t, "/api/workspace-diff/checkout?id=in-repo&runtime=claude", &checkout); code != 200 || checkout.Problem != nil || checkout.Checkout == nil || checkout.Checkout.Root != canonical {
		t.Fatalf("checkout code=%d body=%+v", code, checkout)
	}
	var diff workspace.ReviewDiffResponse
	if code := f.get(t, "/api/workspace-diff?id=in-repo&scope=working", &diff); code != 200 || diff.Problem != nil || len(diff.Files) != 2 {
		t.Fatalf("working code=%d body=%+v", code, diff)
	}
	if diff.Files[0].Path != "base.txt" || diff.Files[1].Path != "new.txt" || !diff.Files[1].Untracked || diff.Files[1].Added != 2 {
		t.Fatalf("working files=%+v", diff.Files)
	}
	var patch workspace.ReviewPatchResponse
	if code := f.get(t, "/api/workspace-diff/file?id=in-repo&scope=working&path=base.txt", &patch); code != 200 || patch.File == nil || !strings.Contains(patch.File.Patch, "+more") {
		t.Fatalf("patch code=%d body=%+v", code, patch)
	}
	if code := f.get(t, "/api/workspace-diff/file?id=in-repo&scope=working&path=../etc", nil); code != http.StatusBadRequest {
		t.Fatalf("unsafe path code=%d", code)
	}
	if code := f.get(t, "/api/workspace-diff/file?id=in-repo&scope=staged&path=base.txt", &patch); code != 200 || patch.Problem == nil || patch.Problem.Code != "file-not-in-scope" {
		t.Fatalf("out-of-scope code=%d body=%+v", code, patch)
	}
	var refs workspace.ReviewRefsResponse
	if code := f.get(t, "/api/workspace-diff/refs?id=in-repo", &refs); code != 200 || refs.Problem != nil || len(refs.Commits) != 1 || len(refs.Branches) != 1 {
		t.Fatalf("refs code=%d body=%+v", code, refs)
	}
	if code := f.get(t, "/api/workspace-diff?id=in-repo&scope=branch&base=no-such-ref", &diff); code != 200 || diff.Problem == nil || diff.Problem.Code != "base-ref-missing" {
		t.Fatalf("bad base code=%d body=%+v", code, diff)
	}
	for _, bad := range []string{"/api/workspace-diff?id=in-repo&scope=sideways", "/api/workspace-diff?id=in-repo&base=--output=x", "/api/workspace-diff/file?id=in-repo", "/api/workspace-diff"} {
		if code := f.get(t, bad, nil); code != http.StatusBadRequest {
			t.Fatalf("%s: code=%d", bad, code)
		}
	}
}

func TestWorkspaceDiffRoutesReadAnyRecordedFolderAndRefuseUnresolvedOnes(t *testing.T) {
	f := newLiveDiffFixture(t)
	var diff workspace.ReviewDiffResponse
	if code := f.get(t, "/api/workspace-diff?id=unknown-session", &diff); code != 200 || diff.Problem == nil || diff.Problem.Code != "no-recorded-folder" || len(diff.Files) != 0 {
		t.Fatalf("no folder code=%d body=%+v", code, diff)
	}
	// A second repository the session recorded is read like any other: no allowlist.
	elsewhere := daemonWorkspaceRepo(t, filepath.Join(t.TempDir(), "elsewhere"))
	f.session(t, "elsewhere", elsewhere)
	canonical, _ := filepath.EvalSymlinks(elsewhere)
	var checkout workspace.ReviewCheckoutResponse
	if code := f.get(t, "/api/workspace-diff/checkout?id=elsewhere", &checkout); code != 200 || checkout.Problem != nil || checkout.Checkout == nil || checkout.Checkout.Root != canonical {
		t.Fatalf("elsewhere code=%d body=%+v", code, checkout)
	}
	// Two runtimes recorded the same native id in different folders: the resolver
	// reads both rows and refuses to pick one.
	f.session(t, "ambiguous", f.repo)
	if err := govTx(t, f.index, func(tx *store.GovTx) error { return tx.EnsureSessionRoot("codex", "ambiguous", "", elsewhere) }); err != nil {
		t.Fatal(err)
	}
	if code := f.get(t, "/api/workspace-diff/checkout?id=ambiguous", &diff); code != 200 || diff.Problem == nil || diff.Problem.Code != "ambiguous-folder" || len(diff.Problem.Roots) != 2 {
		t.Fatalf("ambiguous code=%d body=%+v", code, diff)
	}
	// A folder that is not inside any repository is named as such.
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	f.session(t, "plain", plain)
	if code := f.get(t, "/api/workspace-diff?id=plain", &diff); code != 200 || diff.Problem == nil || diff.Problem.Code != "not-a-repository" || diff.Problem.Folder == "" {
		t.Fatalf("plain code=%d body=%+v", code, diff)
	}
	// A nested folder resolves to the enclosing checkout.
	inside := filepath.Join(f.repo, "plain-inside")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	f.session(t, "nested", inside)
	var nested workspace.ReviewCheckoutResponse
	if code := f.get(t, "/api/workspace-diff/checkout?id=nested", &nested); code != 200 || nested.Problem != nil {
		t.Fatalf("nested code=%d problem=%+v", code, nested.Problem)
	}
}

func TestWorkspaceFilesRoutesListAndReadTheRecordedCheckout(t *testing.T) {
	f := newLiveDiffFixture(t)
	f.session(t, "in-repo", f.repo)
	if err := os.MkdirAll(filepath.Join(f.repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "src", "new.go"), []byte("package src\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var tree workspace.ReviewTreeResponse
	if code := f.get(t, "/api/workspace-files?id=in-repo", &tree); code != 200 || tree.Problem != nil || tree.Checkout == nil || tree.Listing == nil {
		t.Fatalf("root code=%d body=%+v", code, tree)
	}
	names := []string{}
	for _, entry := range tree.Listing.Entries {
		names = append(names, entry.Name+":"+entry.Kind)
	}
	if strings.Join(names, " ") != "src:dir base.txt:file" {
		t.Fatalf("root entries=%v", names)
	}
	tree = workspace.ReviewTreeResponse{} // a fresh target: Unmarshal leaves absent pointer fields as they were
	if code := f.get(t, "/api/workspace-files?id=in-repo&dir=src", &tree); code != 200 || tree.Checkout != nil || tree.Listing == nil || len(tree.Listing.Entries) != 1 || !tree.Listing.Entries[0].Untracked {
		t.Fatalf("src code=%d body=%+v", code, tree)
	}
	tree = workspace.ReviewTreeResponse{}
	if code := f.get(t, "/api/workspace-files?id=in-repo&dir=.", &tree); code != 200 || tree.Listing == nil || tree.Listing.Dir != "" {
		t.Fatalf("dot dir code=%d body=%+v", code, tree)
	}
	var file workspace.ReviewFileResponse
	if code := f.get(t, "/api/workspace-files/read?id=in-repo&path=src/new.go", &file); code != 200 || file.Problem != nil || file.File == nil || file.File.Text != "package src\n" {
		t.Fatalf("read code=%d body=%+v", code, file)
	}
	file = workspace.ReviewFileResponse{}
	if code := f.get(t, "/api/workspace-files/read?id=in-repo&path=missing.go", &file); code != 200 || file.Problem == nil || file.Problem.Code != "file-not-in-tree" {
		t.Fatalf("unlisted code=%d body=%+v", code, file)
	}
	for _, bad := range []string{"/api/workspace-files?id=in-repo&dir=../x", "/api/workspace-files?id=in-repo&dir=.git", "/api/workspace-files?id=in-repo&dir=.GIT", "/api/workspace-files/read?id=in-repo&path=.git/config", "/api/workspace-files/read?id=in-repo&path=a/.git/x", "/api/workspace-files/read?id=in-repo&path=", "/api/workspace-files"} {
		if code := f.get(t, bad, nil); code != http.StatusBadRequest {
			t.Fatalf("%s: code=%d", bad, code)
		}
	}
	tree = workspace.ReviewTreeResponse{}
	if code := f.get(t, "/api/workspace-files?id=unknown", &tree); code != 200 || tree.Problem == nil || tree.Problem.Code != "no-recorded-folder" {
		t.Fatalf("no folder code=%d body=%+v", code, tree)
	}
}
