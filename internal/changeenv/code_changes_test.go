package changeenv

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/codemap"
	"crossing-guard/store"
)

func appendCodeCheckpoint(t *testing.T, ix *store.Index, sessionID, kind, requestID,
	digest string, requestedAt, endedAt int64) store.SessionCheckpoint {
	return appendCodeCheckpointIdentity(t, ix, sessionID, "repo", "checkout", kind, requestID,
		digest, requestedAt, endedAt)
}

func appendCodeCheckpointIdentity(t *testing.T, ix *store.Index, sessionID, repositoryID,
	checkoutID, kind, requestID, digest string, requestedAt, endedAt int64) store.SessionCheckpoint {
	t.Helper()
	boundary := "settled"
	if kind == "attachment" || kind == "pre-mutation" {
		boundary = "direct-pre-release"
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := tx.EnsureSessionCheckpoint(store.SessionCheckpoint{SessionID: sessionID,
		ScopeKey: checkoutID, Kind: kind, RequestID: requestID, WorkingDirectory: "/repo",
		RepositoryID: repositoryID, CheckoutID: checkoutID, CheckoutRoot: "/repo", Status: "pending",
		BoundaryClass: boundary, RequestedAt: requestedAt})
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, won, claimErr := ix.ClaimSessionCheckpoint(checkpoint.ID, requestedAt, 0, boundary); claimErr != nil || !won {
		t.Fatalf("claim checkpoint won=%v err=%v", won, claimErr)
	}
	record := &store.ChangeRecord{SessionID: sessionID, RepositoryID: repositoryID, CheckoutID: checkoutID,
		RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo", Kind: "revision",
		EvidenceClass: "observed", SourceKind: "git", SourceRef: "HEAD", SourceDisplay: "Git",
		SourceDigest: digest, RecordedAt: endedAt, CaptureStartedAt: requestedAt,
		CaptureEndedAt: endedAt, BaseRevision: "base", HeadRevision: "head",
		SnapshotDigest: digest, CaptureAttempts: 1}
	if err := ix.CompleteSessionCheckpoint(checkpoint.ID, record, endedAt); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = ix.SessionCheckpointByID(checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func codeDescriptor(t *testing.T, path, sourceHash string, declarations []codemap.Declaration) string {
	t.Helper()
	descriptor := codemap.UnitDescriptor{Identity: codemap.Identity{Path: path, Language: "fixture",
		AnalyzerID: "fixture-v3", Namespace: "example/core"}, Symbol: codemap.Symbol{Declarations: declarations},
		SourceHash: sourceHash}
	body, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func appendCodeGeneration(t *testing.T, ix *store.Index, digest, sourceA string, includeB bool,
	callStates ...string) store.UnderstandingGeneration {
	t.Helper()
	span, cyclomatic := 4, 2
	runSpan, runCyclomatic := span, cyclomatic
	if sourceA == "current" {
		runSpan, runCyclomatic = 6, 3
	}
	declarations := []codemap.Declaration{{Identity: "function:Run", Name: "Run", Kind: "function",
		Line: 1, EndLine: 4, SourceDigest: "go-source-declaration-v1-sha256:" + sourceA,
		SourceSpanLines: &runSpan, Cyclomatic: &runCyclomatic,
		BodyShapeDigest: "go-body-shape-v1-sha256:shared", BodyShapeNodes: 8}}
	if sourceA == "current" {
		declarations = append(declarations, codemap.Declaration{Identity: "function:Added", Name: "Added",
			Kind: "function", Line: 7, EndLine: 8,
			SourceDigest: "go-source-declaration-v1-sha256:added", SourceSpanLines: &cyclomatic,
			Cyclomatic: &cyclomatic})
	}
	units := []store.UnderstandingUnit{{Path: "a.go", SourceHash: "sha256-v1:" + sourceA,
		Language: "fixture", Namespace: "example/core",
		DescriptorJSON: codeDescriptor(t, "a.go", "sha256-v1:"+sourceA, declarations)}}
	edges := []store.UnderstandingEdge{{FromKind: "file", FromRef: "a.go", Relation: "file_in_package",
		ToKind: "package", ToRef: "example/core", SourcePath: "a.go", SourceLine: 1,
		Provenance: "measured", AnalyzerID: "fixture-v3", EvidenceDigest: "sha256-v1:a-package"},
		{FromKind: "package", FromRef: "example/consumer", Relation: "package_depends_on",
			ToKind: "package", ToRef: "example/core", SourcePath: "consumer.go", SourceLine: 3,
			Provenance: "measured", AnalyzerID: "fixture-v3", EvidenceDigest: "sha256-v1:dependency"},
		{FromKind: "symbol", FromRef: "fixture-v3::function:Caller", Relation: "symbol_calls_symbol",
			ToKind: "symbol", ToRef: "fixture-v3::function:Run", SourcePath: "caller.go", SourceLine: 9,
			Provenance: "measured", AnalyzerID: "fixture-v3", EvidenceDigest: "sha256-v1:call"}}
	if includeB {
		bDecl := []codemap.Declaration{{Identity: "function:Other", Name: "Other", Kind: "function",
			Line: 1, EndLine: 4, SourceDigest: "go-source-declaration-v1-sha256:other",
			SourceSpanLines: &span, Cyclomatic: &cyclomatic,
			BodyShapeDigest: "go-body-shape-v1-sha256:shared", BodyShapeNodes: 8}}
		units = append(units, store.UnderstandingUnit{Path: "b.go", SourceHash: "sha256-v1:b",
			Language: "fixture", Namespace: "example/core",
			DescriptorJSON: codeDescriptor(t, "b.go", "sha256-v1:b", bDecl)})
		edges = append(edges, store.UnderstandingEdge{FromKind: "file", FromRef: "b.go",
			Relation: "file_in_package", ToKind: "package", ToRef: "example/core",
			SourcePath: "b.go", SourceLine: 1, Provenance: "measured", AnalyzerID: "fixture-v3",
			EvidenceDigest: "sha256-v1:b-package"})
	}
	callCoverage := store.UnderstandingCoverage{Family: string(codemap.CapabilitySymbolCall),
		State: "complete", AnalyzerID: "fixture-v3", Attempted: 1, Produced: 1}
	if len(callStates) > 0 {
		callCoverage.State = callStates[0]
		if callCoverage.State == "partial" {
			callCoverage.Unresolved, callCoverage.Reason = 1, "one dynamic call is unresolved"
		}
		if callCoverage.State == "unsupported" {
			callCoverage.Attempted, callCoverage.Produced, callCoverage.Reason = 0, 0, "fixture does not produce calls"
			withoutCalls := make([]store.UnderstandingEdge, 0, len(edges))
			for _, edge := range edges {
				if edge.Relation != "symbol_calls_symbol" {
					withoutCalls = append(withoutCalls, edge)
				}
			}
			edges = withoutCalls
		}
	}
	generation := store.UnderstandingGeneration{RepositoryID: "repo", CheckoutID: "checkout",
		CheckoutRoot: "/repo", Status: "complete", SnapshotProtocol: GitTreeProtocol,
		SnapshotDigest: digest, BaseRevision: "base", HeadRevision: "head",
		StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: "bundle",
		ConventionState: "none", StartedAt: 1, EndedAt: 2, PathTotal: len(units),
		Units: units, Edges: edges, Coverage: []store.UnderstandingCoverage{
			{Family: string(codemap.CapabilityPackageDependency), State: "complete", AnalyzerID: "fixture-v3", Attempted: len(units), Produced: 1},
			{Family: string(codemap.CapabilitySymbolDeclaration), State: "complete", AnalyzerID: "fixture-v3", Attempted: len(units), Produced: len(declarations)},
			{Family: string(codemap.CapabilityResponsibilityFingerprint), State: "complete", AnalyzerID: "fixture-v3", Attempted: len(units), Produced: len(units)},
			callCoverage,
		}}
	if err := ix.AppendUnderstanding(&generation); err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestBuildCodeChangesReturnsExactPagedFactsWithoutJudgmentVocabulary(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	appendCodeCheckpoint(t, ix, "session", "attachment", "baseline", "git-tree-v2-sha256:baseline", 1, 2)
	appendCodeCheckpoint(t, ix, "session", "settled", "current", "git-tree-v2-sha256:current", 3, 4)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:baseline", "baseline", false)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:current", "current", true)

	changes, err := BuildCodeChanges(ix, "session", CodeChangeOptions{AnalyzerBundleDigest: "bundle", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if changes.State != "exact" || changes.Observation != "observed_checkout_delta" || changes.FilePage.Total != 2 || len(changes.Files) != 1 {
		t.Fatalf("code changes=%+v", changes)
	}
	if changes.Files[0].Path != "a.go" || changes.Files[0].DeclarationChanges.Total != 2 {
		t.Fatalf("first code row=%+v", changes.Files[0])
	}
	if changes.FunctionCallCoverage != "exact" || changes.Aggregates.State != "exact" ||
		changes.Aggregates.FileTotal != 2 || changes.Aggregates.DeclarationChanges != 3 ||
		changes.Aggregates.DeclarationsAdded != 2 || changes.Aggregates.DeclarationsModified != 1 ||
		changes.Aggregates.SourceSpanLines.Measured != 1 || changes.Aggregates.SourceSpanLines.Delta != 2 ||
		changes.Aggregates.Cyclomatic.Measured != 1 || changes.Aggregates.Cyclomatic.Delta != 1 ||
		!changes.Aggregates.Callers.Exact || changes.Aggregates.Callers.Total != 1 ||
		len(changes.Aggregates.Callers.Edges) != 1 || changes.Aggregates.Callers.Edges[0].FromRef != "fixture-v3::function:Caller" ||
		changes.Aggregates.DependencyState != "exact" || changes.Aggregates.DependentPackages.Total != 1 ||
		len(changes.Aggregates.ReferencingFiles.Values) != 1 || changes.Aggregates.ReferencingFiles.Values[0] != "consumer.go" ||
		changes.Aggregates.StructuralMatches.State != "exact" || changes.Aggregates.StructuralMatches.Page.Total != 1 ||
		len(changes.Aggregates.StructuralMatches.Rows) != 1 || changes.Aggregates.StructuralMatches.Rows[0].State != "added" {
		t.Fatalf("aggregate facts=%+v coverage=%s", changes.Aggregates, changes.FunctionCallCoverage)
	}
	body, _ := json.Marshal(changes)
	for _, forbidden := range []string{"duplicate", "huge_method", "controller_violation", "quality_score"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("judgment vocabulary %q leaked into %s", forbidden, body)
		}
	}

	detail, err := BuildCodeChangeFile(ix, "session", "a.go",
		CodeChangeOptions{AnalyzerBundleDigest: "bundle"}, 0, 25)
	if err != nil {
		t.Fatal(err)
	}
	if detail.StructuralMatches.State != "exact" || detail.StructuralMatches.Page.Total != 1 ||
		len(detail.StructuralMatches.Rows) != 1 || detail.StructuralMatches.Rows[0].State != "added" {
		t.Fatalf("structural match delta=%+v", detail.StructuralMatches)
	}
	if detail.Dependencies.State != "exact" || detail.Dependencies.FunctionCallCoverage != "exact" ||
		len(detail.Dependencies.DependentPackages.Values) != 1 || detail.Dependencies.ReferencingFiles.Values[0] != "consumer.go" {
		t.Fatalf("dependency facts=%+v", detail.Dependencies)
	}
	if !detail.Dependencies.Callers.Exact || detail.Dependencies.Callers.Total != 1 ||
		len(detail.Dependencies.Callers.Edges) != 1 || detail.Dependencies.Callers.Edges[0].SourcePath != "caller.go" {
		t.Fatalf("caller facts=%+v", detail.Dependencies.Callers)
	}
	if _, err := BuildCodeChangeFile(ix, "session", "../escape.go",
		CodeChangeOptions{AnalyzerBundleDigest: "bundle"}, 0, 25); !errors.Is(err, ErrInvalidFileView) {
		t.Fatalf("unsafe path error=%v", err)
	}
}

func TestBuildCodeChangesPreservesPartialCallerCoverage(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	appendCodeCheckpoint(t, ix, "partial", "attachment", "baseline", "git-tree-v2-sha256:baseline", 1, 2)
	appendCodeCheckpoint(t, ix, "partial", "settled", "current", "git-tree-v2-sha256:current", 3, 4)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:baseline", "baseline", false)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:current", "current", true, "partial")

	changes, err := BuildCodeChanges(ix, "partial", CodeChangeOptions{AnalyzerBundleDigest: "bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if changes.FunctionCallCoverage != "partial" || changes.Aggregates.Callers.State != "partial" ||
		changes.Aggregates.Callers.Exact || changes.Aggregates.Callers.Total != 1 ||
		len(changes.Aggregates.Callers.Coverage) != 1 || changes.Aggregates.Callers.Coverage[0].Unresolved != 1 {
		t.Fatalf("partial caller facts=%+v coverage=%s", changes.Aggregates.Callers, changes.FunctionCallCoverage)
	}
}

func TestBuildCodeChangesPreservesUnsupportedCallerCoverage(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	appendCodeCheckpoint(t, ix, "unsupported", "attachment", "baseline", "git-tree-v2-sha256:baseline", 1, 2)
	appendCodeCheckpoint(t, ix, "unsupported", "settled", "current", "git-tree-v2-sha256:current", 3, 4)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:baseline", "baseline", false)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:current", "current", true, "unsupported")

	changes, err := BuildCodeChanges(ix, "unsupported", CodeChangeOptions{AnalyzerBundleDigest: "bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if changes.FunctionCallCoverage != "unsupported" || changes.Aggregates.Callers.State != "unsupported" ||
		changes.Aggregates.Callers.Exact || changes.Aggregates.Callers.Total != 0 || changes.Aggregates.Callers.Reason == "" {
		t.Fatalf("unsupported caller facts=%+v coverage=%s", changes.Aggregates.Callers, changes.FunctionCallCoverage)
	}
}

func TestBuildCodeChangesCurrentOnlyDoesNotRenderZeroDelta(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	appendCodeCheckpoint(t, ix, "current-only", "settled", "current", "git-tree-v2-sha256:current", 3, 4)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:current", "current", true)

	changes, err := BuildCodeChanges(ix, "current-only", CodeChangeOptions{AnalyzerBundleDigest: "bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if changes.State != "baseline_unavailable" || changes.Aggregates.State != "baseline_unavailable" ||
		changes.Aggregates.Reason == "" || changes.Aggregates.DeclarationChanges != 0 ||
		changes.Aggregates.Cyclomatic.Measured != 0 {
		t.Fatalf("current-only facts rendered a delta: %+v", changes)
	}
}

func TestBuildCodeChangesNeverFallsBackFromUnanalyzedCurrentCheckpoint(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	appendCodeCheckpoint(t, ix, "pending", "attachment", "baseline", "git-tree-v2-sha256:baseline", 1, 2)
	current := appendCodeCheckpoint(t, ix, "pending", "settled", "current", "git-tree-v2-sha256:current", 3, 4)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:baseline", "baseline", false)

	changes, err := BuildCodeChanges(ix, "pending", CodeChangeOptions{AnalyzerBundleDigest: "bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if changes.State != "unavailable" || changes.Current == nil || changes.Current.CheckpointID != current.ID || changes.Current.GenerationID != 0 || changes.FilePage.Total != 0 {
		t.Fatalf("missing current fell back: %+v", changes)
	}
}

func TestBuildCodeChangesRejectsCheckpointFromDifferentRepositoryIdentity(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	appendCodeCheckpointIdentity(t, ix, "session", "repo-a", "shared-checkout", "settled",
		"repo-a", "git-tree-v2-sha256:repo-a", 1, 2)
	other := appendCodeCheckpointIdentity(t, ix, "session", "repo-b", "shared-checkout", "settled",
		"repo-b", "git-tree-v2-sha256:repo-b", 3, 4)
	_, err = BuildCodeChanges(ix, "session", CodeChangeOptions{RepositoryID: "repo-a",
		CheckoutID: "shared-checkout", CheckpointID: other.ID, AnalyzerBundleDigest: "bundle"})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-repository checkpoint error=%v", err)
	}
}
