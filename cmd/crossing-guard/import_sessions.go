package main

import (
	"fmt"
	"os"
	"strconv"

	"crossing-guard/internal/daemon"
)

// importSessions is explicitly second-class history: it cannot recover blocked calls
// or structured targets and never overwrites a session already captured live.
func importSessions(args []string) {
	vendor, project := "", ""
	limit := 0
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--vendor":
			if index+1 < len(args) {
				vendor, index = args[index+1], index+1
			}
		case "--project":
			if index+1 < len(args) {
				project, index = args[index+1], index+1
			}
		case "--limit":
			if index+1 < len(args) {
				value, err := strconv.Atoi(args[index+1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "--limit must be an integer: %q\n", args[index+1])
					os.Exit(2)
				}
				limit, index = value, index+1
			}
		}
	}
	fmt.Println("importing past sessions as governance events (lossy, marked origin=imported)…")
	result, err := daemon.ImportSessions("", vendor, project, limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "import:", err)
		os.Exit(1)
	}
	fmt.Println(daemon.FmtImportResult(result))
	fmt.Println("imported events cannot see blocked actions and carry no structured file/url — see docs.")
}
