package memcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/memory"
)

// Criterion 7 (team item 5 decision 5, R2-H9): `memory migrate` rewrites the mirror but
// never commits it — a record pulled from a team must not enter a git history that a
// later deletion cannot reach, even where an earlier version left a repository there.
func TestMigrateNeverCommitsTheMirror(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "legacy", "--no-gpg-sign")
	if err := memory.Write(dir, memory.Record{ID: "pulled-team-record", Title: "from a teammate", Category: "note", Source: "human", Body: "team body"}); err != nil {
		t.Fatal(err)
	}
	// A legacy-format file the migration rewrites.
	if err := os.WriteFile(filepath.Join(dir, "legacy.md"), []byte("---\nid: legacy\ntitle: Legacy\ncategory: note\nsource: human\ncreated: 2026-01-01\nupdated: 2026-01-01\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateStore(dir); err != nil {
		t.Fatal(err)
	}
	if n := git("rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("migrate committed the mirror: %s commits", n)
	}
	if out := git("log", "--all", "--name-only", "--format="); strings.Contains(out, "pulled-team-record") {
		t.Fatalf("a pulled record is in the mirror's git log: %s", out)
	}
}
