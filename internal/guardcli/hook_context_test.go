package guardcli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"crossing-guard/internal/observation"
)

func decodeHookOutput(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	inner, _ := out["hookSpecificOutput"].(map[string]any)
	if inner == nil {
		t.Fatalf("no hookSpecificOutput: %s", b)
	}
	return inner
}

// The installer owns the envelope, the event set, and the cap (plan A1):
// context is encoded only where the vendor documents it, never on Stop, and
// a denial stays byte-compatible with the historical envelope.
func TestInstallerEncodersOwnContextEventsAndCaps(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		encoder, ok := hookInstallers[runtime].(HookContextEncoder)
		if !ok {
			t.Fatalf("%s publishes no context encoder", runtime)
		}
		for _, event := range []string{"PreToolUse", "PostToolUse", "UserPromptSubmit"} {
			b, ok := encoder.EncodeHookContext(event, "remember this")
			if !ok {
				t.Fatalf("%s refused context at %s", runtime, event)
			}
			inner := decodeHookOutput(t, b)
			if inner["hookEventName"] != event || inner["additionalContext"] != "remember this" {
				t.Fatalf("%s %s: %v", runtime, event, inner)
			}
			if _, present := inner["permissionDecision"]; present {
				t.Fatalf("%s %s: context must never carry a decision key", runtime, event)
			}
		}
		for _, event := range []string{"Stop", "SessionEnd", "Notification", "PreCompact", "preToolUse"} {
			if _, ok := encoder.EncodeHookContext(event, "x"); ok {
				t.Fatalf("%s encoded context at %s", runtime, event)
			}
		}
		long := strings.Repeat("界", 20000)
		b, _ := encoder.EncodeHookContext("PostToolUse", long)
		if got := decodeHookOutput(t, b)["additionalContext"].(string); len(got) > 12000 || !strings.HasSuffix(got, "[truncated by the carrier's byte cap]") || !strings.HasPrefix(got, "界") {
			t.Fatalf("%s cap: len=%d suffix=%q", runtime, len(got), got[len(got)-40:])
		}
		deny := hookInstallers[runtime].(HookDecisionEncoder).EncodeHookDeny("PreToolUse", "because")
		legacy, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "because"}})
		if !bytes.Equal(deny, legacy) {
			t.Fatalf("%s deny envelope changed: %s vs %s", runtime, deny, legacy)
		}
	}
	if _, ok := hookInstallers["cursor"].(HookContextEncoder); ok {
		t.Fatal("cursor publishes a context encoder without a documented surface")
	}
	if _, ok := hookInstallers["opencode"].(HookContextEncoder); ok {
		t.Fatal("opencode delivers through its plugin, not the hook verb")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	fn()
	writer.Close()
	os.Stdout = original
	out, _ := io.ReadAll(reader)
	return string(out)
}

// Generic hook code prints exactly what the runtime's encoder returns:
// nothing without a delivery, nothing for a runtime without an encoder, and
// the raw event name (never the canonicalized one) when it does print.
func TestPrintHookContextIsSilentWithoutDeliveryOrEncoder(t *testing.T) {
	in := hookInput{Runtime: "claude", HookEventName: "PreToolUse", RawHookEventName: "PreToolUse"}
	if out := captureStdout(t, func() { printHookContext(in, nil) }); out != "" {
		t.Fatalf("printed without delivery: %q", out)
	}
	deliveries := []observation.Delivery{{DeliveryID: "odel_1", Message: "first"}, {DeliveryID: "odel_2", Message: "second"}}
	out := captureStdout(t, func() { printHookContext(in, deliveries) })
	inner := decodeHookOutput(t, []byte(strings.TrimSpace(out)))
	if inner["additionalContext"] != "first\n\nsecond" || inner["hookEventName"] != "PreToolUse" {
		t.Fatalf("%v", inner)
	}
	cursor := hookInput{Runtime: "cursor", HookEventName: "PreToolUse", RawHookEventName: "preToolUse"}
	if out := captureStdout(t, func() { printHookContext(cursor, deliveries) }); out != "" {
		t.Fatalf("runtime without encoder printed: %q", out)
	}
	if out := captureStdout(t, func() { printCollectionDeliveries(nil) }); out != "" {
		t.Fatalf("collection channel printed without delivery: %q", out)
	}
	out = captureStdout(t, func() { printCollectionDeliveries(deliveries) })
	var line map[string][]observation.Delivery
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &line); err != nil || len(line["crossing_guard_deliveries"]) != 2 {
		t.Fatalf("%v %q", err, out)
	}
}

// The governed lane declares itself a carrier only where its installer can
// print context; the collection lane only when its plugin asks (--carrier).
func TestHookCarrierDeclaration(t *testing.T) {
	if !hookCanCarry(hookInput{Runtime: "claude", RawHookEventName: "PreToolUse"}) || hookCanCarry(hookInput{Runtime: "claude", RawHookEventName: "Stop"}) {
		t.Fatal("claude carrier declaration wrong")
	}
	if hookCanCarry(hookInput{Runtime: "cursor", RawHookEventName: "preToolUse"}) || hookCanCarry(hookInput{Runtime: "", HookEventName: "PreToolUse"}) {
		t.Fatal("a runtime without an encoder must not declare itself a carrier")
	}
	if !hasFlag([]string{"collect-hook", "--runtime", "opencode", "--carrier"}, "--carrier") || hasFlag([]string{"--runtime", "opencode"}, "--carrier") {
		t.Fatal("flag detection")
	}
	out, err := json.Marshal(GovernDecision{Decision: "allow", Deliveries: []observation.Delivery{{DeliveryID: "odel_1", Message: "m"}}})
	if err != nil || !strings.Contains(string(out), `"crossing_guard_deliveries":[{"delivery_id":"odel_1","message":"m"}]`) {
		t.Fatalf("decision wire shape: %s %v", out, err)
	}
	if out, _ := json.Marshal(GovernDecision{Decision: "deny"}); strings.Contains(string(out), "crossing_guard_deliveries") {
		t.Fatal("a decision without deliveries must not carry the key")
	}
}
