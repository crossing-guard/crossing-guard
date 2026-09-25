package main

import (
	"fmt"
	"os"

	"crossing-guard/internal/daemon"
)

func indexMemory(args []string) {
	fmt.Println("indexing memory into the governance store (entity + classification + FTS)…")
	result, err := daemon.IndexAllMemory("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "index-memory:", err)
		os.Exit(1)
	}
	fmt.Println(daemon.FmtMemoryIndexResult(result))
	fmt.Println("memory is now a governed entity: `crossing-guard entities --kind memory`, searchable via /api/search,")
	fmt.Println("and gateable by policy on its folded state (item 17).")
}
