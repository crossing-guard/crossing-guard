package store

// memory_create_test.go — a create never edits (memory-create-identity plan
// §3.1/§6): CreateMemory refuses an existing id inside the write transaction,
// minted ids stay distinct within one second, and a new record's id and
// labels are checked before anything is stored.

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"crossing-guard/memory"
)

func TestCreateMemoryRefusesExistingID(t *testing.T) {
	ix := openTestMemoryStore(t)
	if _, err := ix.CreateMemory(memRecord("taken"), nil, nil, memHuman()); err != nil {
		t.Fatalf("first create: %v", err)
	}
	second := memRecord("taken")
	second.Title, second.Body = "Overwrite attempt", "other body"
	_, err := ix.CreateMemory(second, nil, nil, memHuman())
	if !errors.Is(err, ErrMemoryExists) {
		t.Fatalf("a create over an existing id must refuse with ErrMemoryExists, got %v", err)
	}
	got, err := ix.MemoryByID("taken")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "T taken" || got.Body != "body of taken" || got.Revision != 1 {
		t.Fatalf("the existing record changed: %+v", got)
	}
	if _, _, _, _, err := ix.MemoryRevisionByID("taken", 2); err == nil {
		t.Fatal("a refused create must not append a revision")
	}
}

// Two handles on one file, as two processes would be. The goroutines usually
// run one after the other, so this pins "exactly one wins" rather than proving
// the check sits inside the transaction; TestCreateMemoryRefusesExistingID and
// the transaction's immediate lock carry that.
func TestCreateMemoryConcurrentSameIDOneWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	handles := make([]*Index, 2)
	for i := range handles {
		ix, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ix.Close() })
		handles[i] = ix
	}
	errs := make([]error, len(handles))
	var wg sync.WaitGroup
	for i, ix := range handles {
		wg.Add(1)
		go func(i int, ix *Index) {
			defer wg.Done()
			r := memRecord("race")
			r.Title = "writer " + string(rune('A'+i))
			_, errs[i] = ix.CreateMemory(r, nil, nil, memHuman())
		}(i, ix)
	}
	wg.Wait()
	exists := 0
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ErrMemoryExists):
			exists++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if exists != 1 {
		t.Fatalf("exactly one concurrent create must be refused, got %d (%v)", exists, errs)
	}
	got, err := handles[0].MemoryByID("race")
	if err != nil || got.Revision != 1 {
		t.Fatalf("one record at revision 1 expected: %+v %v", got, err)
	}
}

func TestCreateMemoryChecksIDAndLabels(t *testing.T) {
	ix := openTestMemoryStore(t)
	bad := memRecord("Not_A_Slug")
	if _, err := ix.CreateMemory(bad, nil, nil, memHuman()); !errors.Is(err, ErrMemoryInvalid) {
		t.Fatalf("a non-slug id must be refused as invalid, got %v", err)
	}
	dup := memRecord("dup-tags")
	dup.Tags = []string{"a", "a"}
	if _, err := ix.CreateMemory(dup, nil, nil, memHuman()); !errors.Is(err, ErrMemoryInvalid) {
		t.Fatalf("duplicate tags must be refused as invalid, got %v", err)
	}
	for _, id := range []string{"Not_A_Slug", "dup-tags"} {
		if _, err := ix.MemoryByID(id); err == nil {
			t.Fatalf("refused create %q was stored", id)
		}
	}
}

func TestNewMemoryIDDistinctAndSlugShaped(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewMemoryID("note")
		if !memoryIDPattern.MatchString(id) {
			t.Fatalf("minted id %q is not a slug", id)
		}
		if seen[id] {
			t.Fatalf("minted id %q twice", id)
		}
		seen[id] = true
	}
}

func TestValidateMemoryLabels(t *testing.T) {
	many := make([]string, memoryLabelsMax+1)
	for i := range many {
		many[i] = "t" + strings.Repeat("x", i)
	}
	cases := []struct {
		name          string
		tags, aliases []string
		ok            bool
	}{
		{"none", nil, nil, true},
		{"plain", []string{"orders", "release notes"}, []string{"v2", "version 2"}, true},
		{"unicode", []string{"café", "日本"}, nil, true},
		{"tag at limit", []string{strings.Repeat("é", memoryTagMaxChars)}, nil, true},
		{"empty tag", []string{""}, nil, false},
		{"long tag", []string{strings.Repeat("x", memoryTagMaxChars+1)}, nil, false},
		{"long alias", nil, []string{strings.Repeat("x", memoryAliasMaxChars+1)}, false},
		{"duplicate", []string{"a", "a"}, nil, false},
		{"comma", []string{"a,b"}, nil, false},
		{"newline", nil, []string{"a\nb"}, false},
		{"leading space", []string{" a"}, nil, false},
		{"too many", many, nil, false},
	}
	for _, c := range cases {
		err := ValidateMemoryLabels(c.tags, c.aliases)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrMemoryInvalid) {
			t.Errorf("%s: want ErrMemoryInvalid, got %v", c.name, err)
		}
	}
}

func TestReviseMemoryRefusesMissingID(t *testing.T) {
	ix := openTestMemoryStore(t)
	if _, err := ix.ReviseMemory(memRecord("gone"), nil, nil, memHuman()); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("a revision of a missing id must refuse with ErrMemoryNotFound, got %v", err)
	}
	if _, err := ix.MemoryByID("gone"); err == nil {
		t.Fatal("a refused revision created the record")
	}
	if _, err := ix.CreateMemory(memRecord("here"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	r := memRecord("here")
	r.Title = "revised"
	got, err := ix.ReviseMemory(r, nil, nil, memHuman())
	if err != nil || got.Revision != 2 {
		t.Fatalf("revision of an existing record: %+v %v", got, err)
	}
}

// The create's slug rule must be the mirror's, or a stored record could be
// one the mirror refuses to write.
func TestMemoryIDPatternMatchesMirrorRule(t *testing.T) {
	for _, id := range []string{"a", "note-20260928-160823-92f2fd2204e6", "Upper", "-lead", "a_b", "a.b", strings.Repeat("a", 100), strings.Repeat("a", 101), ""} {
		mirrorErr := memory.Validate(memory.Record{ID: id, Title: "t", Category: "note"})
		if memoryIDPattern.MatchString(id) != (mirrorErr == nil) {
			t.Errorf("%q: store pattern %v, mirror rule %v", id, memoryIDPattern.MatchString(id), mirrorErr)
		}
	}
}
