package memory

// mirror_test.go — the write-through mirror's contract (plan §3.4/RT-7): writes
// record hashes, hand edits are detected and adoptable, deletes remove both.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mirrorTestRecord(id string) Record {
	now := time.Now().UTC().Format(time.RFC3339)
	return Record{
		ID: id, Title: "T " + id, Category: "note",
		Source: "human", Created: now, Updated: now, Body: "body " + id,
	}
}

func TestMirrorWriteRecordsHashAndDetectsHandEdits(t *testing.T) {
	dir := t.TempDir()
	r := mirrorTestRecord("alpha")
	if err := MirrorWrite(dir, r); err != nil {
		t.Fatalf("mirror write: %v", err)
	}
	if edits := MirrorHandEdits(dir); len(edits) != 0 {
		t.Fatalf("fresh write must not be a hand edit, got %v", edits)
	}

	// A hand edit: change the file on disk WITHOUT the write owner.
	path := filepath.Join(dir, "alpha.md")
	if err := os.WriteFile(path, []byte("---\nid: alpha\n...hand edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edits := MirrorHandEdits(dir)
	if len(edits) != 1 || edits[0] != "alpha" {
		t.Fatalf("hand edit must be detected: %v", edits)
	}

	// Adoption: import-file path re-writes through the store; the ledger
	// re-syncs because the write owner calls MirrorWrite again.
	if err := MirrorWrite(dir, mirrorTestRecord("alpha")); err != nil {
		t.Fatalf("re-mirror after adoption: %v", err)
	}
	if edits := MirrorHandEdits(dir); len(edits) != 0 {
		t.Fatalf("post-adoption mirror must be clean, got %v", edits)
	}
}

func TestMirrorDeleteRemovesFileAndHash(t *testing.T) {
	dir := t.TempDir()
	if err := MirrorWrite(dir, mirrorTestRecord("beta")); err != nil {
		t.Fatal(err)
	}
	if err := MirrorDelete(dir, "beta"); err != nil {
		t.Fatalf("mirror delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "beta.md")); !os.IsNotExist(err) {
		t.Fatal("mirror file must be gone")
	}
	if edits := MirrorHandEdits(dir); len(edits) != 0 {
		t.Fatalf("deleted record must leave no ledger entry, got %v", edits)
	}
}

func TestMirrorHandEditsToleratesMissingLedgerAndFiles(t *testing.T) {
	dir := t.TempDir() // no ledger, no files
	if edits := MirrorHandEdits(dir); len(edits) != 0 {
		t.Fatalf("empty mirror must report nothing, got %v", edits)
	}
}

func TestMigrateFilesToStoreReadsAllStates(t *testing.T) {
	dir := t.TempDir()
	// active record
	if err := Write(dir, mirrorTestRecord("keep")); err != nil {
		t.Fatal(err)
	}
	// pending record
	if err := os.MkdirAll(filepath.Join(dir, "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := mirrorTestRecord("prop")
	p.Category = "note"
	if err := Write(filepath.Join(dir, "pending"), p); err != nil {
		t.Fatal(err)
	}
	// rejected record
	if err := os.MkdirAll(filepath.Join(dir, "rejected"), 0o755); err != nil {
		t.Fatal(err)
	}
	rj := mirrorTestRecord("nope")
	if err := Write(filepath.Join(dir, "rejected"), rj); err != nil {
		t.Fatal(err)
	}
	// tombstone
	if err := os.WriteFile(filepath.Join(dir, "tombstones"), []byte("dead\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	recs, tombstones, st, err := MigrateFilesToStore(dir)
	if err != nil {
		t.Fatalf("migrate read: %v", err)
	}
	if st.Store != 1 || st.Pending != 1 || st.Rejected != 1 || st.Tombstones != 1 {
		t.Fatalf("stats mismatch: %+v", st)
	}
	if len(tombstones) != 1 || tombstones[0] != "dead" {
		t.Fatalf("tombstones mismatch: %v", tombstones)
	}
	byID := map[string]StoreRecord{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	if byID["keep"].Status != "active" {
		t.Fatalf("store record status: %+v", byID["keep"])
	}
	if byID["prop"].Status != "pending" {
		t.Fatalf("pending record status: %+v", byID["prop"])
	}
	if byID["nope"].Status != "rejected" {
		t.Fatalf("rejected record status: %+v", byID["nope"])
	}
	// RT-3: every migrated record carries the minting note.
	if byID["keep"].Origin == "" {
		t.Fatal("migrated record must record the minting in origin")
	}
	// scope mapping: no repository on these records → user
	if byID["keep"].ScopeType != "user" {
		t.Fatalf("no-repository record must be user-scoped, got %q", byID["keep"].ScopeType)
	}
}

func TestMigrateFilesRepositoryScopeMapping(t *testing.T) {
	dir := t.TempDir()
	r := mirrorTestRecord("repo-rec")
	r.Repository = "My-Repo"
	if err := Write(dir, r); err != nil {
		t.Fatal(err)
	}
	recs, _, _, err := MigrateFilesToStore(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("migrate: %v %d", err, len(recs))
	}
	if recs[0].ScopeType != "repository" || recs[0].ScopeID != "My-Repo" || recs[0].RepositoryIdentity != "weak" {
		t.Fatalf("scope mapping mismatch: %+v", recs[0])
	}
}
