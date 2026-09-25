package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"crossing-guard/store"
)

// pinProviderRetryPolicy compresses the shipped windows so tests never sleep
// through real backoffs.
func pinProviderRetryPolicy(t *testing.T, policy func(role string) providerRetryPolicy) {
	t.Helper()
	previous := providerRetryPolicyFor
	providerRetryPolicyFor = policy
	t.Cleanup(func() { providerRetryPolicyFor = previous })
}

func immediateRetryPolicy(role string) providerRetryPolicy {
	return providerRetryPolicy{MaxSameRoute: 3, Backoff: 0, ParkedWindow: time.Hour, TerminalClass: "unavailable"}
}

// fallbackFixtureDriver is a second registered runtime for chain-advance
// tests: same dynamic fixture, its own runtime identity.
type fallbackFixtureDriver struct{ managedDynamicFixtureDriver }

func (fallbackFixtureDriver) ChatCapability() ChatCapability {
	capability := managedFixtureDriver{}.ChatCapability()
	capability.Runtime = "fallback-fixture"
	return capability
}

const classifiedFailureCommand = `printf 'AI_APICallError: Bad Gateway\n' >&2; exit 1`
const quotaFailureCommand = `printf 'AI_APICallError: you have reached your session usage limit, upgrade for higher limits\n' >&2; exit 1`

// TestProviderFailureParksThenRelaunchesSameRoute pins the ladder's first
// rungs (acceptance 1–2): a provider-classified child failure parks the run
// (never terminal-fails it), the cadence pass relaunches on a FRESH
// attempt-scoped task (R2), and the second attempt completes the run with the
// whole attempt history in the ledger. One run row throughout — retries never
// re-admit, so no budget is re-consumed.
func TestProviderFailureParksThenRelaunchesSameRoute(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	var agentAttempts int32
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		if atomic.AddInt32(&agentAttempts, 1) == 1 {
			return classifiedFailureCommand
		}
		return jsonTextCommand(helperClaimV2)
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.bindHelper(t, "agent-park", 10, false, nil, store.ManagedLimits{})
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-park", Cwd: fixture.root}, "park-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)

	parked := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "parked"
	})[0]
	if parked.ErrorClass != providerErrorUnavailable || !strings.Contains(parked.Recovery, "Bad Gateway") {
		t.Fatalf("park lost the provider truth: class=%q recovery=%q", parked.ErrorClass, parked.Recovery)
	}
	ledger := providerOutageLedgerFrom(parked.Detail)
	if len(ledger.Attempts) != 1 || ledger.Attempts[0].Class != providerErrorUnavailable || ledger.RouteIndex != 0 {
		t.Fatalf("attempt ledger not seeded: %+v", ledger)
	}

	fixture.host.relaunchParkedRunsOnce()
	completed := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "completed"
	})[0]
	final := providerOutageLedgerFrom(completed.Detail)
	if len(final.Attempts) != 2 || final.Attempts[1].TaskID == final.Attempts[0].TaskID || final.Attempts[1].TaskID == "" {
		t.Fatalf("relaunch did not get a fresh attempt-scoped task: %+v", final.Attempts)
	}
	if completed.ChildTaskID != final.Attempts[1].TaskID {
		t.Fatalf("run does not point at the live attempt: child=%s attempts=%+v", completed.ChildTaskID, final.Attempts)
	}
	if _, ok := completed.Detail["rerouted_from"]; ok {
		t.Fatal("a same-route retry must not claim a reroute")
	}
}

// TestProviderQuotaAdvancesUserChain pins the reroute centerpiece
// (acceptance 5): quota skips same-route retries (R11), relaunches on the
// user-authored fallback route, and the completed run carries reroute
// attribution — which model actually did the work is never hidden.
func TestProviderQuotaAdvancesUserChain(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	primary := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		return quotaFailureCommand
	}}
	fixture := newAgentHostFixture(t, primary)
	chatDrivers["fallback-fixture"] = fallbackFixtureDriver{managedDynamicFixtureDriver{
		commandFor: func(ChatRequest) string { return jsonTextCommand(helperClaimV2) }}}

	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-chain",
		ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest,
		ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		Runtime: "managed-fixture", Priority: 10,
		Routes:             []store.ManagedRoute{{Runtime: "fallback-fixture"}},
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-chain")}); err != nil {
		t.Fatal(err)
	}
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-chain", Cwd: fixture.root}, "chain-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)

	parked := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "parked"
	})[0]
	if parked.ErrorClass != providerErrorQuota {
		t.Fatalf("quota not classified: %q", parked.ErrorClass)
	}
	if ledger := providerOutageLedgerFrom(parked.Detail); ledger.RouteIndex != 1 {
		t.Fatalf("quota did not advance the chain immediately (R11): %+v", ledger)
	}

	fixture.host.relaunchParkedRunsOnce()
	completed := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "completed"
	})[0]
	ledger := providerOutageLedgerFrom(completed.Detail)
	last := ledger.Attempts[len(ledger.Attempts)-1]
	if last.Runtime != "fallback-fixture" {
		t.Fatalf("relaunch stayed on the dead route: %+v", ledger.Attempts)
	}
	rerouted, _ := completed.Detail["rerouted_from"].(map[string]any)
	if rerouted == nil || rerouted["runtime"] != "managed-fixture" || rerouted["class"] != providerErrorQuota {
		t.Fatalf("reroute attribution missing (plan invariant 2): %v", completed.Detail["rerouted_from"])
	}
}

// TestHelperWindowLapseLandsStaleUnsent pins the helper's terminal honesty
// (acceptance 3): past the freshness window a provider-parked helper settles
// as stale_unsent — a loud non-send, never a late auto-reply.
func TestHelperWindowLapseLandsStaleUnsent(t *testing.T) {
	pinProviderRetryPolicy(t, func(role string) providerRetryPolicy {
		return providerRetryPolicy{MaxSameRoute: 3, Backoff: 0,
			ParkedWindow: time.Second, TerminalClass: "stale_unsent"}
	})
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		return "sleep 2; " + classifiedFailureCommand
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.bindHelper(t, "agent-stale", 10, false, nil, store.ManagedLimits{})
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-stale", Cwd: fixture.root}, "stale-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	settled := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "failed"
	})[0]
	if settled.ErrorClass != "stale_unsent" || !strings.Contains(settled.Message, "NOT sent") {
		t.Fatalf("window lapse not honest: class=%q message=%q", settled.ErrorClass, settled.Message)
	}
}

// TestUnclassifiedFailureKeepsTodaysMapping pins the fail-safe direction
// (R3, acceptance 1): a child that dies without a transport classification
// terminal-fails exactly as before — parking is opt-in per class.
func TestUnclassifiedFailureKeepsTodaysMapping(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		return "exit 1"
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.bindHelper(t, "agent-plain", 10, false, nil, store.ManagedLimits{})
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-plain", Cwd: fixture.root}, "plain-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	failed := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "failed"
	})[0]
	if failed.ErrorClass != "failed" || !strings.Contains(failed.Recovery, "did not complete successfully") {
		t.Fatalf("unclassified failure changed behavior: class=%q recovery=%q", failed.ErrorClass, failed.Recovery)
	}
}

// TestBindingChainValidation pins R1/R8 at the save boundary: an entry on an
// unregistered runtime is refused, and every accepted entry got the same
// runtime-proven read-only mode validation as the primary.
func TestBindingChainValidation(t *testing.T) {
	fixture := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand("noop")
	}})
	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	command := managedBindingCommand{BindingID: "agent-bad-chain",
		ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest,
		ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		Runtime:            "managed-fixture",
		Routes:             []store.ManagedRoute{{Runtime: "no-such-runtime"}},
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-bad-chain")}
	if _, err := fixture.host.putBinding(command); err == nil {
		t.Fatal("chain entry on an unregistered runtime was accepted")
	}
	command.Routes = []store.ManagedRoute{{Runtime: "managed-fixture", Mode: "elevated-nonsense"}}
	if _, err := fixture.host.putBinding(command); err == nil {
		t.Fatal("chain entry with an unproven mode was accepted (R1)")
	}
}

// TestOperatorRerouteJumpsTheQueue pins acceptance 6: the preview names each
// parked run's outcome and target, apply requires the token + confirmation,
// and a confirmed reroute moves the run to the operator-chosen route ahead of
// the retry schedule.
func TestOperatorRerouteJumpsTheQueue(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	primary := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		return classifiedFailureCommand
	}}
	fixture := newAgentHostFixture(t, primary)
	chatDrivers["fallback-fixture"] = fallbackFixtureDriver{managedDynamicFixtureDriver{
		commandFor: func(ChatRequest) string { return jsonTextCommand(helperClaimV2) }}}
	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-jump",
		ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest,
		ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		Runtime: "managed-fixture", Priority: 10,
		Routes:             []store.ManagedRoute{{Runtime: "fallback-fixture"}},
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-jump")}); err != nil {
		t.Fatal(err)
	}
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-jump", Cwd: fixture.root}, "jump-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "parked"
	})

	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, fixture.host, fixture.owner)
	post := func(path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return recorder
	}
	// Confirmation is not optional (acceptance 6).
	if response := post("/api/orchestration/managed/provider-reroute", `{"runtime":"managed-fixture","model":"","confirmed":true}`); response.Code != http.StatusBadRequest {
		t.Fatalf("tokenless apply accepted: %d %s", response.Code, response.Body.String())
	}
	previewResponse := post("/api/orchestration/managed/provider-reroute/preview", `{"runtime":"managed-fixture","model":""}`)
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", previewResponse.Code, previewResponse.Body.String())
	}
	var previewBody struct {
		Decisions []struct {
			Action string             `json:"action"`
			Target store.ManagedRoute `json:"target"`
		} `json:"decisions"`
		PreviewToken string `json:"preview_token"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &previewBody); err != nil {
		t.Fatal(err)
	}
	if len(previewBody.Decisions) != 1 || previewBody.Decisions[0].Action != "reroute" ||
		previewBody.Decisions[0].Target.Runtime != "fallback-fixture" {
		t.Fatalf("preview does not name the plan: %s", previewResponse.Body.String())
	}
	applyResponse := post("/api/orchestration/managed/provider-reroute",
		`{"runtime":"managed-fixture","model":"","preview_token":"`+previewBody.PreviewToken+`","confirmed":true}`)
	if applyResponse.Code != http.StatusOK || !strings.Contains(applyResponse.Body.String(), `"rerouted":1`) {
		t.Fatalf("apply: %d %s", applyResponse.Code, applyResponse.Body.String())
	}
	fixture.host.relaunchParkedRunsOnce()
	completed := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "completed"
	})[0]
	if rerouted, _ := completed.Detail["rerouted_from"].(map[string]any); rerouted == nil {
		t.Fatalf("operator reroute lost attribution: %v", completed.Detail)
	}
}

// TestBreakerParksAdmissionsWithoutLaunching pins acceptance 7: after the
// trip threshold, a new admission on the dead route parks with zero vendor
// spawns, and the projection carries one banner fact for the route.
func TestBreakerParksAdmissionsWithoutLaunching(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	var agentLaunches int32
	driver := managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		atomic.AddInt32(&agentLaunches, 1)
		return classifiedFailureCommand
	}}
	fixture := newAgentHostFixture(t, driver)
	fixture.bindHelper(t, "agent-breaker", 10, false, nil, store.ManagedLimits{})
	for index, key := range []string{"breaker-1", "breaker-2", "breaker-3"} {
		rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question",
			SessionID: "native-" + key, Cwd: fixture.root}, key)
		if err != nil {
			t.Fatal(err)
		}
		wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
		want := index + 1
		waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
			parked := 0
			for _, run := range runs {
				if run.State == "parked" {
					parked++
				}
			}
			return parked == want
		})
	}
	if launches := atomic.LoadInt32(&agentLaunches); launches != 2 {
		t.Fatalf("breaker did not stop the third launch: %d agent spawns", launches)
	}
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 3 })
	circuitParked := false
	for _, run := range runs {
		if strings.Contains(run.Recovery, "circuit open") {
			circuitParked = true
			if ledger := providerOutageLedgerFrom(run.Detail); len(ledger.Attempts) != 0 {
				t.Fatalf("circuit-parked run claims launch attempts: %+v", ledger)
			}
		}
	}
	if !circuitParked {
		t.Fatalf("no run records the circuit-open park: %+v", runs)
	}
	facts := fixture.host.providerOutageFacts()
	if len(facts) != 1 || facts[0].Runtime != "managed-fixture" || facts[0].Parked < 1 {
		t.Fatalf("banner fact missing or wrong: %+v", facts)
	}
}

// TestAgentsSurfaceCarriesRoutesAndOutageFacts pins the wire contract
// (acceptance 8): the agents PUT accepts the ordered routes chain, the
// settings projection publishes it back beside the provider_outages facts,
// and both fields exist even when empty — the frontend never guesses.
func TestAgentsSurfaceCarriesRoutesAndOutageFacts(t *testing.T) {
	fixture := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand("noop")
	}})
	chatDrivers["fallback-fixture"] = fallbackFixtureDriver{managedDynamicFixtureDriver{
		commandFor: func(ChatRequest) string { return jsonTextCommand("noop") }}}
	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, fixture.host, fixture.owner)

	body := `{"profile_id":"` + preview.ProfileID + `","profile_source_digest":"` + preview.SourceDigest +
		`","profile_bundle_digest":"` + preview.BundleDigest + `","project_root":"` + fixture.root +
		`","runtime":"managed-fixture","mode":"","model":"","granted_authority":[],` +
		`"routes":[{"runtime":"fallback-fixture","model":"local/tiny"}],` +
		`"expected_state_token":"` + store.ManagedBindingAbsentToken("agent-wire") + `","confirmed":true}`
	putResponse := httptest.NewRecorder()
	mux.ServeHTTP(putResponse, httptest.NewRequest(http.MethodPut, "/api/orchestration/agents/agent-wire", strings.NewReader(body)))
	if putResponse.Code != http.StatusOK {
		t.Fatalf("binding PUT with routes: %d %s", putResponse.Code, putResponse.Body.String())
	}
	settingsResponse := httptest.NewRecorder()
	mux.ServeHTTP(settingsResponse, httptest.NewRequest(http.MethodGet, "/api/orchestration/managed/settings", nil))
	if settingsResponse.Code != http.StatusOK {
		t.Fatalf("settings projection: %d", settingsResponse.Code)
	}
	var projection struct {
		Bindings []struct {
			BindingID string               `json:"binding_id"`
			Routes    []store.ManagedRoute `json:"routes"`
		} `json:"bindings"`
		ProviderOutages []providerOutageFact `json:"provider_outages"`
	}
	if err := json.Unmarshal(settingsResponse.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, binding := range projection.Bindings {
		if binding.BindingID == "agent-wire" {
			found = true
			if len(binding.Routes) != 1 || binding.Routes[0].Runtime != "fallback-fixture" || binding.Routes[0].Model != "local/tiny" {
				t.Fatalf("routes did not round-trip: %+v", binding.Routes)
			}
		}
	}
	if !found {
		t.Fatalf("binding missing from projection: %s", settingsResponse.Body.String())
	}
	if !strings.Contains(settingsResponse.Body.String(), `"provider_outages"`) {
		t.Fatal("projection lost the provider_outages fact")
	}
}
