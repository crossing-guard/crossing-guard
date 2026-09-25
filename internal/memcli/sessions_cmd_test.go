package memcli

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"crossing-guard/internal/transcriptindex"
)

func captureCLIOutput(t *testing.T, run func()) (string, string) {
	t.Helper()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutWriter, stderrWriter
	defer func() { os.Stdout, os.Stderr = oldStdout, oldStderr }()
	run()
	if err := stdoutWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrWriter.Close(); err != nil {
		t.Fatal(err)
	}
	stdout, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(stderrReader)
	if err != nil {
		t.Fatal(err)
	}
	_ = stdoutReader.Close()
	_ = stderrReader.Close()
	return string(stdout), string(stderr)
}

func TestSessionsSearchJSONCarriesCoverageAndTitleIdentity(t *testing.T) {
	_ = writeCLITranscriptFixture(t, "Searchable planning title", "transcript-only phrase")
	result, err := refreshTranscriptIndex(transcriptindex.RefreshModeIncremental)
	if err != nil || result.Coverage.State != transcriptindex.CoverageCurrent {
		t.Fatalf("refresh=%+v err=%v", result, err)
	}
	stdout, stderr := captureCLIOutput(t, func() {
		sessionsSearch([]string{"Searchable planning", "--json"})
	})
	if stderr != "" {
		t.Fatalf("current search warning=%q", stderr)
	}
	var response indexedSearchResult
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("json=%q err=%v", stdout, err)
	}
	if response.Coverage.State != transcriptindex.CoverageCurrent || len(response.Hits) != 1 ||
		response.Hits[0].Kind != "title" || response.Hits[0].Title != "Searchable planning title" {
		t.Fatalf("response=%+v", response)
	}

	stdout, stderr = captureCLIOutput(t, func() {
		sessionsSearch([]string{"Searchable planning"})
	})
	if stderr != "" || !strings.Contains(stdout, "[title] Searchable planning title") {
		t.Fatalf("human stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestSessionsSearchUnavailableDoesNotScanRawTranscripts(t *testing.T) {
	_ = writeCLITranscriptFixture(t, "Raw fallback must stay hidden", "raw-only-secret-token")
	stdout, stderr := captureCLIOutput(t, func() {
		sessionsSearch([]string{"raw-only-secret-token"})
	})
	if strings.Contains(stdout, "raw-only-secret-token") || stdout != "" {
		t.Fatalf("unavailable indexed search scanned raw source: %q", stdout)
	}
	if !strings.Contains(stderr, "coverage is unavailable") ||
		!strings.Contains(stderr, "crossing-guard harvest") {
		t.Fatalf("recovery warning=%q", stderr)
	}

	stdout, stderr = captureCLIOutput(t, func() {
		sessionsSearch([]string{"raw-only-secret-token", "--json"})
	})
	if stderr != "" || strings.Contains(stdout, "raw-only-secret-token") {
		t.Fatalf("json unavailable stdout=%q stderr=%q", stdout, stderr)
	}
	var response indexedSearchResult
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatal(err)
	}
	if response.Coverage.State != transcriptindex.CoverageUnavailable || len(response.Hits) != 0 {
		t.Fatalf("response=%+v", response)
	}
}
