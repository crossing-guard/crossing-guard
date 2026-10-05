package daemon

// The Claude transport option owns its own vocabulary (session-message-layer
// plan §5.3); the loader never reads inside the blob. The transport is
// resolved once per process; every test restores the compiled default.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeDeliveryOptionsOwnTheirVocabulary(t *testing.T) {
	t.Cleanup(func() { claudeDeliveryTransport = "hook" })
	if err := (claudeChatDriver{}).ConfigureDelivery(json.RawMessage(`{"transport":"socket"}`)); err != nil || claudeDeliveryTransport != "socket" {
		t.Fatal(err, claudeDeliveryTransport)
	}
	capability := claudeDeliveryCapability()
	if !strings.Contains(capability.Boundary, "inbox") || !strings.Contains(capability.Detail, "socket") {
		t.Fatalf("socket capability: %+v", capability)
	}
	if !strings.Contains(capability.Detail, "may hold, drop, or expire") {
		t.Fatalf("socket capability must state the vendor controls: %+v", capability)
	}
	if err := (claudeChatDriver{}).ConfigureDelivery(json.RawMessage(`{"transport":"sctp"}`)); err == nil {
		t.Fatal("unknown transport accepted")
	}
	if err := (claudeChatDriver{}).ConfigureDelivery(json.RawMessage(`{"mode":"x"}`)); err == nil {
		t.Fatal("unknown key accepted")
	}
	if err := (claudeChatDriver{}).ConfigureDelivery(json.RawMessage(`{}`)); err != nil || claudeDeliveryTransport != "hook" {
		t.Fatal(err, claudeDeliveryTransport)
	}
	if got := claudeDeliveryCapability(); !strings.Contains(got.Boundary, "hook boundary") {
		t.Fatalf("hook capability: %+v", got)
	}
}

func TestClaudeDeliverySocketTransportDoesNotEnqueue(t *testing.T) {
	t.Cleanup(func() { claudeDeliveryTransport = "hook" })
	if err := (claudeChatDriver{}).ConfigureDelivery(json.RawMessage(`{"transport":"socket"}`)); err != nil {
		t.Fatal(err)
	}
	// An unresolvable identity is unavailable with the resolver's reason —
	// never a silent fallback to the hook path.
	got := (claudeChatDriver{}).DeliverSessionMessage(context.Background(),
		SessionIdentity{Runtime: "claude", NativeID: "no-such-session-anywhere"}, "hello")
	if got.State != "unavailable" || got.Carrier != "" {
		t.Fatalf("unresolvable: %+v", got)
	}
	// The hook path still answers the boundary carrier.
	if err := (claudeChatDriver{}).ConfigureDelivery(json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	got = (claudeChatDriver{}).DeliverSessionMessage(context.Background(),
		SessionIdentity{Runtime: "claude", NativeID: "any"}, "hello")
	if got.State != "accepted" || got.Carrier != sessionMessageCarrierBoundary {
		t.Fatalf("hook path: %+v", got)
	}
}
