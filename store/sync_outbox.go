package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"crossing-guard/engine"
)

// The sync outbox (team plan §5.12, item 4 §4.2 decision 1). Rows are written in the
// SAME transaction as their source row, only while this device is linked (born at head),
// and drained by the daemon's push job oldest-first. This file is the outbox's one owner:
// the one writer every enqueue site calls, and the drain's read, ack, and summary.

// Outbox record kinds as the outbox stores them; the wire kinds share the spelling.
const (
	OutboxEvent          = "event"
	OutboxMemory         = "memory"
	OutboxCheckpointFact = "session_checkpoint_fact"
	OutboxContent        = "session_content"
	OutboxTombstone      = "tombstone" // team item 5: memory and session-content deletions
)

// ContentOptInAll is the opt-in row an adopted organization bundle's content mandate
// writes (D-12's second path): every session, under consent "mandated-by-bundle".
const ContentOptInAll = "*"

// outboxTx is what an enqueue site holds: a *sql.Tx or a *GovTx.
type outboxTx interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// enqueueOutbox writes one outbox row when — and only when — the device is linked. at is
// Unix seconds (every kind in one unit, so the backlog's oldest age is one comparison).
func enqueueOutbox(tx outboxTx, kind, globalID, contentHash, scope string, at int64) error {
	return enqueueOutboxRow(tx, outboxInsert{kind: kind, globalID: globalID, contentHash: contentHash, scope: scope, at: at})
}

// outboxInsert is one row to enqueue. revision names the memory revision a memory row
// carries (team item 5 decision 1); a tombstone row's body is known at deletion and is
// frozen at enqueue (sentWireBody / sentWireHash), since its source is gone by then.
type outboxInsert struct {
	kind, globalID, contentHash, scope string
	at, revision                       int64
	sentWireBody, sentWireHash         string
}

func enqueueOutboxRow(tx outboxTx, in outboxInsert) error {
	linked, err := deviceLinked(tx)
	if err != nil || !linked {
		return err
	}
	_, err = tx.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at,revision,sent_wire_body,sent_wire_hash) VALUES(?,?,?,?,?,?,?,?)`,
		in.kind, in.globalID, in.contentHash, in.scope, in.at, in.revision, in.sentWireBody, in.sentWireHash)
	return err
}

// deviceLinked reports whether this device is linked to a team (no device row: no).
func deviceLinked(tx outboxTx) (bool, error) {
	var linked int
	if err := tx.QueryRow(`SELECT linked FROM sync_device LIMIT 1`).Scan(&linked); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return linked == 1, nil
}

// CheckpointFactGlobalID is a checkpoint's wire id: stable per (device, checkpoint), so a
// re-push dedupes.
func CheckpointFactGlobalID(deviceID string, checkpointID int64) string {
	return engine.DeterministicTypedID("ckp", deviceID+":"+strconv.FormatInt(checkpointID, 10))
}

// enqueueCheckpointFact enqueues a checkpoint that reached a terminal state. The fact
// (identity, class, base/head, digests — never a body) is built at drain time from the
// row as it then stands; a checkpoint re-claimed after "unavailable" enqueues again, and
// the server keeps the newest (by ended_at). stateDigest is recorded for diagnosis only.
func enqueueCheckpointFact(tx outboxTx, checkpointID int64, sessionID, stateDigest string, at int64) error {
	var deviceID string
	if err := tx.QueryRow(`SELECT id FROM sync_device LIMIT 1`).Scan(&deviceID); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	return enqueueOutbox(tx, OutboxCheckpointFact, CheckpointFactGlobalID(deviceID, checkpointID), stateDigest, sessionID, UnixSeconds(at))
}

// UnixSeconds accepts a timestamp in seconds or nanoseconds (checkpoint times are
// written by several callers) and returns seconds.
func UnixSeconds(t int64) int64 {
	if t > nanosecondFloor {
		return t / 1_000_000_000
	}
	return t
}

// OutboxRow is one pending outbox entry.
type OutboxRow struct {
	Seq         int64
	Kind        string
	GlobalID    string
	ContentHash string
	Scope       string
	EnqueuedAt  int64 // Unix seconds
	Attempts    int
	// Team item 5: the memory revision a memory row carries (0 = a pre-44 row, never
	// encoded), and the wire body frozen at first send so a retry is byte-identical.
	Revision     int64
	SentWireBody string
	SentWireHash string
}

// nanosecondFloor tells a Unix-nanosecond timestamp from a Unix-second one: seconds reach
// it only in the year 5138, nanoseconds pass it within two minutes of 1970.
const nanosecondFloor = 100_000_000_000

// outboxSeconds normalizes enqueued_at: memory rows written before item 4 stored Unix
// nanoseconds; every row since stores seconds.
var outboxSeconds = fmt.Sprintf(`CASE WHEN enqueued_at > %d THEN enqueued_at / 1000000000 ELSE enqueued_at END`, nanosecondFloor)

// outboxSelect is the drain's row read, enqueued_at normalized to seconds.
var outboxSelect = `seq, record_kind, global_id, content_hash, scope, ` + outboxSeconds + `, attempts, revision, sent_wire_body, sent_wire_hash`

// outboxPriorityFirst orders the drain's read: handoff documents and receipts leave
// AHEAD of every other kind, oldest first among themselves, so a handoff never waits
// behind an event backlog (team rest-of-release plan §6.6; criterion 62). Every other
// kind keeps its sequence order.
var outboxPriorityFirst = `CASE WHEN record_kind IN ('` + OutboxHandoff + `','` + OutboxHandoffReceipt + `') THEN 0 ELSE 1 END, seq`

// OutboxBatch returns up to limit unacknowledged rows — handoff rows first, then oldest
// first — leaving out the
// kinds in skip — the drain's parked kinds, which must never occupy the batch head. A
// record's memory and tombstone rows are in flight ONE at a time (team item 5 decision
// 13b): only the lowest-sequence unacked row per global id is selected, in the query,
// so a record's later revisions never take batch slots while its head is unanswered. A
// memory row naming no revision (the pre-44 backlog) is never sent — the drain refuses it
// locally — so it is exempt: each is refused the first time the drain reads it (C-11).
func (ix *Index) OutboxBatch(limit int, skip []string) ([]OutboxRow, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("outbox batch limit must be positive")
	}
	q := `SELECT ` + outboxSelect + `
		FROM sync_outbox o WHERE acked_at IS NULL
		AND (record_kind NOT IN ('` + OutboxMemory + `','` + OutboxTombstone + `') OR (record_kind = '` + OutboxMemory + `' AND revision = 0)
			OR seq = (SELECT MIN(p.seq) FROM sync_outbox p
			WHERE p.acked_at IS NULL AND p.global_id = o.global_id AND p.record_kind IN ('` + OutboxMemory + `','` + OutboxTombstone + `')))`
	args := []any{}
	if len(skip) > 0 {
		q += ` AND record_kind NOT IN (?` + strings.Repeat(`,?`, len(skip)-1) + `)`
		for _, k := range skip {
			args = append(args, k)
		}
	}
	q += ` ORDER BY ` + outboxPriorityFirst + ` LIMIT ?`
	args = append(args, limit)
	return ix.outboxRows(q, args...)
}

// OutboxOldestOfKind returns the oldest unacknowledged row of one kind — the probe a
// parked kind is re-tried with.
func (ix *Index) OutboxOldestOfKind(kind string) (OutboxRow, bool, error) {
	rows, err := ix.outboxRows(`SELECT `+outboxSelect+`
		FROM sync_outbox WHERE acked_at IS NULL AND record_kind = ? ORDER BY seq LIMIT 1`, kind)
	if err != nil || len(rows) == 0 {
		return OutboxRow{}, false, err
	}
	return rows[0], true, nil
}

func (ix *Index) outboxRows(q string, args ...any) ([]OutboxRow, error) {
	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.Seq, &r.Kind, &r.GlobalID, &r.ContentHash, &r.Scope, &r.EnqueuedAt, &r.Attempts,
			&r.Revision, &r.SentWireBody, &r.SentWireHash); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OutboxAttempted counts one push attempt against each row.
func (ix *Index) OutboxAttempted(seqs []int64) error {
	return ix.outboxUpdate(`UPDATE sync_outbox SET attempts = attempts + 1 WHERE seq IN (%s)`, seqs)
}

// OutboxAck acknowledges rows: the server answered them terminally. Idempotent.
func (ix *Index) OutboxAck(seqs []int64, at int64) error {
	return ix.outboxUpdate(`UPDATE sync_outbox SET acked_at = `+strconv.FormatInt(at, 10)+` WHERE acked_at IS NULL AND seq IN (%s)`, seqs)
}

// OutboxAckEntry is one acknowledgement with the answer it was acknowledged with
// (team item 5, C-3): accepted, duplicate, or the refusal code. SentBodyHash is a
// content row's redacted body hash as sent, recorded with its accepting answer so a
// later "delete what was sent" can commit to exactly what the server holds.
type OutboxAckEntry struct {
	Seq          int64
	Code         string
	SentBodyHash string
}

// OutboxAckEntries acknowledges rows with their answers in one transaction. A row already
// acknowledged keeps its first answer. Idempotent.
func (ix *Index) OutboxAckEntries(entries []OutboxAckEntry, at int64) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range entries {
		if _, err := tx.Exec(`UPDATE sync_outbox SET acked_at = ?, ack_code = ?, sent_body_hash = CASE WHEN ? != '' THEN ? ELSE sent_body_hash END
			WHERE seq = ? AND acked_at IS NULL`, at, e.Code, e.SentBodyHash, e.SentBodyHash, e.Seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (ix *Index) outboxUpdate(q string, seqs []int64) error {
	if len(seqs) == 0 {
		return nil
	}
	args := make([]any, len(seqs))
	for i, s := range seqs {
		args[i] = s
	}
	_, err := ix.db.Exec(fmt.Sprintf(q, "?"+strings.Repeat(",?", len(seqs)-1)), args...)
	return err
}

// OutboxSummary is the backlog as the developer's console states it: pending rows per
// kind and the oldest pending row's enqueue time (Unix seconds; 0 when empty).
type OutboxSummary struct {
	Pending  int            `json:"pending"`
	ByKind   map[string]int `json:"by_kind"`
	OldestAt int64          `json:"oldest_at,omitempty"`
}

// OutboxPendingSummary reads the backlog.
func (ix *Index) OutboxPendingSummary() (OutboxSummary, error) {
	s := OutboxSummary{ByKind: map[string]int{}}
	rows, err := ix.db.Query(`SELECT record_kind, count(*), MIN(` + outboxSeconds + `) FROM sync_outbox WHERE acked_at IS NULL GROUP BY record_kind`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		var oldest int64
		if err := rows.Scan(&kind, &n, &oldest); err != nil {
			return s, err
		}
		s.ByKind[kind] = n
		s.Pending += n
		if s.OldestAt == 0 || oldest < s.OldestAt {
			s.OldestAt = oldest
		}
	}
	return s, rows.Err()
}

// EventWireInputsByGlobalID reads the named events as encoder input, each with its
// session's identities and repository — the drain's per-row read. Ids with no event
// (impossible while the outbox and the event commit together) are absent from the map.
func (ix *Index) EventWireInputsByGlobalID(ids []string) (map[string]engine.WireEventInput, error) {
	out := map[string]engine.WireEventInput{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := ix.db.Query(`SELECT `+wireEventCols+` FROM event e LEFT JOIN event_delivery d ON d.event_id = e.id
		WHERE e.global_id IN (?`+strings.Repeat(`,?`, len(ids)-1)+`) ORDER BY e.id`, args...)
	if err != nil {
		return nil, err
	}
	var ins []engine.WireEventInput
	for rows.Next() {
		in, err := scanWireEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ins = append(ins, in)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	deviceID, _, err := ix.Device()
	if err != nil {
		return nil, err
	}
	sessions := map[string]wireSessionFacts{}
	for _, in := range ins {
		f, ok := sessions[in.Session]
		if !ok {
			if f, err = ix.wireSessionFacts(in.Session); err != nil {
				return nil, err
			}
			sessions[in.Session] = f
		}
		in.DeviceID, in.CatalogID, in.ResumeID, in.RepositoryID, in.CheckoutRoot = deviceID, f.catalogID, f.resumeID, f.repositoryID, f.checkoutRoot
		out[in.GlobalID] = in
	}
	return out, nil
}

const wireEventCols = `e.global_id, e.ts, COALESCE(d.received_at,0), e.session_id, e.runtime,
		COALESCE(e.verb,''), COALESCE(e.tool,''), COALESCE(e.target_entity_id,''), COALESCE(e.tags,''),
		COALESCE(e.decision,''), COALESCE(e.reason,''), COALESCE(e.origin,''),
		COALESCE(e.chain_seq,0), COALESCE(e.prev_hash,''), COALESCE(e.hash,''), e.rule_id, COALESCE(e.layer,'')`

func scanWireEvent(rows *sql.Rows) (engine.WireEventInput, error) {
	var in engine.WireEventInput
	err := rows.Scan(&in.GlobalID, &in.TS, &in.ReceivedAt, &in.Session, &in.Runtime, &in.Verb, &in.Tool,
		&in.TargetEntityID, &in.FrozenTags, &in.Decision, &in.Reason, &in.Origin, &in.ChainSeq, &in.PrevHash, &in.Hash, &in.Rule, &in.Layer)
	return in, err
}

type wireSessionFacts struct{ catalogID, resumeID, repositoryID, checkoutRoot string }

// wireSessionFacts resolves a session's other two identities and its repository.
func (ix *Index) wireSessionFacts(sessionID string) (wireSessionFacts, error) {
	var f wireSessionFacts
	var err error
	if f.repositoryID, f.checkoutRoot, _, err = ix.SessionRepository(sessionID); err != nil {
		return f, err
	}
	native := sessionID
	if i := strings.Index(sessionID, "/"); i > 0 {
		native = sessionID[i+1:]
	}
	if err := ix.db.QueryRow(`SELECT catalog_id, resume_id FROM sessions WHERE id IN (?, ?) LIMIT 1`, native, sessionID).Scan(&f.catalogID, &f.resumeID); err != nil && err != sql.ErrNoRows {
		return f, err
	}
	return f, nil
}

// SessionEventSpan counts a session's events and returns the first and last event times
// (Unix seconds; zero when the session has none).
func (ix *Index) SessionEventSpan(sessionID string) (count int, first, last int64, err error) {
	err = ix.db.QueryRow(`SELECT count(*), COALESCE(MIN(ts),0), COALESCE(MAX(ts),0) FROM event WHERE session_id = ?`, sessionID).Scan(&count, &first, &last)
	return count, first, last, err
}

// CheckpointForFact finds the terminal checkpoint whose wire id is globalID within a
// session (the wire id is a one-way derivation, so the drain matches it). ok is false
// when none matches.
func (ix *Index) CheckpointForFact(deviceID, sessionID, globalID string) (SessionCheckpoint, bool, error) {
	rows, err := ix.db.Query(`SELECT id FROM session_checkpoint WHERE session_id = ? AND status IN ('complete','failed','unavailable') ORDER BY id DESC`, sessionID)
	if err != nil {
		return SessionCheckpoint{}, false, err
	}
	var match int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return SessionCheckpoint{}, false, err
		}
		if CheckpointFactGlobalID(deviceID, id) == globalID {
			match = id
			break
		}
	}
	if err := rows.Close(); err != nil || match == 0 {
		return SessionCheckpoint{}, false, err
	}
	c, err := ix.SessionCheckpointByID(match)
	return c, err == nil, err
}

// ContentSessionKey is the one spelling of a session in sync_content_optin: the wire's
// "<runtime>/<native id>" (engine.WireSessionParts), whichever form the event stored.
func ContentSessionKey(runtimeColumn, session string) string {
	runtime, native := engine.WireSessionParts(runtimeColumn, session)
	return runtime + "/" + native
}

// ContentGlobalID is an input's content-chunk wire id: stable per event, so a re-push
// dedupes.
func ContentGlobalID(eventGlobalID string) string {
	return engine.DeterministicTypedID("cnt", eventGlobalID)
}

// enqueueContent enqueues an event's captured input as content — only while linked and
// only when the session is opted in (or an organization mandate is recorded), in the
// same transaction as the input. The drain re-checks consent before anything leaves.
func enqueueContent(tx outboxTx, eventID int64, digest string) error {
	var linked int
	if err := tx.QueryRow(`SELECT linked FROM sync_device LIMIT 1`).Scan(&linked); err != nil || linked != 1 {
		if err == sql.ErrNoRows {
			return nil
		}
		return err // nil when unlinked: the hot path pays one query, not three
	}
	var gid, session, runtime string
	var ts int64
	if err := tx.QueryRow(`SELECT global_id, session_id, runtime, ts FROM event WHERE id = ?`, eventID).Scan(&gid, &session, &runtime, &ts); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM sync_content_optin WHERE session_id IN (?, ?)`, ContentSessionKey(runtime, session), ContentOptInAll).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if digest == "" {
		digest = gid
	}
	return enqueueOutbox(tx, OutboxContent, ContentGlobalID(gid), digest, session, ts)
}

// SetContentOptIn records (on) or removes (off) a session's content consent, keyed by
// ContentSessionKey or ContentOptInAll. Forward-only: turning it on enqueues nothing
// already captured (sync.content_backfill is false, §5.4).
func (ix *Index) SetContentOptIn(key string, on bool, at int64) error {
	if on {
		_, err := ix.db.Exec(`INSERT INTO sync_content_optin(session_id, since) VALUES(?, ?) ON CONFLICT(session_id) DO NOTHING`, key, at)
		return err
	}
	_, err := ix.db.Exec(`DELETE FROM sync_content_optin WHERE session_id = ?`, key)
	return err
}

// ContentOptIns lists the recorded consents: key → since (Unix seconds).
func (ix *Index) ContentOptIns() (map[string]int64, error) {
	rows, err := ix.db.Query(`SELECT session_id, since FROM sync_content_optin`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var at int64
		if err := rows.Scan(&k, &at); err != nil {
			return nil, err
		}
		out[k] = at
	}
	return out, rows.Err()
}

// ContentInput is one event's captured input as the drain reads it for a content chunk.
type ContentInput struct {
	EventGlobalID string
	Session       string
	Runtime       string
	MediaType     string
	Payload       []byte
}

// ContentInputByChunk reads the input a content row names (its wire id is derived from
// the event's, so the session's inputs are matched). ok is false when none remains.
func (ix *Index) ContentInputByChunk(sessionID, chunkID string) (ContentInput, bool, error) {
	rows, err := ix.db.Query(`SELECT e.id, e.global_id FROM event e JOIN event_input i ON i.event_id = e.id
		WHERE e.session_id = ? AND i.payload IS NOT NULL ORDER BY e.id DESC`, sessionID)
	if err != nil {
		return ContentInput{}, false, err
	}
	var match int64
	for rows.Next() {
		var id int64
		var gid string
		if err := rows.Scan(&id, &gid); err != nil {
			rows.Close()
			return ContentInput{}, false, err
		}
		if ContentGlobalID(gid) == chunkID {
			match = id
			break
		}
	}
	if err := rows.Close(); err != nil || match == 0 {
		return ContentInput{}, false, err
	}
	var c ContentInput
	err = ix.db.QueryRow(`SELECT e.global_id, e.session_id, e.runtime, i.media_type, i.payload FROM event e JOIN event_input i ON i.event_id = e.id WHERE e.id = ?`, match).
		Scan(&c.EventGlobalID, &c.Session, &c.Runtime, &c.MediaType, &c.Payload)
	return c, err == nil, err
}

// ResetSyncQueue ends a link's sync state: pending outbox rows are dropped (never sent to
// a later link), every content consent — the mandate row included — is removed, the
// memory pull cursor is forgotten, and every memory record's team sync state is cleared
// and its sharing withdrawn (team item 5): a later link — perhaps to another
// organization — starts from nothing and shares only what is shared again. Pulled
// records stay on the device (an unlink deletes nothing local). Acknowledged rows stay
// as history, marked as this ended link's. The link's handoff rows are marked as an ended
// link's in the same transaction (endHandoffLinkTx: rows stay readable, the handoff pull
// cursor and the member directory are forgotten, waiting open tickets are cancelled).
// Called by unlink, after enqueueing has stopped.
func (ix *Index) ResetSyncQueue() error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	org, err := linkedOrganizationTx(tx)
	if err != nil {
		return err
	}
	// Every acknowledgement this link made is marked as an earlier link's (CR-1): what a
	// previous server accepted is not what the next one holds, so it must stop counting
	// for "delete what was sent", for the deletions whose reach is asked, and for the
	// refusal counts. A relink to the same organization takes its own back
	// (SetLinkedOrganization).
	if _, err := tx.Exec(`UPDATE sync_outbox SET ack_code = ? || ack_code WHERE acked_at IS NOT NULL AND ack_code != '' AND ack_code NOT LIKE 'prior:%'`,
		priorLinkPrefix(org)); err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM sync_outbox WHERE acked_at IS NULL`, `DELETE FROM sync_content_optin`,
		`DELETE FROM sync_cursor WHERE scope LIKE 'memory%' OR scope = '` + linkScope + `'`,
		`UPDATE memory_record SET share_state='unshared', pushed_hash='', server_revision=0, synced_projection_hash='',
			held_wire_record='', held_server_revision=0`,
		`UPDATE sync_outbox SET sent_wire_body='' WHERE record_kind='memory'`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if err := ix.endHandoffLinkTx(tx, org); err != nil {
		return err
	}
	if err := bumpPullEpoch(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ContentSessionKeyFor resolves a session as the console names it (runtime + the id it
// shows) to the key content consent is recorded under — from the session's own stored
// events, so a consent can never be keyed to something the enqueue will not match.
// ok is false when the store holds no event for that session.
func (ix *Index) ContentSessionKeyFor(runtime, id string) (string, bool, error) {
	var session, rt string
	err := ix.db.QueryRow(`SELECT session_id, runtime FROM event WHERE session_id IN (?, ?) AND (? = '' OR runtime = ? OR session_id = ?)
		ORDER BY id DESC LIMIT 1`, id, runtime+"/"+id, runtime, runtime, runtime+"/"+id).Scan(&session, &rt)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return ContentSessionKey(rt, session), true, nil
}
