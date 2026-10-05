package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// freshStoreSeed is the bytes of one migrated, empty store, or nil. It is nil in every
// production binary: only SeedFreshStores sets it, and it refuses outside a test binary.
var freshStoreSeed atomic.Pointer[[]byte]

// SeedFreshStores is for TEST BINARIES whose tests open many new stores. It runs the
// whole open once — schema, every migration, the version stamp — on a store of its own,
// keeps that store's bytes, and from then on an Open of a path that does not exist yet
// starts from a copy of them. stop restores the ordinary behaviour.
//
// Why it exists: creating the schema and climbing the migration ladder is cheap in a
// normal build (about 85 ms at schema 46) and about 2.4 s under the race detector,
// which instruments every memory access of the SQLite engine. A package whose tests
// each open a new store spends most of its -race wall time there (measured 2026-10-04:
// internal/daemon, 2,013 s, of which roughly 1,300 s was this). A copy opens in about
// 0.14 s under -race.
//
// What it does not change: Open still does everything it does to an existing store —
// the version gate, the idempotent schema, migrate, the stamp, the pragma checks — over
// the copy, and the copy is this binary's own ladder's output, so it cannot be a
// layout the code no longer produces. A path that already exists is never touched, so
// a test that prepares an old, damaged or foreign file gets exactly that file. What a
// seeded binary no longer does is climb the ladder from nothing once per test; the
// store package's own tests, which do not seed, are where the ladder is proved.
//
// Outside a test binary it refuses (testing.Testing), and Open does not consult the
// seed at all, so a production caller cannot put one in force. No test covers the
// refusal: a test always runs inside a test binary, where testing.Testing is true.
// Importing testing here adds no command-line flags to a production binary: the
// package registers its flags in testing.Init, which only a test binary's generated
// main calls (since Go 1.13), and the production binary does not link it.
func SeedFreshStores() (stop func(), err error) {
	if !testing.Testing() {
		return nil, errors.New("seed fresh stores: only a test binary may seed new stores")
	}
	dir, err := os.MkdirTemp("", "crossing-guard-store-seed-")
	if err != nil {
		return nil, fmt.Errorf("seed fresh stores: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	seed, err := buildFreshStoreSeed(dir)
	if err != nil {
		return nil, fmt.Errorf("seed fresh stores: %w", err)
	}
	previous := freshStoreSeed.Swap(&seed)
	return func() { freshStoreSeed.Store(previous) }, nil
}

// buildFreshStoreSeed opens a new store the ordinary way and returns its bytes as one
// self-contained file (Export: no -wal or -shm sibling to carry).
func buildFreshStoreSeed(dir string) ([]byte, error) {
	// openAt, not open: the seed is always the ladder's own output, never a copy of
	// an earlier seed.
	ix, err := openAt(filepath.Join(dir, "ladder.sqlite"), false)
	if err != nil {
		return nil, err
	}
	// The ladder minted this store's device id (migrateSyncV31). A copy of that row
	// would make every seeded store the same device, so the seed carries none: the
	// open over each copy finds no device and mints its own, as it does for a store
	// written before schema 31.
	if _, err := ix.db.Exec(`DELETE FROM sync_device`); err != nil {
		_ = ix.Close()
		return nil, fmt.Errorf("drop the seed's device identity: %w", err)
	}
	exported := filepath.Join(dir, "seed.sqlite")
	err = ix.Export(exported)
	if closeErr := ix.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return os.ReadFile(exported)
}

// placeFreshStoreSeed puts a copy of the seed at path when nothing is there. The copy
// appears whole or not at all: it is written beside the path and linked into place,
// and a link onto an existing name fails, so two opens of one new path cannot overwrite
// each other and neither can read a half-written file. Every failure is left alone on
// purpose — the open that follows creates the store the ordinary way, which is always
// correct and only slower.
func placeFreshStoreSeed(path string, seed []byte) {
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return
	}
	staged, err := os.CreateTemp(filepath.Dir(path), ".store-seed-*")
	if err != nil {
		return
	}
	defer func() { _ = os.Remove(staged.Name()) }()
	_, writeErr := staged.Write(seed)
	if closeErr := staged.Close(); writeErr != nil || closeErr != nil {
		return
	}
	_ = os.Link(staged.Name(), path)
}
