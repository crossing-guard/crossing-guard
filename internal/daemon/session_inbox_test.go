package daemon

// The generic half of inbox resolution (session-message-layer plan §5.1, as
// folded by the confirming pass): dispatch through the optional port, refuse
// the unattested, and fail closed on a recycled PID. Vendor-shaped fixtures
// live in session_inbox_claude_test.go.

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// inboxResolverFixtureDriver is a registered driver that carries only the
// resolver port, so the dispatch and the generic rules are testable without
// any vendor file.
type inboxResolverFixtureDriver struct {
	managedDynamicFixtureDriver
	inbox    sessionInbox
	usable   bool
	reason   string
	attempts []SessionIdentity
}

func (d *inboxResolverFixtureDriver) ResolveSessionInbox(identity SessionIdentity) (sessionInbox, bool, string) {
	d.attempts = append(d.attempts, identity)
	return d.inbox, d.usable, d.reason
}

// registerInboxFixtureDriver installs one fixture driver for the test's
// duration and restores the production registry (the house test idiom).
func registerInboxFixtureDriver(t *testing.T, name string, driver ChatDriver) {
	t.Helper()
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{name: driver}
	t.Cleanup(func() { chatDrivers = original })
}

// liveProcessStart reads a process's start time in the same rendering the
// vendor registry rows carry, so the liveness check is tested against a
// process that certainly exists.
func liveProcessStart(t *testing.T, pid int) string {
	t.Helper()
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("ps is unavailable or the process is unreadable: %v", err)
	}
	rendered := strings.TrimSpace(string(out))
	if rendered == "" {
		t.Skip("ps rendered no start time")
	}
	return rendered
}

func TestSessionInboxDispatchesThroughOptionalPort(t *testing.T) {
	resolver := &inboxResolverFixtureDriver{inbox: sessionInbox{
		SocketPath: "/inboxes/live.sock", SessionID: "row-session",
		RegistryPID: os.Getpid(), ProcStart: liveProcessStart(t, os.Getpid())}, usable: true}
	registerInboxFixtureDriver(t, t.Name(), resolver)
	identity := SessionIdentity{Runtime: t.Name(), NativeID: "row-session"}
	inbox, usable, reason := resolveSessionInbox(identity)
	if !usable || inbox.SocketPath != "/inboxes/live.sock" || reason != "" {
		t.Fatalf("resolve: %+v %v %q", inbox, usable, reason)
	}
	// The resolver received the caller's identity untouched: the generic layer
	// adds nothing and matches nothing itself.
	if len(resolver.attempts) != 1 || resolver.attempts[0] != identity {
		t.Fatalf("resolver saw %+v", resolver.attempts)
	}
}

func TestSessionInboxWithoutResolverIsUnavailable(t *testing.T) {
	// A registered driver without the resolver port is a result, not an error.
	got, usable, reason := resolveSessionInbox(SessionIdentity{Runtime: "codex", NativeID: "x"})
	if usable || got.SocketPath != "" || reason == "" {
		t.Fatalf("missing resolver: %+v %v %q", got, usable, reason)
	}
	if _, _, reason := resolveSessionInbox(SessionIdentity{Runtime: "no-such-runtime"}); reason == "" {
		t.Fatal("unregistered runtime must name its miss")
	}
}

func TestSessionInboxRefusesStalePIDAndMissingStart(t *testing.T) {
	// This test process is the liveness fixture: its start time matches by
	// construction; a foreign start and a missing start both fail closed.
	live := liveProcessStart(t, os.Getpid())
	resolver := &inboxResolverFixtureDriver{inbox: sessionInbox{
		SocketPath: "/inboxes/live.sock", RegistryPID: os.Getpid(), ProcStart: live}, usable: true}
	registerInboxFixtureDriver(t, t.Name(), resolver)
	if _, usable, reason := resolveSessionInbox(SessionIdentity{Runtime: t.Name(), NativeID: "s"}); !usable || reason != "" {
		t.Fatalf("matching start refused: %v %q", usable, reason)
	}
	resolver.inbox.ProcStart = "Mon Jan  1 00:00:00 2001"
	if _, usable, reason := resolveSessionInbox(SessionIdentity{Runtime: t.Name(), NativeID: "s"}); usable || reason != "stale-registry" {
		t.Fatalf("recycled PID: %v %q", usable, reason)
	}
	resolver.inbox.ProcStart = ""
	if _, usable, reason := resolveSessionInbox(SessionIdentity{Runtime: t.Name(), NativeID: "s"}); usable || reason != "stale-registry" {
		t.Fatalf("missing start: %v %q", usable, reason)
	}
	// A row without a PID proves nothing and is refused (postwork PW-1).
	resolver.inbox.RegistryPID = 0
	if _, usable, reason := resolveSessionInbox(SessionIdentity{Runtime: t.Name(), NativeID: "s"}); usable || reason != "stale-registry" {
		t.Fatalf("no-PID row: %v %q", usable, reason)
	}
}

func TestSessionInboxPassesResolverRefusalsThrough(t *testing.T) {
	for _, reason := range []string{"unvettable-path", "ambiguous-inbox", "no live inbox for this session", "stale-path"} {
		refuser := &inboxResolverFixtureDriver{usable: false, reason: reason}
		runtime := t.Name() + "-" + reason
		registerInboxFixtureDriver(t, runtime, refuser)
		if _, usable, got := resolveSessionInbox(SessionIdentity{Runtime: runtime, NativeID: "s"}); usable || got != reason {
			t.Fatalf("%s: %v %q", reason, usable, got)
		}
	}
}
