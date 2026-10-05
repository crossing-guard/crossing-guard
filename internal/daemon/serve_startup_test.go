package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/store"

	"github.com/ncruces/go-sqlite3/driver"
)

// serveStartupHelperEnv makes the re-executed test binary run Main instead of the
// suite. Main blocks in ListenAndServe, exits on bad flags, and sets package
// globals, so it only runs in a child process.
const serveStartupHelperEnv = "CG_TEST_SERVE_STARTUP_ARGS"

func TestServeStartupHelperProcess(t *testing.T) {
	raw := os.Getenv(serveStartupHelperEnv)
	if raw == "" {
		t.Skip("helper process for TestServeStartsDegradedOnUnopenableStore")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		fmt.Fprintln(os.Stderr, "helper args:", err)
		os.Exit(3)
	}
	// This process is the real boot: a store seed or a swapped home would make it a
	// boot of something no installed daemon starts from.
	if testMainArranged != "" {
		fmt.Fprintln(os.Stderr, "the serve child was started with test arrangements:", testMainArranged)
		os.Exit(3)
	}
	// The parent holds our stdin open; if it dies without its cleanup (a suite
	// timeout), EOF ends this child instead of leaving a daemon on a random port.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
	Main(args)
	os.Exit(0)
}

// serveStartupBudget bounds one child start-up; the daemon package runs for
// ~15 minutes under -race with the rest of the suite competing for the CPU.
const serveStartupBudget = 60 * time.Second

// minExpectedGetRoutes is a floor on the route sweep, well under the ~75 GET
// routes registered today: fewer means the source scan broke, not that routes
// were removed.
const minExpectedGetRoutes = 50

// syncBuffer is the child's combined output; exec's copy goroutine writes it
// while the test reads it for failure messages.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startupChild is one `serve` running in a child process against its own data dir.
type startupChild struct {
	t       *testing.T
	dataDir string
	base    string
	token   string
	output  *syncBuffer
	exited  chan error
}

// startServeChild runs Main in a child with a minimal environment — nothing from
// the developer's HOME, CG_* or installed daemon reaches it — and waits until
// /api/govern/health answers. prepare runs on the data dir before start-up.
func startServeChild(t *testing.T, extraArgs []string, prepare func(dataDir string)) *startupChild {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	home := filepath.Join(root, "home")
	for _, dir := range []string{dataDir, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if prepare != nil {
		prepare(dataDir)
	}
	// The port is free when chosen and bound by the child moments later; losing it
	// to another test fails loudly as an early exit, never as a false pass.
	addr := freeLoopbackAddr(t)
	args, err := json.Marshal(append([]string{"--no-hook-install", "--data", dataDir, "--addr", addr}, extraArgs...))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeStartupHelperProcess$")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + root,
		serveStartupHelperEnv + "=" + string(args)}
	child := &startupChild{t: t, dataDir: dataDir, base: "http://" + addr, output: &syncBuffer{}, exited: make(chan error, 1)}
	cmd.Stdout, cmd.Stderr = child.output, child.output
	if _, err := cmd.StdinPipe(); err != nil { // closed when this process exits
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { child.exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-child.exited
	})
	deadline := time.Now().Add(serveStartupBudget)
	for {
		child.requireAlive("during start-up")
		child.token = strings.TrimSpace(readFileString(filepath.Join(dataDir, "api-token")))
		if child.token != "" {
			if status, _ := child.request(http.MethodGet, "/api/govern/health", nil); status == http.StatusOK {
				return child
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("health never answered:\n%s", outputTail(child.output.String(), 40))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *startupChild) requireAlive(when string) {
	c.t.Helper()
	select {
	case err := <-c.exited:
		c.exited <- err
		c.t.Fatalf("serve exited %s (%v):\n%s", when, err, outputTail(c.output.String(), 40))
	default:
	}
}

func (c *startupChild) health() GovernorHealth {
	c.t.Helper()
	status, body := c.request(http.MethodGet, "/api/govern/health", nil)
	var health GovernorHealth
	if status != http.StatusOK {
		c.t.Fatalf("health = %d %s", status, body)
	}
	if err := json.Unmarshal(body, &health); err != nil {
		c.t.Fatalf("health: %v: %s", err, body)
	}
	return health
}

func (c *startupChild) request(method, path string, body []byte) (int, []byte) {
	c.t.Helper()
	return serveStartupRequest(c.t, &http.Client{Timeout: 10 * time.Second}, method, c.base+path, c.token, body)
}

// F-8 (A1, A4): a store this binary refuses to open — a newer schema, the rollback
// case — must leave `serve` running degraded with the reason on
// /api/govern/health, never a nil-governor panic that crash-loops the service.
func TestServeStartsDegradedOnUnopenableStore(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child daemon")
	}
	child := startServeChild(t, nil, func(dataDir string) {
		stampStoreVersion(t, filepath.Join(dataDir, "index.sqlite"), 99)
	})
	health := child.health()
	if health.Configured {
		t.Fatalf("health reports configured on an unopenable store: %+v", health)
	}
	if !strings.Contains(health.Problem, "v99") {
		t.Fatalf("health problem = %q, want the store refusal naming v99", health.Problem)
	}
	if status, _ := child.request(http.MethodGet, "/", nil); status != http.StatusOK {
		t.Fatalf("console GET / = %d, want 200", status)
	}
	observe := []byte(`{"session_id":"s","tool":"Bash","decision":"allow"}`)
	if status, _ := child.request(http.MethodPost, "/api/govern/observe", observe); status != http.StatusServiceUnavailable {
		t.Fatalf("observe = %d, want 503", status)
	}
	// The console and CLI 503s carry the start-up reason; the hook-facing one
	// stays bare (degraded-surfaces-state-the-reason plan, A1).
	for path, prefix := range map[string]string{
		"/api/govern/sessions": "governor not configured: ",
		"/api/runtime-tasks":   "runtime task service unavailable: ",
	} {
		status, body := child.request(http.MethodGet, path, nil)
		if status != http.StatusServiceUnavailable || !strings.HasPrefix(string(body), prefix) || !strings.Contains(string(body), "v99") {
			t.Fatalf("%s = %d %q, want 503 %q plus the v99 reason", path, status, body, prefix)
		}
	}
	if _, body := child.request(http.MethodPost, "/api/govern/observe", observe); strings.TrimSpace(string(body)) != "governor not configured" {
		t.Fatalf("hook-facing observe body = %q, want the bare text", body)
	}
	for _, path := range []string{"/api/workspace-diff/checkout?id=s", "/api/workspace-files?id=s"} {
		status, body := child.request(http.MethodGet, path, nil)
		if status != http.StatusServiceUnavailable || !strings.Contains(string(body), "review-unavailable") {
			t.Fatalf("%s = %d %s, want 503 review-unavailable", path, status, body)
		}
	}
	// A4: net/http recovers a handler panic by dropping the connection, so the
	// process staying up proves nothing about the routes. Every parameter-free GET
	// must answer with a status. Most id-taking routes stop at validation without
	// an id, so the common ones are also probed with a well-formed one.
	routes := append(parameterFreeGetRoutes(t), "/api/govern/session?id=claude/s", "/api/govern/entity?id=file:/x",
		"/api/govern/results?session=claude/s", "/api/chain/verify?session=claude/s", "/api/session?runtime=claude&id=s",
		"/api/session/related?runtime=claude&id=s", "/api/refs/backlinks?cwd=/tmp&target=x")
	if len(routes) < minExpectedGetRoutes {
		t.Fatalf("route sweep found only %d GET routes — the source scan is broken", len(routes))
	}
	for _, path := range routes {
		if status, _ := child.request(http.MethodGet, path, nil); status == 0 {
			t.Errorf("GET %s dropped the connection (a recovered handler panic) on a degraded daemon", path)
		}
	}
	child.requireAlive("after the route sweep")
}

// A2: the store is fine but the governor's detector assembly is not; start-up
// degrades and names the detector problem.
func TestServeStartsDegradedOnDetectorError(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child daemon")
	}
	child := startServeChild(t, []string{"--detectors", filepath.Join(t.TempDir(), "missing-detectors.json")}, nil)
	health := child.health()
	if health.Configured || !strings.Contains(health.Problem, "missing-detectors.json") {
		t.Fatalf("health = configured:%v problem:%q, want the detector error", health.Configured, health.Problem)
	}
	child.requireAlive("after health")
}

// A3: the happy path still starts the governor — the team link reads it during
// initGovernor, so publishing it late would panic every healthy start.
func TestServeStartsGovernorOnFreshStore(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child daemon")
	}
	child := startServeChild(t, nil, nil)
	if health := child.health(); !health.Configured || health.Problem != "" {
		t.Fatalf("health on a fresh store = %+v, want configured with no problem", health)
	}
}

// A5: only a busy store (another writer holds the lock) is worth a retry; a
// refused newer store fails the same way every time.
func TestGovernorStartupBusyOnlyWhenLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	holder, err := driver.Open("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	_, busyErr := store.Open(path)
	if !governorStartupBusy(busyErr) {
		t.Fatalf("open under an exclusive lock: err=%v; want a busy error", busyErr)
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	newer := filepath.Join(t.TempDir(), "index.sqlite")
	stampStoreVersion(t, newer, 99)
	_, refusal := store.Open(newer)
	if refusal == nil || governorStartupBusy(refusal) {
		t.Fatalf("newer store: err=%v; want a refusal that is not busy", refusal)
	}
	if governorStartupBusy(nil) {
		t.Fatal("no error classified as busy")
	}
}

// A5 at the Main level: a store whose write lock another process holds through
// start-up is retried once and then served degraded with an actionable reason —
// never an exit that launchd would repeat for as long as the lock is held.
func TestServeStartsDegradedOnBusyStore(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child daemon")
	}
	var holder *sql.DB
	var lock *sql.Conn
	child := startServeChild(t, nil, func(dataDir string) {
		path := filepath.Join(dataDir, "index.sqlite")
		ix, err := store.Open(path) // a current, WAL store
		if err != nil {
			t.Fatal(err)
		}
		if err := ix.Close(); err != nil {
			t.Fatal(err)
		}
		if holder, err = driver.Open("file:" + path); err != nil {
			t.Fatal(err)
		}
		if lock, err = holder.Conn(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := lock.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(func() {
		_, _ = lock.ExecContext(context.Background(), "ROLLBACK")
		_ = lock.Close()
		_ = holder.Close()
	})
	health := child.health()
	if health.Configured || !strings.Contains(health.Problem, "busy at start-up") {
		t.Fatalf("health = configured:%v problem:%q, want the busy-store reason", health.Configured, health.Problem)
	}
	if !strings.Contains(child.output.String(), "retrying once") {
		t.Fatalf("no retry before degrading:\n%s", outputTail(child.output.String(), 40))
	}
	child.requireAlive("after health")
}

// A6: without the governor the pin source is still a function (a nil source
// means "proceed" to profilefs) and it errors, so a history trim is refused.
func TestGovernorPinSourceRefusesWithoutGovernor(t *testing.T) {
	saved := governor
	governor = nil
	t.Cleanup(func() { governor = saved })
	pins := governorPinSource("reviewer")
	if pins == nil {
		t.Fatal("pin source is nil; profilefs would trim pinned history")
	}
	if _, err := pins(); err == nil {
		t.Fatal("pin source answered without a store")
	}
}

// parameterFreeGetRoutes reads the package's route registrations the way
// api-contract-lint does, so a new route joins the sweep without editing a list.
func parameterFreeGetRoutes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`HandleFunc\("GET (/api/[^"{]*)"`)
	seen := map[string]bool{}
	var routes []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(src), -1) {
			path := match[1]
			if strings.HasSuffix(path, "/stream") || seen[path] {
				continue // streams hold the connection open by design
			}
			seen[path] = true
			routes = append(routes, path)
		}
	}
	sort.Strings(routes)
	return routes
}

func serveStartupRequest(t *testing.T, client *http.Client, method, url, token string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil // not listening yet, or a dropped connection: the caller decides
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// stampStoreVersion writes only a user_version, which is what an older binary
// sees when it meets a newer store.
func stampStoreVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := driver.Open("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		t.Fatal(err)
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func outputTail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
