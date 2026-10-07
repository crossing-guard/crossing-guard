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
		first.Coverage.State != transcriptindex.CoverageCurrent ||
		len(first.Coverage.Limitations) != 0 {
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

func TestRefreshIndexesAntigravityMeasuredTextAndResume(t *testing.T) {
	indexPath := writeCLITranscriptFixture(t, "Claude fixture", "claude phrase")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	id := "abcdef00-aaaa-4bbb-8ccc-000000000014"
	path := filepath.Join(home, ".gemini", "antigravity-cli", "brain", id,
		".system_generated", "logs", "transcript_full.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>orbit search phrase</USER_REQUEST><ADDITIONAL_METADATA>hidden metadata</ADDITIONAL_METADATA>"}` + "\n" +
		`{"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"gravity response phrase"}` + "\n" +
		`{"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"view_file","args":{"AbsolutePath":"/fixture/native-tool-marker"}}]}` + "\n" +
		`{"source":"MODEL","type":"GENERIC","status":"DONE","content":"secret generic phrase"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := refreshTranscriptIndex(transcriptindex.RefreshModeIncremental)
	if err != nil || first.Coverage.State != transcriptindex.CoverageCurrent ||
		first.Stats.Discovered != 2 || first.Stats.Updated != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	ix, err := store.OpenRO(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"orbit search", "gravity response", "native-tool-marker"} {
		hits, searchErr := ix.SearchEvents(phrase, 10)
		if searchErr != nil || len(hits) == 0 || hits[0].Vendor != "antigravity" || hits[0].SessionID != id {
			t.Fatalf("phrase %q hits=%+v err=%v", phrase, hits, searchErr)
		}
	}
	for _, phrase := range []string{"hidden metadata", "secret generic"} {
		hits, searchErr := ix.SearchEvents(phrase, 10)
		if searchErr != nil || len(hits) != 0 {
			t.Fatalf("non-searchable %q hits=%+v err=%v", phrase, hits, searchErr)
		}
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body+`{"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>resumed orbit phrase</USER_REQUEST>"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := refreshTranscriptIndex(transcriptindex.RefreshModeIncremental)
	if err != nil || second.Coverage.State != transcriptindex.CoverageCurrent || second.Stats.Updated != 1 || second.Stats.Discovered != 2 {
		t.Fatalf("resumed=%+v err=%v", second, err)
	}
}
