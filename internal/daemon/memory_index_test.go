package daemon

import (
	"path/filepath"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/memory"
	"crossing-guard/store"
)

// TestMemoryBecomesAClassifiedSearchableEntity pins Phase 6: a memory .md folds into
// the ONE store as a memory:<id> entity, classified by the SAME detectors (so a
// memory carrying personal data is LABELLED, and a policy could gate on it), and
// indexed into the SAME FTS (so one search covers memory and sessions).
func TestMemoryBecomesAClassifiedSearchableEntity(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}

	// A memory whose body carries an email address — the shared data.email detector
	// classifies it personal, exactly as it would for a tool call touching the same text.
	rec := memory.Record{
		ID: "onboarding-contact", Title: "Onboarding contact",
		Body:    "For access questions reach the admin at alice@example.com before day one.",
		Updated: "2026-07-20T10:00:00Z", Category: "note",
	}
	labelled, err := indexOneMemory(ix, dets, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !labelled {
		t.Fatal("a memory with an email address should carry a classification label")
	}

	id := engine.EntityID("memory", "onboarding-contact")
	ent, err := ix.LookupEntity(id)
	if err != nil || ent == nil {
		t.Fatalf("memory entity not created: %v", err)
	}
	if ent.Kind != "memory" || ent.Identity != "onboarding-contact" {
		t.Fatalf("wrong entity shape: %+v", ent)
	}

	// The classification folded onto the entity state — the "gateable" part (item 17).
	st, err := ix.EntityState(id)
	if err != nil {
		t.Fatal(err)
	}
	var hasClass bool
	for _, s := range st {
		if s.Key == "data-class" {
			hasClass = true
		}
	}
	if !hasClass {
		t.Errorf("memory not labelled with a data-class; state=%+v", st)
	}

	// Searchable through the ONE FTS, tagged as vendor=memory.
	hits, err := ix.SearchEvents("alice", 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, h := range hits {
		if h.Vendor == "memory" && h.SessionID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("memory not found via the store FTS: %+v", hits)
	}
}

// TestReindexMemoryIsIdempotent: re-indexing the same record does not duplicate its
// FTS row or its entity state.
func TestReindexMemoryIsIdempotent(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	rec := memory.Record{ID: "conv", Title: "A convention", Body: "always kebab-case ids",
		Updated: "2026-07-20T10:00:00Z", Category: "convention"}

	for i := 0; i < 3; i++ {
		if _, err := indexOneMemory(ix, dets, rec); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := ix.SearchEvents("kebab", 10)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range hits {
		if h.Vendor == "memory" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("re-index duplicated the FTS row: %d memory hits, want 1", n)
	}
	// Entity state must also be stable across re-index (UpsertEntityState is keyed on
	// (entity,key,value,detector), so a re-run refreshes rather than accumulates).
	st, err := ix.EntityState(engine.EntityID("memory", "conv"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range st {
		k := s.Key + "=" + s.Value + "/" + s.Detector
		if seen[k] {
			t.Fatalf("re-index duplicated entity state row: %s", k)
		}
		seen[k] = true
	}
}
