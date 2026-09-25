package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// The launchd write path is deliberately NOT exercised here: EnsureService shells out
// to `launchctl bootout`/`bootstrap` against a package-constant label, so running it
// under `go test` would deregister the developer's real service. What is testable, and
// what actually decides whether a scratch build takes the machine, is the read: can we
// recover the owning binary from a plist we wrote, and what do we conclude from it.

func writePlist(t *testing.T, dir, exe, dataDir, addr string) string {
	t.Helper()
	path := filepath.Join(dir, "com.crossing-guard.daemon.plist")
	body := launchdPlist(exe, dataDir, addr, filepath.Join(dataDir, "daemon.log"))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The owner has to survive the round trip, or the guard is reading noise.
func TestServiceOwnerRoundTripsThroughThePlist(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "crossing-guard")
	path := writePlist(t, dir, exe, "/some/data", "127.0.0.1:7788")

	data, addr, owner, ok := serviceArgsOwner(path)
	if !ok {
		t.Fatal("a plist this package wrote did not parse")
	}
	if owner != exe {
		t.Fatalf("owner = %q, want %q", owner, exe)
	}
	if data != "/some/data" || addr != "127.0.0.1:7788" {
		t.Fatalf("target = %q %q", data, addr)
	}
}

// A path needing XML escaping must come back as itself; an owner mangled by the
// round trip would read as foreign and stop the real daemon repairing its service.
func TestServiceOwnerSurvivesEscaping(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tools & builds", "crossing-guard")
	path := writePlist(t, dir, exe, "/data", "127.0.0.1:7788")
	if _, _, owner, _ := serviceArgsOwner(path); owner != exe {
		t.Fatalf("owner = %q, want %q", owner, exe)
	}
}

func TestServiceOwnership(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "installed")
	other := filepath.Join(dir, "scratch")
	for _, path := range []string{self, other} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no service installed is free to claim", func(t *testing.T) {
		if got := serviceOwnership(filepath.Join(dir, "absent.plist"), self); got != OwnershipNone {
			t.Fatalf("got %q, want %q", got, OwnershipNone)
		}
	})

	t.Run("my own service is mine to repair", func(t *testing.T) {
		path := writePlist(t, t.TempDir(), self, "/data", "127.0.0.1:7788")
		if got := serviceOwnership(path, self); got != OwnershipSelf {
			t.Fatalf("got %q, want %q", got, OwnershipSelf)
		}
	})

	t.Run("a live other binary keeps its service", func(t *testing.T) {
		path := writePlist(t, t.TempDir(), other, "/data", "127.0.0.1:7799")
		if got := serviceOwnership(path, self); got != OwnershipForeign {
			t.Fatalf("got %q, want %q", got, OwnershipForeign)
		}
	})

	// The incident's shape: a scratch build took the service, then was deleted.
	// Nobody owns it now, so the real daemon reclaims it with no human step.
	t.Run("a deleted owner leaves the service claimable", func(t *testing.T) {
		path := writePlist(t, t.TempDir(), filepath.Join(dir, "already-cleaned"), "/tmp/scratch", "127.0.0.1:7801")
		if got := serviceOwnership(path, self); got != OwnershipDead {
			t.Fatalf("got %q, want %q", got, OwnershipDead)
		}
	})

	// Refusing forever on a plist we cannot read would be a brick with no repair
	// verb; today such a plist self-heals, and it must keep doing so.
	t.Run("an unreadable service is claimable, not foreign", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "garbage.plist")
		if err := os.WriteFile(path, []byte("not a plist at all"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := serviceOwnership(path, self); got != OwnershipNone {
			t.Fatalf("got %q, want %q", got, OwnershipNone)
		}
	})
}
