package daemon

import (
	"encoding/json"
	"errors"
	"time"

	"crossing-guard/store"
)

type RuntimeTask struct {
	RequestedSettings         *store.TaskRequestedSettings `json:"requested_settings,omitempty"`
	ID                        string                       `json:"id"`
	Runtime                   string                       `json:"session_runtime"`
	CatalogSessionID          string                       `json:"catalog_session_id,omitempty"`
	NativeSessionID           string                       `json:"native_session_id,omitempty"`
	WorkingDirectory          string                       `json:"working_directory"`
	WorkspaceSelectionID      string                       `json:"workspace_selection_id,omitempty"`
	WorkspaceSelectionVersion int64                        `json:"workspace_selection_version,omitempty"`
	Lifecycle                 TaskLifecycle                `json:"lifecycle"`
	Ownership                 string                       `json:"ownership"`
	ObservationMode           string                       `json:"observation_mode"`
	Freshness                 string                       `json:"freshness"`
	Controllable              bool                         `json:"controllable"`
	CreatedAt                 int64                        `json:"created_at"`
	UpdatedAt                 int64                        `json:"updated_at"`
	LastSequence              int64                        `json:"last_sequence"`
	LastEventID               int64                        `json:"last_event_id"`
	ErrorText                 string                       `json:"error,omitempty"`
	RetentionDeadline         int64                        `json:"retention_deadline"`
	Events                    []TaskEvent                  `json:"events,omitempty"`
}

type TaskEvent struct {
	// Producer names the durable stream this event was read from (the task
	// pump or one of the natural hook-row streams). It qualifies orchestration
	// run identity so overlapping integer id spaces never collide (plan B1).
	// Set by the router that read the event; never persisted.
	Producer         string         `json:"-"`
	SchemaVersion    int            `json:"schema_version"`
	EventID          int64          `json:"event_id"`
	TaskID           string         `json:"task_id"`
	Runtime          string         `json:"session_runtime"`
	CatalogSessionID string         `json:"catalog_session_id,omitempty"`
	NativeSessionID  string         `json:"native_session_id,omitempty"`
	Sequence         int64          `json:"sequence"`
	OccurredAt       int64          `json:"occurred_at"`
	ObservedAt       int64          `json:"observed_at"`
	Kind             string         `json:"kind"`
	SourceKind       string         `json:"source_kind"`
	EvidenceClass    string         `json:"evidence_class"`
	Freshness        string         `json:"freshness"`
	Payload          map[string]any `json:"payload"`
}

type taskCreateRecord struct {
	RequestedSettings                                            *store.TaskRequestedSettings
	SessionEffortToken                                           string
	SessionEffortID                                              string
	ID, ConsoleScope, IdempotencyKey, RequestDigest              string
	Runtime, CatalogSessionID, NativeSessionID, WorkingDirectory string
	WorkspaceSelectionID                                         string
	WorkspaceSelectionVersion                                    int64
	CreatedAt, RetentionDeadline                                 int64
}

type taskRepository interface {
	Create(taskCreateRecord) (RuntimeTask, TaskEvent, bool, error)
	ByID(string) (RuntimeTask, bool, error)
	ByIdempotency(string, string) (RuntimeTask, bool, error)
	List(string, string, int) ([]RuntimeTask, error)
	ListActive() ([]RuntimeTask, error)
	Transition(string, TaskLifecycle, TaskLifecycle, int64, string) (RuntimeTask, bool, error)
	TransitionWithEvent(string, TaskLifecycle, TaskLifecycle, int64, string, string, string, map[string]any) (RuntimeTask, TaskEvent, bool, error)
	SetNativeSession(string, string, int64) (bool, error)
	Append(string, string, string, map[string]any, int64) (TaskEvent, error)
	Events(string, int64, int) ([]TaskEvent, error)
	EventsAfter(int64, int) ([]TaskEvent, error)
	Watermark() (int64, error)
	EventRange() (int64, int64, error)
	DeleteExpired(int64) (int64, error)
}

type taskStoreRepository struct{ index *store.Index }

func (r taskStoreRepository) Create(in taskCreateRecord) (RuntimeTask, TaskEvent, bool, error) {
	payload, _ := json.Marshal(map[string]any{"type": "queued", "task_id": in.ID, "requested_settings": in.RequestedSettings})
	record, event, created, err := r.index.CreateRuntimeTaskWithEvent(store.RuntimeTaskRecord{
		RequestedSettings: in.RequestedSettings, SessionEffortToken: in.SessionEffortToken, SessionEffortID: in.SessionEffortID, ID: in.ID, ConsoleScope: in.ConsoleScope, IdempotencyKey: in.IdempotencyKey,
		RequestDigest: in.RequestDigest, Runtime: in.Runtime,
		CatalogSessionID: in.CatalogSessionID, NativeSessionID: in.NativeSessionID, WorkingDirectory: in.WorkingDirectory,
		WorkspaceSelectionID: in.WorkspaceSelectionID, WorkspaceSelectionVersion: in.WorkspaceSelectionVersion,
		Lifecycle: string(TaskQueued), Ownership: "crossing-guard", ObservationMode: "stream",
		Freshness: "live", Controllable: true, CreatedAt: in.CreatedAt,
		UpdatedAt: in.CreatedAt, RetentionDeadline: in.RetentionDeadline,
	}, store.RuntimeTaskEventRecord{SchemaVersion: 1, OccurredAt: in.CreatedAt,
		ObservedAt: in.CreatedAt, Kind: "task.queued", SourceKind: in.Runtime + "-owned-stream",
		EvidenceClass: "observed", Freshness: "live", Payload: payload})
	return runtimeTaskFromStore(record), taskEventFromStore(event), created, err
}

func (r taskStoreRepository) ByID(id string) (RuntimeTask, bool, error) {
	record, found, err := r.index.RuntimeTask(id)
	return runtimeTaskFromStore(record), found, err
}

func (r taskStoreRepository) ByIdempotency(scope, key string) (RuntimeTask, bool, error) {
	record, found, err := r.index.RuntimeTaskByIdempotency(scope, key)
	return runtimeTaskFromStore(record), found, err
}

func (r taskStoreRepository) List(runtime, nativeSessionID string, limit int) ([]RuntimeTask, error) {
	records, err := r.index.ListRuntimeTasks(runtime, nativeSessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RuntimeTask, 0, len(records))
	for _, record := range records {
		out = append(out, runtimeTaskFromStore(record))
	}
	return out, nil
}

func (r taskStoreRepository) ListActive() ([]RuntimeTask, error) {
	records, err := r.index.ListActiveRuntimeTasks()
	if err != nil {
		return nil, err
	}
	out := make([]RuntimeTask, 0, len(records))
	for _, record := range records {
		out = append(out, runtimeTaskFromStore(record))
	}
	return out, nil
}

func (r taskStoreRepository) Transition(id string, from, to TaskLifecycle, at int64, errorText string) (RuntimeTask, bool, error) {
	record, changed, err := r.index.TransitionRuntimeTask(id, string(from), string(to), at, truncate(errorText, 2000))
	return runtimeTaskFromStore(record), changed, err
}

func (r taskStoreRepository) TransitionWithEvent(id string, from, to TaskLifecycle, at int64, errorText, kind, sourceKind string, payload map[string]any) (RuntimeTask, TaskEvent, bool, error) {
	encoded, kind, payload, err := boundedTaskPayload(kind, payload)
	if err != nil {
		return RuntimeTask{}, TaskEvent{}, false, err
	}
	record, eventRecord, changed, err := r.index.TransitionRuntimeTaskWithEvent(id,
		string(from), string(to), at, truncate(errorText, 2000), store.RuntimeTaskEventRecord{
			SchemaVersion: 1, OccurredAt: at, ObservedAt: time.Now().UnixMilli(), Kind: kind,
			SourceKind: sourceKind, EvidenceClass: "observed", Freshness: "live", Payload: encoded,
		})
	if err != nil {
		return RuntimeTask{}, TaskEvent{}, false, err
	}
	event := taskEventFromStore(eventRecord)
	if !changed {
		event.Payload = payload
	}
	return runtimeTaskFromStore(record), event, changed, nil
}

func (r taskStoreRepository) SetNativeSession(id, nativeSessionID string, at int64) (bool, error) {
	return r.index.SetRuntimeTaskNativeSession(id, nativeSessionID, at)
}

func (r taskStoreRepository) Append(taskID, kind, sourceKind string, payload map[string]any, occurredAt int64) (TaskEvent, error) {
	encoded, kind, _, err := boundedTaskPayload(kind, payload)
	if err != nil {
		return TaskEvent{}, err
	}
	now := time.Now().UnixMilli()
	if occurredAt == 0 {
		occurredAt = now
	}
	record, err := r.index.AppendRuntimeTaskEvent(store.RuntimeTaskEventRecord{
		TaskID: taskID, SchemaVersion: 1, OccurredAt: occurredAt, ObservedAt: now,
		Kind: kind, SourceKind: sourceKind, EvidenceClass: "observed",
		Freshness: "live", Payload: encoded,
	})
	if err != nil {
		return TaskEvent{}, err
	}
	return taskEventFromStore(record), nil
}

func boundedTaskPayload(kind string, payload map[string]any) ([]byte, string, map[string]any, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, kind, payload, err
	}
	if len(encoded) > store.RuntimeTaskPayloadLimit {
		payload = map[string]any{"type": "stderr", "text": "Runtime event exceeded the live projection limit; inspect the native session for full details."}
		kind = "coverage.gap"
		encoded, _ = json.Marshal(payload)
	}
	return encoded, kind, payload, nil
}

func (r taskStoreRepository) Events(taskID string, after int64, limit int) ([]TaskEvent, error) {
	records, err := r.index.RuntimeTaskEvents(taskID, after, limit)
	return taskEventsFromStore(records, err)
}

func (r taskStoreRepository) EventsAfter(after int64, limit int) ([]TaskEvent, error) {
	records, err := r.index.RuntimeTaskEventsAfter(after, limit)
	return taskEventsFromStore(records, err)
}

func (r taskStoreRepository) Watermark() (int64, error) { return r.index.RuntimeTaskWatermark() }
func (r taskStoreRepository) EventRange() (int64, int64, error) {
	return r.index.RuntimeTaskEventRange()
}
func (r taskStoreRepository) DeleteExpired(now int64) (int64, error) {
	return r.index.DeleteExpiredRuntimeTasks(now)
}

func runtimeTaskFromStore(record store.RuntimeTaskRecord) RuntimeTask {
	return RuntimeTask{RequestedSettings: record.RequestedSettings, ID: record.ID, Runtime: record.Runtime,
		CatalogSessionID: record.CatalogSessionID, NativeSessionID: record.NativeSessionID, WorkingDirectory: record.WorkingDirectory,
		WorkspaceSelectionID: record.WorkspaceSelectionID, WorkspaceSelectionVersion: record.WorkspaceSelectionVersion,
		Lifecycle: TaskLifecycle(record.Lifecycle), Ownership: record.Ownership,
		ObservationMode: record.ObservationMode, Freshness: record.Freshness,
		Controllable: record.Controllable, CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt, LastSequence: record.LastSequence,
		LastEventID: record.LastEventID, ErrorText: record.ErrorText,
		RetentionDeadline: record.RetentionDeadline}
}

func taskEventFromStore(record store.RuntimeTaskEventRecord) TaskEvent {
	event := TaskEvent{SchemaVersion: record.SchemaVersion, EventID: record.EventID,
		TaskID: record.TaskID, Runtime: record.Runtime, CatalogSessionID: record.CatalogSessionID, NativeSessionID: record.NativeSessionID,
		Sequence: record.Sequence, OccurredAt: record.OccurredAt,
		ObservedAt: record.ObservedAt, Kind: record.Kind, SourceKind: record.SourceKind,
		EvidenceClass: record.EvidenceClass, Freshness: record.Freshness,
		Payload: map[string]any{}}
	_ = json.Unmarshal(record.Payload, &event.Payload)
	return event
}

func taskEventsFromStore(records []store.RuntimeTaskEventRecord, err error) ([]TaskEvent, error) {
	if err != nil {
		return nil, err
	}
	out := make([]TaskEvent, 0, len(records))
	for _, record := range records {
		out = append(out, taskEventFromStore(record))
	}
	return out, nil
}

func runtimeTaskIdempotencyConflict(err error) bool {
	return errors.Is(err, store.ErrRuntimeTaskIdempotencyConflict)
}
