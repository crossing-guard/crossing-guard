package memcli

import (
	"os"
	"path/filepath"
	"testing"

	"crossing-guard/internal/transcriptindex"
	"crossing-guard/store"
)

func writeCLITranscriptFixture(t *testing.T, title, token string) string {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("OPENCODE_DATA_HOME", filepath.Join(homeDir, "opencode-empty"))
	indexPath := filepath.Join(t.TempDir(), "index.sqlite")
	t.Setenv("CG_INDEX", indexPath)
	id := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dir := filepath.Join(homeDir, ".claude", "projects", "-cli-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","sessionId":"` + id + `","timestamp":"2026-08-28T11:00:00Z","cwd":"/cli/repo","message":{"role":"user","content":"` + token + `"}}` + "\n" +
		`{"type":"assistant","sessionId":"` + id + `","timestamp":"2026-08-28T11:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"fixture answer"}]}}` + "\n" +
		`{"type":"custom-title","customTitle":"` + title + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return indexPath
}

func seedPrimaryAndMemory(t *testing.T, path string) {
	t.Helper()
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(store.EventRecord{TS: 1, SessionID: "primary-session",
		Runtime: "claude", Verb: "read", Tool: "Read", Origin: "live"}, nil); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := ix.IndexMemoryText("memory-fixture", 2, "memory preservation token"); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshTranscriptIndexIncrementalAndForcePreserveSharedStore(t *testing.T) {
	path := writeCLITranscriptFixture(t, "CLI projection title", "cli projection token")
	seedPrimaryAndMemory(t, path)

	first, err := refreshTranscriptIndex(transcriptindex.RefreshModeIncremental)
	if err != nil || first.Stats.Discovered != 1 || first.Stats.Updated != 1 ||
		first.Coverage.State != transcriptindex.CoverageCurrent {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := refreshTranscriptIndex(transcriptindex.RefreshModeIncremental)
	if err != nil || second.Stats.Updated != 0 || second.Stats.Skipped != 1 ||
		second.Coverage.State != transcriptindex.CoverageCurrent {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	forced, err := refreshTranscriptIndex(transcriptindex.RefreshModeForce)
	if err != nil || forced.Stats.Updated != 1 || forced.Stats.Skipped != 0 ||
		forced.Coverage.State != transcriptindex.CoverageCurrent {
		t.Fatalf("forced=%+v err=%v", forced, err)
	}

	ix, err := store.OpenRO(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	events, err := ix.EventsForSession("primary-session", 10)
	if err != nil || len(events) != 1 || events[0].Tool != "Read" {
		t.Fatalf("primary events=%+v err=%v", events, err)
	}
	memoryHits, err := ix.SearchEvents("memory preservation", 10)
	if err != nil || len(memoryHits) != 1 || memoryHits[0].Vendor != "memory" {
		t.Fatalf("memory hits=%+v err=%v", memoryHits, err)
	}
	transcriptHits, err := ix.SearchEvents("cli projection", 10)
	if err != nil || len(transcriptHits) == 0 {
		t.Fatalf("transcript hits=%+v err=%v", transcriptHits, err)
	}
}
