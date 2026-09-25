package observation

import (
	"encoding/json"
	"testing"
)

func TestEnvelopeDigestIgnoresDeliveryMutation(t *testing.T) {
	e := Envelope{Schema: SchemaV1, ObservationID: "obs_0123456789abcdef0123456789abcdef",
		SessionID: "claude/s", Runtime: "claude", Tool: "Edit", ToolInput: json.RawMessage(`{"file_path":"a.go","x":1}`),
		ToolInputBytes: 26, ToolInputDigest: "sha256-v1:x", ToolInputCompleteness: "complete",
		QueuedAt: 10, DeliveryAttempts: 1, DeliveryMode: "direct"}
	a, err := e.Digest()
	if err != nil {
		t.Fatal(err)
	}
	e.DeliveryAttempts = 9
	e.DeliveryMode = "replay"
	b, err := e.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("delivery retry changed immutable envelope digest: %s != %s", a, b)
	}
}

func TestEnvelopeRetainsUnknownStructuredInput(t *testing.T) {
	raw := json.RawMessage(`{ "file_path" : "a.go", "vendor_future" : { "nested" : true } }`)
	e := Envelope{Schema: SchemaV1, ObservationID: "obs_0123456789abcdef0123456789abcdef",
		SessionID: "codex/s", Tool: "future_tool", ToolInput: raw, ToolInputBytes: len(raw),
		ToolInputDigest: DigestBytes(raw), ToolInputCompleteness: "complete"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var got Envelope
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.ToolInput) != string(raw) {
		t.Fatalf("unknown structured input lost: %s", got.ToolInput)
	}
}

func TestDirectCaptureBudgetBeatsHookDeliveryBudget(t *testing.T) {
	if DirectCaptureBudget >= HookDeliveryBudget {
		t.Fatalf("direct capture budget %s must be shorter than hook delivery budget %s", DirectCaptureBudget, HookDeliveryBudget)
	}
}
