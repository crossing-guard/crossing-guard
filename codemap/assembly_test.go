package codemap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingManifestProvider struct {
	request   ProviderRequest
	omit      bool
	fail      bool
	oversized bool
}

func (provider *recordingManifestProvider) Descriptor() ProviderDescriptor {
	capabilities := []Capability{CapabilitySymbolDeclaration}
	if provider.oversized {
		capabilities = append(capabilities, CapabilitySymbolCall)
	}
	return ProviderDescriptor{Kind: "test-process", Identity: "test-portable-v1", BundleDigest: "sha256-v1:test",
		Languages: []string{"fixture"}, Extensions: []string{".fixture"},
		Capabilities: capabilities}
}

func (provider *recordingManifestProvider) AnalyzeManifest(_ context.Context, request ProviderRequest) (ProviderResult, error) {
	provider.request = request
	if provider.omit {
		return ProviderResult{}, nil
	}
	if provider.fail {
		return ProviderResult{Failures: []UnitFailure{{Path: request.Sources[0].Path,
			AnalyzerID: provider.Descriptor().Identity, Code: "parse_failed", Reason: "fixture"}},
			Coverage: []CoverageResult{{Family: CapabilitySymbolDeclaration, State: "partial",
				AnalyzerID: provider.Descriptor().Identity, Attempted: 1, Errors: 1, Reason: "fixture"}}}, nil
	}
	if provider.oversized {
		huge, small := request.Sources[0], request.Sources[1]
		return ProviderResult{Units: []ProviderUnit{
			{Source: huge, Language: "fixture", AnalyzerID: provider.Descriptor().Identity,
				Mechanics: &Mechanics{Kind: "source", LOC: 1, Exports: []string{strings.Repeat("x", maxUnitDescriptorBytes+1)},
					Declarations: []Declaration{{Identity: "function:huge", Name: "huge", Kind: "function", Line: 1, EndLine: 1}}}},
			{Source: small, Language: "fixture", AnalyzerID: provider.Descriptor().Identity,
				Mechanics: &Mechanics{Kind: "source", LOC: 1,
					Declarations: []Declaration{{Identity: "function:small", Name: "small", Kind: "function", Line: 1, EndLine: 1}}}},
		}, Edges: []StructuralEdge{
			{FromKind: "symbol", FromRef: "test-portable-v1::function:small", Relation: "symbol_calls_symbol",
				ToKind: "symbol", ToRef: "test-portable-v1::function:huge", SourcePath: small.Path, SourceLine: 1, AnalyzerID: provider.Descriptor().Identity},
			{FromKind: "symbol", FromRef: "test-portable-v1::function:huge", Relation: "symbol_calls_symbol",
				ToKind: "symbol", ToRef: "test-portable-v1::function:small", SourcePath: huge.Path, SourceLine: 1, AnalyzerID: provider.Descriptor().Identity},
		}}, nil
	}
	return ProviderResult{Units: []ProviderUnit{{Source: request.Sources[0], Language: "fixture",
		AnalyzerID: provider.Descriptor().Identity, Mechanics: &Mechanics{Kind: "source", LOC: 1}}}}, nil
}

func TestAnalyzerDescriptorAdmissionRejectsOneUnitAndRemovesDanglingEdges(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"huge.fixture", "small.fixture"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte("value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	provider := &recordingManifestProvider{oversized: true}
	assembly, err := NewAnalyzerAssembly(provider)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewDescriberWithAssembly(root, nil, assembly).AnalyzeManifestSnapshotContext(
		context.Background(), []string{"huge.fixture", "small.fixture"},
		SnapshotIdentity{Protocol: "git-tree-v2", Digest: "git-tree-v2-sha256:exact"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Units) != 1 || result.Units[0].Path != "small.fixture" {
		t.Fatalf("admitted units=%+v", result.Units)
	}
	foundFailure := false
	for _, failure := range result.Failures {
		foundFailure = foundFailure || (failure.Path == "huge.fixture" && failure.Code == "descriptor_too_large")
	}
	if !foundFailure {
		t.Fatalf("oversized descriptor failure absent: %+v", result.Failures)
	}
	for _, edge := range result.Edges {
		if strings.Contains(edge.FromRef, "function:huge") || strings.Contains(edge.ToRef, "function:huge") || edge.SourcePath == "huge.fixture" {
			t.Fatalf("dangling oversized-unit edge retained: %+v", edge)
		}
	}
	foundPartial := false
	for _, coverage := range result.Coverage {
		if coverage.AnalyzerID == provider.Descriptor().Identity && coverage.Family == CapabilitySymbolDeclaration {
			foundPartial = coverage.State == "partial" && coverage.Errors == 1
		}
	}
	if !foundPartial {
		t.Fatalf("oversized descriptor coverage=%+v", result.Coverage)
	}
}

func TestAnalyzerCoverageDoesNotDoubleCountReportedUnitFailures(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.fixture"), []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &recordingManifestProvider{fail: true}
	assembly, err := NewAnalyzerAssembly(provider)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewDescriberWithAssembly(root, nil, assembly).AnalyzeManifestSnapshotContext(
		context.Background(), []string{"one.fixture"}, SnapshotIdentity{Protocol: "git-tree-v2", Digest: "git-tree-v2-sha256:exact"})
	if err != nil {
		t.Fatal(err)
	}
	for _, coverage := range result.Coverage {
		if coverage.AnalyzerID == provider.Descriptor().Identity && coverage.Family == CapabilitySymbolDeclaration {
			if coverage.Errors != 1 || coverage.State != "partial" {
				t.Fatalf("reported unit failure was counted more than once: %+v", coverage)
			}
			return
		}
	}
	t.Fatalf("missing provider coverage: %+v", result.Coverage)
}

func TestAnalyzerAssemblyCarriesExactSnapshotIdentityAndRejectsOmissions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.fixture"), []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &recordingManifestProvider{}
	assembly, err := NewAnalyzerAssembly(provider)
	if err != nil {
		t.Fatal(err)
	}
	describer := NewDescriberWithAssembly(root, nil, assembly)
	snapshot := SnapshotIdentity{Protocol: "git-tree-v2", Digest: "git-tree-v2-sha256:exact"}
	if _, err := describer.AnalyzeManifestSnapshotContext(context.Background(), []string{"one.fixture"}, snapshot); err != nil {
		t.Fatal(err)
	}
	if provider.request.SnapshotProtocol != snapshot.Protocol || provider.request.SnapshotDigest != snapshot.Digest ||
		provider.request.Root != root || len(provider.request.Sources) != 1 || provider.request.Sources[0].Path != "one.fixture" {
		t.Fatalf("provider request did not preserve exact source identity: %+v", provider.request)
	}

	provider.omit = true
	result, err := describer.AnalyzeManifestSnapshotContext(context.Background(), []string{"one.fixture"}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, coverage := range result.Coverage {
		if coverage.AnalyzerID == provider.Descriptor().Identity && coverage.Family == CapabilitySymbolDeclaration {
			found = coverage.State == "failed" && strings.Contains(coverage.Reason, "omitted claimed source")
		}
	}
	if !found {
		t.Fatalf("omitted source was not preserved as failed coverage: %+v", result.Coverage)
	}
}
