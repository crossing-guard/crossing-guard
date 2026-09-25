package guardcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/observation"
)

func resultHookInput(t *testing.T) hookInput {
	t.Helper()
	payload := `{"hook_event_name":"PostToolUse","session_id":"session-1","tool_name":"Edit","tool_use_id":"call-1","cwd":"/repo","transcript_path":"/logs/session-1.jsonl","duration_ms":27,"tool_input":{"file_path":"src/a.go","old_string":"old","new_string":"new","replace_all":true},"tool_response":{"ok":true,"stdout":"hello","stderr":""}}`
	var input hookInput
	if err := json.Unmarshal([]byte(payload), &input); err != nil {
		t.Fatal(err)
	}
	input.Runtime = "claude"
	return input
}

func TestBuildResultEnvelopeDefaultRetainsCodeEffectsNotGenericOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	envelope, err := buildResultEnvelope(resultHookInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if envelope.NativeCallID != "call-1" || envelope.State != "success" || envelope.DurationMS != 27 || envelope.RawFieldBytes == 0 || envelope.DecodedBytes == 0 || envelope.StdoutBytes != 5 || envelope.StdoutDigest == "" || envelope.StderrDigest == "" {
		t.Fatalf("result metadata = %+v", envelope)
	}
	if envelope.Payload != nil || envelope.RetainedBytes != 0 || envelope.Completeness != "metadata-only" {
		t.Fatalf("generic result was retained by default: %+v", envelope)
	}
	if len(envelope.Effects) != 1 {
		t.Fatalf("effects = %+v", envelope.Effects)
	}
	effect := envelope.Effects[0]
	if effect.Operation != "update" || string(effect.BeforePayload) != "old" || string(effect.AfterPayload) != "new" || !effect.ReplaceAll {
		t.Fatalf("edit effect = %+v", effect)
	}
}

func TestBuildResultEnvelopeMetadataOnlyDropsBodiesButKeepsCountsAndDigests(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	data := filepath.Join(home, ".crossing-guard")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "collection.json"), []byte(`{"format_version":1,"result_payload_mode":"metadata-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	envelope, err := buildResultEnvelope(resultHookInput(t))
	if err != nil {
		t.Fatal(err)
	}
	effect := envelope.Effects[0]
	if effect.BeforePayload != nil || effect.AfterPayload != nil || effect.BeforeBytes != len("old") || effect.AfterBytes != len("new") || effect.BeforeDigest == "" || effect.AfterDigest == "" {
		t.Fatalf("metadata-only effect = %+v", effect)
	}
}

func TestBuildResultEnvelopeCompleteBoundedRetainsGenericOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	data := filepath.Join(home, ".crossing-guard")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "collection.json"), []byte(`{"format_version":1,"result_payload_mode":"complete-bounded"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	envelope, err := buildResultEnvelope(resultHookInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Completeness != "complete" || envelope.RetainedBytes == 0 || !strings.Contains(string(envelope.Payload), `"ok":true`) {
		t.Fatalf("complete-bounded result = %+v payload=%s", envelope, envelope.Payload)
	}
	if envelope.PayloadDigest != observation.DigestBytes(envelope.Payload) {
		t.Fatalf("payload digest does not describe decoded payload")
	}
}

func TestClaudeFailurePhaseIsRecordedAsFailureWithoutRelyingOnIsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	input := resultHookInput(t)
	input.HookEventName = "PostToolUseFailure"
	input.ToolIsError = false
	envelope, err := buildResultEnvelope(input)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.State != "failure" || envelope.ErrorClass != "runtime-reported" {
		t.Fatalf("failure result = %+v", envelope)
	}
}
