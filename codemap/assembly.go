package codemap

// AnalyzerAssembly is the immutable production routing boundary shared by scans and
// descriptor reads. Language implementations supply providers; core validates claims,
// routes extensions, and hashes the exact assembly without naming a language.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SourceFile is one core-observed allow-listed regular file offered to a batch provider.
type SourceFile struct {
	Path       string
	SourceHash string
	Bytes      int64
	Lines      int
}

// ProviderDescriptor is the deterministic language-neutral identity and claim set of a
// manifest-scoped analyzer provider.
type ProviderDescriptor struct {
	Kind         string       `json:"kind"`
	Identity     string       `json:"identity"`
	BundleDigest string       `json:"bundle_digest"`
	Languages    []string     `json:"languages"`
	Extensions   []string     `json:"extensions"`
	Capabilities []Capability `json:"capabilities"`
}

// ProviderUnit is raw mechanics for one core-observed source file.
type ProviderUnit struct {
	Source     SourceFile
	Language   string
	AnalyzerID string
	Mechanics  *Mechanics
}

// ProviderResult is one atomic provider result. A provider error means none of these
// facts are accepted by the caller.
type ProviderResult struct {
	Units    []ProviderUnit
	Edges    []StructuralEdge
	Failures []UnitFailure
	Coverage []CoverageResult
}

// ProviderRequest is the complete immutable input identity offered to one provider.
// Snapshot identity is observed by the source owner; providers must not derive or
// reinterpret it from the subset of extensions they happen to claim.
type ProviderRequest struct {
	RequestID        string
	SnapshotProtocol string
	SnapshotDigest   string
	Root             string
	Sources          []SourceFile
}

// ManifestProvider analyzes all claimed paths in one invocation.
type ManifestProvider interface {
	Descriptor() ProviderDescriptor
	AnalyzeManifest(context.Context, ProviderRequest) (ProviderResult, error)
}

// AnalyzerAssembly contains exact deterministic providers and extension routing.
type AnalyzerAssembly struct {
	providers   []ManifestProvider
	byExtension map[string]ManifestProvider
	digest      string
}

// NewAnalyzerAssembly validates claims and freezes deterministic provider order.
func NewAnalyzerAssembly(providers ...ManifestProvider) (*AnalyzerAssembly, error) {
	ordered := append([]ManifestProvider(nil), providers...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i].Descriptor(), ordered[j].Descriptor()
		if left.Identity != right.Identity {
			return left.Identity < right.Identity
		}
		return left.BundleDigest < right.BundleDigest
	})
	assembly := &AnalyzerAssembly{providers: ordered, byExtension: map[string]ManifestProvider{}}
	descriptors := make([]ProviderDescriptor, 0, len(ordered))
	identities := map[string]bool{}
	for _, provider := range ordered {
		if provider == nil {
			return nil, fmt.Errorf("codemap analyzer assembly contains nil provider")
		}
		descriptor := normalizedProviderDescriptor(provider.Descriptor())
		if err := validateProviderDescriptor(descriptor); err != nil {
			return nil, err
		}
		if identities[descriptor.Identity] {
			return nil, fmt.Errorf("codemap analyzer assembly contains duplicate identity %q", descriptor.Identity)
		}
		identities[descriptor.Identity] = true
		for _, extension := range descriptor.Extensions {
			if previous := assembly.byExtension[extension]; previous != nil {
				return nil, fmt.Errorf("codemap analyzer providers %s and %s both claim extension %s",
					previous.Descriptor().Identity, descriptor.Identity, extension)
			}
			assembly.byExtension[extension] = provider
		}
		descriptors = append(descriptors, descriptor)
	}
	body, err := json.Marshal(descriptors)
	if err != nil {
		return nil, fmt.Errorf("encode analyzer assembly identity: %w", err)
	}
	hash := sha256.Sum256(body)
	assembly.digest = "sha256-v1:" + hex.EncodeToString(hash[:])
	return assembly, nil
}

// Digest identifies the exact provider assembly used by a scan/read.
func (assembly *AnalyzerAssembly) Digest() string {
	if assembly == nil {
		return ""
	}
	return assembly.digest
}

// Languages returns the opaque sorted claimed language IDs.
func (assembly *AnalyzerAssembly) Languages() []string {
	if assembly == nil {
		return []string{}
	}
	seen := map[string]bool{}
	for _, provider := range assembly.providers {
		for _, language := range provider.Descriptor().Languages {
			seen[language] = true
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func (assembly *AnalyzerAssembly) providerFor(path string) ManifestProvider {
	if assembly == nil {
		return nil
	}
	index := strings.LastIndex(path, ".")
	if index < 0 {
		return nil
	}
	return assembly.byExtension[strings.ToLower(path[index:])]
}

func (assembly *AnalyzerAssembly) providerList() []ManifestProvider {
	if assembly == nil {
		return []ManifestProvider{}
	}
	return append([]ManifestProvider(nil), assembly.providers...)
}

func normalizedProviderDescriptor(descriptor ProviderDescriptor) ProviderDescriptor {
	descriptor.Languages = append([]string(nil), descriptor.Languages...)
	descriptor.Extensions = append([]string(nil), descriptor.Extensions...)
	descriptor.Capabilities = append([]Capability(nil), descriptor.Capabilities...)
	for index := range descriptor.Extensions {
		descriptor.Extensions[index] = strings.ToLower(descriptor.Extensions[index])
	}
	sort.Strings(descriptor.Languages)
	sort.Strings(descriptor.Extensions)
	sort.Slice(descriptor.Capabilities, func(i, j int) bool {
		return descriptor.Capabilities[i] < descriptor.Capabilities[j]
	})
	return descriptor
}

func validateProviderDescriptor(descriptor ProviderDescriptor) error {
	if descriptor.Kind == "" || descriptor.Identity == "" || descriptor.BundleDigest == "" ||
		len(descriptor.Languages) == 0 || len(descriptor.Extensions) == 0 {
		return fmt.Errorf("codemap analyzer provider has incomplete identity or claims")
	}
	seen := map[string]bool{}
	for _, extension := range descriptor.Extensions {
		if len(extension) < 2 || extension[0] != '.' || extension != strings.ToLower(extension) || seen[extension] {
			return fmt.Errorf("codemap analyzer provider %s has invalid extension %q", descriptor.Identity, extension)
		}
		seen[extension] = true
	}
	return nil
}

type inProcessProvider struct{ analyzer Analyzer }

// NewInProcessProvider wraps one compatibility analyzer behind the same immutable batch
// provider contract. New user modules do not use this compatibility constructor.
func NewInProcessProvider(analyzer Analyzer) ManifestProvider {
	return &inProcessProvider{analyzer: analyzer}
}

func (provider *inProcessProvider) Descriptor() ProviderDescriptor {
	return ProviderDescriptor{Kind: "in-process-compatibility", Identity: provider.analyzer.Identity(),
		BundleDigest: provider.analyzer.Identity(), Languages: []string{provider.analyzer.Language()},
		Extensions: provider.analyzer.Extensions(), Capabilities: provider.analyzer.Coverage()}
}

func (provider *inProcessProvider) AnalyzeManifest(ctx context.Context, request ProviderRequest) (ProviderResult, error) {
	result := ProviderResult{Units: []ProviderUnit{}, Edges: []StructuralEdge{}, Failures: []UnitFailure{}, Coverage: []CoverageResult{}}
	for _, source := range request.Sources {
		if err := ctx.Err(); err != nil {
			return ProviderResult{}, err
		}
		mechanics, err := provider.analyzer.Analyze(request.Root, source.Path)
		if err != nil {
			result.Failures = append(result.Failures, UnitFailure{Path: source.Path,
				AnalyzerID: provider.analyzer.Identity(), Code: "analysis_failed", Reason: err.Error()})
			continue
		}
		result.Units = append(result.Units, ProviderUnit{Source: source, Language: provider.analyzer.Language(),
			AnalyzerID: provider.analyzer.Identity(), Mechanics: mechanics})
	}
	return result, nil
}

// RegisteredAnalyzerAssembly freezes the compatibility registry for legacy callers.
// Production hosts should construct and inject one assembly rather than mutate globals.
func RegisteredAnalyzerAssembly() (*AnalyzerAssembly, error) {
	registryMu.RLock()
	providers := make([]ManifestProvider, 0, len(analyzers))
	for _, analyzer := range analyzers {
		providers = append(providers, NewInProcessProvider(analyzer))
	}
	registryMu.RUnlock()
	return NewAnalyzerAssembly(providers...)
}
