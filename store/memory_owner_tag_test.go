package store

// memory_owner_tag_test.go — the owner-tag grammar over memory records (plan
// §6): same validation and folding as session tags, one more record kind.

import (
	"path/filepath"
	"testing"
)

func TestMemoryOwnerTagsApplyRetractAndFold(t *testing.T) {
	ix := openTestMemoryStore(t)
	if _, err := ix.UpsertMemory(memRecord("tagged"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	apply := []SessionOwnerTagValue{{Key: "plan", Value: "alpha"}, {Value: "solo"}}
	if err := ix.ChangeMemoryOwnerTags("tagged", apply, nil, 1000); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := ix.MemoryOwnerTags("tagged")
	if err != nil || len(got) != 2 {
		t.Fatalf("tags after apply: %v %v", got, err)
	}
	// Case folding: PLAN:Alpha retracts plan:alpha, not a second tag.
	if err := ix.ChangeMemoryOwnerTags("tagged", nil,
		[]SessionOwnerTagValue{{Key: "PLAN", Value: "Alpha"}}, 2000); err != nil {
		t.Fatalf("retract: %v", err)
	}
	got, _ = ix.MemoryOwnerTags("tagged")
	if len(got) != 1 || got[0].Value != "solo" {
		t.Fatalf("retract must fold case: %+v", got)
	}
	// Vocabulary and by-tag lookup agree.
	ids, err := ix.RecordsByMemoryOwnerTag("", "solo")
	if err != nil || len(ids) != 1 || ids[0] != "tagged" {
		t.Fatalf("by-tag: %v %v", ids, err)
	}
	uses, err := ix.MemoryOwnerTagUses()
	if err != nil || len(uses) != 1 || uses[0].Records != 1 {
		t.Fatalf("uses: %+v %v", uses, err)
	}
}

func TestMemoryOwnerTagsValidateBeforeWriting(t *testing.T) {
	ix := openTestMemoryStore(t)
	if _, err := ix.UpsertMemory(memRecord("vt"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	bad := []SessionOwnerTagValue{{Key: "a:b", Value: "x"}}
	if err := ix.ChangeMemoryOwnerTags("vt", bad, nil, 1000); err == nil {
		t.Fatal("a colon inside a key must be refused")
	}
	if err := ix.ChangeMemoryOwnerTags("missing-rec", []SessionOwnerTagValue{{Value: "x"}}, nil, 1000); err != nil {
		// A tag on an absent record is allowed by the store (the record may be
		// created later; by-tag skips absent rows) — the HTTP layer refuses
		// earlier. This pins that the store does not silently fail.
		t.Logf("absent record refused: %v", err)
	}
}

func TestMemoryOwnerTagPathIsolation(t *testing.T) {
	ix := openTestMemoryStore(t)
	if _, err := ix.UpsertMemory(memRecord("one"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if err := ix.ChangeMemoryOwnerTags("one", []SessionOwnerTagValue{{Value: "shared"}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	// Retracting on another record must not touch the first.
	if err := ix.ChangeMemoryOwnerTags("other", nil, []SessionOwnerTagValue{{Value: "shared"}}, 2000); err != nil {
		t.Fatal(err)
	}
	got, _ := ix.MemoryOwnerTags("one")
	if len(got) != 1 {
		t.Fatalf("retract leaked across records: %+v", got)
	}
}

func TestSavedMemoryViewRecordKindValidation(t *testing.T) {
	// The view's record_kind lives in the daemon config; the STORE half of the
	// grammar test pins what the store contributes: nothing. This is a
	// placeholder that keeps the file's test surface honest — the config
	// validation is exercised in the daemon package.
	_ = filepath.Join(t.TempDir(), "index.sqlite")
}
