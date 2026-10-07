package teamwire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// Memory by scope across the team.
// One shared memory record per revision on the wire (memory.schema.json 1.1), deletion
// tombstones (tombstone.schema.json 1.0), and the pull/ack/verify envelopes. Memory does
// not use item 4's immutable-record idempotency: a revision names the base it was written
// against (base_content_hash), and the server answers a stale base by name, carrying the
// revision it holds (decision 13).

// Wire schema versions this package encodes.
const (
	MemorySchemaVersion    = "1.1"
	TombstoneSchemaVersion = "1.0"
)

// The item-5 sync routes a device signs.
const (
	RoutePull            = "/sync/v1/pull"
	RouteAck             = "/sync/v1/ack"
	RouteDeletionsVerify = "/sync/v1/deletions/verify"
)

// Tombstone record types the server accepts (decision 9: allowlisted).
const (
	TombstoneMemory         = "memory"
	TombstoneSessionContent = "session_content"
)

// Item-5 answer codes. stale_base, tombstoned and the scope/deletion refusals come from
// the server; not_shareable, superseded and source_missing are this device's own.
const (
	CodeStaleBase              = "stale_base"
	CodeTombstoned             = "tombstoned"
	CodeOrganizationScopeAdmin = "organization_scope_admin_only"
	CodeForeignOrganization    = "foreign_organization"
	CodeDeleteNotAllowed       = "delete_not_allowed"
	CodeUnknownRecordType      = "unknown_record_type"
	CodeSlugImmutable          = "slug_immutable"
	CodeNotShareable           = "not_shareable"
	CodeSuperseded             = "superseded"
	CodeSourceMissing          = "source_missing"
)

// WireAuthorID is the one author id a memory record carries on the wire. The author of
// record is the member the server authenticates, stored beside the revision (decision
// 6); the body therefore never carries a local account name or a session id (CR-14).
const WireAuthorID = "member"

// Actor is common.schema.json's actor.
type Actor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// MemoryScope is the wire scope: repository or organization, never user.
type MemoryScope struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// MemoryRecord is schemas/memory.schema.json 1.1: one revision of one shared record,
// redacted before it left the device. Revision is the pushing device's local revision
// number (the server numbers its own); ContentHash is MemoryWireHash of this record;
// BaseContentHash is the wire hash of the revision the edit was made against ("" for a
// first appearance).
type MemoryRecord struct {
	SchemaVersion      string      `json:"schema_version"`
	ID                 string      `json:"id"`
	Slug               string      `json:"slug"`
	Revision           int64       `json:"revision"`
	Scope              MemoryScope `json:"scope"`
	RepositoryIdentity string      `json:"repository_identity,omitempty"`
	Title              string      `json:"title"`
	Category           string      `json:"category"`
	Body               string      `json:"body"`
	Tags               []string    `json:"tags"`
	Aliases            []string    `json:"aliases"`
	Source             string      `json:"source"`
	Origin             string      `json:"origin,omitempty"`
	SupersededBy       *string     `json:"superseded_by"`
	VerifiedAt         *string     `json:"verified_at"`
	VerifiedBy         *string     `json:"verified_by"`
	CreatedAt          string      `json:"created_at"`
	UpdatedAt          string      `json:"updated_at"`
	Author             Actor       `json:"author"`
	ContentHash        string      `json:"content_hash"`
	BaseContentHash    string      `json:"base_content_hash,omitempty"`
}

// MemoryWireHash is the one wire hash (decision 13c): sha256 over the canonical JSON of
// the whole redacted record with content_hash and base_content_hash emptied. The device
// computes it at first send and freezes it; the server keeps it as the compare key and
// never recomputes it from a body its backstop may have redacted further.
func MemoryWireHash(r MemoryRecord) string {
	r.ContentHash, r.BaseContentHash = "", ""
	if r.Tags == nil {
		r.Tags = []string{}
	}
	if r.Aliases == nil {
		r.Aliases = []string{}
	}
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MemoryPushID is a memory revision's push record id: the record's global id and the
// pushing device's local revision. The same id with a different hash from the same device
// is tampering (13a check 2); a retry resends the frozen body under the same id.
func MemoryPushID(globalID string, revision int64) string {
	return globalID + "@" + strconv.FormatInt(revision, 10)
}

// Tombstone is schemas/tombstone.schema.json 1.0. For memory, RecordID is the record's
// global id; for session content it is the wire session id (engine.WireSessionID), never
// a local one.
type Tombstone struct {
	SchemaVersion             string   `json:"schema_version"`
	ID                        string   `json:"id"`
	RecordID                  string   `json:"record_id"`
	RecordType                string   `json:"record_type"`
	DeletedAt                 string   `json:"deleted_at"`
	DeletedBy                 Actor    `json:"deleted_by"`
	ReasonClass               string   `json:"reason_class"`
	PriorContentHash          string   `json:"prior_content_hash"`
	RequiredProjectionCleanup []string `json:"required_projection_cleanup"`
}

// MemoryAnswer rides a memory push result: the server revision an accepted or duplicate
// revision was stored as, and on stale_base the revision the server holds, which the
// device lands at once (decision 13b — no pull needed to recover).
type MemoryAnswer struct {
	ServerRevision int64         `json:"server_revision,omitempty"`
	Current        *PulledMemory `json:"current,omitempty"`
}

// PulledMemory is one record revision as the server serves it: the record as stored, its
// wire hash (the device's, never recomputed), the server's revision number, and the
// authenticated author — a separate fact from the body's author (decision 6).
type PulledMemory struct {
	Record               MemoryRecord `json:"record"`
	WireHash             string       `json:"wire_hash"`
	ServerRevision       int64        `json:"server_revision"`
	AuthorUserID         string       `json:"author_user_id"`
	AuthorName           string       `json:"author_name,omitempty"`
	AdditionallyRedacted bool         `json:"additionally_redacted,omitempty"`
}

// PulledTombstone is one deletion as the server serves it.
type PulledTombstone struct {
	Tombstone       Tombstone `json:"tombstone"`
	DeletedByUserID string    `json:"deleted_by_user_id"`
	DeletedByName   string    `json:"deleted_by_name,omitempty"`
}

// PullRequest asks for rows after Cursor (the server's per-organization sequence, gap
// free across records and tombstones), of the named kinds (memory, tombstone).
//
// Bootstrap applies to a handoff pull only: a device sets it on every page until its
// first handoff pull has drained (More false), and while it is set the server omits
// handoffs in a terminal state — a newly linked device receives only what is still
// open, however many pages that takes.
type PullRequest struct {
	SchemaVersion string   `json:"schema_version"`
	Cursor        int64    `json:"cursor"`
	Kinds         []string `json:"kinds"`
	Bootstrap     bool     `json:"bootstrap,omitempty"`
}

// PullRow is one sequenced row: exactly one of Memory, Tombstone and Handoff is set. A
// handoff row's Seq is on the organization's handoff sequence, which a memory pull
// never sees: the two are asked for in separate requests with separate cursors.
type PullRow struct {
	Seq       int64            `json:"seq"`
	Kind      string           `json:"kind"`
	Memory    *PulledMemory    `json:"memory,omitempty"`
	Tombstone *PulledTombstone `json:"tombstone,omitempty"`
	Handoff   *PulledHandoff   `json:"handoff,omitempty"`
}

// PullResponse is one page, in sequence order. Cursor is the last row's seq (or the
// request's cursor when empty); More says another page is waiting.
type PullResponse struct {
	Rows       []PullRow `json:"rows"`
	Cursor     int64     `json:"cursor"`
	More       bool      `json:"more"`
	ServerTime int64     `json:"server_time"`
}

// AckRequest records that this device applied every row up to Cursor.
type AckRequest struct {
	SchemaVersion string `json:"schema_version"`
	Cursor        int64  `json:"cursor"`
}

// AckResponse is the cursor the server holds for this device (monotonic).
type AckResponse struct {
	Cursor int64 `json:"cursor"`
}

// VerifyRequest asks how far one deletion has reached.
type VerifyRequest struct {
	SchemaVersion string `json:"schema_version"`
	RecordID      string `json:"record_id"`
}

// DeletionDevice is one device in a verification answer.
type DeletionDevice struct {
	DeviceID   string `json:"device_id"`
	Name       string `json:"name"`
	OwnerName  string `json:"owner_name"`
	Cursor     int64  `json:"cursor"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// VerifyResponse states a deletion's reach in four named, disjoint states (decision 16):
// acknowledged, outstanding (active, behind the tombstone), stale (outstanding and not
// seen within deletion_ack_horizon), and revoked ("may retain a copy" — revocation
// deletes nothing).
type VerifyResponse struct {
	RecordID     string           `json:"record_id"`
	Seq          int64            `json:"seq"`
	DeletedAt    string           `json:"deleted_at"`
	Acknowledged []DeletionDevice `json:"acknowledged"`
	Outstanding  []DeletionDevice `json:"outstanding"`
	Stale        []DeletionDevice `json:"stale"`
	Revoked      []DeletionDevice `json:"revoked"`
}
