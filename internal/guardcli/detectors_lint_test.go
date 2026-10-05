package guardcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDetectorsLintRejectsStateNamespaceKey drives `detectors lint`'s check: a tag key in
// a stateful-tier namespace fails the whole document with an error that names it, and a
// clean document passes.
func TestDetectorsLintRejectsStateNamespaceKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, key string) string {
		path := filepath.Join(dir, name)
		doc := `{"detectors":[{"id":"custom.one","kind":"source","match":{"tool":["Bash"]},` +
			`"tag":{"key":"` + key + `","value":"x"},"coverage":{"enumerable":true}}]}`
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, key := range []string{"session:x", "target:x", "agent:b:x"} {
		_, _, err := lintDetectorFile(write("bad.json", key))
		if err == nil || !strings.Contains(err.Error(), "state namespace") || !strings.Contains(err.Error(), key) {
			t.Fatalf("lint of key %q: err=%v, want a state-namespace error naming it", key, err)
		}
	}
	count, problems, err := lintDetectorFile(write("good.json", "shell.exec"))
	if err != nil || count != 1 || len(problems) != 0 {
		t.Fatalf("clean document: count=%d problems=%v err=%v", count, problems, err)
	}
}
