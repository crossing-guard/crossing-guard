package changeenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"crossing-guard/codemap"
	"crossing-guard/store"
)

func impactIndex(t *testing.T) *store.Index {
	t.Helper()
	index, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func appendImpactRevision(t *testing.T, index *store.Index, session, digest string, paths ...string) *store.ChangeRecord {
	t.Helper()
	items := make([]store.ChangeItem, 0, len(paths))
	for _, path := range paths {
		items = append(items, store.ChangeItem{Path: path, Layer: "worktree", Status: "M"})
	}
	revision := &store.ChangeRecord{SessionID: session, RepositoryID: "repo", CheckoutID: "checkout", RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo", Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceRef: "git:head", SourceDisplay: "Git", SourceDigest: digest, RecordedAt: 10, CaptureStartedAt: 8, CaptureEndedAt: 10, BaseRevision: "base", HeadRevision: "head", SnapshotDigest: digest, Items: items}
	if err := index.AppendChange(revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func appendImpactGeneration(t *testing.T, index *store.Index, digest, bundle, convention, coverageState string) *store.UnderstandingGeneration {
	t.Helper()
	descriptor := func(path, name, shape string) string {
		body, err := json.Marshal(codemap.UnitDescriptor{Identity: codemap.Identity{Path: path, Language: "go", AnalyzerID: "go-ast-v2"}, Symbol: codemap.Symbol{Declarations: []codemap.Declaration{{Name: name, BodyShapeDigest: shape, BodyShapeNodes: 3}}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	generation := &store.UnderstandingGeneration{RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo", Status: "complete", SnapshotProtocol: "git-tree-v2", SnapshotDigest: digest, StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: bundle, ConventionState: convention, StartedAt: 20, EndedAt: 30,
		Units: []store.UnderstandingUnit{
			{Path: "changed.go", SourceHash: "sha256-v1:changed", Language: "go", DescriptorJSON: descriptor("changed.go", "Changed", "go-shape-v1:same")},
			{Path: "similar.go", SourceHash: "sha256-v1:similar", Language: "go", DescriptorJSON: descriptor("similar.go", "Similar", "go-shape-v1:same")},
			{Path: "similar-two.go", SourceHash: "sha256-v1:similar-two", Language: "go", DescriptorJSON: descriptor("similar-two.go", "SimilarTwo", "go-shape-v1:same")},
		},
		Edges: []store.UnderstandingEdge{
			{FromKind: "file", FromRef: "changed.go", Relation: "file_in_package", ToKind: "package", ToRef: "core", SourcePath: "changed.go", Provenance: "measured", AnalyzerID: "go-ast-v1", EvidenceDigest: "sha256-v1:1"},
			{FromKind: "package", FromRef: "consumer", Relation: "package_depends_on", ToKind: "package", ToRef: "core", SourcePath: "consumer.go", Provenance: "measured", AnalyzerID: "go-ast-v1", EvidenceDigest: "sha256-v1:2"},
			{FromKind: "file", FromRef: "changed.go", Relation: "file_declares_symbol", ToKind: "symbol", ToRef: "core::Changed", SourcePath: "changed.go", SourceLine: 7, Provenance: "measured", AnalyzerID: "go-ast-v1", EvidenceDigest: "sha256-v1:3"},
			{FromKind: "document", FromRef: "docs/work.md", Relation: "document_references_file", ToKind: "file", ToRef: "changed.go", SourcePath: "docs/work.md", SourceLine: 9, Provenance: "measured", AnalyzerID: "reference-index-v1", EvidenceDigest: "sha256-v1:4"},
		},
		Coverage: []store.UnderstandingCoverage{{Family: "package_dependency", State: coverageState, AnalyzerID: "go-ast-v1", Attempted: 3, Produced: 3, Reason: map[bool]string{true: "fixture partial"}[coverageState == "partial"]}, {Family: "responsibility_fingerprint", State: coverageState, AnalyzerID: "go-ast-v2", Attempted: 3, Produced: 3, Reason: map[bool]string{true: "fixture partial"}[coverageState == "partial"]}, {Family: "symbol_call", State: "unsupported", AnalyzerID: "framework-boundary-v1", Reason: "no producer"}},
	}
	if convention == "explicit" {
		generation.ConventionSourceRef = "conventions.json"
		generation.ConventionSourceDigest = "sha256-v1:config"
	}
	if err := index.AppendUnderstanding(generation); err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestImpactSelectsExactGenerationAndShowsDirectAndReverseFacts(t *testing.T) {
	index := impactIndex(t)
	digest := "git-tree-v2-sha256:exact"
	revision := appendImpactRevision(t, index, "session", digest, "changed.go", "deleted.go")
	generation := appendImpactGeneration(t, index, digest, "bundle", "none", "complete")
	view, err := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "exact" || view.Generation == nil || view.Generation.ID != generation.ID {
		t.Fatalf("exact generation not selected: %+v", view)
	}
	if view.NodePage.Total != 6 || view.EdgePage.Total != 4 {
		t.Fatalf("server did not count full direct/downstream population: nodes=%+v edges=%+v", view.NodePage, view.EdgePage)
	}
	direct := map[string]ImpactNode{}
	for _, node := range view.Nodes {
		if node.Direct {
			direct[node.Ref] = node
		}
	}
	if len(direct) != 2 || direct["changed.go"].SourceID != revision.ID || direct["deleted.go"].SourceID != revision.ID {
		t.Fatalf("direct nodes were reconstructed from edge pages: %+v", direct)
	}
	foundDependent := false
	for _, edge := range view.Edges {
		if edge.Relation == "package_depends_on" && edge.FromRef == "consumer" && edge.ToRef == "core" {
			foundDependent = true
		}
	}
	if !foundDependent {
		t.Fatalf("reverse package dependent missing: %+v", view.Edges)
	}
}

func TestImpactNamesStaleFailedExplicitAndPartialStates(t *testing.T) {
	t.Run("historical source", func(t *testing.T) {
		index := impactIndex(t)
		revision := appendImpactRevision(t, index, "v1", "sha256-v1:old", "changed.go")
		if view, _ := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle"}); view.State != "stale_source" {
			t.Fatalf("historical source state=%+v", view)
		}
	})
	t.Run("analyzer changed", func(t *testing.T) {
		index := impactIndex(t)
		digest := "git-tree-v2-sha256:same"
		revision := appendImpactRevision(t, index, "analyzer", digest, "changed.go")
		appendImpactGeneration(t, index, digest, "old-bundle", "none", "complete")
		if view, _ := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "new-bundle"}); view.State != "stale_analyzer" {
			t.Fatalf("analyzer state=%+v", view)
		}
	})
	t.Run("explicit only", func(t *testing.T) {
		index := impactIndex(t)
		digest := "git-tree-v2-sha256:explicit"
		revision := appendImpactRevision(t, index, "explicit", digest, "changed.go")
		appendImpactGeneration(t, index, digest, "bundle", "explicit", "complete")
		if view, _ := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle"}); view.State != "unavailable" {
			t.Fatalf("explicit-only state=%+v", view)
		}
	})
	t.Run("failed exact", func(t *testing.T) {
		index := impactIndex(t)
		digest := "git-tree-v2-sha256:failed"
		revision := appendImpactRevision(t, index, "failed", digest, "changed.go")
		failed := &store.UnderstandingGeneration{RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo", Status: "failed", SnapshotProtocol: "git-tree-v2", SnapshotDigest: digest, StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: "bundle", ConventionState: "none", StartedAt: 20, EndedAt: 21, LimitationCode: "fixture", Limitation: "failed fixture"}
		if err := index.AppendUnderstanding(failed); err != nil {
			t.Fatal(err)
		}
		if view, _ := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle"}); view.State != "failed" {
			t.Fatalf("failed state=%+v", view)
		}
	})
	t.Run("partial coverage", func(t *testing.T) {
		index := impactIndex(t)
		digest := "git-tree-v2-sha256:partial"
		revision := appendImpactRevision(t, index, "partial", digest, "changed.go")
		appendImpactGeneration(t, index, digest, "bundle", "none", "partial")
		if view, _ := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactCenterKind: "file", ImpactCenterRef: "changed.go"}); view.State != "partial" || view.CandidateState != "partial" || view.CandidateReason != "fixture partial" {
			t.Fatalf("partial state=%+v", view)
		}
	})
}

func TestImpactPagesNodesAndEdgesIndependently(t *testing.T) {
	index := impactIndex(t)
	digest := "git-tree-v2-sha256:paged"
	revision := appendImpactRevision(t, index, "paged", digest, "changed.go", "deleted.go")
	appendImpactGeneration(t, index, digest, "bundle", "none", "complete")
	view, err := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactLimit: 1, ImpactNodeOffset: 1, ImpactEdgeOffset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Nodes) != 1 || view.NodePage.Total != 6 || view.NodePage.Offset != 1 || view.NodePage.NextOffset == nil {
		t.Fatalf("node page incorrect: %+v nodes=%+v", view.NodePage, view.Nodes)
	}
	if len(view.Edges) != 1 || view.EdgePage.Total != 4 || view.EdgePage.Offset != 2 || view.EdgePage.NextOffset == nil {
		t.Fatalf("edge page incorrect: %+v edges=%+v", view.EdgePage, view.Edges)
	}
}

func TestImpactCenterMustResolveAndReturnsStoredFacts(t *testing.T) {
	index := impactIndex(t)
	digest := "git-tree-v2-sha256:center"
	revision := appendImpactRevision(t, index, "center", digest, "changed.go")
	generation := appendImpactGeneration(t, index, digest, "bundle", "none", "complete")

	view, err := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactCenterKind: "package", ImpactCenterRef: "consumer"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Center == nil || view.Center.Kind != "package" || view.Center.Ref != "consumer" || view.Center.Provenance != "measured" || view.Center.SourceID != generation.ID {
		t.Fatalf("center is not the resolved generation fact: %+v", view.Center)
	}
	found := false
	for _, edge := range view.Edges {
		if edge.FromKind == "package" && edge.FromRef == "consumer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("centered incident edge missing: %+v", view.Edges)
	}

	_, err = buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactCenterKind: "package", ImpactCenterRef: "invented"})
	if !errors.Is(err, ErrImpactCenterNotFound) {
		t.Fatalf("unknown center error=%v", err)
	}
}

func TestImpactFileCenterReturnsMechanicalCandidatesAndNamedBoundaries(t *testing.T) {
	index := impactIndex(t)
	digest := "git-tree-v2-sha256:candidates"
	revision := appendImpactRevision(t, index, "candidates", digest, "changed.go", "deleted.go")
	appendImpactGeneration(t, index, digest, "bundle", "none", "complete")

	view, err := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactCenterKind: "file", ImpactCenterRef: "changed.go", ImpactCandidateLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if view.CandidateState != "exact" || view.CandidateAnalyzerID != "go-ast-v2" || view.CandidatePage.Total != 2 || len(view.Candidates) != 2 || view.Candidates[0].RightPath != "similar-two.go" || view.Candidates[1].RightPath != "similar.go" || view.Candidates[0].SharedBodyShapes.Total != 1 {
		t.Fatalf("file candidates=%+v", view)
	}
	next, err := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactCenterKind: "file", ImpactCenterRef: "changed.go", ImpactCandidateLimit: 1, ImpactCandidateOffset: 1})
	if err != nil || next.CandidatePage.Offset != 1 || next.CandidatePage.Returned != 1 || next.CandidatePage.NextOffset != nil || len(next.Candidates) != 1 || next.Candidates[0].RightPath != "similar.go" {
		t.Fatalf("candidate next page=%+v err=%v", next, err)
	}
	deleted, err := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactCenterKind: "file", ImpactCenterRef: "deleted.go"})
	if err != nil || deleted.CandidateState != "unavailable" || deleted.CandidateReason == "" || len(deleted.Candidates) != 0 {
		t.Fatalf("deleted center=%+v err=%v", deleted, err)
	}
	nonFile, err := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactCenterKind: "package", ImpactCenterRef: "core"})
	if err != nil || nonFile.CandidateState != "unavailable" || nonFile.CandidateReason != "file center required" {
		t.Fatalf("non-file candidate boundary=%+v err=%v", nonFile, err)
	}
}

func TestSessionImpactCandidatesStayWithinSelectedRepository(t *testing.T) {
	index := impactIndex(t)
	descriptor := func(path, shape string) string {
		body, err := json.Marshal(codemap.UnitDescriptor{Identity: codemap.Identity{Path: path, Language: "go", AnalyzerID: "go-ast-v2"}, Symbol: codemap.Symbol{Declarations: []codemap.Declaration{{Name: path, BodyShapeDigest: shape, BodyShapeNodes: 3}}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	for i := 0; i < 2; i++ {
		repositoryID := fmt.Sprintf("repo-%d", i)
		checkoutID := fmt.Sprintf("checkout-%d", i)
		root := fmt.Sprintf("/repo-%d", i)
		digest := fmt.Sprintf("git-tree-v2-sha256:repo-%d", i)
		candidatePath := fmt.Sprintf("candidate-%d.go", i)
		revision := &store.ChangeRecord{SessionID: "multi-impact", RepositoryID: repositoryID, CheckoutID: checkoutID, RepositoryIdentityKind: "local-sha256", CheckoutRoot: root, Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceRef: "git:head", SourceDisplay: "Git", SourceDigest: digest, RecordedAt: int64(i + 1), CaptureStartedAt: 1, CaptureEndedAt: 2, BaseRevision: "base", HeadRevision: "head", SnapshotDigest: digest, Items: []store.ChangeItem{{Path: "center.go", Layer: "worktree", Status: "M"}}}
		if err := index.AppendChange(revision); err != nil {
			t.Fatal(err)
		}
		shape := fmt.Sprintf("shape-v1:repo-%d", i)
		generation := &store.UnderstandingGeneration{RepositoryID: repositoryID, CheckoutID: checkoutID, CheckoutRoot: root, Status: "complete", SnapshotProtocol: "git-tree-v2", SnapshotDigest: digest, StructuralSchema: codemap.StructuralSchema, AnalyzerBundleDigest: "bundle", ConventionState: "none", StartedAt: 3, EndedAt: 4, Units: []store.UnderstandingUnit{{Path: "center.go", SourceHash: "sha256-v1:center", Language: "go", DescriptorJSON: descriptor("center.go", shape)}, {Path: candidatePath, SourceHash: "sha256-v1:candidate", Language: "go", DescriptorJSON: descriptor(candidatePath, shape)}}, Coverage: []store.UnderstandingCoverage{{Family: "responsibility_fingerprint", State: "complete", AnalyzerID: "go-ast-v2", Attempted: 2, Produced: 2}}}
		if err := index.AppendUnderstanding(generation); err != nil {
			t.Fatal(err)
		}
	}
	for offset := 0; offset < 2; offset++ {
		view, err := Build(index, "multi-impact", ViewOptions{RepositoryOffset: offset, RepositoryLimit: 1, AnalyzerBundleDigest: "bundle", ImpactCenterKind: "file", ImpactCenterRef: "center.go", ImpactCandidateLimit: 100})
		if err != nil {
			t.Fatal(err)
		}
		wantRepo := fmt.Sprintf("repo-%d", offset)
		wantCandidate := fmt.Sprintf("candidate-%d.go", offset)
		if view.RepositoryCount != 2 || len(view.Repositories) != 1 || view.Repositories[0].RepositoryID != wantRepo || len(view.Repositories[0].Downstream.Candidates) != 1 || view.Repositories[0].Downstream.Candidates[0].RightPath != wantCandidate {
			t.Fatalf("repository offset %d crossed impact facts: %+v", offset, view)
		}
	}
}
