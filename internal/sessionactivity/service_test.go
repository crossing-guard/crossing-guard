package sessionactivity

import (
	"context"
	"testing"
	"time"
)

func TestServicePublishesLatestBoundedSnapshot(t *testing.T) {
	calls := 0
	service := NewService(func(_ context.Context, now time.Time) (Capability, []Item) {
		calls++
		return Capability{Status: "available"}, []Item{{Runtime: "claude", CatalogSessionID: "s",
			Presence: "open", Execution: "unknown", ObservedAt: now}}
	}, 5*time.Millisecond, time.Second)
	service.Start(context.Background())
	defer service.Close()

	deadline := time.Now().Add(time.Second)
	for service.Snapshot().Generation == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	initial := service.Snapshot()
	if initial.SchemaVersion != 1 || initial.Generation == 0 || len(initial.Items) != 1 {
		t.Fatalf("initial snapshot = %+v", initial)
	}
	subscription := service.Subscribe(initial.Generation)
	select {
	case next := <-subscription.Updates:
		if next.Generation <= initial.Generation || calls < 2 {
			t.Fatalf("next snapshot = %+v calls=%d", next, calls)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for activity update")
	}
	service.Unsubscribe(subscription.ID)
}

func TestSubscribeAfterOldGenerationReceivesCurrentSnapshot(t *testing.T) {
	service := NewService(func(_ context.Context, now time.Time) (Capability, []Item) {
		return Capability{Status: "available"}, []Item{{Runtime: "codex", CatalogSessionID: "c", ObservedAt: now}}
	}, time.Hour, time.Second)
	service.Start(context.Background())
	defer service.Close()
	deadline := time.Now().Add(time.Second)
	for service.Snapshot().Generation == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	subscription := service.Subscribe(0)
	defer service.Unsubscribe(subscription.ID)
	select {
	case snapshot := <-subscription.Updates:
		if snapshot.Generation == 0 || snapshot.Items[0].CatalogSessionID != "c" {
			t.Fatalf("snapshot = %+v", snapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for current snapshot")
	}
}
