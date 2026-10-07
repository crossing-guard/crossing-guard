package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := Record{
		ID: "round-trip", Title: "a title: with colon", Category: "gotcha",
		Repository: "repo", Tags: []string{"one", "two"}, Aliases: []string{"alias"},
		Source: "human", Created: "2026-07-17T00:00:00Z", Updated: "2026-07-17T00:00:00Z",
		Body: "body with\n\n---\nown dashes inside",
	}
	if err := Write(dir, r); err != nil {
		t.Fatal(err)
	}
	got, err := Read(filepath.Join(dir, "round-trip.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != r.Title || got.Category != r.Category || len(got.Tags) != 2 || got.Body != r.Body {
		t.Fatalf("round-trip drift: %+v", got)
	}
}

func TestWriteBlocksFrontmatterInjection(t *testing.T) {
	dir := t.TempDir()
	r := Record{
		ID: "evil", Title: "x\nsuperseded_by: gone", Category: "note",
		Source: "agent", Created: "2026-07-17T00:00:00Z", Updated: "2026-07-17T00:00:00Z",
		Body: "b",
	}
	if err := Write(dir, r); err != nil {
		t.Fatal(err)
	}
	got, err := Read(filepath.Join(dir, "evil.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Superseded != "" {
		t.Fatalf("frontmatter injection succeeded: superseded=%q", got.Superseded)
	}
	if !strings.Contains(got.Title, "superseded_by: gone") {
		t.Fatalf("title lost its (flattened) content: %q", got.Title)
	}
	// the truncation variant: a title carrying a frontmatter terminator
	r2 := Record{ID: "evil2", Title: "x\n---\nrest", Category: "note", Source: "agent",
		Created: "2026-07-17T00:00:00Z", Updated: "2026-07-17T00:00:00Z", Body: "b"}
	if err := Write(dir, r2); err != nil {
		t.Fatal(err)
	}
	got2, err := Read(filepath.Join(dir, "evil2.md"))
	if err != nil || got2.Category != "note" {
		t.Fatalf("terminator injection corrupted the record: %+v %v", got2, err)
	}
}

func TestPromoteRefusesClobber(t *testing.T) {
	dir := t.TempDir()
	curated := Record{ID: "dup", Title: "curated", Category: "note", Source: "human",
		Created: "2026-07-17T00:00:00Z", Updated: "2026-07-17T00:00:00Z", Body: "keep me"}
	if err := Write(dir, curated); err != nil {
		t.Fatal(err)
	}
	proposal := curated
	proposal.Title = "agent proposal"
	if err := Write(filepath.Join(dir, "pending"), proposal); err != nil {
		t.Fatal(err)
	}
	if err := Promote(dir, "dup"); err == nil {
		t.Fatal("promote clobbered a curated record")
	}
	got, _ := Read(filepath.Join(dir, "dup.md"))
	if got.Title != "curated" {
		t.Fatalf("curated record modified: %q", got.Title)
	}
}

func TestDeleteTombstonesBeforeRemove(t *testing.T) {
	dir := t.TempDir()
	r := Record{ID: "gone", Title: "t", Category: "note", Source: "human",
		Created: "2026-07-17T00:00:00Z", Updated: "2026-07-17T00:00:00Z", Body: "b"}
	if err := Write(dir, r); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir, "gone"); err != nil {
		t.Fatal(err)
	}
	if !IsTombstoned(dir, "gone") {
		t.Fatal("no tombstone after delete")
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.md")); err == nil {
		t.Fatal("record still present")
	}
	// tombstone blocks re-promotion
	if err := Write(filepath.Join(dir, "pending"), r); err != nil {
		t.Fatal(err)
	}
	if err := Promote(dir, "gone"); err == nil {
		t.Fatal("tombstoned slug promoted")
	}
}

func TestPendingNeverInIndex(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Record{ID: "live", Title: "live rec", Category: "note", Source: "human",
		Created: "2026-07-17T00:00:00Z", Updated: "2026-07-17T00:00:00Z", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := Write(filepath.Join(dir, "pending"), Record{ID: "proposed", Title: "proposed rec",
		Category: "note", Source: "agent", Created: "2026-07-17T00:00:00Z",
		Updated: "2026-07-17T00:00:00Z", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	idx := BuildIndex(dir, 4096, "someproject")
	if strings.Contains(idx, "proposed") {
		t.Fatal("pending record projected into the injection index")
	}
	if !strings.Contains(idx, "live") {
		t.Fatal("store record missing from index")
	}
}

func TestSameRepositoryScopeIgnoresCaseOnly(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"example-app", "example-app", true},
		{"example-other-project", "Example-Other-Project", true},
		{"example-tools", "EXAMPLE-tools", true},
		{"example-other-project", "example-app", false},
		{"", "Example-Other-Project", false},
		{"users-example-documents-sites", "Sites", false},
		{"", "", true},
		{"example_other_project", "example-other-project", false},
		{"example-app ", "example-app", false},
	}
	for _, c := range cases {
		if got := SameRepositoryScope(c.a, c.b); got != c.want {
			t.Errorf("SameRepositoryScope(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// writeScoped writes one promoted record for the repository-scope tests.
func writeScoped(t *testing.T, dir, id, repository, body string) {
	t.Helper()
	if err := Write(dir, Record{ID: id, Title: id + " title", Category: "note", Source: "human",
		Repository: repository, Created: "2026-09-14T00:00:00Z", Updated: "2026-09-14T00:00:00Z",
		Body: body}); err != nil {
		t.Fatal(err)
	}
}

// The Claude import lowercases the repository while DetectProject keeps the
// directory's case; the index must still scope those records to their repository.
func TestIndexMatchesRepositoryWithoutCase(t *testing.T) {
	dir := t.TempDir()
	writeScoped(t, dir, "lead-times-note", "example-other-project", "supplier lead times")
	writeScoped(t, dir, "feed-note", "example-app", "supplier feed")
	writeScoped(t, dir, "global-note", "", "supplier naming")

	idx := BuildIndex(dir, 4096, "Example-Other-Project")
	if !strings.Contains(idx, "lead-times-note") {
		t.Fatal("record for the mixed-case repository missing from its index")
	}
	if strings.Contains(idx, "feed-note") {
		t.Fatal("record for a different repository leaked into the index")
	}
	if !strings.Contains(idx, "global-note") {
		t.Fatal("empty-repository record is no longer global in the index")
	}

	logged, err := os.ReadFile(filepath.Join(dir, "recall-log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), `"query":"project=Example-Other-Project"`) {
		t.Fatalf("recall log lost the caller's spelling: %s", logged)
	}
}

func TestSearchRepositoryFacetMatchesWithoutCase(t *testing.T) {
	dir := t.TempDir()
	writeScoped(t, dir, "lead-times-note", "example-other-project", "supplier lead times")
	writeScoped(t, dir, "feed-note", "example-app", "supplier feed")
	writeScoped(t, dir, "global-note", "", "supplier naming")

	hits := Search(dir, "supplier", SearchOpts{Repository: "Example-Other-Project"})
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	if len(ids) != 1 || ids[0] != "lead-times-note" {
		t.Fatalf("facet Example-Other-Project returned %v, want only lead-times-note", ids)
	}
}

// A tag-only search (no query terms) returns every record carrying the tag,
// compared without case, newest first (recall-mcp-v1-plan F6).
func TestSearchTagOnlyReturnsTaggedRecordsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []Record{
		{ID: "old-plan", Title: "old", Tags: []string{"Plan"}, Updated: "2026-09-01T00:00:00Z"},
		{ID: "new-plan", Title: "new", Tags: []string{"plan", "x"}, Updated: "2026-09-20T00:00:00Z"},
		{ID: "untagged", Title: "plan in title only", Updated: "2026-09-25T00:00:00Z"},
	} {
		r.Category, r.Source, r.Created, r.Body = "note", "human", r.Updated, "body"
		if err := Write(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	hits := Search(dir, "", SearchOpts{Tag: "PLAN"})
	if len(hits) != 2 || hits[0].ID != "new-plan" || hits[1].ID != "old-plan" {
		t.Fatalf("tag-only search returned %+v", hits)
	}
	if hits := Search(dir, "title", SearchOpts{Tag: "plan"}); len(hits) != 0 {
		t.Fatalf("tag filter must also narrow a query: %+v", hits)
	}
}

func TestProjectFromCommonDirSharesTheLabelAcrossWorktrees(t *testing.T) {
	if got := ProjectFromCommonDir("/src/Example-Other-Project/.git", "/src/Example-Other-Project/.claude/worktrees/x"); got != "Example-Other-Project" {
		t.Fatalf("worktree label %q", got)
	}
	if got := ProjectFromCommonDir("", "/tmp/scratch"); got != "scratch" {
		t.Fatalf("non-git label %q", got)
	}
}
