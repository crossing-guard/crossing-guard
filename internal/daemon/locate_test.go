package daemon

// These tests pin the two red-team findings that shaped LocateConsole. Both were
// live states on a real machine, not hypotheticals:
//   R1 two data dirs, each with its own api-token — one live, one five days stale
//   R2 a daemon-addr file outliving its daemon, naming a dead port
// A regression in either one prints a plausible URL that does not work, which is
// worse than printing nothing.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceArgsReadsTheDaemonOfRecord(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "svc.plist")
	if err := os.WriteFile(p, []byte(launchdPlist("/opt/crossing-guard/crossing-guard",
		"/Users/x/.crossing-guard", "127.0.0.1:7788", "/Users/x/.crossing-guard/daemon.log")), 0o644); err != nil {
		t.Fatal(err)
	}
	data, addr, ok := serviceArgs(p)
	if !ok {
		t.Fatal("our own plist must be readable by our own parser")
	}
	if data != "/Users/x/.crossing-guard" {
		t.Errorf("data dir = %q, want the pinned --data (picking the wrong dir is the whole bug)", data)
	}
	if addr != "127.0.0.1:7788" {
		t.Errorf("addr = %q, want the pinned --addr", addr)
	}
}

// The plist pins --data explicitly BECAUSE the two default resolvers disagree
// (~/.crossing-guard vs ~/.crossing-guard/console). A plist that is not ours —
// no --data — must not be reported as an answer, or we would resolve a store the
// running daemon never opened.
func TestServiceArgsRefusesAPlistWithoutData(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "foreign.plist")
	body := `<plist version="1.0"><dict>
  <key>ProgramArguments</key>
  <array><string>/usr/local/bin/something</string><string>serve</string></array>
</dict></plist>`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := serviceArgs(p); ok {
		t.Error("a plist with no --data must not be treated as the daemon of record")
	}
}

// R2: the file said 7805 for five days after that daemon died. Presence is not
// liveness — the probe must call the refusal what it is.
func TestProbeReportsADeadPortRatherThanPrintingAURL(t *testing.T) {
	// Port 1 on loopback: reserved, nothing listens, connection refused fast.
	problem := ProbeConsole(Location{Addr: "127.0.0.1:1", DataDir: "/tmp/x", Token: "t"})
	if problem == "" {
		t.Fatal("a refused connection must be reported, never treated as healthy")
	}
	if !strings.Contains(problem, "not running") {
		t.Errorf("diagnosis %q must say the daemon is not running", problem)
	}
}

// R1's bad half: a token that a LIVE daemon rejects. The user must be told the
// token is stale and which file it came from — not handed a URL that 401s.
func TestProbeNamesTheStaleTokenFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	problem := ProbeConsole(Location{Addr: addr, DataDir: "/Users/x/.crossing-guard/console", Token: "stale"})
	if problem == "" {
		t.Fatal("a 401 must be reported, not swallowed")
	}
	if !strings.Contains(problem, "api-token") || !strings.Contains(problem, "console") {
		t.Errorf("diagnosis %q must name the token FILE it read, so the user can see which data dir lost", problem)
	}
}

func TestProbeIsSilentWhenHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	loc := Location{Addr: strings.TrimPrefix(srv.URL, "http://"), DataDir: "/tmp/x", Token: "good"}
	if problem := ProbeConsole(loc); problem != "" {
		t.Fatalf("healthy daemon reported a problem: %s", problem)
	}
	if !strings.HasSuffix(loc.URL(), "/#t=good") {
		t.Errorf("URL %q must carry the token in the FRAGMENT — a query param lands in access logs", loc.URL())
	}
}

// An empty token file is not a token. Reading one as valid would produce a URL
// that authenticates as nobody, and the daemon refuses to serve with an empty
// token anyway (securityMiddleware) — so the confusing failure would land on the
// user, not on us.
func TestReadTokenRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "api-token"), []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(dir); err == nil {
		t.Error("an empty token file must be an error, not an empty token")
	}
}
