package harvest

import (
	"os"
	"path/filepath"
	"testing"
)

// The project is the directory the transcript declares; a dash in a folder
// name is never turned into a path separator.
func TestClaudeProjectIsTheDeclaredDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-Users-me-my-dashed-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	withCwd := filepath.Join(dir, "a.jsonl")
	line := `{"type":"user","cwd":"/Users/me/my-dashed-project","sessionId":"a","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(withCwd, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, _ := claudeRuntime{}.Summarize(fileJob{runtime: "claude", path: withCwd})
	if summary.Project != "/Users/me/my-dashed-project" || summary.Cwd != summary.Project {
		t.Fatalf("project must be the declared cwd: %q (cwd %q)", summary.Project, summary.Cwd)
	}
	noCwd := filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(noCwd, []byte(`{"type":"user","sessionId":"b","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, _ = claudeRuntime{}.Summarize(fileJob{runtime: "claude", path: noCwd})
	if summary.Cwd != "" || summary.Project != "-Users-me-my-dashed-project" {
		t.Fatalf("without a cwd the raw folder name stays a label, never a reconstructed path: %q", summary.Project)
	}
}
