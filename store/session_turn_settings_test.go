package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestEffortCASAndAtomicAdmission(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	empty, err := ix.SessionTurnSettings("fixture", "native", "model")
	if err != nil || empty.Token != "0" || empty.Effort.Kind != "inherit" {
		t.Fatalf("absent: %+v %v", empty, err)
	}
	high := ThinkingEffort{Kind: "level", Value: "opaque-high"}
	saved, err := ix.SaveSessionTurnSettings("fixture", "native", "model", "0", high)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ix.SaveSessionTurnSettings("fixture", "native", "model", "0", high); !errors.Is(err, ErrSessionEffortConflict) {
		t.Fatalf("stale write: %v", err)
	}
	input := RuntimeTaskRecord{ID: "effort-task", ConsoleScope: "local", IdempotencyKey: "effort-key", RequestDigest: "digest", Runtime: "fixture", NativeSessionID: "native", WorkingDirectory: t.TempDir(), Lifecycle: "queued", Ownership: "crossing-guard", ObservationMode: "stream", Freshness: "live", Controllable: true, CreatedAt: 1, UpdatedAt: 1, RetentionDeadline: 100,
		RequestedSettings: &TaskRequestedSettings{Model: "model", Effort: high, Source: "session"}, SessionEffortToken: saved.Token, SessionEffortID: "native"}
	task, created, err := ix.CreateRuntimeTask(input)
	if err != nil || !created || task.RequestedSettings.Effort != high {
		t.Fatalf("admission: %+v %v", task, err)
	}
	reset, err := ix.SaveSessionTurnSettings("fixture", "native", "model", saved.Token, ThinkingEffort{Kind: "inherit"})
	if err != nil || reset.Token == "0" {
		t.Fatalf("reset %+v %v", reset, err)
	}
	// A retry is historical, not a new admission against mutable defaults.
	retry, created, err := ix.CreateRuntimeTask(input)
	if err != nil || created || retry.RequestedSettings.Effort != high {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	input.ID = "refused-task"
	input.IdempotencyKey = "new-key"
	if _, _, err = ix.CreateRuntimeTask(input); !errors.Is(err, ErrSessionEffortConflict) {
		t.Fatalf("stale admission: %v", err)
	}
	if _, found, err := ix.RuntimeTask(input.ID); err != nil || found {
		t.Fatalf("refused admission left a row: %v %v", found, err)
	}
	other, err := ix.SessionTurnSettings("fixture", "native", "different-model")
	if err != nil || other.Token != "0" {
		t.Fatalf("model leaked: %+v %v", other, err)
	}
}

func TestManagedEffortRoundTripAndToken(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding := testManagedBinding()
	without := ManagedBindingStateToken(binding)
	binding.ThinkingEffort = &ThinkingEffort{Kind: "level", Value: "high"}
	binding.Routes = []ManagedRoute{{Runtime: "fixture", Model: "alternative", ThinkingEffort: &ThinkingEffort{Kind: "level", Value: "variant-b"}}}
	if ManagedBindingStateToken(binding) == without {
		t.Fatal("effort not covered by state token")
	}
	saved, err := ix.PutManagedBinding(binding, ManagedBindingAbsentToken(binding.BindingID), 1)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := ix.ManagedBinding(binding.BindingID)
	if err != nil || !found || got.ThinkingEffort.Value != "high" || got.Routes[0].ThinkingEffort.Value != "variant-b" || got.StateToken != saved.StateToken {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
}

func TestEffortMigrationPreservesRowsAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ix.CreateRuntimeTask(RuntimeTaskRecord{ID: "old", ConsoleScope: "console", IdempotencyKey: "old", RequestDigest: "digest", Runtime: "fixture", Lifecycle: "queued", Ownership: "crossing-guard", ObservationMode: "stream", Freshness: "live", CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`ALTER TABLE runtime_task DROP COLUMN requested_settings; ALTER TABLE orchestration_managed_binding DROP COLUMN thinking_effort; DROP TABLE session_turn_settings; PRAGMA user_version=36;`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		ix, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		task, found, err := ix.RuntimeTask("old")
		if err != nil || !found || task.RequestedSettings != nil || task.RequestDigest != "digest" {
			t.Fatalf("task %+v %v %v", task, found, err)
		}
		setting, err := ix.SessionTurnSettings("fixture", "native", "model")
		if err != nil || setting.Token != "0" {
			t.Fatalf("setting %+v %v", setting, err)
		}
		if err := ix.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
