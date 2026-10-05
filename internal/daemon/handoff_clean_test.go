package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Criterion 69: `handoff clean` removes an earlier version's block and file and leaves
// a damaged marker pair untouched and reported.
func TestCleanCheckoutHandoffRemovesBlocksAndLeavesDamagedOnes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
		return path
	}
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	file := write(filepath.Join(".crossing-guard", "handoff.md"), "# Handoff\n")
	claude := write("CLAUDE.md", "# Project\n\nKeep this.\n\n"+markerStart+"\n# Handoff — old\n\nold text\n"+markerEnd+"\n\nAnd this.\n")
	damaged := "# Agents\n\n" + markerStart + "\nold text with no end marker\n"
	agents := write("AGENTS.md", damaged)

	found, err := InspectCheckoutHandoff(dir)
	if err != nil || found.File != file || len(found.Blocks) != 1 || len(found.Damaged) != 1 || !found.Any() {
		t.Fatalf("inspect: %+v %v", found, err)
	}
	if left := CheckoutsWithHandoffLeftovers([]string{dir, filepath.Join(dir, "gone")}); len(left) != 1 || left[0].Dir != dir {
		t.Fatalf("doctor names the checkout that still has them: %+v", left)
	}
	removed, err := CleanCheckoutHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if removed.File != file || len(removed.Blocks) != 1 || removed.Blocks[0] != claude || len(removed.Damaged) != 1 || removed.Damaged[0] != agents {
		t.Fatalf("removed: %+v", removed)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("the handoff file is removed")
	}
	if got := read(claude); got != "# Project\n\nKeep this.\n\nAnd this.\n" {
		t.Fatalf("the block is removed and the rest kept: %q", got)
	}
	if info, _ := os.Stat(claude); info.Mode().Perm() != 0o640 {
		t.Fatalf("the file keeps its mode: %v", info.Mode().Perm())
	}
	if got := read(agents); got != damaged {
		t.Fatalf("a damaged marker pair is left untouched: %q", got)
	}
	// A second run finds only the damaged pair; a clean checkout reports nothing.
	again, err := CleanCheckoutHandoff(dir)
	if err != nil || again.File != "" || len(again.Blocks) != 0 || len(again.Damaged) != 1 {
		t.Fatalf("second run: %+v %v", again, err)
	}
	clean := t.TempDir()
	if left, err := CleanCheckoutHandoff(clean); err != nil || left.Any() {
		t.Fatalf("nothing to clean: %+v %v", left, err)
	}
	if _, err := CleanCheckoutHandoff(filepath.Join(clean, "missing")); err == nil {
		t.Fatal("a missing directory is an error, not a silent pass")
	}
	// Every damaged shape is left alone.
	for name, body := range map[string]string{
		"end-only":   "x\n" + markerEnd + "\n",
		"reversed":   markerEnd + "\n" + markerStart + "\n",
		"two-starts": markerStart + "\na\n" + markerStart + "\nb\n" + markerEnd + "\n",
	} {
		if _, _, found, isDamaged := markerBlock(body); found || !isDamaged {
			t.Fatalf("%s must read as damaged", name)
		}
	}
	if _, _, found, isDamaged := markerBlock("no markers here"); found || isDamaged {
		t.Fatal("a file with no markers holds nothing")
	}
}

// Criterion 69: no code path writes a handoff into a checkout. The publish route is
// not registered, and the daemon's handoff sources contain no writer of the old file.
func TestNoHandoffPublishRouteOrCheckoutWriter(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/api/handoff/publish") {
		t.Fatal("the publish route must not be registered")
	}
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, gone := range []string{"upsertDelimitedBlock", "ensureGitignore", "handleHandoffPublish"} {
			if strings.Contains(string(body), gone) {
				t.Fatalf("%s still has %s: the checkout-writing helpers are deleted", name, gone)
			}
		}
	}
}
