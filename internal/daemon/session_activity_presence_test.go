package daemon

import (
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/sessionactivity"
)

// TestPresenceIdentityCollapseKeepsNewestHeldFile pins Slice A (acceptance
// 1): three held rollout files sharing one native identity produce ONE item
// carrying the newest file's identity, with the collapse counted honestly.
func TestPresenceIdentityCollapseKeepsNewestHeldFile(t *testing.T) {
	now := time.Now()
	sessions := []SessionSummary{
		{Runtime: "codex", ID: "rollout-a", ResumeID: "native-1", Path: "/s/a.jsonl", Modified: now.Add(-3 * time.Hour)},
		{Runtime: "codex", ID: "rollout-b", ResumeID: "native-1", Path: "/s/b.jsonl", Modified: now.Add(-1 * time.Hour)},
		{Runtime: "codex", ID: "rollout-c", ResumeID: "native-1", Path: "/s/c.jsonl", Modified: now.Add(-2 * time.Hour)},
		{Runtime: "codex", ID: "rollout-d", ResumeID: "native-2", Path: "/s/d.jsonl", Modified: now.Add(-1 * time.Hour)},
	}
	open := map[string]bool{"/s/a.jsonl": true, "/s/b.jsonl": true, "/s/c.jsonl": true, "/s/d.jsonl": true}
	items := assemblePresenceItems(now, sessions, open, nil)
	if len(items) != 2 {
		t.Fatalf("collapse produced %d items: %+v", len(items), items)
	}
	if items[0].CatalogSessionID != "rollout-b" || !strings.Contains(items[0].Detail, "3 session files") {
		t.Fatalf("collapse did not keep the newest with an honest count: %+v", items[0])
	}
	if items[1].CatalogSessionID != "rollout-d" || strings.Contains(items[1].Detail, "session files") {
		t.Fatalf("single-file session gained a collapse note: %+v", items[1])
	}
}

// TestPresenceMergePrefersHeldFileOverHookLiveness pins acceptance 4: a
// session present in both lanes yields one item with the stronger evidence.
func TestPresenceMergePrefersHeldFileOverHookLiveness(t *testing.T) {
	now := time.Now()
	sessions := []SessionSummary{
		{Runtime: "claude", ID: "cat-1", ResumeID: "cat-1", Path: "/s/one.jsonl", Modified: now},
	}
	open := map[string]bool{"/s/one.jsonl": true}
	hook := []sessionactivity.Item{{Runtime: "claude", CatalogSessionID: "cat-1", NativeSessionID: "cat-1",
		Presence: "open", Evidence: "hook_liveness", Freshness: "live"}}
	items := assemblePresenceItems(now, sessions, open, hook)
	if len(items) != 1 || items[0].Evidence != "file_open" {
		t.Fatalf("merge did not prefer file_open: %+v", items)
	}
	// And without the held file, the hook item carries the session alone.
	items = assemblePresenceItems(now, sessions, map[string]bool{}, hook)
	if len(items) != 1 || items[0].Evidence != "hook_liveness" {
		t.Fatalf("hook lane alone lost the session: %+v", items)
	}
}
