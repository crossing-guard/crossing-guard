package daemon

// memory_console_write_test.go — the console's write path (plan §5.3): a new
// record is drafted pending through the one store owner; the probe
// memories.json store is gone (RT-8).

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"crossing-guard/store"
)

func TestConsoleUpsertMemoryDraftsPendingThroughOwner(t *testing.T) {
	dataDir := t.TempDir()
	setIndexPath(dataDir)
	t.Cleanup(func() { setIndexPath("") })
	st := NewStore(dataDir)

	saved, err := st.UpsertMemory(Memory{Claim: "A new lesson", Body: "the body"})
	if err != nil {
		t.Fatalf("console write: %v", err)
	}
	if saved.Status != "pending" {
		t.Fatalf("a console-created record must be drafted pending, got %q", saved.Status)
	}

	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	rec, err := ix.MemoryByID(saved.ID)
	if err != nil {
		t.Fatalf("record not in the store: %v", err)
	}
	if rec.Status != "pending" || rec.Source != "human" {
		t.Fatalf("record shape: %+v", rec)
	}
}

func TestConsoleUpsertMemoryEditsExisting(t *testing.T) {
	dataDir := t.TempDir()
	setIndexPath(dataDir)
	t.Cleanup(func() { setIndexPath("") })
	st := NewStore(dataDir)

	// Seed one record through the owner.
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.UpsertMemory(store.MemoryRecord{
		ID: "known-rec", Title: "Known", Category: "how-to", Body: "old body",
		Source: "human", Status: "active", ScopeType: store.MemoryScopeUser,
		AuthorType: "user", AuthorID: "tester",
	}, nil, nil, store.MemoryActor{AuthorType: "user", AuthorID: "tester", ActorSource: "test"}); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	if _, err := st.UpsertMemory(Memory{ID: "known-rec", Claim: "Known", Body: "new body"}); err != nil {
		t.Fatalf("console edit: %v", err)
	}
	ix, err = store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	rec, err := ix.MemoryByID("known-rec")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Body != "new body" {
		t.Fatalf("edit must revise the body, got %q", rec.Body)
	}
	if rec.Revision != 2 {
		t.Fatalf("edit must bump revision, got %d", rec.Revision)
	}
}

func TestConsoleListMemoriesHasNoProbeMerge(t *testing.T) {
	dataDir := t.TempDir()
	setIndexPath(dataDir)
	t.Cleanup(func() { setIndexPath("") })
	st := NewStore(dataDir)

	// A stale probe file from before the retirement must NOT appear.
	if err := writeRawFile(filepath.Join(dataDir, "memories.json"), `[{"id":"probe-1","claim":"stale"}]`); err != nil {
		t.Fatal(err)
	}
	out, err := st.ListMemories()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if m.ID == "probe-1" {
			t.Fatal("probe-store records must not surface after retirement")
		}
	}
}

func writeRawFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// consoleTestStore points the daemon at a fresh data dir (memory-create-identity
// plan §6) and returns the console store over it.
func consoleTestStore(t *testing.T) *Store {
	t.Helper()
	dataDir := t.TempDir()
	setIndexPath(dataDir)
	t.Cleanup(func() { setIndexPath("") })
	return NewStore(dataDir)
}

func readMemory(t *testing.T, id string) store.MemoryRecord {
	t.Helper()
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	rec, err := ix.MemoryByID(id)
	if err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	return rec
}

func countMemories(t *testing.T) int {
	t.Helper()
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	all, err := ix.ListMemory("")
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// The defect: two creates in one second minted one id and the second edited
// the first. Back-to-back creates must be two records at revision 1.
func TestConsoleCreatesBackToBackAreTwoRecords(t *testing.T) {
	st := consoleTestStore(t)
	a, err := st.UpsertMemory(Memory{Claim: "A", Body: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.UpsertMemory(Memory{Claim: "B", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("two creates got one id %q", a.ID)
	}
	for id, title := range map[string]string{a.ID: "A", b.ID: "B"} {
		rec := readMemory(t, id)
		if rec.Title != title || rec.Revision != 1 {
			t.Fatalf("record %s: title %q revision %d", id, rec.Title, rec.Revision)
		}
	}
}

// Even if the mint collides, a create refuses rather than editing.
func TestConsoleCreateCollisionRefusesInsteadOfEditing(t *testing.T) {
	st := consoleTestStore(t)
	first, err := st.UpsertMemory(Memory{Claim: "A", Body: "a"})
	if err != nil {
		t.Fatal(err)
	}
	orig := newConsoleMemoryID
	newConsoleMemoryID = func() string { return first.ID }
	t.Cleanup(func() { newConsoleMemoryID = orig })

	if _, err := st.UpsertMemory(Memory{Claim: "B", Body: "b"}); !errors.Is(err, store.ErrMemoryExists) {
		t.Fatalf("a colliding create must refuse with ErrMemoryExists, got %v", err)
	}
	rec := readMemory(t, first.ID)
	if rec.Title != "A" || rec.Body != "a" || rec.Revision != 1 {
		t.Fatalf("the existing record changed: %+v", rec)
	}
}

func TestConsoleCreateCarriesTagsAndAliases(t *testing.T) {
	st := consoleTestStore(t)
	saved, err := st.UpsertMemory(Memory{Claim: "Tagged", Body: "b", Tags: []string{"orders"}, Aliases: []string{"ords"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := readMemory(t, saved.ID)
	if !reflect.DeepEqual(rec.Tags, []string{"orders"}) || !reflect.DeepEqual(rec.Aliases, []string{"ords"}) {
		t.Fatalf("labels not stored: tags %v aliases %v", rec.Tags, rec.Aliases)
	}
	if !reflect.DeepEqual(saved.Tags, []string{"orders"}) {
		t.Fatalf("response must return the stored tags, got %v", saved.Tags)
	}
}

func TestConsoleCreateRefusesBadTagsWritingNothing(t *testing.T) {
	st := consoleTestStore(t)
	if _, err := st.UpsertMemory(Memory{Claim: "Bad", Tags: []string{"a,b"}}); !errors.Is(err, store.ErrMemoryInvalid) {
		t.Fatalf("a comma tag must be refused as invalid, got %v", err)
	}
	if n := countMemories(t); n != 0 {
		t.Fatalf("a refused create wrote %d records", n)
	}
}

func seedActiveTagged(t *testing.T) {
	t.Helper()
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, err := ix.UpsertMemory(store.MemoryRecord{
		ID: "kept", Title: "Kept", Category: "how-to", Body: "old body",
		Tags: []string{"orders"}, Aliases: []string{"ords"},
		Source: "import", Status: "active", ScopeType: store.MemoryScopeUser,
		AuthorType: "agent", AuthorID: "session:claude/s1",
	}, nil, nil, store.MemoryActor{AuthorType: "user", AuthorID: "tester", ActorSource: "test"}); err != nil {
		t.Fatal(err)
	}
}

// An edit keeps what the request does not carry: absent/null labels, an
// empty body, and the author of record. It re-drafts pending (ADR 0013).
func TestConsoleEditKeepsFieldsNotCarried(t *testing.T) {
	st := consoleTestStore(t)
	seedActiveTagged(t)

	var m Memory
	if err := json.Unmarshal([]byte(`{"id":"kept","claim":"Kept v2","tags":null}`), &m); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertMemory(m); err != nil {
		t.Fatalf("edit: %v", err)
	}
	rec := readMemory(t, "kept")
	if rec.Title != "Kept v2" || rec.Body != "old body" {
		t.Fatalf("title must change and body stay: %+v", rec)
	}
	if !reflect.DeepEqual(rec.Tags, []string{"orders"}) || !reflect.DeepEqual(rec.Aliases, []string{"ords"}) {
		t.Fatalf("absent/null labels must be kept: %v %v", rec.Tags, rec.Aliases)
	}
	if rec.AuthorType != "agent" || rec.AuthorID != "session:claude/s1" || rec.Source != "import" {
		t.Fatalf("author/source of record must be kept: %+v", rec)
	}
	if rec.Status != "pending" || rec.Revision != 2 {
		t.Fatalf("an edit re-drafts pending at the next revision: %s r%d", rec.Status, rec.Revision)
	}

	if err := json.Unmarshal([]byte(`{"id":"kept","claim":"Kept v3","tags":[],"aliases":["alt"]}`), &m); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertMemory(m); err != nil {
		t.Fatalf("edit: %v", err)
	}
	rec = readMemory(t, "kept")
	if len(rec.Tags) != 0 || !reflect.DeepEqual(rec.Aliases, []string{"alt"}) {
		t.Fatalf("[] clears and an array replaces: %v %v", rec.Tags, rec.Aliases)
	}
}

func TestConsoleEditUnknownIDCreatesNothing(t *testing.T) {
	st := consoleTestStore(t)
	if _, err := st.UpsertMemory(Memory{ID: "no-such", Claim: "x"}); !errors.Is(err, errMemoryNotFound) {
		t.Fatalf("an edit of an unknown id must be not-found, got %v", err)
	}
	if n := countMemories(t); n != 0 {
		t.Fatalf("an unknown-id edit wrote %d records", n)
	}
}

func TestMemoryUpsertRouteStatuses(t *testing.T) {
	st := consoleTestStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/memory", func(w http.ResponseWriter, r *http.Request) { handleMemoryUpsert(w, r, st) })
	call := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/memory", strings.NewReader(body)))
		return rec
	}
	ok := call(`{"claim":"A","tags":["orders"]}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("create: %d %s", ok.Code, ok.Body.String())
	}
	var created Memory
	if err := json.Unmarshal(ok.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"claim":""}`, http.StatusBadRequest},
		{`{"claim":"x","tags":["a","a"]}`, http.StatusBadRequest},
		{`{"id":"no-such","claim":"x"}`, http.StatusNotFound},
		{`not json`, http.StatusBadRequest},
	} {
		if got := call(c.body); got.Code != c.code {
			t.Errorf("%s: want %d, got %d %s", c.body, c.code, got.Code, got.Body.String())
		}
	}
	orig := newConsoleMemoryID
	newConsoleMemoryID = func() string { return created.ID }
	t.Cleanup(func() { newConsoleMemoryID = orig })
	if got := call(`{"claim":"B"}`); got.Code != http.StatusConflict {
		t.Fatalf("a colliding create must be 409, got %d %s", got.Code, got.Body.String())
	}
}

// A record written by a door that does not check labels (import, CLI upsert)
// may carry a duplicate tag; editing its title must not be refused over a
// label the request never sent. A rejected record re-drafted pending loses its
// rejection reason.
func TestConsoleEditValidatesOnlyCarriedLabels(t *testing.T) {
	st := consoleTestStore(t)
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.UpsertMemory(store.MemoryRecord{
		ID: "legacy", Title: "Legacy", Category: "note", Body: "b",
		Tags: []string{"a", "a"}, Source: "import", Status: "rejected", RejectReason: "stale",
		ScopeType: store.MemoryScopeUser, AuthorType: "user", AuthorID: "tester",
	}, nil, nil, store.MemoryActor{AuthorType: "user", AuthorID: "tester", ActorSource: "test"}); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	if _, err := st.UpsertMemory(Memory{ID: "legacy", Claim: "Legacy v2"}); err != nil {
		t.Fatalf("a title edit must not validate kept labels: %v", err)
	}
	rec := readMemory(t, "legacy")
	if rec.Title != "Legacy v2" || !reflect.DeepEqual(rec.Tags, []string{"a", "a"}) {
		t.Fatalf("edit result: %+v", rec)
	}
	if rec.Status != "pending" || rec.RejectReason != "" {
		t.Fatalf("re-drafted pending must clear the rejection reason: %s %q", rec.Status, rec.RejectReason)
	}
	if _, err := st.UpsertMemory(Memory{ID: "legacy", Claim: "x", Tags: []string{"b", "b"}}); !errors.Is(err, store.ErrMemoryInvalid) {
		t.Fatalf("carried labels are still validated, got %v", err)
	}
}
