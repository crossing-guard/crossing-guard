package daemon

import (
	"context"
	"crossing-guard/store"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEffortNativeChoicesAndOpaqueVariants(t *testing.T) {
	capability := nativeEffortCapability([]string{"low", "high", "xhigh", "future"}, "high")
	if capability.State != "supported" || len(capability.Choices) != 3 || capability.Choices[2].Label != "Extra high" {
		t.Fatalf("capability %+v", capability)
	}
	variants := map[string]json.RawMessage{"quiet": json.RawMessage(`{"reasoningEffort":"high"}`), "high": json.RawMessage(`{"temperature":1}`), "mixed": json.RawMessage(`{"reasoningEffort":"low","temperature":1}`)}
	mapped := openCodeEffort(variants, nil)
	if len(mapped.Choices) != 1 || mapped.Choices[0].ID != "quiet" || mapped.Choices[0].Label != "High" {
		t.Fatalf("variant names were trusted: %+v", mapped)
	}
	altered := openCodeEffort(map[string]json.RawMessage{"quiet": json.RawMessage(`{"reasoningEffort":"low"}`)}, nil)
	if mapped.Choices[0].MappingDigest == altered.Choices[0].MappingDigest {
		t.Fatal("variant meaning drift was not detected")
	}
}

func TestEffortLegacyConflictsAndSemanticDigest(t *testing.T) {
	for _, test := range []struct {
		driver effortSyntaxParser
		raw    string
	}{{codexChatDriver{}, "--skip-git-repo-check -c model_reasoning_effort=high"}, {claudeChatDriver{}, "--max-turns 2 --effort high"}} {
		request := ChatRequest{Runtime: "fixture", Prompt: "p", Model: "m", ExtraArgs: test.raw}
		parsed, err := test.driver.ParseEffort(request)
		if err != nil || parsed.ThinkingEffort.Value != "high" || !parsed.effortLegacy {
			t.Fatalf("parse %+v %v", parsed, err)
		}
		if strings.Contains(parsed.ExtraArgs, "'") || strings.Contains(parsed.ExtraArgs, "effort") {
			t.Fatalf("other args corrupted: %s", parsed.ExtraArgs)
		}
		request.ThinkingEffort = &store.ThinkingEffort{Kind: "inherit"}
		_, err = test.driver.ParseEffort(request)
		var conflict *EffortError
		if !errors.As(err, &conflict) || conflict.Code != "legacy_conflict" {
			t.Fatalf("inherit conflict %v", err)
		}
		request.ThinkingEffort = &store.ThinkingEffort{Kind: "level", Value: "high"}
		explicit, err := test.driver.ParseEffort(request)
		if err != nil || explicit.effortLegacy {
			t.Fatalf("equal explicit %+v %v", explicit, err)
		}
		if taskRequestDigest(parsed) != taskRequestDigest(explicit) {
			t.Fatal("legacy-equivalent request changed digest")
		}
		explicit.SessionEffortToken = "99"
		if taskRequestDigest(parsed) != taskRequestDigest(explicit) {
			t.Fatal("mutable CAS token changed digest")
		}
	}
}

func TestEffortValidationAndCatalogIsolation(t *testing.T) {
	model := ChatModelOption{ID: "model", Effort: nativeEffortCapability([]string{"low", "high"}, "low")}
	request := ChatRequest{Model: "model", ThinkingEffort: &store.ThinkingEffort{Kind: "level", Value: "high"}}
	resolved, err := validateModelEffort(request, model, "catalog")
	if err != nil || requestedSettings(resolved).CatalogDigest != "catalog" {
		t.Fatalf("resolve %+v %v", resolved, err)
	}
	request.ThinkingEffort.Value = "max"
	if _, err := validateModelEffort(request, model, "catalog"); err == nil {
		t.Fatal("invented choice accepted")
	}
	clone := cloneChatModelList(ChatModelList{Models: []ChatModelOption{model}})
	clone.Models[0].Effort.Choices[0].ID = "changed"
	if model.Effort.Choices[0].ID != "low" {
		t.Fatal("caller mutated cached effort choices")
	}
	for _, effort := range []store.ThinkingEffort{{Kind: "inherit", Value: "high"}, {Kind: "level"}, {Kind: "level", Value: "a\nb"}, {Kind: "mystery"}} {
		if _, err := normalizeEffort(ChatRequest{ThinkingEffort: &effort}); err == nil {
			t.Fatalf("invalid effort accepted %+v", effort)
		}
	}
}

func TestEffortRetryDoesNotRevalidateMutableCatalog(t *testing.T) {
	withFakeRuntime(t, "effort-fixture", lifecycleFixtureDriver{command: `printf '%s\n' '{"text":"ok"}'`})
	service := installTestRuntimeTasks(t)
	calls := 0
	service.effort = func(req ChatRequest) (ChatRequest, error) {
		calls++
		if calls > 1 {
			return req, effortError("unavailable_evidence", "catalog changed")
		}
		return req, nil
	}
	req := ChatRequest{Runtime: "effort-fixture", Model: "model", Cwd: t.TempDir(), Prompt: "p", ThinkingEffort: &store.ThinkingEffort{Kind: "level", Value: "high"}}
	task, created, err := service.Create(req, "effort-idem")
	if err != nil || !created {
		t.Fatalf("first %+v %v", task, err)
	}
	retry, created, err := service.Create(req, "effort-idem")
	if err != nil || created || retry.ID != task.ID || calls != 1 {
		t.Fatalf("retry %+v %v calls=%d", retry, err, calls)
	}
	if retry.RequestedSettings == nil || retry.RequestedSettings.Effort.Value != "high" {
		t.Fatal("retry lost original snapshot")
	}
	if _, _, err = service.Create(req, "effort-new"); err == nil {
		t.Fatal("new admission ignored unavailable catalog")
	}
	// Await execution before the fixture closes its store.
	for i := 0; i < 200; i++ {
		latest, _, _ := service.Task(task.ID)
		if latest.Lifecycle == TaskCompleted || latest.Lifecycle == TaskFailed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fixture did not settle")
}

func TestEffortPreviewStrictAndPure(t *testing.T) {
	for _, body := range []string{`{"runtime":"claude","thinking_effort":{"kind":"level","value":"high","secret":"x"}}`, `{"runtime":"claude","extra_args":"--effort high","thinking_effort":{"kind":"inherit"}}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/chat/effort-preview", strings.NewReader(body))
		handleEffortPreview(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid preview accepted: %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handleEffortPreview(w, httptest.NewRequest("POST", "/api/chat/effort-preview", strings.NewReader(`{"runtime":"claude","extra_args":"--effort high"}`)))
	var snapshot store.TaskRequestedSettings
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &snapshot) != nil || snapshot.Source != "legacy" || snapshot.EffortLabel != "High" {
		t.Fatalf("preview %d %s", w.Code, w.Body.String())
	}
}

func TestOpenCodeEffortRejectsProjectVariantDrift(t *testing.T) {
	level := "high"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/providers" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": []any{map[string]any{"id": "fixture", "models": map[string]any{"model": map[string]any{"variants": map[string]any{"quiet": map[string]string{"reasoningEffort": level}}}}}}})
	}))
	defer server.Close()
	descriptor := openCodeEffort(map[string]json.RawMessage{"quiet": json.RawMessage(`{"reasoningEffort":"high"}`)}, nil)
	protocol := &openCodeServerProtocol{endpoint: server.URL, client: server.Client(), password: "fixture", request: ChatRequest{Model: "fixture/model", ThinkingEffort: &store.ThinkingEffort{Kind: "level", Value: "quiet"}, effortMapping: descriptor.Choices[0].MappingDigest}}
	if err := protocol.validateEffort(context.Background()); err != nil {
		t.Fatal(err)
	}
	level = "low"
	if err := protocol.validateEffort(context.Background()); err == nil {
		t.Fatal("project variant changed meaning without refusal")
	}
}

func TestEffortParentHandbackUsesSourceSettingsAcrossProviders(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(req ChatRequest) string {
		if req.Prompt == "root question" || req.Prompt == "Correct course" {
			return jsonTextCommand("Root done.")
		}
		return jsonTextCommand(helperClaimV2)
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.tasks.effort = func(req ChatRequest) (ChatRequest, error) { return req, nil }
	fixture.bindHelper(t, "handback", 10, false, nil, store.ManagedLimits{})
	source, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "source-native", Cwd: fixture.root, Model: "source-model", ThinkingEffort: &store.ThinkingEffort{Kind: "level", Value: "source-high"}}, "source-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, source.ID, TaskCompleted, 3*time.Second)
	run := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "completed" })[0]
	binding, found, err := fixture.host.ix.ManagedBinding("handback")
	if err != nil || !found {
		t.Fatalf("binding %v %v", found, err)
	}
	binding.Runtime = "another-provider"
	binding.Model = "helper-model"
	binding.ThinkingEffort = &store.ThinkingEffort{Kind: "level", Value: "helper-max"}
	savedBinding, err := fixture.host.ix.PutManagedBinding(binding, binding.StateToken, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	run.BindingStateToken = savedBinding.StateToken
	if err := fixture.host.resumeParent(run, "Correct course", "correction"); err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, r := range runs {
			if r.Kind == "correction" && r.ChildTaskID != "" {
				return true
			}
		}
		return false
	})
	for _, r := range runs {
		if r.Kind != "correction" {
			continue
		}
		resumed, found, err := fixture.tasks.Task(r.ChildTaskID)
		if err != nil || !found || resumed.Runtime != "managed-fixture" || resumed.RequestedSettings == nil || resumed.RequestedSettings.Model != "source-model" || resumed.RequestedSettings.Effort.Value != "source-high" {
			t.Fatalf("handback %+v %v %v", resumed, found, err)
		}
		wantTaskState(t, fixture.tasks, resumed.ID, TaskCompleted, 3*time.Second)
	}
}

func TestEffortRejectedFallbackKeepsAttemptEvidence(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	fixture := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(req ChatRequest) string {
		if req.Prompt == "root question" {
			return jsonTextCommand("Root done.")
		}
		return classifiedFailureCommand
	}})
	fixture.bindHelper(t, "fallback-evidence", 10, false, nil, store.ManagedLimits{})
	source, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "source", Cwd: fixture.root}, "fallback-evidence-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, source.ID, TaskCompleted, 3*time.Second)
	run := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "parked" })[0]
	binding, _, err := fixture.host.ix.ManagedBinding(run.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	binding.Routes = []store.ManagedRoute{{Runtime: "managed-fixture", Model: "fallback-model", ThinkingEffort: &store.ThinkingEffort{Kind: "level", Value: "opaque-high"}}}
	if _, err := fixture.host.ix.PutManagedBinding(binding, binding.StateToken, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	fixture.tasks.effort = func(req ChatRequest) (ChatRequest, error) {
		return req, effortError("invalid_choice", "choice removed")
	}
	ledger := providerOutageLedgerFrom(run.Detail)
	ledger.RouteIndex = 1
	if err := fixture.host.relaunchParkedRun(run, ledger); err != nil {
		t.Fatal(err)
	}
	settled, _, err := fixture.host.ix.ManagedRun(run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := providerOutageLedgerFrom(settled.Detail).Attempts
	last := attempts[len(attempts)-1]
	if settled.State != "failed" || settled.ErrorClass != "configuration" || last.Class != "configuration" || last.TaskID != "" || last.ThinkingEffort == nil || last.ThinkingEffort.Value != "opaque-high" || last.RouteIndex != 1 {
		t.Fatalf("settled %+v attempts %+v", settled, attempts)
	}
}
