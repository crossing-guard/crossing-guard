package store

// Schema 39 (orchestration-flows pilot, plan §2): the append-only owner-tag
// change journal and the flow tables. The journal exists because the mutable
// session_owner_tag rows cannot be observed as changes: apply is an upsert
// refresh, retract an in-place UPDATE, purge a DELETE, rename a rewrite
// (pass-1 RT-2). Every one of those writes now also appends one journal row
// in the SAME transaction, so a position-anchored translator can observe
// applies, removals, renames (removal of the old key + apply of the new) and
// purges. The journal records the tag VALUE the owner wrote — user content,
// the same boundary session_owner_tag documents: nothing here feeds
// governance or agent context; the flow machinery consumes membership facts,
// and the boundary test proves tag values never reach agents.
//
// The flow tables hold only durable MECHANISM state (plan §5 invariant 9):
// flow records with their configuration digest, stage membership snapshots,
// flow-scoped bindings carrying an ownership discriminator (pass-2 B/RT-5),
// folded-state delivery-ceiling counters, and dry-run receipts. Every
// workflow opinion — stages, membership predicates, ceiling numbers,
// reply-class shapes — lives in flows.json/profile data; nothing is compiled
// here.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Flow member exclusion classes (schema 41, flow-member-exclusion-class
// plan D-2). The member table's CHECK is generated from this one list, so a
// class the daemon writes is always a class the store accepts; "" means the
// member is not excluded. Schema 39 shipped a CHECK that forgot no-stage
// and carried an orphan no-root nothing ever wrote (owner D-1: dropped).
const (
	FlowMemberExcludedSharedCheckout = "shared-checkout"
	FlowMemberExcludedTaskOwned      = "task-owned"
	FlowMemberExcludedNoStage        = "no-stage"
)

var flowMemberExclusionClasses = []string{"", FlowMemberExcludedSharedCheckout,
	FlowMemberExcludedTaskOwned, FlowMemberExcludedNoStage}

// FlowMemberExclusionClasses returns a copy of the ordered class vocabulary
// the member table accepts.
func FlowMemberExclusionClasses() []string {
	return append([]string(nil), flowMemberExclusionClasses...)
}

// flowMemberClassSQLList is the vocabulary as a parenthesised SQL list of
// quoted strings. The classes are constants matching [a-z-]*, pinned by
// test, so no quoting beyond the wrapping quotes is needed.
var flowMemberClassSQLList = func() string {
	quoted := make([]string, len(flowMemberExclusionClasses))
	for index, class := range flowMemberExclusionClasses {
		quoted[index] = "'" + class + "'"
	}
	return "(" + strings.Join(quoted, ",") + ")"
}()

// flowMemberCheckClause is the generated CHECK the v41 probe looks for.
var flowMemberCheckClause = "CHECK(excluded IN " + flowMemberClassSQLList + ")"

// flowMemberColumns is the member table's column list in DDL order; the v41
// rebuild copies exactly these, and a test pins it to the live table.
const flowMemberColumns = "flow_id,runtime,session_id,stage,excluded,exclusion_reason,bound,admitted_at,updated_at"

// flowMemberTableBody is the member table after its name. Fresh stores
// (CREATE TABLE IF NOT EXISTS) and the v41 rebuild (CREATE TABLE) share it,
// so both leave byte-identical DDL in sqlite_master.
var flowMemberTableBody = `(
  flow_id TEXT NOT NULL REFERENCES orchestration_flow(flow_id) ON DELETE CASCADE,
  runtime TEXT NOT NULL,
  session_id TEXT NOT NULL,
  stage TEXT NOT NULL,
  excluded TEXT NOT NULL DEFAULT '' ` + flowMemberCheckClause + `,
  exclusion_reason TEXT NOT NULL DEFAULT '',
  bound INTEGER NOT NULL DEFAULT 0 CHECK(bound IN (0,1)),
  admitted_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(flow_id,runtime,session_id)
)`

// flowSchemaV39 creates the flow tables on a fresh store. Its member table
// is the v41 layout (generated CHECK); a store created at v39 is rebuilt by
// migrateFlowMemberExclusionV41.
var flowSchemaV39 = `
CREATE TABLE IF NOT EXISTS session_owner_tag_change(
  change_id INTEGER PRIMARY KEY AUTOINCREMENT,
  runtime TEXT NOT NULL CHECK(length(runtime) > 0),
  session_id TEXT NOT NULL CHECK(length(session_id) > 0),
  change TEXT NOT NULL CHECK(change IN ('applied','removed')),
  key TEXT NOT NULL DEFAULT '',
  value TEXT NOT NULL,
  key_fold TEXT NOT NULL DEFAULT '',
  value_fold TEXT NOT NULL,
  session_title TEXT NOT NULL DEFAULT '',
  session_repository TEXT NOT NULL DEFAULT '',
  session_cwd TEXT NOT NULL DEFAULT '',
  session_touched_at INTEGER NOT NULL DEFAULT 0,
  changed_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS session_owner_tag_change_order
  ON session_owner_tag_change(changed_at,change_id);
CREATE INDEX IF NOT EXISTS session_owner_tag_change_target
  ON session_owner_tag_change(runtime,session_id,change_id);

CREATE TABLE IF NOT EXISTS orchestration_flow(
  flow_id TEXT PRIMARY KEY,
  state TEXT NOT NULL CHECK(state IN ('enabled','disabled')),
  config_json TEXT NOT NULL CHECK(json_valid(config_json)),
  config_digest TEXT NOT NULL,
  journal_position INTEGER NOT NULL DEFAULT 0,
  evaluation_state TEXT NOT NULL DEFAULT '' CHECK(evaluation_state IN ('','pending','done')),
  enabled_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS orchestration_flow_member` + flowMemberTableBody + `;

CREATE TABLE IF NOT EXISTS orchestration_flow_binding(
  binding_id TEXT NOT NULL,
  flow_id TEXT NOT NULL REFERENCES orchestration_flow(flow_id) ON DELETE CASCADE,
  stage TEXT NOT NULL,
  member_runtime TEXT NOT NULL,
  member_session_id TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  inert_reason TEXT NOT NULL DEFAULT '',
  armed INTEGER NOT NULL DEFAULT 0 CHECK(armed IN (0,1)),
  created_at INTEGER NOT NULL,
  released_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(binding_id)
);
CREATE INDEX IF NOT EXISTS orchestration_flow_binding_member
  ON orchestration_flow_binding(flow_id,stage,member_runtime,member_session_id) WHERE released_at=0;

CREATE TABLE IF NOT EXISTS orchestration_flow_ceiling(
  flow_id TEXT NOT NULL REFERENCES orchestration_flow(flow_id) ON DELETE CASCADE,
  member_runtime TEXT NOT NULL,
  member_session_id TEXT NOT NULL,
  deliveries INTEGER NOT NULL DEFAULT 0,
  first_delivery_at INTEGER NOT NULL DEFAULT 0,
  last_reply_digest TEXT NOT NULL DEFAULT '',
  last_delivery_at INTEGER NOT NULL DEFAULT 0,
  breached INTEGER NOT NULL DEFAULT 0 CHECK(breached IN (0,1)),
  breached_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(flow_id,member_runtime,member_session_id)
);

CREATE TABLE IF NOT EXISTS orchestration_flow_dry_run(
  dry_run_id INTEGER PRIMARY KEY AUTOINCREMENT,
  flow_id TEXT NOT NULL REFERENCES orchestration_flow(flow_id) ON DELETE CASCADE,
  runtime TEXT NOT NULL,
  session_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('delivery','transition','binding')),
  detail_json TEXT NOT NULL CHECK(json_valid(detail_json)),
  recorded_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS orchestration_flow_dry_run_flow
  ON orchestration_flow_dry_run(flow_id,recorded_at);
`

func migrateFlowsV39(db schemaDB) error {
	if _, err := db.Exec(flowSchemaV39); err != nil {
		return fmt.Errorf("migrate flows v39: %w", err)
	}
	return nil
}

// migrateFlowMemberExclusionV41 rebuilds a v39 member table whose CHECK
// forbade the no-stage class the daemon writes (SQLite cannot alter a CHECK
// in place). It probes the stored DDL for the generated clause rather than
// the version — the repair discipline — and uses Contains, not equality, so
// a later additive change to this table never re-triggers the rebuild. A row
// in a class outside the vocabulary refuses the migration (plan D-3): there
// is no truthful class to move it to, and the whole open rolls back.
func migrateFlowMemberExclusionV41(db schemaDB) error {
	var stored string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='orchestration_flow_member'`).
		Scan(&stored); err != nil {
		return fmt.Errorf("migrate flow members v41: read table: %w", err)
	}
	if strings.Contains(stored, flowMemberCheckClause) {
		return nil
	}
	retired, total, err := flowMemberRetiredClasses(db)
	if err != nil {
		return fmt.Errorf("migrate flow members v41: inspect classes: %w", err)
	}
	if total > 0 {
		return fmt.Errorf("migrate flow members v41: %d member rows carry a retired exclusion class (%s); "+
			"stop the daemon, run DELETE FROM orchestration_flow_member WHERE excluded NOT IN %s, start it, "+
			"then remove and re-apply each affected session's membership tag",
			total, strings.Join(retired, ", "), flowMemberClassSQLList)
	}
	if _, err := db.Exec(`ALTER TABLE orchestration_flow_member RENAME TO orchestration_flow_member_v39;
CREATE TABLE orchestration_flow_member` + flowMemberTableBody + `;
INSERT INTO orchestration_flow_member(` + flowMemberColumns + `)
SELECT ` + flowMemberColumns + ` FROM orchestration_flow_member_v39;
DROP TABLE orchestration_flow_member_v39;`); err != nil {
		return fmt.Errorf("migrate flow members v41: rebuild: %w", err)
	}
	return nil
}

// flowMemberRetiredClasses counts member rows whose class is outside the
// vocabulary, as "class"×count descriptions.
func flowMemberRetiredClasses(db schemaDB) ([]string, int, error) {
	rows, err := db.Query(`SELECT excluded,count(*) FROM orchestration_flow_member WHERE excluded NOT IN ` +
		flowMemberClassSQLList + ` GROUP BY excluded ORDER BY excluded`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var retired []string
	total := 0
	for rows.Next() {
		var class string
		var count int
		if err := rows.Scan(&class, &count); err != nil {
			return nil, 0, err
		}
		retired = append(retired, fmt.Sprintf("%q×%d", class, count))
		total += count
	}
	return retired, total, rows.Err()
}

// SessionOwnerTagChange is one journal row: the owner's tag change as an
// ordered, append-only fact. Change is "applied" or "removed".
type SessionOwnerTagChange struct {
	ChangeID   int64
	Runtime    string
	SessionID  string
	Change     string
	Key        string
	Value      string
	Title      string
	Repository string
	Cwd        string
	TouchedAt  int64
	ChangedAt  int64
}

// appendOwnerTagChange writes one journal row inside the caller's
// transaction. It is the only journal writer; every session_owner_tag write
// path calls it, so the journal is complete by construction.
func appendOwnerTagChange(tx *sql.Tx, target SessionOwnerTarget, tag SessionOwnerTagValue, change string, at int64) error {
	_, err := tx.Exec(`INSERT INTO session_owner_tag_change(
		runtime,session_id,change,key,value,key_fold,value_fold,
		session_title,session_repository,session_cwd,session_touched_at,changed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		target.Runtime, target.SessionID, change, tag.Key, tag.Value,
		FoldSessionOwnerTagPart(tag.Key), FoldSessionOwnerTagPart(tag.Value),
		target.Title, target.Repository, target.Cwd, target.TouchedAt, at)
	return err
}

// SessionOwnerTagChangesAfter reads journal rows strictly after the position,
// bounded, oldest first. The bool reports the bound cut the read short.
func (ix *Index) SessionOwnerTagChangesAfter(position int64, limit int) ([]SessionOwnerTagChange, bool, error) {
	rows, err := ix.db.Query(`SELECT change_id,runtime,session_id,change,key,value,
		session_title,session_repository,session_cwd,session_touched_at,changed_at
		FROM session_owner_tag_change WHERE change_id>? ORDER BY change_id LIMIT ?`,
		position, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []SessionOwnerTagChange
	for rows.Next() {
		var row SessionOwnerTagChange
		if err := rows.Scan(&row.ChangeID, &row.Runtime, &row.SessionID, &row.Change,
			&row.Key, &row.Value, &row.Title, &row.Repository, &row.Cwd,
			&row.TouchedAt, &row.ChangedAt); err != nil {
			return nil, false, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// SessionOwnerTagChangeHead is the journal's current head; a fresh stream
// position bootstraps here (history is not a signal).
func (ix *Index) SessionOwnerTagChangeHead() (int64, error) {
	var head int64
	err := ix.db.QueryRow(`SELECT COALESCE(MAX(change_id),0) FROM session_owner_tag_change`).Scan(&head)
	return head, err
}

// OrchestrationFlowRecord is one enabled flow's durable state. ConfigJSON is
// the flows.json entry as enabled (the digest names those exact bytes); the
// file stays the live configuration and the row only tracks mechanism
// state — evaluation position, enablement, updated time.
type OrchestrationFlowRecord struct {
	FlowID          string
	State           string
	ConfigDigest    string
	JournalPosition int64
	EvaluationState string
	EnabledAt       int64
	UpdatedAt       int64
}

// OrchestrationFlowRecords lists every flow row.
func (ix *Index) OrchestrationFlowRecords() ([]OrchestrationFlowRecord, error) {
	rows, err := ix.db.Query(`SELECT flow_id,state,config_digest,journal_position,evaluation_state,enabled_at,updated_at
		FROM orchestration_flow ORDER BY flow_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchestrationFlowRecord
	for rows.Next() {
		var record OrchestrationFlowRecord
		if err := rows.Scan(&record.FlowID, &record.State, &record.ConfigDigest, &record.JournalPosition,
			&record.EvaluationState, &record.EnabledAt, &record.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// EnableFlow creates or re-enables one flow row with a pending evaluation.
// The digest names the enabled configuration bytes; enablement resets the
// journal position so the evaluation owns what follows.
func (ix *Index) EnableFlow(flowID string, configJSON []byte, now int64) error {
	digest := flowConfigDigest(configJSON)
	_, err := ix.db.Exec(`INSERT INTO orchestration_flow(flow_id,state,config_json,config_digest,
		journal_position,evaluation_state,enabled_at,updated_at)
		VALUES(?,'enabled',?,?,0,'pending',?,?)
		ON CONFLICT(flow_id) DO UPDATE SET state='enabled',config_json=excluded.config_json,
		config_digest=excluded.config_digest,journal_position=0,evaluation_state='pending',
		enabled_at=excluded.enabled_at,updated_at=excluded.updated_at`,
		flowID, string(configJSON), digest, now, now)
	return err
}

// DisableFlow switches one flow row off; members and bindings keep their
// history (the stage-exit path releases bindings).
func (ix *Index) DisableFlow(flowID string, now int64) error {
	_, err := ix.db.Exec(`UPDATE orchestration_flow SET state='disabled',updated_at=? WHERE flow_id=?`, now, flowID)
	return err
}

// MarkFlowEvaluationDone completes the one-per-enablement evaluation guard.
func (ix *Index) MarkFlowEvaluationDone(flowID string, now int64) error {
	_, err := ix.db.Exec(`UPDATE orchestration_flow SET evaluation_state='done',updated_at=? WHERE flow_id=?`, now, flowID)
	return err
}

// FlowJournalPosition is the flow's own tag-journal position: the evaluation
// seeds it, live tag signals advance it, and a restart resumes from it
// (pass-2 C2's dedup-by-position).
func (ix *Index) FlowJournalPosition(flowID string) (int64, error) {
	var position int64
	err := ix.db.QueryRow(`SELECT journal_position FROM orchestration_flow WHERE flow_id=?`, flowID).Scan(&position)
	return position, err
}

// AdvanceFlowJournalPosition moves the flow's journal cursor forward; it
// only ever advances.
func (ix *Index) AdvanceFlowJournalPosition(flowID string, position int64, now int64) error {
	_, err := ix.db.Exec(`UPDATE orchestration_flow SET journal_position=MAX(journal_position,?),updated_at=? WHERE flow_id=?`,
		position, now, flowID)
	return err
}

func flowConfigDigest(configJSON []byte) string {
	sum := sha256.Sum256(configJSON)
	return hex.EncodeToString(sum[:16])
}

// OrchestrationFlowMember is one session's membership state in one flow.
type OrchestrationFlowMember struct {
	FlowID          string
	Runtime         string
	SessionID       string
	Stage           string
	Excluded        string
	ExclusionReason string
	Bound           bool
	AdmittedAt      int64
	UpdatedAt       int64
}

// ReplaceFlowMembers writes the initial evaluation's member set atomically:
// the whole set (with exclusion reasons) lands in one transaction.
func (ix *Index) ReplaceFlowMembers(flowID string, members []OrchestrationFlowMember, now int64) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM orchestration_flow_member WHERE flow_id=?`, flowID); err != nil {
		return err
	}
	for _, member := range members {
		if _, err := tx.Exec(`INSERT INTO orchestration_flow_member(flow_id,runtime,session_id,stage,excluded,
			exclusion_reason,bound,admitted_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			member.FlowID, member.Runtime, member.SessionID, member.Stage, member.Excluded,
			member.ExclusionReason, 0, member.AdmittedAt, member.UpdatedAt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE orchestration_flow SET updated_at=? WHERE flow_id=?`, now, flowID); err != nil {
		return err
	}
	return tx.Commit()
}

// FlowMembers lists one flow's members.
func (ix *Index) FlowMembers(flowID string) ([]OrchestrationFlowMember, error) {
	rows, err := ix.db.Query(`SELECT flow_id,runtime,session_id,stage,excluded,exclusion_reason,bound,admitted_at,updated_at
		FROM orchestration_flow_member WHERE flow_id=? ORDER BY admitted_at,runtime,session_id`, flowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchestrationFlowMember
	for rows.Next() {
		var member OrchestrationFlowMember
		if err := rows.Scan(&member.FlowID, &member.Runtime, &member.SessionID, &member.Stage,
			&member.Excluded, &member.ExclusionReason, &member.Bound, &member.AdmittedAt, &member.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, member)
	}
	return out, rows.Err()
}

// UpsertFlowMember folds one member's state (folded-state upsert, the
// store's discipline): a live tag signal's admission or exclusion replaces
// the same member row, never duplicates it (pass-2 C2 dedup).
func (ix *Index) UpsertFlowMember(member OrchestrationFlowMember, now int64) error {
	_, err := ix.db.Exec(`INSERT INTO orchestration_flow_member(flow_id,runtime,session_id,stage,excluded,
		exclusion_reason,bound,admitted_at,updated_at) VALUES(?,?,?,?,?,?,0,?,?)
		ON CONFLICT(flow_id,runtime,session_id) DO UPDATE SET stage=excluded.stage,
		excluded=excluded.excluded,exclusion_reason=excluded.exclusion_reason,updated_at=excluded.updated_at`,
		member.FlowID, member.Runtime, member.SessionID, member.Stage, member.Excluded,
		member.ExclusionReason, member.AdmittedAt, now)
	return err
}

// RemoveFlowMember is the membership exit: a member whose tag left the
// flow's grammar loses its row (the stage path releases its bindings
// first).
func (ix *Index) RemoveFlowMember(flowID, runtime, sessionID string, now int64) error {
	_, err := ix.db.Exec(`DELETE FROM orchestration_flow_member WHERE flow_id=? AND runtime=? AND session_id=?`,
		flowID, runtime, sessionID)
	if err == nil {
		_, err = ix.db.Exec(`UPDATE orchestration_flow SET updated_at=? WHERE flow_id=?`, now, flowID)
	}
	return err
}

// OrchestrationFlowBinding is one stage-scoped binding record. InertReason
// is "" for an armed binding; a non-empty reason records why the binding
// does not bound deliveries (reserved for future floors — postwork PO-7
// removed the pass-2 owner-place dedup: the flow never launches, so an
// owner's own place is the launch the flow bounds, never a conflict).
type OrchestrationFlowBinding struct {
	BindingID       string
	FlowID          string
	Stage           string
	MemberRuntime   string
	MemberSessionID string
	ProfileID       string
	InertReason     string
	Armed           bool
	CreatedAt       int64
	ReleasedAt      int64
}

// PutFlowBinding folds one stage binding into place (folded-state upsert):
// the same flow/stage/member/profile is one row, re-admission updates it.
func (ix *Index) PutFlowBinding(binding OrchestrationFlowBinding, inertReason string, now int64) error {
	if binding.BindingID == "" {
		binding.BindingID = newFlowBindingID()
	}
	_, err := ix.db.Exec(`INSERT INTO orchestration_flow_binding(binding_id,flow_id,stage,member_runtime,member_session_id,
		profile_id,inert_reason,armed,created_at,released_at) VALUES(?,?,?,?,?,?,?,?,?,0)
		ON CONFLICT(binding_id) DO UPDATE SET inert_reason=excluded.inert_reason,
		armed=excluded.armed,released_at=0`,
		binding.BindingID, binding.FlowID, binding.Stage, binding.MemberRuntime, binding.MemberSessionID,
		binding.ProfileID, inertReason, inertReason == "", now)
	return err
}

// ArmFlowBinding arms one binding: the ceiling row for the member is
// (re)initialized — deliveries zeroed, breach cleared — and the binding
// row is marked armed. The ceiling VALUES themselves (max deliveries,
// deadline, reply class) live in the flow configuration; delivery reads
// them through the enabled flow's config row, so the store never copies
// configuration into state (ADR 0026's identity rule).
func (ix *Index) ArmFlowBinding(flowID, stage, runtime, sessionID, profileID string, now int64) error {
	_, err := ix.db.Exec(`INSERT INTO orchestration_flow_ceiling(flow_id,member_runtime,member_session_id,
		deliveries,last_reply_digest,last_delivery_at,breached,breached_at,updated_at)
		VALUES(?,?,?,0,'',0,0,0,?)
		ON CONFLICT(flow_id,member_runtime,member_session_id) DO UPDATE SET
		breached=0,breached_at=0,updated_at=excluded.updated_at`,
		flowID, runtime, sessionID, now)
	if err != nil {
		return err
	}
	_, err = ix.db.Exec(`UPDATE orchestration_flow_binding SET armed=1 WHERE flow_id=? AND stage=? AND member_runtime=? AND member_session_id=? AND profile_id=? AND released_at=0`,
		flowID, stage, runtime, sessionID, profileID)
	return err
}

// ReleaseFlowBindings exits a stage: every live binding of this
// flow/stage/member is marked released (history kept; the routing scan
// ignores released rows).
func (ix *Index) ReleaseFlowBindings(flowID, stage, runtime, sessionID string, now int64) error {
	_, err := ix.db.Exec(`UPDATE orchestration_flow_binding SET released_at=?,armed=0
		WHERE flow_id=? AND stage=? AND member_runtime=? AND member_session_id=? AND released_at=0`,
		now, flowID, stage, runtime, sessionID)
	return err
}

// FlowBindings lists one member's live bindings in one flow (routing input).
func (ix *Index) FlowBindings(flowID, runtime, sessionID string) ([]OrchestrationFlowBinding, error) {
	rows, err := ix.db.Query(`SELECT binding_id,flow_id,stage,member_runtime,member_session_id,profile_id,
		inert_reason,armed,created_at,released_at FROM orchestration_flow_binding
		WHERE flow_id=? AND member_runtime=? AND member_session_id=? AND released_at=0 ORDER BY created_at`,
		flowID, runtime, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchestrationFlowBinding
	for rows.Next() {
		var binding OrchestrationFlowBinding
		if err := rows.Scan(&binding.BindingID, &binding.FlowID, &binding.Stage, &binding.MemberRuntime,
			&binding.MemberSessionID, &binding.ProfileID, &binding.InertReason, &binding.Armed,
			&binding.CreatedAt, &binding.ReleasedAt); err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

// cryptoRandRead is the journal's only use of randomness: flow binding ids.
func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }

func newFlowBindingID() string {
	var raw [12]byte
	if _, err := cryptoRandRead(raw[:]); err != nil {
		return "flwb_fallback"
	}
	return "flwb_" + hex.EncodeToString(raw[:])
}

// OrchestrationFlowCeiling is one member's folded-state delivery counter.
type OrchestrationFlowCeiling struct {
	FlowID          string
	Runtime         string
	SessionID       string
	Deliveries      int64
	FirstDeliveryAt int64
	LastReplyDigest string
	LastDeliveryAt  int64
	Breached        int64
	BreachedAt      int64
}

// FlowCeiling reads one member's counter row.
func (ix *Index) FlowCeiling(flowID, runtime, sessionID string) (OrchestrationFlowCeiling, bool, error) {
	var row OrchestrationFlowCeiling
	err := ix.db.QueryRow(`SELECT flow_id,member_runtime,member_session_id,deliveries,first_delivery_at,
		last_reply_digest,last_delivery_at,breached,breached_at FROM orchestration_flow_ceiling
		WHERE flow_id=? AND member_runtime=? AND member_session_id=?`,
		flowID, runtime, sessionID).Scan(&row.FlowID, &row.Runtime, &row.SessionID, &row.Deliveries,
		&row.FirstDeliveryAt, &row.LastReplyDigest, &row.LastDeliveryAt, &row.Breached, &row.BreachedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OrchestrationFlowCeiling{}, false, nil
	}
	return row, err == nil, err
}

// CountFlowDelivery folds one delivered reply into the durable counter
// (folded-state upsert: the row is the current count, never an append log).
func (ix *Index) CountFlowDelivery(flowID, runtime, sessionID, replyDigest string, now int64) error {
	_, err := ix.db.Exec(`UPDATE orchestration_flow_ceiling SET deliveries=deliveries+1,
		first_delivery_at=CASE WHEN first_delivery_at=0 THEN ? ELSE first_delivery_at END,
		last_reply_digest=?,last_delivery_at=?,updated_at=? WHERE flow_id=? AND member_runtime=? AND member_session_id=?`,
		now, replyDigest, now, now, flowID, runtime, sessionID)
	return err
}

// MarkFlowCeilingBreached sets the breach flag; only the owner's re-tag
// clears it (through ArmFlowBinding's reset).
func (ix *Index) MarkFlowCeilingBreached(flowID, runtime, sessionID string, now int64) error {
	_, err := ix.db.Exec(`UPDATE orchestration_flow_ceiling SET breached=1,breached_at=?,updated_at=?
		WHERE flow_id=? AND member_runtime=? AND member_session_id=?`, now, now, flowID, runtime, sessionID)
	return err
}

// RecordFlowDryRun appends one would-have-acted receipt (slice D).
func (ix *Index) RecordFlowDryRun(flowID, runtime, sessionID, kind string, detail map[string]any, now int64) error {
	body, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = ix.db.Exec(`INSERT INTO orchestration_flow_dry_run(flow_id,runtime,session_id,kind,detail_json,recorded_at)
		VALUES(?,?,?,?,?,?)`, flowID, runtime, sessionID, kind, body, now)
	return err
}
