package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/taskinput"
	"crossing-guard/store"
)

// The default installation has no task-inputs.json. The service must then be
// wired with a true nil claimer: a typed nil pointer would satisfy the port and
// every "unavailable" guard would dereference it on task completion.
func TestUnconfiguredTaskInputsCompleteTasksAndRejectInputRequests(t *testing.T) {
	previousService, previousProblem := taskInputs, taskInputsProblem
	taskInputs, taskInputsProblem = nil, "configure task-inputs.json"
	t.Cleanup(func() { taskInputs, taskInputsProblem = previousService, previousProblem })
	if configuredTaskInputs() != nil {
		t.Fatal("unconfigured task inputs must wire a true nil claimer")
	}

	dataDir := t.TempDir()
	index, err := store.Open(filepath.Join(dataDir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	launches := make(chan ChatLaunchContext, 1)
	driver := inputFixtureDriver{launches: launches}
	service := NewTaskApplicationServiceWithInputs(taskStoreRepository{index: index}, NewTaskExecutionRegistry(),
		NewTaskSubscriberHub(), func(runtime string) (ChatDriver, bool) { return driver, runtime == "fixture" },
		func(string, string) (bool, error) { return true, nil }, configuredTaskInputs(), dataDir)

	task, created, err := service.Create(ChatRequest{Runtime: "fixture", Prompt: "plain", Cwd: dataDir}, "fixture-no-inputs")
	if err != nil || !created {
		t.Fatalf("create=%+v created=%v err=%v", task, created, err)
	}
	<-launches
	wantTaskState(t, service, task.ID, TaskCompleted, time.Second)

	_, _, err = service.Create(ChatRequest{Runtime: "fixture", Prompt: "with input", Cwd: dataDir,
		InputScopeID: "scope_00000000000000000000000000000000", InputIDs: []string{"input_00000000000000000000000000000000"}},
		"fixture-input-unavailable")
	if err == nil || !strings.Contains(err.Error(), "task input admission is unavailable") {
		t.Fatalf("input-bearing create err=%v", err)
	}
	if err := service.RecoverLostTasks(); err != nil {
		t.Fatalf("recovery with unconfigured inputs: %v", err)
	}
}

// A text attachment is display bytes, never a document. The preview route must
// not let an extension-derived media type turn an admitted .html file into a
// page rendered on the daemon's own origin.
func TestTaskInputTextPreviewIsAlwaysPlainText(t *testing.T) {
	config := daemonTaskInputConfig()
	config.TextExtensions = append(config.TextExtensions, ".html")
	service, err := taskinput.NewService(t.TempDir()+"/inputs", config, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	previousService, previousConfig, previousProblem := taskInputs, taskInputsConfig, taskInputsProblem
	taskInputs, taskInputsConfig, taskInputsProblem = service, config, ""
	t.Cleanup(func() {
		taskInputs, taskInputsConfig, taskInputsProblem = previousService, previousConfig, previousProblem
	})

	prose := bytes.Repeat([]byte("plain prose that passes the sniff window. "), 20)
	page := append(prose, []byte("<script>document.title='owned'</script>")...)
	stage := httptest.NewRecorder()
	handleTaskInputStage(stage, multipartTaskInputRequest(t, "page.html", "picker", page))
	if stage.Code != http.StatusCreated {
		t.Fatalf("stage status=%d body=%s", stage.Code, stage.Body.String())
	}
	var result struct {
		Scope taskinput.Scope `json:"scope"`
		Input taskinput.Input `json:"input"`
	}
	if err := json.Unmarshal(stage.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Input.Kind != taskinput.KindText {
		t.Fatalf("stage result=%+v", result)
	}

	previewRequest := httptest.NewRequest("GET", "/api/task-inputs/"+result.Input.ID+"/content", nil)
	previewRequest.SetPathValue("input", result.Input.ID)
	previewRequest.Header.Set("X-CG-Input-Scope", result.Scope.ID)
	preview := httptest.NewRecorder()
	handleTaskInputPreview(preview, previewRequest)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	if got := preview.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("text preview content-type=%q", got)
	}
	if preview.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("preview headers=%v", preview.Header())
	}
}
