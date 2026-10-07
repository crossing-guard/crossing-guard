package guardcli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeFakeBinary(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// The search takes its directories and fallbacks as arguments, so a real install on
// the developer's machine cannot change these results.
func TestResolveBinaryInSearchOrder(t *testing.T) {
	root := t.TempDir()
	emptyDir, binDir := filepath.Join(root, "empty"), filepath.Join(root, "bin")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	newer := writeFakeBinary(t, filepath.Join(root, "app", "new", "tool"), 0o755)
	older := writeFakeBinary(t, filepath.Join(root, "app", "old", "tool"), 0o755)

	got, err := resolveBinaryIn("tool", []string{emptyDir}, []string{newer, older})
	if err != nil || got != newer {
		t.Fatalf("first fallback should win: %q %v", got, err)
	}
	got, err = resolveBinaryIn("tool", []string{emptyDir}, []string{filepath.Join(root, "gone"), older})
	if err != nil || got != older {
		t.Fatalf("a missing fallback must fall through to the next: %q %v", got, err)
	}

	// Not executable, and a directory of the right name: neither is a binary.
	notExec := writeFakeBinary(t, filepath.Join(root, "noexec", "tool"), 0o644)
	if err := os.MkdirAll(filepath.Join(root, "isdir", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = resolveBinaryIn("tool", []string{filepath.Dir(notExec), filepath.Join(root, "isdir")}, []string{older})
	if err != nil || got != older {
		t.Fatalf("non-executable and directory candidates must be skipped: %q %v", got, err)
	}

	inDir := writeFakeBinary(t, filepath.Join(binDir, "tool"), 0o755)
	got, err = resolveBinaryIn("tool", []string{emptyDir, binDir}, []string{newer, older})
	if err != nil || got != inDir {
		t.Fatalf("a searched directory must win over every fallback: %q %v", got, err)
	}
}

func TestResolveBinaryNotFoundNamesEverythingSearched(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	fallback := filepath.Join(root, "app", "tool")
	_, err := resolveBinaryIn("tool", []string{dirA, dirB}, []string{fallback})
	if err == nil {
		t.Fatal("nothing exists, yet the search succeeded")
	}
	text := err.Error()
	for _, want := range []string{dirA, dirB, fallback, "place or link the executable", "chat turns only"} {
		if !strings.Contains(text, want) {
			t.Errorf("error does not mention %q:\n%s", want, text)
		}
	}
	// The console has no "Advanced drawer"; the old text sent readers to one.
	if strings.Contains(text, "Advanced drawer") {
		t.Errorf("error names a control that does not exist:\n%s", text)
	}
	if _, err := resolveBinaryIn("tool", []string{dirA}, nil); err == nil || strings.Contains(err.Error(), "also tried") {
		t.Errorf("no fallbacks were given, so none may be reported: %v", err)
	}
	// Generic text: the vendor comes only from the name and fallbacks passed in. The
	// temp root is removed first, because it may itself contain a vendor word.
	if regexp.MustCompile(`(?i)claude|codex|opencode|openai|chatgpt`).MatchString(strings.ReplaceAll(text, root, "")) {
		t.Errorf("generic resolver error carries a vendor name:\n%s", text)
	}
}
