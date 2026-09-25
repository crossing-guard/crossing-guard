package refindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildManifestResolvesOnlyExactOrUniquePaths(t *testing.T) {
	root := t.TempDir()
	paths := []string{"src/exact.go", "one/shared.go", "two/shared.go", "docs/work.md"}
	for _, path := range paths[:3] {
		writeFixture(t, root, path, "fixture")
	}
	writeFixture(t, root, "docs/work.md", "| **D7** | change src/exact.go and shared.go |\n")
	facts, err := BuildManifest(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.IDs["D7"]) != 1 {
		t.Fatalf("definition missing: %+v", facts.IDs)
	}
	if len(facts.Mentions["src/exact.go"]) != 1 {
		t.Fatalf("exact path reference missing: %+v", facts.Mentions)
	}
	if facts.Mentions["one/shared.go"] != nil || facts.Mentions["two/shared.go"] != nil {
		t.Fatalf("ambiguous suffix was guessed: %+v", facts.Mentions)
	}
	if facts.Coverage.State != "partial" || facts.Coverage.Ambiguous != 1 || !strings.Contains(facts.Coverage.Reason, "multiple") {
		t.Fatalf("ambiguity was hidden: %+v", facts.Coverage)
	}
}

func TestBuildManifestReportsOversizedDocumentWithoutPartialFacts(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "target.go", "fixture")
	writeFixture(t, root, "docs/large.md", strings.Repeat("x", refDocMaxBytes+1)+" target.go")
	facts, err := BuildManifest(root, []string{"target.go", "docs/large.md"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.Coverage.State != "partial" || facts.Coverage.Errors != 1 || facts.Coverage.Produced != 0 {
		t.Fatalf("oversized doc became empty complete coverage: %+v", facts.Coverage)
	}
	if len(facts.Mentions) != 0 || len(facts.IDs) != 0 {
		t.Fatalf("oversized doc left partial reference facts: mentions=%+v ids=%+v", facts.Mentions, facts.IDs)
	}
}

func TestBuildManifestDoesNotWidenToUnlistedFiles(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "listed.go", "fixture")
	writeFixture(t, root, "unlisted.go", "fixture")
	writeFixture(t, root, "docs/work.md", "listed.go unlisted.go")
	facts, err := BuildManifest(root, []string{"listed.go", "docs/work.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Mentions["listed.go"]) != 1 {
		t.Fatalf("listed reference missing: %+v", facts.Mentions)
	}
	if facts.Mentions["unlisted.go"] != nil {
		t.Fatalf("reference index widened beyond manifest: %+v", facts.Mentions)
	}
}
