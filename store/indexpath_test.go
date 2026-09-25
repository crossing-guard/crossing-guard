package store

import (
	"path/filepath"
	"testing"
)

// TestIndexPathHonorsExplicitDataDir pins D1. The daemon honored --data for its
// token, address file and logs but NOT for the database: the path was recomputed
// from $HOME on every call, so `--data /scratch` ran scratch detectors against the
// REAL store. Two identical copies of the resolver existed (internal/daemon and
// internal/memcli); this is the one that replaced both.
func TestIndexPathHonorsExplicitDataDir(t *testing.T) {
	home := "/home/u"

	t.Run("explicit data dir wins over env", func(t *testing.T) {
		t.Setenv("CG_INDEX", "/env/index.sqlite")
		got := IndexPath("/scratch", home)
		if want := filepath.Join("/scratch", "index.sqlite"); got != want {
			t.Fatalf("explicit --data ignored: got %q want %q", got, want)
		}
	})

	// The bug's mirror image: passing a DEFAULTED --data would silently disable the
	// env override. Callers must pass "" when the operator did not set the flag.
	t.Run("env wins when no explicit data dir", func(t *testing.T) {
		t.Setenv("CG_INDEX", "/env/index.sqlite")
		if got := IndexPath("", home); got != "/env/index.sqlite" {
			t.Fatalf("env override lost: got %q", got)
		}
	})

	t.Run("legacy env still honored", func(t *testing.T) {
		t.Setenv("CPMEM_INDEX", "/legacy/index.sqlite")
		if got := IndexPath("", home); got != "/legacy/index.sqlite" {
			t.Fatalf("legacy env lost: got %q", got)
		}
	})

	t.Run("product default", func(t *testing.T) {
		got := IndexPath("", home)
		if want := filepath.Join(home, ".crossing-guard", "index.sqlite"); got != want {
			t.Fatalf("default wrong: got %q want %q", got, want)
		}
	})
}
