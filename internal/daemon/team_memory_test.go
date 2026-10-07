package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/driver"

	"crossing-guard/engine"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/teamlink"
	"crossing-guard/memory"
	"crossing-guard/schemas"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Team memory on the device: the drain's
// memory and tombstone encoders behind the gate, "delete what was sent", the pull job's
// landing and mirror, the share action, minting, and scope-aware recall.

func teamHuman() store.MemoryActor {
	return store.MemoryActor{AuthorType: "user", AuthorID: "tester", ActorSource: "cli"}
}

func teamOrgRecord(id, body string) store.MemoryRecord {
	return store.MemoryRecord{ID: id, Title: "T " + id, Category: "gotcha", Body: body, Source: "human", Status: "active",
		ScopeType: store.MemoryScopeOrganization, ScopeID: "org_1", AuthorType: "user", AuthorID: "tester"}
}

func (f *pushFake) records(kind string) []teamwire.PushRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []teamwire.PushRecord
	for _, r := range f.received {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// Criteria 8 and 41 (decisions 1, 13b, 14): the pre-44 backlog and every unreviewed or
// weak record are refused on the device, counted not_shareable on the first tick, and
// nothing of them is sent; a shared record is pushed redacted and schema-valid at memory
// 1.1; a second revision names the first's wire hash as its base; a deletion pushes a
// tombstone.
func TestMemoryDrainGateBacklogRedactionAndBase(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	fake.script(map[string]string{}, false)
	weak := store.MemoryRecord{ID: "weak", Title: "weak", Category: "note", Body: "b", Source: "import", Status: "active",
		ScopeType: store.MemoryScopeRepository, ScopeID: "proj", RepositoryIdentity: "weak", AuthorType: "user", AuthorID: "import"}
	saved, err := ix.CreateMemory(weak, nil, nil, teamHuman())
	if err != nil {
		t.Fatal(err)
	}
	draft := teamOrgRecord("draft", "pending draft")
	draft.Status = "pending"
	if _, err := ix.CreateMemory(draft, nil, nil, teamHuman()); err != nil {
		t.Fatal(err)
	}
	// The installed backlog's shape: pending memory rows that name no revision.
	raw, err := driver.Open("file:" + filepath.Join(tl.storeDir, "index.sqlite") + "?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := raw.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at) VALUES('memory',?,?,'repository:proj',1)`, saved.GlobalID, saved.ContentHash); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	tl.pushOnce()
	if n := fake.got(teamwire.KindMemory); n != 0 {
		t.Fatalf("no weak, pending or backlog row may leave the device, %d sent", n)
	}
	st := tl.status()
	if st.Outbox.Refused[teamwire.KindMemory][teamwire.CodeNotShareable] != 3 || st.Outbox.ByKind[teamwire.KindMemory] != 0 {
		t.Fatalf("the backlog is acked and counted not_shareable on its first tick: %+v", st.Outbox)
	}
	if st.Memory == nil || st.Memory.NotShareable != 3 || st.Memory.WeakRepository != 1 {
		t.Fatalf("the status names it: %+v", st.Memory)
	}

	secret := "the deploy token is ghp_" + strings.Repeat("a1B2c3", 6) + " — kept under /Users/someone/secrets/token.txt"
	rec, err := ix.CreateMemory(teamOrgRecord("deploy", secret), nil, nil, teamHuman())
	if err != nil {
		t.Fatal(err)
	}
	tl.pushOnce()
	sent := fake.records(teamwire.KindMemory)
	fake.mu.Lock()
	invalid := append([]string(nil), fake.invalid...)
	fake.mu.Unlock()
	if len(sent) != 1 || len(invalid) != 0 {
		t.Fatalf("one schema-valid memory record: %d sent, invalid %v", len(sent), invalid)
	}
	var first teamwire.MemoryRecord
	if err := json.Unmarshal(sent[0].Body, &first); err != nil {
		t.Fatal(err)
	}
	if first.SchemaVersion != "1.1" || strings.Contains(first.Body, "ghp_") || strings.Contains(first.Body, "/Users/") || first.BaseContentHash != "" ||
		first.ContentHash != teamwire.MemoryWireHash(first) || sent[0].ID != teamwire.MemoryPushID(rec.GlobalID, 1) {
		t.Fatalf("redacted, portable, first appearance, hashed: %s", sent[0].Body)
	}
	got, _ := ix.MemoryByID("deploy")
	if got.PushedHash != first.ContentHash || got.Body != secret || store.MemoryProjectionHash(got) != got.SyncedProjectionHash {
		t.Fatalf("accepted: the base is the sent wire hash and the local body is untouched: %+v", got)
	}
	got.Body = "rotated; ask the on-call"
	if _, err := ix.UpsertMemory(got, nil, nil, teamHuman()); err != nil {
		t.Fatal(err)
	}
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	sent = fake.records(teamwire.KindMemory)
	var second teamwire.MemoryRecord
	if len(sent) != 1 || json.Unmarshal(sent[0].Body, &second) != nil || second.BaseContentHash != first.ContentHash || second.Revision != 2 {
		t.Fatalf("the next revision names the first's wire hash as its base: %+v", sent)
	}
	if err := ix.DeleteMemory("deploy", teamHuman()); err != nil {
		t.Fatal(err)
	}
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	tombs := fake.records(teamwire.KindTombstone)
	var tomb teamwire.Tombstone
	if len(tombs) != 1 || json.Unmarshal(tombs[0].Body, &tomb) != nil || tomb.RecordType != teamwire.TombstoneMemory || tomb.RecordID != rec.GlobalID {
		t.Fatalf("the deletion pushes a memory tombstone: %+v", tombs)
	}
	if v, err := schemas.Violations("tombstone.schema.json", tombs[0].Body); err != nil || len(v) > 0 {
		t.Fatalf("tombstone schema: %v %v", v, err)
	}
	if st := tl.status(); st.Outbox.ByKind[teamwire.KindMemory]+st.Outbox.ByKind[teamwire.KindTombstone] != 0 {
		t.Fatalf("drained: %+v", st.Outbox)
	}
}

// Criterion 44, the device half (decision 9): the footer's fact is the content the server
// accepted; "delete what was sent" queues a session_content tombstone naming the WIRE
// session id, committed to the sent body hashes, and turns sharing off for the session.
func TestDeleteWhatWasSent(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	fake.script(map[string]string{}, false)
	sentFor := func() int {
		rec := httptest.NewRecorder()
		handleTeamContentSent(rec, httptest.NewRequest("GET", "/api/team/content/sent?runtime=claude&session=s-del", nil))
		var out teamContentSentResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("sent: %d %s", rec.Code, rec.Body)
		}
		return out.Chunks
	}
	appendWithInput(t, ix, "s-del", `{"command":"before opt-in"}`)
	if code, body := postContent(t, `{"runtime":"claude","session":"s-del","enabled":false,"delete_sent":true}`); code != http.StatusConflict {
		t.Fatalf("nothing sent yet: %d %s", code, body)
	}
	if code, _ := postContent(t, `{"runtime":"claude","session":"s-del","enabled":true}`); code != 200 {
		t.Fatal("opt in")
	}
	appendWithInput(t, ix, "s-del", `{"command":"echo shared"}`)
	tl.pushOnce()
	if n := sentFor(); n != 1 {
		t.Fatalf("one accepted chunk is what a deletion would cover, got %d", n)
	}
	if code, body := postContent(t, `{"runtime":"claude","session":"s-del","enabled":true,"delete_sent":true}`); code != http.StatusBadRequest {
		t.Fatalf("delete_sent with enabled true: %d %s", code, body)
	}
	code, body := postContent(t, `{"runtime":"claude","session":"s-del","enabled":false,"delete_sent":true}`)
	if code != 200 || strings.Contains(body, "claude/s-del") {
		t.Fatalf("the deletion also withdraws the session's consent: %d %s", code, body)
	}
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	tombs := fake.records(teamwire.KindTombstone)
	var tomb teamwire.Tombstone
	if len(tombs) != 1 || json.Unmarshal(tombs[0].Body, &tomb) != nil {
		t.Fatalf("one tombstone: %+v", tombs)
	}
	deviceID, _, _ := ix.Device()
	if tomb.RecordType != teamwire.TombstoneSessionContent || tomb.RecordID != engine.WireSessionID(deviceID, "claude/s-del") ||
		len(tomb.RequiredProjectionCleanup) != 1 || tomb.RequiredProjectionCleanup[0] != "cache" || !strings.HasPrefix(tomb.PriorContentHash, "sha256:") {
		t.Fatalf("the tombstone names the wire session and what was sent: %+v", tomb)
	}
	if v, err := schemas.Violations("tombstone.schema.json", tombs[0].Body); err != nil || len(v) > 0 {
		t.Fatalf("schema: %v %v", v, err)
	}
	if n := sentFor(); n != 0 {
		t.Fatalf("covered content offers no further deletion, got %d", n)
	}
	// Content captured after the deletion is not sent: consent is off until re-opted in.
	appendWithInput(t, ix, "s-del", `{"command":"after the deletion"}`)
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	if fake.got(teamwire.KindSessionContent) != 0 {
		t.Fatal("sharing must be off after the deletion")
	}
}

// memoryPullFake serves pull pages and records acknowledgements.
type memoryPullFake struct {
	pushFake
	pullMu sync.Mutex
	rows   []teamwire.PullRow
	acked  int64
}

func (f *memoryPullFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case teamwire.RoutePull:
		var in teamwire.PullRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &in)
		f.pullMu.Lock()
		out := teamwire.PullResponse{Rows: []teamwire.PullRow{}, Cursor: in.Cursor}
		for _, row := range f.rows {
			if row.Seq > in.Cursor {
				out.Rows = append(out.Rows, row)
				out.Cursor = row.Seq
			}
		}
		f.pullMu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	case teamwire.RouteAck:
		var in teamwire.AckRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &in)
		f.pullMu.Lock()
		f.acked = in.Cursor
		f.pullMu.Unlock()
		_ = json.NewEncoder(w).Encode(teamwire.AckResponse{Cursor: in.Cursor})
	default:
		f.pushFake.ServeHTTP(w, r)
	}
}

func pulledRow(seq int64, gid, slug, scopeType, scopeID, body string, rev int64) teamwire.PullRow {
	rec := teamwire.MemoryRecord{SchemaVersion: "1.1", ID: gid, Slug: slug, Revision: 1, Scope: teamwire.MemoryScope{Type: scopeType, ID: scopeID},
		Title: "T " + slug, Category: "gotcha", Body: body, Tags: []string{}, Aliases: []string{}, Source: "human",
		CreatedAt: "2026-10-01T00:00:00Z", UpdatedAt: "2026-10-01T00:00:00Z", Author: teamwire.Actor{Type: "user", ID: "someone"}}
	if scopeType == "repository" {
		rec.RepositoryIdentity = "remote-sha256"
	}
	rec.ContentHash = teamwire.MemoryWireHash(rec)
	return teamwire.PullRow{Seq: seq, Kind: teamwire.KindMemory, Memory: &teamwire.PulledMemory{Record: rec, WireHash: rec.ContentHash, ServerRevision: rev, AuthorUserID: "usr_teammate", AuthorName: "Teammate"}}
}

// Criterion 7, the device half (decisions 5, 8, 16, 17): a pull lands team records,
// writes their mirror files WITHOUT a git commit, advances and acknowledges the cursor;
// a pulled deletion removes the record, its revisions and its mirror file; a re-pull
// after a crash is a no-op.
func TestMemoryPullLandsMirrorsAndAcknowledges(t *testing.T) {
	tl, ix, _ := teamTestLinker(t)
	fake := &memoryPullFake{}
	fake.answer = map[string]string{}
	fake.status.Store("approved")
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	tl.doc.PushInterval.Duration, tl.doc.PullInterval.Duration = time.Hour, time.Hour
	if _, err := tl.beginLink(ts.URL, "laptop"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); tl.status().State != teamLinked && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	gid := engine.NewTypedID("mem")
	fake.pullMu.Lock()
	fake.rows = []teamwire.PullRow{pulledRow(1, gid, "bastion", "organization", "org_1", "use the bastion", 1)}
	fake.pullMu.Unlock()
	tl.pullMemoryOnce()
	rec, ok, err := ix.MemoryByGlobalID(gid)
	if err != nil || !ok || rec.SyncOrigin != "pulled" || rec.TeamAuthor != "Teammate" || rec.Status != "active" || rec.ShareState != "shared" {
		t.Fatalf("landed: %+v %v %v", rec, ok, err)
	}
	mirror := filepath.Join(memory.DefaultDir(), "bastion.md")
	if raw, err := os.ReadFile(mirror); err != nil || !strings.Contains(string(raw), "use the bastion") {
		t.Fatalf("the mirror file is written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(memory.DefaultDir(), ".git")); err == nil {
		t.Fatal("a pull must never create or commit to a git repository in the mirror")
	}
	if n := pending(t, ix).ByKind[store.OutboxMemory]; n != 0 {
		t.Fatalf("landing never enqueues, %d pending", n)
	}
	fake.pullMu.Lock()
	acked := fake.acked
	fake.pullMu.Unlock()
	if cursor, _ := ix.SyncCursor(memoryPullCursor("org_1")); cursor != 1 || acked != 1 {
		t.Fatalf("cursor advanced and acknowledged: %d %d", cursor, acked)
	}
	if st := tl.status(); st.Memory == nil || st.Memory.Pulled != 1 || st.Memory.Pull.Cursor != 1 || st.Memory.Pull.Outcome != "pulled" {
		t.Fatalf("status: %+v", st.Memory)
	}
	// A pulled deletion: record, revisions and mirror file go.
	fake.pullMu.Lock()
	fake.rows = append(fake.rows, teamwire.PullRow{Seq: 2, Kind: teamwire.KindTombstone, Tombstone: &teamwire.PulledTombstone{DeletedByName: "Teammate",
		Tombstone: teamwire.Tombstone{SchemaVersion: "1.0", ID: engine.NewTypedID("tmb"), RecordID: gid, RecordType: teamwire.TombstoneMemory,
			DeletedAt: "2026-10-02T00:00:00Z", DeletedBy: teamwire.Actor{Type: "user", ID: "x"}, ReasonClass: "user-request",
			PriorContentHash: rec.PushedHash, RequiredProjectionCleanup: []string{"local-search"}}}})
	fake.pullMu.Unlock()
	tl.pullMemoryOnce()
	if _, ok, _ := ix.MemoryByGlobalID(gid); ok {
		t.Fatal("the pulled deletion must remove the record")
	}
	if _, err := os.Stat(mirror); err == nil {
		t.Fatal("the mirror file must be removed")
	}
	// A crash before the cursor advanced: the same rows again land nothing.
	if eff, err := ix.ApplyPulledRows(fake.rows, 1, ""); err != nil || len(eff.Landed) != 0 {
		t.Fatalf("re-pull after a deletion re-creates nothing: %+v %v", eff, err)
	}
}

func gitRepo(t *testing.T, name, remote string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		if remote == "" && args[0] == "remote" {
			continue
		}
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

// Criteria 6 and 50 (decisions 12, 18): repository identity is minted where a checkout
// has one origin remote, on the daemon's doors; the records route answers the session's
// recall scope from its cwd; and a search from that checkout returns the organization
// record and this repository's record, not another repository's.
func TestMintingAndScopedRecallFromACheckout(t *testing.T) {
	_, ix, _ := teamTestLinker(t)
	setIndexPath(filepath.Dir(indexPathForTest(t, ix)))
	t.Cleanup(func() { setIndexPath("") })
	here := gitRepo(t, "service", "https://git.example.com/acme/service.git")
	elsewhere := gitRepo(t, "service", "https://git.example.com/acme/other.git")
	noRemote := gitRepo(t, "scratch", "")
	repo, err := changeenv.ResolveRepository(here)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := changeenv.ResolveRepository(elsewhere)

	// Minting (the daemon's doors resolve through mintMemoryScope).
	if id, kind, note := mintMemoryScope(here, ""); id != repo.ID || kind != "remote-sha256" || note != "" {
		t.Fatalf("one origin remote mints the remote identity: %q %q %q", id, kind, note)
	}
	if id, kind, note := mintMemoryScope(noRemote, ""); kind != "weak" || id != "scratch" || note == "" {
		t.Fatalf("no remote stays weak, with the reason: %q %q %q", id, kind, note)
	}
	if id, kind, note := mintMemoryScope(here, "some-other-repo"); kind != "weak" || id != "some-other-repo" || note == "" {
		t.Fatalf("a label naming another repository stays weak: %q %q %q", id, kind, note)
	}
	// The MCP propose door mints from the proposing session's folder.
	dataDir := filepath.Dir(indexPathForTest(t, ix))
	if err := os.WriteFile(filepath.Join(dataDir, "memory.json"), []byte(`{"format_version":1,"propose":{"enabled":true,"per_session_max":5}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	memoryProposeLimits = &proposeLimiter{counts: map[string]int{}, since: time.Now()}
	body, _ := json.Marshal(map[string]any{"title": "proposed", "body": "b", "repository": "service", "session": "claude/s1", "cwd": here})
	rec := httptest.NewRecorder()
	handleMemoryPropose(rec, httptest.NewRequest("POST", "/api/memory/propose", strings.NewReader(string(body))))
	if rec.Code != 200 {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	var proposed memoryProposeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &proposed)
	if p, err := ix.MemoryByID(proposed.ID); err != nil || p.RepositoryIdentity != "remote-sha256" || p.ScopeID != repo.ID || p.Status != "pending" {
		t.Fatalf("a proposal from a single-remote checkout is remote-identified: %+v %v", p, err)
	}

	// Scoped recall and search.
	for _, r := range []store.MemoryRecord{
		{ID: "org-wide", Title: "bastion for everyone", Category: "note", Body: "bastion", Source: "human", Status: "active", ScopeType: store.MemoryScopeOrganization, ScopeID: "org_1", AuthorType: "user", AuthorID: "t"},
		{ID: "this-repo", Title: "bastion here", Category: "note", Body: "bastion", Source: "human", Status: "active", ScopeType: store.MemoryScopeRepository, ScopeID: repo.ID, RepositoryIdentity: "remote-sha256", AuthorType: "user", AuthorID: "t"},
		{ID: "other-repo", Title: "bastion there", Category: "note", Body: "bastion", Source: "human", Status: "active", ScopeType: store.MemoryScopeRepository, ScopeID: other.ID, RepositoryIdentity: "remote-sha256", AuthorType: "user", AuthorID: "t"},
	} {
		if _, err := ix.CreateMemory(r, nil, nil, teamHuman()); err != nil {
			t.Fatal(err)
		}
	}
	rr := httptest.NewRecorder()
	handleMemoryRecords(rr, httptest.NewRequest("GET", "/api/memory/records?status=active&cwd="+here, nil))
	var listed memoryRecordsResponse
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &listed) != nil || listed.RecallScope == nil ||
		listed.RecallScope.RepositoryID != repo.ID || listed.RecallScope.Label != "service" || listed.RecallScope.Resolution != placeGit {
		t.Fatalf("the daemon resolves the hook's cwd to the session's repository: %d %s", rr.Code, rr.Body)
	}
	sr := httptest.NewRecorder()
	handleMemorySearch(sr, httptest.NewRequest("GET", "/api/memory/search?q=bastion&cwd="+here, nil))
	if sr.Code != 200 || !strings.Contains(sr.Body.String(), `"org-wide"`) || !strings.Contains(sr.Body.String(), `"this-repo"`) || strings.Contains(sr.Body.String(), `"other-repo"`) {
		t.Fatalf("search from the checkout returns the organization record and this repository's: %s", sr.Body)
	}
}

func indexPathForTest(t *testing.T, _ *store.Index) string {
	t.Helper()
	return filepath.Join(team.storeDir, "index.sqlite")
}

// Decisions 14 and 18: the guarded identity upgrade turns a weak record remote only when
// exactly one seen checkout carries its label and has one origin remote; it never
// enqueues; an ambiguous label stays weak with the reason; the share action is what sends.
func TestIdentityUpgradeIsGuardedAndNeverEnqueues(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	fake.script(map[string]string{}, false)
	unique := gitRepo(t, "unique-svc", "https://git.example.com/acme/unique.git")
	dupA := gitRepo(t, "twin", "https://git.example.com/acme/twin-a.git")
	dupB := gitRepo(t, "twin", "https://git.example.com/acme/twin-b.git")
	for i, root := range []string{unique, dupA, dupB} {
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := tx.EnsureSessionCheckpoint(store.SessionCheckpoint{Runtime: "claude", SessionID: "claude/up-" + string(rune('a'+i)), ScopeKey: root,
			Kind: "attachment", WorkingDirectory: root, RepositoryID: "rep", CheckoutID: "chk", CheckoutRoot: root, Status: "pending",
			BoundaryClass: "unconfirmed", RequestedAt: time.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, label := range []string{"unique-svc", "twin", "never-seen"} {
		r := store.MemoryRecord{ID: "rec-" + label, Title: label, Category: "note", Body: "b", Source: "human", Status: "active",
			ScopeType: store.MemoryScopeRepository, ScopeID: label, RepositoryIdentity: "weak", AuthorType: "user", AuthorID: "t"}
		if _, err := ix.CreateMemory(r, nil, nil, teamHuman()); err != nil {
			t.Fatal(err)
		}
	}
	tl.upgradeMemoryIdentities()
	up, _ := ix.MemoryByID("rec-unique-svc")
	repo, _ := changeenv.ResolveRepository(unique)
	if up.RepositoryIdentity != "remote-sha256" || up.ScopeID != repo.ID || up.ShareState != "unshared" {
		t.Fatalf("a unique label upgrades and stays unshared: %+v", up)
	}
	twin, _ := ix.MemoryByID("rec-twin")
	never, _ := ix.MemoryByID("rec-never-seen")
	if twin.RepositoryIdentity != "weak" || !strings.Contains(twin.IdentityNote, "more than one repository") ||
		never.RepositoryIdentity != "weak" || !strings.Contains(never.IdentityNote, "no checkout") {
		t.Fatalf("ambiguous and unseen labels stay weak, with the reason: %q / %q", twin.IdentityNote, never.IdentityNote)
	}
	if n := pending(t, ix).ByKind[store.OutboxMemory]; n != 0 {
		t.Fatalf("an identity upgrade enqueues nothing, %d pending", n)
	}
	rec := httptest.NewRecorder()
	handleTeamMemoryShare(rec, httptest.NewRequest("POST", "/api/team/memory/share", strings.NewReader(`{}`)))
	var shared teamMemoryShareResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &shared) != nil || shared.Shared != 1 || shared.Memory.ShareCandidates != 0 {
		t.Fatalf("the share action shares the one candidate: %d %s", rec.Code, rec.Body)
	}
	tl.pushOnce()
	if fake.got(teamwire.KindMemory) != 1 {
		t.Fatalf("the shared record is pushed: %d", fake.got(teamwire.KindMemory))
	}
}

// Criterion 40 at the drain (PW test-honesty): a memory revision the server answers
// `internal` stays queued and frozen, is retried byte-identical, and settles when
// accepted; one that keeps failing is dead-lettered as internal_exhausted — and the next
// revision of the record is never made a conflict copy by either.
func TestMemoryInternalAnswersRetryAndExhaust(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	rec, err := ix.CreateMemory(teamOrgRecord("flaky", "v1"), nil, nil, teamHuman())
	if err != nil {
		t.Fatal(err)
	}
	fake.script(map[string]string{teamwire.KindMemory: teamwire.CodeInternal}, false)
	tl.pushOnce()
	first := fake.records(teamwire.KindMemory)
	if len(first) != 1 || pending(t, ix).ByKind[store.OutboxMemory] != 1 {
		t.Fatalf("an internal answer leaves the row queued: %d sent, %+v", len(first), pending(t, ix))
	}
	cur, _ := ix.MemoryByID("flaky")
	cur.Body = "v2, written while v1 was unanswered"
	if _, err := ix.UpsertMemory(cur, nil, nil, teamHuman()); err != nil {
		t.Fatal(err)
	}
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	retry := fake.records(teamwire.KindMemory)
	if len(retry) != 1 || string(retry[0].Body) != string(first[0].Body) {
		t.Fatalf("the retry resends the frozen body, one revision in flight: %d", len(retry))
	}
	fake.script(map[string]string{}, false)
	tl.pushOnce()
	if conflicts, _ := ix.MemoryConflicts(rec.GlobalID, 0); len(conflicts) != 0 || pending(t, ix).ByKind[store.OutboxMemory] != 0 {
		t.Fatalf("revision 2 follows, with no conflict copy: %+v", conflicts)
	}
	// A revision the server can never store is dead-lettered, not retried forever.
	tl.mu.Lock()
	tl.doc.PushMaxAttempts = 2
	tl.mu.Unlock()
	cur, _ = ix.MemoryByID("flaky")
	cur.Body = "v3"
	if _, err := ix.UpsertMemory(cur, nil, nil, teamHuman()); err != nil {
		t.Fatal(err)
	}
	fake.script(map[string]string{teamwire.KindMemory: teamwire.CodeInternal}, false)
	tl.pushOnce()
	tl.pushOnce()
	if st := tl.status(); st.Outbox.DeadLetter[teamwire.KindMemory]["internal_exhausted"] != 1 || st.Outbox.ByKind[teamwire.KindMemory] != 0 {
		t.Fatalf("exhausted: %+v", st.Outbox)
	}
	if got, _ := ix.MemoryByID("flaky"); store.MemoryProjectionHash(got) == got.SyncedProjectionHash {
		t.Fatal("an unsent revision leaves the record diverged, so a later pull keeps it as a conflict copy")
	}
}

// linkTo links a test linker to a fake team server through the real enrollment flow,
// with cadences long enough that only the test's own ticks run.
func linkTo(t *testing.T, tl *teamLinker, handler http.Handler) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	tl.mu.Lock()
	tl.doc.PushInterval.Duration, tl.doc.PullInterval.Duration, tl.doc.MemoryPullStartDelay.Duration = time.Hour, time.Hour, time.Hour
	tl.mu.Unlock()
	if _, err := tl.beginLink(ts.URL, "laptop"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); tl.status().State != teamLinked && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if st := tl.status(); st.State != teamLinked {
		t.Fatalf("not linked: %+v", st)
	}
}

func contentSent(t *testing.T, session string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	handleTeamContentSent(rec, httptest.NewRequest("GET", "/api/team/content/sent?runtime=claude&session="+session, nil))
	var out teamContentSentResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("sent: %d %s", rec.Code, rec.Body)
	}
	return out.Chunks
}

// CR-1 through the daemon: content one team server accepted is not what the next one
// holds. After unlink and a link to ANOTHER server the footer's fact is zero, the delete
// action is refused, and no tombstone is sent for the old link's content. A relink to
// the same server (which keeps the device's row) gets its content back.
func TestRelinkDoesNotOfferDeletionOfAnotherServersContent(t *testing.T) {
	tl, ix, _ := teamTestLinker(t)
	first := &pushFake{answer: map[string]string{}}
	first.status.Store("approved")
	linkTo(t, tl, first)
	appendWithInput(t, ix, "s-old", `{"command":"before the opt-in"}`)
	if code, body := postContent(t, `{"runtime":"claude","session":"s-old","enabled":true}`); code != 200 {
		t.Fatalf("opt in: %d %s", code, body)
	}
	appendWithInput(t, ix, "s-old", `{"command":"echo for the first team"}`)
	tl.pushOnce()
	if n := contentSent(t, "s-old"); n != 1 {
		t.Fatalf("the first server holds one chunk: %d", n)
	}
	if _, err := tl.unlink(); err != nil {
		t.Fatal(err)
	}
	second := &pushFake{answer: map[string]string{}}
	second.status.Store("approved")
	second.orgID = "org_2"
	linkTo(t, tl, second)
	if n := contentSent(t, "s-old"); n != 0 {
		t.Fatalf("the second server holds none of it; the footer must offer nothing, got %d", n)
	}
	if code, body := postContent(t, `{"runtime":"claude","session":"s-old","enabled":false,"delete_sent":true}`); code != http.StatusConflict {
		t.Fatalf("a deletion of the old link's content must be refused: %d %s", code, body)
	}
	tl.pushOnce()
	if n := second.got(teamwire.KindTombstone); n != 0 {
		t.Fatalf("no tombstone may reach the second server, %d sent", n)
	}
	if org, _ := ix.LinkedOrganization(); org != "org_2" {
		t.Fatalf("the store knows its link: %q", org)
	}
	// Back to the first server: the device's row and its content are still there.
	if _, err := tl.unlink(); err != nil {
		t.Fatal(err)
	}
	linkTo(t, tl, first)
	if n := contentSent(t, "s-old"); n != 1 {
		t.Fatalf("a relink to the same server keeps what it accepted: %d", n)
	}
}

// Criterion 48 (O-8) with a really adopted bundle: the organization publishes a signed
// bundle whose content policy mandates content; the device verifies it, pins the key and
// ADOPTS it through the adopt route. Content then flows as mandated-by-bundle with no
// opt-in; "delete what was sent" is allowed under the mandate; and content produced
// afterwards is shared again, per the mandate.
func TestDeletionUnderAnAdoptedContentMandate(t *testing.T) {
	tl, ix, dir := teamTestLinker(t)
	lf := &layersFake{}
	lf.teamFake.status.Store("approved")
	_, orgKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	entry, signed, digest := signedOrgBundle(t, "organization", "org-mandate-rule", "never-matches", orgKey, "k_mandate",
		map[string]any{"content_policy": map[string]any{"sync_content": "mandated", "repositories": []string{}}})
	lf.catalog = map[string]any{"bundles": []any{map[string]any{"id": entry.ID, "scope": entry.Scope, "revision": entry.Revision,
		"expires_at": entry.ExpiresAt, "failure_mode": entry.FailureMode,
		"documents":      []any{map[string]any{"kind": "rulebook", "name": "test rules", "digest": digest, "media_type": "application/json"}},
		"org_public_key": entry.OrgPublicKey}}}
	lf.signedByID = map[string][]byte{entry.ID: signed}
	var mu sync.Mutex
	var received []teamwire.PushRecord
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != teamwire.RoutePush {
			lf.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var in teamwire.PushRequest
		_ = json.Unmarshal(body, &in)
		out := teamwire.PushResponse{ServerTime: time.Now().Unix()}
		mu.Lock()
		for _, rec := range in.Records {
			received = append(received, rec)
			out.Results = append(out.Results, teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusAccepted})
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	linkTo(t, tl, handler)
	for i := 0; i < 100 && loadPinnedOrgKey(dir).KeyID != "k_mandate"; i++ {
		tl.pullLayersOnce()
		time.Sleep(20 * time.Millisecond)
	}
	tl.pullLayersOnce()
	tl.mu.Lock()
	available := tl.available
	tl.mu.Unlock()
	if len(available) != 1 {
		t.Fatalf("the bundle must be offered: %+v", available)
	}
	if st := tl.status(); st.Content.Mandate != nil {
		t.Fatal("an offered bundle mandates nothing: availability is not activation")
	}
	rec := httptest.NewRecorder()
	handleTeamAdopt(rec, httptest.NewRequest("POST", "/api/team/layers/adopt", strings.NewReader(`{"scope":"organization","digest":"`+available[0].ID+`","state_token":"`+available[0].StateToken+`"}`)))
	if rec.Code != 200 {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body)
	}
	tl.pullLayersOnce() // the adoption shows in the offer
	if st := tl.status(); st.Content.Mandate == nil || st.Content.Mandate.BundleID != available[0].ID {
		t.Fatalf("the adopted bundle's mandate is in force: %+v", st.Content)
	}
	chunks := func() (out []teamwire.ContentChunk, tombs []teamwire.Tombstone) {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range received {
			switch r.Kind {
			case teamwire.KindSessionContent:
				var c teamwire.ContentChunk
				_ = json.Unmarshal(r.Body, &c)
				out = append(out, c)
			case teamwire.KindTombstone:
				var tb teamwire.Tombstone
				_ = json.Unmarshal(r.Body, &tb)
				tombs = append(tombs, tb)
			}
		}
		return out, tombs
	}
	tl.pushOnce() // the drain records the adopted mandate in the store; content is forward only
	appendWithInput(t, ix, "s-mandated", `{"command":"echo under the mandate"}`)
	tl.pushOnce()
	sent, _ := chunks()
	if len(sent) != 1 || sent[0].Consent != teamlink.ContentMandated {
		t.Fatalf("content flows under the mandate with no opt-in: %+v", sent)
	}
	if n := contentSent(t, "s-mandated"); n != 1 {
		t.Fatalf("the footer's fact: %d", n)
	}
	if code, body := postContent(t, `{"runtime":"claude","session":"s-mandated","enabled":false,"delete_sent":true}`); code != 200 {
		t.Fatalf("deletion is allowed under a mandate (O-8): %d %s", code, body)
	}
	tl.pushOnce()
	if _, tombs := chunks(); len(tombs) != 1 || tombs[0].RecordType != teamwire.TombstoneSessionContent {
		t.Fatalf("the deletion is sent: %+v", tombs)
	}
	appendWithInput(t, ix, "s-mandated", `{"command":"echo after the deletion"}`)
	tl.pushOnce()
	if sent, _ := chunks(); len(sent) != 2 || sent[1].Consent != teamlink.ContentMandated || !strings.Contains(sent[1].Body, "after the deletion") {
		t.Fatalf("content produced afterwards is shared per the mandate: %+v", sent)
	}
}

// heldAnswerFake is a team server for the held-pull cases: pull pages, and a push answer
// of the test's choosing with what the real server adds to a memory answer.
func heldAnswerSetup(t *testing.T) (*teamLinker, *store.Index, *memoryPullFake, string) {
	t.Helper()
	tl, ix, _ := teamTestLinker(t)
	fake := &memoryPullFake{}
	fake.answer = map[string]string{}
	fake.status.Store("approved")
	linkTo(t, tl, fake)
	gid := engine.NewTypedID("mem")
	fake.pullMu.Lock()
	fake.rows = []teamwire.PullRow{pulledRow(1, gid, "runbook", "organization", "org_1", "v1", 1)}
	fake.pullMu.Unlock()
	tl.pullMemoryOnce()
	rec, ok, err := ix.MemoryByGlobalID(gid)
	if err != nil || !ok {
		t.Fatalf("setup: %v %v", ok, err)
	}
	rec.Body = "this device's edit"
	if _, err := ix.UpsertMemory(rec, nil, nil, teamHuman()); err != nil {
		t.Fatal(err)
	}
	// A teammate's revision 2 arrives while this device's edit is unanswered: held.
	theirs := pulledRow(2, gid, "runbook", "organization", "org_1", "the teammate's revision", 2)
	fake.pullMu.Lock()
	fake.rows = append(fake.rows, theirs)
	fake.pullMu.Unlock()
	tl.pullMemoryOnce()
	if held, _, _ := ix.MemoryByGlobalID(gid); held.HeldServerRevision != 2 || held.Body != "this device's edit" {
		t.Fatalf("the pull over an unacked push must be held: %+v", held)
	}
	return tl, ix, fake, gid
}

// Criterion 40 (Q-2) through the DRAIN: a pull held behind this device's unanswered push
// lands — or is rightly discarded — after each answer the server or the device can give,
// with the answers produced by the push route, not fed to the store by hand.
func TestHeldPullSettlesThroughTheDrainForEveryAnswer(t *testing.T) {
	theirs := func(fake *memoryPullFake) *teamwire.PulledMemory {
		fake.pullMu.Lock()
		defer fake.pullMu.Unlock()
		return fake.rows[1].Memory
	}
	for _, answer := range []string{teamwire.StatusAccepted, teamwire.StatusDuplicate, teamwire.CodeStaleBase, teamwire.CodeNotShareable, "internal_exhausted"} {
		t.Run(answer, func(t *testing.T) {
			tl, ix, fake, gid := heldAnswerSetup(t)
			switch answer {
			case teamwire.StatusAccepted, teamwire.StatusDuplicate:
				// The server stored this device's revision after the teammate's: number 3.
				fake.script(map[string]string{teamwire.KindMemory: answer}, false)
				fake.memory = func(teamwire.PushRecord, string, string) *teamwire.MemoryAnswer {
					return &teamwire.MemoryAnswer{ServerRevision: 3}
				}
			case teamwire.CodeStaleBase:
				fake.script(map[string]string{teamwire.KindMemory: teamwire.CodeStaleBase}, false)
				current := theirs(fake)
				fake.memory = func(teamwire.PushRecord, string, string) *teamwire.MemoryAnswer {
					return &teamwire.MemoryAnswer{Current: current}
				}
			case teamwire.CodeNotShareable:
				// Sharing withdrawn after the edit was queued: the drain refuses it locally.
				raw, err := driver.Open("file:" + filepath.Join(tl.storeDir, "index.sqlite") + "?_pragma=busy_timeout(2000)")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`UPDATE memory_record SET share_state='unshared' WHERE global_id=?`, gid); err != nil {
					t.Fatal(err)
				}
				_ = raw.Close()
				fake.script(map[string]string{}, false)
			case "internal_exhausted":
				tl.mu.Lock()
				tl.doc.PushMaxAttempts = 1
				tl.mu.Unlock()
				fake.script(map[string]string{teamwire.KindMemory: teamwire.CodeInternal}, false)
			}
			tl.pushOnce()
			got, ok, err := ix.MemoryByGlobalID(gid)
			if err != nil || !ok || got.HeldServerRevision != 0 {
				t.Fatalf("after %s the held slot must be empty: %+v %v %v", answer, got, ok, err)
			}
			conflicts, _ := ix.MemoryConflicts(gid, 10)
			switch answer {
			case teamwire.StatusAccepted, teamwire.StatusDuplicate:
				if got.Body != "this device's edit" || got.ServerRevision != 3 || len(conflicts) != 0 {
					t.Fatalf("%s: this device's revision is the newer one, the held one is discarded: %+v %+v", answer, got, conflicts)
				}
			default:
				if got.Body != "the teammate's revision" || got.ServerRevision != 2 || len(conflicts) != 1 || conflicts[0].Body != "this device's edit" {
					t.Fatalf("%s: the teammate's revision lands and the edit is a conflict copy: %+v %+v", answer, got, conflicts)
				}
			}
			if pending(t, ix).ByKind[store.OutboxMemory] != 0 {
				t.Fatalf("%s: the row is settled", answer)
			}
		})
	}
}

// pagingFake serves one row per page and says more are waiting, forever.
type pagingFake struct {
	pushFake
	pulls  atomic.Int64
	onPull func() // runs while a pull is in flight
}

func (f *pagingFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case teamwire.RoutePull:
		n := f.pulls.Add(1)
		if f.onPull != nil {
			f.onPull()
		}
		_ = json.NewEncoder(w).Encode(teamwire.PullResponse{More: true, Cursor: n,
			Rows: []teamwire.PullRow{pulledRow(n, engine.NewTypedID("mem"), "page-"+strconv.FormatInt(n, 10), "organization", "org_1", "b", 1)}})
	case teamwire.RouteAck:
		_ = json.NewEncoder(w).Encode(teamwire.AckResponse{})
	default:
		f.pushFake.ServeHTTP(w, r)
	}
}

// Criterion 49, the client's bound: one tick lands at most memory_pull_pages pages and
// resumes from its cursor on the next. PW-M4: a tick whose link ended while its page was
// in flight lands nothing and records no cursor.
func TestPullTickIsBoundedAndDropsAPageFromAnEndedLink(t *testing.T) {
	tl, ix, _ := teamTestLinker(t)
	fake := &pagingFake{}
	fake.answer = map[string]string{}
	fake.status.Store("approved")
	linkTo(t, tl, fake)
	tl.mu.Lock()
	tl.doc.MemoryPullPages = 2
	tl.mu.Unlock()
	tl.pullMemoryOnce()
	if n := fake.pulls.Load(); n != 2 {
		t.Fatalf("one tick pulls memory_pull_pages pages: %d", n)
	}
	if c, _ := ix.SyncCursor(memoryPullCursor("org_1")); c != 2 {
		t.Fatalf("and stops at its cursor: %d", c)
	}
	tl.pullMemoryOnce()
	if c, _ := ix.SyncCursor(memoryPullCursor("org_1")); c != 4 || fake.pulls.Load() != 4 {
		t.Fatalf("the next tick resumes from it: cursor %d after %d pulls", c, fake.pulls.Load())
	}
	// The link ends while a page is in flight: the generation moved, nothing is written.
	recs, _ := ix.ListMemory("")
	before := len(recs)
	fake.onPull = func() {
		tl.mu.Lock()
		tl.gen++
		tl.mu.Unlock()
	}
	tl.pullMemoryOnce()
	recs, _ = ix.ListMemory("")
	if c, _ := ix.SyncCursor(memoryPullCursor("org_1")); c != 4 || len(recs) != before {
		t.Fatalf("a page pulled under an ended link must not land: cursor %d, %d records (was %d)", c, len(recs), before)
	}
}

// PW-M6: the identity upgrade forks git only when its inputs changed — the weak labels
// and the checkout roots seen — and a resolution that times out makes the whole pass
// inconclusive: nothing is upgraded on a guess.
func TestIdentityUpgradeRunsOnChangeAndNeverOnATimeout(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	fake.script(map[string]string{}, false)
	repo := gitRepo(t, "counted", "https://git.example.com/acme/counted.git")
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tx.EnsureSessionCheckpoint(store.SessionCheckpoint{Runtime: "claude", SessionID: "claude/c", ScopeKey: repo, Kind: "attachment",
		WorkingDirectory: repo, RepositoryID: "rep", CheckoutID: "chk", CheckoutRoot: repo, Status: "pending", BoundaryClass: "unconfirmed", RequestedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	weak := func(id string) {
		r := store.MemoryRecord{ID: id, Title: id, Category: "note", Body: "b", Source: "human", Status: "active",
			ScopeType: store.MemoryScopeRepository, ScopeID: "counted", RepositoryIdentity: "weak", AuthorType: "user", AuthorID: "t"}
		if _, err := ix.CreateMemory(r, nil, nil, teamHuman()); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	timeout := atomic.Bool{}
	real := resolveCheckout
	resolveCheckout = func(ctx context.Context, dir string) (changeenv.Repository, error) {
		calls.Add(1)
		if timeout.Load() {
			<-ctx.Done()
			return changeenv.Repository{}, ctx.Err()
		}
		return real(ctx, dir)
	}
	t.Cleanup(func() { resolveCheckout = real })
	tl.mu.Lock()
	tl.doc.IdentityResolveTimeout.Duration = 50 * time.Millisecond
	tl.mu.Unlock()

	weak("first")
	timeout.Store(true)
	tl.upgradeMemoryIdentities()
	if r, _ := ix.MemoryByID("first"); r.RepositoryIdentity != "weak" || calls.Load() != 1 {
		t.Fatalf("a timed-out resolution upgrades nothing: %s after %d calls", r.RepositoryIdentity, calls.Load())
	}
	timeout.Store(false)
	tl.mu.Lock()
	tl.doc.IdentityResolveTimeout.Duration = 10 * time.Second
	tl.mu.Unlock()
	tl.upgradeMemoryIdentities()
	if r, _ := ix.MemoryByID("first"); r.RepositoryIdentity != "remote-sha256" || calls.Load() != 2 {
		t.Fatalf("the inconclusive pass is retried and upgrades: %s after %d calls", r.RepositoryIdentity, calls.Load())
	}
	weak("second")
	tl.upgradeMemoryIdentities()
	after := calls.Load()
	if r, _ := ix.MemoryByID("second"); r.RepositoryIdentity != "remote-sha256" || after != 3 {
		t.Fatalf("a new weak record is new input: %s after %d calls", r.RepositoryIdentity, after)
	}
	weak("third")
	tl.upgradeMemoryIdentities()
	tl.upgradeMemoryIdentities()
	tl.upgradeMemoryIdentities()
	if calls.Load() != after+1 {
		t.Fatalf("unchanged inputs fork nothing: %d calls for three ticks", calls.Load()-after)
	}
}

// Criterion 40 (PW-M8) at the console's door: a save made against a revision that has
// since moved answers 409 and writes nothing; an edit of a team record without the
// revision it read answers 409; with the current revision it is saved.
func TestConsoleEditNamesTheRevisionItRead(t *testing.T) {
	tl, ix, fake, _ := linkForPush(t)
	fake.script(map[string]string{}, false)
	setIndexPath(tl.storeDir)
	t.Cleanup(func() { setIndexPath("") })
	rec, err := ix.CreateMemory(teamOrgRecord("handbook", "v1"), nil, nil, teamHuman())
	if err != nil {
		t.Fatal(err)
	}
	tl.pushOnce() // accepted: the team holds it
	cur, _ := ix.MemoryByID("handbook")
	cur.Body = "a teammate's revision landed meanwhile"
	if _, err := ix.UpsertMemory(cur, nil, nil, teamHuman()); err != nil {
		t.Fatal(err)
	}
	st := NewStore(tl.storeDir)
	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		handleMemoryUpsert(w, httptest.NewRequest("POST", "/api/memory", strings.NewReader(body)), st)
		return w.Code, w.Body.String()
	}
	if code, body := post(`{"id":"handbook","claim":"T handbook","body":"my stale save","revision":` + strconv.FormatInt(rec.Revision, 10) + `}`); code != http.StatusConflict {
		t.Fatalf("a stale save: %d %s", code, body)
	}
	if code, body := post(`{"id":"handbook","claim":"T handbook","body":"no revision at all"}`); code != http.StatusConflict {
		t.Fatalf("a team record's edit without a revision: %d %s", code, body)
	}
	if got, _ := ix.MemoryByID("handbook"); got.Body != "a teammate's revision landed meanwhile" {
		t.Fatalf("nothing may have been written: %q", got.Body)
	}
	now, _ := ix.MemoryByID("handbook")
	if code, body := post(`{"id":"handbook","claim":"T handbook","body":"an edit on the current revision","revision":` + strconv.FormatInt(now.Revision, 10) + `}`); code != 200 {
		t.Fatalf("on the current revision: %d %s", code, body)
	}
	if got, _ := ix.MemoryByID("handbook"); got.Body != "an edit on the current revision" || got.Status != "pending" {
		t.Fatalf("saved and re-drafted pending: %+v", got)
	}
}

// Criterion 50, the synthesis door: a lesson drafted from a session in a checkout with
// one origin remote is keyed by that remote; from a folder that is no checkout it stays
// weak, with the reason the console shows.
func TestSynthesisMintsRepositoryIdentity(t *testing.T) {
	teamTestLinker(t)
	repo := gitRepo(t, "lessons", "https://git.example.com/acme/lessons.git")
	resolved, err := changeenv.ResolveRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	detail := &SessionDetail{}
	detail.Runtime, detail.ID, detail.Cwd, detail.Title = "claude", "s-synth", repo, "a session"
	rec, _ := buildSynthesisProposal("lesson-1", detail, "two failures")
	if rec.ScopeType != store.MemoryScopeRepository || rec.RepositoryIdentity != "remote-sha256" || rec.ScopeID != resolved.ID || rec.Status != "pending" {
		t.Fatalf("a draft from a single-remote checkout is remote-identified: %+v", rec)
	}
	detail.Cwd = t.TempDir()
	rec, _ = buildSynthesisProposal("lesson-2", detail, "two failures")
	if rec.RepositoryIdentity != "weak" || rec.IdentityNote == "" {
		t.Fatalf("no checkout stays weak, with the reason: %+v", rec)
	}
}

// FR-6 through the daemon: a member's edit of an organization record is refused by the
// team; the record says so in GET /api/memory (`refused`, not in sync); the take-team-
// version route gives the edit up, and the pull it starts lands the team's revision and
// keeps the edit as a conflict copy. FR-1 at the console's door: a console edit of a team
// record stays that record, at its scope — no copy.
func TestARefusedEditIsShownAndTheTeamsVersionIsTaken(t *testing.T) {
	tl, ix, _ := teamTestLinker(t)
	fake := &memoryPullFake{}
	fake.answer = map[string]string{}
	fake.status.Store("approved")
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	tl.doc.PushInterval.Duration, tl.doc.PullInterval.Duration = time.Hour, time.Hour
	if _, err := tl.beginLink(ts.URL, "laptop"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); tl.status().State != teamLinked && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	setIndexPath(tl.storeDir)
	t.Cleanup(func() { setIndexPath("") })
	gid := engine.NewTypedID("mem")
	row := pulledRow(1, gid, "policy", "organization", "org_1", "the team's text", 1)
	fake.pullMu.Lock()
	fake.rows = []teamwire.PullRow{row}
	fake.pullMu.Unlock()
	tl.pullMemoryOnce()
	st := NewStore(tl.storeDir)
	teamOf := func() *MemoryTeam {
		t.Helper()
		list, err := st.ListMemories()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range list {
			if m.ID == "policy" {
				return m.Team
			}
		}
		t.Fatal("policy is not listed")
		return nil
	}
	if tm := teamOf(); tm == nil || tm.Refused != "" || !tm.InSync {
		t.Fatalf("landed, nothing refused: %+v", tm)
	}

	// The member edits it; the team refuses.
	cur, _ := ix.MemoryByID("policy")
	cur.Body = "a member's edit"
	if _, err := ix.UpsertMemory(cur, nil, nil, teamHuman()); err != nil {
		t.Fatal(err)
	}
	fake.script(map[string]string{teamwire.KindMemory: teamwire.CodeOrganizationScopeAdmin}, false)
	tl.pushOnce()
	if tm := teamOf(); tm.Refused != teamwire.CodeOrganizationScopeAdmin || tm.InSync {
		t.Fatalf("the record carries the refusal: %+v", tm)
	}

	take := func(body string) (int, string) {
		w := httptest.NewRecorder()
		handleTeamMemoryTake(w, httptest.NewRequest("POST", "/api/team/memory/take-team-version", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := take(`{"id":"no-such-record"}`); code != http.StatusNotFound {
		t.Fatalf("an unknown id: %d %s", code, body)
	}
	if code, body := take(`{}`); code != http.StatusBadRequest {
		t.Fatalf("no id: %d %s", code, body)
	}
	if code, body := take(`{"id":"policy"}`); code != 200 {
		t.Fatalf("take the team's version: %d %s", code, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := ix.MemoryByID("policy"); got.Body == "the team's text" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := ix.MemoryByID("policy")
	copies, _ := ix.MemoryConflicts(gid, 10)
	if got.Body != "the team's text" || len(copies) != 1 || copies[0].Reason != "pulled_over_edit" {
		t.Fatalf("the team's revision is back and the edit is kept as a conflict copy: %q %+v", got.Body, copies)
	}
	if tm := teamOf(); !tm.InSync {
		t.Fatalf("in sync again: %+v", tm)
	}
	if n := fake.got(teamwire.KindMemory); n != 1 {
		t.Fatalf("taking the team's version sends nothing more: %d memory pushes", n)
	}

	// FR-1, the console door: an edit of the team record is that record's next revision.
	w := httptest.NewRecorder()
	handleMemoryUpsert(w, httptest.NewRequest("POST", "/api/memory",
		strings.NewReader(`{"id":"policy","claim":"T policy","body":"edited in the console","revision":`+strconv.FormatInt(got.Revision, 10)+`}`)), st)
	after, _ := ix.MemoryByID("policy")
	all, _ := ix.ListMemory("")
	if w.Code != 200 || after.GlobalID != gid || after.ScopeType != store.MemoryScopeOrganization || after.Body != "edited in the console" || len(all) != 1 {
		t.Fatalf("a console edit stays the team record at its scope, with no copy: %d %+v (%d records)", w.Code, after, len(all))
	}
}
