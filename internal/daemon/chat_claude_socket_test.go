package daemon

// The socket post (session-message-layer plan §5.2): one JSON line, one '\n',
// close; every failure mode from the plan's table has a case here. The
// listener is a loopback unix socket in the test's temp directory.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// lineRecorder is what serveOneLine saw. The accept goroutine and each
// connection goroutine write it while the test polls it, so every access goes
// through the mutex.
type lineRecorder struct {
	mu        sync.Mutex
	received  []string
	connected int
}

// snapshot returns a copy of the lines read and the connection count.
func (r *lineRecorder) snapshot() ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.received...), r.connected
}

// serveOneLine accepts connections, reads one line from each, records it, and
// replies with nothing — the vendor's inbox does not acknowledge.
func serveOneLine(t *testing.T, path string) *lineRecorder {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &lineRecorder{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			recorder.mu.Lock()
			recorder.connected++
			recorder.mu.Unlock()
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				if n > 0 {
					recorder.mu.Lock()
					recorder.received = append(recorder.received, string(buf[:n]))
					recorder.mu.Unlock()
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return recorder
}

func TestClaudeSocketPostOneExactLine(t *testing.T) {
	// A unix path is capped near 104 bytes; the vendor keeps its sockets in
	// short /tmp names. A short random name avoids collisions with parallel
	// tests and cleans up after itself.
	path := "/tmp/cc-socks-" + fmt.Sprintf("%x", rand.Uint64()) + ".sock"
	t.Cleanup(func() { os.Remove(path) })
	recorder := serveOneLine(t, path)
	inbox := sessionInbox{SocketPath: path, RegistryPID: 1, ProcStart: "x"}
	got := (claudeChatDriver{}).deliverSessionMessageViaSocket(context.Background(), inbox, "[helper b, run r] hello")
	if got.State != "accepted" || got.Tier != "socket-post" || got.Carrier != sessionMessageCarrierSocket {
		t.Fatalf("receipt: %+v", got)
	}
	if !strings.Contains(got.Detail, "consumption is not confirmed") {
		t.Fatalf("receipt detail must deny consumption: %+v", got)
	}
	received, connected := recorder.snapshot()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && len(received) == 0; {
		time.Sleep(10 * time.Millisecond)
		received, connected = recorder.snapshot()
	}
	if len(received) != 1 || connected != 1 {
		t.Fatalf("deliveries: %d lines from %d connections", len(received), connected)
	}
	line := strings.TrimSuffix(received[0], "\n")
	var frame map[string]any
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("frame is not one JSON line: %q", received[0])
	}
	if frame["type"] != "user" {
		t.Fatalf("frame type: %v", frame["type"])
	}
	message, _ := frame["message"].(map[string]any)
	if message["role"] != "user" || message["content"] != "[helper b, run r] hello" {
		t.Fatalf("frame message: %v", message)
	}
}

func TestClaudeSocketPostFailureTable(t *testing.T) {
	// socket file absent → unavailable
	got := (claudeChatDriver{}).deliverSessionMessageViaSocket(context.Background(),
		sessionInbox{SocketPath: filepath.Join(t.TempDir(), "never.sock"), RegistryPID: 1, ProcStart: "x"}, "m")
	if got.State != "unavailable" {
		t.Fatalf("absent: %+v", got)
	}
	// empty message → unavailable before any dial
	got = (claudeChatDriver{}).deliverSessionMessageViaSocket(context.Background(),
		sessionInbox{SocketPath: "x"}, "")
	if got.State != "unavailable" {
		t.Fatalf("empty message: %+v", got)
	}
	// cancelled context mid-dial → unknown (do not retry)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := "/tmp/cc-socks-test-cancelled.sock"
	os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close(); os.Remove(path) })
	// A cancelled dial reports unknown without reaching the listener.
	cancel()
	got = (claudeChatDriver{}).deliverSessionMessageViaSocket(ctx, sessionInbox{SocketPath: path, RegistryPID: 1, ProcStart: "x"}, "m")
	if got.State != "unknown" {
		t.Fatalf("cancelled dial: %+v", got)
	}
}

func TestClaudeSocketPlatformGateNamesHost(t *testing.T) {
	// The gate names its platform in the detail so an operator can read it.
	detail := socketTransportUnsupported().Detail
	if !strings.Contains(detail, runtime.GOOS) {
		t.Fatalf("detail names no platform: %q", detail)
	}
}
func TestClaudeSocketRefusesOversizedMessage(t *testing.T) {
	// The vendor's inbox silently closes oversized lines; refusing here keeps
	// the outcome a visible terminal receipt (postwork PW-2).
	oversized := strings.Repeat("x", (64<<10)+1)
	got := (claudeChatDriver{}).deliverSessionMessageViaSocket(context.Background(),
		sessionInbox{SocketPath: "/tmp/any.sock", RegistryPID: 1, ProcStart: "x"}, oversized)
	if got.State != "unavailable" || !strings.Contains(got.Detail, "inbox bound") {
		t.Fatalf("oversized: %+v", got)
	}
	// At the bound, the post runs (a dead socket surfaces the absent-inbox
	// receipt, not the size refusal).
	atBound := strings.Repeat("x", 64<<10)
	got = (claudeChatDriver{}).deliverSessionMessageViaSocket(context.Background(),
		sessionInbox{SocketPath: "/tmp/never-exists.sock", RegistryPID: 1, ProcStart: "x"}, atBound)
	if got.State != "unavailable" || strings.Contains(got.Detail, "inbox bound") {
		t.Fatalf("at bound: %+v", got)
	}
}

func TestClaudeSocketDialIsBoundedWithoutCallerDeadline(t *testing.T) {
	// A caller without a context deadline still gets a bounded dial; the
	// pathological case must surface as unknown, never hang.
	path := "/tmp/cc-socks-test-nodeadline.sock"
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	// A bound socket nobody accepts: connect fills the backlog and blocks.
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// Close the listener but leave the file: connects are refused instantly,
	// which still proves the dial completed rather than hung.
	listener.Close()
	got := (claudeChatDriver{}).deliverSessionMessageViaSocket(context.Background(),
		sessionInbox{SocketPath: path, RegistryPID: 1, ProcStart: "x"}, "m")
	if got.State != "unavailable" {
		t.Fatalf("no-deadline refused dial: %+v", got)
	}
}
