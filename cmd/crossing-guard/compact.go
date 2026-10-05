package main

import (
	"fmt"
	"os"
	"path/filepath"

	"crossing-guard/internal/daemon"
	"crossing-guard/store"
)

// compact rewrites the store file without its free pages (store.Index.Compact). It
// is whole-file maintenance: it runs only when no daemon can be using the store,
// holds the store exclusively from open to close — a pending schema migration
// included — and changes nothing when it refuses.
func compact(args []string) {
	os.Exit(runCompact(args))
}

func runCompact(args []string) int {
	// The default is the data directory of the installed service, which is where its
	// daemon publishes the address the probe below reads.
	dir, _ := daemon.ServiceTarget()
	explicit := false
	for index := 0; index < len(args); index++ {
		switch {
		case args[index] == "--data" && index+1 < len(args):
			dir, explicit, index = args[index+1], true, index+1
		default:
			fmt.Fprintln(os.Stderr, "usage: crossing-guard compact [--data DIR]")
			return 2
		}
	}
	path := filepath.Join(dir, "index.sqlite")
	// Without --data the store is wherever the daemon would find it. If that is not
	// this data directory's own file ($CG_INDEX points elsewhere), the directory's
	// daemon-addr says nothing about who holds it, so there is nothing to probe.
	if !explicit {
		if resolved := store.IndexPath("", mustHome()); filepath.Clean(resolved) != filepath.Clean(path) {
			fmt.Fprintf(os.Stderr, "compact: the store resolves to %s, not %s; name its directory with --data\n", resolved, path)
			return 1
		}
	}
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "compact: no store at %s\n", path)
		return 1
	}
	// A daemon started by hand without --data publishes its address one level down.
	for _, published := range []string{dir, filepath.Join(dir, "console")} {
		if presence, why := daemon.ProbeDataDir(published); presence != daemon.DaemonAbsent {
			fmt.Fprintf(os.Stderr, "compact: refusing while a daemon may be using this store — %s.\nStop the daemon first (if the address is stale, remove %s); the store is unchanged.\n",
				why, filepath.Join(published, "daemon-addr"))
			return 1
		}
	}
	before, err := store.StoredSchemaVersion(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compact: read the store's schema version: %v\n", err)
		return 1
	}
	// SQLite's temporary files (index sorters, VACUUM's temporary database) go beside
	// the store, so the room check measures the one volume that matters.
	if err := os.Setenv("SQLITE_TMPDIR", dir); err != nil {
		fmt.Fprintln(os.Stderr, "compact:", err)
		return 1
	}
	fmt.Printf("opening %s exclusively (a pending schema migration runs now and can take minutes)\n", path)
	index, err := store.OpenExclusive(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compact: open store: %v\nIf another process holds the store, stop it; the store is unchanged.\n", err)
		return 1
	}
	if before != store.SchemaVersion {
		fmt.Printf("migrated the store from schema %d to %d\n", before, store.SchemaVersion)
	}
	fmt.Println("compacting")
	result, compactErr := index.Compact()
	closeErr := index.Close()
	if compactErr != nil {
		fmt.Fprintln(os.Stderr, compactErr)
		if before != store.SchemaVersion {
			fmt.Fprintf(os.Stderr, "the store IS migrated to schema %d and was not compacted; a schema-%d binary will refuse it\n", store.SchemaVersion, before)
		}
	}
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, "compact: close store:", closeErr)
	}
	if compactErr != nil || closeErr != nil {
		return 1
	}
	fmt.Printf("store: %s\nbefore: %d bytes\nafter:  %d bytes\n", path, result.BytesBefore, result.BytesAfter)
	fmt.Println("a daemon started while this ran could not open the store; start (or restart) it now")
	return 0
}
