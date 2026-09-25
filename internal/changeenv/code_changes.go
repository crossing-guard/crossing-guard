package changeenv

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"crossing-guard/codemap"
	"crossing-guard/store"
)

const (
	maxCodeChangeFiles        = 25
	maxDeclarationChangeFacts = 50
	maxDependencyFacts        = 100
)

// CodeChangeOptions selects one immutable current boundary and one bounded file page.
// An empty checkout is accepted only when the session has exactly one checkout.
type CodeChangeOptions struct {
	RepositoryID         string
	CheckoutID           string
	CheckpointID         int64
	Offset               int
	Limit                int
	AnalyzerBundleDigest string
}

// CodeChangeBoundary identifies one immutable repository snapshot and its compiled
// understanding generation.
type CodeChangeBoundary struct {
	CheckpointID   int64  `json:"checkpoint_id"`
	Kind           string `json:"kind"`
	CapturedAt     int64  `json:"captured_at"`
	SnapshotDigest string `json:"snapshot_digest"`
	GenerationID   int64  `json:"generation_id,omitempty"`
}

// CodeDeclarationFacts is a bounded population of declaration-level changes.
type CodeDeclarationFacts struct {
	Total     int                         `json:"total"`
	Returned  int                         `json:"returned"`
	Truncated bool                        `json:"truncated"`
	Changes   []codemap.DeclarationChange `json:"changes"`
}

// CurrentDeclarationFacts is a bounded population of current declarations when no
// exact baseline exists.
type CurrentDeclarationFacts struct {
	Total        int                   `json:"total"`
	Returned     int                   `json:"returned"`
	Truncated    bool                  `json:"truncated"`
	Declarations []codemap.Declaration `json:"declarations"`
}

// CodeFileChange is a factual, bounded row. It contains no size threshold, score,
// duplicate label, architectural role, or causal claim.
type CodeFileChange struct {
	Path                string                  `json:"path"`
	State               string                  `json:"state"`
	Language            string                  `json:"language,omitempty"`
	Namespace           string                  `json:"namespace,omitempty"`
	BeforeSourceHash    string                  `json:"before_source_hash,omitempty"`
	AfterSourceHash     string                  `json:"after_source_hash,omitempty"`
	DeclarationChanges  CodeDeclarationFacts    `json:"declaration_changes"`
	CurrentDeclarations CurrentDeclarationFacts `json:"current_declarations"`
}

// CodeAttributionCoverage summarizes existing path reconciliation independently from
// the checkout delta. Other preserves classes that must not be collapsed into exact,
// ambiguous, or unattributed.
type CodeAttributionCoverage struct {
	Total        int            `json:"total"`
	Returned     int            `json:"returned"`
	Exact        bool           `json:"exact"`
	ExactMatched int            `json:"exact_matched"`
	Ambiguous    int            `json:"ambiguous"`
	Unattributed int            `json:"unattributed"`
	Other        int            `json:"other"`
	Classes      map[string]int `json:"classes"`
}

// CodeIntegerDeltaPopulation aggregates only declarations with measurements on both
// sides. Added and removed declarations remain in their separate exact populations.
type CodeIntegerDeltaPopulation struct {
	Measured  int `json:"measured"`
	Delta     int `json:"delta"`
	Increased int `json:"increased"`
	Decreased int `json:"decreased"`
}

// CodeChangeAggregates contains mechanical before/after populations. State is partial
// when the bounded descriptor read cannot cover every changed analyzed path.
type CodeChangeAggregates struct {
	State                    string                                   `json:"state"`
	Reason                   string                                   `json:"reason,omitempty"`
	FileTotal                int                                      `json:"file_total"`
	FileReturned             int                                      `json:"file_returned"`
	DeclarationChanges       int                                      `json:"declaration_changes"`
	DeclarationsAdded        int                                      `json:"declarations_added"`
	DeclarationsRemoved      int                                      `json:"declarations_removed"`
	DeclarationsModified     int                                      `json:"declarations_modified"`
	DeclarationsMoved        int                                      `json:"declarations_moved"`
	SourceSpanLines          CodeIntegerDeltaPopulation               `json:"source_span_lines"`
	Cyclomatic               CodeIntegerDeltaPopulation               `json:"cyclomatic"`
	Callers                  CodeCallerFacts                          `json:"callers"`
	DependencyState          string                                   `json:"dependency_state"`
	DependencyReason         string                                   `json:"dependency_reason,omitempty"`
	DependentPackages        CodeStringFacts                          `json:"dependent_packages"`
	ReferencingFiles         CodeStringFacts                          `json:"referencing_files"`
	StructuralCenterTotal    int                                      `json:"structural_center_total"`
	StructuralCenterReturned int                                      `json:"structural_center_returned"`
	StructuralMatches        codemap.StructuralMatchPopulationChanges `json:"structural_matches"`
}

// CodeChanges is the bounded collection response for one repository checkout.
type CodeChanges struct {
	ID                         string                        `json:"id"`
	Observation                string                        `json:"observation"`
	Population                 string                        `json:"population"`
	State                      string                        `json:"state"`
	Reason                     string                        `json:"reason,omitempty"`
	RepositoryID               string                        `json:"repository_id,omitempty"`
	CheckoutID                 string                        `json:"checkout_id,omitempty"`
	AvailableCheckouts         []CodeCheckoutIdentity        `json:"available_checkouts,omitempty"`
	Baseline                   *CodeChangeBoundary           `json:"baseline,omitempty"`
	Current                    *CodeChangeBoundary           `json:"current,omitempty"`
	FilePage                   Page                          `json:"file_page"`
	Files                      []CodeFileChange              `json:"files"`
	CurrentCoverage            []store.UnderstandingCoverage `json:"current_coverage"`
	BaselineCoverage           []store.UnderstandingCoverage `json:"baseline_coverage"`
	Attribution                CodeAttributionCoverage       `json:"attribution"`
	Aggregates                 CodeChangeAggregates          `json:"aggregates"`
	FunctionCallCoverage       string                        `json:"function_call_coverage"`
	CurrentCheckpointPathTotal int                           `json:"current_checkpoint_path_total"`
	CurrentAnalyzedUnits       int                           `json:"current_analyzed_units"`
	BaselineAnalyzedUnits      int                           `json:"baseline_analyzed_units"`
}

// CodeCheckoutIdentity identifies one repository checkout observed in a session.
type CodeCheckoutIdentity struct {
	RepositoryID string `json:"repository_id"`
	CheckoutID   string `json:"checkout_id"`
}

// CodeChangeStatus is the identity-only projection carried by the small session
// summary. It never includes units, edges, or declaration populations.
type CodeChangeStatus struct {
	RepositoryID         string              `json:"repository_id"`
	CheckoutID           string              `json:"checkout_id"`
	State                string              `json:"state"`
	Reason               string              `json:"reason,omitempty"`
	Baseline             *CodeChangeBoundary `json:"baseline,omitempty"`
	Current              *CodeChangeBoundary `json:"current,omitempty"`
	FunctionCallCoverage string              `json:"function_call_coverage"`
}

// CodeStringFacts is an exact or explicitly truncated distinct string population.
type CodeStringFacts struct {
	Total     int      `json:"total"`
	Returned  int      `json:"returned"`
	Truncated bool     `json:"truncated"`
	Exact     bool     `json:"exact"`
	Values    []string `json:"values"`
}

// CodeCallerFacts contains only persisted, analyzer-qualified incoming call edges.
// Exact is false whenever analyzer coverage or the bounded declaration/edge reads are
// incomplete.
type CodeCallerFacts struct {
	Boundary            string                        `json:"boundary"`
	State               string                        `json:"state"`
	Reason              string                        `json:"reason,omitempty"`
	Total               int                           `json:"total"`
	Returned            int                           `json:"returned"`
	Truncated           bool                          `json:"truncated"`
	Exact               bool                          `json:"exact"`
	DeclarationTotal    int                           `json:"declaration_total"`
	DeclarationReturned int                           `json:"declaration_returned"`
	Edges               []store.UnderstandingEdge     `json:"edges"`
	Coverage            []store.UnderstandingCoverage `json:"coverage"`
}

// CodeDependencyFacts keeps package dependency and symbol-call evidence separate.
// Calls are never inferred from package fan-in or declaration names.
type CodeDependencyFacts struct {
	State                string          `json:"state"`
	Reason               string          `json:"reason,omitempty"`
	DependentPackages    CodeStringFacts `json:"dependent_packages"`
	ReferencingFiles     CodeStringFacts `json:"referencing_files"`
	FunctionCallCoverage string          `json:"function_call_coverage"`
	Callers              CodeCallerFacts `json:"callers"`
}

// CodeChangeFileDetail joins one file's declaration, structural, dependency, and
// observed-action attribution facts at immutable repository boundaries.
type CodeChangeFileDetail struct {
	ID                  string                         `json:"id"`
	RepositoryID        string                         `json:"repository_id"`
	CheckoutID          string                         `json:"checkout_id"`
	State               string                         `json:"state"`
	Reason              string                         `json:"reason,omitempty"`
	Baseline            *CodeChangeBoundary            `json:"baseline,omitempty"`
	Current             *CodeChangeBoundary            `json:"current,omitempty"`
	File                CodeFileChange                 `json:"file"`
	StructuralMatches   codemap.StructuralMatchChanges `json:"structural_matches"`
	Dependencies        CodeDependencyFacts            `json:"dependencies"`
	Attribution         []store.PathReconciliation     `json:"attribution"`
	AttributionCoverage CodeAttributionCoverage        `json:"attribution_coverage"`
}

type codeChangeContext struct {
	available     []store.SessionUnderstandingCheckout
	checkout      store.SessionUnderstandingCheckout
	baseline      store.UnderstandingCheckpoint
	baselineFound bool
	baselineGen   store.UnderstandingGeneration
	baselineExact bool
	current       store.UnderstandingCheckpoint
	currentGen    store.UnderstandingGeneration
	currentExact  bool
	state         string
	reason        string
}

// CodeChangeStatuses performs identity-only reads for the small session summary.
func CodeChangeStatuses(ix *store.Index, sessionID, analyzerBundle string) ([]CodeChangeStatus, error) {
	checkouts, err := ix.SessionUnderstandingCheckouts(sessionID, 100)
	if err != nil {
		return nil, err
	}
	out := make([]CodeChangeStatus, 0, len(checkouts))
	for _, checkout := range checkouts {
		ctx, loadErr := loadCodeChangeContext(ix, sessionID, CodeChangeOptions{
			RepositoryID: checkout.RepositoryID, CheckoutID: checkout.CheckoutID,
			AnalyzerBundleDigest: analyzerBundle,
		})
		if loadErr != nil {
			return nil, loadErr
		}
		response := contextResponse(sessionID, ctx)
		if ctx.currentExact && ctx.currentGen.Status == "complete" {
			coverage, coverageErr := ix.UnderstandingCoverage(ctx.currentGen.ID)
			if coverageErr != nil {
				return nil, coverageErr
			}
			response.FunctionCallCoverage, _ = coverageState(coverage, string(codemap.CapabilitySymbolCall))
		}
		out = append(out, CodeChangeStatus{RepositoryID: response.RepositoryID,
			CheckoutID: response.CheckoutID, State: response.State, Reason: response.Reason,
			Baseline: response.Baseline, Current: response.Current,
			FunctionCallCoverage: response.FunctionCallCoverage})
	}
	return out, nil
}

// BuildCodeChanges compares only selected descriptor paths. HTTP callers cannot cause
// analysis; a missing current generation is returned as state.
func BuildCodeChanges(ix *store.Index, sessionID string, options CodeChangeOptions) (CodeChanges, error) {
	ctx, err := loadCodeChangeContext(ix, sessionID, options)
	if err != nil {
		return CodeChanges{}, err
	}
	out := contextResponse(sessionID, ctx)
	if !ctx.currentExact || ctx.currentGen.Status != "complete" {
		return out, nil
	}
	if err := loadCodeChangeCoverage(ix, ctx, &out); err != nil {
		return CodeChanges{}, err
	}
	paths, err := codeChangePathPopulation(ix, ctx, &out)
	if err != nil {
		return CodeChanges{}, err
	}
	out.Aggregates, err = buildCodeChangeAggregates(ix, ctx, paths)
	if err != nil {
		return CodeChanges{}, err
	}
	limit := options.Limit
	if limit <= 0 || limit > maxCodeChangeFiles {
		limit = maxCodeChangeFiles
	}
	selected, filePage := slice(paths, options.Offset, limit)
	out.FilePage = filePage
	out.Files, err = projectCodeChangeRows(ix, ctx, selected)
	if err != nil {
		return CodeChanges{}, err
	}
	out.Attribution, err = attributionCoverage(ix, sessionID, selected)
	return out, err
}

func loadCodeChangeCoverage(ix *store.Index, ctx codeChangeContext, out *CodeChanges) error {
	var err error
	out.CurrentCoverage, err = ix.UnderstandingCoverage(ctx.currentGen.ID)
	if err != nil {
		return err
	}
	out.FunctionCallCoverage, _ = coverageState(out.CurrentCoverage, string(codemap.CapabilitySymbolCall))
	if ctx.baselineExact {
		out.BaselineCoverage, err = ix.UnderstandingCoverage(ctx.baselineGen.ID)
	}
	return err
}

func codeChangePathPopulation(ix *store.Index, ctx codeChangeContext, out *CodeChanges) ([]string, error) {
	currentIndex, err := ix.UnderstandingUnitIndex(ctx.currentGen.ID)
	if err != nil {
		return nil, err
	}
	currentByPath := indexUnits(currentIndex)
	out.CurrentAnalyzedUnits = len(currentIndex)
	out.CurrentCheckpointPathTotal = uniqueChangePathCount(ctx.current.Change.Items)
	paths := make([]string, 0, len(currentByPath))
	if ctx.baselineExact {
		baselineIndex, indexErr := ix.UnderstandingUnitIndex(ctx.baselineGen.ID)
		if indexErr != nil {
			return nil, indexErr
		}
		baselineByPath := indexUnits(baselineIndex)
		out.BaselineAnalyzedUnits = len(baselineIndex)
		seen := map[string]bool{}
		for path, before := range baselineByPath {
			seen[path] = true
			if after, found := currentByPath[path]; !found || before.SourceHash != after.SourceHash {
				paths = append(paths, path)
			}
		}
		for path := range currentByPath {
			if !seen[path] {
				paths = append(paths, path)
			}
		}
	} else {
		out.Population = "current_analyzed_units"
		for path := range currentByPath {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func projectCodeChangeRows(ix *store.Index, ctx codeChangeContext, paths []string) ([]CodeFileChange, error) {
	beforeUnits := []store.UnderstandingUnit{}
	var err error
	if ctx.baselineExact {
		beforeUnits, err = ix.UnderstandingUnitsForPaths(ctx.baselineGen.ID, paths)
		if err != nil {
			return nil, err
		}
	}
	afterUnits, err := ix.UnderstandingUnitsForPaths(ctx.currentGen.ID, paths)
	if err != nil {
		return nil, err
	}
	beforeDescriptors, err := decodeUnits(beforeUnits)
	if err != nil {
		return nil, err
	}
	afterDescriptors, err := decodeUnits(afterUnits)
	if err != nil {
		return nil, err
	}
	rows := make([]CodeFileChange, 0, len(paths))
	for _, path := range paths {
		row, rowErr := projectCodeFile(path, beforeDescriptors[path], afterDescriptors[path], ctx.baselineExact)
		if rowErr != nil {
			return nil, rowErr
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// BuildCodeChangeFile returns one centered comparison. It is the only read which may
// load the bounded unit population needed for an O(units) structural set difference.
func BuildCodeChangeFile(ix *store.Index, sessionID, path string, options CodeChangeOptions,
	structuralOffset, structuralLimit int) (CodeChangeFileDetail, error) {
	if !validCodePath(path) {
		return CodeChangeFileDetail{}, ErrInvalidFileView
	}
	ctx, err := loadCodeChangeContext(ix, sessionID, options)
	if err != nil {
		return CodeChangeFileDetail{}, err
	}
	out := emptyCodeChangeFileDetail(sessionID, ctx)
	if !ctx.currentExact || ctx.currentGen.Status != "complete" {
		return out, nil
	}
	beforeDescriptors, afterDescriptors, err := loadCodeFileDescriptors(ix, ctx, path)
	if err != nil {
		return CodeChangeFileDetail{}, err
	}
	out.File, err = projectCodeFile(path, beforeDescriptors[path], afterDescriptors[path], ctx.baselineExact)
	if err != nil {
		return CodeChangeFileDetail{}, err
	}
	currentCoverage, err := ix.UnderstandingCoverage(ctx.currentGen.ID)
	if err != nil {
		return CodeChangeFileDetail{}, err
	}
	out.Dependencies, err = codeDependencyFacts(ix, ctx.currentGen.ID, path, afterDescriptors[path],
		callerDeclarationIdentities(out.File, afterDescriptors[path]), currentCoverage)
	if err != nil {
		return CodeChangeFileDetail{}, err
	}
	if err := loadCodeFileAttribution(ix, sessionID, path, &out); err != nil {
		return CodeChangeFileDetail{}, err
	}
	if ctx.baselineExact {
		out.StructuralMatches, err = codeFileStructuralMatches(ix, ctx, path, structuralOffset, structuralLimit)
		if err != nil {
			return CodeChangeFileDetail{}, err
		}
	}
	return out, nil
}

func emptyCodeChangeFileDetail(sessionID string, ctx codeChangeContext) CodeChangeFileDetail {
	out := CodeChangeFileDetail{ID: sessionID, RepositoryID: ctx.checkout.RepositoryID,
		CheckoutID: ctx.checkout.CheckoutID, State: ctx.state, Reason: ctx.reason,
		StructuralMatches: codemap.StructuralMatchChanges{State: "unavailable", Reason: "exact baseline and current analysis required", Rows: []codemap.StructuralMatchChange{}},
		Dependencies:      unavailableCodeDependencies("exact current analysis required"),
		Attribution:       []store.PathReconciliation{}, AttributionCoverage: CodeAttributionCoverage{Classes: map[string]int{}}}
	if ctx.current.Checkpoint.ID != 0 {
		out.Current = boundaryFact(ctx.current, ctx.currentGen)
	}
	if ctx.baseline.Checkpoint.ID != 0 {
		out.Baseline = boundaryFact(ctx.baseline, ctx.baselineGen)
	}
	return out
}

func loadCodeFileDescriptors(ix *store.Index, ctx codeChangeContext, path string) (map[string]*codemap.UnitDescriptor,
	map[string]*codemap.UnitDescriptor, error) {
	afterUnits, err := ix.UnderstandingUnitsForPaths(ctx.currentGen.ID, []string{path})
	if err != nil {
		return nil, nil, err
	}
	beforeUnits := []store.UnderstandingUnit{}
	if ctx.baselineExact {
		beforeUnits, err = ix.UnderstandingUnitsForPaths(ctx.baselineGen.ID, []string{path})
		if err != nil {
			return nil, nil, err
		}
	}
	if len(beforeUnits) == 0 && len(afterUnits) == 0 {
		return nil, nil, sql.ErrNoRows
	}
	beforeDescriptors, err := decodeUnits(beforeUnits)
	if err != nil {
		return nil, nil, err
	}
	afterDescriptors, err := decodeUnits(afterUnits)
	if err != nil {
		return nil, nil, err
	}
	return beforeDescriptors, afterDescriptors, nil
}

func loadCodeFileAttribution(ix *store.Index, sessionID, path string, out *CodeChangeFileDetail) error {
	var err error
	out.Attribution, _, err = ix.PathReconciliationsForFile(sessionID, []string{path}, 200)
	if err != nil {
		return err
	}
	out.AttributionCoverage, err = attributionCoverage(ix, sessionID, []string{path})
	return err
}

func codeFileStructuralMatches(ix *store.Index, ctx codeChangeContext, path string,
	offset, limit int) (codemap.StructuralMatchChanges, error) {
	beforePopulation, err := loadGenerationUnits(ix, ctx.baselineGen.ID)
	if err != nil {
		return codemap.StructuralMatchChanges{}, err
	}
	afterPopulation, err := loadGenerationUnits(ix, ctx.currentGen.ID)
	if err != nil {
		return codemap.StructuralMatchChanges{}, err
	}
	beforeCoverage, err := ix.UnderstandingCoverage(ctx.baselineGen.ID)
	if err != nil {
		return codemap.StructuralMatchChanges{}, err
	}
	afterCoverage, err := ix.UnderstandingCoverage(ctx.currentGen.ID)
	if err != nil {
		return codemap.StructuralMatchChanges{}, err
	}
	return codemap.CompareStructuralMatches(path,
		storedResponsibilityUnits(beforePopulation), storedResponsibilityUnits(afterPopulation),
		responsibilityCoverage(beforeCoverage), responsibilityCoverage(afterCoverage), offset, limit), nil
}

func storedResponsibilityUnits(units []store.UnderstandingUnit) []codemap.StoredResponsibilityUnit {
	out := make([]codemap.StoredResponsibilityUnit, 0, len(units))
	for _, unit := range units {
		out = append(out, codemap.StoredResponsibilityUnit{Path: unit.Path, DescriptorJSON: unit.DescriptorJSON})
	}
	return out
}

func responsibilityCoverage(facts []store.UnderstandingCoverage) []codemap.ResponsibilityCoverageFact {
	out := make([]codemap.ResponsibilityCoverageFact, 0, len(facts))
	for _, fact := range facts {
		out = append(out, codemap.ResponsibilityCoverageFact{Family: fact.Family, State: fact.State,
			AnalyzerID: fact.AnalyzerID, Reason: fact.Reason})
	}
	return out
}

func unavailableCodeDependencies(reason string) CodeDependencyFacts {
	return CodeDependencyFacts{State: "unavailable", Reason: reason,
		DependentPackages: CodeStringFacts{Values: []string{}},
		ReferencingFiles:  CodeStringFacts{Values: []string{}}, FunctionCallCoverage: "unavailable",
		Callers: CodeCallerFacts{State: "unavailable", Reason: reason, Edges: []store.UnderstandingEdge{},
			Coverage: []store.UnderstandingCoverage{}}}
}

func codeDependencyFacts(ix *store.Index, generationID int64, path string,
	descriptor *codemap.UnitDescriptor, callerIdentities []string,
	coverage []store.UnderstandingCoverage) (CodeDependencyFacts, error) {
	out := unavailableCodeDependencies("")
	analyzerID := ""
	if descriptor != nil {
		analyzerID = descriptor.Identity.AnalyzerID
	}
	out.State, out.Reason = coverageStateForAnalyzer(coverage, string(codemap.CapabilityPackageDependency), analyzerID)
	if out.State == "unsupported" || out.State == "failed" || out.State == "unavailable" {
		callers, err := codeCallerFacts(ix, generationID, descriptor, callerIdentities, coverage)
		out.Callers, out.FunctionCallCoverage = callers, callers.State
		return out, err
	}
	fileEdges, fileTotal, err := ix.UnderstandingEdges(generationID, "file", path, 0, maxDependencyFacts)
	if err != nil {
		return out, err
	}
	exact := fileTotal == len(fileEdges)
	packages := map[string]bool{}
	for _, edge := range fileEdges {
		if edge.Relation == "file_in_package" && edge.FromKind == "file" && edge.FromRef == path {
			packages[edge.ToRef] = true
		}
	}
	dependent := map[string]bool{}
	referencing := map[string]bool{}
	for packageRef := range packages {
		edges, total, readErr := ix.UnderstandingEdges(generationID, "package", packageRef, 0, maxDependencyFacts)
		if readErr != nil {
			return out, readErr
		}
		exact = exact && total == len(edges)
		for _, edge := range edges {
			if edge.Relation == "package_depends_on" && edge.ToKind == "package" && edge.ToRef == packageRef {
				dependent[edge.FromRef] = true
				if edge.SourcePath != "" {
					referencing[edge.SourcePath] = true
				}
			}
		}
	}
	out.DependentPackages = stringFacts(dependent, exact)
	out.ReferencingFiles = stringFacts(referencing, exact)
	if !exact {
		out.State = "partial"
		out.Reason = "dependency edge traversal reached its bounded read limit"
	}
	out.Callers, err = codeCallerFacts(ix, generationID, descriptor, callerIdentities, coverage)
	out.FunctionCallCoverage = out.Callers.State
	if err != nil {
		return out, err
	}
	return out, nil
}

func codeCallerFacts(ix *store.Index, generationID int64, descriptor *codemap.UnitDescriptor,
	declarationIdentities []string, coverage []store.UnderstandingCoverage) (CodeCallerFacts, error) {
	out := CodeCallerFacts{Boundary: "current", State: "unavailable", Reason: "current changed declarations are unavailable",
		Edges: []store.UnderstandingEdge{}, Coverage: []store.UnderstandingCoverage{}}
	if descriptor == nil || descriptor.Identity.AnalyzerID == "" {
		return out, nil
	}
	for _, fact := range coverage {
		if fact.Family == string(codemap.CapabilitySymbolCall) && fact.AnalyzerID == descriptor.Identity.AnalyzerID {
			out.Coverage = append(out.Coverage, fact)
		}
	}
	out.State, out.Reason = coverageStateForAnalyzer(coverage, string(codemap.CapabilitySymbolCall), descriptor.Identity.AnalyzerID)
	declarationRefs := make([]string, 0, len(declarationIdentities))
	for _, identity := range declarationIdentities {
		if identity != "" {
			declarationRefs = append(declarationRefs, descriptor.Identity.AnalyzerID+"::"+identity)
		}
	}
	sort.Strings(declarationRefs)
	out.DeclarationTotal = len(declarationRefs)
	if out.State == "unsupported" || out.State == "failed" || out.State == "unavailable" {
		return out, nil
	}
	selectedRefs := declarationRefs
	if len(selectedRefs) > maxDependencyFacts {
		selectedRefs = selectedRefs[:maxDependencyFacts]
	}
	out.DeclarationReturned = len(selectedRefs)
	edges, total, err := ix.UnderstandingCallers(generationID, selectedRefs, 0, maxDependencyFacts)
	if err != nil {
		return out, err
	}
	out.Total, out.Returned, out.Edges = total, len(edges), edges
	out.Exact = out.State == "exact" && out.DeclarationReturned == out.DeclarationTotal && out.Returned == out.Total
	out.Truncated = out.DeclarationReturned < out.DeclarationTotal || out.Returned < out.Total
	if out.State == "exact" && !out.Exact {
		out.State = "partial"
		switch {
		case out.DeclarationReturned < out.DeclarationTotal:
			out.Reason = "caller lookup reached the bounded declaration limit"
		case out.Returned < out.Total:
			out.Reason = "caller lookup reached the bounded edge limit"
		}
	}
	return out, nil
}

func callerDeclarationIdentities(file CodeFileChange, descriptor *codemap.UnitDescriptor) []string {
	if descriptor == nil {
		return []string{}
	}
	if file.State == "current_only" {
		out := make([]string, 0, len(descriptor.Symbol.Declarations))
		for _, declaration := range descriptor.Symbol.Declarations {
			out = append(out, declaration.Identity)
		}
		return out
	}
	out := make([]string, 0, len(file.DeclarationChanges.Changes))
	for _, declaration := range file.DeclarationChanges.Changes {
		if declaration.State != "removed" {
			out = append(out, declaration.Identity)
		}
	}
	return out
}

func buildCodeChangeAggregates(ix *store.Index, ctx codeChangeContext, paths []string) (CodeChangeAggregates, error) {
	out := emptyCodeChangeAggregates(paths)
	if !ctx.baselineExact || !ctx.currentExact {
		return out, nil
	}
	selected := paths
	out.State, out.Reason = "exact", ""
	if len(selected) > maxDependencyFacts {
		selected = selected[:maxDependencyFacts]
		out.State = "partial"
		out.Reason = "aggregate descriptor read reached the bounded changed-file limit"
	}
	out.FileReturned = len(selected)
	out.StructuralCenterReturned = len(selected)
	before, after, err := loadAggregateDescriptors(ix, ctx, selected)
	if err != nil {
		return CodeChangeAggregates{}, err
	}
	measurements, err := measureAggregateCodeChanges(&out, selected, before, after)
	if err != nil {
		return CodeChangeAggregates{}, err
	}
	currentCoverage, err := ix.UnderstandingCoverage(ctx.currentGen.ID)
	if err != nil {
		return CodeChangeAggregates{}, err
	}
	out.DependencyState, out.DependencyReason, out.DependentPackages, out.ReferencingFiles, err =
		aggregateDependencyFacts(ix, ctx.currentGen.ID, measurements.currentPaths, measurements.dependencyAnalyzers,
			currentCoverage, len(selected) == len(paths))
	if err != nil {
		return CodeChangeAggregates{}, err
	}
	out.Callers, err = aggregateCallerFacts(ix, ctx.currentGen.ID, measurements.callerRefs,
		measurements.callerAnalyzers, currentCoverage)
	if err != nil {
		return CodeChangeAggregates{}, err
	}
	out.StructuralMatches, err = aggregateStructuralMatches(ix, ctx, selected, currentCoverage)
	if err != nil {
		return CodeChangeAggregates{}, err
	}
	if len(selected) < len(paths) && out.StructuralMatches.State == "exact" {
		out.StructuralMatches.State = "partial"
		out.StructuralMatches.Reason = "structural match population reached the bounded changed-file limit"
	}
	return out, nil
}

func emptyCodeChangeAggregates(paths []string) CodeChangeAggregates {
	return CodeChangeAggregates{State: "baseline_unavailable", Reason: "exact baseline and current analysis required",
		FileTotal: len(paths), Callers: CodeCallerFacts{Boundary: "current", State: "baseline_unavailable",
			Reason: "exact baseline and current analysis required", Edges: []store.UnderstandingEdge{},
			Coverage: []store.UnderstandingCoverage{}}, DependencyState: "baseline_unavailable",
		DependentPackages: CodeStringFacts{Values: []string{}}, ReferencingFiles: CodeStringFacts{Values: []string{}},
		StructuralCenterTotal: len(paths),
		StructuralMatches: codemap.StructuralMatchPopulationChanges{State: "baseline_unavailable",
			Reason: "exact baseline and current analysis required", Rows: []codemap.StructuralMatchPopulationRow{}}}
}

func loadAggregateDescriptors(ix *store.Index, ctx codeChangeContext, paths []string) (map[string]*codemap.UnitDescriptor,
	map[string]*codemap.UnitDescriptor, error) {
	beforeUnits, err := ix.UnderstandingUnitsForPaths(ctx.baselineGen.ID, paths)
	if err != nil {
		return nil, nil, err
	}
	afterUnits, err := ix.UnderstandingUnitsForPaths(ctx.currentGen.ID, paths)
	if err != nil {
		return nil, nil, err
	}
	before, err := decodeUnits(beforeUnits)
	if err != nil {
		return nil, nil, err
	}
	after, err := decodeUnits(afterUnits)
	if err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

type aggregateCodeMeasurements struct {
	callerRefs          map[string]bool
	callerAnalyzers     map[string]bool
	currentPaths        []string
	dependencyAnalyzers map[string]bool
}

func measureAggregateCodeChanges(out *CodeChangeAggregates, paths []string, before,
	after map[string]*codemap.UnitDescriptor) (aggregateCodeMeasurements, error) {
	measurements := aggregateCodeMeasurements{callerRefs: map[string]bool{}, callerAnalyzers: map[string]bool{},
		currentPaths: []string{}, dependencyAnalyzers: map[string]bool{}}
	for _, path := range paths {
		change, compareErr := codemap.CompareUnits(before[path], after[path])
		if compareErr != nil {
			return aggregateCodeMeasurements{}, compareErr
		}
		if after[path] != nil {
			measurements.currentPaths = append(measurements.currentPaths, path)
			measurements.dependencyAnalyzers[after[path].Identity.AnalyzerID] = true
		}
		for _, declaration := range change.Declarations {
			if declaration.State == "unchanged" {
				continue
			}
			out.DeclarationChanges++
			switch declaration.State {
			case "added":
				out.DeclarationsAdded++
			case "removed":
				out.DeclarationsRemoved++
			case "modified":
				out.DeclarationsModified++
			case "moved":
				out.DeclarationsMoved++
			}
			addIntegerDelta(&out.SourceSpanLines, declaration.SourceSpanLines.Delta)
			addIntegerDelta(&out.Cyclomatic, declaration.Cyclomatic.Delta)
			if declaration.State != "removed" && after[path] != nil && after[path].Identity.AnalyzerID != "" {
				measurements.callerRefs[after[path].Identity.AnalyzerID+"::"+declaration.Identity] = true
				measurements.callerAnalyzers[after[path].Identity.AnalyzerID] = true
			}
		}
	}
	return measurements, nil
}

func aggregateStructuralMatches(ix *store.Index, ctx codeChangeContext, paths []string,
	currentCoverage []store.UnderstandingCoverage) (codemap.StructuralMatchPopulationChanges, error) {
	beforePopulation, err := loadGenerationUnits(ix, ctx.baselineGen.ID)
	if err != nil {
		return codemap.StructuralMatchPopulationChanges{}, err
	}
	afterPopulation, err := loadGenerationUnits(ix, ctx.currentGen.ID)
	if err != nil {
		return codemap.StructuralMatchPopulationChanges{}, err
	}
	beforeCoverage, err := ix.UnderstandingCoverage(ctx.baselineGen.ID)
	if err != nil {
		return codemap.StructuralMatchPopulationChanges{}, err
	}
	return codemap.CompareStructuralMatchPopulation(paths,
		storedResponsibilityUnits(beforePopulation), storedResponsibilityUnits(afterPopulation),
		responsibilityCoverage(beforeCoverage), responsibilityCoverage(currentCoverage), 0, maxDependencyFacts), nil
}

func aggregateDependencyFacts(ix *store.Index, generationID int64, paths []string, analyzers map[string]bool,
	coverage []store.UnderstandingCoverage, completePaths bool) (string, string, CodeStringFacts, CodeStringFacts, error) {
	empty := CodeStringFacts{Values: []string{}}
	if len(paths) == 0 {
		return "not_applicable", "no current changed files", empty, empty, nil
	}
	state, reason := coverageStateForAnalyzers(coverage, string(codemap.CapabilityPackageDependency), analyzers)
	if state == "unsupported" || state == "failed" || state == "unavailable" {
		return state, reason, empty, empty, nil
	}
	values, err := ix.UnderstandingDependentPackageValues(generationID, paths, maxDependencyFacts)
	if err != nil {
		return "", "", empty, empty, err
	}
	valuesComplete := len(values.DependentPackages) == values.DependentPackageTotal &&
		len(values.ReferencingFiles) == values.ReferencingFileTotal
	exact := state == "exact" && completePaths && valuesComplete
	if !exact && state == "exact" {
		state = "partial"
		if !completePaths {
			reason = "dependency population reached the bounded changed-file limit"
		} else {
			reason = "dependency population reached the bounded value limit"
		}
	}
	return state, reason,
		CodeStringFacts{Total: values.DependentPackageTotal, Returned: len(values.DependentPackages),
			Truncated: len(values.DependentPackages) < values.DependentPackageTotal, Exact: exact,
			Values: values.DependentPackages},
		CodeStringFacts{Total: values.ReferencingFileTotal, Returned: len(values.ReferencingFiles),
			Truncated: len(values.ReferencingFiles) < values.ReferencingFileTotal, Exact: exact,
			Values: values.ReferencingFiles}, nil
}

func aggregateCallerFacts(ix *store.Index, generationID int64, refs, analyzers map[string]bool,
	coverage []store.UnderstandingCoverage) (CodeCallerFacts, error) {
	out := CodeCallerFacts{Boundary: "current", Edges: []store.UnderstandingEdge{}, Coverage: []store.UnderstandingCoverage{}}
	allRefs := make([]string, 0, len(refs))
	for ref := range refs {
		allRefs = append(allRefs, ref)
	}
	sort.Strings(allRefs)
	out.DeclarationTotal = len(allRefs)
	if len(allRefs) == 0 {
		out.State, out.Reason, out.Exact = "not_applicable", "no current changed declaration identities", true
		return out, nil
	}
	for _, fact := range coverage {
		if fact.Family == string(codemap.CapabilitySymbolCall) && analyzers[fact.AnalyzerID] {
			out.Coverage = append(out.Coverage, fact)
		}
	}
	out.State, out.Reason = coverageStateForAnalyzers(coverage, string(codemap.CapabilitySymbolCall), analyzers)
	if out.State == "unsupported" || out.State == "failed" || out.State == "unavailable" {
		return out, nil
	}
	selected := allRefs
	if len(selected) > maxDependencyFacts {
		selected = selected[:maxDependencyFacts]
	}
	out.DeclarationReturned = len(selected)
	edges, total, err := ix.UnderstandingCallers(generationID, selected, 0, maxDependencyFacts)
	if err != nil {
		return CodeCallerFacts{}, err
	}
	out.Total, out.Returned, out.Edges = total, len(edges), edges
	out.Exact = out.State == "exact" && out.DeclarationReturned == out.DeclarationTotal && out.Returned == out.Total
	out.Truncated = out.DeclarationReturned < out.DeclarationTotal || out.Returned < out.Total
	if out.State == "exact" && !out.Exact {
		out.State = "partial"
		if out.DeclarationReturned < out.DeclarationTotal {
			out.Reason = "caller lookup reached the bounded declaration limit"
		} else {
			out.Reason = "caller lookup reached the bounded edge limit"
		}
	}
	return out, nil
}

func coverageStateForAnalyzers(coverage []store.UnderstandingCoverage, family string,
	analyzers map[string]bool) (string, string) {
	filtered := make([]store.UnderstandingCoverage, 0, len(coverage))
	seen := map[string]bool{}
	for _, fact := range coverage {
		if fact.Family == family && analyzers[fact.AnalyzerID] {
			filtered = append(filtered, fact)
			seen[fact.AnalyzerID] = true
		}
	}
	if len(seen) == 0 {
		return "unavailable", "coverage fact is missing for the selected analyzers"
	}
	if len(seen) != len(analyzers) {
		return "partial", "coverage fact is missing for one or more selected analyzers"
	}
	return coverageStateForAnalyzer(filtered, family, "")
}

func addIntegerDelta(population *CodeIntegerDeltaPopulation, delta *int) {
	if delta == nil {
		return
	}
	population.Measured++
	population.Delta += *delta
	if *delta > 0 {
		population.Increased++
	} else if *delta < 0 {
		population.Decreased++
	}
}

func coverageState(coverage []store.UnderstandingCoverage, family string) (string, string) {
	return coverageStateForAnalyzer(coverage, family, "")
}

func coverageStateForAnalyzer(coverage []store.UnderstandingCoverage, family, analyzerID string) (string, string) {
	matches := make([]store.UnderstandingCoverage, 0, len(coverage))
	for _, fact := range coverage {
		if fact.Family != family || (analyzerID != "" && fact.AnalyzerID != analyzerID) {
			continue
		}
		matches = append(matches, fact)
	}
	if len(matches) == 0 {
		return "unavailable", "coverage fact is missing"
	}
	allComplete, allUnsupported, allFailed := true, true, true
	reasons := []string{}
	for _, fact := range matches {
		allComplete = allComplete && fact.State == "complete"
		allUnsupported = allUnsupported && fact.State == "unsupported"
		allFailed = allFailed && fact.State == "failed"
		if fact.Reason != "" {
			reasons = append(reasons, fact.Reason)
		}
	}
	if allComplete {
		return "exact", ""
	}
	reason := strings.Join(reasons, "; ")
	if allUnsupported {
		return "unsupported", reason
	}
	if allFailed {
		return "failed", reason
	}
	if reason == "" {
		reason = "coverage differs across selected analyzers"
	}
	return "partial", reason
}

func stringFacts(values map[string]bool, exact bool) CodeStringFacts {
	all := make([]string, 0, len(values))
	for value := range values {
		all = append(all, value)
	}
	sort.Strings(all)
	returned := len(all)
	if returned > maxDependencyFacts {
		returned = maxDependencyFacts
		exact = false
	}
	return CodeStringFacts{Total: len(all), Returned: returned, Truncated: returned < len(all), Exact: exact,
		Values: append([]string(nil), all[:returned]...)}
}

func validCodePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(path))
	return cleaned == path && path != "." && path != ".." && !strings.HasPrefix(path, "../")
}

func loadCodeChangeContext(ix *store.Index, sessionID string, options CodeChangeOptions) (codeChangeContext, error) {
	if strings.TrimSpace(options.AnalyzerBundleDigest) == "" {
		return codeChangeContext{}, errors.New("analyzer bundle identity is required")
	}
	checkouts, err := ix.SessionUnderstandingCheckouts(sessionID, 100)
	if err != nil {
		return codeChangeContext{}, err
	}
	ctx := codeChangeContext{state: "unavailable", reason: "no completed checkout checkpoint was captured", available: checkouts}
	ctx.checkout, ctx.reason = selectCodeCheckout(checkouts, options)
	if ctx.checkout.CheckoutID == "" {
		return ctx, nil
	}
	if err := loadCodeChangeBoundaries(ix, sessionID, options, &ctx); err != nil {
		return codeChangeContext{}, err
	}
	if ctx.current.Checkpoint.ID == 0 {
		ctx.reason = "no completed current checkpoint was captured"
		return ctx, nil
	}
	ready, err := loadCurrentCodeAnalysis(ix, options.AnalyzerBundleDigest, &ctx)
	if err != nil || !ready {
		return ctx, err
	}
	ready, err = loadBaselineCodeAnalysis(ix, options.AnalyzerBundleDigest, &ctx)
	if err != nil || !ready {
		return ctx, err
	}
	ctx.state, ctx.reason = "exact", ""
	return ctx, nil
}

func selectCodeCheckout(checkouts []store.SessionUnderstandingCheckout, options CodeChangeOptions) (
	store.SessionUnderstandingCheckout, string) {
	selected := store.SessionUnderstandingCheckout{}
	for _, checkout := range checkouts {
		if options.CheckoutID != "" && checkout.CheckoutID != options.CheckoutID {
			continue
		}
		if options.RepositoryID != "" && checkout.RepositoryID != options.RepositoryID {
			continue
		}
		if selected.CheckoutID != "" {
			return store.SessionUnderstandingCheckout{}, "repository_id and checkout_id are required when a session has multiple checkouts"
		}
		selected = checkout
	}
	if selected.CheckoutID == "" {
		if options.CheckoutID != "" || options.RepositoryID != "" {
			return selected, "requested checkout was not captured for this session"
		}
		return selected, "no completed checkout checkpoint was captured"
	}
	return selected, ""
}

func loadCodeChangeBoundaries(ix *store.Index, sessionID string, options CodeChangeOptions,
	ctx *codeChangeContext) error {
	baseline, baselineFound, current, currentFound, boundaryErr := ix.SessionUnderstandingBoundaries(
		sessionID, ctx.checkout.RepositoryID, ctx.checkout.CheckoutID)
	if boundaryErr != nil {
		return boundaryErr
	}
	ctx.baselineFound = baselineFound
	if baselineFound {
		ctx.baseline.Checkpoint = baseline
	}
	if currentFound {
		ctx.current.Checkpoint = current
	}
	if options.CheckpointID != 0 {
		loaded, err := ix.UnderstandingCheckpointByID(options.CheckpointID)
		if err != nil {
			return err
		}
		ctx.current = loaded
		if ctx.current.Checkpoint.SessionID != sessionID ||
			ctx.current.Change.RepositoryID != ctx.checkout.RepositoryID ||
			ctx.current.Change.CheckoutID != ctx.checkout.CheckoutID {
			return sql.ErrNoRows
		}
	} else if ctx.current.Checkpoint.ID != 0 {
		loaded, err := ix.UnderstandingCheckpointByID(ctx.current.Checkpoint.ID)
		if err != nil {
			return err
		}
		ctx.current = loaded
	}
	return nil
}

func loadCurrentCodeAnalysis(ix *store.Index, analyzerBundle string, ctx *codeChangeContext) (bool, error) {
	var err error
	ctx.currentGen, ctx.currentExact, err = exactGeneration(ix, ctx.current.Change, analyzerBundle)
	if err != nil {
		return false, err
	}
	if !ctx.currentExact {
		ctx.reason = "the selected checkpoint has no exact current-analyzer generation"
		return false, nil
	}
	if ctx.currentGen.Status == "failed" {
		ctx.state = "failed"
		ctx.reason = strings.TrimSpace(ctx.currentGen.LimitationCode + ": " + ctx.currentGen.Limitation)
		return false, nil
	}
	return true, nil
}

func loadBaselineCodeAnalysis(ix *store.Index, analyzerBundle string, ctx *codeChangeContext) (bool, error) {
	if !ctx.baselineFound {
		ctx.state = "baseline_unavailable"
		ctx.reason = "no completed attachment or pre-mutation baseline was captured"
		return false, nil
	}
	baselineCheckpoint, loadErr := ix.UnderstandingCheckpointByID(ctx.baseline.Checkpoint.ID)
	if loadErr != nil {
		return false, loadErr
	}
	ctx.baseline = baselineCheckpoint
	var err error
	ctx.baselineGen, ctx.baselineExact, err = exactGeneration(ix, baselineCheckpoint.Change, analyzerBundle)
	if err != nil {
		return false, err
	}
	if !ctx.baselineExact || ctx.baselineGen.Status != "complete" {
		ctx.baselineExact = false
		ctx.state = "baseline_unavailable"
		ctx.reason = "the first observed baseline has no exact current-analyzer generation"
		return false, nil
	}
	return true, nil
}

func exactGeneration(ix *store.Index, change store.ChangeRecord, analyzerBundle string) (store.UnderstandingGeneration, bool, error) {
	return ix.UnderstandingForSnapshot(change.RepositoryID, change.CheckoutID,
		change.SnapshotDigest, codemap.StructuralSchema, analyzerBundle, "none", "")
}

func contextResponse(sessionID string, ctx codeChangeContext) CodeChanges {
	out := CodeChanges{ID: sessionID, Observation: "observed_checkout_delta", Population: "analyzed_unit_delta", State: ctx.state,
		Reason: ctx.reason, RepositoryID: ctx.checkout.RepositoryID, CheckoutID: ctx.checkout.CheckoutID,
		Files: []CodeFileChange{}, CurrentCoverage: []store.UnderstandingCoverage{},
		BaselineCoverage: []store.UnderstandingCoverage{}, FunctionCallCoverage: "unavailable",
		FilePage: page(0, 0, maxCodeChangeFiles), Attribution: CodeAttributionCoverage{Classes: map[string]int{}},
		Aggregates: CodeChangeAggregates{State: "unavailable", Reason: ctx.reason,
			Callers: CodeCallerFacts{Boundary: "current", State: "unavailable", Reason: ctx.reason,
				Edges: []store.UnderstandingEdge{}, Coverage: []store.UnderstandingCoverage{}},
			DependencyState: "unavailable", DependencyReason: ctx.reason,
			DependentPackages: CodeStringFacts{Values: []string{}}, ReferencingFiles: CodeStringFacts{Values: []string{}},
			StructuralMatches: codemap.StructuralMatchPopulationChanges{State: "unavailable", Reason: ctx.reason,
				Rows: []codemap.StructuralMatchPopulationRow{}}}}
	if ctx.checkout.CheckoutID == "" {
		for _, checkout := range ctx.available {
			out.AvailableCheckouts = append(out.AvailableCheckouts, CodeCheckoutIdentity{
				RepositoryID: checkout.RepositoryID, CheckoutID: checkout.CheckoutID})
		}
	}
	if ctx.current.Checkpoint.ID != 0 {
		out.Current = boundaryFact(ctx.current, ctx.currentGen)
	}
	if ctx.baseline.Checkpoint.ID != 0 {
		out.Baseline = boundaryFact(ctx.baseline, ctx.baselineGen)
	}
	return out
}

func uniqueChangePathCount(items []store.ChangeItem) int {
	paths := map[string]bool{}
	for _, item := range items {
		if item.Path != "" {
			paths[item.Path] = true
		}
		if item.OldPath != "" {
			paths[item.OldPath] = true
		}
	}
	return len(paths)
}

func boundaryFact(checkpoint store.UnderstandingCheckpoint, generation store.UnderstandingGeneration) *CodeChangeBoundary {
	return &CodeChangeBoundary{CheckpointID: checkpoint.Checkpoint.ID, Kind: checkpoint.Checkpoint.Kind,
		CapturedAt: checkpoint.Checkpoint.CaptureEndedAt, SnapshotDigest: checkpoint.Change.SnapshotDigest,
		GenerationID: generation.ID}
}

func indexUnits(units []store.UnderstandingUnit) map[string]store.UnderstandingUnit {
	out := make(map[string]store.UnderstandingUnit, len(units))
	for _, unit := range units {
		out[unit.Path] = unit
	}
	return out
}

func decodeUnits(units []store.UnderstandingUnit) (map[string]*codemap.UnitDescriptor, error) {
	out := make(map[string]*codemap.UnitDescriptor, len(units))
	for _, unit := range units {
		var descriptor codemap.UnitDescriptor
		if err := json.Unmarshal([]byte(unit.DescriptorJSON), &descriptor); err != nil {
			return nil, fmt.Errorf("decode understanding descriptor for %s: %w", unit.Path, err)
		}
		if descriptor.Identity.Path != unit.Path {
			return nil, fmt.Errorf("understanding descriptor path mismatch %q != %q", descriptor.Identity.Path, unit.Path)
		}
		descriptor.SourceHash = unit.SourceHash
		copy := descriptor
		out[unit.Path] = &copy
	}
	return out, nil
}

func projectCodeFile(path string, before, after *codemap.UnitDescriptor, baselineExact bool) (CodeFileChange, error) {
	row := CodeFileChange{Path: path, DeclarationChanges: CodeDeclarationFacts{Changes: []codemap.DeclarationChange{}},
		CurrentDeclarations: CurrentDeclarationFacts{Declarations: []codemap.Declaration{}}}
	if after != nil {
		row.Language, row.Namespace, row.AfterSourceHash = after.Identity.Language, after.Identity.Namespace, after.SourceHash
		row.CurrentDeclarations.Total = len(after.Symbol.Declarations)
		end := len(after.Symbol.Declarations)
		if end > maxDeclarationChangeFacts {
			end = maxDeclarationChangeFacts
		}
		row.CurrentDeclarations.Declarations = append(row.CurrentDeclarations.Declarations, after.Symbol.Declarations[:end]...)
		row.CurrentDeclarations.Returned = end
		row.CurrentDeclarations.Truncated = end < row.CurrentDeclarations.Total
	}
	if before != nil {
		row.BeforeSourceHash = before.SourceHash
		if row.Language == "" {
			row.Language, row.Namespace = before.Identity.Language, before.Identity.Namespace
		}
	}
	if !baselineExact {
		row.State = "current_only"
		return row, nil
	}
	change, err := codemap.CompareUnits(before, after)
	if err != nil {
		return CodeFileChange{}, err
	}
	row.State = change.State
	changedDeclarations := make([]codemap.DeclarationChange, 0, len(change.Declarations))
	for _, declaration := range change.Declarations {
		if declaration.State != "unchanged" {
			changedDeclarations = append(changedDeclarations, declaration)
		}
	}
	row.CurrentDeclarations = CurrentDeclarationFacts{Declarations: []codemap.Declaration{}}
	row.DeclarationChanges.Total = len(changedDeclarations)
	end := len(changedDeclarations)
	if end > maxDeclarationChangeFacts {
		end = maxDeclarationChangeFacts
	}
	row.DeclarationChanges.Changes = append(row.DeclarationChanges.Changes, changedDeclarations[:end]...)
	row.DeclarationChanges.Returned = end
	row.DeclarationChanges.Truncated = end < row.DeclarationChanges.Total
	return row, nil
}

func attributionCoverage(ix *store.Index, sessionID string, paths []string) (CodeAttributionCoverage, error) {
	out := CodeAttributionCoverage{Classes: map[string]int{}}
	if len(paths) == 0 {
		out.Exact = true
		return out, nil
	}
	facts, total, err := ix.PathReconciliationsForFile(sessionID, paths, 200)
	if err != nil {
		return out, err
	}
	out.Total, out.Returned, out.Exact = total, len(facts), total == len(facts)
	for _, fact := range facts {
		out.Classes[fact.Classification]++
		switch fact.Classification {
		case "native-effect-git-matched":
			out.ExactMatched++
		case "ambiguous":
			out.Ambiguous++
		case "unattributed":
			out.Unattributed++
		default:
			out.Other++
		}
	}
	return out, nil
}
