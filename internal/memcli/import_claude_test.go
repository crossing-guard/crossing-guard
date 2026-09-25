package memcli

import (
	"os"
	"path/filepath"
	"testing"
)

// The import writes lowercased repository slugs, so its --project filter must
// accept the directory's own spelling (memory-repository-case-fix-plan.md).
func TestImportClaudeMemoryProjectFilterIgnoresCase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, ".claude", "projects",
		"-Users-fixture-Documents-Sites-Example-Other-Project", "memory")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	topic := "---\ndescription: Example shop supplier returns\ntype: note\n---\n\nSupplier returns take two weeks.\n"
	if err := os.WriteFile(filepath.Join(project, "supplier-returns.md"), []byte(topic), 0o644); err != nil {
		t.Fatal(err)
	}

	matched, err := importClaudeMemory(filepath.Join(home, "matched"), "Example-Other-Project", false)
	if err != nil {
		t.Fatal(err)
	}
	if matched.Projects != 1 || matched.Imported != 1 {
		t.Fatalf("--project Example-Other-Project: %+v, want 1 project and 1 imported", matched)
	}

	other, err := importClaudeMemory(filepath.Join(home, "other"), "example-app", false)
	if err != nil {
		t.Fatal(err)
	}
	if other.Projects != 0 || other.Imported != 0 {
		t.Fatalf("--project example-app: %+v, want nothing imported", other)
	}
}
