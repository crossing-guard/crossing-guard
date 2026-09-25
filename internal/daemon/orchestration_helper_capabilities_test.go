package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

func TestHelperWildcardSelectorsUseCatalogAndExplicitOverride(t *testing.T) {
	profile := profilefs.CompiledProfile{Instructions: "default", Trigger: profilefs.CompiledTrigger{Event: "*"}}
	selectors := profileSignalSelectors(profile)
	// The wildcard takes every kind except those a session-scoped kind
	// supersedes (plan H3): one fact, one fire on both paths.
	superseded := 0
	for _, signal := range orchestration.SignalCatalog() {
		if signal.Superseded != "" {
			superseded++
			if _, selected := selectors[signal.Kind]; selected {
				t.Fatalf("wildcard selected superseded %s", signal.Kind)
			}
		}
	}
	if superseded == 0 || len(selectors) != len(orchestration.SignalCatalog())-superseded {
		t.Fatal(selectors)
	}
	profile.Stages = map[string]string{"*": "fallback", "task.completed": "specific"}
	selectors = profileSignalSelectors(profile)
	if selectors["task.completed"] != "specific" || selectors["session.tool-completed"] != "fallback" {
		t.Fatal(selectors)
	}
	if _, selected := selectors["task.tool-completed"]; selected {
		t.Fatal("explicit override must not resurrect a superseded kind under the wildcard")
	}
	if _, ok := selectors["*"]; ok {
		t.Fatal("unexpanded wildcard")
	}
}

func TestHelperTaskContextPinsCutoffAndDisclosesBounds(t *testing.T) {
	f := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("unused") }})
	task, _, _, err := f.tasks.repository.Create(taskCreateRecord{ID: "context-test", ConsoleScope: "test", IdempotencyKey: "context-test", RequestDigest: "context-test", Runtime: "managed-fixture", CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(kind, text string) TaskEvent {
		e, err := f.tasks.repository.Append(task.ID, kind, "test", map[string]any{"text": text}, time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	appendEvent("message.completed", "Earlier decision")
	cutoff := appendEvent("tool.completed", "Observed tool result")
	appendEvent("message.completed", "Future secret")
	body, err := f.host.tasks.taskMessagesContext(task.ID, cutoff.Sequence, 2048)
	if err != nil || !strings.Contains(body, "Observed tool result") || strings.Contains(body, "Future secret") || !strings.Contains(body, "original user input") {
		t.Fatalf("%s %v", body, err)
	}
	final, err := f.host.tasks.taskMessageThrough(task.ID, cutoff.Sequence)
	if err != nil || final != "Earlier decision" {
		t.Fatalf("%s %v", final, err)
	}
	last := appendEvent("tool.completed", strings.Repeat("界", 2000))
	body, err = f.host.tasks.taskMessagesContext(task.ID, last.Sequence, 256)
	encodedBody, _ := json.Marshal(body)
	if err != nil || len(encodedBody) > 256 || !json.Valid([]byte(body)) || !strings.Contains(body, "omitted_events") {
		t.Fatalf("%s %v", body, err)
	}
	if _, err = f.host.tasks.taskMessagesContext("natural:codex:test", 1, 2048); err == nil {
		t.Fatal("natural context invented")
	}
	if _, err = f.host.tasks.taskMessagesContext(task.ID, last.Sequence, 1); err == nil {
		t.Fatal("impossible bound accepted")
	}
	profile := profilefs.CompiledProfile{Context: []profilefs.CompiledContext{{Kind: "task.messages", Required: true, MaxBytes: 2048}}}
	if _, err = f.host.agentPromptContext(profile, &store.ManagedGroup{}, orchestration.ManagedSource{TaskID: "natural:test", Sequence: 1}); err == nil {
		t.Fatal("required context silently absent")
	}
}

type deliveryFixtureDriver struct {
	managedDynamicFixtureDriver
	mu                sync.Mutex
	targets, messages []string
}

func (d *deliveryFixtureDriver) ChatCapability() ChatCapability {
	capability := d.managedDynamicFixtureDriver.ChatCapability()
	capability.MessageDelivery = SessionMessageCapability{Supported: true, Boundary: "fixture", Detail: "test transport"}
	return capability
}

func (d *deliveryFixtureDriver) DeliverSessionMessage(_ context.Context, target SessionIdentity, message string) SessionMessageReceipt {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targets = append(d.targets, target.NativeID)
	d.messages = append(d.messages, message)
	return SessionMessageReceipt{State: "accepted", MessageID: "fixture-receipt"}
}

func TestHelperDeliveryUsesExactSourceAndCompletedReplayDoesNotResend(t *testing.T) {
	driver := &deliveryFixtureDriver{managedDynamicFixtureDriver: managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(`{"action":"send_message","message":"Remember the architecture decision.","citations":["source.final_message"]}`)
		}
		return jsonTextCommand("Source design question")
	}}}
	f := newAgentHostFixture(t, driver)
	authored := strings.ReplaceAll(string(courseCorrectorProfileSource()), "request-interrupt", "send-message")
	profile := selectManagedProfile(t, f.owner, []byte(authored))
	binding, err := f.host.putBinding(managedBindingCommand{BindingID: "agent-delivery", ProfileID: profile.ProfileID, ProfileSourceDigest: profile.SourceDigest, ProfileBundleDigest: profile.BundleDigest, ProjectRoot: f.root, Runtime: "managed-fixture", GrantedAuthority: []string{"send-message"}, AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-delivery")})
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := f.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "Source", SessionID: "exact-native-source", Cwd: f.root}, "delivery-source")
	if err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, f.host, func(runs []store.ManagedRun) bool {
		for _, r := range runs {
			if r.Detail["delivery"] != nil {
				v, _ := r.Detail["delivery"].(map[string]any)
				if v["state"] == "accepted" {
					return true
				}
			}
		}
		return false
	})
	var run store.ManagedRun
	for _, r := range runs {
		if r.Action == "send_message" {
			run = r
			break
		}
	}
	if err = f.host.finishManagedChild(run, TaskEvent{TaskID: run.ChildTaskID, Kind: "task.completed"}); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	if len(driver.targets) != 1 || driver.targets[0] != "exact-native-source" || !strings.Contains(driver.messages[0], run.RunID) || !strings.Contains(driver.messages[0], "not operator authorization") {
		t.Errorf("%v %v", driver.targets, driver.messages)
	}
	driver.mu.Unlock()
	if run.Detail["context_coverage"] == nil {
		t.Fatal("settlement discarded context coverage")
	}
	if run.SourceTaskID != source.ID {
		t.Fatal(run)
	}
	if reason := f.host.automaticActionDenied(run, "request_interrupt"); reason != "authority_not_granted" {
		t.Fatal(reason)
	}
	stale := run
	stale.BindingStateToken = "old"
	if reason := f.host.automaticActionDenied(stale, "send_message"); reason != "binding_changed" {
		t.Fatal(reason)
	}
	if _, err = f.host.ix.DisableManagedBinding(binding.BindingID, binding.StateToken, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if reason := f.host.automaticActionDenied(run, "send_message"); reason != "binding_changed" {
		t.Fatal(reason)
	}
}

func TestHelperCodexDeliveryExactCommandAndAcknowledgement(t *testing.T) {
	const target = "01a09633-9b2d-7a22-a6cf-e370c30cb22e"
	const messageID = "01a09639-a7cd-7830-bca8-1ff8f8992032"
	root := t.TempDir()
	// The executable checks each argument separately, including literal shell syntax.
	script := "#!/bin/sh\n[ \"$1\" = queue ] && [ \"$2\" = --thread ] && [ \"$3\" = " + target + " ] && [ \"$4\" = --message ] && [ \"$5\" = 'literal $(no-shell); message' ] || exit 2\necho 'Queued message " + messageID + " for thread " + target + ".'\n"
	if err := os.WriteFile(filepath.Join(root, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	got := (codexChatDriver{}).DeliverSessionMessage(context.Background(), SessionIdentity{Runtime: "codex", NativeID: target}, "literal $(no-shell); message")
	if got.State != "accepted" || got.MessageID != messageID {
		t.Fatal(got)
	}
	for _, output := range []string{"", "Queued message " + messageID + " for thread other.", "noise\nQueued message " + messageID + " for thread " + target + "."} {
		if got := codexQueueReceipt(target, output); got.State != "unknown" {
			t.Fatal(got)
		}
	}
	if got := (codexChatDriver{}).DeliverSessionMessage(context.Background(), SessionIdentity{Runtime: "codex", NativeID: "ambiguous title"}, "hello"); got.State != "unavailable" {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := (codexChatDriver{}).DeliverSessionMessage(ctx, SessionIdentity{Runtime: "codex", NativeID: target}, "hello"); got.State != "unavailable" {
		t.Fatal(got)
	}
}

func TestHelperDeliveryUnsupportedAndOutputBounds(t *testing.T) {
	f := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("source") }})
	f.bindHelper(t, "agent-test", 1, false, []string{"reply"}, store.ManagedLimits{})
	source, _, err := f.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "source", Cwd: f.root}, "unsupported-source")
	if err != nil {
		t.Fatal(err)
	}
	runs := waitForRuns(t, f.host, func(runs []store.ManagedRun) bool { return len(runs) > 0 && runs[0].State == "failed" })
	run := runs[0]
	run.SourceTaskID = source.ID
	if err = f.host.deliverSourceMessage(run, "message"); err != nil {
		t.Fatal(err)
	}
	saved, _, err := f.host.ix.ManagedRun(run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := saved.Detail["delivery"].(map[string]any)
	if receipt["state"] != "unavailable" {
		t.Fatal(saved.Detail)
	}
	output := &codexDeliveryOutput{}
	if n, err := output.Write([]byte(strings.Repeat("x", 10000))); err != nil || n != 10000 || output.buf.Len() != 8192 || !output.overflow {
		t.Fatalf("%d %v %+v", n, err, output)
	}
}

func TestHelperUngrantedInterruptClaimCannotReachTaskControl(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "helper agent") {
			return jsonTextCommand(`{"action":"request_interrupt","message":"Stop it.","citations":["source.final_message"]}`)
		}
		return jsonTextCommand("Read-only source")
	}}
	f := newAgentHostFixture(t, driver)
	f.bindHelper(t, "agent-reply-only", 1, true, []string{"reply"}, store.ManagedLimits{})
	if _, _, err := f.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "source", Cwd: f.root}, "ungranted-interrupt"); err != nil {
		t.Fatal(err)
	}
	waitForRuns(t, f.host, func(runs []store.ManagedRun) bool {
		for _, r := range runs {
			if r.Detail["auto_action_suppressed"] == "authority_not_granted" {
				return true
			}
		}
		return false
	})
	controls, err := f.host.ix.ManagedControls(10)
	if err != nil || len(controls) != 0 {
		t.Fatalf("%+v %v", controls, err)
	}
}

func TestHelperCodexDeliveryInterruptedCommandIsUnknown(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "codex"), []byte("#!/bin/sh\n/bin/sleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	receipt := (codexChatDriver{}).DeliverSessionMessage(ctx, SessionIdentity{Runtime: "codex", NativeID: "01a09633-9b2d-7a22-a6cf-e370c30cb22e"}, "message")
	if receipt.State != "unknown" {
		t.Fatal(receipt)
	}
}

func TestHelperContextRequiredEmptyUnavailableAndEncodedBounds(t *testing.T) {
	f := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("unused") }})
	task, _, _, err := f.tasks.repository.Create(taskCreateRecord{ID: "empty-context", ConsoleScope: "test", IdempotencyKey: "empty-context", RequestDigest: "empty-context", Runtime: "managed-fixture", CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	event, err := f.tasks.repository.Append(task.ID, "message.completed", "test", map[string]any{"text": ""}, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	request := orchestration.ContextRequest{Source: orchestration.ManagedSource{TaskID: task.ID, Sequence: event.Sequence}, Selections: []orchestration.ContextSelection{{Kind: "task.final-response", Required: true, MaxBytes: 128}, {Kind: "session.tags", Required: true, MaxBytes: 128}}}
	envelope, err := f.host.contextReader.ReadContext(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, coverage := range envelope.Coverage {
		if coverage.State != "empty" || coverage.ReadAt == 0 {
			t.Fatalf("%+v", coverage)
		}
	}
	request.Source.TaskID = "missing"
	envelope, err = f.host.contextReader.ReadContext(context.Background(), request)
	if err == nil || envelope.Coverage[len(envelope.Coverage)-1].State != "unavailable" {
		t.Fatalf("%+v %v", envelope, err)
	}
	for _, body := range []string{strings.Repeat("界", 1000), strings.Repeat("\n\"<", 1000), `{"valid":"json"}`} {
		bounded, truncated := boundedContextText(body, 24)
		encoded, err := json.Marshal(bounded)
		if err != nil || len(encoded) > 24 || (!truncated && len(body) > 24) {
			t.Fatalf("%q %v", bounded, err)
		}
	}
}

func TestHelperLastNonemptyMessageSharedWithChildProjection(t *testing.T) {
	f := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("unused") }})
	task, _, _, err := f.tasks.repository.Create(taskCreateRecord{ID: "last-nonempty", ConsoleScope: "test", IdempotencyKey: "last-nonempty", RequestDigest: "last-nonempty", Runtime: "managed-fixture", CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"usable claim", ""} {
		if _, err := f.tasks.repository.Append(task.ID, "message.completed", "test", map[string]any{"text": body}, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	body, err := f.host.finalTaskMessage(task.ID)
	if err != nil || body != "usable claim" {
		t.Fatalf("%q %v", body, err)
	}
}

func TestHelperUnknownActionFailsClosed(t *testing.T) {
	var host orchestrationManagedHost
	if host.automaticActionDenied(store.ManagedRun{}, "invented_action") != "unknown_action" {
		t.Fatal("unknown action allowed")
	}
	for _, action := range []string{"no_action", "advise_user", "draft_reply"} {
		if host.automaticActionDenied(store.ManagedRun{}, action) != "" {
			t.Fatal(action)
		}
	}
}

func TestHelperSourceCapabilityIndependentOfHelperRoute(t *testing.T) {
	f := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("unused") }})
	chatDrivers["codex"] = codexChatDriver{}
	chatDrivers["claude"] = claudeChatDriver{}
	chatDrivers["opencode"] = openCodeChatDriver{}
	source := strings.ReplaceAll(string(helperAgentProfileSource()), "kind: draft-reply", "kind: intervention")
	source = strings.ReplaceAll(source, "  - reply", "  - send-message")
	profile := selectManagedProfile(t, f.owner, []byte(source))
	for _, helper := range []string{"codex", "claude", "opencode"} {
		mode := ""
		if helper == "claude" {
			mode = "plan"
		}
		for _, source := range []string{"codex", "claude", "opencode", ""} {
			id := "agent-" + helper + "-" + source + "-test"
			_, err := f.host.putBinding(managedBindingCommand{BindingID: id, ProfileID: profile.ProfileID, ProfileSourceDigest: profile.SourceDigest, ProfileBundleDigest: profile.BundleDigest, ProjectRoot: f.root, Runtime: helper, Mode: mode, ScopeRuntime: source, GrantedAuthority: []string{"send-message"}, AutoAction: true, ExpectedStateToken: store.ManagedBindingAbsentToken(id)})
			// Every registered runtime now publishes a delivery transport
			// (Codex queue; Claude and OpenCode the boundary carrier), so a
			// send-message grant is accepted for each source scope.
			if err != nil {
				t.Fatalf("helper=%s source=%s err=%v", helper, source, err)
			}
		}
	}
}

type fixtureContextReader struct{ request orchestration.ContextRequest }

func (reader *fixtureContextReader) ReadContext(_ context.Context, request orchestration.ContextRequest) (orchestration.ContextEnvelope, error) {
	reader.request = request
	return orchestration.ContextEnvelope{Source: request.Source, Items: []orchestration.PromptContext{{Label: "fixture", Body: "substituted"}}}, nil
}

func TestHelperContextPortCanSubstituteWithoutStores(t *testing.T) {
	reader := &fixtureContextReader{}
	host := orchestrationManagedHost{ctx: context.Background(), contextReader: reader}
	source := orchestration.ManagedSource{TaskID: "task", Sequence: 42}
	envelope, err := host.agentPromptContext(profilefs.CompiledProfile{Context: []profilefs.CompiledContext{{Kind: "session.tags", MaxBytes: 10}}}, &store.ManagedGroup{GroupID: "group", RootCatalogSessionID: "catalog"}, source)
	if err != nil || reader.request.Source.Sequence != 42 || reader.request.SessionID != "catalog" || envelope.Items[0].Body != "substituted" {
		t.Fatalf("%+v %v", envelope, err)
	}
}

func TestHelperContextEmptyWindowAndImpossibleBound(t *testing.T) {
	f := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("unused") }})
	task, _, _, err := f.tasks.repository.Create(taskCreateRecord{ID: "empty-window", ConsoleScope: "test", IdempotencyKey: "empty-window", RequestDigest: "empty-window", Runtime: "managed-fixture", CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	event, err := f.tasks.repository.Append(task.ID, "task.running", "test", nil, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	request := orchestration.ContextRequest{Source: orchestration.ManagedSource{TaskID: task.ID, Sequence: event.Sequence}, Selections: []orchestration.ContextSelection{{Kind: "task.messages", Required: true, MaxBytes: 512}}}
	envelope, err := f.host.contextReader.ReadContext(context.Background(), request)
	if err != nil || envelope.Coverage[1].State != "empty" {
		t.Fatalf("%+v %v", envelope, err)
	}
	request.Selections = []orchestration.ContextSelection{{Kind: "session.tags", Required: true, MaxBytes: 1}}
	if _, err := f.host.contextReader.ReadContext(context.Background(), request); err == nil {
		t.Fatal("impossible JSON string bound accepted")
	}
}
