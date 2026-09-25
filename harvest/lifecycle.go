package harvest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// LifecycleNormalizer is an optional vendor capability for exact tool completion and
// mechanically structured file-effect facts. It deliberately does not alter Runtime or
// CanonicalEvent: vendors without lifecycle support keep working, and display parsing
// cannot become the persistence contract.
type LifecycleNormalizer interface {
	NormalizeLifecycle(path string) (LifecycleBatch, error)
}

// IncrementalLifecycleNormalizer is the bounded live-collection capability. The
// complete-file LifecycleNormalizer remains for compatibility, one-shot imports and
// explicit recovery; the daemon prefers this interface so an unchanged growing
// transcript is never reparsed from byte zero.
type IncrementalLifecycleNormalizer interface {
	NormalizeLifecycleIncremental(LifecycleReadRequest) (LifecycleBatch, error)
}

// LogicalLifecycleSource owns paging/cursor mechanics for a non-file native source.
// Canonical lifecycle facts remain LifecycleBatch; only source traversal is optional.
type LogicalLifecycleSource interface {
	NormalizeLogicalLifecycle(LogicalLifecycleReadRequest) (LogicalLifecyclePage, error)
}

type LogicalLifecycleReadRequest struct {
	Source             string
	Segment            string
	SessionID          string
	Cursor             []byte
	MaxRecords         int
	RetainEffectBodies bool
}

type LogicalLifecyclePage struct {
	Batch        LifecycleBatch
	Generation   string
	UpdateMarker string
	Cursor       []byte
	Continuation bool
}

func LogicalLifecycle(runtime string, request LogicalLifecycleReadRequest) (LogicalLifecyclePage, bool, error) {
	rt := runtimeFor(runtime)
	source, ok := rt.(LogicalLifecycleSource)
	if !ok {
		return LogicalLifecyclePage{}, false, nil
	}
	page, err := source.NormalizeLogicalLifecycle(request)
	return page, true, err
}

const (
	DefaultLifecycleReadBytes   int64 = 1 << 20
	DefaultLifecycleReadRecords       = 2048
	MaxLifecycleRecordBytes     int64 = 4 << 20
	MaxLifecycleStateBytes            = 1 << 20
	lifecycleReaderBufferBytes        = 64 << 10
)

type LifecycleReadRequest struct {
	Path               string `json:"path"`
	Runtime            string `json:"runtime,omitempty"`
	SourceSegmentID    string `json:"source_segment_id,omitempty"`
	SourceGeneration   string `json:"source_generation,omitempty"`
	Offset             int64  `json:"offset"`
	SourceLine         int64  `json:"source_line"`
	State              []byte `json:"state,omitempty"`
	MaxBytes           int64  `json:"max_bytes,omitempty"`
	MaxRecords         int    `json:"max_records,omitempty"`
	RetainEffectBodies bool   `json:"retain_effect_bodies,omitempty"`
}

type LifecycleBatch struct {
	Runtime            string                       `json:"runtime"`
	CanonicalSessionID string                       `json:"canonical_session_id"`
	SourceSegmentID    string                       `json:"source_segment_id"`
	SourcePath         string                       `json:"source_path"`
	Actions            []LifecycleAction            `json:"actions,omitempty"`
	Results            []LifecycleResult            `json:"results"`
	NextOffset         int64                        `json:"next_offset"`
	NextSourceLine     int64                        `json:"next_source_line"`
	ParserState        []byte                       `json:"parser_state,omitempty"`
	BytesInspected     int64                        `json:"bytes_inspected"`
	PeakRetainedBytes  int64                        `json:"peak_retained_bytes"`
	RecordsDecoded     int                          `json:"records_decoded"`
	MalformedRecords   int                          `json:"malformed_records"`
	OversizedRecords   []OversizedRecordObservation `json:"oversized_records,omitempty"`
	EvictedCalls       []EvictedCallObservation     `json:"evicted_calls,omitempty"`
	Continuation       bool                         `json:"continuation"`
}

// OversizedRecordObservation identifies one source record discarded without ever
// allocating the whole record. Its issue identity remains stable across resumptions.
type OversizedRecordObservation struct {
	IssueID          string `json:"issue_id"`
	RecordStart      int64  `json:"record_start"`
	DiscardedThrough int64  `json:"discarded_through"`
	SourceGeneration string `json:"source_generation"`
}

// EvictedCallObservation identifies one pending native call removed to keep parser
// state within its hard cap. The daemon persists it before advancing the cursor.
type EvictedCallObservation struct {
	OccurrenceID       string `json:"occurrence_id"`
	NativeCallID       string `json:"native_call_id"`
	CanonicalSessionID string `json:"canonical_session_id"`
	SuppliedSessionID  string `json:"supplied_session_id,omitempty"`
	SourceSegmentID    string `json:"source_segment_id"`
	SourceRef          string `json:"source_ref"`
	Reason             string `json:"reason"`
}

// LifecycleAction is an exact action fact exposed by a vendor transcript. It is not a
// live policy decision and it does not imply that any nested/governed tool ran. The
// daemon routes it through the canonical action owner with transcript lineage.
type LifecycleAction struct {
	ObservationID      string `json:"observation_id"`
	Runtime            string `json:"runtime"`
	CanonicalSessionID string `json:"canonical_session_id"`
	SuppliedSessionID  string `json:"supplied_session_id,omitempty"`
	SourceSegmentID    string `json:"source_segment_id"`
	SourceKind         string `json:"source_kind"`
	SourceRef          string `json:"source_ref"`
	SourceSequence     string `json:"source_sequence"`
	SourceDigest       string `json:"source_digest"`
	NativeCallID       string `json:"native_call_id,omitempty"`
	NativeCallKind     string `json:"native_call_kind,omitempty"`
	Tool               string `json:"tool"`
	ObservedAt         string `json:"observed_at,omitempty"`
	MediaType          string `json:"media_type,omitempty"`
	RawBytes           int    `json:"raw_bytes"`
	DecodedBytes       int    `json:"decoded_bytes"`
	InputDigest        string `json:"input_digest,omitempty"`
	Completeness       string `json:"completeness"`
	InputPayload       []byte `json:"input_payload,omitempty"`
}

type LifecycleResult struct {
	ObservationID      string            `json:"observation_id"`
	Runtime            string            `json:"runtime"`
	CanonicalSessionID string            `json:"canonical_session_id"`
	SuppliedSessionID  string            `json:"supplied_session_id,omitempty"`
	SourceSegmentID    string            `json:"source_segment_id"`
	SourceKind         string            `json:"source_kind"`
	SourceRef          string            `json:"source_ref"`
	SourceSequence     string            `json:"source_sequence"`
	SourceDigest       string            `json:"source_digest"`
	NativeCallID       string            `json:"native_call_id,omitempty"`
	NativeCallKind     string            `json:"native_call_kind,omitempty"`
	NativeCallAliases  []NativeCallAlias `json:"native_call_aliases,omitempty"`
	Tool               string            `json:"tool,omitempty"`
	State              string            `json:"state"`
	ErrorClass         string            `json:"error_class,omitempty"`
	CompletedAt        string            `json:"completed_at,omitempty"`
	RawBytes           int               `json:"raw_bytes"`
	DecodedBytes       int               `json:"decoded_bytes"`
	RetainedBytes      int               `json:"retained_bytes"`
	PayloadDigest      string            `json:"payload_digest,omitempty"`
	Completeness       string            `json:"completeness"`
	StdoutBytes        int               `json:"stdout_bytes,omitempty"`
	StdoutDigest       string            `json:"stdout_digest,omitempty"`
	StderrBytes        int               `json:"stderr_bytes,omitempty"`
	StderrDigest       string            `json:"stderr_digest,omitempty"`
	Effects            []LifecycleEffect `json:"effects,omitempty"`
}

// NativeCallAlias is adapter-supplied evidence that one result identity is also
// exposed by the runtime under another exact namespace. It does not replace the raw
// NativeCallKind/NativeCallID pair and must never be synthesized from proximity.
type NativeCallAlias struct {
	NativeCallKind string `json:"native_call_kind"`
	NativeCallID   string `json:"native_call_id"`
	Algorithm      string `json:"algorithm"`
}

// LifecycleAliasRepairContract is an optional adapter declaration for replaying a
// mechanical alias onto historical results collected before the adapter emitted it.
// Generic callers iterate registered capabilities and never name a runtime.
type LifecycleAliasRepairContract struct {
	Runtime              string
	ResultSourceKind     string
	ResultNativeCallKind string
	AliasNativeCallKind  string
	Tool                 string
	Algorithm            string
}

type LifecycleAliasRepairProvider interface {
	LifecycleAliasRepairContracts() []LifecycleAliasRepairContract
}

// LifecycleActionCollectionContract declares that a runtime adapter can emit canonical
// transcript actions and names the historical result surface that makes a bounded
// existing-source repair useful. Generic daemon/store code consumes this contract and
// never names a vendor.
type LifecycleActionCollectionContract struct {
	Runtime              string
	ParserVersion        int
	ResultSourceKind     string
	ResultNativeCallKind string
}

type LifecycleActionCollectionProvider interface {
	LifecycleActionCollectionContracts() []LifecycleActionCollectionContract
}

func LifecycleActionCollectionContracts() []LifecycleActionCollectionContract {
	out := []LifecycleActionCollectionContract{}
	for _, runtime := range Runtimes() {
		if provider, ok := runtime.(LifecycleActionCollectionProvider); ok {
			out = append(out, provider.LifecycleActionCollectionContracts()...)
		}
	}
	return out
}

func LifecycleActionCollectionVersion(runtime string) int {
	for _, contract := range LifecycleActionCollectionContracts() {
		if contract.Runtime == runtime && contract.ParserVersion > 0 {
			return contract.ParserVersion
		}
	}
	return 0
}

func LifecycleAliasRepairContracts() []LifecycleAliasRepairContract {
	out := []LifecycleAliasRepairContract{}
	for _, runtime := range Runtimes() {
		if provider, ok := runtime.(LifecycleAliasRepairProvider); ok {
			out = append(out, provider.LifecycleAliasRepairContracts()...)
		}
	}
	return out
}

type LifecycleEffect struct {
	Ordinal           int    `json:"ordinal"`
	RawIdentity       string `json:"raw_identity"`
	Operation         string `json:"operation"`
	MoveTarget        string `json:"move_target,omitempty"`
	EvidenceSource    string `json:"evidence_source"`
	SourceField       string `json:"source_field"`
	Completeness      string `json:"completeness"`
	ReplaceAll        bool   `json:"replace_all,omitempty"`
	ReplacementBefore []byte `json:"replacement_before,omitempty"`
	ReplacementAfter  []byte `json:"replacement_after,omitempty"`
	BeforeBytes       int    `json:"before_bytes,omitempty"`
	AfterBytes        int    `json:"after_bytes,omitempty"`
	BeforeDigest      string `json:"before_digest,omitempty"`
	AfterDigest       string `json:"after_digest,omitempty"`
	ContentPayload    []byte `json:"content_payload,omitempty"`
	ContentBytes      int    `json:"content_bytes,omitempty"`
	ContentDigest     string `json:"content_digest,omitempty"`
	DiffPayload       []byte `json:"diff_payload,omitempty"`
	DiffBytes         int    `json:"diff_bytes,omitempty"`
	DiffDigest        string `json:"diff_digest,omitempty"`
	DiffCompleteness  string `json:"diff_completeness"`
}

func Lifecycle(runtime, path string) (LifecycleBatch, bool, error) {
	rt := runtimeFor(runtime)
	n, ok := rt.(LifecycleNormalizer)
	if !ok {
		return LifecycleBatch{}, false, nil
	}
	batch, err := n.NormalizeLifecycle(path)
	return batch, true, err
}

func LifecycleIncremental(runtime string, request LifecycleReadRequest) (LifecycleBatch, bool, error) {
	rt := runtimeFor(runtime)
	n, ok := rt.(IncrementalLifecycleNormalizer)
	if !ok {
		return LifecycleBatch{}, false, nil
	}
	batch, err := n.NormalizeLifecycleIncremental(request)
	return batch, true, err
}

func lifecycleDigest(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256-v1:" + hex.EncodeToString(h[:])
}

func sourceSequence(line, ordinal int) string {
	return strconv.Itoa(line) + ":" + strconv.Itoa(ordinal)
}

func resultObservationID(runtime, segment, sourceSequence string, raw []byte) string {
	return "res_" + strings.TrimPrefix(lifecycleDigest([]byte(runtime+"\x00"+segment+"\x00"+sourceSequence+"\x00"+lifecycleDigest(raw))), "sha256-v1:")
}

func actionObservationID(runtime, segment, sourceSequence string, raw []byte) string {
	return "act_" + strings.TrimPrefix(lifecycleDigest([]byte(runtime+"\x00"+segment+"\x00"+sourceSequence+"\x00"+lifecycleDigest(raw))), "sha256-v1:")
}

func resultPayloadMeta(v any) (raw []byte, decoded []byte) {
	raw, _ = json.Marshal(v)
	switch value := v.(type) {
	case string:
		decoded = []byte(value)
	case nil:
		decoded = nil
	default:
		decoded = raw
	}
	return raw, decoded
}

func newTranscriptAction(runtime, canonical, supplied, segment, sourceRef, sequence,
	nativeID, nativeKind, tool, observed string, input any, record []byte) LifecycleAction {
	_, decoded := resultPayloadMeta(input)
	mediaType, completeness, digest := "application/json; charset=utf-8", "complete", lifecycleDigest(decoded)
	if _, ok := input.(string); ok {
		mediaType = "text/plain; charset=utf-8"
	}
	if input == nil {
		decoded, mediaType, completeness, digest = nil, "", "unavailable", ""
	}
	return LifecycleAction{ObservationID: actionObservationID(runtime, segment, sequence, record),
		Runtime: runtime, CanonicalSessionID: canonical, SuppliedSessionID: supplied,
		SourceSegmentID: segment, SourceKind: "vendor-transcript", SourceRef: sourceRef,
		SourceSequence: sequence, SourceDigest: lifecycleDigest(record), NativeCallID: nativeID,
		NativeCallKind: nativeKind, Tool: tool, ObservedAt: observed, MediaType: mediaType,
		RawBytes: len(decoded), DecodedBytes: len(decoded), InputDigest: digest,
		Completeness: completeness, InputPayload: decoded}
}

func newTranscriptResult(runtime, canonical, supplied, segment, sourceKind, sourceRef, sequence,
	nativeID, nativeKind, tool, state, errorClass, completed string, record, raw, decoded []byte) LifecycleResult {
	return LifecycleResult{
		ObservationID: resultObservationID(runtime, segment, sequence, record), Runtime: runtime,
		CanonicalSessionID: canonical, SuppliedSessionID: supplied, SourceSegmentID: segment,
		SourceKind: sourceKind, SourceRef: sourceRef, SourceSequence: sequence,
		SourceDigest: lifecycleDigest(record), NativeCallID: nativeID, NativeCallKind: nativeKind,
		Tool: tool, State: state, ErrorClass: errorClass, CompletedAt: completed,
		RawBytes: len(raw), DecodedBytes: len(decoded), RetainedBytes: 0,
		PayloadDigest: lifecycleDigest(decoded), Completeness: "metadata-only",
	}
}

func newEvictedCallObservation(runtime, canonical, segment, sourceRef, generation,
	nativeID string, call lifecycleCallState) EvictedCallObservation {
	reason := "parser-state-cap"
	occurrence := lifecycleDigest([]byte(runtime + "\x00" + canonical + "\x00" + sourceRef +
		"\x00" + segment + "\x00" + generation + "\x00" + nativeID + "\x00" + reason))
	return EvictedCallObservation{OccurrenceID: "evicted_" + strings.TrimPrefix(occurrence, "sha256-v1:"),
		NativeCallID: nativeID, CanonicalSessionID: canonical, SuppliedSessionID: call.Session,
		SourceSegmentID: segment, SourceRef: sourceRef, Reason: reason}
}
