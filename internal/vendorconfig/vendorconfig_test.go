package vendorconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceBacksUpAtomicallyAndPreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vendor", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	beforeBytes := []byte("before\n")
	if err := os.WriteFile(path, beforeBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := Replace(path, before, []byte("after\n"))
	if err != nil {
		t.Fatal(err)
	}
	if backup != path+".crossing-guard.bak" {
		t.Fatalf("backup = %q", backup)
	}
	if got, _ := os.ReadFile(path); string(got) != "after\n" {
		t.Fatalf("active = %q", got)
	}
	if got, _ := os.ReadFile(backup); !bytes.Equal(got, beforeBytes) {
		t.Fatalf("backup = %q", got)
	}
	activeInfo, _ := os.Stat(path)
	backupInfo, _ := os.Stat(backup)
	if activeInfo.Mode().Perm() != 0o640 || backupInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes active=%o backup=%o", activeInfo.Mode().Perm(), backupInfo.Mode().Perm())
	}
}

func TestReplaceMissingAndPresentEmptyAreDistinct(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	snapshot, err := Read(missing)
	if err != nil || snapshot.Exists {
		t.Fatalf("missing snapshot = %+v err=%v", snapshot, err)
	}
	if _, err := Replace(missing, snapshot, []byte{}); err != nil {
		t.Fatal(err)
	}
	created, err := Read(missing)
	if err != nil || !created.Exists || len(created.Data) != 0 || created.Mode != 0o644 {
		t.Fatalf("created snapshot = %+v err=%v", created, err)
	}
}

func TestReplaceNoOpDoesNotRewriteBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup := path + ".crossing-guard.bak"
	if err := os.WriteFile(backup, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := Read(path)
	gotBackup, err := Replace(path, snapshot, []byte("same"))
	if err != nil || gotBackup != "" {
		t.Fatalf("no-op = backup %q err=%v", gotBackup, err)
	}
	if got, _ := os.ReadFile(backup); string(got) != "keep" {
		t.Fatalf("no-op rewrote backup: %q", got)
	}
}

func TestReplaceRejectsConcurrentChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := Read(path)
	if err := os.WriteFile(path, []byte("vendor-change"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Replace(path, snapshot, []byte("ours")); err == nil {
		t.Fatal("concurrent change must be rejected")
	}
	if got, _ := os.ReadFile(path); string(got) != "vendor-change" {
		t.Fatalf("concurrent bytes lost: %q", got)
	}
	if _, err := os.Stat(path + ".crossing-guard.bak"); !os.IsNotExist(err) {
		t.Fatalf("conflict created a backup: %v", err)
	}
}

func TestReadRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "config")
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link); err == nil {
		t.Fatal("symlink must fail visibly")
	}
}
