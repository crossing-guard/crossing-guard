package store

// Governance persistence (governance-model.md rev. 3): the append-only event log
// (primary truth) plus the materialized fold (entity/session state). Writes go
// through a GovTx so one observed action commits atomically (event + fold together
// or not at all). store takes no dependency on engine — the daemon owns the fold
// policy and calls down here.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/engine"
)

// eventCols is the event projection, in the exact order scanEvents decodes. ONE
// definition so a new column is a single-site edit, not a three-way drift between the
// two queries and the decoder.
const (
	eventCols                = "id,ts,session_id,runtime,verb,tool,target_entity_id,tags,decision,reason,origin"
	defaultSessionTraceLimit = 100
	maxSessionTraceLimit     = 200
)

// EventRecord is one row of PRIMARY TRUTH (the append-only event log). Tags are the
// FROZEN classification (JSON) captured at observe time, so replaying the log re-folds
// state deterministically regardless of later detector edits. Origin is data LINEAGE
// ("live" | "transcript" | "imported") — distinct from entity/session-state provenance, the engine
// TRUST enum. The JSON tags are part of the API contract (ADR 0022, BYO-GUI), not
// decoration: this type is served verbatim by GET /api/govern/session, and Go field
// casing would have leaked `TargetEntityID` into a contract consumers build against.
// Decision is "" for every event recorded before commit 00032f0, when the field was not
// yet written — absent means UNKNOWN, never "allowed".
type EventRecord struct {
	ID        int64  `json:"id"`
	TS        int64  `json:"ts"`
	SessionID string `json:"session_id"`
	// Runtime is WHICH agent produced this action ("claude", "codex", …), reported by
	// the hook the installer wired into that vendor. Empty means the event predates
	// the field or came from a hook installed before it — absent is UNKNOWN, never
	// guessed at, and never back-filled by inference.
	Runtime        string `json:"runtime,omitempty"`
	Verb           string `json:"verb"`
	Tool           string `json:"tool"`
	TargetEntityID string `json:"target_entity_id,omitempty"`
	Tags           string `json:"tags,omitempty"` // JSON, frozen at observe time
	Decision       string `json:"decision,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Origin         string `json:"origin,omitempty"` // "live" | "transcript" | "imported"
	// GlobalID is the wire identity (engine.NewTypedID "evt"); minted by AppendEvent when
	// empty. Legacy rows carry a deterministic id from the V31 backfill.
	GlobalID string `json:"global_id,omitempty"`
}

// EventResource is one exact structured target associated with a returned event.
// It projects the existing event_resource relation; it is not another event log.
type EventResource struct {
	EventID       int64  `json:"event_id"`
	Ordinal       int    `json:"ordinal"`
	EntityID      string `json:"entity_id"`
	Kind          string `json:"kind"`
	Identity      string `json:"identity"`
	Source        string `json:"source"`
	RawIdentity   string `json:"raw_identity,omitempty"`
	Operation     string `json:"operation,omitempty"`
	EvidenceClass string `json:"evidence_class,omitempty"`
	SourceField   string `json:"source_field,omitempty"`
	Completeness  string `json:"completeness,omitempty"`
}

type EventDelivery struct {
	EventID           int64  `json:"event_id"`
	ObservationID     string `json:"observation_id"`
	ActionID          string `json:"action_id,omitempty"`
	ObservationSchema string `json:"observation_schema"`
	EnvelopeDigest    string `json:"envelope_digest"`
	CollectorID       string `json:"collector_id"`
	CollectorVersion  string `json:"collector_version,omitempty"`
	NativeCallID      string `json:"native_call_id,omitempty"`
	NativeCallKind    string `json:"native_call_kind,omitempty"`
	QueuedAt          int64  `json:"queued_at,omitempty"`
	ReceivedAt        int64  `json:"received_at"`
	DeliveryAttempts  int    `json:"delivery_attempts"`
	DeliveryMode      string `json:"delivery_mode"`
}

type EventInput struct {
	EventID       int64  `json:"event_id"`
	MediaType     string `json:"media_type"`
	RawBytes      int    `json:"raw_bytes"`
	CapturedBytes int    `json:"captured_bytes"`
	Digest        string `json:"digest,omitempty"`
	Completeness  string `json:"completeness"`
	Payload       []byte `json:"-"`
	SourceRef     string `json:"source_ref,omitempty"`
}

type EventResourceEvidence struct {
	EventID       int64
	Ordinal       int
	EntityID      string
	Source        string
	RawIdentity   string
	Operation     string
	EvidenceClass string
	SourceField   string
	Completeness  string
}

type SessionCheckpoint struct {
	ID                   int64  `json:"id"`
	Runtime              string `json:"runtime,omitempty"`
	SessionID            string `json:"session_id"`
	ScopeKey             string `json:"scope_key"`
	Kind                 string `json:"kind"`
	RequestID            string `json:"request_id"`
	TriggerEventID       int64  `json:"trigger_event_id,omitempty"`
	TriggerResultID      int64  `json:"trigger_result_id,omitempty"`
	TriggerObservationID string `json:"trigger_observation_id,omitempty"`
	PredecessorID        int64  `json:"predecessor_checkpoint_id,omitempty"`
	WorkingDirectory     string `json:"working_directory"`
	RepositoryID         string `json:"repository_id,omitempty"`
	CheckoutID           string `json:"checkout_id,omitempty"`
	CheckoutRoot         string `json:"checkout_root,omitempty"`
	Status               string `json:"status"`
	BoundaryClass        string `json:"boundary_class"`
	RequestedAt          int64  `json:"requested_at"`
	CaptureStartedAt     int64  `json:"capture_started_at,omitempty"`
	CaptureEndedAt       int64  `json:"capture_ended_at,omitempty"`
	CaptureAttempts      int    `json:"capture_attempts"`
	ChangeRecordID       int64  `json:"change_record_id,omitempty"`
	FailureKind          string `json:"failure_kind,omitempty"`
	DetailDigest         string `json:"detail_digest,omitempty"`
}

type CollectionIssue struct {
	ID              int64  `json:"id"`
	IssueID         string `json:"issue_id"`
	SessionID       string `json:"session_id,omitempty"`
	Runtime         string `json:"runtime,omitempty"`
	ObservationID   string `json:"observation_id,omitempty"`
	NativeCallID    string `json:"native_call_id,omitempty"`
	SourceRef       string `json:"source_ref,omitempty"`
	SourceSegmentID string `json:"source_segment_id,omitempty"`
	CollectorID     string `json:"collector_id"`
	Kind            string `json:"kind"`
	AffectedCount   int    `json:"affected_count"`
	FirstSeen       int64  `json:"first_seen"`
	LastSeen        int64  `json:"last_seen"`
	DetailDigest    string `json:"detail_digest,omitempty"`
	ResolvedAt      int64  `json:"resolved_at,omitempty"`
	ResolutionClass string `json:"resolution_class,omitempty"`
}

// StateRow is the persisted shape of one fold fact. The fold's identity and merge
// semantics are engine.FoldFact; the SQL upserts below implement that contract and a
// test pins them equal.
type StateRow = engine.StateFact

// GovTx is one atomic governance write (an observed action): append the event, then
// fold its state, then Commit. All-or-nothing, so the fold can never be missing an
// event that PRIMARY TRUTH already recorded, and a failure leaves nothing partial.
type GovTx struct{ tx *sql.Tx }

// BeginGov opens a governance write transaction.
func (ix *Index) BeginGov() (*GovTx, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return nil, err
	}
	return &GovTx{tx}, nil
}

func (g *GovTx) Commit() error   { return g.tx.Commit() }
func (g *GovTx) Rollback() error { return g.tx.Rollback() }

// AppendEvent appends one event to the log. Append-only by discipline — no update,
// no delete (the seam where a hash chain could later be added, per the model doc).
// AppendEvent writes one action. It is the ONE event writer: the global id is minted
// here, the per-session chain entry is computed here against the caller-held anchor
// (nil = unchained, for legacy paths and tests), and — only while this device is linked
// — the outbox row is written in the same transaction, so an event and its outbox
// entry commit or fail together (team plan §5.12).
func (g *GovTx) AppendEvent(e EventRecord, anchor *engine.ChainAnchor) (int64, error) {
	if e.GlobalID == "" {
		e.GlobalID = engine.NewTypedID("evt")
	}
	var chainSeq sql.NullInt64
	var prev, hash sql.NullString
	if anchor != nil {
		body := engine.EventChainBody{GlobalID: e.GlobalID, TS: e.TS, Session: e.SessionID,
			Runtime: e.Runtime, Verb: e.Verb, Tool: e.Tool, Target: e.TargetEntityID,
			TagsDigest: engine.TagsDigest(e.Tags), Decision: e.Decision, Reason: e.Reason, Origin: e.Origin}
		p, h := anchor.Advance(&body)
		chainSeq = sql.NullInt64{Int64: body.Seq, Valid: true}
		prev, hash = sql.NullString{String: p, Valid: true}, sql.NullString{String: h, Valid: true}
	}
	res, err := g.tx.Exec(
		`INSERT INTO event(ts,session_id,runtime,verb,tool,target_entity_id,tags,decision,reason,origin,global_id,chain_seq,prev_hash,hash)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TS, e.SessionID, e.Runtime, e.Verb, e.Tool, e.TargetEntityID, e.Tags, e.Decision, e.Reason, e.Origin,
		e.GlobalID, chainSeq, prev, hash)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	var linked int
	if err := g.tx.QueryRow(`SELECT linked FROM sync_device LIMIT 1`).Scan(&linked); err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if linked == 1 {
		contentHash := hash.String
		if contentHash == "" {
			contentHash = engine.TagsDigest(e.Tags)
		}
		if _, err := g.tx.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at) VALUES('event',?,?,?,?)`,
			e.GlobalID, contentHash, e.SessionID, e.TS); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// EventChainTail returns the last chained entry of a session, for seeding an anchor
// after a daemon restart. ok is false when the session has no chained rows.
func (ix *Index) EventChainTail(sessionID string) (seq int64, tail string, ok bool, err error) {
	err = ix.db.QueryRow(`SELECT chain_seq, hash FROM event WHERE session_id = ? AND hash IS NOT NULL ORDER BY chain_seq DESC LIMIT 1`, sessionID).Scan(&seq, &tail)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return seq, tail, true, nil
}

// EventChainRows reads a session's rows in append order as the engine's verification
// input. The tags digest is recomputed from the stored bytes, so an edited tags column
// fails verification like any other edited field.
func (ix *Index) EventChainRows(sessionID string) ([]engine.ChainRow, error) {
	rows, err := ix.db.Query(`SELECT global_id, COALESCE(chain_seq,0), ts, session_id, runtime, COALESCE(verb,''), COALESCE(tool,''),
		COALESCE(target_entity_id,''), COALESCE(tags,''), COALESCE(decision,''), COALESCE(reason,''), COALESCE(origin,''),
		COALESCE(prev_hash,''), COALESCE(hash,'') FROM event WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []engine.ChainRow
	for rows.Next() {
		var r engine.ChainRow
		var tags string
		if err := rows.Scan(&r.Body.GlobalID, &r.Body.Seq, &r.Body.TS, &r.Body.Session, &r.Body.Runtime, &r.Body.Verb, &r.Body.Tool,
			&r.Body.Target, &tags, &r.Body.Decision, &r.Body.Reason, &r.Body.Origin, &r.Prev, &r.Hash); err != nil {
			return nil, err
		}
		r.Body.TagsDigest = engine.TagsDigest(tags)
		out = append(out, r)
	}
	return out, rows.Err()
}

// VerifyEventChain recomputes a session's chain; pass the daemon's held anchor to also
// check the tail, or nil for internal consistency only (the report says which).
func (ix *Index) VerifyEventChain(sessionID string, held *engine.ChainAnchor) (engine.ChainReport, error) {
	rows, err := ix.EventChainRows(sessionID)
	if err != nil {
		return engine.ChainReport{}, err
	}
	return engine.VerifyEventChain(sessionID, rows, held), nil
}

// SessionRepository resolves the repository a session worked in, from its most recent
// checkpoint that recorded one. The checkout root is returned only so an encoder can make
// targets repo-relative; it never ships. ok is false when no checkpoint carried an identity.
func (ix *Index) SessionRepository(sessionID string) (repositoryID, checkoutRoot string, ok bool, err error) {
	err = ix.db.QueryRow(`SELECT repository_id, checkout_root FROM session_checkpoint
		WHERE session_id = ? AND repository_id != '' ORDER BY id DESC LIMIT 1`, sessionID).Scan(&repositoryID, &checkoutRoot)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return repositoryID, checkoutRoot, true, nil
}

// EventWireInputs reads a session's events, in append order, as input for
// engine.EncodeWireEvent: the row, its delivery time when one exists, the device
// identity, the session's other two identities, and its repository. It reads; it does
// not decide what is pushed — the outbox and the content policy do that.
func (ix *Index) EventWireInputs(sessionID string) ([]engine.WireEventInput, error) {
	deviceID, _, err := ix.Device()
	if err != nil {
		return nil, err
	}
	repositoryID, checkoutRoot, _, err := ix.SessionRepository(sessionID)
	if err != nil {
		return nil, err
	}
	native := sessionID
	if i := strings.Index(sessionID, "/"); i > 0 {
		native = sessionID[i+1:]
	}
	var catalogID, resumeID string
	if err := ix.db.QueryRow(`SELECT catalog_id, resume_id FROM sessions WHERE id IN (?, ?) LIMIT 1`, native, sessionID).Scan(&catalogID, &resumeID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	rows, err := ix.db.Query(`SELECT e.global_id, e.ts, COALESCE(d.received_at,0), e.session_id, e.runtime,
		COALESCE(e.verb,''), COALESCE(e.tool,''), COALESCE(e.target_entity_id,''), COALESCE(e.tags,''),
		COALESCE(e.decision,''), COALESCE(e.reason,''), COALESCE(e.origin,''),
		COALESCE(e.chain_seq,0), COALESCE(e.prev_hash,''), COALESCE(e.hash,'')
		FROM event e LEFT JOIN event_delivery d ON d.event_id = e.id
		WHERE e.session_id = ? ORDER BY e.id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []engine.WireEventInput
	for rows.Next() {
		in := engine.WireEventInput{DeviceID: deviceID, CatalogID: catalogID, ResumeID: resumeID,
			RepositoryID: repositoryID, CheckoutRoot: checkoutRoot}
		if err := rows.Scan(&in.GlobalID, &in.TS, &in.ReceivedAt, &in.Session, &in.Runtime, &in.Verb, &in.Tool,
			&in.TargetEntityID, &in.FrozenTags, &in.Decision, &in.Reason, &in.Origin, &in.ChainSeq, &in.PrevHash, &in.Hash); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// SchemaOnDisk reports the store's user_version and whether it is behind this binary's
// SchemaVersion — what a read-only opener meets on a store no read-write process has
// migrated yet. A caller that needs newer columns must refuse with that fact rather
// than fail on a SQL error the user cannot act on.
func (ix *Index) SchemaOnDisk() (have int, behind bool, err error) {
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return 0, false, err
	}
	return have, have < SchemaVersion, nil
}

// Device is this store's sync identity, minted at migration V31.
func (ix *Index) Device() (id string, linked bool, err error) {
	var l int
	err = ix.db.QueryRow(`SELECT id, linked FROM sync_device LIMIT 1`).Scan(&id, &l)
	return id, l == 1, err
}

// SetLinked flips outbox enqueueing. The outbox is born at head: nothing before this
// moment is enqueued implicitly (the 2026-09-12 replay-from-zero lesson).
func (ix *Index) SetLinked(linked bool) error {
	v := 0
	if linked {
		v = 1
	}
	_, err := ix.db.Exec(`UPDATE sync_device SET linked = ?`, v)
	return err
}

// OutboxPending counts unacknowledged outbox rows.
func (ix *Index) OutboxPending() (int, error) {
	var n int
	err := ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE acked_at IS NULL`).Scan(&n)
	return n, err
}

// AppendEventResource attaches one exact structured resource to an already appended
// event. It shares GovTx so an action, its targets, entities and fold commit together.
func (g *GovTx) AppendEventResource(eventID int64, ordinal int, entityID, source string) error {
	return g.AppendEventResourceEvidence(EventResourceEvidence{EventID: eventID, Ordinal: ordinal,
		EntityID: entityID, Source: source, Operation: "unknown", EvidenceClass: "unknown", Completeness: "unknown"})
}

func (g *GovTx) AppendEventResourceEvidence(r EventResourceEvidence) error {
	_, err := g.tx.Exec(`INSERT INTO event_resource(
		event_id,ordinal,entity_id,source,raw_identity,operation,evidence_class,source_field,completeness)
		VALUES(?,?,?,?,?,?,?,?,?)`, r.EventID, r.Ordinal, r.EntityID, r.Source, r.RawIdentity,
		r.Operation, r.EvidenceClass, r.SourceField, r.Completeness)
	return err
}

func (g *GovTx) AppendEventDelivery(d EventDelivery) error {
	_, err := g.tx.Exec(`INSERT INTO event_delivery(event_id,observation_id,observation_schema,
		action_id,envelope_digest,collector_id,collector_version,native_call_id,native_call_kind,queued_at,
		received_at,delivery_attempts,delivery_mode) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, d.EventID,
		d.ObservationID, d.ObservationSchema, d.ActionID, d.EnvelopeDigest, d.CollectorID, d.CollectorVersion,
		d.NativeCallID, d.NativeCallKind, d.QueuedAt, d.ReceivedAt, d.DeliveryAttempts, d.DeliveryMode)
	return err
}

func (g *GovTx) TouchEventDelivery(observationID string, attempts int) error {
	_, err := g.tx.Exec(`UPDATE event_delivery SET delivery_attempts=MAX(delivery_attempts,?) WHERE observation_id=?`, attempts, observationID)
	return err
}

func (g *GovTx) AppendEventInput(in EventInput) error {
	var payload any
	if in.Payload != nil {
		payload = in.Payload
	}
	_, err := g.tx.Exec(`INSERT INTO event_input(event_id,media_type,raw_bytes,captured_bytes,
		digest,completeness,payload,source_ref) VALUES(?,?,?,?,?,?,?,?)`, in.EventID, in.MediaType,
		in.RawBytes, in.CapturedBytes, in.Digest, in.Completeness, payload, in.SourceRef)
	return err
}

func observationIdentity(q interface{ QueryRow(string, ...any) *sql.Row }, observationID string) (int64, string, bool, error) {
	var eventID int64
	var digest string
	err := q.QueryRow(`SELECT event_id,envelope_digest FROM event_delivery WHERE observation_id=?`, observationID).Scan(&eventID, &digest)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}
	return eventID, digest, err == nil, err
}

func (g *GovTx) ObservationIdentity(observationID string) (int64, string, bool, error) {
	return observationIdentity(g.tx, observationID)
}

func (ix *Index) ObservationIdentity(observationID string) (int64, string, bool, error) {
	return observationIdentity(ix.db, observationID)
}

func (ix *Index) ObservationEvidence(observationID string) (EventDelivery, EventInput, bool, error) {
	var delivery EventDelivery
	var input EventInput
	var payload []byte
	err := ix.db.QueryRow(`SELECT d.event_id,d.observation_id,d.observation_schema,d.action_id,d.envelope_digest,
		d.collector_id,d.collector_version,d.native_call_id,d.native_call_kind,d.queued_at,d.received_at,
		d.delivery_attempts,d.delivery_mode,i.media_type,i.raw_bytes,i.captured_bytes,i.digest,
		i.completeness,i.payload,i.source_ref FROM event_delivery d JOIN event_input i ON i.event_id=d.event_id
		WHERE d.observation_id=?`, observationID).Scan(&delivery.EventID, &delivery.ObservationID,
		&delivery.ObservationSchema, &delivery.ActionID, &delivery.EnvelopeDigest, &delivery.CollectorID,
		&delivery.CollectorVersion, &delivery.NativeCallID, &delivery.NativeCallKind,
		&delivery.QueuedAt, &delivery.ReceivedAt, &delivery.DeliveryAttempts,
		&delivery.DeliveryMode, &input.MediaType, &input.RawBytes, &input.CapturedBytes,
		&input.Digest, &input.Completeness, &payload, &input.SourceRef)
	if err == sql.ErrNoRows {
		return delivery, input, false, nil
	}
	if err != nil {
		return delivery, input, false, err
	}
	input.EventID, input.Payload = delivery.EventID, payload
	return delivery, input, true, nil
}

const checkpointCols = `id,runtime,session_id,scope_key,kind,request_id,COALESCE(trigger_event_id,0),
	COALESCE(trigger_result_id,0),trigger_observation_id,COALESCE(predecessor_checkpoint_id,0),
	working_directory,repository_id,checkout_id,checkout_root,status,
	boundary_class,requested_at,capture_started_at,capture_ended_at,capture_attempts,
	COALESCE(change_record_id,0),failure_kind,detail_digest`

func scanCheckpoint(row *sql.Row) (SessionCheckpoint, error) {
	var c SessionCheckpoint
	err := row.Scan(&c.ID, &c.Runtime, &c.SessionID, &c.ScopeKey, &c.Kind, &c.RequestID,
		&c.TriggerEventID, &c.TriggerResultID, &c.TriggerObservationID, &c.PredecessorID,
		&c.WorkingDirectory, &c.RepositoryID, &c.CheckoutID,
		&c.CheckoutRoot, &c.Status, &c.BoundaryClass, &c.RequestedAt, &c.CaptureStartedAt,
		&c.CaptureEndedAt, &c.CaptureAttempts, &c.ChangeRecordID, &c.FailureKind, &c.DetailDigest)
	return c, err
}

func (g *GovTx) EnsureSessionCheckpoint(c SessionCheckpoint) (SessionCheckpoint, bool, error) {
	if c.RequestID == "" {
		c.RequestID = c.Kind + ":" + c.Runtime + ":" + c.SessionID + ":" + c.ScopeKey
	}
	res, err := g.tx.Exec(`INSERT INTO session_checkpoint(runtime,session_id,scope_key,kind,request_id,
		trigger_event_id,trigger_result_id,trigger_observation_id,predecessor_checkpoint_id,working_directory,repository_id,checkout_id,
		checkout_root,status,boundary_class,requested_at,capture_started_at,capture_ended_at,
		capture_attempts,change_record_id,failure_kind,detail_digest)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		c.Runtime, c.SessionID, c.ScopeKey, c.Kind, c.RequestID, nullableInt(c.TriggerEventID), nullableInt(c.TriggerResultID), c.TriggerObservationID, nullableInt(c.PredecessorID),
		c.WorkingDirectory, c.RepositoryID, c.CheckoutID, c.CheckoutRoot, c.Status,
		c.BoundaryClass, c.RequestedAt, c.CaptureStartedAt, c.CaptureEndedAt, c.CaptureAttempts,
		nullableInt(c.ChangeRecordID), c.FailureKind, c.DetailDigest)
	if err != nil {
		return SessionCheckpoint{}, false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return SessionCheckpoint{}, false, err
	}
	got, err := scanCheckpoint(g.tx.QueryRow(`SELECT `+checkpointCols+` FROM session_checkpoint WHERE request_id=? OR (runtime=? AND session_id=? AND scope_key=? AND kind=? AND kind IN ('attachment','pre-mutation')) ORDER BY id LIMIT 1`, c.RequestID, c.Runtime, c.SessionID, c.ScopeKey, c.Kind))
	return got, rows == 1, err
}

func (ix *Index) SessionCheckpointByID(id int64) (SessionCheckpoint, error) {
	return scanCheckpoint(ix.db.QueryRow(`SELECT `+checkpointCols+` FROM session_checkpoint WHERE id=?`, id))
}

func (ix *Index) AddSessionCheckpointTrigger(checkpointID, resultID int64, observationID string, acceptedAt int64) error {
	if checkpointID == 0 || resultID == 0 || observationID == "" {
		return fmt.Errorf("checkpoint trigger requires checkpoint, result, and observation identity")
	}
	_, err := ix.db.Exec(`INSERT INTO session_checkpoint_trigger(
		checkpoint_id,result_id,observation_id,accepted_at) VALUES(?,?,?,?)
		ON CONFLICT(checkpoint_id,result_id) DO NOTHING`, checkpointID, resultID,
		observationID, acceptedAt)
	return err
}

func (ix *Index) SessionCheckpointTriggerCount(checkpointID int64) (int, error) {
	var count int
	err := ix.db.QueryRow(`SELECT COUNT(*) FROM session_checkpoint_trigger WHERE checkpoint_id=?`,
		checkpointID).Scan(&count)
	return count, err
}

func (ix *Index) SessionCheckpointForObservation(observationID string) (SessionCheckpoint, bool, error) {
	c, err := scanCheckpoint(ix.db.QueryRow(`SELECT `+checkpointCols+` FROM session_checkpoint WHERE trigger_observation_id=? ORDER BY id LIMIT 1`, observationID))
	if err == sql.ErrNoRows {
		return SessionCheckpoint{}, false, nil
	}
	return c, err == nil, err
}

func (ix *Index) SessionCheckpointForWorkingDirectory(runtime, sessionID, cwd string) (SessionCheckpoint, bool, error) {
	c, err := scanCheckpoint(ix.db.QueryRow(`SELECT `+checkpointCols+` FROM session_checkpoint
		WHERE runtime=? AND session_id=? AND working_directory=? ORDER BY id LIMIT 1`, runtime, sessionID, cwd))
	if err == sql.ErrNoRows {
		return SessionCheckpoint{}, false, nil
	}
	return c, err == nil, err
}

func (ix *Index) PendingSessionCheckpoints(limit int) ([]SessionCheckpoint, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := ix.db.Query(`SELECT `+checkpointCols+` FROM session_checkpoint
		WHERE status IN ('pending','capturing') AND kind NOT IN ('attachment','pre-mutation')
		ORDER BY requested_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionCheckpoint{}
	for rows.Next() {
		var c SessionCheckpoint
		if err := rows.Scan(&c.ID, &c.Runtime, &c.SessionID, &c.ScopeKey, &c.Kind, &c.RequestID,
			&c.TriggerEventID, &c.TriggerResultID, &c.TriggerObservationID, &c.PredecessorID,
			&c.WorkingDirectory, &c.RepositoryID, &c.CheckoutID, &c.CheckoutRoot, &c.Status,
			&c.BoundaryClass, &c.RequestedAt, &c.CaptureStartedAt, &c.CaptureEndedAt,
			&c.CaptureAttempts, &c.ChangeRecordID, &c.FailureKind, &c.DetailDigest); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (ix *Index) SessionCheckpoints(sessionID string, limit int) ([]SessionCheckpoint, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := ix.db.Query(`SELECT `+checkpointCols+` FROM session_checkpoint
		WHERE session_id=? ORDER BY requested_at,id LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionCheckpoint{}
	for rows.Next() {
		var c SessionCheckpoint
		if err := rows.Scan(&c.ID, &c.Runtime, &c.SessionID, &c.ScopeKey, &c.Kind, &c.RequestID,
			&c.TriggerEventID, &c.TriggerResultID, &c.TriggerObservationID, &c.PredecessorID,
			&c.WorkingDirectory, &c.RepositoryID, &c.CheckoutID, &c.CheckoutRoot, &c.Status,
			&c.BoundaryClass, &c.RequestedAt, &c.CaptureStartedAt, &c.CaptureEndedAt,
			&c.CaptureAttempts, &c.ChangeRecordID, &c.FailureKind, &c.DetailDigest); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimSessionCheckpoint serializes automatic Git capture across concurrent hooks and
// daemon restarts. A capturing row older than staleBefore is recoverable after a crash.
func (ix *Index) ClaimSessionCheckpoint(id, now, staleBefore int64, boundaryClass string) (SessionCheckpoint, bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return SessionCheckpoint{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE session_checkpoint SET status='capturing',boundary_class=?,
		capture_started_at=?,capture_ended_at=0,capture_attempts=capture_attempts+1,
		failure_kind='',detail_digest=''
		WHERE id=? AND (status IN ('pending','failed','unavailable') OR
		(status='capturing' AND capture_started_at < ?))`, boundaryClass, now, id, staleBefore)
	if err != nil {
		return SessionCheckpoint{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return SessionCheckpoint{}, false, err
	}
	c, err := scanCheckpoint(tx.QueryRow(`SELECT `+checkpointCols+` FROM session_checkpoint WHERE id=?`, id))
	if err != nil {
		return SessionCheckpoint{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return SessionCheckpoint{}, false, err
	}
	return c, n == 1, nil
}

func (ix *Index) FailSessionCheckpoint(id, endedAt int64, status, boundaryClass, failureKind, detailDigest string) error {
	if status != "failed" && status != "unavailable" {
		return sql.ErrNoRows
	}
	if boundaryClass != "late-replay" && boundaryClass != "unconfirmed" && boundaryClass != "settled" && boundaryClass != "point-in-time" && boundaryClass != "exact-close" {
		return sql.ErrNoRows
	}
	res, err := ix.db.Exec(`UPDATE session_checkpoint SET status=?,boundary_class=?,capture_ended_at=?,
		change_record_id=NULL,failure_kind=?,detail_digest=? WHERE id=? AND status='capturing'`,
		status, boundaryClass, endedAt, failureKind, detailDigest, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (ix *Index) SetSessionCheckpointRepository(id int64, repositoryID, checkoutID, checkoutRoot string) error {
	res, err := ix.db.Exec(`UPDATE session_checkpoint SET repository_id=?,checkout_id=?,checkout_root=?
		WHERE id=? AND status='capturing'`, repositoryID, checkoutID, checkoutRoot, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (ix *Index) RecordCollectionIssue(issue CollectionIssue) error {
	if issue.AffectedCount < 1 {
		issue.AffectedCount = 1
	}
	if err := validateCollectionIssueIdentity(issue); err != nil {
		return err
	}
	_, err := ix.db.Exec(`INSERT INTO collection_issue(issue_id,session_id,runtime,observation_id,
		native_call_id,source_ref,source_segment_id,collector_id,kind,affected_count,first_seen,last_seen,
		detail_digest,resolved_at,resolution_class)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(issue_id) DO UPDATE SET
		affected_count=collection_issue.affected_count+excluded.affected_count,
		last_seen=MAX(collection_issue.last_seen,excluded.last_seen),
		detail_digest=CASE WHEN excluded.detail_digest!='' THEN excluded.detail_digest ELSE collection_issue.detail_digest END,
		resolved_at=CASE WHEN collection_issue.resolution_class='outside-observation' THEN 0 ELSE collection_issue.resolved_at END,
		resolution_class=CASE WHEN collection_issue.resolution_class='outside-observation' THEN '' ELSE collection_issue.resolution_class END`,
		issue.IssueID, issue.SessionID, issue.Runtime, issue.ObservationID, issue.NativeCallID,
		issue.SourceRef, issue.SourceSegmentID, issue.CollectorID,
		issue.Kind, issue.AffectedCount, issue.FirstSeen, issue.LastSeen, issue.DetailDigest,
		issue.ResolvedAt, issue.ResolutionClass)
	return err
}

var ErrCollectionIssueCollision = errors.New("collection issue identity collision")

func validateCollectionIssueIdentity(issue CollectionIssue) error {
	if issue.Kind == "parser-state-evicted" && (issue.NativeCallID == "" ||
		issue.SourceRef == "" || issue.SourceSegmentID == "") {
		return fmt.Errorf("parser-state-evicted issue requires native call, source, and segment identity")
	}
	return nil
}

func ensureCollectionIssueTx(tx *sql.Tx, issue CollectionIssue) error {
	if issue.IssueID == "" || issue.CollectorID == "" || issue.Kind == "" || issue.AffectedCount < 1 {
		return fmt.Errorf("collection issue occurrence requires identity, collector, kind, and count")
	}
	if err := validateCollectionIssueIdentity(issue); err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO collection_issue(issue_id,session_id,runtime,observation_id,
		native_call_id,source_ref,source_segment_id,collector_id,kind,affected_count,first_seen,last_seen,
		detail_digest,resolved_at,resolution_class) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(issue_id) DO NOTHING`, issue.IssueID, issue.SessionID, issue.Runtime,
		issue.ObservationID, issue.NativeCallID, issue.SourceRef, issue.SourceSegmentID,
		issue.CollectorID, issue.Kind, issue.AffectedCount, issue.FirstSeen, issue.LastSeen,
		issue.DetailDigest, issue.ResolvedAt, issue.ResolutionClass)
	if err != nil {
		return err
	}
	inserted, err := res.RowsAffected()
	if err != nil || inserted == 1 {
		return err
	}
	var existing CollectionIssue
	err = tx.QueryRow(`SELECT issue_id,session_id,runtime,observation_id,native_call_id,
		source_ref,source_segment_id,collector_id,kind,affected_count,first_seen,last_seen,
		detail_digest,resolved_at,resolution_class FROM collection_issue WHERE issue_id=?`, issue.IssueID).
		Scan(&existing.IssueID, &existing.SessionID, &existing.Runtime, &existing.ObservationID,
			&existing.NativeCallID, &existing.SourceRef, &existing.SourceSegmentID,
			&existing.CollectorID, &existing.Kind, &existing.AffectedCount, &existing.FirstSeen,
			&existing.LastSeen, &existing.DetailDigest, &existing.ResolvedAt,
			&existing.ResolutionClass)
	if err != nil {
		return err
	}
	if existing.IssueID != issue.IssueID || existing.SessionID != issue.SessionID ||
		existing.Runtime != issue.Runtime || existing.ObservationID != issue.ObservationID ||
		existing.NativeCallID != issue.NativeCallID || existing.SourceRef != issue.SourceRef ||
		existing.SourceSegmentID != issue.SourceSegmentID || existing.CollectorID != issue.CollectorID ||
		existing.Kind != issue.Kind || existing.AffectedCount != issue.AffectedCount ||
		existing.DetailDigest != issue.DetailDigest {
		return ErrCollectionIssueCollision
	}
	return nil
}

// EnsureCollectionIssue appends one immutable deterministic occurrence. Replaying the
// same semantic fact succeeds without incrementing its count; conflicting reuse fails.
func (ix *Index) EnsureCollectionIssue(issue CollectionIssue) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureCollectionIssueTx(tx, issue); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveCollectionIssue preserves the failure record while marking the first time
// its underlying collection gap was successfully recovered. Repeated recovery is
// idempotent and cannot rewrite the original resolution time.
func (ix *Index) ResolveCollectionIssue(issueID string, resolvedAt int64) error {
	res, err := ix.db.Exec(`UPDATE collection_issue
		SET resolved_at=CASE WHEN resolved_at=0 THEN ? ELSE resolved_at END,
			resolution_class='recovered'
		WHERE issue_id=? AND resolution_class!='recovered'`, resolvedAt, issueID)
	if err != nil {
		return err
	}
	_, err = res.RowsAffected()
	return err
}

func (ix *Index) CollectionIssueExists(issueID string) (bool, error) {
	var found int
	err := ix.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM collection_issue WHERE issue_id=?)`, issueID).Scan(&found)
	return found == 1, err
}

func (ix *Index) CollectionIssuesForSession(sessionID string, limit int) ([]CollectionIssue, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := ix.db.Query(`SELECT id,issue_id,session_id,runtime,observation_id,native_call_id,
		source_ref,source_segment_id,collector_id,kind,affected_count,first_seen,last_seen,
		detail_digest,resolved_at,resolution_class FROM collection_issue
		WHERE session_id=? ORDER BY last_seen DESC,id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []CollectionIssue{}
	for rows.Next() {
		var issue CollectionIssue
		if err := rows.Scan(&issue.ID, &issue.IssueID, &issue.SessionID, &issue.Runtime,
			&issue.ObservationID, &issue.NativeCallID, &issue.SourceRef, &issue.SourceSegmentID,
			&issue.CollectorID, &issue.Kind, &issue.AffectedCount,
			&issue.FirstSeen, &issue.LastSeen, &issue.DetailDigest, &issue.ResolvedAt,
			&issue.ResolutionClass); err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// IndexPath is the ONE resolver for the index location. It existed twice —
// identically — in internal/daemon and internal/memcli, and neither copy consulted
// the daemon's --data flag: the flag was honored for the token, the address file and
// the logs, but not for the database. So `--data /scratch` gave you scratch
// detectors driving writes into the REAL store, silently.
//
// Precedence, most specific first:
//  1. dataDir — pass non-empty ONLY when the operator set it explicitly. Passing a
//     defaulted value here would silently disable the env override below.
//  2. $CG_INDEX / $CPMEM_INDEX — an explicit file path (ambient, so it loses to 1).
//  3. ~/.crossing-guard/index.sqlite — the product default.
func IndexPath(dataDir, home string) string {
	if dataDir != "" {
		return filepath.Join(dataDir, "index.sqlite")
	}
	for _, k := range []string{"CG_INDEX", "CPMEM_INDEX"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return filepath.Join(home, ".crossing-guard", "index.sqlite")
}

// EventsForSession returns a session's events, oldest first. The FIRST reader of
// the event table: until now it was written and never read, so nothing could show
// or verify what was captured — including whether enforcement fired. Bounded by
// `limit` because a long session's log is unbounded (retention is R8/D10).
func (ix *Index) EventsForSession(sessionID string, limit int) ([]EventRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	return scanEvents(ix.db.Query(
		`SELECT `+eventCols+` FROM event WHERE session_id=? ORDER BY ts, id LIMIT ?`, sessionID, limit))
}

// SessionGovernanceCursor returns the highest ingested event id for one
// session — the live view's governance delta cursor (natural-session plan
// B-GUI). Everything at or below it is history the panels already render.
func (ix *Index) SessionGovernanceCursor(runtime, sessionID string) (int64, error) {
	var cursor int64
	err := ix.db.QueryRow(`SELECT COALESCE(max(id),0) FROM event WHERE runtime=? AND session_id=?`,
		runtime, sessionID).Scan(&cursor)
	return cursor, err
}

// SessionGovernanceAfter returns ingested event rows for one session with id
// strictly greater than after, oldest first, bounded — the live governed-
// action feed. The new cursor is the last returned row's id (unchanged when
// nothing is new).
func (ix *Index) SessionGovernanceAfter(runtime, sessionID string, after int64, limit int) ([]EventRecord, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	events, err := scanEvents(ix.db.Query(
		`SELECT `+eventCols+` FROM event WHERE runtime=? AND session_id=? AND id>? ORDER BY id LIMIT ?`,
		runtime, sessionID, after, limit))
	if err != nil {
		return nil, after, err
	}
	cursor := after
	if n := len(events); n > 0 {
		cursor = events[n-1].ID
	}
	return events, cursor, nil
}

// EventResourcesForSession returns associations only for the same bounded event
// population exposed by EventsForSession, preserving event and resource order.
func (ix *Index) EventResourcesForSession(sessionID string, eventLimit, resourceLimit int) ([]EventResource, int, error) {
	if eventLimit <= 0 {
		eventLimit = 500
	}
	if resourceLimit <= 0 {
		resourceLimit = 5000
	}
	const selected = `WITH returned AS (SELECT id FROM event WHERE session_id=? ORDER BY ts,id LIMIT ?)`
	var total int
	if err := ix.db.QueryRow(selected+` SELECT COUNT(*) FROM returned r JOIN event_resource er ON er.event_id=r.id`, sessionID, eventLimit).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := ix.db.Query(selected+` SELECT er.event_id,er.ordinal,er.entity_id,e.kind,e.identity,er.source,
		er.raw_identity,er.operation,er.evidence_class,er.source_field,er.completeness
	FROM returned r JOIN event_resource er ON er.event_id=r.id
	JOIN entity e ON e.id=er.entity_id ORDER BY er.event_id,er.ordinal LIMIT ?`, sessionID, eventLimit, resourceLimit)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []EventResource{}
	for rows.Next() {
		var resource EventResource
		if err := rows.Scan(&resource.EventID, &resource.Ordinal, &resource.EntityID,
			&resource.Kind, &resource.Identity, &resource.Source, &resource.RawIdentity,
			&resource.Operation, &resource.EvidenceClass, &resource.SourceField,
			&resource.Completeness); err != nil {
			return nil, 0, err
		}
		out = append(out, resource)
	}
	return out, total, rows.Err()
}

// SessionEventStat is the unbounded aggregate behind exact session totals. Event
// payloads remain capped for response size; a cap must never masquerade as the total.
type SessionEventStat struct {
	Total       int   `json:"total"`
	LastEventTS int64 `json:"last_event_ts"`
}

// SessionDecisionStat is an exact aggregate over a session's complete action log.
// Empty Decision rows are retained as Unknown; callers must not describe them as allow.
type SessionDecisionStat struct {
	Allow   int `json:"allow"`
	Ask     int `json:"ask"`
	Deny    int `json:"deny"`
	Unknown int `json:"unknown"`
}

// DecisionStatForSession returns exact decision populations for one session.
func (ix *Index) DecisionStatForSession(sessionID string) (SessionDecisionStat, error) {
	var out SessionDecisionStat
	err := ix.db.QueryRow(`SELECT
		COUNT(CASE WHEN decision='allow' THEN 1 END),
		COUNT(CASE WHEN decision='ask' THEN 1 END),
		COUNT(CASE WHEN decision='deny' THEN 1 END),
		COUNT(CASE WHEN COALESCE(decision,'')='' THEN 1 END)
		FROM event WHERE session_id=?`, sessionID).
		Scan(&out.Allow, &out.Ask, &out.Deny, &out.Unknown)
	return out, err
}

// SessionFactCounts holds exact aggregate populations for the session header.
type SessionFactCounts struct {
	Resources           int `json:"resources"`
	Results             int `json:"results"`
	Checkpoints         int `json:"checkpoints"`
	PathReconciliations int `json:"path_reconciliations"`
	OpenIssues          int `json:"open_issues"`
	CheckoutChanged     int `json:"checkout_changed"`
}

// FactCountsForSession counts existing evidence families without loading their rows.
func (ix *Index) FactCountsForSession(sessionID string) (SessionFactCounts, error) {
	var out SessionFactCounts
	err := ix.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM event_resource er JOIN event e ON e.id=er.event_id WHERE e.session_id=?),
		(SELECT COUNT(*) FROM result_observation WHERE session_id=?),
		(SELECT COUNT(*) FROM session_checkpoint WHERE session_id=?),
		(SELECT COUNT(*) FROM path_reconciliation WHERE session_id=?),
		(SELECT COUNT(*) FROM collection_issue WHERE session_id=? AND resolved_at=0),
		(SELECT COUNT(DISTINCT CAST(i.record_id AS TEXT)||char(0)||i.path) FROM change_item i WHERE i.record_id IN (
			SELECT MAX(id) FROM change_record WHERE session_id=? AND kind='revision'
			GROUP BY repository_id,checkout_id))`,
		sessionID, sessionID, sessionID, sessionID, sessionID, sessionID).
		Scan(&out.Resources, &out.Results, &out.Checkpoints, &out.PathReconciliations,
			&out.OpenIssues, &out.CheckoutChanged)
	return out, err
}

// SessionEvidenceExists reports whether any primary session evidence family has rows.
func (ix *Index) SessionEvidenceExists(sessionID string) (bool, error) {
	var exists int
	err := ix.db.QueryRow(`SELECT (EXISTS(SELECT 1 FROM event WHERE session_id=? LIMIT 1)
		OR EXISTS(SELECT 1 FROM result_observation WHERE session_id=? LIMIT 1)
		OR EXISTS(SELECT 1 FROM session_checkpoint WHERE session_id=? LIMIT 1)
		OR EXISTS(SELECT 1 FROM change_record WHERE session_id=? LIMIT 1))`,
		sessionID, sessionID, sessionID, sessionID).Scan(&exists)
	return exists == 1, err
}

// SessionRuntimeDecisionStat powers the Reach tab without downloading action rows.
type SessionRuntimeDecisionStat struct {
	Runtime string `json:"runtime"`
	Actions int    `json:"actions"`
	Allow   int    `json:"allow"`
	Ask     int    `json:"ask"`
	Deny    int    `json:"deny"`
	Unknown int    `json:"unknown"`
}

// RuntimeDecisionStatsForSession returns exact action/decision counts grouped by runtime.
func (ix *Index) RuntimeDecisionStatsForSession(sessionID string) ([]SessionRuntimeDecisionStat, error) {
	rows, err := ix.db.Query(`SELECT runtime,COUNT(*),
		COUNT(CASE WHEN decision='allow' THEN 1 END),
		COUNT(CASE WHEN decision='ask' THEN 1 END),
		COUNT(CASE WHEN decision='deny' THEN 1 END),
		COUNT(CASE WHEN COALESCE(decision,'')='' THEN 1 END)
		FROM event WHERE session_id=? GROUP BY runtime ORDER BY COUNT(*) DESC,runtime`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionRuntimeDecisionStat{}
	for rows.Next() {
		var stat SessionRuntimeDecisionStat
		if err := rows.Scan(&stat.Runtime, &stat.Actions, &stat.Allow, &stat.Ask,
			&stat.Deny, &stat.Unknown); err != nil {
			return nil, err
		}
		out = append(out, stat)
	}
	return out, rows.Err()
}

// SessionTraceQuery describes one stable walk over the action log. SnapshotID is the
// largest event row visible to the walk; later/backfilled inserts wait for refresh.
type SessionTraceQuery struct {
	SnapshotID int64
	AfterTS    int64
	AfterID    int64
	Limit      int
	Decision   string
	Origin     string
	Query      string
}

// SessionTracePage is one snapshot-bound keyset page plus its exact filtered total.
type SessionTracePage struct {
	Events     []EventRecord   `json:"events"`
	Resources  []EventResource `json:"event_resources"`
	Total      int             `json:"total"`
	SnapshotID int64           `json:"snapshot_id"`
	HasMore    bool            `json:"has_more"`
	LastTS     int64           `json:"-"`
	LastID     int64           `json:"-"`
}

func traceWhere(sessionID string, q SessionTraceQuery, after bool) (string, []any) {
	where := []string{"e.session_id=?", "e.id<=?"}
	args := []any{sessionID, q.SnapshotID}
	if q.Decision != "" {
		where = append(where, "e.decision=?")
		args = append(args, q.Decision)
	}
	if q.Origin != "" {
		where = append(where, "e.origin=?")
		args = append(args, q.Origin)
	}
	if q.Query != "" {
		where = append(where, `(LOWER(COALESCE(e.runtime,'')||' '||COALESCE(e.tool,'')||' '||COALESCE(e.verb,'')||' '||COALESCE(e.decision,'')||' '||COALESCE(e.origin,'')) LIKE ?
			OR EXISTS (SELECT 1 FROM event_resource er JOIN entity n ON n.id=er.entity_id
				WHERE er.event_id=e.id AND LOWER(n.kind||':'||n.identity||' '||er.operation) LIKE ?))`)
		like := "%" + strings.ToLower(q.Query) + "%"
		args = append(args, like, like)
	}
	if after && (q.AfterTS != 0 || q.AfterID != 0) {
		where = append(where, "(e.ts>? OR (e.ts=? AND e.id>?))")
		args = append(args, q.AfterTS, q.AfterTS, q.AfterID)
	}
	return strings.Join(where, " AND "), args
}

// TracePageForSession filters before paging and returns all declared resources for
// the returned action IDs. The extra row determines whether a next cursor exists.
func (ix *Index) TracePageForSession(sessionID string, q SessionTraceQuery) (SessionTracePage, error) {
	var out SessionTracePage
	if q.Limit <= 0 || q.Limit > maxSessionTraceLimit {
		q.Limit = defaultSessionTraceLimit
	}
	if q.SnapshotID <= 0 {
		if err := ix.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM event WHERE session_id=?`, sessionID).
			Scan(&q.SnapshotID); err != nil {
			return out, err
		}
	}
	out.SnapshotID = q.SnapshotID
	countWhere, countArgs := traceWhere(sessionID, q, false)
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM event e WHERE `+countWhere, countArgs...).Scan(&out.Total); err != nil {
		return out, err
	}
	pageWhere, pageArgs := traceWhere(sessionID, q, true)
	pageArgs = append(pageArgs, q.Limit+1)
	events, err := scanEvents(ix.db.Query(`SELECT `+eventCols+` FROM event e WHERE `+pageWhere+
		` ORDER BY e.ts,e.id LIMIT ?`, pageArgs...))
	if err != nil {
		return out, err
	}
	if len(events) > q.Limit {
		out.HasMore = true
		events = events[:q.Limit]
	}
	out.Events = events
	if len(events) == 0 {
		out.Resources = []EventResource{}
		return out, nil
	}
	out.LastTS, out.LastID = events[len(events)-1].TS, events[len(events)-1].ID
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(events)), ",")
	args := make([]any, 0, len(events))
	for _, event := range events {
		args = append(args, event.ID)
	}
	rows, err := ix.db.Query(`SELECT er.event_id,er.ordinal,er.entity_id,n.kind,n.identity,er.source,
		er.raw_identity,er.operation,er.evidence_class,er.source_field,er.completeness
		FROM event_resource er JOIN entity n ON n.id=er.entity_id
		WHERE er.event_id IN (`+placeholders+`) ORDER BY er.event_id,er.ordinal`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Resources = []EventResource{}
	for rows.Next() {
		var resource EventResource
		if err := rows.Scan(&resource.EventID, &resource.Ordinal, &resource.EntityID,
			&resource.Kind, &resource.Identity, &resource.Source, &resource.RawIdentity,
			&resource.Operation, &resource.EvidenceClass, &resource.SourceField,
			&resource.Completeness); err != nil {
			return out, err
		}
		out.Resources = append(out.Resources, resource)
	}
	return out, rows.Err()
}

// EventForSession returns an event only when the supplied session owns it.
func (ix *Index) EventForSession(sessionID string, eventID int64) (EventRecord, error) {
	rows, err := scanEvents(ix.db.Query(`SELECT `+eventCols+` FROM event WHERE session_id=? AND id=?`, sessionID, eventID))
	if err != nil {
		return EventRecord{}, err
	}
	if len(rows) == 0 {
		return EventRecord{}, sql.ErrNoRows
	}
	return rows[0], nil
}

// EventResourcesForEvent returns all ordered declared resources for one event.
func (ix *Index) EventResourcesForEvent(eventID int64) ([]EventResource, error) {
	rows, err := ix.db.Query(`SELECT er.event_id,er.ordinal,er.entity_id,n.kind,n.identity,er.source,
		er.raw_identity,er.operation,er.evidence_class,er.source_field,er.completeness
		FROM event_resource er JOIN entity n ON n.id=er.entity_id WHERE er.event_id=?
		ORDER BY er.ordinal`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventResource{}
	for rows.Next() {
		var resource EventResource
		if err := rows.Scan(&resource.EventID, &resource.Ordinal, &resource.EntityID,
			&resource.Kind, &resource.Identity, &resource.Source, &resource.RawIdentity,
			&resource.Operation, &resource.EvidenceClass, &resource.SourceField,
			&resource.Completeness); err != nil {
			return nil, err
		}
		out = append(out, resource)
	}
	return out, rows.Err()
}

func (ix *Index) EventStatForSession(sessionID string) (SessionEventStat, error) {
	var out SessionEventStat
	err := ix.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(ts),0) FROM event WHERE session_id=?`, sessionID).
		Scan(&out.Total, &out.LastEventTS)
	return out, err
}

// RuntimeStat is one agent's presence in the event log: how much of the record it
// produced, and when it was last seen producing any. It is the evidence behind
// "is this runtime's hook actually FIRING" — a question no config file can
// answer, because a config records registration and this records behaviour.
type RuntimeStat struct {
	Runtime   string `json:"runtime"` // "" = events from a hook older than the flag
	Events    int    `json:"events"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// RuntimeStats groups the event log by runtime, most recently active first. The
// unattributed bucket (runtime "") is RETURNED, never dropped: hiding it would
// make a log that is 90% unattributed look fully attributed, and the size of that
// bucket is how a reader knows how much of their history predates the field.
// Keep this scan unindexed until the event table reaches roughly 1,000,000 rows
// or representative local measurements put /api/govern/runtimes above 50 ms p50.
// That evidence, rather than an assumed future bottleneck, triggers event(runtime).
func (ix *Index) RuntimeStats() ([]RuntimeStat, error) {
	rows, err := ix.db.Query(`SELECT COALESCE(runtime,''), COUNT(*), MIN(ts), MAX(ts)
	                            FROM event GROUP BY COALESCE(runtime,'') ORDER BY MAX(ts) DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RuntimeStat{}
	for rows.Next() {
		var s RuntimeStat
		if err := rows.Scan(&s.Runtime, &s.Events, &s.FirstSeen, &s.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SessionEventCount is one session's presence in the event log — the rail's source.
// Deliberately NOT the harvest session list: that one enumerates vendor files and
// knows nothing about what we captured, so it cannot answer "which sessions do we
// actually hold governance rows for".
type SessionEventCount struct {
	SessionID string `json:"session_id"`
	Events    int    `json:"events"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// IndexMemoryText makes a memory searchable through the store's one relational search-
// content owner. External-content FTS is trigger-derived; memory code never dual-writes
// the virtual table.
func (ix *Index) IndexMemoryText(id string, ts int64, text string) error {
	return ix.replaceMemorySearchDocument(id, ts, text)
}

// UpsertMemoryEntity records a memory as a governed entity plus its folded
// classification, in one transaction (Phase 6). Memory is a RESOURCE, not an action,
// so it does NOT append to the event log — it establishes the entity and its state
// directly, the same entity/entity_state model the live fold uses. Re-indexing
// refreshes last_seen and re-folds; stale labels are handled by the caller re-running.
func (ix *Index) UpsertMemoryEntity(id string, ts int64, state []StateRow) error {
	tx, err := ix.BeginGov()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.UpsertEntity(id, "memory", memoryIdentity(id), ts); err != nil {
		return err
	}
	for _, s := range state {
		if err := tx.UpsertEntityState(id, s, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// memoryIdentity strips the entity-id prefix back to the bare memory id for the
// entity's identity column (mirrors how file/url identities carry the raw value).
func memoryIdentity(entityID string) string {
	if id, ok := strings.CutPrefix(entityID, "memory:"); ok {
		return id
	}
	return entityID
}

// CountEventsByOrigin returns how many of a session's events carry the given origin
// ("live"|"imported"). The importer uses it to (a) never clobber a session already
// captured LIVE, and (b) know whether a re-import needs to clear prior imported rows.
func (ix *Index) CountEventsByOrigin(sessionID, origin string) (int64, error) {
	var n int64
	err := ix.db.QueryRow(`SELECT COUNT(*) FROM event WHERE session_id=? AND origin=?`,
		sessionID, origin).Scan(&n)
	return n, err
}

// DeleteImportedForSession removes a session's IMPORTED events so a re-import replaces
// rather than duplicates them. It touches only origin='imported' — live truth is never
// deleted by an import.
func (ix *Index) DeleteImportedForSession(sessionID string) (int64, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteResultReconciliationsForEvents(tx,
		`SELECT id FROM event WHERE session_id=? AND origin='imported'`, sessionID); err != nil {
		return 0, err
	}
	for _, table := range []string{"event_input", "event_delivery", "event_resource"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE event_id IN (SELECT id FROM event WHERE session_id=? AND origin='imported')`, sessionID); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`DELETE FROM event WHERE session_id=? AND origin='imported'`, sessionID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// CountEventsBefore reports how many events are older than cutoff (unix seconds).
// The dry-run half of retention (D10/R8): a prune of PRIMARY TRUTH must show what it
// would delete before it deletes it.
func (ix *Index) CountEventsBefore(cutoff int64) (int64, error) {
	var n int64
	err := ix.db.QueryRow(`SELECT COUNT(*) FROM event WHERE ts < ?`, cutoff).Scan(&n)
	return n, err
}

// PruneEventsBefore deletes events older than cutoff and returns the count removed.
// This is the ONLY delete of the append-only event log, and it is deliberately not
// automatic and not folded into the hot path: retention of PRIMARY TRUTH is an
// operator act (`crossing-guard prune`), preceded by an export, never a silent background trim.
//
// It does NOT touch the folded state tables: session_state/entity_state are the
// CURRENT belief (already materialized with their own first/last_seen), so pruning old
// events leaves today's state intact — the cost is only that a full replay can no
// longer reconstruct history before the cutoff, which is exactly what retention trades.
// Disk is reclaimed on the next VACUUM/export, not here (VACUUM needs an exclusive lock
// the live daemon holds off).
func (ix *Index) PruneEventsBefore(cutoff int64) (int64, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteResultReconciliationsForEvents(tx,
		`SELECT id FROM event WHERE ts < ?`, cutoff); err != nil {
		return 0, err
	}
	for _, table := range []string{"event_input", "event_delivery", "event_resource"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE event_id IN (SELECT id FROM event WHERE ts < ?)`, cutoff); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`DELETE FROM event WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// deleteResultReconciliationsForEvents removes derived links before their action
// evidence is deliberately removed. Result observations and effects are independent
// primary evidence and remain retained; a later pass can honestly classify them as
// unjoined. Keeping an "exact" link to a deleted event would be a false fact, and the
// event foreign key deliberately prevents that state.
func deleteResultReconciliationsForEvents(tx *sql.Tx, eventQuery string, args ...any) error {
	rows, err := tx.Query(`SELECT DISTINCT reconciliation_id
		FROM result_reconciliation_candidate
		WHERE event_id IN (`+eventQuery+`)`, args...)
	if err != nil {
		return err
	}
	var reconciliationIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		reconciliationIDs = append(reconciliationIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range reconciliationIDs {
		if _, err := tx.Exec(`DELETE FROM result_reconciliation_candidate WHERE reconciliation_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM result_reconciliation WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// EventLogStat is the cheap health summary of the append-only log: how much is in it
// and how recent the newest row is. The whole point of D13 is that a capture surface
// must be able to say "the last thing we captured was N minutes ago" — silence is the
// symptom of every silent-loss path, so the newest timestamp is the detector.
type EventLogStat struct {
	Total       int64 `json:"total"`
	LastEventTS int64 `json:"last_event_ts"` // 0 if the log is empty
}

// EventLogStat returns the row count and newest ts in one pass.
func (ix *Index) EventLogStat() (EventLogStat, error) {
	var s EventLogStat
	var last sql.NullInt64
	err := ix.db.QueryRow(`SELECT COUNT(*), MAX(ts) FROM event`).Scan(&s.Total, &last)
	if err != nil {
		return s, err
	}
	if last.Valid {
		s.LastEventTS = last.Int64
	}
	return s, nil
}

// SessionsWithEvents lists sessions holding at least one event, most recent first.
func (ix *Index) SessionsWithEvents(limit int) ([]SessionEventCount, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := ix.db.Query(
		`SELECT session_id, COUNT(*), MIN(ts), MAX(ts) FROM event
		   WHERE session_id != '' GROUP BY session_id
		   ORDER BY MAX(ts) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SessionEventCount{}
	for rows.Next() {
		var s SessionEventCount
		if err := rows.Scan(&s.SessionID, &s.Events, &s.FirstSeen, &s.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// scanEvents decodes event rows. ONE decoder for both readers: a second hand-written
// Scan would drift the moment a column is added, and the columns here are the frozen
// truth record.
func scanEvents(rows *sql.Rows, err error) ([]EventRecord, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EventRecord
	for rows.Next() {
		var e EventRecord
		if err := rows.Scan(&e.ID, &e.TS, &e.SessionID, &e.Runtime, &e.Verb, &e.Tool, &e.TargetEntityID,
			&e.Tags, &e.Decision, &e.Reason, &e.Origin); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventsForEntity returns events that named an entity through either the current
// plural-resource relation or the legacy primary-target column, oldest first. DISTINCT
// preserves action cardinality when a compatibility event contains both relations.
func (ix *Index) EventsForEntity(entityID string, limit int) ([]EventRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	return scanEvents(ix.db.Query(`WITH matched AS (
		SELECT id FROM event WHERE target_entity_id=?
		UNION SELECT event_id FROM event_resource WHERE entity_id=?
	) SELECT event.id,event.ts,event.session_id,event.runtime,event.verb,event.tool,
		event.target_entity_id,event.tags,event.decision,event.reason,event.origin
		FROM event JOIN matched ON matched.id=event.id ORDER BY event.ts,event.id LIMIT ?`,
		entityID, entityID, limit))
}

// Entity is a governed resource plus its observation window.
type Entity struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Identity  string `json:"identity"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// LookupEntity returns one entity, or nil when it has never been observed. Absence
// is not an error: "we have no record of this file" is a real, sayable answer.
func (ix *Index) LookupEntity(id string) (*Entity, error) {
	var e Entity
	err := ix.db.QueryRow(
		`SELECT id,kind,identity,first_seen,last_seen FROM entity WHERE id=?`, id).
		Scan(&e.ID, &e.Kind, &e.Identity, &e.FirstSeen, &e.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// UpsertEntity records/refreshes a governed resource. The (first_seen,last_seen)
// window is folded with MIN/MAX so it is independent of arrival order — the property
// that makes live-accumulated state equal a ts-ordered replay (model doc R2).
func (g *GovTx) UpsertEntity(id, kind, identity string, ts int64) error {
	_, err := g.tx.Exec(
		`INSERT INTO entity(id,kind,identity,first_seen,last_seen) VALUES(?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   first_seen=MIN(first_seen,excluded.first_seen),
		   last_seen=MAX(last_seen,excluded.last_seen)`,
		id, kind, identity, ts, ts)
	return err
}

// UpsertEntityState folds one state fact onto an entity. Identity is
// (entity,key,value,detector); the observation window folds MIN/MAX (order-independent).
// provenance and evidence are left as first-written: provenance is fixed per detector,
// and evidence is a non-load-bearing sample — updating either on conflict would make the
// fold order-sensitive again for no governance benefit.
func (g *GovTx) UpsertEntityState(entityID string, s StateRow, ts int64) error {
	_, err := g.tx.Exec(
		`INSERT INTO entity_state(entity_id,key,value,detector,provenance,evidence,first_seen,last_seen)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(entity_id,key,value,detector) DO UPDATE SET
		   first_seen=MIN(first_seen,excluded.first_seen),
		   last_seen=MAX(last_seen,excluded.last_seen)`,
		entityID, s.Key, s.Value, s.Detector, s.Provenance, s.Evidence, ts, ts)
	return err
}

// UpsertSessionState folds one state fact onto a session (intrinsic or accumulated).
func (g *GovTx) UpsertSessionState(sessionID string, s StateRow, ts int64) error {
	_, err := g.tx.Exec(
		`INSERT INTO session_state(session_id,key,value,detector,provenance,evidence,first_seen,last_seen)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(session_id,key,value,detector) DO UPDATE SET
		   first_seen=MIN(first_seen,excluded.first_seen),
		   last_seen=MAX(last_seen,excluded.last_seen)`,
		sessionID, s.Key, s.Value, s.Detector, s.Provenance, s.Evidence, ts, ts)
	return err
}

// SessionState returns the folded state for a session.
func (ix *Index) SessionState(sessionID string) ([]StateRow, error) {
	return scanState(ix.db.Query(
		`SELECT key,value,detector,provenance,evidence,first_seen,last_seen
		 FROM session_state WHERE session_id=? ORDER BY key,value`, sessionID))
}

// SessionStateFacet is one folded detector fact with the session it is on.
type SessionStateFacet struct {
	SessionID string
	StateRow
}

// AllSessionStateFacets reads the folded detector state of every session, most
// recently seen first, for callers that decorate a whole session list. It is a
// plain read (no transaction: the store's transactions are write-locked). The
// bool reports that limit cut the read short. session_state carries no runtime,
// so neither does a facet; the caller joins by session identity.
func (ix *Index) AllSessionStateFacets(limit int) ([]SessionStateFacet, bool, error) {
	rows, err := ix.db.Query(`SELECT session_id,key,value,detector,provenance,first_seen,last_seen
		FROM session_state ORDER BY last_seen DESC LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []SessionStateFacet
	for rows.Next() {
		var facet SessionStateFacet
		if err := rows.Scan(&facet.SessionID, &facet.Key, &facet.Value, &facet.Detector,
			&facet.Provenance, &facet.FirstSeen, &facet.LastSeen); err != nil {
			return nil, false, err
		}
		out = append(out, facet)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// EntityState returns the folded state for an entity.
func (ix *Index) EntityState(entityID string) ([]StateRow, error) {
	return scanState(ix.db.Query(
		`SELECT key,value,detector,provenance,evidence,first_seen,last_seen
		 FROM entity_state WHERE entity_id=? ORDER BY key,value`, entityID))
}

func scanState(rows *sql.Rows, err error) ([]StateRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateRow
	for rows.Next() {
		var s StateRow
		if err := rows.Scan(&s.Key, &s.Value, &s.Detector, &s.Provenance, &s.Evidence, &s.FirstSeen, &s.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// EntityWithState is one governed resource plus the facts folded onto it.
type EntityWithState struct {
	Entity Entity
	State  []StateRow
}

// ListEntities returns governed resources, most recently seen first, each with its
// folded state. The inspection surface behind `crossing-guard entities`: until now the entity
// table had no reader outside a single-id lookup, so "what does the system believe
// about my files" was unanswerable without SQL.
func (ix *Index) ListEntities(kind string, limit int) ([]EntityWithState, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id,kind,identity,first_seen,last_seen FROM entity`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind=?`
		args = append(args, kind)
	}
	q += ` ORDER BY last_seen DESC LIMIT ?`
	args = append(args, limit)

	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var out []EntityWithState
	for rows.Next() {
		var e Entity
		if err := rows.Scan(&e.ID, &e.Kind, &e.Identity, &e.FirstSeen, &e.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, EntityWithState{Entity: e})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// State per entity after the cursor is closed — one connection, no nested query.
	for i := range out {
		st, err := ix.EntityState(out[i].Entity.ID)
		if err != nil {
			return nil, err
		}
		out[i].State = st
	}
	return out, nil
}
