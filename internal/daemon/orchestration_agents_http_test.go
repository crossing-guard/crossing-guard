package daemon

// HTTP coverage for the agent-shaped surface: the roster with resolved shipped
// defaults and the published signal catalog, the agent PUT accepting
// priority/declared_tags/limits under the existing CAS semantics, group notes
// CRUD, and the session tags read.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

func TestAgentsHTTPSurfaceServesRosterDefaultsAndSignals(t *testing.T) {
	root := t.TempDir()
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	preview := selectManagedProfile(t, owner, helperAgentProfileSource())
	ix, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(ix, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = ix.Close() })
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"managed-fixture": managedFixtureDriver{}}
	t.Cleanup(func() { chatDrivers = original })
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, host, owner)

	body, _ := json.Marshal(map[string]any{"profile_id": preview.ProfileID,
		"profile_source_digest": preview.SourceDigest, "profile_bundle_digest": preview.BundleDigest,
		"project_root": root, "runtime": "managed-fixture", "mode": "",
		"granted_authority": []string{"reply"}, "auto_action": false,
		"priority": 7, "declared_tags": []string{"needs-review"},
		"limits":               map[string]any{"max_total": 3, "loop_budget": 2},
		"expected_state_token": store.ManagedBindingAbsentToken("agent-x"), "confirmed": true})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/orchestration/agents/agent-x", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("agent put status=%d body=%s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/agents", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("agents status=%d body=%s", response.Code, response.Body.String())
	}
	var roster struct {
		Agents []struct {
			BindingID     string              `json:"binding_id"`
			Role          string              `json:"role"`
			Priority      int64               `json:"priority"`
			DeclaredTags  []string            `json:"declared_tags"`
			Limits        store.ManagedLimits `json:"limits"`
			State         string              `json:"state"`
			ProfileID     string              `json:"profile_id"`
			ProfileName   string              `json:"profile_name"`
			ProfileDesc   string              `json:"profile_description"`
			PromptExcerpt string              `json:"prompt_excerpt"`
			StateToken    string              `json:"state_token"`
		} `json:"agents"`
		Defaults map[string]any `json:"defaults"`
		Signals  []struct {
			Kind     string `json:"kind"`
			Terminal bool   `json:"terminal"`
		} `json:"signals"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &roster); err != nil {
		t.Fatal(err)
	}
	if len(roster.Agents) != 1 {
		t.Fatalf("agents=%+v", roster.Agents)
	}
	agent := roster.Agents[0]
	if agent.BindingID != "agent-x" || agent.Role != "helper" || agent.Priority != 7 ||
		len(agent.DeclaredTags) != 1 || agent.DeclaredTags[0] != "needs-review" ||
		agent.Limits.MaxTotal != 3 || agent.Limits.LoopBudget != 2 || agent.State != "enabled" ||
		agent.ProfileName != "Design helper" || agent.PromptExcerpt == "" {
		t.Fatalf("agent=%+v", agent)
	}
	// Defaults are the resolved shipped configuration, not frozen literals.
	if roster.Defaults["max_total"] != float64(shippedAgentDefaults.MaxTotal) ||
		roster.Defaults["loop_budget"] != float64(shippedAgentDefaults.LoopBudget) {
		t.Fatalf("defaults=%+v", roster.Defaults)
	}
	sawTerminal := false
	for _, signal := range roster.Signals {
		sawTerminal = sawTerminal || (signal.Kind == "task.completed" && signal.Terminal)
	}
	if !sawTerminal {
		t.Fatalf("signals=%+v", roster.Signals)
	}

	disableBody := []byte(`{"expected_state_token":"` + agent.StateToken + `","confirmed":true}`)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/orchestration/agents/agent-x/disable", bytes.NewReader(disableBody)))
	if response.Code != http.StatusOK {
		t.Fatalf("agent disable status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGroupNotesAndTagsHTTP(t *testing.T) {
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
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, host, owner)

	binding, err := ix.PutManagedBinding(store.ManagedBinding{BindingID: "agent-n", State: "enabled",
		Role: "helper", ProjectRoot: root, ProfileID: "design-helper", ProfileSourceDigest: "sha256-v1:s",
		ProfileBundleDigest: "sha256-v1:b", Runtime: "managed-fixture", Mode: "", Authority: []string{},
		AllowedProfiles: []store.ManagedProfileRef{}}, store.ManagedBindingAbsentToken("agent-n"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_http", BindingID: binding.BindingID, State: "active",
		RootTaskID: "task_root", RootRuntime: "managed-fixture", RootCatalogSessionID: "sess-http",
		ProjectRoot: root, CreatedAt: 1, UpdatedAt: 1}
	run := store.ManagedRun{RunID: "orun_http", IdempotencyKey: "idem_http", GroupID: group.GroupID,
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "helper",
		ProfileID: "design-helper", ProfileSourceDigest: "sha256-v1:s", ProfileBundleDigest: "sha256-v1:b",
		SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}

	// Notes: create, list, retract, and refuse a second retract.
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/orchestration/groups/org_http/notes",
		strings.NewReader(`{"body":"Prefer ADR 0028."}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("note post status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/orchestration/groups/org_missing/notes",
		strings.NewReader(`{"body":"lost"}`)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing group note status=%d", response.Code)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/groups/org_http/notes", nil))
	var listed struct {
		Notes []store.ManagedGroupNote `json:"notes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Notes) != 1 {
		t.Fatalf("notes=%+v err=%v", listed, err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/orchestration/notes/"+listed.Notes[0].NoteID+"/retract",
		strings.NewReader(`{}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("retract status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/orchestration/notes/"+listed.Notes[0].NoteID+"/retract",
		strings.NewReader(`{}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("second retract status=%d", response.Code)
	}

	// Tags: session-scoped active read; session_id is required.
	if err := ix.PutOrchestrationTags([]store.OrchestrationTag{{TagID: "otag_http", RunID: run.RunID,
		BindingID: binding.BindingID, AgentKey: "agent:agent-n:needs-review", Tag: "needs-review",
		Runtime: "managed-fixture", SessionID: "sess-http", Anchor: "1", AppliedAt: 5}}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/tags?session_id=sess-http", nil))
	var tags struct {
		Tags []store.OrchestrationTag `json:"tags"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &tags); err != nil || len(tags.Tags) != 1 ||
		tags.Tags[0].AgentKey != "agent:agent-n:needs-review" {
		t.Fatalf("tags=%+v err=%v", tags, err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/tags", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("tags without session status=%d", response.Code)
	}
}

// G-4/g4-plan coverage: the settings payload publishes creation CAS tokens
// only for UNBOUND follower/helper-compatible profiles; the managed
// projection matches any of the repeated session identities against catalog
// OR native root ids (empty values never widen the filter); the tags read
// merges identity alternates and 400s when all are empty.
func TestAgentsHTTPAbsentTokensAndMultiIdentityFilters(t *testing.T) {
	root := t.TempDir()
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	preview := selectManagedProfile(t, owner, helperAgentProfileSource())
	ix, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(ix, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = ix.Close() })
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"managed-fixture": managedFixtureDriver{}}
	t.Cleanup(func() { chatDrivers = original })
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, host, owner)

	readSettings := func() map[string]string {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/managed/settings", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("settings status=%d body=%s", response.Code, response.Body.String())
		}
		var settings struct {
			AbsentStateTokens map[string]string `json:"absent_state_tokens"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
			t.Fatal(err)
		}
		return settings.AbsentStateTokens
	}
	candidateID := "agent-" + preview.ProfileID
	absent := readSettings()
	if absent[candidateID] != store.ManagedBindingAbsentToken(candidateID) {
		t.Fatalf("unbound compatible profile lost its creation token: %+v", absent)
	}

	body, _ := json.Marshal(map[string]any{"profile_id": preview.ProfileID,
		"profile_source_digest": preview.SourceDigest, "profile_bundle_digest": preview.BundleDigest,
		"project_root": root, "runtime": "managed-fixture", "mode": "",
		"granted_authority": []string{}, "auto_action": false,
		"expected_state_token": absent[candidateID], "confirmed": true})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/orchestration/agents/"+candidateID, bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("create through absent token status=%d body=%s", response.Code, response.Body.String())
	}
	if _, still := readSettings()[candidateID]; still {
		t.Fatal("a BOUND binding id must never carry an absent creation token (silent-rebind hole)")
	}

	// One group recorded under two root identities; only exact matches hit.
	binding, _, err := ix.ManagedBinding(candidateID)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_multi", BindingID: candidateID, State: "active",
		RootTaskID: "task_root", RootRuntime: "managed-fixture", RootCatalogSessionID: "cat-1",
		RootNativeSessionID: "thread-1", ProjectRoot: root, CreatedAt: 1, UpdatedAt: 1}
	run := store.ManagedRun{RunID: "orun_multi", IdempotencyKey: "idem_multi", GroupID: group.GroupID,
		BindingID: candidateID, BindingStateToken: binding.StateToken, Role: "helper",
		ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest,
		ProfileBundleDigest: preview.BundleDigest, SourceTaskID: "task_root", SourceEventID: 1,
		AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	countGroups := func(query string) int {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/managed"+query, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("projection %s status=%d body=%s", query, response.Code, response.Body.String())
		}
		var projection struct {
			Groups []store.ManagedGroup `json:"groups"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
			t.Fatal(err)
		}
		return len(projection.Groups)
	}
	if countGroups("?session=thread-1") != 1 || countGroups("?session=cat-1") != 1 {
		t.Fatal("single-identity matches must keep working under either root id")
	}
	if countGroups("?session=stem-9&session=thread-1") != 1 {
		t.Fatal("any repeated identity alternate must match the group")
	}
	if countGroups("?session=stem-9&session=meta-9") != 0 {
		t.Fatal("non-matching identities must not leak groups")
	}
	if countGroups("?runtime=managed-fixture&session=&session=+") != 1 {
		t.Fatal("empty session values are ignored — runtime-only filter, never a mismatch")
	}

	// Tags under the native identity merge with (deduped against) the
	// catalog identity read; all-empty session_id stays a 400.
	if err := ix.PutOrchestrationTags([]store.OrchestrationTag{{TagID: "otag_multi", RunID: run.RunID,
		BindingID: candidateID, AgentKey: "agent:" + candidateID + ":needs-review", Tag: "needs-review",
		Runtime: "managed-fixture", SessionID: "thread-1", Anchor: "1", AppliedAt: 5}}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/orchestration/tags?session_id=cat-1&session_id=thread-1&session_id=thread-1", nil))
	var tags struct {
		Tags []store.OrchestrationTag `json:"tags"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &tags); err != nil || len(tags.Tags) != 1 {
		t.Fatalf("multi-identity tag read=%+v err=%v", tags, err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/tags?session_id=&session_id=+", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("all-empty session_id status=%d", response.Code)
	}
}
