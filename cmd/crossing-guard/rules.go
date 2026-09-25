package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"crossing-guard/internal/rulebook"
)

func rulesCmd(args []string) {
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			rulesUsage()
		}
		printRulebookStatus()
		return
	}
	switch args[0] {
	case "select":
		rulesSelect(args[1:])
	case "unselect", "rollback-legacy":
		rulesRollback(args[1:])
	default:
		rulesUsage()
	}
}

func rulesUsage() {
	fmt.Fprintln(os.Stderr, "usage: crossing-guard rules status | select (current|safety-starter|security-observe|none|PATH) [--yes] | unselect [--yes]")
	os.Exit(2)
}

func printRulebookStatus() {
	loaded, err := rulebook.LoadDocument()
	if err != nil {
		fmt.Println("rulebook: UNAVAILABLE")
		fmt.Println("  reason: " + err.Error())
		fmt.Println("  recovery: remove any CG_RULES override, then inspect or roll back the selection")
		return
	}
	fmt.Printf("rulebook:\n  available starter: %s\n", loaded.AvailableStarterDigest)
	fmt.Printf("  selected:          %t\n", loaded.Selected)
	fmt.Printf("  active:            %t\n", loaded.Active)
	fmt.Printf("  origin:            %s\n", loaded.Origin)
	fmt.Printf("  selection:         %s\n", loaded.Selection)
	fmt.Printf("  digest:            %s\n", loaded.Digest)
	fmt.Printf("  install cohort:    %s\n", loaded.InstallCohort)
	if loaded.InstallProfileCreated {
		fmt.Printf("  framework state:   created %s/installation.json (cohort fact; no policy selected)\n", dataDir())
	}
	fmt.Printf("  path:              %s\n", loaded.Path)
	if loaded.Selector != "" {
		fmt.Printf("  selector:          %s\n", loaded.Selector)
	}
	if loaded.SelectedAt != "" {
		fmt.Printf("  selected at:       %s\n", loaded.SelectedAt)
	}
	if loaded.SelectedSource != "" {
		fmt.Printf("  selected source:   %s\n", loaded.SelectedSource)
	}
	if loaded.SelectedSourceRef != "" {
		fmt.Printf("  source reference:  %s\n", loaded.SelectedSourceRef)
	}
	if loaded.Compatibility != "" {
		if loaded.Selection == "unselected-baseline" {
			fmt.Printf("  baseline:          %s (no policy consequence)\n", loaded.Compatibility)
		} else {
			fmt.Printf("  compatibility:     %s (file discovery is not consent)\n", loaded.Compatibility)
		}
	}
	if loaded.Displaced != nil {
		fmt.Printf("  displaced user selection: %s (%s by %s)\n",
			loaded.Displaced.Digest, loaded.Displaced.Source, loaded.Displaced.Selector)
	}
	if loaded.DisplacedError != "" {
		fmt.Printf("  displaced selection error: %s\n", loaded.DisplacedError)
	}
	if loaded.Selection == "explicit-user" {
		fmt.Println("  recovery: crossing-guard rules unselect")
	} else if loaded.Selection == "invocation-path" {
		fmt.Println("  scope: invocation override; durable selection changes stay displaced until CG_RULES is removed")
	} else {
		candidate := "current"
		if loaded.Selection == "unselected-baseline" {
			candidate = "safety-starter"
		}
		fmt.Println("  select explicitly: crossing-guard rules select " + candidate)
	}
}

func rulesSelect(args []string) {
	yes, positional := yesAndArgs(args)
	if len(positional) != 1 {
		rulesUsage()
	}
	source, path := positional[0], ""
	switch source {
	case "current", "starter", "safety-starter", "security-observe", "none":
	default:
		path = source
		source = "file"
	}
	preview, err := rulebook.PreviewSelection(source, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot preview selection:", err)
		os.Exit(1)
	}
	printSelectionPreview(preview)
	if !yes && !ask(bufio.NewReader(os.Stdin), "Select these exact bytes at the user layer?") {
		fmt.Println("not selected")
		return
	}
	result, err := rulebook.Select(rulebook.SelectRequest{Source: source, Path: path,
		Selector: "cli", ExpectedActiveDigest: preview.ExpectedActiveDigest,
		ExpectedStateToken: preview.ExpectedStateToken,
		AcknowledgedDigest: preview.ProposedDigest})
	if err != nil {
		fmt.Fprintln(os.Stderr, "selection failed:", err)
		os.Exit(1)
	}
	fmt.Printf("selected: %s\n", result.DurableSelection.Digest)
	if result.Active.Selection == "invocation-path" {
		fmt.Printf("active remains invocation override %s; remove CG_RULES to activate the durable selection\n", result.Active.Digest)
	} else {
		fmt.Printf("active:   %s [%s]\n", result.Active.Digest, result.Active.Selection)
	}
}

func printSelectionPreview(preview rulebook.SelectionPreview) {
	fmt.Printf("rulebook selection review:\n  source:           %s", preview.Source)
	if preview.SourcePath != "" {
		fmt.Printf(" (%s)", preview.SourcePath)
	}
	fmt.Printf("\n  current digest:   %s [%s]\n", preview.ExpectedActiveDigest, preview.ActiveOrigin)
	fmt.Printf("  proposed digest:  %s\n", preview.ProposedDigest)
	fmt.Printf("  rules:            %d (%s)\n", preview.RuleCount, preview.ActionSummary)
	fmt.Printf("  added IDs:        %s\n", listOrNone(preview.Added))
	fmt.Printf("  removed IDs:      %s\n", listOrNone(preview.Removed))
	if len(preview.Changed) == 0 {
		fmt.Println("  changed IDs:      (none)")
	} else {
		for _, change := range preview.Changed {
			fmt.Printf("  changed ID:       %s action %s -> %s predicate %s -> %s\n",
				change.ID, change.BeforeAction, change.AfterAction,
				change.BeforePredicate, change.AfterPredicate)
		}
	}
	fmt.Printf("  semantic reach:   %s\n", preview.SemanticReach)
	fmt.Println("  unselect:         crossing-guard rules unselect")
}

func rulesRollback(args []string) {
	yes, positional := yesAndArgs(args)
	if len(positional) != 0 {
		rulesUsage()
	}
	loaded, err := rulebook.LoadDocument()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot load active selection:", err)
		os.Exit(1)
	}
	if loaded.Selection != "explicit-user" {
		fmt.Fprintln(os.Stderr, "unselect requires an active durable user selection; current state is", loaded.Selection)
		os.Exit(1)
	}
	fmt.Printf("selected digest: %s\ncohort baseline will be preflighted before the selection record is archived.\n", loaded.Digest)
	if !yes && !ask(bufio.NewReader(os.Stdin), "Archive this selection and return to the cohort baseline?") {
		fmt.Println("not unselected")
		return
	}
	active, archive, err := rulebook.Unselect(loaded.StateToken, "cli")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unselect failed:", err)
		os.Exit(1)
	}
	fmt.Printf("selection archived: %s\nactive: %s [%s]\n", archive, active.Digest, active.Selection)
}

func yesAndArgs(args []string) (bool, []string) {
	var positional []string
	yes := false
	for _, arg := range args {
		if arg == "--yes" {
			yes = true
		} else {
			positional = append(positional, arg)
		}
	}
	return yes, positional
}

func listOrNone(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ", ")
}
