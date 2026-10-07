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

// An optional field omitted when empty leaves an existing entry's bytes, and so its
// digest, exactly as they were (team rest-of-release plan §6.4, §17.3). The expected
// bytes are written out here as the serialization before the field existed, so adding
// it — or reordering it ahead of another field — cannot move an existing digest
// without failing this test.
func TestSessionEntryDigestIsUnchangedByTheOptionalTicket(t *testing.T) {
	entry := SessionEntryEnvelope{Schema: SessionEntrySchemaV1, ObservationID: "ent_0123456789abcdef0123456789abcdef",
		CollectorID: CollectorSessionEntry, CollectorVersion: "v1", Runtime: "claude", SessionID: "native-1",
		HookEventName: "SessionStart", EntryKind: "start", NativeSource: "startup", TranscriptPath: "/t/native-1.jsonl",
		Cwd: "/repo", ObservedAt: 1790000000, QueuedAt: 1790000000, DeliveryAttempts: 3, DeliveryMode: "replay"}
	before := `{"session_entry_schema":"` + SessionEntrySchemaV1 + `","session_entry_observation_id":"ent_0123456789abcdef0123456789abcdef",` +
		`"collector_id":"` + CollectorSessionEntry + `","collector_version":"v1","runtime":"claude","session":"native-1",` +
		`"hook_event_name":"SessionStart","entry_kind":"start","native_source":"startup","transcript_path":"/t/native-1.jsonl",` +
		`"cwd":"/repo","observed_at":1790000000,"queued_at":1790000000,"delivery_attempts":0,"delivery_mode":""}`
	got, err := entry.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if want := DigestBytes([]byte(before)); got != want {
		t.Fatalf("an existing entry's digest moved: %s, want %s", got, want)
	}
	entry.HandoffTicket = "tkt_01JB9ZK6M3Q0V7W8X9Y0Z1A2B3"
	if ticketed, _ := entry.Digest(); ticketed == got {
		t.Fatal("an entry that carries a ticket is a different entry")
	}
}
