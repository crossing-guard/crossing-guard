package memcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This provider deliberately implements the contract without a native adapter.
type independentMemoryAdapter struct{ name string }

func (a independentMemoryAdapter) Name() string                      { return a.name }
func (independentMemoryAdapter) ConfigFlag() string                  { return "config" }
func (independentMemoryAdapter) DefaultConfigPath() string           { return "" }
func (independentMemoryAdapter) Attach(string, string) (bool, error) { return false, nil }
func (independentMemoryAdapter) Detach(string, string) (bool, error) { return false, nil }
func (independentMemoryAdapter) InjectedText([]byte) (string, bool)  { return "", false }
func (independentMemoryAdapter) MemoryHookBinary(string) string      { return "" }

type independentMemoryEncoder struct {
	independentMemoryAdapter
	match bool
}

func (a independentMemoryEncoder) MatchesLegacyMemoryHook([]byte) bool { return a.match }
func (independentMemoryEncoder) EncodeMemoryIndex(block string) ([]byte, error) {
	return json.Marshal(map[string]string{"different_context": block})
}

func TestMemoryIndexOutputRegistryAndAmbiguity(t *testing.T) {
	previous := adapters
	adapters = map[string]Adapter{}
	t.Cleanup(func() { adapters = previous })
	registerAdapter(independentMemoryAdapter{name: "no-output"})
	registerAdapter(independentMemoryEncoder{independentMemoryAdapter{"alpha"}, true})
	for _, name := range []string{"", "unknown", "no-output"} {
		if _, err := memoryIndexOutput(name, true, nil); err == nil {
			t.Fatalf("explicit unsupported runtime %q accepted", name)
		}
	}
	encoder, err := memoryIndexOutput("alpha", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	const block = "context π\n"
	out, err := encoder.EncodeMemoryIndex(block)
	if err != nil || string(out) != `{"different_context":"context π\n"}` {
		t.Fatalf("independent envelope = %s, %v", out, err)
	}
	if _, err := memoryIndexOutput("", false, []byte("legacy")); err != nil {
		t.Fatal(err)
	}
	registerAdapter(independentMemoryEncoder{independentMemoryAdapter{"beta"}, true})
	if _, err := memoryIndexOutput("", false, nil); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("multiple recognizers must refuse: %v", err)
	}
	if _, err := memoryIndexOutput("alpha", true, nil); err != nil {
		t.Fatalf("explicit selection must override ambiguity: %v", err)
	}
}

func TestMemoryIndexLegacyRecognitionAndExactNativeContext(t *testing.T) {
	for _, tc := range []struct {
		payload string
		match   bool
	}{
		{`{"hook_event_name":"SessionStart","transcript_path":"/home/.codex/sessions/a"}`, true},
		{`{"hook_event_name":"OtherEvent","transcript_path":"/home/.codex/sessions/a"}`, true},
		{`{"hook_event_name":"SessionStart","transcript_path":"/custom/sessions/a"}`, false},
		{`{"transcript_path":"/home/.codex/sessions/a"}`, false},
		{`{"hook_event_name":"SessionStart","transcript_path":"/home/.codex/a","cwd":123}`, false},
		{"{broken", false},
		{"", false},
	} {
		encoder, err := memoryIndexOutput("", false, []byte(tc.payload))
		if err != nil || (encoder != nil) != tc.match {
			t.Fatalf("legacy routing %q = %v, %v", tc.payload, encoder, err)
		}
	}
	const block = "[crossing-guard-memory-beacon date nonce]\nπ\n"
	for _, runtime := range []string{"claude", "codex"} {
		encoder, err := memoryIndexOutput(runtime, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := encoder.EncodeMemoryIndex(block)
		if err != nil {
			t.Fatal(err)
		}
		if runtime == "claude" {
			if string(out) != block {
				t.Fatalf("plaintext drift: %q", out)
			}
			continue
		}
		var envelope struct {
			Hook struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out, &envelope); err != nil || envelope.Hook.Event != "SessionStart" || envelope.Hook.Context != block {
			t.Fatalf("native envelope drift: %s, %v", out, err)
		}
	}
}

func TestMemoryIndexExplicitDoesNotReadOpenStdinAndLogsOnce(t *testing.T) {
	reader := &fakeDaemonReader{answers: map[string]any{"/api/memory/records": map[string]any{"records": []any{}}}}
	withDaemon(t, reader)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = oldStdin; _ = input.Close(); _ = writer.Close() })
	stdout, stderr := captureCLIOutput(t, func() { memIndex([]string{"--runtime", "codex", "--project", "test"}) })
	if stderr != "" || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, "memory-beacon") {
		t.Fatalf("explicit open-pipe command failed: %q, %q", stdout, stderr)
	}
	if len(dataCalls(reader)) != 1 || len(loggedNonces(memoryDir())) != 1 {
		t.Fatalf("expected one read/emission: %v", reader.calls)
	}
}

func TestMemoryIndexLegacyInputBoundAndTerminal(t *testing.T) {
	oldStdin := os.Stdin
	t.Cleanup(func() { os.Stdin = oldStdin })
	input := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(input, []byte(strings.Repeat("x", 70<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	os.Stdin = f
	if raw := readHookPayload(); len(raw) != 64<<10 {
		t.Fatalf("payload bound = %d", len(raw))
	} else if encoder, err := memoryIndexOutput("", false, raw); err != nil || encoder != nil {
		t.Fatalf("oversized malformed input must retain plaintext fallback: %v, %v", encoder, err)
	}
	terminal, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	os.Stdin = terminal
	if raw := readHookPayload(); raw != nil {
		t.Fatalf("character device must not be read: %q", raw)
	}
}

func TestMemoryIndexUnavailableRetainsNativeDiagnostic(t *testing.T) {
	withDaemon(t, &fakeDaemonReader{err: errors.New("unavailable")})
	stdout, stderr := captureCLIOutput(t, func() { memIndex([]string{"--runtime", "codex"}) })
	if stderr != "" || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, "memory store unavailable") || strings.Contains(stdout, "memory-beacon") {
		t.Fatalf("unavailable-store output = %q, %q", stdout, stderr)
	}
	if len(loggedNonces(memoryDir())) != 0 {
		t.Fatal("unavailable store minted nonce")
	}
}

func TestMemoryIndexEmissionNonceRequiresInjectedPosition(t *testing.T) {
	withDaemon(t, &fakeDaemonReader{answers: map[string]any{"/api/memory/records": map[string]any{"records": []any{}}}})
	stdout, _ := captureCLIOutput(t, func() { memIndex([]string{"--runtime", "codex"}) })
	var out struct {
		Hook struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	nonces := loggedNonces(memoryDir())
	if len(nonces) != 1 {
		t.Fatalf("emission minted %d nonces", len(nonces))
	}
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	for _, role := range []string{"developer", "assistant"} {
		row, err := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "message", "role": role, "content": []any{map[string]any{"text": out.Hook.Context}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(row, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := sessionHasInjectedNonce(Session{Vendor: "codex", Path: path}, nonces); got != (role == "developer") {
			t.Fatalf("nonce verification accepted wrong position %s: %v", role, got)
		}
		if sessionHasInjectedNonce(Session{Vendor: "codex", Path: path}, map[string]bool{"unlogged": true}) {
			t.Fatal("unlogged nonce accepted")
		}
	}
}
