package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/store"
)

// export snapshots PRIMARY TRUTH through SQLite's consistent read path while the
// daemon may still be writing. Every post-open branch closes before process exit.
func export(args []string) {
	destination := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		destination = args[0]
	}
	if destination == "" {
		destination = filepath.Join(dataDir(), "exports", fmt.Sprintf("index-%d.sqlite", time.Now().Unix()))
	}
	source := store.IndexPath("", mustHome())
	if _, err := os.Stat(source); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "no store to export at %s\n", source)
		os.Exit(1)
	}
	index, err := store.OpenRO(source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	if err := index.Export(destination); err != nil {
		closeErr := index.Close()
		fmt.Fprintln(os.Stderr, "export:", err)
		if closeErr != nil {
			fmt.Fprintln(os.Stderr, "close store:", closeErr)
		}
		os.Exit(1)
	}
	if err := index.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close store:", err)
		os.Exit(1)
	}
	info, _ := os.Stat(destination)
	fmt.Printf("exported %s -> %s", source, destination)
	if info != nil {
		fmt.Printf(" (%d bytes)", info.Size())
	}
	fmt.Println()
	fmt.Println("the snapshot is itself a store: open it with any sqlite3 client, or point --data at its dir")
}
