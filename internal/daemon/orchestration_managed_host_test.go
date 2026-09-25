package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

type managedFixtureDriver struct{ command string }

type managedDynamicFixtureDriver struct{ commandFor func(ChatRequest) string }

func (driver managedFixtureDriver) BuildCmd(ChatRequest, ChatLaunchContext) (*exec.Cmd, error) {
	return exec.Command("/bin/sh", "-c", driver.command), nil
}

func TestManagedHTTPSettingsAndExplicitBindingLifecycle(t *testing.T) {
	root := t.TempDir()
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	source := followerProfileSource()
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Select(profilefs.SelectCommand{SourceName: "PROFILE.md", Source: source, ExpectedSourceDigest: preview.SourceDigest, ExpectedBundleDigest: preview.BundleDigest, ExpectedStateToken: preview.StateToken}); err != nil {
		t.Fatal(err)
	}
	ix, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(ix, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = ix.Close() })
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"managed-fixture": managedFixtureDriver{}}
	t.Cleanup(func() { chatDrivers = original })
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, host, owner)
	body, _ := json.Marshal(map[string]any{"profile_id": preview.ProfileID, "profile_source_digest": preview.SourceDigest, "profile_bundle_digest": preview.BundleDigest, "project_root": root, "runtime": "managed-fixture", "mode": "", "granted_authority": []string{"draft-reply"}, "auto_action": false, "expected_state_token": store.ManagedBindingAbsentToken("managed-follower"), "confirmed": true})
	request := httptest.NewRequest(http.MethodPut, "/api/orchestration/agents/managed-follower", bytes.NewReader(body))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/orchestration/managed/settings", nil)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", response.Code, response.Body.String())
	}
	var settings struct {
		Bindings     []store.ManagedBinding `json:"bindings"`
		RuntimeReady bool                   `json:"runtime_ready"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Bindings) != 1 || settings.Bindings[0].Role != "follower" || settings.RuntimeReady {
		t.Fatalf("settings=%+v", settings)
	}
}
func (managedFixtureDriver) ProjectEvent(object map[string]any) []ChatEvent {
	return []ChatEvent{{"type": "text", "text": anyString(object["text"])}}
}
func (managedFixtureDriver) ChatCapability() ChatCapability {
	return ChatCapability{Runtime: "managed-fixture", DisplayName: "Managed fixture", CanStart: true, CanResume: true, Modes: []ChatMode{{ID: "", Label: "Read only", Risk: "normal"}}, Models: []ChatModelOption{{ID: "", Label: "Default"}}}
}

func (driver managedDynamicFixtureDriver) BuildCmd(request ChatRequest, _ ChatLaunchContext) (*exec.Cmd, error) {
	return exec.Command("/bin/sh", "-c", driver.commandFor(request)), nil
}
func (managedDynamicFixtureDriver) ProjectEvent(object map[string]any) []ChatEvent {
	return []ChatEvent{{"type": "text", "text": anyString(object["text"])}}
}
func (managedDynamicFixtureDriver) ChatCapability() ChatCapability {
	return managedFixtureDriver{}.ChatCapability()
}

func followerProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: design-follower
version: "1.0.0"
name: Design follower
description: Reviews a completed coding turn and drafts a reply from project design guidance.
role: follower
execution: managed-turn
trigger:
  event: task.completed
context:
  - kind: task.final-response
    required: true
output:
  kind: draft-reply
authority-requests:
  - draft-reply
requirements:
  capabilities:
    - managed-turn
  destination:
    locality: local-only
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
  max-input-bytes: 65536
  max-output-bytes: 4096
  max-tokens: 512
  max-retries: 0
  max-concurrency: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Review the returned coding-agent response. Draft a concise answer grounded in the project design documents.
`)
}

func coordinatorProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: stage-coordinator
version: "1.0.0"
name: Stage coordinator
description: Classifies the completed stage and selects one exact allowlisted review helper.
role: coordinator
execution: managed-turn
trigger:
  event: task.completed
context:
  - kind: task.final-response
    required: true
output:
  kind: stage-classification
authority-requests:
  - launch-profile
allowed-profiles:
  - design-review-child
requirements:
  capabilities:
    - managed-turn
    - profile-launch
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Classify the completed project stage and choose only the allowlisted design review child.
`)
}

func delegateProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: design-review-child
version: "1.0.0"
name: Design review child
description: Reviews one coordinator classification against the project design.
role: delegate
execution: managed-turn
trigger:
  event: task.completed
context:
  - kind: task.final-response
    required: true
output:
  kind: advice
authority-requests:
  - advise
requirements:
  capabilities:
    - managed-turn
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Review the stage result against the project design and return concise advice.
`)
}

func courseCorrectorProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: course-corrector
version: "1.0.0"
name: Course corrector
description: Reviews committed progress and requests exact interruption when work diverges.
role: course-corrector
execution: managed-turn
trigger:
  event: task.message-completed
context:
  - kind: task.final-response
    required: true
output:
  kind: intervention
authority-requests:
  - request-interrupt
requirements:
  capabilities:
    - managed-turn
    - task-control
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Request interruption only when the latest committed message clearly diverges from the project design.
`)
}

func selectManagedProfile(t *testing.T, owner *profilefs.Owner, source []byte) profilefs.Preview {
	t.Helper()
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Select(profilefs.SelectCommand{SourceName: "PROFILE.md", Source: source, ExpectedSourceDigest: preview.SourceDigest, ExpectedBundleDigest: preview.BundleDigest, ExpectedStateToken: preview.StateToken}); err != nil {
		t.Fatal(err)
	}
	return preview
}

func TestManagedHostHelperConsumesDurableCompletionAndCreatesVisibleDraft(t *testing.T) {
	root := t.TempDir()
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	// draft_reply is a helper action on the unified claim wire; a passive
	// follower observes and tags only.
	source := helperAgentProfileSource()
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Select(profilefs.SelectCommand{SourceName: "PROFILE.md", Source: source, ExpectedSourceDigest: preview.SourceDigest, ExpectedBundleDigest: preview.BundleDigest, ExpectedStateToken: preview.StateToken}); err != nil {
		t.Fatal(err)
	}
	taskIndex, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer taskIndex.Close()
	original := chatDrivers
	claim := `{"action":"draft_reply","message":"Use ADR 0028 and keep orchestration above governance.","citations":["source.final_message"]}`
	chatDrivers = map[string]ChatDriver{"managed-fixture": managedFixtureDriver{command: "printf '%s\\n' '{\"text\":\"" + escapeShellJSON(claim) + "\"}'"}}
	t.Cleanup(func() { chatDrivers = original })
	tasks := NewTaskApplicationService(taskStoreRepository{index: taskIndex}, NewTaskExecutionRegistry(), NewTaskSubscriberHub(), registeredTaskRuntime, func(string, string) (bool, error) { return true, nil }, root)
	hostIndex, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(hostIndex, owner, tasks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = hostIndex.Close() })
	if _, err := host.putBinding(managedBindingCommand{BindingID: "managed-helper", ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: root, Runtime: "managed-fixture", Mode: "", ExpectedStateToken: store.ManagedBindingAbsentToken("managed-helper")}); err != nil {
		t.Fatal(err)
	}
	rootTask, _, err := tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "question", Cwd: root}, "managed-root-task")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		runs, readErr := host.ix.ManagedRuns(10)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(runs) == 1 && runs[0].State == "completed" {
			if runs[0].Action != "draft_reply" || runs[0].Message == "" || runs[0].ChildTaskID == "" {
				t.Fatalf("run=%+v", runs[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	runs, _ := host.ix.ManagedRuns(10)
	t.Fatalf("follower draft did not complete: %+v", runs)
}

// The legacy-authored role aliases (role: coordinator here) compile to the
// helper agent type; the binding, prompt, and claim all speak helper.
func TestManagedHostLegacyRoleProfileBindsAsHelperAndLaunchesOnlyPinnedChild(t *testing.T) {
	root := t.TempDir()
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	selectManagedProfile(t, owner, delegateProfileSource())
	launcher := selectManagedProfile(t, owner, coordinatorProfileSource())
	taskIndex, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer taskIndex.Close()
	original := chatDrivers
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		switch {
		case strings.Contains(request.Prompt, "child helper"):
			return jsonTextCommand("Child design review complete.")
		case strings.Contains(request.Prompt, "helper agent"):
			claim := `{"action":"launch_profile","message":"The design plan is ready for review.","citations":["source.final_message"],"stage_id":"design-review","child_profile_id":"design-review-child"}`
			return jsonTextCommand(claim)
		default:
			return jsonTextCommand("Design plan complete.")
		}
	}}
	chatDrivers = map[string]ChatDriver{"managed-fixture": driver}
	t.Cleanup(func() { chatDrivers = original })
	tasks := NewTaskApplicationService(taskStoreRepository{index: taskIndex}, NewTaskExecutionRegistry(), NewTaskSubscriberHub(), registeredTaskRuntime, func(string, string) (bool, error) { return true, nil }, root)
	hostIndex, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(hostIndex, owner, tasks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = hostIndex.Close() })
	if _, err := host.putBinding(managedBindingCommand{BindingID: "agent-launch", ProfileID: launcher.ProfileID, ProfileSourceDigest: launcher.SourceDigest, ProfileBundleDigest: launcher.BundleDigest, ProjectRoot: root, Runtime: "managed-fixture", GrantedAuthority: []string{"launch-profile"}, AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-launch")}); err != nil {
		t.Fatal(err)
	}
	rootTask, _, err := tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root stage", Cwd: root}, "launch-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, readErr := host.ix.ManagedRuns(10)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(runs) == 2 {
			var helperRun, child *store.ManagedRun
			for index := range runs {
				// Every binding-taxonomy run records role "helper"; the
				// launched child is distinguished by kind, never by role.
				if runs[index].Kind == "" {
					helperRun = &runs[index]
				}
				if runs[index].Kind == "delegate" {
					child = &runs[index]
				}
			}
			if helperRun != nil && child != nil && helperRun.Action == "launch_profile" && child.State == "completed" {
				if child.ProfileID != "design-review-child" {
					t.Fatalf("child=%+v", child)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	runs, _ := host.ix.ManagedRuns(10)
	t.Fatalf("helper child did not complete: %+v", runs)
}

// The legacy intervention alias (role: course-corrector) compiles to helper;
// the interrupt request and corrective resume flow through helper vocabulary.
func TestManagedHostHelperInterruptStoresRequestAndExactOutcome(t *testing.T) {
	root := t.TempDir()
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	profile := selectManagedProfile(t, owner, courseCorrectorProfileSource())
	taskIndex, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer taskIndex.Close()
	original := chatDrivers
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			claim := `{"action":"request_interrupt","message":"The latest step diverges from the architecture.","citations":["source.final_message"]}`
			return jsonTextCommand(claim)
		}
		if request.Prompt == "Correct course" {
			return jsonTextCommand("Corrective turn complete.")
		}
		return jsonTextCommand("Diverging implementation started.") + "; sleep 10"
	}}
	chatDrivers = map[string]ChatDriver{"managed-fixture": driver}
	t.Cleanup(func() { chatDrivers = original })
	tasks := NewTaskApplicationService(taskStoreRepository{index: taskIndex}, NewTaskExecutionRegistry(), NewTaskSubscriberHub(), registeredTaskRuntime, func(string, string) (bool, error) { return true, nil }, root)
	hostIndex, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(hostIndex, owner, tasks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = hostIndex.Close() })
	if _, err := host.putBinding(managedBindingCommand{BindingID: "agent-interrupt", ProfileID: profile.ProfileID, ProfileSourceDigest: profile.SourceDigest, ProfileBundleDigest: profile.BundleDigest, ProjectRoot: root, Runtime: "managed-fixture", GrantedAuthority: []string{"request-interrupt"}, AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-interrupt")}); err != nil {
		t.Fatal(err)
	}
	rootTask, _, err := tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "long root", SessionID: "native-course", Cwd: root}, "course-root")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		controls, readErr := host.ix.ManagedControls(10)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(controls) == 1 && controls[0].Outcome == "confirmed" {
			task := wantTaskState(t, tasks, rootTask.ID, TaskInterrupted, time.Second)
			if task.ID != controls[0].TaskID {
				t.Fatalf("task=%+v control=%+v", task, controls[0])
			}
			runs, _ := host.ix.ManagedRuns(10)
			var intervention *store.ManagedRun
			for index := range runs {
				// The legacy intervention profile binds as a helper agent.
				if runs[index].Role == "helper" {
					intervention = &runs[index]
					break
				}
			}
			if intervention == nil {
				t.Fatal("course-correction run missing")
			}
			if err := host.resumeParent(*intervention, "Correct course", "correction"); err != nil {
				t.Fatal(err)
			}
			resumeDeadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(resumeDeadline) {
				updated, _ := host.ix.ManagedRuns(10)
				for _, item := range updated {
					if item.Kind == "correction" && item.State == "completed" {
						return
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("corrective resume did not complete")
		}
		time.Sleep(20 * time.Millisecond)
	}
	controls, _ := host.ix.ManagedControls(10)
	runs, _ := host.ix.ManagedRuns(10)
	t.Fatalf("course correction did not confirm exact interrupt: controls=%+v runs=%+v", controls, runs)
}

func escapeShellJSON(value string) string {
	out := ""
	for _, char := range value {
		switch char {
		case '\\':
			out += "\\\\"
		case '"':
			out += "\\\""
		default:
			out += string(char)
		}
	}
	return out
}

func jsonTextCommand(text string) string {
	return "printf '%s\\n' '{\"text\":\"" + escapeShellJSON(text) + "\"}'"
}
