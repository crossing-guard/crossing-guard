// Package analyzerhost assembles the shipped compatibility provider and exact selected
// process modules for one daemon/CLI host. It is the only package that names a shipped
// language adapter; generic scan, store, and presentation code receive codemap ports.
package analyzerhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/codemap"
	"crossing-guard/codemap/adapters/golang"
	"crossing-guard/internal/analyzermodule"
)

// BuildResult always contains the compatibility assembly when that assembly itself is
// valid. SelectionError means no selected process module was activated.
type BuildResult struct {
	Assembly       *codemap.AnalyzerAssembly
	Selected       []analyzermodule.Package
	SelectionError error
}

// Compatibility returns the shipped analyzer assembly without reading any executable
// module selection. It exists for tests and recovery paths; production startup calls
// Build with the configured data directory.
func Compatibility() (*codemap.AnalyzerAssembly, error) {
	return codemap.NewAnalyzerAssembly(codemap.NewInProcessProvider(golang.New()))
}

// Build resolves exact user-scope module selection without executing it and freezes one
// deterministic assembly. Selected claims explicitly displace overlapping compatibility
// providers; unrelated compatibility providers remain active.
func Build(dataDir string) (BuildResult, error) {
	selected, selectionErr := analyzermodule.ResolveSelected(dataDir)
	providers := make([]codemap.ManifestProvider, 0, len(selected)+1)
	compatibility := codemap.NewInProcessProvider(golang.New())
	if !overlapsSelected(compatibility.Descriptor().Extensions, selected) {
		providers = append(providers, compatibility)
	}
	if selectionErr == nil {
		for _, installed := range selected {
			providers = append(providers, &processProvider{installed: installed})
		}
	} else {
		selected = nil
	}
	assembly, err := codemap.NewAnalyzerAssembly(providers...)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{Assembly: assembly, Selected: selected, SelectionError: selectionErr}, nil
}

func overlapsSelected(extensions []string, selected []analyzermodule.Package) bool {
	claims := map[string]bool{}
	for _, installed := range selected {
		for _, extension := range installed.Manifest.Extensions {
			claims[strings.ToLower(extension)] = true
		}
	}
	for _, extension := range extensions {
		if claims[strings.ToLower(extension)] {
			return true
		}
	}
	return false
}

type processProvider struct{ installed analyzermodule.Package }

func (provider *processProvider) Descriptor() codemap.ProviderDescriptor {
	manifest := provider.installed.Manifest
	return codemap.ProviderDescriptor{Kind: "selected-process-module", Identity: manifest.AnalyzerIdentity,
		BundleDigest: provider.installed.PackageDigest + "+" + provider.installed.EntrypointDigest,
		Languages:    manifest.Languages, Extensions: manifest.Extensions, Capabilities: manifest.Capabilities}
}

func (provider *processProvider) AnalyzeManifest(ctx context.Context, request codemap.ProviderRequest) (codemap.ProviderResult, error) {
	paths := make([]codemap.AnalyzerPath, 0, len(request.Sources))
	for _, source := range request.Sources {
		paths = append(paths, codemap.AnalyzerPath(source))
	}
	result, err := analyzermodule.Run(ctx, provider.installed, analyzermodule.ScanRequest{
		RequestID: request.RequestID, SnapshotProtocol: request.SnapshotProtocol,
		SnapshotDigest: request.SnapshotDigest, CheckoutRoot: request.Root, Paths: paths})
	if err != nil {
		return codemap.ProviderResult{}, err
	}
	converted := codemap.ProviderResult{Units: []codemap.ProviderUnit{}, Edges: []codemap.StructuralEdge{},
		Failures: []codemap.UnitFailure{}, Coverage: []codemap.CoverageResult{}}
	sourceByPath := map[string]codemap.SourceFile{}
	for _, source := range request.Sources {
		sourceByPath[source.Path] = source
	}
	for _, unit := range result.Units {
		mechanics, err := convertUnit(request.Root, sourceByPath[unit.Path], unit)
		if err != nil {
			return codemap.ProviderResult{}, err
		}
		converted.Units = append(converted.Units, codemap.ProviderUnit{Source: sourceByPath[unit.Path],
			Language: unit.Language, AnalyzerID: provider.installed.Manifest.AnalyzerIdentity, Mechanics: mechanics})
	}
	for _, edge := range result.Edges {
		converted.Edges = append(converted.Edges, codemap.StructuralEdge{FromKind: edge.FromKind,
			FromRef: edge.FromRef, Relation: edge.Relation, ToKind: edge.ToKind, ToRef: edge.ToRef,
			SourcePath: edge.SourcePath, SourceLine: edge.SourceLine,
			AnalyzerID: provider.installed.Manifest.AnalyzerIdentity})
	}
	for _, coverage := range result.Coverage {
		converted.Coverage = append(converted.Coverage, codemap.CoverageResult{Family: coverage.Family,
			State: coverage.State, AnalyzerID: provider.installed.Manifest.AnalyzerIdentity,
			Attempted: coverage.Attempted, Produced: coverage.Produced, Errors: coverage.Errors,
			Unresolved: coverage.Unresolved, Ambiguous: coverage.Ambiguous, Reason: coverage.Reason})
	}
	for _, failure := range result.Failures {
		converted.Failures = append(converted.Failures, codemap.UnitFailure{Path: failure.Path,
			AnalyzerID: provider.installed.Manifest.AnalyzerIdentity, Code: failure.Code, Reason: failure.Reason})
	}
	return converted, nil
}

func convertUnit(root string, source codemap.SourceFile, unit codemap.ModuleUnit) (*codemap.Mechanics, error) {
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(unit.Path)))
	if err != nil {
		return nil, fmt.Errorf("read module unit %s: %w", unit.Path, err)
	}
	hash := sha256.Sum256(body)
	if "sha256-v1:"+hex.EncodeToString(hash[:]) != source.SourceHash {
		return nil, fmt.Errorf("module unit %s source changed before conversion", unit.Path)
	}
	mechanics := &codemap.Mechanics{Namespace: unit.Namespace, Kind: unit.Kind,
		Exports: append([]string(nil), unit.Exports...), Imports: append([]string(nil), unit.Imports...),
		External: append([]string(nil), unit.External...), LOC: unit.LOC,
		Signals: append([]string(nil), unit.Signals...)}
	if unit.Cyclomatic != nil {
		mechanics.Cyclomatic = *unit.Cyclomatic
	}
	if unit.AbstractTypes != nil {
		mechanics.AbstractTypes = *unit.AbstractTypes
	}
	if unit.TotalTypes != nil {
		mechanics.TotalTypes = *unit.TotalTypes
	}
	for _, declaration := range unit.Declarations {
		if declaration.StartByte < 0 || declaration.EndByte > int64(len(body)) || declaration.EndByte <= declaration.StartByte {
			return nil, fmt.Errorf("module unit %s declaration %s has invalid byte range", unit.Path, declaration.Identity)
		}
		digest := sha256.Sum256(body[declaration.StartByte:declaration.EndByte])
		span := declaration.EndLine - declaration.Line + 1
		mechanics.Declarations = append(mechanics.Declarations, codemap.Declaration{Identity: declaration.Identity,
			Name: declaration.Name, Kind: declaration.Kind, Line: declaration.Line, EndLine: declaration.EndLine,
			SourceDigest: "sha256-v1:" + hex.EncodeToString(digest[:]), SourceSpanLines: &span,
			Cyclomatic: declaration.Cyclomatic, BodyShapeDigest: declaration.BodyShapeDigest,
			BodyShapeNodes: declaration.BodyShapeNodes})
	}
	return mechanics, nil
}
