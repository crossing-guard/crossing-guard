package transcriptindex

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func productionGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "analyzers":
				return filepath.SkipDir
			}
			// A nested checkout is a DIFFERENT tree, not this one's production
			// code. Walking the repository's own worktrees made this gate fail
			// for anyone using them — seven false failures here — which is how a
			// gate stops being a signal. Detect one by its own git pointer
			// (a worktree's .git is a FILE, so name it rather than the
			// convention-of-the-week directory it happens to live under).
			if path != root {
				if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestTranscriptIndexArchitectureBoundaries(t *testing.T) {
	root := repositoryRoot(t)
	files := productionGoFiles(t, root)
	// A walker that skips nested checkouts could skip everything and pass
	// silently. Prove it still reaches this tree's own code, including the one
	// file the events_fts rule exempts by name.
	seen := map[string]bool{}
	for _, path := range files {
		if rel, err := filepath.Rel(root, path); err == nil {
			seen[rel] = true
		}
	}
	for _, required := range []string{
		filepath.Join("store", "search_index.go"),
		filepath.Join("internal", "transcriptindex", "contracts.go"),
	} {
		if !seen[required] {
			t.Fatalf("the production file walk no longer reaches %s — this gate is not looking at anything", required)
		}
	}
	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if rel != filepath.Join("store", "search_index.go") &&
			(strings.Contains(text, "INSERT INTO events_fts") ||
				strings.Contains(text, "DELETE FROM events_fts") ||
				strings.Contains(text, "UPDATE events_fts")) {
			t.Errorf("%s directly mutates events_fts outside its relational owner", rel)
		}
		if strings.HasPrefix(rel, "harvest"+string(filepath.Separator)) ||
			strings.HasPrefix(rel, "store"+string(filepath.Separator)) {
			if strings.Contains(text, "crossing-guard/internal/transcriptindex") {
				t.Errorf("lower package %s imports application policy", rel)
			}
		}
	}

	for _, rel := range []string{
		filepath.Join("internal", "transcriptindex", "indexer.go"),
		filepath.Join("internal", "transcriptindex", "adapters.go"),
	} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"Governor", "lifecycle", "internal/daemon",
			"github.com/ncruces/go-sqlite3"} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("%s contains forbidden dependency marker %q", rel, forbidden)
			}
		}
	}

	adapterPath := filepath.Join(root, "internal", "transcriptindex", "adapters.go")
	adapterBody, err := os.ReadFile(adapterPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), adapterPath, adapterBody, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		for _, runtimeName := range []string{"claude", "codex", "opencode"} {
			if literal.Value == "\""+runtimeName+"\"" {
				t.Errorf("generic adapter branches on runtime name %q", runtimeName)
			}
		}
		return true
	})

	memcliIndex := readArchitectureFile(t, root, "internal", "memcli", "index.go")
	for _, forbidden := range []string{"os.Remove(indexDB())", "harvestIndex(", "sessionFiles(",
		"summarizeFile(", "sessionEventRows("} {
		if strings.Contains(memcliIndex, forbidden) {
			t.Errorf("legacy CLI index path retained %q", forbidden)
		}
	}
	storeIndex := readArchitectureFile(t, root, "store", "index.go")
	for _, forbidden := range []string{"func (ix *Index) Watermarks(",
		"func (ix *Index) SessionSourceWatermarks(", "func (ix *Index) BeginHarvest(",
		"type Harvest struct"} {
		if strings.Contains(storeIndex, forbidden) {
			t.Errorf("legacy store harvest owner retained %q", forbidden)
		}
	}
}

func readArchitectureFile(t *testing.T, root string, parts ...string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
