package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesWholeFileAtModeAndLeavesNoTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "module.json")
	for _, body := range []string{"first\n", "second, longer body\n"} {
		if err := Write(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Fatalf("read back %q, %v; want %q", got, err, body)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".atomic-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	if err := Write(filepath.Join(dir, "missing", "x.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("a write into a missing directory must fail, not create it")
	}
}
