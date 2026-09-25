package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	core "crossing-guard/internal/refindex"
)

// TestDocBacklinksTruncationIsDeterministic locks the fix for a bug the review
// found: capped backlink results must be the stable first-N-by-position.
func TestDocBacklinksTruncationIsDeterministic(t *testing.T) {
	const target = "internal/daemon/thing.go"
	total := backlinkDocCap * 3
	root := t.TempDir()
	paths := []string{target}
	if err := os.MkdirAll(filepath.Join(root, "internal/daemon"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(target)), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0700); err != nil {
		t.Fatal(err)
	}
	for n := total; n > 0; n-- {
		path := fmt.Sprintf("docs/d-%03d.md", n)
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte("see "+target), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	facts, err := core.BuildManifest(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Coverage.Produced != total {
		t.Fatalf("fixture docs were not parsed: %+v", facts.Coverage)
	}
	idx, err := core.Build(root)
	if err != nil {
		t.Fatal(err)
	}
	first, count1 := idx.Backlinks(target, backlinkDocCap)
	second, count2 := idx.Backlinks(target, backlinkDocCap)
	if count1 <= len(first) || count2 <= len(second) {
		t.Fatalf("expected truncation for %d mentions over a cap of %d", total, backlinkDocCap)
	}
	if len(first) != backlinkDocCap {
		t.Fatalf("truncated result must be exactly the cap (%d), got %d", backlinkDocCap, len(first))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("truncated result is not stable at %d: %v vs %v", i, first[i], second[i])
		}
		if i > 0 && first[i-1].Path > first[i].Path {
			t.Errorf("not sorted by path at %d: %q > %q", i, first[i-1].Path, first[i].Path)
		}
	}
}
