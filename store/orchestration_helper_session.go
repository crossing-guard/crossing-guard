package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Schema 30 — helper session persistence (helper-persistent-session plan).
// The group is the pairing record between one binding and one source
// session; it now also owns the ONE vendor session the helper runs in and a
// single pending slot for a signal that arrived while a turn occupied it.

const managedGroupColumns = `group_id,binding_id,state,root_task_id,root_runtime,root_catalog_session_id,root_native_session_id,project_root,created_at,updated_at,` +
	`helper_runtime,helper_native_session_id,helper_turns,helper_session_replaced,helper_transcript_seq,pending_producer,pending_event_id,pending_coalesced,pending_at,pending_dropped,pending_dropped_reason`

func scanManagedGroup(row interface{ Scan(...any) error }) (ManagedGroup, error) {
	var out ManagedGroup
	err := row.Scan(&out.GroupID, &out.BindingID, &out.State, &out.RootTaskID, &out.RootRuntime, &out.RootCatalogSessionID, &out.RootNativeSessionID, &out.ProjectRoot, &out.CreatedAt, &out.UpdatedAt,
		&out.HelperRuntime, &out.HelperNativeSessionID, &out.HelperTurns, &out.HelperSessionReplaced, &out.HelperTranscriptSeq, &out.PendingProducer, &out.PendingEventID, &out.PendingCoalesced, &out.PendingAt, &out.PendingDropped, &out.PendingDroppedReason)
	return out, err
}

// ManagedPendingSignal is the durable shape of one coalesced signal: enough
// to relaunch it exactly (producer stream + event id qualify the run key) once
// the helper session frees. Task is the router's RuntimeTask projection,
// opaque to the store.
type ManagedPendingSignal struct {
	BindingID  string          `json:"binding_id"`
	Producer   string          `json:"producer"`
	EventID    int64           `json:"event_id"`
	Sequence   int64           `json:"sequence,omitempty"`
	Signal     string          `json:"signal"`
	Kind       string          `json:"kind,omitempty"`
	OccurredAt int64           `json:"occurred_at,omitempty"`
	Task       json.RawMessage `json:"task"`
	At         int64           `json:"at"`
}

// ManagedGroupForSource finds the newest active group for one binding and
// source session: by root task id first (the first console turn learns its
// session id only after task.started), then by either non-empty session
// identity. Group identity is a LOOKUP, never a hash of a task id
// (plan D1; red-team finding 4).
func (ix *Index) ManagedGroupForSource(bindingID, taskID, runtime, catalogSessionID, nativeSessionID string) (ManagedGroup, bool, error) {
	out, err := scanManagedGroup(ix.db.QueryRow(`SELECT `+managedGroupColumns+` FROM orchestration_group
		WHERE binding_id=? AND state='active' AND (
			root_task_id=?
			OR (?<>'' AND root_runtime=? AND (root_catalog_session_id=? OR root_native_session_id=?))
			OR (?<>'' AND root_runtime=? AND (root_catalog_session_id=? OR root_native_session_id=?)))
		ORDER BY created_at DESC, group_id DESC LIMIT 1`,
		bindingID, taskID,
		catalogSessionID, runtime, catalogSessionID, catalogSessionID,
		nativeSessionID, runtime, nativeSessionID, nativeSessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedGroup{}, false, nil
	}
	return out, err == nil, err
}

// SetGroupRootIdentity records a learned root session identity once: only
// empty columns are filled, a recorded identity is never overwritten
// (plan invariant 8).
func (ix *Index) SetGroupRootIdentity(groupID, catalogSessionID, nativeSessionID string, at int64) error {
	_, err := ix.db.Exec(`UPDATE orchestration_group SET
		root_catalog_session_id=CASE WHEN root_catalog_session_id='' THEN ? ELSE root_catalog_session_id END,
		root_native_session_id=CASE WHEN root_native_session_id='' THEN ? ELSE root_native_session_id END,
		updated_at=? WHERE group_id=?`, catalogSessionID, nativeSessionID, at, groupID)
	return err
}

// SetGroupHelperSession adopts a REPORTED helper session id. A different id
// than the recorded one counts as a replacement (vendor fork, or a fresh
// session after a lost one). turns/transcriptSeq advance monotonically.
func (ix *Index) SetGroupHelperSession(groupID, runtime, nativeSessionID string, transcriptSeq int64, completedTurn bool, at int64) error {
	turn := int64(0)
	if completedTurn {
		turn = 1
	}
	_, err := ix.db.Exec(`UPDATE orchestration_group SET
		helper_session_replaced=helper_session_replaced+CASE WHEN helper_native_session_id<>'' AND (helper_native_session_id<>? OR helper_runtime<>?) THEN 1 ELSE 0 END,
		helper_runtime=?, helper_native_session_id=?,
		helper_turns=helper_turns+?,
		helper_transcript_seq=MAX(helper_transcript_seq,?),
		updated_at=? WHERE group_id=?`, nativeSessionID, runtime, runtime, nativeSessionID, turn, transcriptSeq, at, groupID)
	return err
}

// ClearGroupHelperSession forgets a helper session the vendor could not
// resume so the next turn starts fresh; the replacement is counted.
func (ix *Index) ClearGroupHelperSession(groupID string, at int64) error {
	_, err := ix.db.Exec(`UPDATE orchestration_group SET
		helper_session_replaced=helper_session_replaced+CASE WHEN helper_native_session_id<>'' THEN 1 ELSE 0 END,
		helper_runtime='', helper_native_session_id='', updated_at=? WHERE group_id=?`, at, groupID)
	return err
}

// OccupyingManagedRuns counts the runs that hold the group's helper session:
// admitted, running, or parked (a parked turn still owns a relaunch).
func (ix *Index) OccupyingManagedRuns(groupID string) (int64, error) {
	var n int64
	err := ix.db.QueryRow(`SELECT COUNT(*) FROM orchestration_managed_run WHERE group_id=? AND state IN ('admitted','running','parked')`, groupID).Scan(&n)
	return n, err
}

// ManagedPendingTake is one drain read: the pending signal, the coalesced
// count it carries, and the token ClearGroupPendingSignal needs so a slot
// replaced after the read is never cleared by mistake.
type ManagedPendingTake struct {
	Signal    ManagedPendingSignal
	Coalesced int64
	Token     ManagedPendingToken
}

type ManagedPendingToken struct {
	Producer string
	EventID  int64
	At       int64
}

// TakeGroupPendingSignal reads the pending slot when — in the same
// transaction — no run occupies the group. The slot is left in place: the
// caller clears it with the token only after the drained admission landed,
// so a crash between take and launch replays on the next drain
// (at-least-once; admission is idempotent by run key).
func (ix *Index) TakeGroupPendingSignal(groupID string) (ManagedPendingTake, bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return ManagedPendingTake{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var occupying int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM orchestration_managed_run WHERE group_id=? AND state IN ('admitted','running','parked')`, groupID).Scan(&occupying); err != nil {
		return ManagedPendingTake{}, false, err
	}
	if occupying > 0 {
		return ManagedPendingTake{}, false, nil
	}
	var payload string
	var take ManagedPendingTake
	err = tx.QueryRow(`SELECT pending_signal_json,pending_producer,pending_event_id,pending_coalesced,pending_at FROM orchestration_group WHERE group_id=? AND pending_event_id<>0`, groupID).
		Scan(&payload, &take.Token.Producer, &take.Token.EventID, &take.Coalesced, &take.Token.At)
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedPendingTake{}, false, nil
	}
	if err != nil {
		return ManagedPendingTake{}, false, err
	}
	if err := json.Unmarshal([]byte(payload), &take.Signal); err != nil {
		return ManagedPendingTake{}, false, err
	}
	return take, true, tx.Commit()
}

// ClearGroupPendingSignal empties the slot only while it still holds the
// taken signal (token match); a signal that replaced it meanwhile survives.
func (ix *Index) ClearGroupPendingSignal(groupID string, token ManagedPendingToken, at int64) (bool, error) {
	result, err := ix.db.Exec(`UPDATE orchestration_group SET pending_signal_json='{}',pending_producer='',pending_event_id=0,pending_coalesced=0,pending_at=0,updated_at=?
		WHERE group_id=? AND pending_producer=? AND pending_event_id=? AND pending_at=?`, at, groupID, token.Producer, token.EventID, token.At)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// PendingManagedGroups lists groups holding a pending signal (the sweep's
// drain candidates; restart included).
func (ix *Index) PendingManagedGroups(limit int) ([]ManagedGroup, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT `+managedGroupColumns+` FROM orchestration_group WHERE pending_event_id<>0 ORDER BY pending_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedGroup{}
	for rows.Next() {
		item, err := scanManagedGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RunningManagedRuns lists runs in the running state (the sweep's liveness
// reconciliation reads their child task lifecycle).
func (ix *Index) RunningManagedRuns(limit int) ([]ManagedRun, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT `+managedRunColumns+` FROM orchestration_managed_run WHERE state='running' ORDER BY started_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedRun{}
	for rows.Next() {
		item, err := scanManagedRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ManagedGroupReplyCount counts recorded reply arcs in one group by query —
// a per-session group outgrows any relationship page (red-team finding 14).
func (ix *Index) ManagedGroupReplyCount(groupID string) (int, error) {
	var n int
	err := ix.db.QueryRow(`SELECT COUNT(*) FROM orchestration_relationship WHERE group_id=? AND reply_task_id<>''`, groupID).Scan(&n)
	return n, err
}

// AdoptHelperSessionForRun adopts the helper session one COMPLETED run ran in,
// exactly once per run: the run must be completed and not yet marked; the
// group update and the run's `helper_session_adopted` mark commit together,
// so a pump/sweep race over the same terminal event counts one turn.
func (ix *Index) AdoptHelperSessionForRun(runID, groupID, runtime, nativeSessionID string, transcriptSeq, at int64) (bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE orchestration_managed_run SET detail_json=json_set(detail_json,'$.helper_session_adopted',1)
		WHERE run_id=? AND state='completed' AND json_extract(detail_json,'$.helper_session_adopted') IS NULL`, runID)
	if err != nil {
		return false, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE orchestration_group SET
		helper_session_replaced=helper_session_replaced+CASE WHEN helper_native_session_id<>'' AND (helper_native_session_id<>? OR helper_runtime<>?) THEN 1 ELSE 0 END,
		helper_runtime=?, helper_native_session_id=?,
		helper_turns=helper_turns+1,
		helper_transcript_seq=MAX(helper_transcript_seq,?),
		updated_at=? WHERE group_id=?`, nativeSessionID, runtime, runtime, nativeSessionID, transcriptSeq, at, groupID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ClearHelperSessionForRun forgets the helper session after one FAILED run
// whose vendor never answered the resume, exactly once per run (the run is
// marked `helper_session_cleared`). A parked or still-active run never
// clears: it still owns a relaunch on that session.
func (ix *Index) ClearHelperSessionForRun(runID, groupID string, at int64) (bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE orchestration_managed_run SET detail_json=json_set(detail_json,'$.helper_session_cleared',1)
		WHERE run_id=? AND state IN ('failed','interrupted','unknown') AND json_extract(detail_json,'$.helper_session_cleared') IS NULL`, runID)
	if err != nil {
		return false, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE orchestration_group SET
		helper_session_replaced=helper_session_replaced+CASE WHEN helper_native_session_id<>'' THEN 1 ELSE 0 END,
		helper_runtime='', helper_native_session_id='', updated_at=? WHERE group_id=?`, at, groupID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// DropGroupPendingSignal empties the slot (token-guarded, like Clear) and
// records that the signal was dropped and why — never a silent loss.
func (ix *Index) DropGroupPendingSignal(groupID string, token ManagedPendingToken, reason string, at int64) (bool, error) {
	result, err := ix.db.Exec(`UPDATE orchestration_group SET pending_signal_json='{}',pending_producer='',pending_event_id=0,pending_coalesced=0,pending_at=0,
		pending_dropped=pending_dropped+1,pending_dropped_reason=?,updated_at=?
		WHERE group_id=? AND pending_producer=? AND pending_event_id=? AND pending_at=?`, reason, at, groupID, token.Producer, token.EventID, token.At)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// StaleAdmittedManagedRuns lists runs admitted before `before` that never
// linked a child task (Create failed after admission, or the process died
// between the two writes); they occupy their group until settled.
func (ix *Index) StaleAdmittedManagedRuns(before int64, limit int) ([]ManagedRun, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT `+managedRunColumns+` FROM orchestration_managed_run WHERE state='admitted' AND child_task_id='' AND admitted_at<? ORDER BY admitted_at ASC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedRun{}
	for rows.Next() {
		item, err := scanManagedRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
