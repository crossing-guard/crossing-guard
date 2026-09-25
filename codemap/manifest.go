package codemap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	StructuralSchema       = "codemap-structural-v3"
	maxUnitDescriptorBytes = 256 << 10
)

type ManifestUnit struct {
	Path       string          `json:"path"`
	AnalyzerID string          `json:"analyzer_id"`
	Descriptor *UnitDescriptor `json:"descriptor"`
}

type StructuralEdge struct {
	FromKind   string `json:"from_kind"`
	FromRef    string `json:"from_ref"`
	Relation   string `json:"relation"`
	ToKind     string `json:"to_kind"`
	ToRef      string `json:"to_ref"`
	SourcePath string `json:"source_path,omitempty"`
	SourceLine int    `json:"source_line,omitempty"`
	AnalyzerID string `json:"analyzer_id"`
}

type UnitFailure struct {
	Path       string `json:"path"`
	AnalyzerID string `json:"analyzer_id,omitempty"`
	Code       string `json:"code"`
	Reason     string `json:"reason"`
}

type CoverageResult struct {
	Family     Capability `json:"family"`
	State      string     `json:"state"`
	AnalyzerID string     `json:"analyzer_id"`
	Attempted  int        `json:"attempted"`
	Produced   int        `json:"produced"`
	Errors     int        `json:"errors"`
	Unresolved int        `json:"unresolved,omitempty"`
	Ambiguous  int        `json:"ambiguous,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

type ManifestResult struct {
	PathTotal int              `json:"path_total"`
	Units     []ManifestUnit   `json:"units"`
	Edges     []StructuralEdge `json:"edges"`
	Failures  []UnitFailure    `json:"failures"`
	Coverage  []CoverageResult `json:"coverage"`
}

// SnapshotIdentity is observed by the source/checkpoint owner and carried unchanged
// through the analyzer boundary. A zero value is valid only for manual descriptor
// reads, where codemap records a source-set identity instead of claiming a Git snapshot.
type SnapshotIdentity struct {
	Protocol string
	Digest   string
}

type analyzedUnit struct {
	path       string
	hash       string
	language   string
	analyzerID string
	mech       *Mechanics
}

type coverageKey struct {
	analyzerID string
	family     Capability
}

const manifestHistoryBatch = 256

// AnalyzeManifest analyzes only the explicit sorted repository manifest given
// by the source owner. It never walks the filesystem to silently widen scope.
func (d *Describer) AnalyzeManifest(paths []string) (ManifestResult, error) {
	return d.AnalyzeManifestContext(context.Background(), paths)
}

// AnalyzeManifestContext is the cancellable production path shared by in-process
// compatibility analyzers and selected process-module providers.
func (d *Describer) AnalyzeManifestContext(ctx context.Context, paths []string) (ManifestResult, error) {
	return d.analyzeManifestContext(ctx, paths, SnapshotIdentity{})
}

// AnalyzeManifestSnapshotContext analyzes an explicit manifest under an exact source
// identity supplied by the checkpoint owner. The identity is never inferred here.
func (d *Describer) AnalyzeManifestSnapshotContext(ctx context.Context, paths []string, snapshot SnapshotIdentity) (ManifestResult, error) {
	if strings.TrimSpace(snapshot.Protocol) == "" || strings.TrimSpace(snapshot.Digest) == "" {
		return ManifestResult{}, fmt.Errorf("codemap snapshot analysis requires protocol and digest")
	}
	return d.analyzeManifestContext(ctx, paths, snapshot)
}

func (d *Describer) analyzeManifestContext(ctx context.Context, paths []string, snapshot SnapshotIdentity) (ManifestResult, error) {
	if len(paths) > 10000 {
		return ManifestResult{}, fmt.Errorf("codemap manifest has %d paths; maximum is 10000", len(paths))
	}
	unique := make(map[string]bool, len(paths))
	normalized := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.ToSlash(strings.TrimPrefix(path, "./"))
		if path == "" || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
			return ManifestResult{}, fmt.Errorf("codemap manifest contains unsafe path %q", path)
		}
		if !unique[path] {
			unique[path] = true
			normalized = append(normalized, path)
		}
	}
	sort.Strings(normalized)
	out := ManifestResult{PathTotal: len(normalized), Units: []ManifestUnit{}, Edges: []StructuralEdge{}, Failures: []UnitFailure{}, Coverage: []CoverageResult{}}
	assembly, err := d.analyzerAssembly()
	if err != nil {
		return ManifestResult{}, err
	}
	counts := map[coverageKey]*CoverageResult{}
	providerSources := map[ManifestProvider][]SourceFile{}
	for _, provider := range assembly.providerList() {
		descriptor := normalizedProviderDescriptor(provider.Descriptor())
		for _, family := range portableCapabilities() {
			row := &CoverageResult{Family: family, AnalyzerID: descriptor.Identity}
			if !providerClaims(descriptor, family) {
				row.State = "unsupported"
				row.Reason = "analyzer does not claim " + string(family)
			}
			counts[coverageKey{descriptor.Identity, family}] = row
		}
	}
	analyzed := make([]analyzedUnit, 0, len(normalized))
	for _, path := range normalized {
		if err := ctx.Err(); err != nil {
			return ManifestResult{}, err
		}
		provider := assembly.providerFor(path)
		if provider == nil {
			out.Failures = append(out.Failures, UnitFailure{Path: path, Code: "unsupported_analyzer",
				Reason: (&ErrNoAnalyzer{Path: path, Registered: assembly.Languages()}).Error()})
			continue
		}
		source, err := d.sourceFile(path)
		if err != nil {
			descriptor := provider.Descriptor()
			out.Failures = append(out.Failures, UnitFailure{Path: path, AnalyzerID: descriptor.Identity,
				Code: "source_read_failed", Reason: err.Error()})
			for _, family := range descriptor.Capabilities {
				counts[coverageKey{descriptor.Identity, family}].Errors++
			}
			continue
		}
		providerSources[provider] = append(providerSources[provider], source)
	}
	if snapshot.Protocol == "" {
		snapshot.Protocol = "codemap-source-set-v1"
		snapshot.Digest = digestSourceSet(normalized, providerSources)
	}
	for _, provider := range assembly.providerList() {
		descriptor := normalizedProviderDescriptor(provider.Descriptor())
		sources := providerSources[provider]
		for _, family := range descriptor.Capabilities {
			counts[coverageKey{descriptor.Identity, family}].Attempted = len(sources)
		}
		requestID := digestProviderRequest(assembly.Digest(), descriptor.Identity, snapshot, sources)
		providerResult, err := provider.AnalyzeManifest(ctx, ProviderRequest{RequestID: requestID,
			SnapshotProtocol: snapshot.Protocol, SnapshotDigest: snapshot.Digest, Root: d.root, Sources: sources})
		if err != nil {
			for _, source := range sources {
				out.Failures = append(out.Failures, UnitFailure{Path: source.Path, AnalyzerID: descriptor.Identity,
					Code: "provider_failed", Reason: err.Error()})
			}
			for _, family := range descriptor.Capabilities {
				row := counts[coverageKey{descriptor.Identity, family}]
				row.State, row.Errors, row.Reason = "failed", len(sources), err.Error()
			}
			continue
		}
		if err := validateProviderResult(descriptor, sources, providerResult); err != nil {
			for _, family := range descriptor.Capabilities {
				row := counts[coverageKey{descriptor.Identity, family}]
				row.State, row.Errors, row.Reason = "failed", len(sources), err.Error()
			}
			continue
		}
		out.Failures = append(out.Failures, providerResult.Failures...)
		out.Edges = append(out.Edges, providerResult.Edges...)
		for _, unit := range providerResult.Units {
			analyzed = append(analyzed, analyzedUnit{path: unit.Source.Path, hash: unit.Source.SourceHash,
				language: unit.Language, analyzerID: unit.AnalyzerID, mech: unit.Mechanics})
		}
		coverageProvided := map[Capability]bool{}
		for _, coverage := range providerResult.Coverage {
			coverageProvided[coverage.Family] = true
			row := counts[coverageKey{descriptor.Identity, coverage.Family}]
			row.State, row.Attempted, row.Errors = coverage.State, coverage.Attempted, coverage.Errors
			row.Unresolved, row.Ambiguous, row.Reason = coverage.Unresolved, coverage.Ambiguous, coverage.Reason
		}
		for range providerResult.Failures {
			for _, family := range descriptor.Capabilities {
				if !coverageProvided[family] {
					counts[coverageKey{descriptor.Identity, family}].Errors++
				}
			}
		}
	}

	graphInput := make(map[string]*Mechanics, len(analyzed))
	for _, unit := range analyzed {
		graphInput[unit.path] = unit.mech
	}
	graph := projectGraphFromMechanics(graphInput)
	historyPaths := make([]string, 0, len(analyzed))
	for _, unit := range analyzed {
		historyPaths = append(historyPaths, unit.path)
	}
	history := d.manifestEvolution(historyPaths)
	admitted := make([]analyzedUnit, 0, len(analyzed))
	rejectedUnits := map[string]bool{}
	rejectedSymbols := map[string]bool{}
	for _, unit := range analyzed {
		observation := history[unit.path]
		descriptor := d.assemble(unit.path, unit.hash, unit.language, unit.analyzerID, unit.mech, graph, &observation)
		body, err := json.Marshal(descriptor)
		if err != nil {
			return ManifestResult{}, fmt.Errorf("encode codemap descriptor for %s: %w", unit.path, err)
		}
		if len(body) <= maxUnitDescriptorBytes {
			admitted = append(admitted, unit)
			continue
		}
		rejectedUnits[manifestUnitKey(unit.analyzerID, unit.path)] = true
		for _, declaration := range unit.mech.Declarations {
			if declaration.Identity != "" {
				rejectedSymbols[unit.analyzerID+"::"+declaration.Identity] = true
			}
		}
		out.Failures = append(out.Failures, UnitFailure{Path: unit.path, AnalyzerID: unit.analyzerID,
			Code: "descriptor_too_large", Reason: fmt.Sprintf("descriptor is %d bytes; maximum is %d", len(body), maxUnitDescriptorBytes)})
		for _, family := range portableCapabilities() {
			if row := counts[coverageKey{unit.analyzerID, family}]; row != nil && row.State != "unsupported" {
				row.Errors++
				if row.Reason == "" {
					row.Reason = "one or more exact descriptors exceeded the portable size limit"
				}
			}
		}
	}
	if len(rejectedUnits) > 0 {
		filtered := out.Edges[:0]
		for _, edge := range out.Edges {
			sourceRejected := rejectedUnits[manifestUnitKey(edge.AnalyzerID, edge.SourcePath)] ||
				(edge.FromKind == "symbol" && rejectedSymbols[edge.FromRef])
			targetRejected := edge.ToKind == "symbol" && rejectedSymbols[edge.ToRef]
			if sourceRejected || targetRejected {
				if !sourceRejected && targetRejected && (edge.Relation == "symbol_calls_symbol" || edge.Relation == "file_references_symbol") {
					if row := counts[coverageKey{edge.AnalyzerID, CapabilitySymbolCall}]; row != nil && row.State != "unsupported" {
						row.Unresolved++
					}
				}
				continue
			}
			filtered = append(filtered, edge)
		}
		out.Edges = filtered
		graphInput = make(map[string]*Mechanics, len(admitted))
		historyPaths = historyPaths[:0]
		for _, unit := range admitted {
			graphInput[unit.path] = unit.mech
			historyPaths = append(historyPaths, unit.path)
		}
		graph = projectGraphFromMechanics(graphInput)
		history = d.manifestEvolution(historyPaths)
	}
	analyzed = admitted
	for _, unit := range analyzed {
		observation := history[unit.path]
		desc := d.assemble(unit.path, unit.hash, unit.language, unit.analyzerID, unit.mech, graph, &observation)
		out.Units = append(out.Units, ManifestUnit{Path: unit.path, AnalyzerID: unit.analyzerID, Descriptor: desc})
		if unit.mech.Namespace != "" {
			out.Edges = append(out.Edges, StructuralEdge{FromKind: "file", FromRef: unit.path, Relation: "file_in_package", ToKind: "package", ToRef: unit.mech.Namespace, SourcePath: unit.path, AnalyzerID: unit.analyzerID})
		}
		imports := append([]string(nil), unit.mech.Imports...)
		sort.Strings(imports)
		for i, imported := range imports {
			if unit.mech.Namespace == "" || imported == "" || (i > 0 && imported == imports[i-1]) {
				continue
			}
			out.Edges = append(out.Edges, StructuralEdge{FromKind: "package", FromRef: unit.mech.Namespace, Relation: "package_depends_on", ToKind: "package", ToRef: imported, SourcePath: unit.path, AnalyzerID: unit.analyzerID})
		}
		declarations := append([]Declaration(nil), unit.mech.Declarations...)
		sort.Slice(declarations, func(i, j int) bool {
			if declarations[i].Name != declarations[j].Name {
				return declarations[i].Name < declarations[j].Name
			}
			return declarations[i].Line < declarations[j].Line
		})
		for _, declaration := range declarations {
			ref := declaration.Name
			if declaration.Identity != "" {
				ref = unit.analyzerID + "::" + declaration.Identity
			}
			out.Edges = append(out.Edges, StructuralEdge{FromKind: "file", FromRef: unit.path, Relation: "file_declares_symbol", ToKind: "symbol", ToRef: ref, SourcePath: unit.path, SourceLine: declaration.Line, AnalyzerID: unit.analyzerID})
			if declaration.BodyShapeDigest != "" {
				if row := counts[coverageKey{unit.analyzerID, CapabilityResponsibilityFingerprint}]; row != nil && row.State != "unsupported" {
					row.Produced++
				}
			}
		}
	}
	for _, edge := range out.Edges {
		var family Capability
		switch edge.Relation {
		case "package_depends_on", "file_in_package":
			family = CapabilityPackageDependency
		case "file_declares_symbol":
			family = CapabilitySymbolDeclaration
		case "symbol_calls_symbol", "file_references_symbol":
			family = CapabilitySymbolCall
		}
		if row := counts[coverageKey{edge.AnalyzerID, family}]; row != nil {
			row.Produced++
		}
	}
	for _, row := range counts {
		if row.State == "unsupported" || row.State == "failed" {
			out.Coverage = append(out.Coverage, *row)
			continue
		}
		if row.State == "partial" || row.Errors > 0 || row.Unresolved > 0 || row.Ambiguous > 0 {
			row.State = "partial"
			if row.Reason == "" {
				row.Reason = "one or more claimed facts could not be analyzed or resolved"
			}
		} else {
			row.State = "complete"
		}
		out.Coverage = append(out.Coverage, *row)
	}
	sort.Slice(out.Edges, func(i, j int) bool {
		a, b := out.Edges[i], out.Edges[j]
		ak := a.FromKind + "\x00" + a.FromRef + "\x00" + a.Relation + "\x00" + a.ToKind + "\x00" + a.ToRef + "\x00" + a.SourcePath + fmt.Sprintf("\x00%09d\x00", a.SourceLine) + a.AnalyzerID
		bk := b.FromKind + "\x00" + b.FromRef + "\x00" + b.Relation + "\x00" + b.ToKind + "\x00" + b.ToRef + "\x00" + b.SourcePath + fmt.Sprintf("\x00%09d\x00", b.SourceLine) + b.AnalyzerID
		return ak < bk
	})
	sort.Slice(out.Coverage, func(i, j int) bool {
		if out.Coverage[i].Family != out.Coverage[j].Family {
			return out.Coverage[i].Family < out.Coverage[j].Family
		}
		return out.Coverage[i].AnalyzerID < out.Coverage[j].AnalyzerID
	})
	return out, nil
}

func manifestUnitKey(analyzerID, path string) string {
	return analyzerID + "\x00" + path
}

func digestSourceSet(paths []string, grouped map[ManifestProvider][]SourceFile) string {
	byPath := map[string]SourceFile{}
	for _, sources := range grouped {
		for _, source := range sources {
			byPath[source.Path] = source
		}
	}
	hash := sha256.New()
	for _, path := range paths {
		source := byPath[path]
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%d\n", path, source.SourceHash, source.Bytes, source.Lines)
	}
	return "sha256-v1:" + hex.EncodeToString(hash.Sum(nil))
}

func digestProviderRequest(assemblyDigest, analyzerID string, snapshot SnapshotIdentity, sources []SourceFile) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%s\n", assemblyDigest, analyzerID, snapshot.Protocol, snapshot.Digest)
	for _, source := range sources {
		_, _ = fmt.Fprintf(hash, "%s\x00%s\n", source.Path, source.SourceHash)
	}
	return "sha256-v1:" + hex.EncodeToString(hash.Sum(nil))
}

func portableCapabilities() []Capability {
	return []Capability{CapabilityPackageDependency, CapabilitySymbolDeclaration,
		CapabilityResponsibilityFingerprint, CapabilitySymbolCall}
}

func providerClaims(descriptor ProviderDescriptor, wanted Capability) bool {
	for _, capability := range descriptor.Capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func (d *Describer) sourceFile(path string) (SourceFile, error) {
	body, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(path)))
	if err != nil {
		return SourceFile{}, fmt.Errorf("codemap: cannot read %s: %w", path, err)
	}
	hash := sha256.Sum256(body)
	lines := 0
	if len(body) > 0 {
		lines = bytes.Count(body, []byte{'\n'}) + 1
	}
	return SourceFile{Path: path, SourceHash: "sha256-v1:" + hex.EncodeToString(hash[:]),
		Bytes: int64(len(body)), Lines: lines}, nil
}

func validateProviderResult(descriptor ProviderDescriptor, sources []SourceFile, result ProviderResult) error {
	allowed := map[string]SourceFile{}
	for _, source := range sources {
		allowed[source.Path] = source
	}
	seen := map[string]bool{}
	for _, unit := range result.Units {
		source, ok := allowed[unit.Source.Path]
		if !ok || seen[unit.Source.Path] || unit.Source.SourceHash != source.SourceHash ||
			unit.AnalyzerID != descriptor.Identity || unit.Mechanics == nil {
			return fmt.Errorf("analyzer provider %s returned invalid unit %q", descriptor.Identity, unit.Source.Path)
		}
		seen[unit.Source.Path] = true
	}
	for _, edge := range result.Edges {
		if edge.AnalyzerID != descriptor.Identity || allowed[edge.SourcePath].Path == "" {
			return fmt.Errorf("analyzer provider %s returned invalid edge source", descriptor.Identity)
		}
	}
	for _, failure := range result.Failures {
		if failure.AnalyzerID != descriptor.Identity || allowed[failure.Path].Path == "" || seen[failure.Path] {
			return fmt.Errorf("analyzer provider %s returned invalid failure", descriptor.Identity)
		}
		seen[failure.Path] = true
	}
	for _, source := range sources {
		if !seen[source.Path] {
			return fmt.Errorf("analyzer provider %s omitted claimed source %q", descriptor.Identity, source.Path)
		}
	}
	for _, coverage := range result.Coverage {
		if coverage.AnalyzerID != descriptor.Identity || !providerClaims(descriptor, coverage.Family) {
			return fmt.Errorf("analyzer provider %s returned invalid coverage", descriptor.Identity)
		}
	}
	return nil
}

// manifestEvolution collects current-path history in bounded batches. Git cannot
// apply --follow semantics to multiple paths, so every resulting descriptor says
// that renames were not followed. Failures remain descriptor limitations and do
// not erase otherwise measured structural facts.
func (d *Describer) manifestEvolution(paths []string) map[string]evolutionObservation {
	out := make(map[string]evolutionObservation, len(paths))
	for _, path := range paths {
		out[path] = evolutionObservation{available: true, currentPath: true}
	}
	for start := 0; start < len(paths); start += manifestHistoryBatch {
		end := start + manifestHistoryBatch
		if end > len(paths) {
			end = len(paths)
		}
		batch := paths[start:end]
		args := []string{"log", "--format=%x1e%H%x00%aI", "--name-only", "-z", "--no-renames", "--"}
		args = append(args, batch...)
		raw, err := d.gitLimited(manifestHistoryOutputLimit, args...)
		if err != nil {
			for _, path := range batch {
				out[path] = evolutionObservation{currentPath: true, limitation: "bounded manifest git history unavailable, so age and churn are unknown: " + err.Error()}
			}
			continue
		}
		applyManifestHistory(raw, batch, out)
	}
	return out
}

func applyManifestHistory(raw []byte, paths []string, out map[string]evolutionObservation) {
	allowed := make(map[string]bool, len(paths))
	for _, path := range paths {
		allowed[path] = true
	}
	for _, record := range bytes.Split(raw, []byte{0x1e}) {
		fields := bytes.Split(record, []byte{0})
		if len(fields) < 3 {
			continue
		}
		date := string(fields[1])
		seen := map[string]bool{}
		for index, field := range fields[2:] {
			if index == 0 && len(field) > 0 && field[0] == '\n' {
				field = field[1:]
			}
			path := string(field)
			if path == "" || !allowed[path] || seen[path] {
				continue
			}
			seen[path] = true
			observation := out[path]
			if observation.evolution.Commits == 0 {
				observation.evolution.LastSeen = date
			}
			observation.evolution.FirstSeen = date
			observation.evolution.Commits++
			out[path] = observation
		}
	}
}

func projectGraphFromMechanics(units map[string]*Mechanics) *projectGraph {
	g := &projectGraph{fanIn: map[string]int{}, pkg: map[string]*pkgFacts{}}
	for _, mech := range units {
		facts := g.pkg[mech.Namespace]
		if facts == nil {
			facts = &pkgFacts{imports: map[string]bool{}}
			g.pkg[mech.Namespace] = facts
		}
		facts.abstractTypes += mech.AbstractTypes
		facts.totalTypes += mech.TotalTypes
		seen := map[string]bool{}
		for _, imported := range mech.Imports {
			facts.imports[imported] = true
			if !seen[imported] {
				seen[imported] = true
				g.fanIn[imported]++
			}
		}
	}
	return g
}
