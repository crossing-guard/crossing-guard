package infer

import (
	"encoding/json"
	"testing"
)

func TestLocalOllamaPathIsLiteralLoopbackAndDigestPinned(t *testing.T) {
	path, err := NewLocalOllamaPath("http://127.0.0.1:11434/", "qwen:test", 1024, 512, 128)
	if err != nil {
		t.Fatal(err)
	}
	if path.Endpoint != "http://127.0.0.1:11434" || path.Digest == "" || path.FallbackPolicy != "disabled" {
		t.Fatalf("path = %+v", path)
	}
	other, err := NewLocalOllamaPath("http://127.0.0.1:11434", "qwen:other", 1024, 512, 128)
	if err != nil || other.Digest == path.Digest {
		t.Fatalf("request path identity did not pin model: %+v err=%v", other, err)
	}
	for _, endpoint := range []string{
		"https://127.0.0.1:11434", "http://localhost:11434", "http://0.0.0.0:11434",
		"http://127.0.0.1", "http://127.0.0.1:11434/api", "http://user@127.0.0.1:11434",
		"http://127.0.0.1:11434?x=1", "http://example.com:11434",
	} {
		if _, err := NewLocalOllamaPath(endpoint, "model", 1, 1, 1); err == nil {
			t.Errorf("accepted unsafe endpoint %q", endpoint)
		}
	}
}

func TestValidateRequestRejectsTamperingAndBounds(t *testing.T) {
	path, err := NewLocalOllamaPath("http://[::1]:11434", "model", 64, 64, 8)
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object"}`)
	valid := Request{Path: path, Messages: []Message{{Role: "system", Content: "review"}}, Schema: schema}
	if err := ValidateRequest(valid, "ollama"); err != nil {
		t.Fatal(err)
	}
	tampered := valid
	tampered.Path.Endpoint = "http://127.0.0.1:11434"
	if err := ValidateRequest(tampered, "ollama"); err == nil {
		t.Fatal("accepted request path changed after digest")
	}
	tooLarge := valid
	tooLarge.Messages = []Message{{Role: "user", Content: string(make([]byte, 65))}}
	if err := ValidateRequest(tooLarge, "ollama"); err == nil {
		t.Fatal("accepted oversized messages")
	}
}

func TestOllamaBackendIsRegistered(t *testing.T) {
	backend, ok := Lookup("ollama")
	if !ok || backend.Name() != "ollama" {
		t.Fatalf("backend = %v found=%v", backend, ok)
	}
}
