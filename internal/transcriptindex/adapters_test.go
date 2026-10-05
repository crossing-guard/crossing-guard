package transcriptindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/store"

	"github.com/ncruces/go-sqlite3"
)

func writeAdapterClaudeFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCODE_DATA_HOME", filepath.Join(home, "opencode-empty"))
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dir := filepath.Join(home, ".claude", "projects", "-adapter-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","sessionId":"` + id + `","timestamp":"2026-08-28T10:00:00Z","cwd":"/adapter/repo","message":{"role":"user","content":"adapter question"}}` + "\n" +
		`{"type":"assistant","sessionId":"` + id + `","timestamp":"2026-08-28T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"adapter answer"}]}}` + "\n" +
		`{"type":"custom-title","customTitle":"Adapter renamed title"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHarvestCatalogPreservesNativeGenerationAndCanonicalMapping(t *testing.T) {
	id := writeAdapterClaudeFixture(t)
	catalog := NewRegisteredHarvestCatalog()
	discovery, err := catalog.Discover(context.Background(), DefaultLimits())
	if err != nil || !discovery.Complete || len(discovery.Limitations) != 0 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	var session SourceSession
	for _, candidate := range discovery.Sessions {
		if candidate.Key.Runtime == "claude" && candidate.Key.SessionID == id {
			session = candidate
			break
		}
	}
	if session.Key.SessionID == "" || session.CatalogID != id || session.ResumeID != id ||
		session.Title != "Adapter renamed title" ||
		session.CWD != "/adapter/repo" || session.Project != "repo" ||
		len(session.Segments) != 1 || session.catalogToken == nil {
		t.Fatalf("mapped session=%+v token=%T", session, session.catalogToken)
	}
	generation, err := catalog.Generation(context.Background(), session, DefaultLimits())
	if err != nil || generation != session.Generation {
		t.Fatalf("generation=%q want=%q err=%v", generation, session.Generation, err)
	}
	snapshot, err := catalog.Read(context.Background(), session, DefaultLimits())
	if err != nil || snapshot.Generation != session.Generation || len(snapshot.Events) != 2 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if snapshot.Events[0].Kind != "user" || snapshot.Events[0].Text != "adapter question" ||
		snapshot.Events[1].Kind != "assistant" || snapshot.Events[1].Lineage == "" {
		t.Fatalf("events=%+v", snapshot.Events)
	}

	copyWithoutToken := session
	copyWithoutToken.catalogToken = nil
	_, err = catalog.Generation(context.Background(), copyWithoutToken, DefaultLimits())
	limitation, ok := LimitationFromError(err)
	if !ok || limitation.Kind != LimitationInvalidProjection {
		t.Fatalf("missing token limitation=%+v ok=%v err=%v", limitation, ok, err)
	}
}

func TestStoreRepositoryPreservesProjectionAndOptimisticConcurrency(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	repository := NewStoreRepository(ix)
	key := SessionKey{Runtime: "adapter-runtime", SessionID: "canonical-session"}
	firstAt := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	first := ProjectionReplacement{
		Session: SessionMetadata{Key: key, CatalogID: "catalog-session", ResumeID: "resume-session",
			Path: "/opaque", CWD: "/repo", Project: "repo", Title: "First adapter title",
			Modified: firstAt, Turns: 1},
		Generation: "g1", IndexedAt: firstAt, SourceCount: 2, SourceBytes: 200,
		TextBytes: 20, Documents: []SearchDocument{
			{Order: 0, Timestamp: firstAt.Format(time.RFC3339), Kind: "title",
				Text: "First adapter title", Lineage: "title"},
			{Order: 1, Timestamp: firstAt.Format(time.RFC3339), Kind: "user",
				Text: "adapter persistence token", Lineage: "event"},
		},
	}
	created, err := repository.ReplaceTranscriptProjection(context.Background(), first)
	if err != nil || !created.Replaced || created.State.Generation != "g1" ||
		created.State.SourceCount != 2 || created.State.DocumentCount != 2 {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	states, err := repository.ProjectionStates(context.Background())
	if err != nil || states[key].Generation != "g1" || states[key].IndexedAt != firstAt {
		t.Fatalf("states=%+v err=%v", states, err)
	}
	hits, err := ix.SearchEvents("persistence token", 10)
	if err != nil || len(hits) != 1 || hits[0].SessionID != key.SessionID ||
		hits[0].CatalogID != first.Session.CatalogID || hits[0].ResumeID != first.Session.ResumeID ||
		hits[0].Title != first.Session.Title {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}

	secondAt := firstAt.Add(time.Minute)
	second := first
	second.ExpectedGeneration = created.State.Generation
	second.ExpectedIndexedAt = created.State.IndexedAt
	second.Generation = "g2"
	second.IndexedAt = secondAt
	second.Session.Title = "Concurrent title"
	second.Documents[0].Text = "Concurrent title"
	committed, err := repository.ReplaceTranscriptProjection(context.Background(), second)
	if err != nil || !committed.Replaced {
		t.Fatalf("committed=%+v err=%v", committed, err)
	}
	returned, err := repository.RecordTranscriptProjectionLimitation(context.Background(), key,
		created.State, secondAt.Add(time.Minute), Limitation{Kind: LimitationSourceUnreadable})
	if err != nil || returned.Generation != "g2" || returned.Limitation != nil {
		t.Fatalf("stale limitation changed concurrent success: state=%+v err=%v", returned, err)
	}
}

func TestConcreteAdapterErrorsUseClosedLimitations(t *testing.T) {
	for _, test := range []struct {
		err  error
		want LimitationKind
	}{
		{err: sqlite3.BUSY, want: LimitationRepositoryBusy},
		{err: store.ErrTranscriptProjectionChanged, want: LimitationSourceMutated},
		{err: errors.New("private driver detail"), want: LimitationRepositoryUnavailable},
	} {
		mapped := repositoryAdapterError(test.err, SessionKey{Runtime: "runtime", SessionID: "session"})
		limitation, ok := LimitationFromError(mapped)
		if !ok || limitation.Kind != test.want || limitation.Runtime != "runtime" ||
			limitation.SessionID != "session" {
			t.Fatalf("err=%v limitation=%+v ok=%v", test.err, limitation, ok)
		}
	}
}
