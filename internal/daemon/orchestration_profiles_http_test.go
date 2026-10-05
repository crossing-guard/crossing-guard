package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/orchestration/profilefs"
)

func orchestrationProfileHarness(t *testing.T) (http.Handler, string) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := profilefs.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerOrchestrationProfileRoutes(mux, owner, func(string) profilefs.PinSource { return nil })
	return securityMiddleware(mux, "127.0.0.1:3210", "test-token", filepath.Join(dataDir, "api-token")), dataDir
}

func profileHTTPRequest(handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
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

func validHTTPProfile(version, instruction string) []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: command-reviewer
version: "` + version + `"
name: Command reviewer
description: Reviews a command independently.
role: reviewer
execution: stateless-review
trigger:
  event: pretool.action
context:
  - kind: pretool-action
    required: true
output:
  kind: review-recommendation
authority-requests:
  - advise
requirements:
  capabilities:
    - one-shot-inference
limits:
  timeout: 30s
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: block
  unavailable-capability: block
  timeout: record-unavailable
  malformed-output: record-unavailable
---
` + instruction)
}

func decodeProfileHTTPBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response status=%d is not JSON: %q: %v", recorder.Code, recorder.Body.String(), err)
	}
	return body
}

func TestOrchestrationProfileAuthenticatedPreviewSelectListDetail(t *testing.T) {
	handler, dataDir := orchestrationProfileHarness(t)
	source := validHTTPProfile("1.0.0", "Review the command.\n")
	payload := map[string]any{"source_name": "PROFILE.md", "source_base64": base64.StdEncoding.EncodeToString(source)}
	previewRecorder := profileHTTPRequest(handler, http.MethodPost, "/api/v1/orchestration/profiles/preview", payload)
	if previewRecorder.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRecorder.Code, previewRecorder.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "orchestration")); !os.IsNotExist(err) {
		t.Fatalf("HTTP preview wrote state: %v", err)
	}
	previewBody := decodeProfileHTTPBody(t, previewRecorder)
	preview := previewBody["preview"].(map[string]any)
	if preview["runtime_effects"] != false || preview["selection_state"] != "not_selected" {
		t.Fatalf("preview = %v", preview)
	}
	selectPayload := map[string]any{"source_name": "PROFILE.md", "source_base64": payload["source_base64"],
		"source_digest": preview["source_digest"], "bundle_digest": preview["bundle_digest"],
		"state_token": preview["state_token"], "confirmed": true}
	selected := profileHTTPRequest(handler, http.MethodPost, "/api/orchestration/profiles/select", selectPayload)
	if selected.Code != http.StatusOK || !strings.Contains(selected.Body.String(), `"selection_state":"selected_inert"`) ||
		!strings.Contains(selected.Body.String(), `"runtime_effects":false`) {
		t.Fatalf("select status=%d body=%s", selected.Code, selected.Body.String())
	}
	list := profileHTTPRequest(handler, http.MethodGet, "/api/v1/orchestration/profiles", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "command-reviewer") ||
		!strings.Contains(list.Body.String(), `"selected_inert"`) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	detail := profileHTTPRequest(handler, http.MethodGet, "/api/orchestration/profiles/command-reviewer", nil)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "Review the command") ||
		!strings.Contains(detail.Body.String(), `"integrity":"verified"`) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	for _, forbidden := range []string{"bindings", "groups", "invocations", "tasks", "events", "approvals"} {
		if _, err := os.Lstat(filepath.Join(dataDir, "orchestration", forbidden)); !os.IsNotExist(err) {
			t.Fatalf("HTTP selection created deferred artifact %s: %v", forbidden, err)
		}
	}
}

func TestOrchestrationProfileRejectsUnsafeTransportWithoutEcho(t *testing.T) {
	handler, dataDir := orchestrationProfileHarness(t)
	secret := "DO-NOT-ECHO-HTTP-SECRET"
	invalidSource := bytes.Replace(validHTTPProfile("1.0.0", "Review.\n"),
		[]byte("name: Command reviewer\n"), []byte("name: Command reviewer\nunknown: "+secret+"\n"), 1)
	cases := []struct {
		name   string
		body   any
		status int
	}{
		{"bad base64", map[string]any{"source_name": "PROFILE.md", "source_base64": "%%%"}, http.StatusBadRequest},
		{"wrong name", map[string]any{"source_name": "profile.md", "source_base64": base64.StdEncoding.EncodeToString(validHTTPProfile("1.0.0", "Review.\n"))}, http.StatusBadRequest},
		{"unsafe yaml", map[string]any{"source_name": "PROFILE.md", "source_base64": base64.StdEncoding.EncodeToString(invalidSource)}, http.StatusUnprocessableEntity},
		{"unknown json", map[string]any{"source_name": "PROFILE.md", "source_base64": base64.StdEncoding.EncodeToString(validHTTPProfile("1.0.0", "Review.\n")), "prompt": secret}, http.StatusBadRequest},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			recorder := profileHTTPRequest(handler, http.MethodPost, "/api/orchestration/profiles/preview", item.body)
			if recorder.Code != item.status || strings.Contains(recorder.Body.String(), secret) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "orchestration")); !os.IsNotExist(err) {
		t.Fatalf("invalid previews wrote state: %v", err)
	}

	unconfirmed := profileHTTPRequest(handler, http.MethodPost, "/api/orchestration/profiles/select",
		map[string]any{"source_name": "PROFILE.md", "source_base64": base64.StdEncoding.EncodeToString(validHTTPProfile("1.0.0", "Review.\n")), "confirmed": false})
	if unconfirmed.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed status=%d body=%s", unconfirmed.Code, unconfirmed.Body.String())
	}

	tooLarge := profileHTTPRequest(handler, http.MethodPost, "/api/orchestration/profiles/preview",
		map[string]any{"source_name": "PROFILE.md", "source_base64": strings.Repeat("A", orchestrationProfileRequestLimit)})
	if tooLarge.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status=%d body=%s", tooLarge.Code, tooLarge.Body.String())
	}

	trailingRequest := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:3210/api/orchestration/profiles/preview", strings.NewReader(`{} {}`))
	trailingRequest.Header.Set("X-CG-Token", "test-token")
	trailingRequest.Header.Set("Origin", "http://127.0.0.1:3210")
	trailing := httptest.NewRecorder()
	handler.ServeHTTP(trailing, trailingRequest)
	if trailing.Code != http.StatusBadRequest {
		t.Fatalf("trailing request status=%d body=%s", trailing.Code, trailing.Body.String())
	}

	missing := profileHTTPRequest(handler, http.MethodGet, "/api/orchestration/profiles/not-selected", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing detail status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestOrchestrationProfileStalePreviewReturnsConflict(t *testing.T) {
	handler, _ := orchestrationProfileHarness(t)
	firstSource := validHTTPProfile("1.0.0", "First.\n")
	secondSource := validHTTPProfile("1.1.0", "Second.\n")
	preview := func(source []byte) map[string]any {
		recorder := profileHTTPRequest(handler, http.MethodPost, "/api/orchestration/profiles/preview",
			map[string]any{"source_name": "PROFILE.md", "source_base64": base64.StdEncoding.EncodeToString(source)})
		if recorder.Code != http.StatusOK {
			t.Fatalf("preview status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		return decodeProfileHTTPBody(t, recorder)["preview"].(map[string]any)
	}
	first := preview(firstSource)
	second := preview(secondSource)
	selectRequest := func(source []byte, item map[string]any) *httptest.ResponseRecorder {
		return profileHTTPRequest(handler, http.MethodPost, "/api/orchestration/profiles/select", map[string]any{
			"source_name": "PROFILE.md", "source_base64": base64.StdEncoding.EncodeToString(source),
			"source_digest": item["source_digest"], "bundle_digest": item["bundle_digest"],
			"state_token": item["state_token"], "confirmed": true})
	}
	if selected := selectRequest(firstSource, first); selected.Code != http.StatusOK {
		t.Fatalf("first select status=%d body=%s", selected.Code, selected.Body.String())
	}
	stale := selectRequest(secondSource, second)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "state_conflict") {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestOrchestrationProfileSecurityAppliesBeforePreview(t *testing.T) {
	handler, dataDir := orchestrationProfileHarness(t)
	request := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:3210/api/orchestration/profiles/preview", strings.NewReader(`{}`))
	request.Header.Set("Origin", "https://malicious.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "orchestration")); !os.IsNotExist(err) {
		t.Fatalf("unauthorized request wrote state: %v", err)
	}
}
