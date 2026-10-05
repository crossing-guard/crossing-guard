package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/transcriptindex"
	"crossing-guard/store"
)

func withDaemonSearchIndex(t *testing.T) *store.Index {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	prior := resolvedIndexPath
	resolvedIndexPath = path
	t.Cleanup(func() {
		resolvedIndexPath = prior
		_ = ix.Close()
	})
	return ix
}

func TestIndexedSearchPreservesIdentityKindMetadataAndCanonicalDedupe(t *testing.T) {
	ix := withDaemonSearchIndex(t)
	indexedAt := time.Unix(100, 0).UTC()
	_, err := ix.ReplaceTranscriptProjection(store.TranscriptProjection{
		Session: store.SessionRow{Vendor: "codex", ID: "native-thread",
			CatalogID: "rollout-newest", ResumeID: "native-thread",
			Title: "Sprint retro systems", Project: "repository"},
		Generation: "g1", IndexedAt: indexedAt, SourceCount: 2,
		Documents: []store.SearchDocument{
			{Order: 0, Kind: "title", Text: "Sprint retro systems"},
			{Order: 1, Kind: "user", Text: "retro question"},
			{Order: 2, Kind: "assistant", Text: "retro answer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IndexMemoryText("memory-retro", 1, "sprint retro memory"); err != nil {
		t.Fatal(err)
	}
	coverage := transcriptindex.Coverage{State: transcriptindex.CoverageCurrent,
		CoverageAsOf: indexedAt}

	title := SearchAll("systems", coverage)
	if title.Coverage.State != transcriptindex.CoverageCurrent || len(title.Hits) != 1 ||
		title.Hits[0].Kind != "title" || title.Hits[0].ID != "rollout-newest" ||
		title.Hits[0].ResumeID != "native-thread" ||
		title.Hits[0].Title != "Sprint retro systems" ||
		title.Hits[0].Project != "repository" {
		t.Fatalf("title search=%+v", title)
	}
	transcript := SearchAll("retro", coverage)
	if len(transcript.Hits) != 1 || transcript.Hits[0].ID != "rollout-newest" ||
		transcript.Hits[0].Kind == "memory" {
		t.Fatalf("canonical dedupe=%+v", transcript)
	}
}

func TestIndexedSearchQualifiesZeroAndUnavailableWithoutRawFallback(t *testing.T) {
	withDaemonSearchIndex(t)
	catchingUp := transcriptindex.Coverage{State: transcriptindex.CoverageCatchingUp,
		CoverageAsOf: time.Unix(100, 0).UTC()}
	result := SearchAll("absent phrase", catchingUp)
	if result.Coverage.State != transcriptindex.CoverageCatchingUp || len(result.Hits) != 0 ||
		result.Source != "fts5 [index]" {
		t.Fatalf("incomplete zero=%+v", result)
	}

	resolvedIndexPath = filepath.Join(t.TempDir(), "missing.sqlite")
	unavailable := SearchAll("raw-only phrase", transcriptindex.Coverage{
		State: transcriptindex.CoverageCurrent, CoverageAsOf: time.Unix(100, 0).UTC(),
	})
	if unavailable.Coverage.State != transcriptindex.CoverageUnavailable ||
		len(unavailable.Hits) != 0 || unavailable.Source != "fts5 [index]" {
		t.Fatalf("unavailable response=%+v", unavailable)
	}
}

// A session store that cannot be read is named as such, never reported as
// "session not found" and never answered from the no-transcript fallback
// (managed turn start identity plan D2). A readable store without the session
// still says not found.
func TestLoadSessionNamesAnUnreadableStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whose permissions forbid it")
	}
	withDaemonSearchIndex(t)
	dir := t.TempDir()
	t.Setenv("OPENCODE_DATA_HOME", dir)
	if _, err := LoadSession("opencode", "ses_absent"); err == nil || !strings.HasPrefix(err.Error(), "session not found: opencode/ses_absent") {
		t.Fatalf("no store file lists as empty, so the session is not found: %v", err)
	}
	database := filepath.Join(dir, "opencode.db")
	if err := os.WriteFile(database, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession("opencode", "ses_absent"); err == nil || !strings.HasPrefix(err.Error(), "session store unreadable: opencode/ses_absent: ") {
		t.Fatalf("an unreadable store must be named: %v", err)
	}
}
