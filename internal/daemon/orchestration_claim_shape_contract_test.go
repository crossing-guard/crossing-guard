package daemon

// Payload-shape contract for the managed projection the orchestration GUI
// consumes (plan §5: the path-grep contract tests cannot catch a silent JS
// break). One run with a full v2 claim detail and one reply relationship are
// written through the real store owners and served through the real HTTP
// projection; the test then asserts the EXACT field names the JS reads
// (session-agents-panel.js, agent-turn-decorators.js, settings-agents.js), so
// a Go rename breaks CI instead of the browser.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

func requireKeys(t *testing.T, where string, object map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Fatalf("%s lost field %q (have %v)", where, key, object)
		}
	}
}

func TestManagedProjectionClaimPayloadShapeMatchesWhatTheGUIReads(t *testing.T) {
	root := t.TempDir()
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(ix, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = ix.Close() })

	binding, err := ix.PutManagedBinding(store.ManagedBinding{BindingID: "agent-shape", State: "enabled",
		Role: "helper", ProjectRoot: root, ProfileID: "design-helper", ProfileSourceDigest: "sha256-v1:s",
		ProfileBundleDigest: "sha256-v1:b", Runtime: "managed-fixture", Mode: "", Authority: []string{},
		AllowedProfiles: []store.ManagedProfileRef{}}, store.ManagedBindingAbsentToken("agent-shape"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_shape", BindingID: binding.BindingID, State: "active",
		RootTaskID: "task_root", RootRuntime: "managed-fixture", RootCatalogSessionID: "sess-shape",
		ProjectRoot: root, CreatedAt: 1, UpdatedAt: 1}
	// The exact detail shape finishManagedChild + claimRefResolver write.
	detail := map[string]any{
		"verdict": "needs changes",
		"tags":    []any{"needs-review"},
		"findings": []any{map[string]any{
			"severity": "warn", "statement": "Missing ADR link",
			"refs": []any{map[string]any{
				"kind": "file", "path": "README.md", "line": 3, "session": "sess-shape",
				"anchor": "a1", "resolution": "resolved", "resolved_path": "README.md",
			}},
		}},
	}
	run := store.ManagedRun{RunID: "orun_shape", IdempotencyKey: "idem_shape", GroupID: group.GroupID,
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "helper",
		ProfileID: "design-helper", ProfileSourceDigest: "sha256-v1:s", ProfileBundleDigest: "sha256-v1:b",
		SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: detail}
	if _, _, err := ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	if err := ix.StartManagedRun(run.RunID, "task_child_shape", "orel_shape", 2); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteManagedRun(run.RunID, "completed", "draft_reply", "Link the ADR.", []string{}, detail, "", "", 3); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetRelationshipReply(run.RunID, "task_reply_shape", 1, "anchor-src", "anchor-reply", 4); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, host, owner)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/managed", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("managed status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Runs          []map[string]any `json:"runs"`
		Relationships []map[string]any `json:"relationships"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Runs) != 1 || len(payload.Relationships) != 1 {
		t.Fatalf("payload runs=%d relationships=%d body=%s", len(payload.Runs), len(payload.Relationships), response.Body.String())
	}

	runRow := payload.Runs[0]
	requireKeys(t, "run", runRow, "run_id", "group_id", "binding_id", "role", "state", "action",
		"message", "citations", "detail", "project_root", "source_task_id", "child_task_id")
	if runRow["project_root"] != root {
		t.Fatalf("run.project_root=%v want %q", runRow["project_root"], root)
	}
	detailRow, _ := runRow["detail"].(map[string]any)
	requireKeys(t, "run.detail", detailRow, "verdict", "findings", "tags")
	findings, _ := detailRow["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("findings=%v", detailRow["findings"])
	}
	finding, _ := findings[0].(map[string]any)
	requireKeys(t, "finding", finding, "severity", "statement", "refs")
	refs, _ := finding["refs"].([]any)
	if len(refs) != 1 {
		t.Fatalf("refs=%v", finding["refs"])
	}
	ref, _ := refs[0].(map[string]any)
	requireKeys(t, "ref", ref, "kind", "path", "line", "session", "anchor", "resolution", "resolved_path")

	relationship := payload.Relationships[0]
	requireKeys(t, "relationship", relationship, "group_id", "run_id", "parent_task_id",
		"reply_task_id", "source_turn_anchor", "reply_turn_anchor", "cycle")
	if relationship["reply_task_id"] != "task_reply_shape" || relationship["cycle"] != float64(1) ||
		relationship["source_turn_anchor"] != "anchor-src" || relationship["reply_turn_anchor"] != "anchor-reply" {
		t.Fatalf("relationship=%v", relationship)
	}
}
