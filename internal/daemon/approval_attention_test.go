package daemon

import (
	"testing"
	"time"
)

func TestApprovalAttentionRoutesOnlySupplementalNotification(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	var notifications []string
	router := newApprovalAttentionRouter(func(message string) {
		notifications = append(notifications, message)
	})
	router.now = func() time.Time { return now }

	if notified := router.Notify("first"); !notified || len(notifications) != 1 {
		t.Fatalf("disconnected notification = %v %#v", notified, notifications)
	}
	router.Update("client_12345678", true, true)
	if notified := router.Notify("foreground"); notified || len(notifications) != 1 {
		t.Fatalf("foreground notification = %v %#v", notified, notifications)
	}
	router.Update("client_12345678", true, false)
	if notified := router.Notify("background"); !notified || len(notifications) != 2 {
		t.Fatalf("background notification = %v %#v", notified, notifications)
	}
	router.Update("client_12345678", true, true)
	now = now.Add(approvalPresenceLease + time.Millisecond)
	if notified := router.Notify("stale"); !notified || len(notifications) != 3 {
		t.Fatalf("stale notification = %v %#v", notified, notifications)
	}
}

func TestApprovalAttentionAnyFocusedTabSuppressesDuplicate(t *testing.T) {
	var count int
	router := newApprovalAttentionRouter(func(string) { count++ })
	router.Update("client_hidden1", false, false)
	router.Update("client_active2", true, true)
	if router.Notify("approval") || count != 0 {
		t.Fatalf("active second tab did not suppress duplicate OS notification")
	}
}
