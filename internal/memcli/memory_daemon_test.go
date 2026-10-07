package memcli

// memory_daemon_test.go — the memory read verbs read through the daemon
// (memory-reads-through-daemon plan §4.2): they never switch stores silently,
// say what to do when the daemon is down, carry the caller label, and build
// the SessionStart index byte-identically to the store-read path they replace.

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/memory"
	"crossing-guard/store"
)

// fakeDaemonReader answers canned JSON per route path and records each call.
// err fails every call (transport); errs fails one route with the daemon's
// own answer.
type fakeDaemonReader struct {
	answers map[string]any
	err     error
	errs    map[string]error
	calls   []string
}

func (f *fakeDaemonReader) GetJSON(path string, out any) error {
	f.calls = append(f.calls, path)
	if f.err != nil {
		return f.err
	}
	route, _, _ := strings.Cut(path, "?")
	if err := f.errs[route]; err != nil {
		return err
	}
	raw, err := json.Marshal(f.answers[route])
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// withDaemon points the read verbs at a fake daemon that reports reading
// <dataDir>/index.sqlite, and the CLI's own store at the same index unless the
// test overrides CG_INDEX.
func withDaemon(t *testing.T, reader *fakeDaemonReader) string {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("CG_INDEX", filepath.Join(dataDir, "index.sqlite"))
	t.Setenv("CPMEM_INDEX", "")
	t.Setenv("CG_MEMORY_DIR", t.TempDir())
	if reader.answers == nil {
		reader.answers = map[string]any{}
	}
	if _, set := reader.answers["/api/memory/config"]; !set {
		reader.answers["/api/memory/config"] = daemonConfig(filepath.Join(dataDir, "index.sqlite"))
	}
	prev := locateDaemon
	locateDaemon = func() (Daemon, error) { return Daemon{Reader: reader, Addr: "127.0.0.1:1"}, nil }
	t.Cleanup(func() { locateDaemon = prev })
	return dataDir
}

// daemonConfig is GET /api/memory/config naming the index the daemon reads.
func daemonConfig(indexPath string) map[string]any {
	return map[string]any{"search": map[string]any{"memory_search_limit_max": 50},
		"store": map[string]any{"index_path": indexPath}}
}

// dataCalls are the calls past the store check (the config read).
func dataCalls(r *fakeDaemonReader) []string {
	var out []string
	for _, c := range r.calls {
		if c != "/api/memory/config" {
			out = append(out, c)
		}
	}
	return out
}

func TestMemoryReadsRefuseADifferentStore(t *testing.T) {
	reader := &fakeDaemonReader{}
	dataDir := withDaemon(t, reader)
	other := filepath.Join(t.TempDir(), "index.sqlite")
	t.Setenv("CG_INDEX", other)
	_, _, err := memoryDaemon()
	if err == nil || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), filepath.Join(dataDir, "index.sqlite")) {
		t.Fatalf("a store mismatch must refuse naming both paths: %v", err)
	}
	if block := memIndexBlock(4096, "p"); !strings.HasPrefix(block, "(crossing-guard memory store unavailable: ") {
		t.Fatalf("the index must say the store is unavailable, not inject nothing: %q", block)
	}
	if calls := dataCalls(reader); len(calls) != 0 {
		t.Fatalf("a refused read must not read records: %v", calls)
	}
}

// TestMemoryReadsTrustTheDaemonsOwnStorePath is PW-1: a daemon started without
// --data reads ~/.crossing-guard/index.sqlite while its data dir is elsewhere;
// only the daemon knows, so the CLI compares against what the daemon reports.
func TestMemoryReadsTrustTheDaemonsOwnStorePath(t *testing.T) {
	home := t.TempDir()
	reader := &fakeDaemonReader{answers: map[string]any{"/api/memory/records": map[string]any{"records": []any{}}}}
	withDaemon(t, reader)
	t.Setenv("HOME", home)
	t.Setenv("CG_INDEX", "")
	reader.answers["/api/memory/config"] = daemonConfig(filepath.Join(home, ".crossing-guard", "index.sqlite"))
	if _, _, err := memoryDaemon(); err != nil {
		t.Fatalf("a default-data daemon reading the CLI's default store must be accepted: %v", err)
	}
	reader.answers["/api/memory/config"] = map[string]any{"search": map[string]any{}}
	if _, _, err := memoryDaemon(); err == nil || !strings.Contains(err.Error(), "does not say which store") {
		t.Fatalf("a daemon that names no store must be refused, not guessed: %v", err)
	}
}

// TestMemoryReadsKeepTheDaemonsReason is PW-3: an answer from the daemon keeps
// its own reason (the A-4 store failure); only a transport failure reads as
// "did not answer"; an auth failure points at doctor.
func TestMemoryReadsKeepTheDaemonsReason(t *testing.T) {
	reader := &fakeDaemonReader{errs: map[string]error{
		"/api/memory/search": errors.New("/api/memory/search?q=x: 503 Service Unavailable: memory store unavailable: boom"),
		"/api/memory/record": errors.New("/api/memory/record?id=x: 401 Unauthorized: bad token"),
	}}
	withDaemon(t, reader)
	_, _, err := daemonSearch("x", "", "", "", 0)
	if err == nil || !strings.Contains(err.Error(), "memory store unavailable: boom") || strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("the daemon's own reason must survive: %v", err)
	}
	_, _, err = daemonRecord("x")
	if err == nil || !strings.Contains(err.Error(), "bad token") || !strings.Contains(err.Error(), "crossing-guard doctor") {
		t.Fatalf("an auth failure must point at doctor: %v", err)
	}
}

func TestMemoryReadsCompareStoresByLocationNotSpelling(t *testing.T) {
	reader := &fakeDaemonReader{answers: map[string]any{"/api/memory/records": map[string]any{"records": []any{}}}}
	dataDir := withDaemon(t, reader)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dataDir, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_INDEX", filepath.Join(link, "index.sqlite"))
	if _, _, err := memoryDaemon(); err != nil {
		t.Fatalf("the same store through a symlink is the same store: %v", err)
	}
}

func TestMemoryReadsNameTheDaemonWhenItDoesNotAnswer(t *testing.T) {
	reader := &fakeDaemonReader{err: &url.Error{Op: "Get", URL: "http://127.0.0.1:1/api/memory/search",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}}}
	withDaemon(t, reader)
	_, _, err := daemonSearch("routing", "", "", "", 0)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") || !strings.Contains(err.Error(), "crossing-guard doctor") {
		t.Fatalf("an unreachable daemon must be named with the next step: %v", err)
	}
}

func TestMemorySearchCarriesFacetsLimitAndCallerLabel(t *testing.T) {
	reader := &fakeDaemonReader{answers: map[string]any{"/api/memory/search": map[string]any{
		"hits":  []any{map[string]any{"id": "a", "title": "A", "score": 3, "why": []string{"title:a"}, "pending": true}},
		"total": 7, "truncated": true}}}
	withDaemon(t, reader)
	resp, raw, err := daemonSearch("order routing", "bug-fix", "example-app", "orders", 1)
	if err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(strings.SplitN(dataCalls(reader)[0], "?", 2)[1])
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"q": "order routing", "category": "bug-fix",
		"repository": "example-app", "tag": "orders", "limit": "1", "via": "cli"} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
	if len(resp.Hits) != 1 || !resp.Hits[0].Pending || resp.Total != 7 || !resp.Truncated || resp.LimitMax != 50 {
		t.Errorf("decoded response: %+v", resp)
	}
	if !json.Valid(raw) || !strings.Contains(string(raw), `"truncated":true`) {
		t.Errorf("--json prints the daemon's answer as-is: %s", raw)
	}
}

// TestMemoryIndexBlockMatchesTheStoreReadPath is A-6: the index built from the
// daemon's records equals the one the CLI built from the store, nonce aside,
// across user-, repository- and superseded-scope records.
func TestMemoryIndexBlockMatchesTheStoreReadPath(t *testing.T) {
	updated := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	recs := []store.MemoryRecord{
		{ID: "user-rec", Status: "active", ScopeType: store.MemoryScopeUser, Title: "User", Category: "how-to", Body: "b", CreatedAt: updated, UpdatedAt: updated},
		{ID: "repo-rec", Status: "active", ScopeType: store.MemoryScopeRepository, ScopeID: "proj", Title: "Repo", Category: "gotcha", Body: "b", CreatedAt: updated, UpdatedAt: updated},
		{ID: "other-repo", Status: "active", ScopeType: store.MemoryScopeRepository, ScopeID: "elsewhere", Title: "Other", Category: "note", Body: "b", CreatedAt: updated, UpdatedAt: updated},
		{ID: "old-rec", Status: "active", ScopeType: store.MemoryScopeUser, Title: "Old", Category: "note", Body: "b", SupersededBy: "user-rec", CreatedAt: updated, UpdatedAt: updated},
	}
	// The route's shape for each record (the daemon's memoryRecordMap).
	var wire []any
	var direct []memory.Record
	for _, r := range recs {
		wire = append(wire, map[string]any{"id": r.ID, "title": r.Title, "category": r.Category,
			"scope_type": string(r.ScopeType), "repository": r.ScopeID, "tags": []string{}, "aliases": []string{},
			"source": r.Source, "origin": r.Origin, "superseded_by": r.SupersededBy,
			"verified_at": r.VerifiedAt, "verified_by": r.VerifiedBy,
			"created": time.Unix(0, r.CreatedAt).UTC().Format(time.RFC3339),
			"updated": time.Unix(0, r.UpdatedAt).UTC().Format(time.RFC3339),
			"body":    r.Body, "status": r.Status})
		// The store-read conversion the CLI used before this change.
		direct = append(direct, memory.RecordFromStore(r.ID, r.Status, string(r.ScopeType), r.ScopeID,
			r.Title, r.Category, r.Body, r.Tags, r.Aliases, r.Source, r.Origin,
			r.SupersededBy, r.VerifiedAt, r.VerifiedBy, fmtMemoryTime(r.CreatedAt), fmtMemoryTime(r.UpdatedAt)))
	}
	reader := &fakeDaemonReader{answers: map[string]any{"/api/memory/records": map[string]any{"records": wire}}}
	withDaemon(t, reader)
	got := memIndexBlock(4096, "proj")
	want := memory.BuildIndexFromRecords(memory.DefaultDir(), 4096, "proj", direct)
	stripNonce := func(s string) string { _, rest, _ := strings.Cut(s, "\n"); return rest }
	if stripNonce(got) != stripNonce(want) {
		t.Fatalf("index drifted:\n--- daemon\n%s\n--- store\n%s", got, want)
	}
	if !strings.Contains(got, "repo-rec") || strings.Contains(got, "other-repo") || strings.Contains(got, "old-rec") {
		t.Fatalf("scope and supersession filtering lost: %s", got)
	}
	// The hook sends its cwd so the daemon resolves the scope (team item 5 decision 12).
	if call := dataCalls(reader)[0]; !strings.HasPrefix(call, "/api/memory/records?cwd=") || !strings.HasSuffix(call, "&status=active") {
		t.Fatalf("index reads the active records, carrying its cwd: %v", reader.calls)
	}
}

// Criterion 6 (team item 5 decision 12): the injected header reads `user: N ·
// repository: P · organization: M`; an organization record recalls in a repository whose
// folder name differs from the organization id; a remote-identified repository record
// recalls in a checkout of that remote and not in another; a shadowed record is not
// recalled; a weak record keeps the folder-name match.
func TestMemoryIndexRecallsByResolvedScope(t *testing.T) {
	rec := func(id, scope, scopeID, identity, collision string) map[string]any {
		return map[string]any{"id": id, "title": id, "category": "note", "scope_type": scope, "repository": scopeID, "scope_id": scopeID,
			"repository_identity": identity, "collision": collision, "tags": []string{}, "aliases": []string{}, "source": "human",
			"created": "2026-10-01T00:00:00Z", "updated": "2026-10-01T00:00:00Z", "body": "b", "status": "active"}
	}
	wire := []any{
		rec("mine", "user", "", "weak", ""),
		rec("org-wide", "organization", "org_01TEAM", "weak", ""),
		rec("this-remote", "repository", "sha256:thisrepo", "remote-sha256", ""),
		rec("other-remote", "repository", "sha256:otherrepo", "remote-sha256", ""),
		rec("weak-here", "repository", "Service", "weak", ""),
		rec("weak-elsewhere", "repository", "elsewhere", "weak", ""),
		rec("shadowed-team", "organization", "org_01TEAM", "weak", "shadowed"),
	}
	reader := &fakeDaemonReader{answers: map[string]any{"/api/memory/records": map[string]any{"records": wire,
		"recall_scope": map[string]any{"label": "service", "repository_id": "sha256:thisrepo", "resolution": "git"}}}}
	withDaemon(t, reader)
	got := memIndexBlock(4096, "")
	if !strings.Contains(got, "user: 1 · repository: 2 · organization: 1\n") {
		t.Fatalf("the header counts recall by scope: %s", got)
	}
	for _, want := range []string{"mine", "org-wide", "this-remote", "weak-here"} {
		if !strings.Contains(got, "- "+want+" ") {
			t.Errorf("%s must be recalled: %s", want, got)
		}
	}
	for _, not := range []string{"other-remote", "weak-elsewhere", "shadowed-team"} {
		if strings.Contains(got, "- "+not+" ") {
			t.Errorf("%s must not be recalled: %s", not, got)
		}
	}
	// The same records from a checkout the daemon could not resolve to a remote: the
	// remote-identified record is not recalled; the organization record still is.
	reader.answers["/api/memory/records"] = map[string]any{"records": wire, "recall_scope": map[string]any{"label": "service", "resolution": "folder"}}
	got = memIndexBlock(4096, "")
	if strings.Contains(got, "- this-remote ") || !strings.Contains(got, "- org-wide ") || !strings.Contains(got, "user: 1 · repository: 1 · organization: 1") {
		t.Fatalf("an unresolved checkout recalls no remote-identified repository record: %s", got)
	}
}
