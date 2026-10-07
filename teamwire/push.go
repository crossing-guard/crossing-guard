package teamwire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// WireVersion is the push envelope's version.
const WireVersion = "1.0"

// Route patterns a device signs. They are the literals in the server's route table.
const (
	RoutePush          = "/sync/v1/push"
	RouteEnrollStart   = "/sync/v1/devices/enroll/start"
	RouteEnrollPoll    = "/sync/v1/devices/enroll/poll"
	RouteDevicesRevoke = "/sync/v1/devices/revoke"
)

// Enrollment statuses a poll answers with. The device acts on every one, so they are
// contract, not prose: a new status is a new literal here, on both sides.
const (
	EnrollPending  = "pending"
	EnrollApproved = "approved"
	EnrollDenied   = "denied"
	EnrollExpired  = "expired"
)

// Record kinds. A server answers an unknown kind per record, never by failing the batch,
// so an older server and a newer client degrade one record at a time.
const (
	KindDeviceReport = "device-report"
	// Item 4: the session-review kinds the outbox drain pushes (plan §4.3).
	KindEvent          = "event"
	KindSession        = "session"
	KindCheckpointFact = "session_checkpoint_fact"
	KindSessionContent = "session_content"
	// Item 5: memory by scope across the team, and deletion tombstones (memory records and
	// session content). An older server answers them unsupported_kind and the drain parks.
	KindMemory    = "memory"
	KindTombstone = "tombstone"
)

// SchemaForKind names the wire schema a kind's body must satisfy; "" = no schema (an
// unknown kind, answered per record).
func SchemaForKind(kind string) string {
	return map[string]string{KindDeviceReport: "device-report.schema.json", KindEvent: "event.schema.json",
		KindSession: "session.schema.json", KindCheckpointFact: "session-checkpoint-fact.schema.json",
		KindSessionContent: "session-content.schema.json", KindMemory: "memory.schema.json",
		KindTombstone: "tombstone.schema.json", KindHandoff: "handoff.schema.json",
		KindHandoffReceipt: "handoff-receipt.schema.json"}[kind]
}

// Per-record results (synchronization-protocol.md, "Partial batches return per-record
// results"). Replaying a record whose hash matches is a success, reported as duplicate.
const (
	StatusAccepted  = "accepted"
	StatusDuplicate = "duplicate"
	StatusRejected  = "rejected"
	StatusConflict  = "conflict"
)

// PushRequest is one batch.
type PushRequest struct {
	SchemaVersion string       `json:"schema_version"`
	Records       []PushRecord `json:"records"`
}

// PushRecord is one immutable record. ContentHash is over Body's exact bytes, so
// idempotency is by id + hash: equal id with an unequal hash is a conflict and a security
// event, never an overwrite.
type PushRecord struct {
	Kind        string          `json:"kind"`
	ID          string          `json:"id"`
	ContentHash string          `json:"content_hash"`
	Body        json.RawMessage `json:"body"`
}

// PushResponse answers a batch. ServerTime lets a client notice its own clock skew.
type PushResponse struct {
	Results    []PushResult `json:"results"`
	ServerTime int64        `json:"server_time"`
}

// PushResult is one record's outcome.
type PushResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *Error `json:"error,omitempty"`
	// Memory carries a memory revision's server revision, and on stale_base the revision
	// the server holds (item 5, decision 13). Absent for every other kind.
	Memory *MemoryAnswer `json:"memory,omitempty"`
}

// Error is the one error envelope the team server speaks ({"error": {...}}). The
// contract: Field is a JSON path and Message names a rule; neither may quote what was
// submitted. This package carries the shape; the server's tests hold it to the contract.
type Error struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Field    string `json:"field,omitempty"`
	Recovery string `json:"recovery,omitempty"`
	// ServerTime accompanies timestamp_out_of_window so a device can report its skew.
	ServerTime int64 `json:"server_time,omitempty"`
}

// ErrorBody is the envelope as it appears on the wire.
type ErrorBody struct {
	Error Error `json:"error"`
}

// Error codes a device acts on. Anything else is shown and retried on the normal cadence.
const (
	CodeDeviceRevoked        = "device_revoked"
	CodeNoPendingEnrollment  = "no_pending_enrollment" // a poll for an enrollment the server no longer holds
	CodeUnauthenticated      = "unauthenticated"       // unknown device and bad signature are deliberately one code
	CodeTimestampOutOfWindow = "timestamp_out_of_window"
	CodeReplayedNonce        = "replayed_nonce"
	CodeUnsupportedKind      = "unsupported_kind"
	CodeInvalidRecord        = "invalid_record"
	CodeHashMismatch         = "content_hash_mismatch"
	// CodeInternal is a per-record server failure to store: transient, so a device
	// retries it (bounded by its push_max_attempts), never acknowledges it as refused.
	CodeInternal = "internal"
)

// ContentHash is "sha256:" + hex over a record body's exact bytes.
func ContentHash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
