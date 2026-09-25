package daemon

import (
	"encoding/json"
	"testing"
)

// The exact transport strings measured during the 2026-08-30 Ollama-cloud
// outage — the incident this classification exists to make honest.
const (
	measuredOllamaQuota = "AI_APICallError: you (dreamy_hypatia_601) have reached your session usage limit, " +
		"upgrade for higher limits: https://ollama.com/upgrade or add extra usage: https://ollama.com/settings"
	measuredOllamaGateway = "AI_APICallError: Bad Gateway"
)

func TestSharedProviderErrorClassification(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{measuredOllamaQuota, providerErrorQuota},
		{measuredOllamaGateway, providerErrorUnavailable},
		{"429 Too Many Requests", providerErrorQuota},
		{"rate limit exceeded, retry later", providerErrorQuota},
		{"connect ECONNREFUSED 127.0.0.1:11434", providerErrorUnavailable},
		{"request to https://api.example.com timed out", providerErrorUnavailable},
		{"dial tcp: lookup api.example.com: no such host", providerErrorUnavailable},
		{"failed to authenticate", providerErrorAuth},
		// Not positively classifiable → "" → today's plain failure (R3's
		// fail-safe direction: a miss can only degrade to current behavior).
		{"model produced malformed output", ""},
		{"exit status 1", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := sharedProviderErrorClass(tc.line); got != tc.want {
			t.Errorf("class(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// TestDriverErrorEventsCarryProviderClass pins the vendor edge: the typed
// error channel of each driver stamps the class; auth failures keep their
// existing auth_required shape (no regression).
func TestDriverErrorEventsCarryProviderClass(t *testing.T) {
	openCode := openCodeChatDriver{}.ProjectEvent(map[string]any{
		"type": "error", "message": measuredOllamaQuota})
	assertOneErrorClass(t, "opencode quota", openCode, providerErrorQuota)

	openCodeGateway := openCodeChatDriver{}.ProjectEvent(map[string]any{
		"type": "error", "message": measuredOllamaGateway})
	assertOneErrorClass(t, "opencode gateway", openCodeGateway, providerErrorUnavailable)

	codex := codexChatDriver{}.ProjectEvent(map[string]any{
		"type": "error", "message": "rate limit exceeded"})
	assertOneErrorClass(t, "codex quota", codex, providerErrorQuota)

	auth := openCodeChatDriver{}.ProjectEvent(map[string]any{
		"type": "error", "message": "failed to authenticate"})
	if len(auth) != 1 || auth[0]["type"] != "auth_required" {
		t.Fatalf("auth failure lost its auth_required shape: %v", auth)
	}

	plain := openCodeChatDriver{}.ProjectEvent(map[string]any{
		"type": "error", "message": "model produced malformed output"})
	if len(plain) != 1 || plain[0]["error_class"] != nil {
		t.Fatalf("unclassifiable error gained a class: %v", plain)
	}
}

// TestModelContentNeverClassifies pins R3: content a MODEL can influence —
// assistant text, tool results — must never produce a provider class, even
// when it quotes a quota wall verbatim. Otherwise a session that PRINTS the
// magic words steers parking, breakers, and reroutes.
func TestModelContentNeverClassifies(t *testing.T) {
	poisoned := []map[string]any{
		{"type": "text", "part": map[string]any{"text": measuredOllamaQuota}},
		{"type": "tool_result", "part": map[string]any{"text": measuredOllamaQuota, "is_error": true}},
		{"type": "tool", "part": map[string]any{"tool": "bash",
			"state": map[string]any{"input": map[string]any{"command": "true"}, "output": measuredOllamaQuota}}},
	}
	for _, obj := range poisoned {
		for _, event := range (openCodeChatDriver{}).ProjectEvent(obj) {
			if event["error_class"] != nil {
				encoded, _ := json.Marshal(event)
				t.Fatalf("model-influenceable content classified as provider failure: %s", encoded)
			}
		}
	}
}

func assertOneErrorClass(t *testing.T, label string, events []ChatEvent, want string) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("%s: events = %v", label, events)
	}
	event := events[0]
	if event["type"] != "error" || event["error_class"] != want {
		t.Fatalf("%s: event %v missing error_class %q", label, event, want)
	}
	if text, _ := event["text"].(string); text == "" {
		t.Fatalf("%s: recovery text dropped: %v", label, event)
	}
}
