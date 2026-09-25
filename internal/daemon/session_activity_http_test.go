package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"crossing-guard/internal/sessionactivity"
)

func installTestSessionActivity(t *testing.T) *sessionactivity.Service {
	t.Helper()
	previous := nativeSessionActivity
	service := sessionactivity.NewService(func(_ context.Context, now time.Time) (sessionactivity.Capability, []sessionactivity.Item) {
		return sessionactivity.Capability{Status: "available", Detail: "test"}, []sessionactivity.Item{{
			Runtime: "claude", CatalogSessionID: "catalog", Presence: "open", Execution: "unknown",
			Evidence: "file_open", Freshness: "live", Authority: "observed", ObservedAt: now,
			ExpiresAt: now.Add(time.Minute),
		}}
	}, time.Hour, time.Second)
	service.Start(context.Background())
	setNativeSessionActivity(service)
	t.Cleanup(func() { service.Close(); setNativeSessionActivity(previous) })
	deadline := time.Now().Add(time.Second)
	for service.Snapshot().Generation == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	return service
}

func TestSessionActivityListReturnsQualifiedSnapshot(t *testing.T) {
	installTestSessionActivity(t)
	recorder := httptest.NewRecorder()
	handleSessionActivityList(recorder, httptest.NewRequest("GET", "/api/session-activity", nil))
	if recorder.Code != 200 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var snapshot sessionactivity.Snapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != 1 || len(snapshot.Items) != 1 || snapshot.Items[0].Execution != "unknown" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestSessionActivityStreamRejectsInvalidGeneration(t *testing.T) {
	installTestSessionActivity(t)
	recorder := httptest.NewRecorder()
	handleSessionActivityStream(recorder, httptest.NewRequest("GET", "/api/session-activity/stream?after=-1", nil))
	if recorder.Code != 400 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
