package daemon

// Unit coverage for the off-pump claim-ref resolver: statuses are written into
// the completed run's detail_json, session refs resolve through one batched
// catalog set, event refs use exact session-scoped lookup, and failures are
// recorded — never re-targeted.

import (
	"errors"
	"path/filepath"
	"testing"

	"crossing-guard/internal/orchestration"
	"crossing-guard/store"
)

func TestClaimRefResolverWritesResolutionStatuses(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutManagedBinding(store.ManagedBinding{BindingID: "agent-refs", State: "enabled",
		Role: "helper", ProjectRoot: "/repo", ProfileID: "design-helper", ProfileSourceDigest: "sha256-v1:s",
		ProfileBundleDigest: "sha256-v1:b", Runtime: "managed-fixture", Mode: "", Authority: []string{},
		AllowedProfiles: []store.ManagedProfileRef{}}, store.ManagedBindingAbsentToken("agent-refs"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_refs", BindingID: binding.BindingID, State: "active",
		RootTaskID: "task_root", RootRuntime: "managed-fixture", ProjectRoot: "/repo", CreatedAt: 1, UpdatedAt: 1}
	run := store.ManagedRun{RunID: "orun_refs", IdempotencyKey: "idem_refs", GroupID: group.GroupID,
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "helper",
		ProfileID: "design-helper", ProfileSourceDigest: "sha256-v1:s", ProfileBundleDigest: "sha256-v1:b",
		SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	if err := ix.StartManagedRun(run.RunID, "task_child_refs", "orel_refs", 2); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteManagedRun(run.RunID, "completed", "draft_reply", "Findings attached.", nil, nil, "", "", 3); err != nil {
		t.Fatal(err)
	}

	catalogCalls := 0
	resolver := newClaimRefResolver(ix)
	defer resolver.close()
	resolver.resolvePath = func(root, token string) (*RefTarget, error) {
		if root != "/repo" {
			return nil, errors.New("resolver escaped the project-root jail")
		}
		if token == "docs/design/plan.md" {
			return &RefTarget{Token: token, State: RefStale, Path: "docs/design/plan.md"}, nil
		}
		return nil, errors.New("index unavailable")
	}
	resolver.catalogSessionIDs = func() map[string]bool {
		catalogCalls++
		return map[string]bool{"sess-known": true}
	}
	resolver.now = func() int64 { return 42 }

	findings := []orchestration.Finding{{Severity: "warn", Statement: "check refs", Refs: []orchestration.Ref{
		{Kind: "doc", Path: "docs/design/plan.md"},
		{Kind: "file", Path: "missing.go"},
		{Kind: "session", Session: "sess-known"},
		{Kind: "session", Session: "sess-unknown"},
		{Kind: "event", Session: "sess-known", Path: "7"},
	}}}
	resolver.resolveJob(claimRefJob{runID: run.RunID, projectRoot: "/repo", findings: findings})

	stored, found, err := ix.ManagedRun(run.RunID)
	if err != nil || !found || stored.Detail["refs_resolved_at"] != float64(42) {
		t.Fatalf("stored=%+v found=%v err=%v", stored, found, err)
	}
	rows, _ := stored.Detail["findings"].([]any)
	if len(rows) != 1 {
		t.Fatalf("findings=%+v", stored.Detail["findings"])
	}
	refs, _ := rows[0].(map[string]any)["refs"].([]any)
	if len(refs) != 5 {
		t.Fatalf("refs=%+v", refs)
	}
	resolutions := make([]string, 0, len(refs))
	for _, ref := range refs {
		resolution, _ := ref.(map[string]any)["resolution"].(string)
		resolutions = append(resolutions, resolution)
	}
	want := []string{"stale", "unresolved", "resolved", "missing", "missing"}
	for index, expected := range want {
		if resolutions[index] != expected {
			t.Fatalf("resolutions=%v want %v", resolutions, want)
		}
	}
	if catalogCalls != 1 {
		t.Fatalf("catalog scans=%d (session resolution must batch)", catalogCalls)
	}
	if stored.State != "completed" || stored.Message != "Findings attached." {
		t.Fatalf("resolver changed run state/message: %+v", stored)
	}
}
