package store

import (
	"database/sql"
	"errors"
	"fmt"
)

const RuntimeTaskPayloadLimit = 64 * 1024

var ErrRuntimeTaskIdempotencyConflict = errors.New("runtime task idempotency key reused with a different request")

type RuntimeTaskRecord struct {
	ID                        string
	ConsoleScope              string
	IdempotencyKey            string
	RequestDigest             string
	Runtime                   string
	CatalogSessionID          string
	NativeSessionID           string
	WorkingDirectory          string
	WorkspaceSelectionID      string
	WorkspaceSelectionVersion int64
	Lifecycle                 string
	Ownership                 string
	ObservationMode           string
	Freshness                 string
	Controllable              bool
	CreatedAt                 int64
	UpdatedAt                 int64
	LastSequence              int64
	LastEventID               int64
	ErrorText                 string
	RetentionDeadline         int64
}

type RuntimeTaskEventRecord struct {
	EventID          int64
	TaskID           string
	Runtime          string
	CatalogSessionID string
	NativeSessionID  string
	Sequence         int64
	SchemaVersion    int
	OccurredAt       int64
	ObservedAt       int64
	Kind             string
	SourceKind       string
	EvidenceClass    string
	Freshness        string
	Payload          []byte
}

func scanRuntimeTask(row interface{ Scan(...any) error }) (RuntimeTaskRecord, error) {
	var out RuntimeTaskRecord
	var controllable int
	err := row.Scan(&out.ID, &out.ConsoleScope, &out.IdempotencyKey, &out.RequestDigest,
		&out.Runtime, &out.CatalogSessionID, &out.NativeSessionID, &out.WorkingDirectory,
		&out.WorkspaceSelectionID, &out.WorkspaceSelectionVersion, &out.Lifecycle,
		&out.Ownership, &out.ObservationMode, &out.Freshness, &controllable,
		&out.CreatedAt, &out.UpdatedAt, &out.LastSequence, &out.LastEventID,
		&out.ErrorText, &out.RetentionDeadline)
	out.Controllable = controllable == 1
	return out, err
}

const runtimeTaskColumns = `id,console_scope,idempotency_key,request_digest,runtime,
	catalog_session_id,native_session_id,working_directory,workspace_selection_id,workspace_selection_version,
	lifecycle,ownership,observation_mode,freshness,
	controllable,created_at,updated_at,last_sequence,last_event_id,error_text,retention_deadline`

// CreateRuntimeTask is atomic on (console scope, idempotency key). A retry returns the
// original task; reusing the key for different input is an explicit conflict.
func (ix *Index) CreateRuntimeTask(in RuntimeTaskRecord) (RuntimeTaskRecord, bool, error) {
	out, _, created, err := ix.createRuntimeTask(in, nil)
	return out, created, err
}

// CreateRuntimeTaskWithEvent commits the initial queued projection with the task.
// A task therefore cannot exist durably without being replayable in another tab.
func (ix *Index) CreateRuntimeTaskWithEvent(in RuntimeTaskRecord, initial RuntimeTaskEventRecord) (RuntimeTaskRecord, RuntimeTaskEventRecord, bool, error) {
	return ix.createRuntimeTask(in, &initial)
}

func (ix *Index) createRuntimeTask(in RuntimeTaskRecord, initial *RuntimeTaskEventRecord) (RuntimeTaskRecord, RuntimeTaskEventRecord, bool, error) {
	if initial != nil && len(initial.Payload) > RuntimeTaskPayloadLimit {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false,
			fmt.Errorf("runtime task event payload is %d bytes; limit is %d", len(initial.Payload), RuntimeTaskPayloadLimit)
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`INSERT OR IGNORE INTO runtime_task(
		id,console_scope,idempotency_key,request_digest,runtime,catalog_session_id,native_session_id,
		working_directory,workspace_selection_id,workspace_selection_version,lifecycle,ownership,
		observation_mode,freshness,controllable,created_at,updated_at,last_sequence,last_event_id,error_text,retention_deadline)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.ID, in.ConsoleScope,
		in.IdempotencyKey, in.RequestDigest, in.Runtime, in.CatalogSessionID, in.NativeSessionID,
		in.WorkingDirectory, in.WorkspaceSelectionID, in.WorkspaceSelectionVersion,
		in.Lifecycle, in.Ownership, in.ObservationMode,
		in.Freshness, boolInt(in.Controllable), in.CreatedAt, in.UpdatedAt,
		in.LastSequence, in.LastEventID, in.ErrorText, in.RetentionDeadline)
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, fmt.Errorf("create runtime task: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, fmt.Errorf("create runtime task rows: %w", err)
	}
	var queued RuntimeTaskEventRecord
	if rows == 1 && initial != nil {
		queued = *initial
		queued.TaskID = in.ID
		queued.Runtime = in.Runtime
		queued.CatalogSessionID = in.CatalogSessionID
		queued.NativeSessionID = in.NativeSessionID
		queued.Sequence = 1
		eventResult, eventErr := tx.Exec(`INSERT INTO runtime_task_event(task_id,sequence,schema_version,
			occurred_at,observed_at,kind,source_kind,evidence_class,freshness,payload)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, queued.TaskID, queued.Sequence, queued.SchemaVersion,
			queued.OccurredAt, queued.ObservedAt, queued.Kind, queued.SourceKind,
			queued.EvidenceClass, queued.Freshness, queued.Payload)
		if eventErr != nil {
			return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, fmt.Errorf("create runtime task event: %w", eventErr)
		}
		queued.EventID, err = eventResult.LastInsertId()
		if err != nil {
			return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
		}
		if _, err = tx.Exec(`UPDATE runtime_task SET last_sequence=1,last_event_id=? WHERE id=?`,
			queued.EventID, in.ID); err != nil {
			return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
		}
	}
	out, err := scanRuntimeTask(tx.QueryRow(`SELECT `+runtimeTaskColumns+`
		FROM runtime_task WHERE console_scope=? AND idempotency_key=?`,
		in.ConsoleScope, in.IdempotencyKey))
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, fmt.Errorf("read created runtime task: %w", err)
	}
	if out.RequestDigest != in.RequestDigest {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, ErrRuntimeTaskIdempotencyConflict
	}
	if err := tx.Commit(); err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	return out, queued, rows == 1, nil
}

func (ix *Index) RuntimeTask(id string) (RuntimeTaskRecord, bool, error) {
	out, err := scanRuntimeTask(ix.db.QueryRow(`SELECT `+runtimeTaskColumns+`
		FROM runtime_task WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeTaskRecord{}, false, nil
	}
	return out, err == nil, err
}

func (ix *Index) RuntimeTaskByIdempotency(scope, key string) (RuntimeTaskRecord, bool, error) {
	out, err := scanRuntimeTask(ix.db.QueryRow(`SELECT `+runtimeTaskColumns+`
		FROM runtime_task WHERE console_scope=? AND idempotency_key=?`, scope, key))
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeTaskRecord{}, false, nil
	}
	return out, err == nil, err
}

func (ix *Index) ListRuntimeTasks(runtime, nativeSessionID string, limit int) ([]RuntimeTaskRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := ix.db.Query(`SELECT `+runtimeTaskColumns+` FROM runtime_task
		WHERE (?='' OR runtime=?) AND (?='' OR native_session_id=?)
		ORDER BY CASE WHEN lifecycle IN ('queued','starting','running') THEN 0 ELSE 1 END,
		updated_at DESC,id DESC LIMIT ?`, runtime, runtime,
		nativeSessionID, nativeSessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RuntimeTaskRecord, 0)
	for rows.Next() {
		record, err := scanRuntimeTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (ix *Index) ListActiveRuntimeTasks() ([]RuntimeTaskRecord, error) {
	rows, err := ix.db.Query(`SELECT ` + runtimeTaskColumns + ` FROM runtime_task
		WHERE lifecycle IN ('queued','starting','running') ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RuntimeTaskRecord, 0)
	for rows.Next() {
		record, err := scanRuntimeTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// TransitionRuntimeTask uses compare-and-set so concurrent terminal callbacks cannot
// revive or overwrite a task that has already reached an authoritative state.
func (ix *Index) TransitionRuntimeTask(id, from, to string, at int64, errorText string) (RuntimeTaskRecord, bool, error) {
	result, err := ix.db.Exec(`UPDATE runtime_task SET lifecycle=?,updated_at=?,error_text=?,
		controllable=CASE WHEN ?='unknown' THEN 0 ELSE controllable END,
		freshness=CASE WHEN ?='unknown' THEN 'unknown' ELSE freshness END
		WHERE id=? AND lifecycle=?`, to, at, errorText, to, to, id, from)
	if err != nil {
		return RuntimeTaskRecord{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RuntimeTaskRecord{}, false, err
	}
	out, found, err := ix.RuntimeTask(id)
	return out, found && rows == 1, err
}

// SetRuntimeTaskNativeSession records the runtime-reported native session id.
// It updates only an empty or equal value and REPORTS whether the write landed:
// a differing id (a forked/child session after a resume) must surface to the
// caller instead of being silently dropped (agents redesign, ART-02 repair).
func (ix *Index) SetRuntimeTaskNativeSession(id, nativeSessionID string, at int64) (bool, error) {
	result, err := ix.db.Exec(`UPDATE runtime_task SET native_session_id=?,updated_at=?
		WHERE id=? AND (native_session_id='' OR native_session_id=?)`, nativeSessionID, at,
		id, nativeSessionID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (ix *Index) AppendRuntimeTaskEvent(in RuntimeTaskEventRecord) (RuntimeTaskEventRecord, error) {
	if len(in.Payload) > RuntimeTaskPayloadLimit {
		return RuntimeTaskEventRecord{}, fmt.Errorf("runtime task event payload is %d bytes; limit is %d", len(in.Payload), RuntimeTaskPayloadLimit)
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return RuntimeTaskEventRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var last int64
	if err := tx.QueryRow(`SELECT last_sequence,runtime,catalog_session_id,native_session_id FROM runtime_task WHERE id=?`, in.TaskID).
		Scan(&last, &in.Runtime, &in.CatalogSessionID, &in.NativeSessionID); err != nil {
		return RuntimeTaskEventRecord{}, err
	}
	in.Sequence = last + 1
	result, err := tx.Exec(`INSERT INTO runtime_task_event(task_id,sequence,schema_version,
		occurred_at,observed_at,kind,source_kind,evidence_class,freshness,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, in.TaskID, in.Sequence, in.SchemaVersion,
		in.OccurredAt, in.ObservedAt, in.Kind, in.SourceKind, in.EvidenceClass,
		in.Freshness, in.Payload)
	if err != nil {
		return RuntimeTaskEventRecord{}, fmt.Errorf("append runtime task event: %w", err)
	}
	in.EventID, err = result.LastInsertId()
	if err != nil {
		return RuntimeTaskEventRecord{}, err
	}
	if _, err := tx.Exec(`UPDATE runtime_task SET last_sequence=?,last_event_id=?,updated_at=?
		WHERE id=?`, in.Sequence, in.EventID, in.ObservedAt, in.TaskID); err != nil {
		return RuntimeTaskEventRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return RuntimeTaskEventRecord{}, err
	}
	return in, nil
}

// TransitionRuntimeTaskWithEvent commits the authoritative lifecycle change and the
// replay event in one transaction. A browser can never observe a terminal snapshot
// whose terminal event was lost between two writes.
func (ix *Index) TransitionRuntimeTaskWithEvent(id, from, to string, at int64, errorText string, in RuntimeTaskEventRecord) (RuntimeTaskRecord, RuntimeTaskEventRecord, bool, error) {
	if len(in.Payload) > RuntimeTaskPayloadLimit {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false,
			fmt.Errorf("runtime task event payload is %d bytes; limit is %d", len(in.Payload), RuntimeTaskPayloadLimit)
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE runtime_task SET lifecycle=?,updated_at=?,error_text=?,
		controllable=CASE WHEN ?='unknown' THEN 0 ELSE controllable END,
		freshness=CASE WHEN ?='unknown' THEN 'unknown' ELSE freshness END
		WHERE id=? AND lifecycle=?`, to, at, errorText, to, to, id, from)
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	if rows == 0 {
		record, readErr := scanRuntimeTask(tx.QueryRow(`SELECT `+runtimeTaskColumns+` FROM runtime_task WHERE id=?`, id))
		return record, RuntimeTaskEventRecord{}, false, readErr
	}
	var last int64
	if err := tx.QueryRow(`SELECT last_sequence,runtime,catalog_session_id,native_session_id FROM runtime_task WHERE id=?`, id).
		Scan(&last, &in.Runtime, &in.CatalogSessionID, &in.NativeSessionID); err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	in.TaskID = id
	in.Sequence = last + 1
	eventResult, err := tx.Exec(`INSERT INTO runtime_task_event(task_id,sequence,schema_version,
		occurred_at,observed_at,kind,source_kind,evidence_class,freshness,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, in.TaskID, in.Sequence, in.SchemaVersion,
		in.OccurredAt, in.ObservedAt, in.Kind, in.SourceKind, in.EvidenceClass,
		in.Freshness, in.Payload)
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	in.EventID, err = eventResult.LastInsertId()
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	if _, err := tx.Exec(`UPDATE runtime_task SET last_sequence=?,last_event_id=? WHERE id=?`,
		in.Sequence, in.EventID, id); err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	record, err := scanRuntimeTask(tx.QueryRow(`SELECT `+runtimeTaskColumns+` FROM runtime_task WHERE id=?`, id))
	if err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return RuntimeTaskRecord{}, RuntimeTaskEventRecord{}, false, err
	}
	return record, in, true, nil
}

func scanRuntimeTaskEvent(row interface{ Scan(...any) error }) (RuntimeTaskEventRecord, error) {
	var out RuntimeTaskEventRecord
	err := row.Scan(&out.EventID, &out.TaskID, &out.Runtime, &out.CatalogSessionID, &out.NativeSessionID,
		&out.Sequence, &out.SchemaVersion,
		&out.OccurredAt, &out.ObservedAt, &out.Kind, &out.SourceKind,
		&out.EvidenceClass, &out.Freshness, &out.Payload)
	return out, err
}

func (ix *Index) RuntimeTaskEvents(taskID string, afterSequence int64, limit int) ([]RuntimeTaskEventRecord, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	rows, err := ix.db.Query(`SELECT event.event_id,event.task_id,task.runtime,task.catalog_session_id,task.native_session_id,
		event.sequence,event.schema_version,event.occurred_at,event.observed_at,event.kind,
		event.source_kind,event.evidence_class,event.freshness,event.payload
		FROM runtime_task_event event JOIN runtime_task task ON task.id=event.task_id
		WHERE event.task_id=? AND event.sequence>? ORDER BY event.sequence LIMIT ?`,
		taskID, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RuntimeTaskEventRecord, 0)
	for rows.Next() {
		event, err := scanRuntimeTaskEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (ix *Index) RuntimeTaskEventsAfter(eventID int64, limit int) ([]RuntimeTaskEventRecord, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	rows, err := ix.db.Query(`SELECT event.event_id,event.task_id,task.runtime,task.catalog_session_id,task.native_session_id,
		event.sequence,event.schema_version,event.occurred_at,event.observed_at,event.kind,
		event.source_kind,event.evidence_class,event.freshness,event.payload
		FROM runtime_task_event event JOIN runtime_task task ON task.id=event.task_id
		WHERE event.event_id>? ORDER BY event.event_id LIMIT ?`, eventID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RuntimeTaskEventRecord, 0)
	for rows.Next() {
		event, err := scanRuntimeTaskEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (ix *Index) RuntimeTaskWatermark() (int64, error) {
	var out int64
	err := ix.db.QueryRow(`SELECT COALESCE(MAX(event_id),0) FROM runtime_task_event`).Scan(&out)
	return out, err
}

func (ix *Index) RuntimeTaskEventRange() (int64, int64, error) {
	var minimum, maximum int64
	err := ix.db.QueryRow(`SELECT COALESCE(MIN(event_id),0),COALESCE(MAX(event_id),0)
		FROM runtime_task_event`).Scan(&minimum, &maximum)
	return minimum, maximum, err
}

// DeleteExpiredRuntimeTasks enforces the stored retention contract without ever
// deleting active work. Event rows are removed by the task foreign-key cascade.
func (ix *Index) DeleteExpiredRuntimeTasks(now int64) (int64, error) {
	result, err := ix.db.Exec(`DELETE FROM runtime_task
		WHERE retention_deadline<=? AND lifecycle IN ('completed','interrupted','failed','unknown')`, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
