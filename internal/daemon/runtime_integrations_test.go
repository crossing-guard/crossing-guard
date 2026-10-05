package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
)

func runtimeIntegrationHarness(t *testing.T) (http.Handler, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "cursor"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	oldExecutable := runtimeIntegrationExecutable
	oldPreviews := runtimeIntegrationPreviews
	runtimeIntegrationExecutable = func() (string, error) { return "/opt/crossing-guard", nil }
	runtimeIntegrationPreviews = newRuntimeIntegrationPreviewStore()
	t.Cleanup(func() {
		runtimeIntegrationExecutable = oldExecutable
		runtimeIntegrationPreviews = oldPreviews
	})
	mux := http.NewServeMux()
	registerRuntimeIntegrationRoutes(mux)
	return securityMiddleware(mux, "127.0.0.1:3210", "test-token", filepath.Join(home, "api-token")), home
}

func runtimeIntegrationRequest(handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1:3210"+path, bytes.NewReader(raw))
	request.Header.Set("X-CG-Token", "test-token")
	request.Header.Set("Origin", "http://127.0.0.1:3210")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeRuntimeIntegrationBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: status=%d body=%q err=%v", recorder.Code, recorder.Body.String(), err)
	}
	return body
}

func TestRuntimeIntegrationPreviewConnectDisconnectThroughAuthenticatedAPI(t *testing.T) {
	handler, home := runtimeIntegrationHarness(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := []byte(`{"version":1,"foreign_secret":"do-not-return","hooks":{"workspaceOpen":[{"command":"/private/foreign-command"}]}}`)
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	list := runtimeIntegrationRequest(handler, http.MethodGet, "/api/runtime-integrations", nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "do-not-return") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	previewRecorder := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/preview-connect", nil)
	if previewRecorder.Code != http.StatusOK || strings.Contains(previewRecorder.Body.String(), "do-not-return") ||
		strings.Contains(previewRecorder.Body.String(), "/private/foreign-command") {
		t.Fatalf("preview status=%d body=%s", previewRecorder.Code, previewRecorder.Body.String())
	}
	preview := decodeRuntimeIntegrationBody(t, previewRecorder)
	token, _ := preview["preview_token"].(string)
	if token == "" {
		t.Fatalf("preview token missing: %v", preview)
	}
	if got, _ := os.ReadFile(path); string(got) != string(foreign) {
		t.Fatalf("preview changed config: %s", got)
	}

	connected := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/connect", map[string]any{"preview_token": token, "confirmed": true})
	if connected.Code != http.StatusOK {
		t.Fatalf("connect status=%d body=%s", connected.Code, connected.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "--runtime cursor") || !strings.Contains(string(raw), "foreign_secret") {
		t.Fatalf("connect did not preserve/attach: %s", raw)
	}
	if replay := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/connect", map[string]any{"preview_token": token, "confirmed": true}); replay.Code != http.StatusConflict {
		t.Fatalf("single-use token replay status=%d body=%s", replay.Code, replay.Body.String())
	}

	disconnectPreviewRecorder := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/preview-disconnect", nil)
	if disconnectPreviewRecorder.Code != http.StatusOK {
		t.Fatalf("disconnect preview status=%d body=%s", disconnectPreviewRecorder.Code, disconnectPreviewRecorder.Body.String())
	}
	disconnectPreview := decodeRuntimeIntegrationBody(t, disconnectPreviewRecorder)
	disconnectToken, _ := disconnectPreview["preview_token"].(string)
	disconnected := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/disconnect", map[string]any{"preview_token": disconnectToken, "confirmed": true})
	if disconnected.Code != http.StatusOK {
		t.Fatalf("disconnect status=%d body=%s", disconnected.Code, disconnected.Body.String())
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "crossing-guard") || !strings.Contains(string(raw), "foreign_secret") {
		t.Fatalf("disconnect did not preserve/remove surgically: %s", raw)
	}
}

func TestRuntimeIntegrationSecurityConfirmationAndStalePreview(t *testing.T) {
	handler, home := runtimeIntegrationHarness(t)
	unauthorizedRequest := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:3210/api/runtime-integrations/cursor/preview-connect", nil)
	unauthorizedRequest.Header.Set("Origin", "https://malicious.example")
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); !os.IsNotExist(err) {
		t.Fatalf("unauthorized preview created Cursor state: %v", err)
	}

	previewRecorder := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/preview-connect", nil)
	preview := decodeRuntimeIntegrationBody(t, previewRecorder)
	token, _ := preview["preview_token"].(string)
	missingConfirmation := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/connect", map[string]any{"preview_token": token, "confirmed": false})
	if missingConfirmation.Code != http.StatusBadRequest {
		t.Fatalf("missing confirmation status=%d body=%s", missingConfirmation.Code, missingConfirmation.Body.String())
	}
	// A rejected request never consumed the token because it did not express consent.
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	changed := []byte(`{"version":1,"foreign":"changed-after-preview"}`)
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/connect", map[string]any{"preview_token": token, "confirmed": true})
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "preview_changed") {
		t.Fatalf("stale preview status=%d body=%s", stale.Code, stale.Body.String())
	}
	if got, _ := os.ReadFile(path); string(got) != string(changed) {
		t.Fatalf("stale preview overwrote config: %s", got)
	}
}

func TestRuntimeIntegrationPreviewTokensExpireAndBindOperation(t *testing.T) {
	store := newRuntimeIntegrationPreviewStore()
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	token, _, err := store.issue("cursor", "connect", "digest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.consume(token, "cursor", "disconnect"); err == nil {
		t.Fatal("operation-mismatched token was accepted")
	}
	token, _, err = store.issue("cursor", "connect", "digest")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(runtimeIntegrationPreviewTTL)
	if _, err := store.consume(token, "cursor", "connect"); err == nil {
		t.Fatal("expired token was accepted")
	}
}

func TestRuntimeIntegrationFrontendIsProviderNeutralAndExplicit(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	client := read("js/runtime-integrations.js")
	settings := read("js/views/settings-runtimes.js")
	for _, required := range []string{"/api/runtime-integrations", "preview-", "preview_token", "confirmed: true", "/watch", "/confirm-visible", "JSON.parse", "payload.message.trim()"} {
		if !bytes.Contains(client, []byte(required)) {
			t.Errorf("runtime connection client lost %q", required)
		}
	}
	for _, forbidden := range []string{"config_path:", "hook_binary:", "cursor", "launch"} {
		if bytes.Contains(client, []byte(forbidden)) {
			t.Errorf("runtime connection client gained provider/path behavior %q", forbidden)
		}
	}
	for _, required := range []string{"runtime-integrations.js", "Preview connection", "Preview disconnect", "Connect ", "commitRuntimeIntegration", "Start watching", "I personally saw ", "will_create_file", "Hook phases:", "durable connection consent", "not saved as provider verification", "Refresh current status", "runtimeMutationFailureMessage", "integration.consented &&", "aria-live", "refreshRuntimeConnections(host, result.note", "confirm-visible')?.remove()"} {
		if !bytes.Contains(settings, []byte(required)) {
			t.Errorf("Settings connection flow lost %q", required)
		}
	}
	for _, forbidden := range []string{"runtime === 'cursor'", "runtime == 'cursor'", "if (integration.runtime", "open(", "osascript"} {
		if bytes.Contains(settings, []byte(forbidden)) {
			t.Errorf("Settings gained provider launch/branch behavior %q", forbidden)
		}
	}
	for _, name := range []string{
		"runtime_integrations.go", "runtime_integration_watch.go",
		"../guardcli/runtime_connection.go", "../guardcli/runtime_connection_mutation.go",
	} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte(`"Cursor`)) {
			t.Errorf("provider-neutral connection owner %s contains provider-specific display copy", name)
		}
	}
}

func TestRuntimeIntegrationWatchUsesExactEventCursorAndOwnerConfirmation(t *testing.T) {
	handler, home := runtimeIntegrationHarness(t)
	executable := filepath.Join(home, "bin", "crossing-guard")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeIntegrationExecutable = func() (string, error) { return executable, nil }
	configPath := filepath.Join(home, ".cursor", "hooks.json")
	hooks := map[string]any{}
	for _, event := range []string{"sessionStart", "preToolUse", "postToolUse", "postToolUseFailure", "sessionEnd"} {
		hooks[event] = []any{map[string]any{"command": `"` + executable + `" hook --runtime cursor`}}
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "hooks": hooks})
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	unconsentedWatch := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/watch", map[string]any{"surface": "agent-chat"})
	if unconsentedWatch.Code != http.StatusConflict ||
		!strings.Contains(unconsentedWatch.Body.String(), "explicit connection consent") {
		t.Fatalf("unconsented watch status=%d body=%s", unconsentedWatch.Code, unconsentedWatch.Body.String())
	}
	if err := guardcli.RecordConsent("cursor", configPath, executable, false); err != nil {
		t.Fatal(err)
	}
	index, err := store.Open(filepath.Join(home, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	oldGovernor := governor
	governor = NewGovernor(index, nil) // a real governor: the final canary arrives through ingest
	t.Cleanup(func() { governor = oldGovernor })

	start := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/watch", map[string]any{"surface": "agent-chat"})
	if start.Code != http.StatusOK {
		t.Fatalf("watch start status=%d body=%s", start.Code, start.Body.String())
	}
	startBody := decodeRuntimeIntegrationBody(t, start)
	token, _ := startBody["watch_token"].(string)
	if token == "" || startBody["surface_evidence"] == "native" {
		t.Fatalf("watch start = %v", startBody)
	}
	waiting := runtimeIntegrationRequest(handler, http.MethodGet,
		"/api/runtime-integrations/cursor/watch/"+token, nil)
	if waiting.Code != http.StatusOK || !strings.Contains(waiting.Body.String(), `"status":"waiting"`) {
		t.Fatalf("waiting status=%d body=%s", waiting.Code, waiting.Body.String())
	}

	tx, err := index.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	// A deny by some OTHER rule proves the hook fires, and must not count as the proof
	// rule blocking: only rule_id identifies that, because no command text is ever
	// frozen onto an event.
	if _, err := tx.AppendEvent(store.EventRecord{TS: 200, SessionID: "cursor-natural",
		Runtime: "cursor", Verb: "exec", Tool: "Shell", Decision: "deny", Reason: "another rule",
		Origin: "live", Tags: `[]`, RuleID: "destructive-rm"}, nil); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	observed := runtimeIntegrationRequest(handler, http.MethodGet,
		"/api/runtime-integrations/cursor/watch/"+token, nil)
	if observed.Code != http.StatusOK || !strings.Contains(observed.Body.String(), `"status":"decision_firing_observed"`) {
		t.Fatalf("near-match status=%d body=%s", observed.Code, observed.Body.String())
	}

	tx, err = index.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	for eventIndex := 0; eventIndex < 101; eventIndex++ {
		if _, err := tx.AppendEvent(store.EventRecord{TS: int64(201 + eventIndex), SessionID: "cursor-noise",
			Runtime: "cursor", Verb: "read", Tool: "Read", Decision: "allow", Origin: "live", Tags: `[]`}, nil); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The canary itself takes the production path: hook envelope → ingest → store, so this
	// test cannot pass by seeding a shape the hook never produces.
	canary := testV1Envelope(t, t.TempDir())
	canary.ObservationID, canary.ActionID = "obs_feedfacefeedfacefeedfacefeedfa02", "act_feedfacefeedfacefeedfacefeedfa02"
	canary.SessionID, canary.Runtime, canary.Tool = "cursor-natural", "cursor", "Shell"
	canary.Decision, canary.Reason, canary.Rule = "deny", "proof rule", rulebook.CanaryRuleID
	canary.TS = 302
	if _, err := ingestObservationV1(t.Context(), governor, canary); err != nil {
		t.Fatal(err)
	}
	tx, err = index.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	paged := runtimeIntegrationRequest(handler, http.MethodGet,
		"/api/runtime-integrations/cursor/watch/"+token, nil)
	if paged.Code != http.StatusOK || !strings.Contains(paged.Body.String(), `"status":"decision_firing_observed"`) {
		t.Fatalf("paged watch status=%d body=%s", paged.Code, paged.Body.String())
	}
	denied := runtimeIntegrationRequest(handler, http.MethodGet,
		"/api/runtime-integrations/cursor/watch/"+token, nil)
	if denied.Code != http.StatusOK || !strings.Contains(denied.Body.String(), `"status":"denial_returned"`) ||
		strings.Contains(denied.Body.String(), rulebook.CanaryMarker) {
		t.Fatalf("denial status=%d body=%s", denied.Code, denied.Body.String())
	}
	confirmed := runtimeIntegrationRequest(handler, http.MethodPost,
		"/api/runtime-integrations/cursor/watch/"+token+"/confirm-visible", nil)
	if confirmed.Code != http.StatusOK || !strings.Contains(confirmed.Body.String(), `"status":"enforcement_verified"`) ||
		!strings.Contains(confirmed.Body.String(), `"visible_blocking":"owner-confirmed"`) {
		t.Fatalf("confirmation status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
}
