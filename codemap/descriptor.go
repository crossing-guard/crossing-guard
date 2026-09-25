// Package codemap is the code-understanding framework: one normalized descriptor
// for a unit of code, fed by whichever analyzer fits the file
// (console-and-info-panel §6).
//
// THE FRAMEWORK NAMES NO LANGUAGE AND NO FRAMEWORK. Three strictly separated
// levels, and this package is only the first:
//
//	framework core     — this package: the ontology, the descriptor, the analyzer
//	                     PORT, the role MECHANISM, the cache. Names nothing.
//	language adapter   — implements the port for one language; the only code that
//	                     may name a language, and only its own.
//	convention config  — declarative data mapping a stack's conventions onto the
//	                     role taxonomy. The ONLY place a framework name (laravel,
//	                     symfony, rails) may ever appear, and it is data, not code.
//
// So the pairing is language ↔ language and framework ↔ framework — never
// "Laravel vs Go", which compares a framework to a language. Adding a language is
// ADDING an adapter behind the factory; adding a framework is adding a config
// file. If a language identifier appears in this package, the seam has leaked
// (the vendor-abstraction-review bar, ADR 0020's port pattern applied to code
// analysis).
package codemap

// Provenance says HOW a fact was obtained, so the panel can never show a guess
// as a measurement. Three values, deliberately few.
//
// This is NOT the governance provenance enum (observed | identity-derived |
// user-asserted). That one describes what an AGENT did; this one describes how a
// CODE fact was produced. An scc line count is not "an observation of the agent",
// and conflating the two would corrupt both vocabularies.
type Provenance string

const (
	// Measured — a tool computed it (a parser, git, a metrics binary).
	Measured Provenance = "measured"
	// Inferred — a heuristic or convention rule judged it (a role assignment).
	Inferred Provenance = "inferred"
	// Generated — a model wrote it (a local summary). Never a measurement.
	Generated Provenance = "generated"
)

// Identity locates a unit. `Language` is a value supplied by whichever adapter
// claimed the file — core stores it, and never branches on it.
type Identity struct {
	Path       string `json:"path"`
	Language   string `json:"language,omitempty"`
	AnalyzerID string `json:"analyzer_id,omitempty"`
	Namespace  string `json:"namespace,omitempty"` // package / namespace / module
}

// Symbol is the L2 surface: what this unit declares.
type Symbol struct {
	Kind         string        `json:"kind,omitempty"` // LSP SymbolKind vocabulary
	Exports      []string      `json:"exports,omitempty"`
	Declarations []Declaration `json:"declarations,omitempty"`
}

// Structure is L2 shape: who depends on whom. Ca/Ce at unit granularity — the
// same edges a call hierarchy shows at symbol granularity (§6).
type Structure struct {
	Imports  []string `json:"imports,omitempty"`  // outgoing, internal only
	Callers  []string `json:"callers,omitempty"`  // units importing this one
	FanIn    int      `json:"fan_in"`             // Ca — afferent coupling
	FanOut   int      `json:"fan_out"`            // Ce — efferent coupling
	External []string `json:"external,omitempty"` // outgoing, outside the project
}

// Metrics is L3. Instability and Distance are computed here because they are
// arithmetic over Ca/Ce, not language knowledge:
//
//	I = Ce / (Ce + Ca)          instability
//	D = |A + I - 1|             distance from the main sequence
//
// D is what says whether a unit sits at the right level of abstraction — "level
// of abstraction" is a number, not a vibe (§6).
type Metrics struct {
	LOC          int     `json:"loc"`
	Cyclomatic   int     `json:"cyclomatic,omitempty"`
	Abstractness float64 `json:"abstractness"` // A: abstract types / total
	Instability  float64 `json:"instability"`  // I
	Distance     float64 `json:"distance"`     // D
}

// RoleSource says where a role came from. Distinct from Provenance: it records
// which MECHANISM assigned the role, while Provenance records how much to trust
// the fact at all.
type RoleSource string

const (
	RoleFromConvention RoleSource = "convention" // a config rule matched
	RoleDeclared       RoleSource = "declared"   // the code says so explicitly
	RoleInferred       RoleSource = "inferred"   // a heuristic over mechanics
	RoleUnknown        RoleSource = "unknown"    // nothing matched — say so
)

// Role is L4, the only altitude that is not auto-derivable (§6). Core owns the
// MECHANISM ("does a role exist, and what assigned it"); the taxonomy and the
// rules are config.
type Role struct {
	Layer  string     `json:"layer,omitempty"`
	Source RoleSource `json:"source"`
	// Rule names the config rule that matched, so an assignment is auditable
	// rather than magic.
	Rule string `json:"rule,omitempty"`
	// Why states the signal in words, for the panel.
	Why string `json:"why,omitempty"`
}

// Evolution is history. Language-blind by nature: git does not care what the
// file is written in, which is why this lives in core rather than an adapter.
type Evolution struct {
	Commits   int    `json:"commits"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
	IsNew     bool   `json:"is_new"`
	// Hotspot is decode-only compatibility for descriptors generated before C9e.
	// New analyzers do not derive or present this unselected quality judgment.
	Hotspot string `json:"hotspot,omitempty"`
}

// IntentSource distinguishes prose a human already wrote from prose a model
// produced. The difference matters more than the text.
type IntentSource string

const (
	IntentDocLinked IntentSource = "doc-linked" // a design doc or tracker item says it
	IntentGenerated IntentSource = "llm"        // a local model wrote it
)

// Intent is the L4 semantic layer: what the unit is SUPPOSED to do.
type Intent struct {
	Summary string       `json:"summary,omitempty"`
	Source  IntentSource `json:"source,omitempty"`
	// Cite points at the doc that states it, so doc-linked intent is checkable.
	Cite string `json:"cite,omitempty"`
}

// UnitDescriptor is the normalized answer for one unit, whatever produced it.
// The panel renders THIS and never knows which adapter ran.
type UnitDescriptor struct {
	Identity  Identity  `json:"identity"`
	Symbol    Symbol    `json:"symbol"`
	Structure Structure `json:"structure"`
	Metrics   Metrics   `json:"metrics"`
	Role      Role      `json:"role"`
	Evolution Evolution `json:"evolution"`
	Intent    Intent    `json:"intent"`

	// Provenance tags fields by dotted name ("metrics.loc", "role.layer") so the
	// panel can label every value it displays. A field absent from this map has
	// no claimed provenance and must be rendered as unknown, not as measured.
	Provenance map[string]Provenance `json:"provenance,omitempty"`

	// Notes record what could NOT be determined and why. An empty descriptor
	// field and an unavailable analyzer look identical otherwise, and only one of
	// them is the truth.
	Notes []string `json:"notes,omitempty"`

	// SourceHash is the content hash the descriptor was computed at, so a reader
	// (or a later policy) can tell whether it predates the working tree.
	SourceHash string `json:"source_hash,omitempty"`
}

// tag records a field's provenance.
func (d *UnitDescriptor) tag(field string, p Provenance) {
	if d.Provenance == nil {
		d.Provenance = map[string]Provenance{}
	}
	d.Provenance[field] = p
}

// note appends an honest gap.
func (d *UnitDescriptor) note(msg string) { d.Notes = append(d.Notes, msg) }
