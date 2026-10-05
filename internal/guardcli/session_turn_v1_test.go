package guardcli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"crossing-guard/internal/observation"
)

// decodeHookPayload decodes raw hook stdin the way cmdHook does. A tag on
// hookInput alone is never read; only UnmarshalJSON's wire struct is.
func decodeHookPayload(t *testing.T, payload []byte) hookInput {
	t.Helper()
	var in hookInput
	if err := json.NewDecoder(bytes.NewReader(payload)).Decode(&in); err != nil {
		t.Fatalf("payload did not decode: %v", err)
	}
	in.Runtime = "claude"
	return in
}

// A captured Claude Notification names its sub-type, and the turn keeps it as
// provenance: `Notification:permission_prompt`, not a bare `Notification`.
func TestNotificationSubtypeReachesNativeSource(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude_2_1_280_notification_payload.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Payload) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	in := decodeHookPayload(t, fixture.Payload)
	if in.NotificationType != "permission_prompt" || in.HookEventName != "Notification" ||
		in.SessionID != "abcdef00-aaaa-4bbb-8ccc-000000000002" {
		t.Fatalf("decoded %+v", in)
	}
	turn, err := buildSessionTurnEnvelope(in, "input.requested")
	if err != nil {
		t.Fatal(err)
	}
	if turn.NativeSource != "Notification:permission_prompt" || turn.Kind != "input.requested" {
		t.Fatalf("turn native_source %q kind %q", turn.NativeSource, turn.Kind)
	}
}

// The sub-type is provenance, so it can never cost the turn: a value of the
// wrong shape is ignored rather than failing the payload (which would drop
// the turn), and one too long for the envelope's bound is dropped rather than
// getting the whole turn rejected by the daemon.
func TestNotificationSubtypeNeverCostsTheTurn(t *testing.T) {
	for _, odd := range []string{`7`, `true`, `null`, `{"type":"permission_prompt"}`, `["permission_prompt"]`} {
		payload := `{"hook_event_name":"Notification","session_id":"s1","message":"m","notification_type":` + odd + `}`
		in := decodeHookPayload(t, []byte(payload))
		turn, err := buildSessionTurnEnvelope(in, "input.requested")
		if err != nil || in.SessionID != "s1" || in.NotificationType != "" || turn.NativeSource != "Notification" {
			t.Fatalf("notification_type %s: session %q type %q native %q err %v",
				odd, in.SessionID, in.NotificationType, turn.NativeSource, err)
		}
	}
	room := observation.MaxNativeSourceBytes - len("Notification:")
	for _, c := range []struct {
		length int
		want   string
	}{
		{room, "Notification:" + strings.Repeat("x", room)},
		{room + 1, "Notification"},
	} {
		in := hookInput{Runtime: "claude", SessionID: "s1", HookEventName: "Notification",
			NotificationType: strings.Repeat("x", c.length)}
		turn, err := buildSessionTurnEnvelope(in, "input.requested")
		if err != nil || turn.NativeSource != c.want || len(turn.NativeSource) > observation.MaxNativeSourceBytes {
			t.Fatalf("sub-type of %d bytes: native %d bytes, err %v", c.length, len(turn.NativeSource), err)
		}
	}
}
