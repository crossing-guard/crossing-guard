package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestRuntimeTaskIdempotencyOrderingAndTransition(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	input := RuntimeTaskRecord{
		ID: "task-1", ConsoleScope: "local", IdempotencyKey: "send-1",
		RequestDigest: "digest-1", Runtime: "fixture", CatalogSessionID: "catalog-1",
		NativeSessionID: "native-1", WorkingDirectory: t.TempDir(),
		Lifecycle: "queued", Ownership: "crossing-guard", ObservationMode: "stream",
		Freshness: "live", Controllable: true, CreatedAt: 100, UpdatedAt: 100,
		RetentionDeadline: 200,
	}
	created, fresh, err := ix.CreateRuntimeTask(input)
	if err != nil || !fresh || created.ID != input.ID || created.CatalogSessionID != "catalog-1" {
		t.Fatalf("create=%+v fresh=%v err=%v", created, fresh, err)
	}
	retry, fresh, err := ix.CreateRuntimeTask(input)
	if err != nil || fresh || retry.ID != input.ID {
		t.Fatalf("retry=%+v fresh=%v err=%v", retry, fresh, err)
	}
	conflict := input
	conflict.ID = "task-2"
	conflict.RequestDigest = "digest-2"
	if _, _, err := ix.CreateRuntimeTask(conflict); !errors.Is(err, ErrRuntimeTaskIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}

	if _, changed, err := ix.TransitionRuntimeTask(input.ID, "queued", "starting", 110, ""); err != nil || !changed {
		t.Fatalf("queued->starting changed=%v err=%v", changed, err)
	}
	if _, changed, err := ix.TransitionRuntimeTask(input.ID, "queued", "running", 111, ""); err != nil || changed {
		t.Fatalf("stale transition changed=%v err=%v", changed, err)
	}
	transitioned, transitionEvent, changed, err := ix.TransitionRuntimeTaskWithEvent(input.ID,
		"starting", "running", 111, "", RuntimeTaskEventRecord{SchemaVersion: 1,
			OccurredAt: 111, ObservedAt: 111, Kind: "task.started", SourceKind: "fixture-stream",
			EvidenceClass: "observed", Freshness: "live", Payload: []byte(`{"type":"spawn"}`)})
	if err != nil || !changed || transitioned.Lifecycle != "running" || transitionEvent.Sequence != 1 {
		t.Fatalf("atomic transition task=%+v event=%+v changed=%v err=%v", transitioned, transitionEvent, changed, err)
	}

	first, err := ix.AppendRuntimeTaskEvent(RuntimeTaskEventRecord{TaskID: input.ID,
		SchemaVersion: 1, OccurredAt: 112, ObservedAt: 112, Kind: "task.started",
		SourceKind: "fixture-stream", EvidenceClass: "observed", Freshness: "live",
		Payload: []byte(`{"type":"spawn"}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ix.AppendRuntimeTaskEvent(RuntimeTaskEventRecord{TaskID: input.ID,
		SchemaVersion: 1, OccurredAt: 113, ObservedAt: 113, Kind: "message.completed",
		SourceKind: "fixture-stream", EvidenceClass: "observed", Freshness: "live",
		Payload: []byte(`{"type":"text","text":"ok"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 2 || second.Sequence != 3 || second.EventID <= first.EventID ||
		second.CatalogSessionID != "catalog-1" || second.NativeSessionID != "native-1" {
		t.Fatalf("events out of order: first=%+v second=%+v", first, second)
	}
	events, err := ix.RuntimeTaskEvents(input.ID, 2, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 3 {
		t.Fatalf("replay=%+v err=%v", events, err)
	}
}

func TestRuntimeTaskPayloadBound(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	_, _, err = ix.CreateRuntimeTask(RuntimeTaskRecord{ID: "task", ConsoleScope: "local",
		IdempotencyKey: "key", RequestDigest: "digest", Runtime: "fixture",
		WorkingDirectory: t.TempDir(), Lifecycle: "queued", Ownership: "crossing-guard",
		ObservationMode: "stream", Freshness: "live", Controllable: true,
		CreatedAt: 1, UpdatedAt: 1, RetentionDeadline: 2})
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, RuntimeTaskPayloadLimit+1)
	if _, err := ix.AppendRuntimeTaskEvent(RuntimeTaskEventRecord{TaskID: "task",
		SchemaVersion: 1, OccurredAt: 1, ObservedAt: 1, Kind: "task.activity",
		SourceKind: "fixture", EvidenceClass: "observed", Freshness: "live",
		Payload: payload}); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestRuntimeTaskUnknownDropsControlAuthority(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	_, _, err = ix.CreateRuntimeTask(RuntimeTaskRecord{ID: "task", ConsoleScope: "local",
		IdempotencyKey: "unknown-key", RequestDigest: "digest", Runtime: "fixture",
		WorkingDirectory: t.TempDir(), Lifecycle: "queued", Ownership: "crossing-guard",
		ObservationMode: "stream", Freshness: "live", Controllable: true,
		CreatedAt: 1, UpdatedAt: 1, RetentionDeadline: 2})
	if err != nil {
		t.Fatal(err)
	}
	task, _, changed, err := ix.TransitionRuntimeTaskWithEvent("task", "queued", "unknown", 2,
		"lost", RuntimeTaskEventRecord{SchemaVersion: 1, OccurredAt: 2, ObservedAt: 2,
			Kind: "task.unknown", SourceKind: "fixture", EvidenceClass: "observed",
			Freshness: "unknown", Payload: []byte(`{"type":"error"}`)})
	if err != nil || !changed || task.Controllable || task.Freshness != "unknown" {
		t.Fatalf("task=%+v changed=%v err=%v", task, changed, err)
	}
}

func TestRuntimeTaskRetentionDeletesOnlyExpiredTerminalWork(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	create := func(id, lifecycle string, deadline int64) {
		t.Helper()
		_, _, err := ix.CreateRuntimeTask(RuntimeTaskRecord{ID: id, ConsoleScope: "local",
			IdempotencyKey: "key-" + id, RequestDigest: "digest-" + id, Runtime: "fixture",
			WorkingDirectory: t.TempDir(), Lifecycle: lifecycle, Ownership: "crossing-guard",
			ObservationMode: "stream", Freshness: "live", Controllable: true,
			CreatedAt: 1, UpdatedAt: 1, RetentionDeadline: deadline})
		if err != nil {
			t.Fatal(err)
		}
	}
	create("expired", "completed", 2)
	create("active", "running", 2)
	create("retained", "completed", 20)
	deleted, err := ix.DeleteExpiredRuntimeTasks(10)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if _, found, _ := ix.RuntimeTask("expired"); found {
		t.Fatal("expired terminal task remains")
	}
	for _, id := range []string{"active", "retained"} {
		if _, found, err := ix.RuntimeTask(id); err != nil || !found {
			t.Fatalf("task %s found=%v err=%v", id, found, err)
		}
	}
}

func TestRuntimeTaskSnapshotPrioritizesActiveWork(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for _, task := range []RuntimeTaskRecord{
		{ID: "active", ConsoleScope: "local", IdempotencyKey: "active-key",
			RequestDigest: "active-digest", Runtime: "fixture", WorkingDirectory: t.TempDir(),
			Lifecycle: "running", Ownership: "crossing-guard", ObservationMode: "stream",
			Freshness: "live", Controllable: true, CreatedAt: 1, UpdatedAt: 1, RetentionDeadline: 100},
		{ID: "newer-terminal", ConsoleScope: "local", IdempotencyKey: "terminal-key",
			RequestDigest: "terminal-digest", Runtime: "fixture", WorkingDirectory: t.TempDir(),
			Lifecycle: "completed", Ownership: "crossing-guard", ObservationMode: "stream",
			Freshness: "live", Controllable: false, CreatedAt: 2, UpdatedAt: 2, RetentionDeadline: 100},
	} {
		if _, _, err := ix.CreateRuntimeTask(task); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := ix.ListRuntimeTasks("", "", 1)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "active" {
		t.Fatalf("snapshot tasks=%+v err=%v", tasks, err)
	}
}
