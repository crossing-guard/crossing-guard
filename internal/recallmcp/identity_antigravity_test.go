package recallmcp

import (
	"encoding/json"
	"testing"
)

func TestAntigravityDoesNotInventCallerIdentity(t *testing.T) {
	server, err := NewServer("antigravity", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := server.identity.SessionID(json.RawMessage(`{"sessionId":"unverified","conversationId":"unverified"}`)); got != "" {
		t.Fatal("unmeasured identity trusted")
	}
	if dir, source := server.identity.Place(); dir != "" || source != "" {
		t.Fatal("unmeasured working directory")
	}
}
