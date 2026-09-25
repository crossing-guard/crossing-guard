package understanding

// Package understanding owns one lifecycle only: assemble and atomically append
// an immutable source/analyzer/configuration generation from existing producers.
// It owns no Git commands, analyzer, reference parser, SQL, policy, or UI view.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"crossing-guard/codemap"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/refindex"
	"crossing-guard/store"
)

type ScanInput struct {
	RepoDir        string
	Base           string
	Conventions    string
	Expected       *SnapshotExpectation
	Now            func() time.Time
	BetweenCapture func(attempt int) // tests only; nil in production
	Assembly       *codemap.AnalyzerAssembly
}

// SnapshotExpectation is the complete immutable checkpoint identity an automatic scan
// is allowed to analyze. Manual scans leave it nil and retain the bounded retry path.
type SnapshotExpectation struct {
	RepositoryID     string
	CheckoutID       string
	CheckoutRoot     string
	SnapshotProtocol string
	SnapshotDigest   string
	BaseRevision     string
	HeadRevision     string
}

type ScanResult struct {
	Generation *store.UnderstandingGeneration
	Recorded   bool
	Outcome    string
}

// ErrSnapshotSuperseded means the checkout no longer matches the immutable checkpoint
// offered to an automatic scan. It is normal coalescing, not an analyzer failure.
var ErrSnapshotSuperseded = errors.New("understanding snapshot superseded")

func digestText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256-v1:" + hex.EncodeToString(sum[:])
}

func appendFailed(ix *store.Index, repository changeenv.Repository, input ScanInput, observed *changeenv.GitCapture, startedAt int64, conventionState, conventionRef, conventionDigest, code string, cause error) (ScanResult, error) {
	now := time.Now
	if input.Now != nil {
		now = input.Now
	}
	generation := &store.UnderstandingGeneration{
		RepositoryID: repository.ID, CheckoutID: repository.CheckoutID, CheckoutRoot: repository.Root,
		Status: "failed", ConventionState: conventionState, ConventionSourceRef: conventionRef,
		ConventionSourceDigest: conventionDigest, StructuralSchema: codemap.StructuralSchema,
		AnalyzerBundleDigest: input.Assembly.Digest(), StartedAt: startedAt, EndedAt: now().UnixNano(),
		LimitationCode: code, Limitation: cause.Error(),
	}
	if observed != nil {
		generation.SnapshotProtocol = changeenv.GitTreeProtocol
		generation.SnapshotDigest = observed.SnapshotDigest
		generation.BaseRevision = observed.Base
		generation.HeadRevision = observed.Head
	} else if input.Expected != nil {
		generation.SnapshotProtocol = input.Expected.SnapshotProtocol
		generation.SnapshotDigest = input.Expected.SnapshotDigest
		generation.BaseRevision = input.Expected.BaseRevision
		generation.HeadRevision = input.Expected.HeadRevision
	}
	if err := ix.AppendUnderstanding(generation); err != nil {
		return ScanResult{Generation: generation, Outcome: "failed"}, fmt.Errorf("%s; failed attempt NOT RECORDED: %w", cause, err)
	}
	return ScanResult{Generation: generation, Recorded: true, Outcome: "failed"}, cause
}

// Scan runs at most three full A/analyze/B attempts and appends only a stable
// complete generation. Producer failures are recorded as failed attempts when
// the store remains writable.
func Scan(ctx context.Context, ix *store.Index, input ScanInput) (ScanResult, error) {
	if ix == nil || strings.TrimSpace(input.RepoDir) == "" || strings.TrimSpace(input.Base) == "" {
		return ScanResult{}, fmt.Errorf("understanding scan requires store, repo, and base")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now
	if input.Now != nil {
		now = input.Now
	}
	assembly := input.Assembly
	if assembly == nil {
		var err error
		assembly, err = codemap.RegisteredAnalyzerAssembly()
		if err != nil {
			return ScanResult{}, fmt.Errorf("assemble analyzers: %w", err)
		}
		input.Assembly = assembly
	}
	if err := validateExpectedSnapshot(input); err != nil {
		return ScanResult{Outcome: "failed"}, err
	}
	repository, err := changeenv.ResolveRepository(input.RepoDir)
	if err != nil {
		if input.Expected != nil {
			expectedRepository := changeenv.Repository{ID: input.Expected.RepositoryID,
				CheckoutID: input.Expected.CheckoutID, Root: input.Expected.CheckoutRoot}
			return appendFailed(ix, expectedRepository, input, nil, now().UnixNano(),
				"none", "", "", "repository_resolution_failed", err)
		}
		return ScanResult{}, err
	}
	if err := validateExpectation(repository, input); err != nil {
		return ScanResult{Outcome: "superseded"}, err
	}
	startedAt := now().UnixNano()
	conventionState, conventionRef, conventionDigest := "none", "", ""
	var conventions *codemap.Config
	if strings.TrimSpace(input.Conventions) != "" {
		conventionState = "explicit"
		source, sourceErr := changeenv.SourceFile(repository, input.Conventions)
		if sourceErr != nil {
			return appendFailed(ix, repository, input, nil, startedAt, conventionState, "unavailable", "sha256-v1:unavailable", "convention_source_invalid", sourceErr)
		}
		conventionRef, conventionDigest = source.Ref, source.Digest
		conventions, err = codemap.LoadConfig(input.Conventions)
		if err != nil {
			return appendFailed(ix, repository, input, nil, startedAt, conventionState, conventionRef, conventionDigest, "convention_invalid", err)
		}
	}
	bundleDigest := assembly.Digest()
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			if input.Expected != nil {
				return ScanResult{Outcome: "failed"}, err
			}
			return appendFailed(ix, repository, input, nil, startedAt, conventionState, conventionRef, conventionDigest, "interrupted", err)
		}
		before, err := changeenv.ObserveGit(ctx, repository, input.Base)
		if err != nil {
			if input.Expected != nil {
				return ScanResult{Outcome: "failed"}, err
			}
			code := "source_observation_failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				code = "interrupted"
			}
			return appendFailed(ix, repository, input, nil, startedAt, conventionState, conventionRef, conventionDigest, code, err)
		}
		if !matchesExpectation(before, input.Expected) {
			return ScanResult{Outcome: "superseded"}, fmt.Errorf("%w: checkpoint source changed before analysis", ErrSnapshotSuperseded)
		}
		paths := regularManifestPaths(before.Manifest)
		describer := codemap.NewDescriberWithAssembly(repository.Root, conventions, assembly)
		structure, err := describer.AnalyzeManifestSnapshotContext(ctx, paths, codemap.SnapshotIdentity{
			Protocol: changeenv.GitTreeProtocol, Digest: before.SnapshotDigest})
		if err != nil {
			return appendFailed(ix, repository, input, &before, startedAt, conventionState, conventionRef, conventionDigest, "structural_analysis_failed", err)
		}
		references, err := refindex.BuildManifest(repository.Root, paths)
		if err != nil {
			return appendFailed(ix, repository, input, &before, startedAt, conventionState, conventionRef, conventionDigest, "reference_analysis_failed", err)
		}
		if input.BetweenCapture != nil {
			input.BetweenCapture(attempt)
		}
		after, err := changeenv.ObserveGit(ctx, repository, input.Base)
		if err != nil {
			return appendFailed(ix, repository, input, &before, startedAt, conventionState, conventionRef, conventionDigest, "source_observation_failed", err)
		}
		if input.Expected != nil && !matchesExpectation(after, input.Expected) {
			return ScanResult{Outcome: "superseded"}, fmt.Errorf("%w: checkpoint source changed during analysis", ErrSnapshotSuperseded)
		}
		if !changeenv.SameGitSource(before, after) {
			continue
		}
		generation, err := assembleGeneration(repository, before, bundleDigest, conventionState, conventionRef, conventionDigest, startedAt, now().UnixNano(), structure, references)
		if err != nil {
			return appendFailed(ix, repository, input, &before, startedAt, conventionState, conventionRef, conventionDigest, "generation_encoding_failed", err)
		}
		if err := ix.AppendUnderstanding(generation); err != nil {
			return ScanResult{Generation: generation, Outcome: "failed"}, fmt.Errorf("complete understanding generation NOT RECORDED: %w", err)
		}
		return ScanResult{Generation: generation, Recorded: true, Outcome: "complete"}, nil
	}
	return appendFailed(ix, repository, input, nil, startedAt, conventionState, conventionRef, conventionDigest, "source_changed", fmt.Errorf("repository changed during analysis"))
}

func validateExpectation(repository changeenv.Repository, input ScanInput) error {
	if input.Expected == nil {
		return nil
	}
	expected := input.Expected
	if repository.ID != expected.RepositoryID || repository.CheckoutID != expected.CheckoutID ||
		repository.Root != expected.CheckoutRoot || input.Base != expected.BaseRevision {
		return fmt.Errorf("%w: checkpoint repository identity changed", ErrSnapshotSuperseded)
	}
	return nil
}

func validateExpectedSnapshot(input ScanInput) error {
	if input.Expected == nil {
		return nil
	}
	expected := input.Expected
	if expected.RepositoryID == "" || expected.CheckoutID == "" || expected.CheckoutRoot == "" ||
		expected.SnapshotProtocol != changeenv.GitTreeProtocol || expected.SnapshotDigest == "" ||
		expected.BaseRevision == "" || expected.HeadRevision == "" || input.Base != expected.BaseRevision {
		return fmt.Errorf("automatic understanding scan requires complete expected source identity")
	}
	return nil
}

func matchesExpectation(capture changeenv.GitCapture, expected *SnapshotExpectation) bool {
	if expected == nil {
		return true
	}
	return capture.Repository.ID == expected.RepositoryID &&
		capture.Repository.CheckoutID == expected.CheckoutID &&
		capture.Repository.Root == expected.CheckoutRoot &&
		capture.Base == expected.BaseRevision && capture.Head == expected.HeadRevision &&
		capture.SnapshotDigest == expected.SnapshotDigest
}

func regularManifestPaths(manifest []changeenv.GitManifestEntry) []string {
	paths := make([]string, 0, len(manifest))
	for _, entry := range manifest {
		if entry.Kind == "regular" && entry.Present {
			paths = append(paths, entry.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

func assembleGeneration(repository changeenv.Repository, capture changeenv.GitCapture, bundleDigest, conventionState, conventionRef, conventionDigest string, startedAt, endedAt int64, structure codemap.ManifestResult, references refindex.ManifestFacts) (*store.UnderstandingGeneration, error) {
	generation := &store.UnderstandingGeneration{
		RepositoryID: repository.ID, CheckoutID: repository.CheckoutID, CheckoutRoot: repository.Root,
		Status: "complete", SnapshotProtocol: changeenv.GitTreeProtocol, SnapshotDigest: capture.SnapshotDigest,
		BaseRevision: capture.Base, HeadRevision: capture.Head, StructuralSchema: codemap.StructuralSchema,
		AnalyzerBundleDigest: bundleDigest, ConventionState: conventionState,
		ConventionSourceRef: conventionRef, ConventionSourceDigest: conventionDigest,
		StartedAt: startedAt, EndedAt: endedAt, PathTotal: len(capture.Manifest),
		ErrorTotal: structuralErrorTotal(structure.Failures) + references.Coverage.Errors + references.Coverage.Ambiguous,
	}
	for _, unit := range structure.Units {
		body, err := json.Marshal(unit.Descriptor)
		if err != nil {
			return nil, err
		}
		generation.Units = append(generation.Units, store.UnderstandingUnit{Path: unit.Path, SourceHash: unit.Descriptor.SourceHash, Language: unit.Descriptor.Identity.Language, Namespace: unit.Descriptor.Identity.Namespace, DescriptorJSON: string(body)})
	}
	for _, edge := range structure.Edges {
		generation.Edges = append(generation.Edges, store.UnderstandingEdge{FromKind: edge.FromKind, FromRef: edge.FromRef, Relation: edge.Relation, ToKind: edge.ToKind, ToRef: edge.ToRef, SourcePath: edge.SourcePath, SourceLine: edge.SourceLine, Provenance: "measured", AnalyzerID: edge.AnalyzerID, EvidenceDigest: digestText(edge.FromKind + "\x00" + edge.FromRef + "\x00" + edge.Relation + "\x00" + edge.ToKind + "\x00" + edge.ToRef)})
	}
	appendReferenceEdges(generation, references)
	generation.Coverage = append(generation.Coverage, store.UnderstandingCoverage{Family: "file_inventory", State: "complete", AnalyzerID: changeenv.GitTreeProtocol, Attempted: len(capture.Manifest), Produced: len(references.Paths)})
	for _, coverage := range structure.Coverage {
		generation.Coverage = append(generation.Coverage, store.UnderstandingCoverage{Family: string(coverage.Family), State: coverage.State, AnalyzerID: coverage.AnalyzerID, Attempted: coverage.Attempted, Produced: coverage.Produced, Errors: coverage.Errors, Unresolved: coverage.Unresolved, Ambiguous: coverage.Ambiguous, Reason: coverage.Reason})
	}
	generation.Coverage = append(generation.Coverage, store.UnderstandingCoverage{Family: "document_reference", State: references.Coverage.State, AnalyzerID: "reference-index-v1", Attempted: references.Coverage.Attempted, Produced: references.Coverage.Produced, Errors: references.Coverage.Errors + references.Coverage.Ambiguous, Reason: references.Coverage.Reason})
	for _, family := range []string{"configuration_effect", "data_effect", "policy_effect", "journey_effect"} {
		generation.Coverage = append(generation.Coverage, store.UnderstandingCoverage{Family: family, State: "unsupported", AnalyzerID: "framework-boundary-v1", Reason: "no typed producer is registered for this fact family"})
	}
	return generation, nil
}

func appendReferenceEdges(generation *store.UnderstandingGeneration, facts refindex.ManifestFacts) {
	definitionItems := map[string][]string{}
	manifestPaths := make(map[string]bool, len(facts.Paths))
	for _, path := range facts.Paths {
		manifestPaths[path] = true
	}
	seen := map[string]bool{}
	appendEdge := func(edge store.UnderstandingEdge) {
		key := edge.FromKind + "\x00" + edge.FromRef + "\x00" + edge.Relation + "\x00" + edge.ToKind + "\x00" + edge.ToRef + "\x00" + edge.SourcePath + fmt.Sprintf("\x00%d\x00", edge.SourceLine) + edge.AnalyzerID
		if !seen[key] {
			seen[key] = true
			generation.Edges = append(generation.Edges, edge)
		}
	}
	for id, definitions := range facts.IDs {
		for _, definition := range definitions {
			appendEdge(store.UnderstandingEdge{FromKind: "document", FromRef: definition.Path, Relation: "document_defines_item", ToKind: "item", ToRef: id, SourcePath: definition.Path, SourceLine: definition.Line, Provenance: "measured", AnalyzerID: "reference-index-v1", EvidenceDigest: digestText(definition.Text)})
			key := definition.Path + fmt.Sprintf(":%d", definition.Line)
			definitionItems[key] = append(definitionItems[key], id)
		}
	}
	for target, mentions := range facts.Mentions {
		// The shared index also records ID-to-ID mentions for backlinks. Only an
		// exact manifest path can become a typed document/file edge.
		if !manifestPaths[target] {
			continue
		}
		for _, mention := range mentions {
			appendEdge(store.UnderstandingEdge{FromKind: "document", FromRef: mention.Path, Relation: "document_references_file", ToKind: "file", ToRef: target, SourcePath: mention.Path, SourceLine: mention.Line, Provenance: "measured", AnalyzerID: "reference-index-v1", EvidenceDigest: digestText(mention.Text)})
			for _, id := range definitionItems[mention.Path+fmt.Sprintf(":%d", mention.Line)] {
				appendEdge(store.UnderstandingEdge{FromKind: "item", FromRef: id, Relation: "item_references_file", ToKind: "file", ToRef: target, SourcePath: mention.Path, SourceLine: mention.Line, Provenance: "measured", AnalyzerID: "reference-index-v1", EvidenceDigest: digestText(mention.Text)})
			}
		}
	}
	sort.Slice(generation.Edges, func(i, j int) bool {
		a, b := generation.Edges[i], generation.Edges[j]
		ak := a.FromKind + "\x00" + a.FromRef + "\x00" + a.Relation + "\x00" + a.ToKind + "\x00" + a.ToRef + "\x00" + a.SourcePath + fmt.Sprintf("\x00%09d", a.SourceLine)
		bk := b.FromKind + "\x00" + b.FromRef + "\x00" + b.Relation + "\x00" + b.ToKind + "\x00" + b.ToRef + "\x00" + b.SourcePath + fmt.Sprintf("\x00%09d", b.SourceLine)
		return ak < bk
	})
}

func structuralErrorTotal(failures []codemap.UnitFailure) int {
	total := 0
	for _, failure := range failures {
		if failure.Code != "unsupported_analyzer" {
			total++
		}
	}
	return total
}
