package infer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testRequest(t *testing.T, endpoint string) Request {
	t.Helper()
	path, err := NewLocalOllamaPath(endpoint, "fixture-model", 4096, 1024, 64)
	if err != nil {
		t.Fatal(err)
	}
	return Request{Path: path, Messages: []Message{{Role: "system", Content: "review"}, {Role: "user", Content: "data"}},
		Schema: json.RawMessage(`{"type":"object","properties":{"decision":{"type":"string"}}}`)}
}

func TestOllamaBackendSendsStructuredNonStreamingRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "fixture-model" || body["stream"] != false || body["format"] == nil {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"fixture-model","message":{"role":"assistant","content":"{\"decision\":\"abstain\"}"},"prompt_eval_count":12,"eval_count":3}`))
	}))
	defer server.Close()
	response, err := (ollamaBackend{}).Run(context.Background(), testRequest(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Content) != `{"decision":"abstain"}` || response.PromptTokens != 12 || response.CompletionTokens != 3 {
		t.Fatalf("response = %+v", response)
	}
}

func TestOllamaBackendRefusesRedirectMalformedAndDeadline(t *testing.T) {
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"content":"{}"}}`))
	}))
	defer redirectTarget.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := (ollamaBackend{}).Run(context.Background(), testRequest(t, redirect.URL)); Kind(err) != ErrorUnavailable {
		t.Fatalf("redirect error = %v kind=%s", err, Kind(err))
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"content":"` + strings.Repeat("x", 1025) + `"}}`))
	}))
	defer malformed.Close()
	if _, err := (ollamaBackend{}).Run(context.Background(), testRequest(t, malformed.URL)); Kind(err) != ErrorMalformed {
		t.Fatalf("malformed error = %v kind=%s", err, Kind(err))
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"message":{"content":"{}"}}`))
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := (ollamaBackend{}).Run(ctx, testRequest(t, slow.URL)); Kind(err) != ErrorTimeout {
		t.Fatalf("deadline error = %v kind=%s", err, Kind(err))
	}
}
