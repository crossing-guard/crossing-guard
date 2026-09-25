package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"crossing-guard/internal/analyzermodule"
)

func runAnalyzers(args []string) int {
	data, command, rest, err := parseDataSubcommand("analyzers", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	data = effectiveAnalyzerDataDir(data)
	switch command {
	case "list":
		return listAnalyzers(data, rest)
	case "install":
		return installAnalyzer(data, rest)
	case "inspect":
		return inspectAnalyzer(data, rest)
	case "remove":
		return removeAnalyzer(data, rest)
	case "select":
		return selectAnalyzer(data, rest)
	case "deselect":
		return deselectAnalyzer(data, rest)
	case "doctor":
		return doctorAnalyzers(data, rest)
	default:
		analyzersUsage(os.Stderr)
		return 2
	}
}

func removeAnalyzer(data string, args []string) int {
	flags := flag.NewFlagSet("analyzers remove", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	yes := flags.Bool("yes", false, "confirm exact package removal")
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard analyzers [--data DIR] remove --yes MODULE@DIGEST")
		return 2
	}
	if !*yes {
		fmt.Println("not removed — repeat with --yes; selected packages must be deselected first")
		return 2
	}
	if err := analyzermodule.Remove(data, flags.Arg(0)); err != nil {
		fmt.Fprintln(os.Stderr, "remove analyzer:", err)
		return 1
	}
	fmt.Println("removed", flags.Arg(0))
	return 0
}

func effectiveAnalyzerDataDir(data string) string {
	if strings.TrimSpace(data) != "" {
		return data
	}
	return dataDir()
}

func listAnalyzers(data string, args []string) int {
	flags := flag.NewFlagSet("analyzers list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard analyzers [--data DIR] list [--json]")
		return 2
	}
	packages, catalogErrors := analyzermodule.List(data)
	selection, selectionErr := analyzermodule.LoadSelection(data)
	selected := map[string]string{}
	if selectionErr == nil {
		for _, item := range selection.Modules {
			selected[item.ModuleID] = item.PackageDigest
		}
	}
	if *jsonOutput {
		payload := struct {
			Packages       []analyzerDisplay `json:"packages"`
			CatalogErrors  []string          `json:"catalog_errors"`
			SelectionError string            `json:"selection_error,omitempty"`
		}{Packages: analyzerDisplays(packages, selected), CatalogErrors: errorStrings(catalogErrors)}
		if selectionErr != nil {
			payload.SelectionError = selectionErr.Error()
		}
		return writeAnalyzerJSON(payload)
	}
	if len(packages) == 0 {
		fmt.Println("No analyzer modules are installed.")
	}
	for _, display := range analyzerDisplays(packages, selected) {
		state := "available"
		if display.Selected {
			state = "selected"
		}
		fmt.Printf("%s  %s  %s\n", state, display.Reference, strings.Join(display.Extensions, ","))
	}
	for _, catalogErr := range catalogErrors {
		fmt.Fprintln(os.Stderr, "catalog error:", catalogErr)
	}
	if selectionErr != nil {
		fmt.Fprintln(os.Stderr, "selection error:", selectionErr)
		return 1
	}
	return boolExit(len(catalogErrors) != 0)
}

type analyzerDisplay struct {
	Reference    string   `json:"reference"`
	ModuleID     string   `json:"module_id"`
	Version      string   `json:"version"`
	AnalyzerID   string   `json:"analyzer_identity"`
	Extensions   []string `json:"extensions"`
	Capabilities []string `json:"capabilities"`
	Selected     bool     `json:"selected"`
}

func analyzerDisplays(packages []analyzermodule.Package, selected map[string]string) []analyzerDisplay {
	displays := make([]analyzerDisplay, 0, len(packages))
	for _, installed := range packages {
		capabilities := make([]string, len(installed.Manifest.Capabilities))
		for index, capability := range installed.Manifest.Capabilities {
			capabilities[index] = string(capability)
		}
		sort.Strings(capabilities)
		displays = append(displays, analyzerDisplay{Reference: installed.Manifest.ModuleID + "@" + installed.PackageDigest,
			ModuleID: installed.Manifest.ModuleID, Version: installed.Manifest.ModuleVersion,
			AnalyzerID: installed.Manifest.AnalyzerIdentity, Extensions: installed.Manifest.Extensions,
			Capabilities: capabilities, Selected: selected[installed.Manifest.ModuleID] == installed.PackageDigest})
	}
	return displays
}

func installAnalyzer(data string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard analyzers [--data DIR] install DIRECTORY")
		return 2
	}
	installed, err := analyzermodule.Install(data, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "install analyzer:", err)
		return 1
	}
	printAnalyzerPackage(installed)
	fmt.Println("available only — this module has not been executed or selected")
	return 0
}

func inspectAnalyzer(data string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard analyzers [--data DIR] inspect MODULE@DIGEST")
		return 2
	}
	installed, err := analyzermodule.Find(data, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "inspect analyzer:", err)
		return 1
	}
	printAnalyzerPackage(installed)
	return 0
}

func selectAnalyzer(data string, args []string) int {
	flags := flag.NewFlagSet("analyzers select", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	yes := flags.Bool("yes", false, "consent to execute selected local module")
	replace := flags.String("replace", "", "explicitly replace selected module ID")
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard analyzers [--data DIR] select [--replace MODULE] --yes MODULE@DIGEST")
		return 2
	}
	installed, err := analyzermodule.Find(data, flags.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "select analyzer:", err)
		return 1
	}
	printAnalyzerPackage(installed)
	fmt.Println("SECURITY: selecting this module authorizes its native executable to run with your local user permissions.")
	fmt.Println("Crossing Guard v1 does not sandbox its filesystem access or network access.")
	if !*yes {
		fmt.Println("not selected — inspect the exact digest, then repeat with --yes")
		return 2
	}
	selection, err := analyzermodule.Select(data, installed, strings.TrimSpace(*replace), "cli", time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "select analyzer:", err)
		return 1
	}
	fmt.Printf("selected %s (%d selected process module(s)); restart the daemon to activate the new assembly\n",
		installed.Manifest.ModuleID, len(selection.Modules))
	return 0
}

func deselectAnalyzer(data string, args []string) int {
	flags := flag.NewFlagSet("analyzers deselect", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	yes := flags.Bool("yes", false, "confirm deselection")
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard analyzers [--data DIR] deselect MODULE --yes")
		return 2
	}
	if !*yes {
		fmt.Println("not deselected — repeat with --yes")
		return 2
	}
	selection, err := analyzermodule.Deselect(data, flags.Arg(0), "cli", time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "deselect analyzer:", err)
		return 1
	}
	fmt.Printf("deselected %s (%d selected process module(s)); restart the daemon to activate the new assembly\n",
		flags.Arg(0), len(selection.Modules))
	return 0
}

func doctorAnalyzers(data string, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard analyzers [--data DIR] doctor")
		return 2
	}
	packages, err := analyzermodule.ResolveSelected(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "analyzer selection is inactive:", err)
		return 1
	}
	if len(packages) == 0 {
		fmt.Println("no process analyzer module selected; compatibility analyzers remain separate")
		return 0
	}
	for _, installed := range packages {
		fmt.Printf("active selection %s@%s\n", installed.Manifest.ModuleID, installed.PackageDigest)
	}
	return 0
}

func printAnalyzerPackage(installed analyzermodule.Package) {
	fmt.Printf("module: %s %s\n", installed.Manifest.ModuleID, installed.Manifest.ModuleVersion)
	fmt.Printf("package: %s\n", installed.PackageDigest)
	fmt.Printf("entrypoint: %s (%s)\n", installed.Manifest.Entrypoint, installed.EntrypointDigest)
	fmt.Printf("analyzer: %s · protocol %s\n", installed.Manifest.AnalyzerIdentity, installed.Manifest.ProtocolVersion)
	fmt.Printf("claims: %s · %s\n", strings.Join(installed.Manifest.Languages, ","), strings.Join(installed.Manifest.Extensions, ","))
}

func analyzersUsage(output io.Writer) {
	fmt.Fprintln(output, "usage: crossing-guard analyzers [--data DIR] list|install|inspect|select|deselect|remove|doctor ...")
}

func errorStrings(errorsFound []error) []string {
	values := make([]string, len(errorsFound))
	for index, err := range errorsFound {
		values[index] = err.Error()
	}
	return values
}

func writeAnalyzerJSON(value any) int {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, "write analyzer JSON:", err)
		return 1
	}
	return 0
}

func boolExit(failed bool) int {
	if failed {
		return 1
	}
	return 0
}
