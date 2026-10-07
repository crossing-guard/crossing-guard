package memcli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

func gitCheckout(t *testing.T, dir, remote string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	steps := [][]string{{"init", "-q"}}
	if remote != "" {
		steps = append(steps, []string{"remote", "add", "origin", remote})
	}
	for _, args := range steps {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Criterion 38, the client half (decision 7): an unlinked device refuses organization
// scope; a linked one takes the scope id from team.json whatever --repository says.
// Criterion 50, the CLI door (decision 18): a repository record written inside a checkout
// with one origin remote is keyed by that remote; elsewhere it stays weak, with the reason.
func TestUpsertScopeFromTeamJSONAndTheCurrentCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, _, _, _, err := upsertScope("organization", "not-the-org"); err == nil {
		t.Fatal("an unlinked device must refuse --scope organization")
	}
	doc, err := teamlink.Default()
	if err != nil {
		t.Fatal(err)
	}
	doc.Server, doc.LinkedAt = "https://team.example.com", "2026-10-02T00:00:00Z"
	doc.Organization = teamlink.Organization{ID: "org_01LINKED", Name: "Linked"}
	if err := os.MkdirAll(filepath.Dir(indexDB()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := teamlink.Save(filepath.Dir(indexDB()), doc); err != nil {
		t.Fatal(err)
	}
	scope, id, _, _, err := upsertScope("organization", "not-the-org")
	if err != nil || scope != store.MemoryScopeOrganization || id != "org_01LINKED" {
		t.Fatalf("the organization id comes from team.json, never from --repository: %s %q %v", scope, id, err)
	}

	checkout := gitCheckout(t, filepath.Join(home, "work", "svc"), "https://git.example.com/acme/svc.git")
	repo, err := changeenv.ResolveRepository(checkout)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(checkout)
	for _, flags := range [][2]string{{"repository", ""}, {"", "svc"}, {"repository", "SVC"}} {
		scope, id, identity, note, err := upsertScope(flags[0], flags[1])
		if err != nil || scope != store.MemoryScopeRepository || identity != "remote-sha256" || id != repo.ID || note != "" {
			t.Fatalf("--scope %q --repository %q in a single-remote checkout: %s %q %s %q %v", flags[0], flags[1], scope, id, identity, note, err)
		}
	}
	if _, id, identity, note, _ := upsertScope("", "some-other-repo"); identity != "weak" || id != "some-other-repo" || note == "" {
		t.Fatalf("a label naming another repository stays weak, with the reason: %q %s %q", id, identity, note)
	}
	t.Chdir(gitCheckout(t, filepath.Join(home, "work", "bare"), ""))
	if _, id, identity, note, _ := upsertScope("repository", ""); identity != "weak" || id != "bare" || note == "" {
		t.Fatalf("no origin remote stays weak, with the reason: %q %s %q", id, identity, note)
	}
	if scope, id, _, _, err := upsertScope("", ""); err != nil || scope != store.MemoryScopeUser || id != "" {
		t.Fatalf("no flags is user scope: %s %q %v", scope, id, err)
	}
}

// Criterion 40 (PW-M8) at the CLI's doors: `edit` and `verify` write against the revision
// they read; when the record has moved since, nothing is written.
func TestCLIEditWritesAgainstTheRevisionItRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CG_MEMORY_DIR", filepath.Join(t.TempDir(), "mirror"))
	rec := store.MemoryRecord{ID: "runbook", Title: "Runbook", Category: "how-to", Body: "v1", Source: "human", Status: "active",
		ScopeType: store.MemoryScopeUser, AuthorType: "user", AuthorID: "t"}
	read, err := upsertThroughStore(rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Another door writes after this verb read the record.
	moved := read
	moved.Body = "v2, landed meanwhile"
	if _, err := upsertThroughStore(moved, nil); err != nil {
		t.Fatal(err)
	}
	stale := read
	stale.Body = "my edit of v1"
	if _, err := reviseThroughStore(stale, read.Revision); !errors.Is(err, store.ErrMemoryStale) {
		t.Fatalf("an edit made against the old revision must be refused: %v", err)
	}
	ix, err := memStore()
	if err != nil {
		t.Fatal(err)
	}
	now, err := ix.MemoryByID("runbook")
	_ = ix.Close()
	if err != nil || now.Body != "v2, landed meanwhile" {
		t.Fatalf("nothing may have been written: %q %v", now.Body, err)
	}
	now.Body = "my edit of v2"
	if saved, err := reviseThroughStore(now, now.Revision); err != nil || saved.Body != "my edit of v2" {
		t.Fatalf("on the current revision it is saved: %v", err)
	}
}

// teamRecordInStore puts one organization record the team ACCEPTED into the CLI's store:
// created on a linked device, its first revision frozen, and answered accepted.
func teamRecordInStore(t *testing.T, id string) store.MemoryRecord {
	t.Helper()
	ix, err := memStore()
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	actor := store.MemoryActor{AuthorType: "user", AuthorID: "t", ActorSource: "cli"}
	r, err := ix.CreateMemory(store.MemoryRecord{ID: id, Title: "Runbook", Category: "how-to", Body: "team v1", Source: "human", Status: "active",
		ScopeType: store.MemoryScopeOrganization, ScopeID: "org_01LINKED", AuthorType: "user", AuthorID: "t"}, nil, nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ix.OutboxBatch(10, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("one queued revision: %d %v", len(rows), err)
	}
	if _, code, err := ix.MemoryPushRecord(rows[0], nil, "org_01LINKED"); err != nil || code != "" {
		t.Fatalf("freeze: %q %v", code, err)
	}
	if _, err := ix.SettleMemoryPush(store.MemorySettle{Seq: rows[0].Seq, Code: teamwire.StatusAccepted, ServerRevision: 1}, 1); err != nil {
		t.Fatal(err)
	}
	return r
}

func storeRecords(t *testing.T) map[string]store.MemoryRecord {
	t.Helper()
	recs, err := memListRecords("")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.MemoryRecord{}
	for _, r := range recs {
		out[r.ID] = r
	}
	return out
}

// FR-1 at the CLI's doors. `memory upsert` with no --scope ("reuse = update in place")
// and `memory import-file` of a hand-edited mirror file update a team record where it is,
// at its stored scope; neither makes a copy. Only `--scope user` detaches, and saying it
// again revises the same copy.
func TestOnlyScopeUserNarrowsATeamRecordAtTheCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mirror := filepath.Join(t.TempDir(), "mirror")
	t.Setenv("CG_MEMORY_DIR", mirror)
	team := teamRecordInStore(t, "runbook")

	memUpsert([]string{"--id", "runbook", "--title", "Runbook", "--category", "how-to", "--content", "updated in place"})
	recs := storeRecords(t)
	if got := recs["runbook"]; len(recs) != 1 || got.GlobalID != team.GlobalID || got.ScopeType != store.MemoryScopeOrganization ||
		got.ScopeID != "org_01LINKED" || got.Body != "updated in place" || got.Revision != 2 {
		t.Fatalf("a scope-less upsert updates the team record at its stored scope: %d records, %+v", len(recs), got)
	}

	// The mirror file of the record, edited by hand, then adopted.
	file := filepath.Join(mirror, "runbook.md")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the mirror file: %v", err)
	}
	edited := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(edited, []byte(strings.Replace(string(raw), "updated in place", "edited by hand", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	memImportFile([]string{edited})
	recs = storeRecords(t)
	if got := recs["runbook"]; len(recs) != 1 || got.ScopeType != store.MemoryScopeOrganization || got.ScopeID != "org_01LINKED" ||
		got.Body != "edited by hand" || got.Revision != 3 {
		t.Fatalf("import-file keeps the stored scope and makes no copy: %d records, %+v", len(recs), got)
	}

	for _, body := range []string{"mine", "mine again"} {
		memUpsert([]string{"--id", "runbook", "--scope", "user", "--title", "My notes", "--category", "note", "--content", body})
	}
	recs = storeRecords(t)
	if len(recs) != 2 {
		t.Fatalf("--scope user twice makes exactly one copy: %d records", len(recs))
	}
	for id, r := range recs {
		if id == "runbook" {
			if r.ScopeType != store.MemoryScopeOrganization || r.Body != "edited by hand" || r.Revision != 3 {
				t.Fatalf("the team's record is untouched by the narrowing: %+v", r)
			}
			continue
		}
		if !strings.HasPrefix(id, "runbook-") || r.ScopeType != store.MemoryScopeUser || r.Body != "mine again" || r.Revision != 2 || r.GlobalID == team.GlobalID {
			t.Fatalf("the copy: %+v", r)
		}
	}
}
