package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"crossing-guard/internal/taskinput"
)

// Fake runtimes with deliberately alien schemes (design §10). No framework
// test names a real runtime except to prove the image rule leaves it alone.

type fakeModelDriver struct {
	discover func(context.Context, ChatModelEnv) (ChatModelDiscovery, error)
	models   []ChatModelOption
	inputs   []ChatInputCapability
}

func (fakeModelDriver) BuildCmd(ChatRequest, ChatLaunchContext) (*exec.Cmd, error) {
	return nil, errors.New("unused")
}
func (fakeModelDriver) ProjectEvent(map[string]any) []ChatEvent { return nil }
func (d fakeModelDriver) ChatCapability() ChatCapability {
	return ChatCapability{Runtime: "fakealpha", DisplayName: "Alpha", Models: d.models, Inputs: d.inputs,
		Modes: []ChatMode{{ID: "", Label: "Default", Risk: "normal"}}}
}

type fakeDiscoveringDriver struct{ fakeModelDriver }

func (d fakeDiscoveringDriver) DiscoverChatModels(ctx context.Context, env ChatModelEnv) (ChatModelDiscovery, error) {
	return d.discover(ctx, env)
}

func withFakeRuntime(t *testing.T, name string, driver ChatDriver) {
	t.Helper()
	previous, had := chatDrivers[name]
	chatDrivers[name] = driver
	t.Cleanup(func() {
		if had {
			chatDrivers[name] = previous
		} else {
			delete(chatDrivers, name)
		}
	})
}

func testModelBounds() ConsoleModelsDefaults {
	return ConsoleModelsDefaults{RefreshAfterSeconds: 60, MinRefreshSeconds: 5, DiscoveryTimeoutSeconds: 5,
		DiscoveryOutputMaxBytes: 1 << 20, MaxEntries: 50, MaxIDBytes: 256, MaxLabelBytes: 200}
}

func newTestModelCache(t *testing.T, now *time.Time) *chatModelCache {
	dir := t.TempDir()
	return &chatModelCache{entries: map[string]*chatModelCacheEntry{}, now: func() time.Time { return *now },
		bounds: testModelBounds, workDir: func() string { return filepath.Join(dir, "discovery") }}
}

func alphaModels() []ChatModelOption {
	price := &ChatModelPrice{Unit: "credits", PerTokens: 1000, Rates: []ChatModelRate{
		{Class: "input", Amount: 2}, {Class: "audio-in", Label: "Audio in", Amount: 9}}}
	return []ChatModelOption{
		{ID: "a|b::c", Label: "Pipe model", Group: "g1", GroupLabel: "<b>Group</b>", Inputs: []string{"text", "image"}, Price: price},
		{ID: "m1", Label: "M1 in g1", Group: "g1"},
		{ID: "m1", Label: "M1 again (duplicate id)", Group: "g2"},
		{ID: "custom", Label: "Collides with a declared id"},
		{ID: "", Label: "Collides with the declared default"},
		{ID: "odd ?#&\"<>/ ü", Label: "Special characters"},
	}
}

func TestChatModelCacheDiscoversValidatesAndRefusesCollisions(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cache := newTestModelCache(t, &now)
	withFakeRuntime(t, "fakealpha", fakeDiscoveringDriver{fakeModelDriver{
		models: []ChatModelOption{{ID: "", Label: "Default"}, {ID: "custom", Label: "Exact…", Custom: true}},
		discover: func(context.Context, ChatModelEnv) (ChatModelDiscovery, error) {
			return ChatModelDiscovery{Models: alphaModels(), Scope: "alpha scope", Binary: "/opaque/bin", Rejected: 1}, nil
		}}})
	list := cache.List("fakealpha")
	if list.State != modelStateFresh || list.Digest == "" || list.ObservedAt == nil || list.Scope != "alpha scope" {
		t.Fatalf("list: %+v", list)
	}
	ids := []string{}
	for _, model := range list.Models {
		ids = append(ids, model.ID)
		if model.Source != chatModelSourceRuntime {
			t.Fatalf("discovered entry without runtime source: %+v", model)
		}
	}
	if strings.Join(ids, ",") != `a|b::c,m1,odd ?#&"<>/ ü` || list.Rejected != 4 {
		t.Fatalf("ids=%q rejected=%d (1 adapter + duplicate + custom + empty)", ids, list.Rejected)
	}
	if model, ok := cache.Model("fakealpha", `odd ?#&"<>/ ü`); !ok || model.Label != "Special characters" {
		t.Fatal("an id with special characters did not round-trip byte for byte")
	}
	if list := cache.List("nosuchdriver"); list.State != modelStateUnsupported {
		t.Fatalf("a runtime without the port must be unsupported: %+v", list)
	}
}

func TestChatModelCacheStaleWhileRevalidateAndKeepsLastGoodList(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cache := newTestModelCache(t, &now)
	var calls atomic.Int32
	var failing atomic.Bool
	withFakeRuntime(t, "fakealpha", fakeDiscoveringDriver{fakeModelDriver{
		discover: func(context.Context, ChatModelEnv) (ChatModelDiscovery, error) {
			calls.Add(1)
			if failing.Load() {
				return ChatModelDiscovery{}, modelDiscoveryFailure(modelReasonExited)
			}
			return ChatModelDiscovery{Models: []ChatModelOption{{ID: "m1", Label: "M1"}}}, nil
		}}})
	if list := cache.List("fakealpha"); list.State != modelStateFresh || calls.Load() != 1 {
		t.Fatalf("first read: %+v calls=%d", list, calls.Load())
	}
	cache.List("fakealpha")
	if calls.Load() != 1 {
		t.Fatal("a fresh list must be served from the cache")
	}
	// Explicit refresh inside min_refresh_seconds does not rerun.
	cache.Refresh("fakealpha")
	if calls.Load() != 1 {
		t.Fatal("refresh was not rate-limited")
	}
	failing.Store(true)
	now = now.Add(2 * time.Minute)
	if list := cache.List("fakealpha"); list.State != modelStateStale || len(list.Models) != 1 {
		t.Fatalf("an old list must be served stale while revalidating: %+v", list)
	}
	for deadline := time.Now().Add(2 * time.Second); calls.Load() < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not run")
		}
	}
	waitForModelRefresh(t, cache, "fakealpha")
	list := cache.snapshot("fakealpha")
	if list.State != modelStateStale || list.ReasonCode != modelReasonExited || len(list.Models) != 1 {
		t.Fatalf("a failed refresh must keep the last good list, stale, with the reason: %+v", list)
	}
	if _, ok := cache.Model("fakealpha", "m1"); !ok {
		t.Fatal("a stale good list still answers model lookups")
	}
	// P-RT4: the failed refresh left the list old, but the next read inside
	// min_refresh_seconds must not start another discovery.
	cache.List("fakealpha")
	waitForModelRefresh(t, cache, "fakealpha")
	if calls.Load() != 2 {
		t.Fatalf("a failing runtime was rediscovered on every read: calls=%d", calls.Load())
	}
}

func waitForModelRefresh(t *testing.T, cache *chatModelCache, runtime string) {
	t.Helper()
	cache.mu.Lock()
	running := cache.entries[runtime].running
	cache.mu.Unlock()
	if running == nil {
		return
	}
	select {
	case <-running:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not finish")
	}
}

func TestChatModelCacheColdFailureIsUnavailableAndFailsClosed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cache := newTestModelCache(t, &now)
	withFakeRuntime(t, "fakealpha", fakeDiscoveringDriver{fakeModelDriver{
		discover: func(context.Context, ChatModelEnv) (ChatModelDiscovery, error) {
			return ChatModelDiscovery{}, errors.New("vendor said: key sk-SECRET is invalid")
		}}})
	list := cache.List("fakealpha")
	encoded, _ := json.Marshal(list)
	if list.State != modelStateUnavailable || list.ReasonCode != modelReasonUnparseable || strings.Contains(string(encoded), "SECRET") {
		t.Fatalf("cold failure: %s", encoded)
	}
	if _, ok := cache.Model("fakealpha", "anything"); ok {
		t.Fatal("a cold cache must fail closed")
	}
}

func TestChatModelCacheRefusesTooManyEntriesRatherThanTruncating(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cache := newTestModelCache(t, &now)
	many := make([]ChatModelOption, 51)
	for i := range many {
		many[i] = ChatModelOption{ID: "m" + string(rune('A'+i)), Label: "x"}
	}
	withFakeRuntime(t, "fakealpha", fakeDiscoveringDriver{fakeModelDriver{
		discover: func(context.Context, ChatModelEnv) (ChatModelDiscovery, error) {
			return ChatModelDiscovery{Models: many}, nil
		}}})
	if list := cache.List("fakealpha"); list.State != modelStateUnavailable || list.ReasonCode != modelReasonTooManyEntries || len(list.Models) != 0 {
		t.Fatalf("list: %+v", list)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRunner(t *testing.T, timeout time.Duration, maxBytes int64) (boundedRunner, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return boundedRunner{ctx: ctx, workDir: filepath.Join(t.TempDir(), "work"), maxBytes: maxBytes,
		waitDelay: 200 * time.Millisecond}, cancel
}

func TestBoundedRunnerCapsKillsAndDiscardsStderr(t *testing.T) {
	runner, cancel := testRunner(t, 3*time.Second, 4096)
	defer cancel()
	out, err := runner.Run(exec.Command(writeScript(t, `echo out; echo secret-on-stderr >&2; pwd`)))
	if err != nil || !strings.Contains(string(out), "out") || strings.Contains(string(out), "secret") ||
		!strings.Contains(string(out), filepath.Base(runner.workDir)) {
		t.Fatalf("out=%q err=%v (stderr must be discarded; the runner owns the directory)", out, err)
	}
	started := time.Now()
	if _, err := runner.Run(exec.Command(writeScript(t, `while :; do echo endless; done`))); modelDiscoveryReason(err) != modelReasonTooLarge {
		t.Fatalf("endless output: %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("output cap waited for the deadline")
	}
	if _, err := runner.Run(exec.Command(writeScript(t, `exit 3`))); modelDiscoveryReason(err) != modelReasonExited {
		t.Fatalf("exit: %v", err)
	}
	if _, err := runner.Run(exec.Command(filepath.Join(t.TempDir(), "missing"))); modelDiscoveryReason(err) != modelReasonNotInstalled {
		t.Fatalf("missing: %v", err)
	}
	short, cancelShort := testRunner(t, 300*time.Millisecond, 64)
	defer cancelShort()
	started = time.Now()
	if _, err := short.Run(exec.Command(writeScript(t, `sleep 30 & sleep 30`))); modelDiscoveryReason(err) != modelReasonTimedOut {
		t.Fatalf("hung: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("the deadline did not end a hung process group")
	}
}

func TestBoundedRunnerRefusesAWorkDirInsideARepository(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runner := boundedRunner{ctx: ctx, workDir: filepath.Join(repo, "data", "model-discovery"), maxBytes: 64}
	if _, err := runner.Run(exec.Command(writeScript(t, `echo x`))); modelDiscoveryReason(err) != modelReasonWorkDirInRepo {
		t.Fatalf("err = %v", err)
	}
}

func TestBoundedRunnerSessionExchangesLinesWithinBounds(t *testing.T) {
	runner, cancel := testRunner(t, 3*time.Second, 1024)
	defer cancel()
	session, err := runner.Session(exec.Command(writeScript(t, `while read line; do echo "reply:$line"; done`)))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Stdin.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if line := <-session.Lines; string(line) != "reply:ping" {
		t.Fatalf("line = %q", line)
	}
	session.Close()
	for range session.Lines {
	}

	small, cancelSmall := testRunner(t, 3*time.Second, 16)
	defer cancelSmall()
	flood, err := small.Session(exec.Command(writeScript(t, `while :; do echo 0123456789; done`)))
	if err != nil {
		t.Fatal(err)
	}
	for range flood.Lines {
	}
	if modelDiscoveryReason(flood.Err()) != modelReasonTooLarge {
		t.Fatalf("session cap: %v", flood.Err())
	}
}

func imageInputs() []taskinput.ResolvedInput {
	return []taskinput.ResolvedInput{{Input: taskinput.Input{Kind: taskinput.KindImage}, Path: "/opaque/x.png"}}
}

func TestModelConditionalInputRuleIsScopedToDiscoveringRuntimes(t *testing.T) {
	listed := map[string]ChatModelOption{
		"vision": {ID: "vision", Label: "V", Source: chatModelSourceRuntime, Inputs: []string{"text", "image"}},
		"text":   {ID: "text", Label: "T", Source: chatModelSourceRuntime, Inputs: []string{"text"}},
	}
	service := &TaskApplicationService{models: func(_, id string) (ChatModelOption, bool) {
		model, ok := listed[id]
		return model, ok
	}}
	discovering := fakeDiscoveringDriver{fakeModelDriver{inputs: []ChatInputCapability{
		{Kind: "image", MediaTypes: []string{"image/png"}, ModelConditional: true, CatalogRequired: true}}}}
	cases := []struct {
		model string
		ok    bool
	}{{"vision", true}, {"text", false}, {"typed-not-listed", false}, {"", true}}
	for _, c := range cases {
		err := service.admitModelConditionalInputs(discovering, ChatRequest{Runtime: "fakealpha", Model: c.model}, imageInputs())
		if (err == nil) != c.ok {
			t.Fatalf("model %q: err = %v", c.model, err)
		}
	}
	// A cold cache refuses a concrete model.
	cold := &TaskApplicationService{models: func(string, string) (ChatModelOption, bool) { return ChatModelOption{}, false }}
	if err := cold.admitModelConditionalInputs(discovering, ChatRequest{Model: "vision"}, imageInputs()); err == nil {
		t.Fatal("a cold catalog must fail closed")
	}
	// A-RT1: runtimes without the port keep their own validator as the only
	// authority, whatever model they name.
	for _, driver := range []ChatDriver{claudeChatDriver{}, fakeModelDriver{}, fakeDiscoveringDriver{fakeModelDriver{inputs: []ChatInputCapability{{Kind: "image", ModelConditional: true}}}}} {
		if err := cold.admitModelConditionalInputs(driver, ChatRequest{Model: "anything"}, imageInputs()); err != nil {
			t.Fatalf("%T: the catalog rule touched a runtime that does not discover models: %v", driver, err)
		}
	}
}

// Codex discovers models but refuses images for any named model; its own
// validator runs first, so its refusal (not a list message) is what the user
// sees, and a model-less image turn still passes the model-list rule.
func TestCodexImageAdmissionKeepsItsOwnRule(t *testing.T) {
	service := &TaskApplicationService{models: func(string, string) (ChatModelOption, bool) {
		return ChatModelOption{}, false
	}}
	if err := (codexChatDriver{}).ValidateChatInputs(ChatRequest{Model: "gpt-x"}, imageInputs()); err == nil ||
		!strings.Contains(err.Error(), "not verified") {
		t.Fatalf("named model: %v", err)
	}
	if err := (codexChatDriver{}).ValidateChatInputs(ChatRequest{}, imageInputs()); err != nil {
		t.Fatal(err)
	}
	if err := service.admitModelConditionalInputs(codexChatDriver{}, ChatRequest{Runtime: "codex"}, imageInputs()); err != nil {
		t.Fatalf("a model-less image turn must pass the list rule: %v", err)
	}
}
