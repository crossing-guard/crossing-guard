package guardcli

import (
	"os"
	"path/filepath"
	"testing"
)

// Ownership decides whether a starting daemon may rewrite an installation it found.
// Getting it wrong in one direction lets a scratch build take the machine; getting it
// wrong in the other stops the real daemon repairing itself. Both are silent, so the
// rule is tested directly rather than through the writers that shell out to launchctl.
func TestOwnershipOf(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "installed")
	other := filepath.Join(dir, "scratch")
	for _, path := range []string{self, other} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(dir, "deleted")

	for _, testCase := range []struct {
		name  string
		owner string
		want  string
	}{
		{"nothing recorded", "", "none"},
		{"blank is nothing", "   ", "none"},
		{"the same binary", self, "self"},
		{"another binary that exists", other, "foreign"},
		{"another binary that is gone", missing, "dead"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := OwnershipOf(testCase.owner, self); got != testCase.want {
				t.Fatalf("OwnershipOf(%q) = %q, want %q", testCase.owner, got, testCase.want)
			}
		})
	}
}

// A symlinked or otherwise differently-spelled path to the SAME file must read as
// self. On macOS /tmp is a symlink to /private/tmp, so a plain string compare would
// tell the installed daemon its own hooks belong to someone else and stop it
// repairing them — this guard's own failure mode, inverted.
func TestOwnershipSeesThroughSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-binary")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link-to-binary")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := OwnershipOf(link, real); got != "self" {
		t.Fatalf("a symlink to my own binary read as %q, want self", got)
	}
	if got := OwnershipOf(real, link); got != "self" {
		t.Fatalf("my own binary read through a symlinked self as %q, want self", got)
	}
}

// A directory that no longer exists resolves neither side; the recorded owner is
// still gone, so it must be claimable rather than an error that blocks repair.
func TestOwnershipOfVanishedTree(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "installed")
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := OwnershipOf("/tmp/a-scratch-dir-that-was-cleaned/binary", self); got != "dead" {
		t.Fatalf("a binary under a removed directory read as %q, want dead", got)
	}
}
