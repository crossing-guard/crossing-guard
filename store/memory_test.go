package store

// memory_test.go — the write owner's contract (memory-first-class-records plan
// §8/§9): one transactional writer, revisions that chain, migration-idempotent
// inserts, tombstones that hold, sources that validate, and the outbox
// enqueue joining the same transaction.

import (
	"path/filepath"
	"testing"

	"crossing-guard/engine"
)

func openTestMemoryStore(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func memRecord(id string) MemoryRecord {
	return MemoryRecord{
		ID: id, Title: "T " + id, Category: "how-to", Body: "body of " + id,
		Tags: []string{"t"}, Aliases: []string{id + "-alt"},
		Source: "human", ScopeType: MemoryScopeUser,
		AuthorType: "user", AuthorID: "tester",
	}
}

func memHuman() MemoryActor {
	return MemoryActor{AuthorType: "user", AuthorID: "tester", ActorSource: "cli"}
}

func TestMemoryUpsertInsertsAndRevises(t *testing.T) {
	ix := openTestMemoryStore(t)
	r, err := ix.UpsertMemory(memRecord("alpha"), nil, nil, memHuman())
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if r.Revision != 1 || r.GlobalID == "" {
		t.Fatalf("first write must mint revision 1 and a global id, got rev=%d gid=%q", r.Revision, r.GlobalID)
	}
	if r.ContentHash != MemoryContentHash(r) {
		t.Fatalf("content hash must commit to the wire shape")
	}

	r.Body = "revised body"
	r2, err := ix.UpsertMemory(r, nil, nil, memHuman())
	if err != nil {
		t.Fatalf("revise: %v", err)
	}
	if r2.Revision != 2 {
		t.Fatalf("second write must bump revision, got %d", r2.Revision)
	}
	if r2.GlobalID != r.GlobalID {
		t.Fatalf("revision must keep the global id")
	}

	// Both revisions exist; the chain links them.
	var prevHash string
	if err := ix.db.QueryRow(`SELECT content_hash FROM memory_revision WHERE record_id=? AND revision=1`,
		"alpha").Scan(&prevHash); err != nil {
		t.Fatalf("revision 1 row missing: %v", err)
	}
	var gotPrev string
	if err := ix.db.QueryRow(`SELECT prev_hash FROM memory_revision WHERE record_id=? AND revision=2`,
		"alpha").Scan(&gotPrev); err != nil {
		t.Fatalf("revision 2 row missing: %v", err)
	}
	if gotPrev != prevHash {
		t.Fatalf("revision 2 prev_hash %q must match revision 1 hash %q", gotPrev, prevHash)
	}
}

func TestMemoryValidationEnforcesWireShape(t *testing.T) {
	ix := openTestMemoryStore(t)
	// RT-3: an empty author is invalid — wire actor {type,id} is required.
	noAuthor := memRecord("beta")
	noAuthor.AuthorType, noAuthor.AuthorID = "", ""
	if _, err := ix.UpsertMemory(noAuthor, nil, nil, memHuman()); err == nil {
		t.Fatal("record without author must be refused")
	}
	// scope discipline
	scoped := memRecord("gamma")
	scoped.ScopeType, scoped.ScopeID = MemoryScopeRepository, ""
	if _, err := ix.UpsertMemory(scoped, nil, nil, memHuman()); err == nil {
		t.Fatal("repository-scoped record without scope id must be refused")
	}
	// category enum
	badCat := memRecord("delta")
	badCat.Category = "vibes"
	if _, err := ix.UpsertMemory(badCat, nil, nil, memHuman()); err == nil {
		t.Fatal("unknown category must be refused")
	}
}

func TestMemorySourcesValidateAnchorKinds(t *testing.T) {
	ix := openTestMemoryStore(t)
	_, err := ix.UpsertMemory(memRecord("eps"), []MemorySource{
		{Vendor: "claude", SessionID: "s1", Anchor: "uuid-1", AnchorKind: "event-uuid"},
	}, nil, memHuman())
	if err != nil {
		t.Fatalf("valid source: %v", err)
	}
	srcs, err := ix.MemorySources("eps")
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources read back: %v %d", err, len(srcs))
	}
	if srcs[0].Vendor != "claude" || srcs[0].AnchorKind != "event-uuid" {
		t.Fatalf("source round-trip mismatch: %+v", srcs[0])
	}
	// anchor kind none cannot carry an anchor
	if _, err := ix.UpsertMemory(memRecord("zeta"), []MemorySource{
		{Vendor: "claude", SessionID: "s", Anchor: "x", AnchorKind: "none"},
	}, nil, memHuman()); err == nil {
		t.Fatal("anchor_kind none with an anchor must be refused")
	}
	// unknown kind
	if _, err := ix.UpsertMemory(memRecord("eta"), []MemorySource{
		{Vendor: "claude", SessionID: "s", Anchor: "x", AnchorKind: "timestamp"},
	}, nil, memHuman()); err == nil {
		t.Fatal("unknown anchor kind must be refused")
	}
}

func TestMemoryLifecyclePromoteRejectDelete(t *testing.T) {
	ix := openTestMemoryStore(t)
	pending := memRecord("note-1")
	pending.Status = "pending"
	if _, err := ix.UpsertMemory(pending, nil, nil, memHuman()); err != nil {
		t.Fatalf("pending insert: %v", err)
	}
	if _, err := ix.PromoteMemory("note-1", memHuman()); err != nil {
		t.Fatalf("promote: %v", err)
	}
	got, err := ix.MemoryByID("note-1")
	if err != nil || got.Status != "active" {
		t.Fatalf("promoted record must be active: %v %s", err, got.Status)
	}
	if _, err := ix.RejectMemory("note-1", "not wanted", memHuman()); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, err = ix.MemoryByID("note-1")
	if err != nil || got.Status != "rejected" || got.RejectReason != "not wanted" {
		t.Fatalf("rejected record must keep reason as a field: %+v", got)
	}

	if _, err := ix.UpsertMemory(memRecord("gone"), nil, nil, memHuman()); err != nil {
		t.Fatalf("insert gone: %v", err)
	}
	if err := ix.DeleteMemory("gone", memHuman()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !ix.MemoryTombstoned("gone") {
		t.Fatal("delete must tombstone")
	}
	if _, err := ix.MemoryByID("gone"); err == nil {
		t.Fatal("deleted record must be gone")
	}
	// revisions retained (history), record row gone
	var revs int
	if err := ix.db.QueryRow(`SELECT count(*) FROM memory_revision WHERE record_id='gone'`).Scan(&revs); err != nil || revs != 1 {
		t.Fatalf("revision history must survive deletion: %d %v", revs, err)
	}
}

func TestMemoryEntityAndFTSMaintainedThroughWriteOwner(t *testing.T) {
	ix := openTestMemoryStore(t)
	// A content-keyword detector proves classification runs through the owner
	// in the SAME transaction (kind "content", roles any — a memory is not a
	// tool call, per memory_index.go's role model).
	dets := []engine.Detector{{
		ID: "test.mentions", Kind: "content", Keywords: []string{"alpha"},
		Tag: struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}{Key: "test", Value: "mentions-alpha"},
	}}
	classified := memRecord("classified")
	classified.Body = "body mentioning alpha for the keyword detector"
	r, err := ix.UpsertMemory(classified, nil, dets, memHuman())
	if err != nil {
		t.Fatalf("insert with detectors: %v", err)
	}
	_ = r
	id := engine.EntityID("memory", "classified")
	ent, err := ix.LookupEntity(id)
	if err != nil || ent == nil {
		t.Fatalf("memory entity must exist after write: %v", err)
	}
	var labelled int
	if err := ix.db.QueryRow(`SELECT count(*) FROM entity_state WHERE entity_id=? AND value='mentions-alpha'`, id).Scan(&labelled); err != nil || labelled == 0 {
		t.Fatalf("classification label missing: %v", err)
	}
	// FTS row present — one search_document row keyed by the PREFIXED entity id
	// (Phase 6's established FTS key, engine.EntityID("memory", <id>)).
	var docs int
	if err := ix.db.QueryRow(`SELECT count(*) FROM search_document WHERE owner_kind='memory' AND session_id=?`,
		engine.EntityID("memory", "classified")).Scan(&docs); err != nil || docs != 1 {
		t.Fatalf("memory search document missing: %d %v", docs, err)
	}
}

func TestMemoryOutboxJoinsOnlyShareableScopes(t *testing.T) {
	ix := openTestMemoryStore(t)
	// unlinked device: no enqueue at all
	userRec := memRecord("user-scoped")
	if _, err := ix.UpsertMemory(userRec, nil, nil, memHuman()); err != nil {
		t.Fatalf("user write: %v", err)
	}
	var pending int
	if err := ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE acked_at IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("unlinked store must not enqueue: %d %v", pending, err)
	}
	// link the device; a repository-scoped record enqueues; user does not
	if err := ix.SetLinked(true); err != nil {
		t.Fatalf("link device: %v", err)
	}
	shared := memRecord("repo-scoped")
	shared.ScopeType, shared.ScopeID, shared.RepositoryIdentity = MemoryScopeRepository, "my-repo", "remote-sha256"
	if _, err := ix.UpsertMemory(shared, nil, nil, memHuman()); err != nil {
		t.Fatalf("shared write: %v", err)
	}
	// A weak (folder-name) repository record never travels (team item 5, O-4 / decision 14).
	weak := memRecord("repo-weak")
	weak.ScopeType, weak.ScopeID, weak.RepositoryIdentity = MemoryScopeRepository, "my-repo", "weak"
	if _, err := ix.UpsertMemory(weak, nil, nil, memHuman()); err != nil {
		t.Fatalf("weak write: %v", err)
	}
	if _, err := ix.UpsertMemory(memRecord("user-scoped-2"), nil, nil, memHuman()); err != nil {
		t.Fatalf("user write 2: %v", err)
	}
	rows, err := ix.db.Query(`SELECT record_kind,global_id,scope FROM sync_outbox WHERE record_kind='memory'`)
	if err != nil {
		t.Fatalf("outbox read: %v", err)
	}
	defer rows.Close()
	var kinds, scopes []string
	for rows.Next() {
		var kind, gid, scope string
		if err := rows.Scan(&kind, &gid, &scope); err != nil {
			t.Fatal(err)
		}
		kinds, scopes = append(kinds, gid), append(scopes, scope)
	}
	if len(kinds) != 1 {
		t.Fatalf("only the remote-identified repository record must enqueue, got %d", len(kinds))
	}
	if scopes[0] != "repository:my-repo" {
		t.Fatalf("scope must be 'repository:my-repo', got %q", scopes[0])
	}
}

func TestMemoryListByStatus(t *testing.T) {
	ix := openTestMemoryStore(t)
	p := memRecord("p1")
	p.Status = "pending"
	if _, err := ix.UpsertMemory(p, nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.UpsertMemory(memRecord("a1"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	active, err := ix.ListMemory("active")
	if err != nil || len(active) != 1 || active[0].ID != "a1" {
		t.Fatalf("active list: %v %+v", err, active)
	}
	pending, err := ix.ListMemory("pending")
	if err != nil || len(pending) != 1 || pending[0].ID != "p1" {
		t.Fatalf("pending list: %v %+v", err, pending)
	}
}
