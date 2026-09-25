package codemap

// The analyzer PORT and its factory (console-and-info-panel §6, the ADR 0020
// infer.Backend pattern applied to code analysis).
//
// Core RESOLVES an adapter; it never imports one. That is the whole open/closed
// property: adding a language is adding a package that calls Register, and no
// file in this package changes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Mechanics is everything an adapter is allowed to know: the raw shape of a
// file. Note what is NOT here — no role, no layer, no architectural judgement.
// An adapter parses; it does not decide what a "controller" is. Role assignment
// rides on top as convention config, so the same adapter serves any number of
// frameworks (§6: "mechanics from the adapter, assignment from config").
type Mechanics struct {
	Namespace    string        // package / namespace this unit belongs to
	Kind         string        // LSP SymbolKind vocabulary, if the adapter can say
	Exports      []string      // the public surface
	Declarations []Declaration // mechanically declared symbols, with source lines
	Imports      []string      // internal dependencies, as namespaces or paths
	External     []string      // dependencies outside the project
	LOC          int
	Cyclomatic   int
	// AbstractTypes / TotalTypes feed Martin's A. An adapter that cannot compute
	// them leaves both zero and core records no abstractness rather than
	// inventing one.
	AbstractTypes int
	TotalTypes    int
	// Symbols the config may match on without core knowing what they mean —
	// e.g. a base class, an embedded type, a registered handler. Opaque strings
	// to core, meaningful only to a convention config.
	Signals []string
}

// Declaration is a source-declared symbol. Identity and Kind are adapter-defined
// mechanical values; core assigns no language semantics to either. Optional numeric
// facts are pointers so unsupported never collapses into a measured zero.
type Declaration struct {
	Identity        string `json:"identity,omitempty"`
	Name            string `json:"name"`
	Kind            string `json:"kind,omitempty"`
	Line            int    `json:"line,omitempty"`
	EndLine         int    `json:"end_line,omitempty"`
	SourceDigest    string `json:"source_digest,omitempty"`
	SourceSpanLines *int   `json:"source_span_lines,omitempty"`
	Cyclomatic      *int   `json:"cyclomatic,omitempty"`
	BodyShapeDigest string `json:"body_shape_digest,omitempty"`
	BodyShapeNodes  int    `json:"body_shape_nodes,omitempty"`
}

// Capability is a structural fact family an analyzer can mechanically produce.
// It is framework vocabulary, not a project policy or architectural convention.
type Capability string

const (
	CapabilityPackageDependency         Capability = "package_dependency"
	CapabilitySymbolDeclaration         Capability = "symbol_declaration"
	CapabilityResponsibilityFingerprint Capability = "responsibility_fingerprint"
	CapabilitySymbolCall                Capability = "symbol_call"
)

// Analyzer is the port. One implementation per language, each naming ONLY the
// language it adapts.
type Analyzer interface {
	// Identity changes whenever mechanics or emitted identifiers change. Durable
	// generations use it to refuse silent reinterpretation by a newer adapter.
	Identity() string
	// Coverage names the structural families this adapter can actually produce.
	Coverage() []Capability
	// Language is the key this adapter registers under. It is the one string in
	// the whole system that names a language in code, and it belongs to the
	// adapter — never to core.
	Language() string
	// Extensions the adapter claims. Core uses these to route a file without
	// knowing what any of them mean.
	Extensions() []string
	// Analyze returns raw mechanics for one file, relative to root.
	Analyze(root, rel string) (*Mechanics, error)
}

var (
	registryMu sync.RWMutex
	analyzers  = map[string]Analyzer{}
)

// Register adds an adapter to the factory. Calling it twice for the same
// language replaces the earlier one, so a host can substitute an adapter without
// core changing.
func Register(a Analyzer) {
	if a == nil || a.Language() == "" || a.Identity() == "" {
		panic("codemap: analyzer must report language and stable identity")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	analyzers[a.Language()] = a
}

// AnalyzerBundleDigest identifies the exact registered adapter assembly. It is
// deterministic and contains no project convention selection.
func AnalyzerBundleDigest() string {
	type identity struct {
		Language     string       `json:"language"`
		Identity     string       `json:"identity"`
		Extensions   []string     `json:"extensions"`
		Capabilities []Capability `json:"capabilities"`
	}
	registryMu.RLock()
	rows := make([]identity, 0, len(analyzers))
	for _, analyzer := range analyzers {
		exts := append([]string(nil), analyzer.Extensions()...)
		caps := append([]Capability(nil), analyzer.Coverage()...)
		sort.Strings(exts)
		sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
		rows = append(rows, identity{analyzer.Language(), analyzer.Identity(), exts, caps})
	}
	registryMu.RUnlock()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Language != rows[j].Language {
			return rows[i].Language < rows[j].Language
		}
		return rows[i].Identity < rows[j].Identity
	})
	body, _ := json.Marshal(rows)
	sum := sha256.Sum256(body)
	return "sha256-v1:" + hex.EncodeToString(sum[:])
}

// Languages lists what is currently bound, so a caller can state the gap
// honestly instead of showing an empty descriptor.
func Languages() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(analyzers))
	for k := range analyzers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// analyzerFor resolves the adapter that claims this file's extension. Core does
// not map extensions to languages itself — the adapter declares its own, so the
// mapping table lives with the code that understands it.
func analyzerFor(rel string) (Analyzer, bool) {
	dot := strings.LastIndex(rel, ".")
	if dot < 0 {
		return nil, false
	}
	ext := strings.ToLower(rel[dot:])
	registryMu.RLock()
	defer registryMu.RUnlock()
	for _, a := range analyzers {
		for _, e := range a.Extensions() {
			if strings.EqualFold(e, ext) {
				return a, true
			}
		}
	}
	return nil, false
}

// ErrNoAnalyzer says no adapter claimed this file. It is a normal state — most
// projects have files nobody analyzes — and callers report it rather than
// pretending the descriptor is empty because the file is empty.
type ErrNoAnalyzer struct {
	Path       string
	Registered []string
}

func (e *ErrNoAnalyzer) Error() string {
	if len(e.Registered) == 0 {
		return fmt.Sprintf("no analyzer is registered for %s (none are bound at all)", e.Path)
	}
	return fmt.Sprintf("no analyzer claims %s (registered: %s)", e.Path, strings.Join(e.Registered, ", "))
}
