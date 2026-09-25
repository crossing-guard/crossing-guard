package guardcli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCanonicalEntityIDResolvesRelativePaths pins a false negative found by running
// the command rather than by testing it: entities are stored under ABSOLUTE paths
// (the hook reports what the runtime gave it), so `crossing-guard entities --id docs/x.md` from
// the repo root answered "never observed" for a file the store knew about. An
// authoritative-sounding wrong answer is worse than an error.
func TestCanonicalEntityIDResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// t.TempDir may sit behind a symlink (/var vs /private/var on darwin); compare
	// against the resolved cwd so the test asserts absoluteness, not a literal path.
	here, _ := os.Getwd()

	got := canonicalEntityID("docs/x.md")
	want := "file:" + filepath.Join(here, "docs/x.md")
	if got != want {
		t.Fatalf("relative path not resolved:\n got %s\nwant %s", got, want)
	}

	// Already-canonical ids and urls must pass through untouched.
	if got := canonicalEntityID("file:/abs/y.md"); got != "file:/abs/y.md" {
		t.Fatalf("canonical id rewritten: %s", got)
	}
	if got := canonicalEntityID("https://example.test/a"); got != "url:example.test" {
		t.Fatalf("url not canonicalized: %s", got)
	}
}

// TestDetectorBasisClassifiesTheShippedLibrary: `crossing-guard entities` explains WHY a fact is
// believed. A detector with no basis prints "unknown detector", which is honest but
// useless if it happens for the shipped set.
func TestDetectorBasisClassifiesTheShippedLibrary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := detectorBasis()
	if len(b) == 0 {
		t.Fatal("no detectors classified — every fact would print as unknown")
	}
	if got := b["area.secrets"]; got != "source-map: declared tool/path rule" {
		t.Fatalf("source-kind detector misclassified: %q", got)
	}
	if got := b["secret.aws-key"]; got != "scan floor: regex over text" {
		t.Fatalf("pattern detector misclassified: %q", got)
	}
}
