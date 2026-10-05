package main

import (
	"bufio"
	"fmt"

	"crossing-guard/internal/guardcli"
)

// initRecall is init's recall step (recall-mcp-v1-plan §3.7). For each runtime
// with hook consent it asks once to register the recall tools, and repairs
// without asking where the yes is already recorded. answer is "yes"
// (--recall), "no" (--no-recall) or "" (ask); --yes never answers it, and a
// "no" never removes an existing registration (uninstall does).
func initRecall(in *bufio.Reader, self string, dryRun bool, answer string) {
	consent := guardcli.LoadConsent() // read fresh: the hook loop may have just added consents
	printed := false
	header := func() {
		if !printed {
			fmt.Println("\nRecall tools (search sessions, memories and tags, and see who else is working here):")
			printed = true
		}
	}
	for _, name := range guardcli.RecallRuntimes() {
		vendor, consented := consent.Vendors[name]
		if !consented || vendor.Config == "" {
			continue
		}
		file := guardcli.RecallConfigFor(name, vendor.Config)
		state, err := guardcli.RecallStatusFor(name, vendor.Config, self)
		header()
		if err != nil {
			fmt.Printf("  %-8s cannot read %s: %v\n", name, file, err)
			continue
		}
		if vendor.Recall != nil {
			switch {
			case state == guardcli.RecallCurrent:
				fmt.Printf("  %-8s registered (%s)\n", name, file)
			case state == guardcli.RecallForeign:
				fmt.Printf("  %-8s another program owns the entry in %s — left alone\n", name, file)
			case dryRun:
				fmt.Printf("  %-8s would repair the registration in %s\n", name, file)
			default:
				reportRecall(name, file, guardcli.RegisterRecallFor(name, vendor.Config, self), "repaired")
			}
			continue
		}
		if dryRun {
			fmt.Printf("  %-8s would ask to register them in %s\n", name, file)
			continue
		}
		yes := answer == "yes"
		if answer == "" {
			yes = askWithFlag(in, fmt.Sprintf("  Also give %s sessions the Crossing Guard recall tools (%s)?", name, file), "--recall")
		}
		if !yes {
			fmt.Printf("  %-8s recall tools not registered\n", name)
			continue
		}
		reportRecall(name, file, guardcli.RegisterRecallFor(name, vendor.Config, self), "registered")
	}
}

func reportRecall(name, file string, err error, done string) {
	if err != nil {
		fmt.Printf("  %-8s FAILED — %v\n", name, err)
		return
	}
	fmt.Printf("  %-8s %s in %s\n", name, done, file)
}
