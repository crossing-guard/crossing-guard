// Package observation owns the versioned hook-to-daemon wire contract. It contains
// no policy, path interpretation, storage, Git, or transport behavior.
package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

const (
	SchemaV1              = "pretool-observation-v1"
	ResultSchemaV1        = "posttool-result-v1"
	ClosureSchemaV1       = "session-closure-v1"
	SessionEntrySchemaV1  = "session-entry-v1"
	SessionTurnSchemaV1   = "session-turn-v1"
	CollectorPreTool      = "guardcli-pretool"
	CollectorPostTool     = "guardcli-posttool"
	CollectorClosure      = "guardcli-session-closure"
	CollectorSessionEntry = "guardcli-session-entry"
	CollectorSessionTurn  = "guardcli-session-turn"
	InputMediaTypeJSON    = "application/json; charset=utf-8"
	MaxRetainedInput      = 1 << 20
	MaxEnvelopeBytes      = 2 << 20
	// MaxNativeSourceBytes bounds an envelope's native_source provenance; the
	// daemon rejects a longer one.
	MaxNativeSourceBytes = 128
	// Direct capture must expire before the hook's entire delivery budget; otherwise
	// the hook could release the tool while a later snapshot was still labeled as a
	// pre-release boundary.
	DirectCaptureBudget = 1250 * time.Millisecond
	HookDeliveryBudget  = 1500 * time.Millisecond
	// HookDeadlineHeader carries the unix-millisecond moment a hook client
	// stops waiting for its reply. The daemon claims pending helper messages
	// for a boundary only while that reply can still be read
	// (delivery-claim-on-reply plan D2).
	HookDeadlineHeader = "X-CG-Hook-Deadline"
)

// HookDeadlineValue renders the deadline header for a request whose client
// timeout is HookDeliveryBudget and starts at sentAt.
func HookDeadlineValue(sentAt time.Time) string {
	return strconv.FormatInt(sentAt.Add(HookDeliveryBudget).UnixMilli(), 10)
}

// ParseHookDeadline reads the header; ok=false when absent or malformed (an
// older hook), in which case the caller judges by the request context alone.
func ParseHookDeadline(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// SessionEntryEnvelope is collection-only lifecycle evidence. EntryKind is the
// vendor-neutral projection produced by the selected runtime adapter; NativeSource
// retains the bounded original value so an unknown mapping remains inspectable.
type SessionEntryEnvelope struct {
	Schema           string `json:"session_entry_schema"`
	ObservationID    string `json:"session_entry_observation_id"`
	CollectorID      string `json:"collector_id"`
	CollectorVersion string `json:"collector_version,omitempty"`
	Runtime          string `json:"runtime"`
	SessionID        string `json:"session"`
	HookEventName    string `json:"hook_event_name"`
	EntryKind        string `json:"entry_kind"`
	NativeSource     string `json:"native_source,omitempty"`
	TranscriptPath   string `json:"transcript_path,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	ObservedAt       int64  `json:"observed_at"`
	QueuedAt         int64  `json:"queued_at"`
	DeliveryAttempts int    `json:"delivery_attempts"`
	DeliveryMode     string `json:"delivery_mode"`
	// HandoffTicket is the open ticket the launching process carried in its
	// environment (HandoffTicketEnv), copied here by the lifecycle hook when
	// present: the session this entry opens was started by a console Open of a
	// handoff. Omitted when empty, so an entry without one keeps the digest it
	// always had.
	HandoffTicket string `json:"handoff_ticket,omitempty"`
}

// HandoffTicketEnv is the environment variable a console Open sets on the first
// turn's process and on nothing else. The lifecycle hook copies it into the
// session entry; the daemon removes it from its own environment at start-up
// and from every environment it builds for a runtime process.
const HandoffTicketEnv = "CG_HANDOFF_TICKET"

// MaxHandoffTicketBytes bounds the copied value: a ticket id is a short typed
// id, and anything longer is not one.
const MaxHandoffTicketBytes = 64

// SessionEntryReceipt confirms durable activity storage and the bounded attachment
// checkpoint attempted before the start hook was released.
type SessionEntryReceipt struct {
	Schema        string            `json:"session_entry_schema"`
	ObservationID string            `json:"session_entry_observation_id"`
	Duplicate     bool              `json:"duplicate"`
	EvidenceClass string            `json:"evidence_class"`
	Checkpoint    CheckpointReceipt `json:"checkpoint"`
}

// Digest returns the retry-stable semantic identity of a session-entry envelope.
func (e SessionEntryEnvelope) Digest() (string, error) {
	e.DeliveryAttempts = 0
	e.DeliveryMode = ""
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

// SessionTurnKinds is the closed, framework-owned vocabulary of turn-boundary
// facts. Provider events are translated INTO these by installers and plugins;
// nothing downstream ever sees a provider's event name as a decision input.
var SessionTurnKinds = map[string]bool{
	"turn.started": true, "turn.ended": true, "input.requested": true,
	"subagent.started": true, "subagent.ended": true, "context.compacted": true,
}

// SessionTurnEnvelope is collection-only turn-boundary evidence: the agent
// began a turn, handed the conversation back, or is blocked asking the human.
// Kind is OUR vocabulary (SessionTurnKinds); NativeSource keeps the bounded
// original event name and sub-type so an unexpected mapping stays inspectable.
type SessionTurnEnvelope struct {
	Schema           string `json:"session_turn_schema"`
	ObservationID    string `json:"session_turn_observation_id"`
	CollectorID      string `json:"collector_id"`
	CollectorVersion string `json:"collector_version,omitempty"`
	Runtime          string `json:"runtime"`
	SessionID        string `json:"session"`
	Kind             string `json:"kind"`
	NativeSource     string `json:"native_source,omitempty"`
	TranscriptPath   string `json:"transcript_path,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	ObservedAt       int64  `json:"observed_at"`
	QueuedAt         int64  `json:"queued_at"`
	DeliveryAttempts int    `json:"delivery_attempts"`
	DeliveryMode     string `json:"delivery_mode"`
	// Carrier says this client can print injected context into the session's
	// own conversation (a hook installer with a context encoder, or a plugin
	// that captured stdout; never a hook its runtime reports as inside a
	// sub-agent, whose output reaches only the sub-agent). The daemon hands pending helper messages only
	// to a carrier; a boundary that cannot carry leaves them pending until expiry.
	// Excluded from the digest: transport capability is not evidence.
	Carrier bool `json:"carrier,omitempty"`
}

// Digest returns the retry-stable semantic identity of a session-turn envelope.
func (e SessionTurnEnvelope) Digest() (string, error) {
	e.DeliveryAttempts = 0
	e.DeliveryMode = ""
	e.Carrier = false
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

// SessionTurnReceipt confirms durable storage of one turn-boundary row.
type SessionTurnReceipt struct {
	Schema        string     `json:"session_turn_schema"`
	ObservationID string     `json:"session_turn_observation_id"`
	Duplicate     bool       `json:"duplicate"`
	Deliveries    []Delivery `json:"deliveries,omitempty"`
}

// Delivery is one helper message the daemon handed to this boundary for the
// session the receipt names (helper-session-attachment plan D5). It is
// attributed, untrusted context for the vendor's own injection surface — never
// a decision. The daemon marked it delivered before responding; a client that
// dies after reading loses it rather than seeing it again.
type Delivery struct {
	DeliveryID string `json:"delivery_id"`
	Message    string `json:"message"`
}

type ResultEffect struct {
	Ordinal          int    `json:"ordinal"`
	RawIdentity      string `json:"raw_identity"`
	Operation        string `json:"operation"`
	MoveTarget       string `json:"move_target,omitempty"`
	EvidenceSource   string `json:"evidence_source"`
	SourceField      string `json:"source_field"`
	Completeness     string `json:"completeness"`
	ReplaceAll       bool   `json:"replace_all,omitempty"`
	BeforeBytes      int    `json:"replacement_before_bytes,omitempty"`
	BeforeDigest     string `json:"replacement_before_digest,omitempty"`
	BeforePayload    []byte `json:"replacement_before_payload,omitempty"`
	AfterBytes       int    `json:"replacement_after_bytes,omitempty"`
	AfterDigest      string `json:"replacement_after_digest,omitempty"`
	AfterPayload     []byte `json:"replacement_after_payload,omitempty"`
	ContentBytes     int    `json:"content_bytes,omitempty"`
	ContentDigest    string `json:"content_digest,omitempty"`
	ContentPayload   []byte `json:"content_payload,omitempty"`
	DiffBytes        int    `json:"diff_bytes,omitempty"`
	DiffDigest       string `json:"diff_digest,omitempty"`
	DiffCompleteness string `json:"diff_completeness"`
	DiffPayload      []byte `json:"diff_payload,omitempty"`
}

// ResultEnvelope is collection-only. It carries a runtime completion and bounded
// mechanically structured code effects; it has no decision fields by design.
type ResultEnvelope struct {
	Schema               string         `json:"result_schema"`
	ObservationID        string         `json:"result_observation_id"`
	CollectorID          string         `json:"collector_id"`
	CollectorVersion     string         `json:"collector_version,omitempty"`
	Runtime              string         `json:"runtime"`
	SessionID            string         `json:"session"`
	SuppliedSessionID    string         `json:"supplied_session_id,omitempty"`
	SourceSegmentID      string         `json:"source_segment_id,omitempty"`
	HookEventName        string         `json:"hook_event_name"`
	TranscriptPath       string         `json:"transcript_path,omitempty"`
	Cwd                  string         `json:"cwd,omitempty"`
	NativeCallID         string         `json:"native_call_id,omitempty"`
	NativeCallKind       string         `json:"native_call_kind,omitempty"`
	Tool                 string         `json:"tool,omitempty"`
	State                string         `json:"state"`
	ErrorClass           string         `json:"error_class,omitempty"`
	CompletedAt          int64          `json:"completed_at,omitempty"`
	DurationMS           int64          `json:"duration_ms,omitempty"`
	MediaType            string         `json:"media_type,omitempty"`
	RawEnvelopeBytes     int            `json:"raw_envelope_bytes"`
	RawFieldBytes        int            `json:"raw_field_bytes"`
	DecodedBytes         int            `json:"decoded_bytes"`
	RetainedBytes        int            `json:"retained_bytes"`
	ExternalLocatorBytes int            `json:"external_locator_bytes"`
	PayloadDigest        string         `json:"payload_digest,omitempty"`
	Completeness         string         `json:"completeness"`
	Payload              []byte         `json:"payload,omitempty"`
	StdoutBytes          int            `json:"stdout_bytes,omitempty"`
	StdoutDigest         string         `json:"stdout_digest,omitempty"`
	StderrBytes          int            `json:"stderr_bytes,omitempty"`
	StderrDigest         string         `json:"stderr_digest,omitempty"`
	Effects              []ResultEffect `json:"effects,omitempty"`
	QueuedAt             int64          `json:"queued_at"`
	DeliveryAttempts     int            `json:"delivery_attempts"`
	DeliveryMode         string         `json:"delivery_mode"`
	// Carrier says this client can print injected context into the session's
	// own conversation (a hook installer with a context encoder, or a plugin
	// that captured stdout; never a hook its runtime reports as inside a
	// sub-agent, whose output reaches only the sub-agent). The daemon hands pending helper messages only
	// to a carrier; a boundary that cannot carry leaves them pending until expiry.
	// Excluded from the digest: transport capability is not evidence.
	Carrier bool `json:"carrier,omitempty"`
}

type ResultReceipt struct {
	Schema        string     `json:"result_schema"`
	ObservationID string     `json:"result_observation_id"`
	ResultID      int64      `json:"result_id"`
	Duplicate     bool       `json:"duplicate"`
	JoinClass     string     `json:"join_class"`
	Deliveries    []Delivery `json:"deliveries,omitempty"`
}

func (e ResultEnvelope) Digest() (string, error) {
	e.DeliveryAttempts = 0
	e.DeliveryMode = ""
	e.Carrier = false
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

type ClosureEnvelope struct {
	Schema           string `json:"closure_schema"`
	ObservationID    string `json:"closure_observation_id"`
	CollectorID      string `json:"collector_id"`
	CollectorVersion string `json:"collector_version,omitempty"`
	Runtime          string `json:"runtime"`
	SessionID        string `json:"session"`
	HookEventName    string `json:"hook_event_name"`
	TranscriptPath   string `json:"transcript_path,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	ObservedAt       int64  `json:"observed_at"`
	QueuedAt         int64  `json:"queued_at"`
	DeliveryAttempts int    `json:"delivery_attempts"`
	DeliveryMode     string `json:"delivery_mode"`
}

type ClosureReceipt struct {
	Schema        string            `json:"closure_schema"`
	ObservationID string            `json:"closure_observation_id"`
	Checkpoint    CheckpointReceipt `json:"checkpoint"`
}

// Digest returns retry-stable closure identity.
func (e ClosureEnvelope) Digest() (string, error) {
	e.DeliveryAttempts = 0
	e.DeliveryMode = ""
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

type ResourceClaim struct {
	Ordinal       int    `json:"ordinal"`
	Kind          string `json:"kind"`
	RawIdentity   string `json:"raw_identity"`
	Identity      string `json:"identity,omitempty"`
	Operation     string `json:"operation"`
	EvidenceClass string `json:"evidence_class"`
	SourceField   string `json:"source_field"`
	Completeness  string `json:"completeness"`
	Resolution    string `json:"resolution,omitempty"`
}

// Envelope preserves the current projection alongside the complete bounded input.
// Retry-only fields are deliberately excluded from Digest.
type Envelope struct {
	Schema        string `json:"observation_schema"`
	ObservationID string `json:"observation_id"`
	// ActionID is optional collector-owned correlation for one tool attempt. It is
	// not a policy decision, approval identity, or orchestration instruction.
	ActionID         string   `json:"action_id,omitempty"`
	CollectorID      string   `json:"collector_id"`
	CollectorVersion string   `json:"collector_version,omitempty"`
	NativeCallID     string   `json:"native_call_id,omitempty"`
	NativeCallKind   string   `json:"native_call_kind,omitempty"`
	TranscriptPath   string   `json:"transcript_path,omitempty"`
	SessionID        string   `json:"session"`
	Runtime          string   `json:"runtime,omitempty"`
	Tool             string   `json:"tool"`
	Command          string   `json:"command,omitempty"`
	Content          string   `json:"content,omitempty"`
	FilePath         string   `json:"file_path,omitempty"`
	FilePaths        []string `json:"file_paths,omitempty"`
	Cwd              string   `json:"cwd,omitempty"`
	URL              string   `json:"url,omitempty"`
	Skill            string   `json:"skill,omitempty"`
	Decision         string   `json:"decision,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	// Rule is the rule that produced or asked for Decision. It is evidence, so it is inside
	// the digest; omitempty keeps every envelope written before the field existed — and
	// every spool file still waiting to replay — at the digest it always had.
	Rule string `json:"rule,omitempty"`
	// Layer is the distribution tier that rule arrived by (schema 38): user |
	// repository | organization. The same evidence-and-digest rule as Rule: inside the
	// digest, omitempty so every older envelope and spool file keeps its digest.
	Layer string `json:"layer,omitempty"`
	// LayerReasons carries the layered loader's notes (an expired layer, one that
	// failed to parse, a checkout whose repository layer is not yet staged) — a
	// separate fact from the tier enum, inside the digest by the same rule.
	LayerReasons string `json:"layer_reasons,omitempty"`
	TS           int64  `json:"ts,omitempty"`
	// ToolInput is encoded as base64 by encoding/json so the exact vendor bytes,
	// including insignificant JSON whitespace, survive the outer wire envelope.
	ToolInput             []byte          `json:"tool_input_payload,omitempty"`
	ToolInputBytes        int             `json:"tool_input_bytes"`
	ToolInputDigest       string          `json:"tool_input_digest,omitempty"`
	ToolInputCompleteness string          `json:"tool_input_completeness"`
	ResourceClaims        []ResourceClaim `json:"resource_claims,omitempty"`
	QueuedAt              int64           `json:"queued_at"`
	DeliveryAttempts      int             `json:"delivery_attempts"`
	DeliveryMode          string          `json:"delivery_mode"`
	// Carrier says this client can print injected context into the session's
	// own conversation (a hook installer with a context encoder, or a plugin
	// that captured stdout; never a hook its runtime reports as inside a
	// sub-agent, whose output reaches only the sub-agent). The daemon hands pending helper messages only
	// to a carrier; a boundary that cannot carry leaves them pending until expiry.
	// Excluded from the digest: transport capability is not evidence.
	Carrier bool `json:"carrier,omitempty"`
}

type CheckpointReceipt struct {
	ID             int64  `json:"id,omitempty"`
	Status         string `json:"status,omitempty"`
	BoundaryClass  string `json:"boundary_class,omitempty"`
	ChangeRecordID int64  `json:"change_record_id,omitempty"`
	FailureKind    string `json:"failure_kind,omitempty"`
}

type Receipt struct {
	Schema        string            `json:"observation_schema"`
	ObservationID string            `json:"observation_id"`
	ActionID      string            `json:"action_id,omitempty"`
	EventID       int64             `json:"event_id"`
	Duplicate     bool              `json:"duplicate"`
	Checkpoint    CheckpointReceipt `json:"checkpoint,omitempty"`
	Deliveries    []Delivery        `json:"deliveries,omitempty"`
}

func DigestBytes(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256-v1:" + hex.EncodeToString(h[:])
}

func (e Envelope) Digest() (string, error) {
	e.DeliveryAttempts = 0
	e.DeliveryMode = ""
	e.Carrier = false
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}
