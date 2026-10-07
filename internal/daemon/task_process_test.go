//go:build !windows

package daemon

import (
	"crossing-guard/store"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestTaskProcessDrainsProviderStderrBeforeWait(t *testing.T) {
	// Close stdout first, then emit diagnostics while the first event is still
	// being consumed. Waiting for process exit must not discard the unread tail.
	cmd := exec.Command("/bin/sh", "-c", "exec 1>&-; printf 'first diagnostic\\n' >&2; sleep 0.05; "+quotaFailureCommand)
	var events []ChatEvent
	outcome := runTaskProcess(taskExecutionLaunch{
		cmd: cmd, runtime: "managed-fixture", driver: managedFixtureDriver{}, started: func() {},
		event: func(event ChatEvent) {
			if event["text"] == "first diagnostic" {
				time.Sleep(200 * time.Millisecond)
			}
			events = append(events, event)
		},
	}, func() bool { return false })
	if outcome.Err == nil {
		t.Fatal("nonzero provider exit was lost")
	}
	found := false
	for _, event := range events {
		if text, ok := event["text"].(string); ok && strings.Contains(text, "session usage limit") && event["error_class"] == providerErrorQuota {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider diagnostic lost before completion: %#v", events)
	}
}

// TestTaskProviderFallbackThroughHTTP exercises the application API with a real
// subprocess and disposable store; no installed runtime or external provider.
func TestTaskProviderFallbackThroughHTTP(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	driver := managedDynamicFixtureDriver{commandFor: func(req ChatRequest) string {
		if strings.Contains(req.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		return quotaFailureCommand
	}}
	f := newAgentHostFixture(t, driver)
	chatDrivers["fallback-fixture"] = fallbackFixtureDriver{managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand(helperClaimV2) }}}
	preview := selectManagedProfile(t, f.owner, helperAgentProfileSource())
	_, err := f.host.putBinding(managedBindingCommand{BindingID: "http-chain", ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: f.root, RouteID: testRouteID(f.host, "managed-fixture", "", nil), Priority: 10, Routes: testChain(f.host, store.ManagedRoute{Runtime: "fallback-fixture"}), ExpectedStateToken: store.ManagedBindingAbsentToken("http-chain")})
	if err != nil {
		t.Fatal(err)
	}
	previous := runtimeTasks
	runtimeTasks = f.tasks
	t.Cleanup(func() { runtimeTasks = previous })
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runtime-tasks", handleRuntimeTaskCreate)
	mux.HandleFunc("GET /api/runtime-tasks", handleRuntimeTaskList)
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = securityMiddleware(mux, server.Listener.Addr().String(), "synthetic-test-token", "/unused-token")
	server.Start()
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/runtime-tasks", nil)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", denied.StatusCode)
	}
	body, err := json.Marshal(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "http-session", Cwd: f.root, IdempotencyKey: "http-root"})
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest(http.MethodPost, server.URL+"/api/runtime-tasks", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer synthetic-test-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var root RuntimeTask
	err = json.NewDecoder(response.Body).Decode(&root)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusAccepted {
		t.Fatalf("create status=%d decode=%v", response.StatusCode, err)
	}
	wantTaskState(t, f.tasks, root.ID, TaskCompleted, 3*time.Second)
	parked := waitForRuns(t, f.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "parked" })[0]
	if parked.ErrorClass != providerErrorQuota {
		t.Fatalf("quota classification missing: %s", parked.ErrorClass)
	}
	f.host.relaunchParkedRunsOnce()
	completed := waitForRuns(t, f.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "completed" })[0]
	ledger := providerOutageLedgerFrom(completed.Detail)
	if len(ledger.Attempts) != 2 || ledger.Attempts[1].Runtime != "fallback-fixture" {
		t.Fatalf("fallback attribution: %+v", ledger.Attempts)
	}
	request, err = http.NewRequest(http.MethodGet, server.URL+"/api/runtime-tasks", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer synthetic-test-token")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Tasks []RuntimeTask `json:"tasks"`
	}
	err = json.NewDecoder(response.Body).Decode(&listing)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || len(listing.Tasks) < 3 {
		t.Fatalf("list status=%d tasks=%d decode=%v", response.StatusCode, len(listing.Tasks), err)
	}
	t.Log("HTTP auth denial, task creation, quota classification, attributed fallback, and task listing passed")
}

type oneShotProtocolFixture struct{}

func (oneShotProtocolFixture) WaitForNaturalExit() bool { return true }
func (oneShotProtocolFixture) Run(stdout io.ReadCloser, emit func(ChatEvent)) error {
	text, err := io.ReadAll(stdout)
	if err != nil {
		return err
	}
	emit(ChatEvent{"type": "text", "text": string(text)})
	return nil
}

func TestTaskProcessIndependentOneShotLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		exit         int
	}{
		{"success", "printf finished", 0},
		{"failed after output", "printf finished; exit 7", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", tc.script)
			var events []ChatEvent
			result := runTaskProcess(taskExecutionLaunch{cmd: cmd, protocol: oneShotProtocolFixture{}, started: func() {}, event: func(event ChatEvent) { events = append(events, event) }}, func() bool { return false })
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.exit || len(events) != 1 || events[0]["text"] != "finished" {
				t.Fatalf("state=%v result=%+v events=%+v", cmd.ProcessState, result, events)
			}
			var exitErr *exec.ExitError
			if tc.exit == 0 && result.Err != nil || tc.exit != 0 && !errors.As(result.Err, &exitErr) {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}
