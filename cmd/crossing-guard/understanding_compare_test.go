package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"crossing-guard/codemap"
	"crossing-guard/store"
)

func TestReadUnderstandingCandidatesStatesAndExactFacts(t *testing.T) {
	index, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	left := compareDescriptor(t, "a.go", "go-ast-v2", "Same", "shape-v1:same")
	right := compareDescriptor(t, "b.go", "go-ast-v2", "Other", "shape-v1:same")
	generation := &store.UnderstandingGeneration{
		RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo", Status: "complete",
		SnapshotProtocol: "git-tree-v2", SnapshotDigest: "git-tree-v2-sha256:fixture", StructuralSchema: codemap.StructuralSchema,
		AnalyzerBundleDigest: "sha256-v1:bundle", ConventionState: "none", StartedAt: 1, EndedAt: 2,
		Units: []store.UnderstandingUnit{
			{Path: "a.go", SourceHash: "sha256-v1:a", Language: "go", DescriptorJSON: left},
			{Path: "b.go", SourceHash: "sha256-v1:b", Language: "go", DescriptorJSON: right},
		},
		Coverage: []store.UnderstandingCoverage{{Family: "responsibility_fingerprint", State: "complete", AnalyzerID: "go-ast-v2", Attempted: 2, Produced: 2}},
	}
	if err := index.AppendUnderstanding(generation); err != nil {
		t.Fatal(err)
	}
	view, err := readUnderstandingCandidates(index, *generation, "a.go", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "exact" || view.Page == nil || view.Page.Total != 1 || len(view.Rows) != 1 || view.Rows[0].RightPath != "b.go" {
		t.Fatalf("exact candidates=%+v", view)
	}
	missing, err := readUnderstandingCandidates(index, *generation, "missing.go", 0, 100)
	if err != nil || missing.State != "unavailable" || missing.Reason == "" || len(missing.Rows) != 0 {
		t.Fatalf("missing center=%+v err=%v", missing, err)
	}
}

func TestReadUnderstandingCandidatesDoesNotTurnUnsupportedOrMalformedIntoNone(t *testing.T) {
	index, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	good := compareDescriptor(t, "a.go", "fixture-v1", "Same", "")
	for _, fixture := range []struct {
		name, descriptor, coverageState, want string
	}{
		{name: "unsupported", descriptor: good, coverageState: "unsupported", want: "unsupported"},
		{name: "malformed", descriptor: `{`, coverageState: "complete", want: "failed"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			generation := &store.UnderstandingGeneration{
				RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo", Status: "complete",
				SnapshotProtocol: "git-tree-v2", SnapshotDigest: "git-tree-v2-sha256:" + fixture.name, StructuralSchema: codemap.StructuralSchema,
				AnalyzerBundleDigest: "sha256-v1:" + fixture.name, ConventionState: "none", StartedAt: 10, EndedAt: 11,
				Units:    []store.UnderstandingUnit{{Path: "a.go", SourceHash: "sha256-v1:a", DescriptorJSON: fixture.descriptor}},
				Coverage: []store.UnderstandingCoverage{{Family: "responsibility_fingerprint", State: fixture.coverageState, AnalyzerID: "fixture-v1", Reason: map[bool]string{true: "not supported"}[fixture.coverageState == "unsupported"]}},
			}
			if err := index.AppendUnderstanding(generation); err != nil {
				t.Fatal(err)
			}
			view, err := readUnderstandingCandidates(index, *generation, "a.go", 0, 100)
			if err != nil || view.State != fixture.want || view.Reason == "" || len(view.Rows) != 0 {
				t.Fatalf("view=%+v err=%v", view, err)
			}
		})
	}
}

func compareDescriptor(t *testing.T, path, analyzer, name, shape string) string {
	t.Helper()
	declaration := codemap.Declaration{Name: name}
	if shape != "" {
		declaration.BodyShapeDigest, declaration.BodyShapeNodes = shape, 3
	}
	body, err := json.Marshal(codemap.UnitDescriptor{Identity: codemap.Identity{Path: path, Language: "go", AnalyzerID: analyzer}, Symbol: codemap.Symbol{Declarations: []codemap.Declaration{declaration}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
