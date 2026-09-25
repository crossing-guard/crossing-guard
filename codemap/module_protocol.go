package codemap

// The analyzer module protocol is the language-neutral process boundary. It contains
// mechanics only: module lifecycle/trust, process execution, persistence, configuration,
// and quality judgments belong to their existing owners outside this file.

const (
	AnalyzerManifestV1 = "crossing-guard-analyzer-manifest-v1"
	AnalyzerProtocolV1 = "crossing-guard-analyzer-stream-v1"
)

// AnalyzerModuleManifest is the inert static identity and capability claim of one
// installable analyzer package. Reading it never authorizes executing the entrypoint.
type AnalyzerModuleManifest struct {
	FormatVersion       string       `json:"format_version"`
	ModuleID            string       `json:"module_id"`
	ModuleVersion       string       `json:"module_version"`
	ProtocolVersion     string       `json:"protocol_version"`
	Entrypoint          string       `json:"entrypoint"`
	AnalyzerIdentity    string       `json:"analyzer_identity"`
	Languages           []string     `json:"languages"`
	Extensions          []string     `json:"extensions"`
	Capabilities        []Capability `json:"capabilities"`
	BodyShapeAlgorithms []string     `json:"body_shape_algorithms,omitempty"`
}

// AnalyzerScanStart pins one module invocation to an exact checkout observation.
type AnalyzerScanStart struct {
	ProtocolVersion  string `json:"protocol_version"`
	RequestID        string `json:"request_id"`
	SnapshotProtocol string `json:"snapshot_protocol"`
	SnapshotDigest   string `json:"snapshot_digest"`
	CheckoutRoot     string `json:"checkout_root"`
	PathLimit        int    `json:"path_limit"`
	UnitLimit        int    `json:"unit_limit"`
	EdgeLimit        int    `json:"edge_limit"`
}

// AnalyzerPath is one regular exact-manifest file offered to a selected module.
type AnalyzerPath struct {
	Path       string `json:"path"`
	SourceHash string `json:"source_hash"`
	Bytes      int64  `json:"bytes"`
	Lines      int    `json:"lines"`
}

// AnalyzerInputRecord is one framed request record.
type AnalyzerInputRecord struct {
	Type  string             `json:"type"`
	Start *AnalyzerScanStart `json:"start,omitempty"`
	Path  *AnalyzerPath      `json:"path,omitempty"`
}

// ModuleDeclaration is one analyzer-produced declaration range. Core computes the
// persisted exact source digest from StartByte/EndByte after validating the range.
type ModuleDeclaration struct {
	Identity        string `json:"identity"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Line            int    `json:"line"`
	EndLine         int    `json:"end_line"`
	StartByte       int64  `json:"start_byte"`
	EndByte         int64  `json:"end_byte"`
	Cyclomatic      *int   `json:"cyclomatic,omitempty"`
	BodyShapeDigest string `json:"body_shape_digest,omitempty"`
	BodyShapeNodes  int    `json:"body_shape_nodes,omitempty"`
}

// ModuleUnit is portable raw mechanics for one allow-listed path.
type ModuleUnit struct {
	Path          string              `json:"path"`
	Language      string              `json:"language"`
	Namespace     string              `json:"namespace,omitempty"`
	Kind          string              `json:"kind,omitempty"`
	Exports       []string            `json:"exports,omitempty"`
	Declarations  []ModuleDeclaration `json:"declarations,omitempty"`
	Imports       []string            `json:"imports,omitempty"`
	External      []string            `json:"external,omitempty"`
	LOC           int                 `json:"loc"`
	Cyclomatic    *int                `json:"cyclomatic,omitempty"`
	AbstractTypes *int                `json:"abstract_types,omitempty"`
	TotalTypes    *int                `json:"total_types,omitempty"`
	Signals       []string            `json:"signals,omitempty"`
}

// ModuleEdge is one portable typed relationship emitted by a module.
type ModuleEdge struct {
	FromKind   string `json:"from_kind"`
	FromRef    string `json:"from_ref"`
	Relation   string `json:"relation"`
	ToKind     string `json:"to_kind"`
	ToRef      string `json:"to_ref"`
	SourcePath string `json:"source_path"`
	SourceLine int    `json:"source_line"`
}

// ModuleCoverage reports one portable family for one analyzer invocation.
type ModuleCoverage struct {
	Family     Capability `json:"family"`
	State      string     `json:"state"`
	Attempted  int        `json:"attempted"`
	Produced   int        `json:"produced"`
	Errors     int        `json:"errors"`
	Unresolved int        `json:"unresolved"`
	Ambiguous  int        `json:"ambiguous"`
	Reason     string     `json:"reason,omitempty"`
}

// ModuleUnitFailure preserves a bounded named failure for one allow-listed path.
type ModuleUnitFailure struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// AnalyzerOutputRecord is one framed response record. Exactly one payload must match
// Type; lifecycle validation is owned by the process-module runner.
type AnalyzerOutputRecord struct {
	Type             string             `json:"type"`
	ProtocolVersion  string             `json:"protocol_version,omitempty"`
	RequestID        string             `json:"request_id,omitempty"`
	ModuleID         string             `json:"module_id,omitempty"`
	AnalyzerIdentity string             `json:"analyzer_identity,omitempty"`
	Unit             *ModuleUnit        `json:"unit,omitempty"`
	Edge             *ModuleEdge        `json:"edge,omitempty"`
	Coverage         *ModuleCoverage    `json:"coverage,omitempty"`
	Failure          *ModuleUnitFailure `json:"failure,omitempty"`
	UnitTotal        int                `json:"unit_total,omitempty"`
	EdgeTotal        int                `json:"edge_total,omitempty"`
	FailureTotal     int                `json:"failure_total,omitempty"`
}
