package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// withStartupProblems sets the degraded start-up state for one test and restores
// the package globals afterwards.
func withStartupProblems(t *testing.T, governorReason, tasksReason string) {
	t.Helper()
	savedGovernor, savedGovernorProblem := governor, governorProblem
	savedTasks, savedTaskIndex, savedTasksProblem := runtimeTasks, runtimeTaskIndex, runtimeTasksProblem
	t.Cleanup(func() {
		governor, governorProblem = savedGovernor, savedGovernorProblem
		runtimeTasks, runtimeTaskIndex, runtimeTasksProblem = savedTasks, savedTaskIndex, savedTasksProblem
	})
	governor, governorProblem = nil, governorReason
	runtimeTasks, runtimeTaskIndex, runtimeTasksProblem = nil, nil, tasksReason
}

func degradedBody(t *testing.T, handler http.HandlerFunc, method, target string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(method, target, strings.NewReader("{}")))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s %s = %d, want 503", method, target, recorder.Code)
	}
	return strings.TrimSpace(recorder.Body.String())
}

func taskFeedUnavailableText(t *testing.T) string {
	t.Helper()
	var frames []feedFrame
	err := taskFeed(context.Background(), 0, func(frame feedFrame) error { frames = append(frames, frame); return nil }, func() {})
	if err != nil || len(frames) != 1 || frames[0].Event != "unavailable" {
		t.Fatalf("taskFeed = %v %+v, want one unavailable frame", err, frames)
	}
	var payload map[string]string
	if err := json.Unmarshal(frames[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload["error"]
}

// hookFacingGovernorRoutes are the six POST routes the hook calls. They are the
// only ones that answer the bare governor text.
var hookFacingGovernorRoutes = map[string]http.HandlerFunc{
	"/api/govern/observe":          handleGovernObserve,
	"/api/govern/observe/v1":       handleGovernObserveV1,
	"/api/govern/result/v1":        handleGovernResultV1,
	"/api/govern/closure/v1":       handleGovernClosureV1,
	"/api/govern/session-entry/v1": handleGovernSessionEntryV1,
	"/api/govern/session-turn/v1":  handleGovernSessionTurnV1,
}

// A2: the console and CLI 503s name the start-up reason, the hook-facing one
// does not, and with no recorded reason every text is the bare one.
func TestDegradedUnavailableTextsCarryTheStartupReason(t *testing.T) {
	const reason = "store schema is v99"
	withStartupProblems(t, reason, reason)
	cases := []struct {
		name, got, want string
	}{
		{"govern sessions", degradedBody(t, handleGovernSessions, http.MethodGet, "/api/govern/sessions"), "governor not configured: " + reason},
		{"task list", degradedBody(t, handleRuntimeTaskList, http.MethodGet, "/api/runtime-tasks"), "runtime task service unavailable: " + reason},
		{"effort get", degradedBody(t, handleSessionEffortGet, http.MethodGet, "/api/session-turn-settings"), "task settings unavailable: " + reason},
		{"task feed", taskFeedUnavailableText(t), "runtime task service unavailable: " + reason},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	// Every hook-facing POST stays bare: guardcli prints the body on the hook's
	// stderr (plan RT-1).
	for route, handler := range hookFacingGovernorRoutes {
		if got := degradedBody(t, handler, http.MethodPost, route); got != governorNotConfigured {
			t.Errorf("hook route %s = %q, want the bare text", route, got)
		}
	}
	governorProblem, runtimeTasksProblem = "", ""
	if got := degradedBody(t, handleGovernSessions, http.MethodGet, "/api/govern/sessions"); got != "governor not configured" {
		t.Errorf("no recorded reason: govern sessions = %q", got)
	}
	if got := degradedBody(t, handleRuntimeTaskList, http.MethodGet, "/api/runtime-tasks"); got != "runtime task service unavailable" {
		t.Errorf("no recorded reason: task list = %q", got)
	}
}

// A3: the bare texts live only in their owners, so a new site cannot drop the
// reason by copying an old literal; and each opening-snapshot handler keeps its
// one 503 in the nil branch, because the event stream client reads a 503 there
// as "absent for the life of the process" (plan §2.2, RT-3).
// It matches quoted literals line by line, so a text built by concatenation or
// fmt.Sprintf, or split across lines, is not caught.
func TestDegradedUnavailableTextsHaveOneOwner(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	governorLiteral := regexp.MustCompile(`"governor not configured"`)
	bareWrites := 0
	taskLiteral := regexp.MustCompile(`"(runtime task service unavailable|task settings unavailable)"`)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(source), "\n") {
			if strings.Contains(line, "http.Error(w, governorNotConfigured,") {
				bareWrites++
			}
			if governorLiteral.MatchString(line) && !strings.Contains(line, "const governorNotConfigured =") {
				t.Errorf("%s writes the bare governor text outside governorNotConfigured: %s", file, strings.TrimSpace(line))
			}
			for _, match := range taskLiteral.FindAllStringIndex(line, -1) {
				if !strings.HasSuffix(line[:match[0]], "runtimeTasksUnavailable(") {
					t.Errorf("%s writes a bare task text outside runtimeTasksUnavailable: %s", file, strings.TrimSpace(line))
				}
			}
		}
	}
	if bareWrites != len(hookFacingGovernorRoutes) {
		t.Errorf("%d routes write the bare governor text, want only the %d hook-facing ones", bareWrites, len(hookFacingGovernorRoutes))
	}
	for file, handler := range map[string]string{"task_http.go": "handleRuntimeTaskList", "session_activity_http.go": "handleSessionActivityList"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(source)
		start := strings.Index(body, "\nfunc "+handler+"(")
		if start < 0 {
			t.Fatalf("%s: %s not found", file, handler)
		}
		body = body[start+1:]
		if end := strings.Index(body, "\nfunc "); end >= 0 {
			body = body[:end]
		}
		if n := strings.Count(body, "http.StatusServiceUnavailable") + strings.Count(body, "503") +
			strings.Count(body, "writeGovernorUnavailable"); n != 1 {
			t.Errorf("%s has %d 503s, want exactly the nil-service one", handler, n)
		}
	}
}
