package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// AgentSessionParents (g4 plan §3a): resolves agent child sessions to their
// group's root identities, excludes reply/correction tasks (which resume the
// PARENT session), and chunk-loops so no supplied id is silently truncated.
func TestAgentSessionParentsResolvesRootsExcludesRepliesAndNeverTruncates(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	insertTask := func(id, native string) {
		t.Helper()
		if _, err := ix.db.Exec(`INSERT INTO runtime_task(id,console_scope,idempotency_key,request_digest,runtime,
			catalog_session_id,native_session_id,working_directory,lifecycle,ownership,observation_mode,freshness,
			controllable,created_at,updated_at,retention_deadline)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, "scope", id, "digest", "codex", "", native, "/repo", "running", "crossing-guard", "stream", "live",
			1, 1, 1, 99999); err != nil {
			t.Fatal(err)
		}
	}
	binding, err := ix.PutManagedBinding(testManagedBinding(), ManagedBindingAbsentToken("managed-follower"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := ManagedGroup{GroupID: "org_g", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root",
		RootRuntime: "codex", RootCatalogSessionID: "parent-cat", RootNativeSessionID: "parent-thread",
		ProjectRoot: "/repo", CreatedAt: 2, UpdatedAt: 2}
	budget := ManagedGroupBudget{MaxTotal: 8, MaxActive: 4}
	agentRun := ManagedRun{RunID: "orun_agent", IdempotencyKey: "idem_a", GroupID: group.GroupID,
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "follower",
		ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest,
		ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: "task_root", SourceEventID: 1,
		AdmittedAt: 3, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, agentRun, budget); err != nil {
		t.Fatal(err)
	}
	insertTask("task_child_agent", "agent-native-1")
	if err := ix.StartManagedRun("orun_agent", "task_child_agent", "orel_a", 4); err != nil {
		t.Fatal(err)
	}
	replyRun := ManagedRun{RunID: "orun_reply", IdempotencyKey: "idem_r", GroupID: group.GroupID,
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "helper", Kind: "reply",
		ProfileID: binding.ProfileID, ProfileSourceDigest: binding.ProfileSourceDigest,
		ProfileBundleDigest: binding.ProfileBundleDigest, SourceTaskID: "task_root", SourceEventID: 2,
		AdmittedAt: 5, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, replyRun, budget); err != nil {
		t.Fatal(err)
	}
	insertTask("task_child_reply", "parent-thread")
	if err := ix.StartManagedRun("orun_reply", "task_child_reply", "orel_r", 6); err != nil {
		t.Fatal(err)
	}

	// The real id sits past the 200-id chunk boundary: the loop must reach it.
	ids := make([]string, 0, 450)
	for i := 0; i < 420; i++ {
		ids = append(ids, fmt.Sprintf("filler-%d", i))
	}
	ids = append(ids, "agent-native-1", "parent-thread", "unknown-session")
	parents, err := ix.AgentSessionParents(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 1 {
		t.Fatalf("expected exactly the agent child session, got %#v", parents)
	}
	parent, ok := parents["agent-native-1"]
	if !ok || parent.Role != "follower" || parent.RootRuntime != "codex" ||
		parent.RootCatalogSessionID != "parent-cat" || parent.RootNativeSessionID != "parent-thread" {
		t.Fatalf("agent child did not resolve to its group roots: %#v", parent)
	}
	if _, ok := parents["parent-thread"]; ok {
		t.Fatal("a reply task resumed the PARENT session; it must never mark the parent as an agent session")
	}
}
