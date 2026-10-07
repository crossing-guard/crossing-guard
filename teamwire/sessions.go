package teamwire

// The session-review record bodies (team item 4, plan §4.3). Each mirrors one wire schema
// at 1.0; the event body is engine.WireEvent (the encoder is the one owner of its shape).

// SessionRecord is schemas/session.schema.json 1.0: the three identities, the event span,
// and the device's own chain report — a CLAIM the server renders as "as reported by the
// device", never as its own verification (item 4 decision 7).
type SessionRecord struct {
	SchemaVersion string          `json:"schema_version"`
	ID            string          `json:"id"`
	DeviceID      string          `json:"device_id"`
	Session       SessionIdentity `json:"session"`
	Workspace     Workspace       `json:"workspace"`
	Events        EventSpan       `json:"events"`
	ChainClaim    ChainClaim      `json:"chain_claim"`
	ReportedAt    string          `json:"reported_at"`
}

// SessionIdentity is the common schema's session: the wire id, the runtime, the native
// id, and the other two identities when known.
type SessionIdentity struct {
	ID        string `json:"id"`
	Runtime   string `json:"runtime"`
	NativeID  string `json:"native_id"`
	CatalogID string `json:"catalog_id,omitempty"`
	ResumeID  string `json:"resume_id,omitempty"`
}

// Workspace is the repository a session worked in; nil when unknown.
type Workspace struct {
	RepositoryID *string `json:"repository_id"`
}

// EventSpan counts a session's events and bounds them in time (RFC 3339; nil when empty).
type EventSpan struct {
	Count   int     `json:"count"`
	FirstAt *string `json:"first_at"`
	LastAt  *string `json:"last_at"`
}

// ChainClaim is the device's verification of its own chain, as it reported it.
type ChainClaim struct {
	Status          string    `json:"status"`
	Rows            int       `json:"rows"`
	Chained         int       `json:"chained"`
	Legacy          int       `json:"legacy"`
	DiskSpan        [2]int64  `json:"disk_span"`
	HeldSpan        *[2]int64 `json:"held_span"`
	TailMatchesHeld *bool     `json:"tail_matches_held,omitempty"`
}

// CheckpointFact is schemas/session-checkpoint-fact.schema.json 1.0: what a checkpoint
// established, never a body and never an absolute path.
type CheckpointFact struct {
	SchemaVersion string              `json:"schema_version"`
	ID            string              `json:"id"`
	DeviceID      string              `json:"device_id"`
	SessionID     string              `json:"session_id"`
	Kind          string              `json:"kind"`
	Status        string              `json:"status"`
	BoundaryClass string              `json:"boundary_class"`
	RepositoryID  *string             `json:"repository_id"`
	RequestedAt   string              `json:"requested_at"`
	EndedAt       *string             `json:"ended_at"`
	Revision      *CheckpointRevision `json:"revision"`
	FailureKind   string              `json:"failure_kind"`
	DetailDigest  string              `json:"detail_digest"`
}

// CheckpointRevision is the change record a completed checkpoint linked.
type CheckpointRevision struct {
	Kind               string  `json:"kind"`
	EvidenceClass      string  `json:"evidence_class"`
	Base               *string `json:"base"`
	Head               *string `json:"head"`
	SnapshotDigest     *string `json:"snapshot_digest"`
	SourceDigest       string  `json:"source_digest"`
	VerificationResult *string `json:"verification_result"`
	Items              int     `json:"items"`
}

// ContentChunk is schemas/session-content.schema.json 1.0: one opted-in body, redacted
// before it left the device. Its id derives from its event's id; body_hash is the hash
// of the text sent.
type ContentChunk struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	DeviceID      string   `json:"device_id"`
	SessionID     string   `json:"session_id"`
	EventID       string   `json:"event_id"`
	Part          string   `json:"part"`
	Consent       string   `json:"consent"`
	Body          string   `json:"body"`
	BodyHash      string   `json:"body_hash"`
	Redactions    []string `json:"redactions"`
}

// ContentPartEventInput is the one content part this release carries: the tool input an
// event decided on.
const ContentPartEventInput = "event_input"

// ContentChunkMaxBytes is the schema's per-chunk cap, restated for the encoder so an
// oversize body is refused on the device, before the server refuses it.
const ContentChunkMaxBytes = 262144
