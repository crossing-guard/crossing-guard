package daemon

import (
	"bytes"
	"os"
	"testing"
)

func TestTranscriptIndexFrontendHasOneCoveragePresenter(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	presenter := read("js/transcript-index-coverage.js")
	sessions := read("js/views/sessions.js")
	plan := read("js/views/session-plan.js")

	for _, required := range [][]byte{
		[]byte("presentTranscriptIndexCoverage"), []byte("zeroAuthoritative"),
		[]byte("coverage_as_of"), []byte("repository-unavailable"),
		[]byte("crossing-guard harvest --rebuild"),
	} {
		if !bytes.Contains(presenter, required) {
			t.Errorf("coverage presenter lost %q", required)
		}
	}
	for name, view := range map[string][]byte{"sessions": sessions, "plan": plan} {
		for _, required := range [][]byte{
			[]byte("transcript-index-coverage.js"), []byte("presentTranscriptIndexCoverage"),
			[]byte("transcript-index-coverage-state"), []byte("transcript-index-coverage-detail"),
			[]byte("transcript-index-coverage-time"), []byte("transcript-index-coverage-recovery"),
		} {
			if !bytes.Contains(view, required) {
				t.Errorf("%s view lost coverage contract %q", name, required)
			}
		}
		for _, forbidden := range [][]byte{
			[]byte("'catching-up'"), []byte("'stale'"), []byte("'incomplete'"),
			[]byte("'repository-busy'"), []byte("'source-too-large'"),
		} {
			if bytes.Contains(view, forbidden) {
				t.Errorf("%s view duplicates coverage vocabulary %q", name, forbidden)
			}
		}
	}
	for _, required := range [][]byte{
		[]byte("search-hit-kind"), []byte("Title match"), []byte("Transcript match"),
		[]byte("createSessionRowShell(s)"), []byte("zeroAuthoritative"),
	} {
		if !bytes.Contains(sessions, required) {
			t.Errorf("search view lost title/identity/zero contract %q", required)
		}
	}
	for _, required := range [][]byte{
		[]byte("response?.coverage"), []byte("transcript-index-zero"),
		[]byte("No attributed statements are indexed as of"), []byte("Promise.allSettled"),
	} {
		if !bytes.Contains(plan, required) {
			t.Errorf("plan view lost statement coverage contract %q", required)
		}
	}
}

func TestTranscriptIndexHTTPResponsesCarryCoverageAndMatchKind(t *testing.T) {
	for name, body := range map[string][]byte{
		"search":     mustReadRepositoryFile(t, "harvest.go"),
		"statements": mustReadRepositoryFile(t, "session_review_facts.go"),
	} {
		for _, required := range [][]byte{[]byte("json:\"coverage\""), []byte("transcriptindex.Coverage")} {
			if !bytes.Contains(body, required) {
				t.Errorf("%s response lost %q", name, required)
			}
		}
	}
	if !bytes.Contains(mustReadRepositoryFile(t, "harvest.go"), []byte("json:\"kind\"")) {
		t.Error("search hits lost match kind")
	}
}

func mustReadRepositoryFile(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
