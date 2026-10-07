package daemon

// Vendor-shaped fixtures for the runtime's inbox resolver (session-message-
// layer plan §5.1): the registry is read from a fixture HOME, never from the
// live store, and every refusal this file owns has its own case.

import (
	"os"
	"path/filepath"
	"testing"
)

// writeInboxRegistry writes one registry directory with the given rows, in the
// vendor's own field names, and points HOME at it for the test's duration.
func writeInboxRegistry(t *testing.T, rows ...string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if err := os.WriteFile(filepath.Join(dir, itoaTest(i)+".json"), []byte(row), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
}

func itoaTest(v int) string { return strconvItoa(v) }

func strconvItoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func TestClaudeInboxResolvesExactSession(t *testing.T) {
	writeInboxRegistry(t,
		`{"pid":4242,"sessionId":"aaaa1111-1111-1111-1111-111111111111","cwd":"/repo","procStart":"Sat Sep 26 18:07:02 2026","status":"idle","messagingSocketPath":"/tmp/cc-socks/4242.sock"}`,
		`{"pid":5252,"sessionId":"bbbb2222-2222-2222-2222-222222222222","cwd":"/repo","procStart":"Sat Sep 26 18:07:02 2026","status":"idle","messagingSocketPath":"/tmp/cc-socks/5252.sock"}`)
	inbox, usable, reason := (claudeChatDriver{}).ResolveSessionInbox(
		SessionIdentity{Runtime: "claude", NativeID: "bbbb2222-2222-2222-2222-222222222222"})
	if !usable || reason != "" || inbox.SocketPath != "/tmp/cc-socks/5252.sock" || inbox.RegistryPID != 5252 {
		t.Fatalf("resolve: %+v %v %q", inbox, usable, reason)
	}
}

func TestClaudeInboxUnknownSessionIsNotFound(t *testing.T) {
	writeInboxRegistry(t,
		`{"pid":4242,"sessionId":"aaaa1111-1111-1111-1111-111111111111","messagingSocketPath":"/tmp/cc-socks/4242.sock"}`)
	_, usable, reason := (claudeChatDriver{}).ResolveSessionInbox(
		SessionIdentity{Runtime: "claude", NativeID: "cccc3333-3333-3333-3333-333333333333"})
	if usable || reason != "no live inbox for this session" {
		t.Fatalf("unknown session: %v %q", usable, reason)
	}
}

func TestClaudeInboxAmbiguityFailsClosed(t *testing.T) {
	row := func(pid int) string {
		return `{"pid":` + strconvItoa(pid) + `,"sessionId":"dddd4444-4444-4444-4444-444444444444","messagingSocketPath":"/tmp/cc-socks/` + strconvItoa(pid) + `.sock"}`
	}
	writeInboxRegistry(t, row(4242), row(4243))
	_, usable, reason := (claudeChatDriver{}).ResolveSessionInbox(
		SessionIdentity{Runtime: "claude", NativeID: "dddd4444-4444-4444-4444-444444444444"})
	if usable || reason != "ambiguous-inbox" {
		t.Fatalf("ambiguous rows: %v %q", usable, reason)
	}
}

func TestClaudeInboxRefusesUnvettedPaths(t *testing.T) {
	writeInboxRegistry(t,
		`{"pid":4242,"sessionId":"eeee5555-5555-5555-5555-555555555555","messagingSocketPath":"/tmp/others/4242.sock"}`)
	_, usable, reason := (claudeChatDriver{}).ResolveSessionInbox(
		SessionIdentity{Runtime: "claude", NativeID: "eeee5555-5555-5555-5555-555555555555"})
	if usable || reason != "unvettable-path" {
		t.Fatalf("unvetted path: %v %q", usable, reason)
	}
}

func TestClaudeVettedSocketDirsShape(t *testing.T) {
	dirs := claudeVettedSocketDirs()
	if len(dirs) < 2 {
		t.Fatalf("vetted dirs: %q", dirs)
	}
	if !claudeSocketPathVetted("/tmp/cc-socks/4242.sock") {
		t.Fatal("the shared namespace must be vetted")
	}
	if claudeSocketPathVetted("/tmp/cc-socks") || claudeSocketPathVetted("/tmp/cc-socks/../etc/passwd") {
		t.Fatal("the directory itself and traversal shapes are not inbox paths")
	}
	if claudeSocketPathVetted("") {
		t.Fatal("an empty path is not vetted")
	}
}

func TestClaudeInboxRequiresNativeID(t *testing.T) {
	_, usable, reason := (claudeChatDriver{}).ResolveSessionInbox(SessionIdentity{Runtime: "claude", CatalogID: "cat-only"})
	if usable || reason != "registry requires the runtime's native session id" {
		t.Fatalf("catalog-only identity: %v %q", usable, reason)
	}
}

func TestClaudeInboxSkipsPartialRows(t *testing.T) {
	// A row mid-write (no socket path yet) must not blind the resolver: the
	// complete row next to it still resolves.
	writeInboxRegistry(t,
		`{"pid":4242,"sessionId":"ffff6666-6666-6666-6666-666666666666"}`,
		`{"pid":5252,"sessionId":"ffff6667-6667-6667-6667-666666666667","messagingSocketPath":"/tmp/cc-socks/5252.sock"}`)
	inbox, usable, reason := (claudeChatDriver{}).ResolveSessionInbox(
		SessionIdentity{Runtime: "claude", NativeID: "ffff6667-6667-6667-6667-666666666667"})
	if !usable || inbox.RegistryPID != 5252 || reason != "" {
		t.Fatalf("partial rows: %+v %v %q", inbox, usable, reason)
	}
}
