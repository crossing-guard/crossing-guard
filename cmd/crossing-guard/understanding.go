package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"crossing-guard/codemap"
	"crossing-guard/internal/analyzerhost"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/understanding"
	"crossing-guard/store"
)

type understandingShow struct {
	Store               string                             `json:"store"`
	Generation          store.UnderstandingGeneration      `json:"generation"`
	Units               []store.UnderstandingUnit          `json:"units"`
	UnitTotal           int                                `json:"unit_total"`
	Edges               []store.UnderstandingEdge          `json:"edges"`
	EdgeTotal           int                                `json:"edge_total"`
	Coverage            []store.UnderstandingCoverage      `json:"coverage"`
	CandidateState      string                             `json:"candidate_state,omitempty"`
	CandidateReason     string                             `json:"candidate_reason,omitempty"`
	CandidateAnalyzerID string                             `json:"candidate_analyzer_id,omitempty"`
	CandidatePage       *codemap.ResponsibilityPage        `json:"candidate_page,omitempty"`
	Candidates          *[]codemap.ResponsibilityCandidate `json:"candidates,omitempty"`
}

func runUnderstand(args []string) int {
	data, subcommand, rest, err := parseDataSubcommand("understand", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if subcommand != "scan" && subcommand != "show" {
		fmt.Fprintln(os.Stderr, "unknown understand subcommand:", subcommand)
		return 2
	}
	path := store.IndexPath(data, mustHome())
	if subcommand == "scan" {
		return runUnderstandScan(path, effectiveAnalyzerDataDir(data), rest)
	}
	return runUnderstandShow(path, rest)
}

func runUnderstandScan(path, dataDir string, args []string) int {
	flags := flag.NewFlagSet("understand scan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repo, base, conventions := "", "", ""
	timeout := 2 * time.Minute
	flags.StringVar(&repo, "repo", "", "repository directory")
	flags.StringVar(&base, "base", "", "required base ref")
	flags.StringVar(&conventions, "conventions", "", "explicit convention configuration file")
	flags.DurationVar(&timeout, "timeout", timeout, "scan timeout (maximum 10m)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(repo) == "" || strings.TrimSpace(base) == "" || timeout <= 0 || timeout > 10*time.Minute {
		fmt.Fprintln(os.Stderr, "scan requires --repo, --base, no positional arguments, and --timeout in (0,10m]")
		return 2
	}
	index, err := store.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		return 1
	}
	defer func() { _ = index.Close() }()
	interruptContext, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(interruptContext, timeout)
	defer cancel()
	host, err := analyzerhost.Build(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "assemble analyzers:", err)
		return 1
	}
	if host.SelectionError != nil {
		fmt.Fprintln(os.Stderr, "selected analyzers inactive:", host.SelectionError)
	}
	result, scanErr := understanding.Scan(ctx, index, understanding.ScanInput{RepoDir: repo, Base: base,
		Conventions: conventions, Assembly: host.Assembly})
	if result.Generation != nil {
		summary := *result.Generation
		summary.Units = nil
		summary.Edges = nil
		body, _ := json.MarshalIndent(struct {
			Store      string                         `json:"store"`
			Recorded   bool                           `json:"recorded"`
			Generation *store.UnderstandingGeneration `json:"generation"`
		}{Store: path, Recorded: result.Recorded, Generation: &summary}, "", "  ")
		fmt.Println(string(body))
	}
	if scanErr != nil {
		fmt.Fprintln(os.Stderr, "understand scan:", scanErr)
		fmt.Fprintln(os.Stderr, "store:", path)
		return 1
	}
	return 0
}

func runUnderstandShow(path string, args []string) int {
	flags := flag.NewFlagSet("understand show", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repo, center, compare := "", "", ""
	generationID := int64(0)
	edgeOffset, edgeLimit := 0, 100
	unitOffset, unitLimit := 0, 100
	candidateOffset, candidateLimit := 0, 100
	jsonOutput := false
	flags.StringVar(&repo, "repo", "", "repository directory")
	flags.Int64Var(&generationID, "generation", 0, "generation id")
	flags.StringVar(&center, "center", "", "optional KIND:REF edge center")
	flags.StringVar(&compare, "compare", "", "optional exact stored file path for mechanical candidates")
	flags.IntVar(&edgeOffset, "edge-offset", 0, "edge page offset")
	flags.IntVar(&edgeLimit, "edge-limit", 100, "edge page limit")
	flags.IntVar(&unitOffset, "unit-offset", 0, "unit page offset")
	flags.IntVar(&unitLimit, "unit-limit", 100, "unit page limit")
	flags.IntVar(&candidateOffset, "candidate-offset", 0, "candidate page offset")
	flags.IntVar(&candidateLimit, "candidate-limit", 100, "candidate page limit (maximum 100)")
	flags.BoolVar(&jsonOutput, "json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || (generationID == 0) == (strings.TrimSpace(repo) == "") || edgeOffset < 0 || unitOffset < 0 || candidateOffset < 0 || edgeLimit <= 0 || edgeLimit > 1000 || unitLimit <= 0 || unitLimit > 1000 || candidateLimit <= 0 || candidateLimit > 100 {
		fmt.Fprintln(os.Stderr, "show requires exactly one of --repo or --generation, nonnegative offsets, edge/unit limits in [1,1000], and candidate limit in [1,100]")
		return 2
	}
	index, err := store.OpenRO(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store read-only:", err)
		return 1
	}
	defer func() { _ = index.Close() }()
	var generation store.UnderstandingGeneration
	var found bool
	if generationID != 0 {
		generation, found, err = index.UnderstandingGenerationByID(generationID)
	} else {
		repository, resolveErr := changeenv.ResolveRepository(repo)
		if resolveErr != nil {
			fmt.Fprintln(os.Stderr, "resolve repository:", resolveErr)
			return 1
		}
		// The default is the newest attempt retention has not pruned (it may be a
		// failed attempt, as before); a checkout whose every generation was pruned
		// shows the newest one.
		generations, _, listErr := index.UnderstandingGenerations(repository.ID, repository.CheckoutID, 0)
		err = listErr
		if len(generations) > 0 {
			generation, found = generations[0], true
		}
		for _, candidate := range generations {
			if candidate.FactsState != store.UnderstandingFactsPruned {
				generation = candidate
				break
			}
		}
	}
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("understanding generation not found")
		}
		fmt.Fprintln(os.Stderr, "understand show:", err)
		fmt.Fprintln(os.Stderr, "store:", path)
		return 1
	}
	centerKind, centerRef := "", ""
	if center != "" {
		var ok bool
		centerKind, centerRef, ok = strings.Cut(center, ":")
		if !ok || centerKind == "" || centerRef == "" {
			fmt.Fprintln(os.Stderr, "--center must be KIND:REF")
			return 2
		}
	}
	if generation.FactsState == store.UnderstandingFactsPruned {
		return showPrunedUnderstanding(path, generation, compare != "", jsonOutput)
	}
	units, unitTotal, err := index.UnderstandingUnits(generation.ID, unitOffset, unitLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read units:", err)
		return 1
	}
	edges, edgeTotal, err := index.UnderstandingEdges(generation.ID, centerKind, centerRef, edgeOffset, edgeLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read edges:", err)
		return 1
	}
	coverage, err := index.UnderstandingCoverage(generation.ID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read coverage:", err)
		return 1
	}
	view := understandingShow{Store: path, Generation: generation, Units: units, UnitTotal: unitTotal, Edges: edges, EdgeTotal: edgeTotal, Coverage: coverage}
	if compare != "" {
		candidateView, candidateErr := readUnderstandingCandidates(index, generation, compare, candidateOffset, candidateLimit)
		if candidateErr != nil {
			fmt.Fprintln(os.Stderr, "read candidates:", candidateErr)
			return 1
		}
		view.CandidateState = candidateView.State
		view.CandidateReason = candidateView.Reason
		view.CandidateAnalyzerID = candidateView.AnalyzerID
		view.CandidatePage = candidateView.Page
		view.Candidates = &candidateView.Rows
	}
	if jsonOutput {
		body, _ := json.MarshalIndent(view, "", "  ")
		fmt.Println(string(body))
		return 0
	}
	fmt.Printf("generation %d  %s  snapshot %s\n", generation.ID, generation.Status, generation.SnapshotDigest)
	fmt.Printf("analyzer %s  conventions %s  units %d/%d  edges %d/%d\n", generation.AnalyzerBundleDigest, generation.ConventionState, len(units), unitTotal, len(edges), edgeTotal)
	for _, row := range coverage {
		fmt.Printf("%-24s %-11s %s  %d/%d errors=%d\n", row.Family, row.State, row.AnalyzerID, row.Produced, row.Attempted, row.Errors)
	}
	if compare != "" {
		fmt.Printf("candidate facts for %s  state=%s  analyzer=%s\n", compare, view.CandidateState, view.CandidateAnalyzerID)
		if view.CandidateReason != "" {
			fmt.Println("candidate boundary:", view.CandidateReason)
		}
		if view.CandidatePage != nil {
			fmt.Printf("candidate page %d/%d returned=%d limit=%d\n", view.CandidatePage.Offset, view.CandidatePage.Total, len(*view.Candidates), view.CandidatePage.Limit)
		}
		for _, row := range *view.Candidates {
			fmt.Printf("%-40s shared names=%d body-shapes=%d internal-dependencies=%d external-dependencies=%d\n", row.RightPath, row.SharedDeclarations.Total, row.SharedBodyShapes.Total, row.SharedInternalDependencies.Total, row.SharedExternalDependencies.Total)
		}
	}
	fmt.Println("store:", path)
	return 0
}

// showPrunedUnderstanding prints a generation whose facts retention removed. It
// reads no unit, edge or coverage rows: rows not yet deleted are garbage no reader
// resolves, and an empty page would read as "measured nothing".
func showPrunedUnderstanding(path string, generation store.UnderstandingGeneration, compared, jsonOutput bool) int {
	const reason = "analysis facts were removed by retention"
	view := understandingShow{Store: path, Generation: generation, Units: []store.UnderstandingUnit{},
		Edges: []store.UnderstandingEdge{}, Coverage: []store.UnderstandingCoverage{}}
	if compared {
		view.CandidateState, view.CandidateReason = "unavailable", reason
	}
	if jsonOutput {
		body, _ := json.MarshalIndent(view, "", "  ")
		fmt.Println(string(body))
		return 0
	}
	fmt.Printf("generation %d  %s  snapshot %s\n", generation.ID, generation.Status, generation.SnapshotDigest)
	fmt.Printf("%s; the scan measured %d units and %d edges\n", reason, generation.UnitTotal, generation.EdgeTotal)
	fmt.Println("store:", path)
	return 0
}

func readUnderstandingCandidates(index *store.Index, generation store.UnderstandingGeneration, centerPath string, offset, limit int) (codemap.ResponsibilityResult, error) {
	if generation.Status != "complete" {
		return codemap.ResponsibilityResult{State: "unavailable", Reason: "candidate facts require a complete understanding generation", Rows: []codemap.ResponsibilityCandidate{}}, nil
	}
	units := make([]store.UnderstandingUnit, 0, generation.UnitTotal)
	for pageOffset := 0; ; {
		page, total, err := index.UnderstandingUnits(generation.ID, pageOffset, 1000)
		if err != nil {
			return codemap.ResponsibilityResult{}, err
		}
		units = append(units, page...)
		pageOffset += len(page)
		if pageOffset >= total || len(page) == 0 {
			break
		}
	}
	coverage, err := index.UnderstandingCoverage(generation.ID)
	if err != nil {
		return codemap.ResponsibilityResult{}, err
	}
	stored := make([]codemap.StoredResponsibilityUnit, 0, len(units))
	for _, unit := range units {
		stored = append(stored, codemap.StoredResponsibilityUnit{Path: unit.Path, DescriptorJSON: unit.DescriptorJSON})
	}
	coverageFacts := make([]codemap.ResponsibilityCoverageFact, 0, len(coverage))
	for _, fact := range coverage {
		coverageFacts = append(coverageFacts, codemap.ResponsibilityCoverageFact{Family: fact.Family, State: fact.State, AnalyzerID: fact.AnalyzerID, Reason: fact.Reason})
	}
	return codemap.ProjectResponsibility(centerPath, stored, coverageFacts, offset, limit), nil
}
