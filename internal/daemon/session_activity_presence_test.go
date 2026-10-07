package daemon

import (
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

// TestPresenceIdentityCollapseKeepsNewestHeldFile pins Slice A (acceptance
// 1): three held rollout files sharing one native identity produce ONE item
// carrying the newest file's identity, with the collapse counted honestly.
func TestPresenceIdentityCollapseKeepsNewestHeldFile(t *testing.T) {
	now := time.Now()
	sessions := []SessionSummary{
		{Runtime: "codex", ID: "rollout-a", ThreadID: "native-1", ResumeID: "native-1", Path: "/s/a.jsonl", Modified: now.Add(-3 * time.Hour)},
		{Runtime: "codex", ID: "rollout-b", ThreadID: "native-1", ResumeID: "native-1", Path: "/s/b.jsonl", Modified: now.Add(-1 * time.Hour)},
		{Runtime: "codex", ID: "rollout-c", ThreadID: "native-1", ResumeID: "native-1", Path: "/s/c.jsonl", Modified: now.Add(-2 * time.Hour)},
		{Runtime: "codex", ID: "rollout-d", ThreadID: "native-2", ResumeID: "native-2", Path: "/s/d.jsonl", Modified: now.Add(-1 * time.Hour)},
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

func TestPresenceKeepsGuardianSeparateFromParent(t *testing.T) {
	now := time.Now()
	parent := SessionSummary{Runtime: "codex", ID: "parent-rollout", MetaID: "parent", ThreadID: "parent", ResumeID: "parent", Path: "/s/parent.jsonl", Modified: now.Add(-time.Minute)}
	child := SessionSummary{Runtime: "codex", ID: "guardian-rollout", MetaID: "guardian", ThreadID: "parent", ResumeID: "parent", LineageKind: "native-guardian", Path: "/s/guardian.jsonl", Modified: now}
	for _, rows := range [][]SessionSummary{{child, parent}, {parent, child}} {
		items := assemblePresenceItems(now, rows, map[string]bool{parent.Path: true, child.Path: true}, nil)
		if len(items) != 2 {
			t.Fatalf("guardian stole parent presence: %+v", items)
		}
		for _, item := range items {
			if item.CatalogSessionID == child.ID && item.NativeSessionID != child.MetaID {
				t.Fatalf("child published parent identity: %+v", item)
			}
		}
		set := presenceOpenSetFrom(sessionactivity.Snapshot{Capability: sessionactivity.Capability{Status: "available"}, ObservedAt: now, Items: items}, now)
		if !set.isOpen(parent) || !set.isOpen(child) {
			t.Fatalf("exact observed rows missing: %+v", set)
		}
	}
	// A held child cannot suppress the weaker, independently observed parent hook.
	hook := []sessionactivity.Item{{Runtime: "codex", CatalogSessionID: parent.ID, NativeSessionID: "parent", Presence: "open", Evidence: "hook_liveness"}}
	items := assemblePresenceItems(now, []SessionSummary{child, parent}, map[string]bool{child.Path: true}, hook)
	if len(items) != 2 {
		t.Fatalf("child file suppressed parent hook: %+v", items)
	}
}

func TestHookPresenceResolvesParentRatherThanGuardian(t *testing.T) {
	fixture := naturalFixture(t, managedDynamicFixtureDriver{})
	previous := governor
	governor = sendGovernor(fixture)
	t.Cleanup(func() { governor = previous })
	now := time.Now()
	appendNaturalActivity(t, fixture, "start", now.Unix()-30)
	if err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error {
		_, err := tx.AppendEvent(store.EventRecord{TS: now.Unix(), SessionID: "ses-natural", Runtime: "codex", Verb: "read", Tool: "fixture", Origin: "hook"}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	parent := SessionSummary{Runtime: "codex", ID: "parent-rollout", MetaID: "ses-natural", ThreadID: "ses-natural", ResumeID: "ses-natural"}
	child := SessionSummary{Runtime: "codex", ID: "guardian-rollout", MetaID: "guardian", ThreadID: "ses-natural", ResumeID: "ses-natural", LineageKind: "native-guardian"}
	items := hookLiveSessionItems(now, []SessionSummary{child, parent})
	if len(items) != 1 || items[0].CatalogSessionID != parent.ID || items[0].NativeSessionID != parent.ThreadID {
		t.Fatalf("parent hook resolved to guardian: %+v", items)
	}
	if items := hookLiveSessionItems(now, []SessionSummary{child}); len(items) != 0 {
		t.Fatalf("parent hook must not open child-only catalog: %+v", items)
	}
	// A parent action is an unknown parent candidate beside an open child,
	// rather than an unknown duplicate that overwrites the child's catalog row.
	childOpen := []sessionactivity.Item{{Runtime: "codex", CatalogSessionID: child.ID, NativeSessionID: child.MetaID, Presence: "open"}}
	status := mergeStatusCandidates(now, []SessionSummary{child, parent}, childOpen)
	if len(status) != 2 || status[1].CatalogSessionID != parent.ID || status[1].NativeSessionID != parent.ThreadID || status[1].Presence != "unknown" {
		t.Fatalf("parent action candidate misplaced or claimed open: %+v", status)
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
