package sessionactivity

import (
	"context"
	"sync"
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

// TestRefreshAndReplaceSerialization pins plan D6: an event-driven Replace
// arriving during a sampler refresh lands AFTER the refresh, so a stale
// refresh cannot overwrite a newer event replacement.
func TestRefreshAndReplaceSerialization(t *testing.T) {
	var samplerStarted, samplerCanProceed sync.WaitGroup
	samplerStarted.Add(1)
	samplerCanProceed.Add(1)
	service := NewService(func(_ context.Context, now time.Time) (Capability, []Item) {
		samplerStarted.Done()
		samplerCanProceed.Wait() // block the sampler inside refresh
		return Capability{Status: "available"}, []Item{{Runtime: "opencode", CatalogSessionID: "ses-blocked",
			Presence: "open", Execution: "unknown", ObservedAt: now}}
	}, time.Hour, 5*time.Second)
	service.Start(context.Background())
	defer service.Close()
	samplerStarted.Wait() // the first refresh is now inside the sampler, holding writerMu

	// An event-driven Replace arrives while the refresh is blocked. It should
	// wait for the refresh to finish, then land as the newer generation.
	done := make(chan struct{})
	go func() {
		service.Replace(Item{Runtime: "opencode", CatalogSessionID: "ses-blocked",
			Presence: "unknown", Execution: "running", ObservedAt: time.Now().UTC()})
		close(done)
	}()

	// Give the Replace goroutine time to block on writerMu.
	time.Sleep(20 * time.Millisecond)
	samplerCanProceed.Done() // release the sampler

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Replace did not complete after the refresh finished")
	}
	snapshot := service.Snapshot()
	if snapshot.Generation < 2 {
		t.Fatalf("expected at least 2 generations, got %d", snapshot.Generation)
	}
	// The Replace must have landed: the item's execution is "running", not
	// the sampler's "unknown".
	if len(snapshot.Items) != 1 || snapshot.Items[0].Execution != "running" {
		t.Fatalf("Replace was overwritten by the stale refresh: %+v", snapshot.Items)
	}
}
