package daemon

// Provider-outage degradation (provider-outage plan, Slices B + C backend).
// The recovery ladder for a managed run whose PROVIDER died before the agent
// could work: retry (same route) → reroute (the user-authored chain) → park
// (awaiting the operator) → terminal-honest. Provider failure is not agent
// failure: a parked run keeps its idempotency key and signal anchor, consumes
// no loop/hop budget (nothing re-admits), and every relaunch attempt gets a
// FRESH attempt-scoped task key (red-team R2) so an idempotent task Create
// can never hand back the dead child.
//
// Stale-attempt safety rides the existing keying: finishManagedChild looks
// runs up by child_task_id, and a relaunch moves that pointer to the new
// child — a dead attempt's late events find no run.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// providerRetryPolicy is CONFIGURATION shipped with the product, not frozen
// code facts (the shippedAgentDefaults discipline): per-role failure
// economics from the plan's role table.
type providerRetryPolicy struct {
	MaxSameRoute  int           // launch attempts on one route before the chain advances
	Backoff       time.Duration // base wait before a same-route retry; doubles per attempt
	Freshness     time.Duration // helper reply-value window; 0 = replies do not decay
	ParkedWindow  time.Duration // total parked age before terminal honesty
	TerminalClass string        // the honest terminal error class when the window lapses
}

// providerRetryPolicyFor is a var so tests can compress the windows.
var providerRetryPolicyFor = defaultProviderRetryPolicy

func defaultProviderRetryPolicy(role string) providerRetryPolicy {
	switch role {
	case "helper":
		// A reply loses value fast; a stale one must reach the operator as a
		// draft, never a send (plan invariant 5).
		return providerRetryPolicy{MaxSameRoute: 3, Backoff: time.Minute,
			Freshness: 30 * time.Minute, ParkedWindow: 30 * time.Minute, TerminalClass: "stale_unsent"}
	case "reviewer":
		// Reviews do not decay; absence of a verdict must stay loud, so the
		// window is the longest and the terminal class says "unreviewed".
		return providerRetryPolicy{MaxSameRoute: 3, Backoff: time.Minute,
			ParkedWindow: 48 * time.Hour, TerminalClass: "unreviewed"}
	default: // follower
		return providerRetryPolicy{MaxSameRoute: 3, Backoff: time.Minute,
			ParkedWindow: 24 * time.Hour, TerminalClass: "unavailable"}
	}
}

// providerOutageLedger is the attempt history a parked run carries in its
// Detail under "provider_outage". JSON round-trips through map[string]any.
type providerOutageLedger struct {
	Class          string               `json:"class"`
	ProviderDetail string               `json:"provider_detail"`
	RouteIndex     int                  `json:"route_index"` // 0 = the binding's primary route
	NextEligibleAt int64                `json:"next_eligible_at"`
	Attempts       []providerOutageTurn `json:"attempts"`
	// SkippedRoutes names the fallback chain entries the reroute passed over because
	// their model route can no longer be read (team rest-of-release plan §5.3): a
	// missing entry is skipped and named, never replaced by a default model.
	SkippedRoutes []providerSkippedRoute `json:"skipped_routes,omitempty"`
}

// providerSkippedRoute is one chain entry the reroute skipped, and why.
type providerSkippedRoute struct {
	RouteIndex int    `json:"route_index"`
	RouteID    string `json:"route_id"`
	Reason     string `json:"reason"`
}

// noteSkipped records a skipped chain entry once.
func (ledger *providerOutageLedger) noteSkipped(index int, routeID, reason string) {
	for _, skipped := range ledger.SkippedRoutes {
		if skipped.RouteIndex == index && skipped.RouteID == routeID {
			return
		}
	}
	ledger.SkippedRoutes = append(ledger.SkippedRoutes, providerSkippedRoute{RouteIndex: index, RouteID: routeID, Reason: reason})
}

type providerOutageTurn struct {
	ThinkingEffort *store.ThinkingEffort `json:"thinking_effort,omitempty"`
	Attempt        int                   `json:"attempt"`
	RouteIndex     int                   `json:"route_index"`
	Runtime        string                `json:"runtime"`
	Model          string                `json:"model,omitempty"`
	TaskID         string                `json:"task_id"`
	Class          string                `json:"class,omitempty"`
}

func providerOutageLedgerFrom(detail map[string]any) providerOutageLedger {
	ledger := providerOutageLedger{}
	raw, ok := detail["provider_outage"]
	if !ok {
		return ledger
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ledger
	}
	_ = json.Unmarshal(encoded, &ledger)
	return ledger
}

func (ledger providerOutageLedger) into(detail map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range detail {
		out[key] = value
	}
	out["provider_outage"] = ledger
	return out
}

// bindingRoutes is the full ordered route list: the binding's primary route
// followed by the user-authored fallback chain. Index 0 always exists.
func bindingRoutes(binding store.ManagedBinding) []store.ManagedRoute {
	routes := []store.ManagedRoute{{RouteID: binding.RouteID, Runtime: binding.Runtime, Model: binding.Model, ThinkingEffort: binding.ThinkingEffort, Mode: binding.Mode}}
	return append(routes, binding.Routes...)
}

// providerFailureForTask scans the dead child's recent events for the newest
// provider class the vendor edge stamped (Slice A). The class rides ONLY
// transport-channel events (R3); this reads what the drivers wrote, it never
// re-detects. Returns ("", "") when the failure was not provider-shaped.
func (host *orchestrationManagedHost) providerFailureForTask(taskID string) (string, string) {
	task, found, err := host.tasks.Task(taskID)
	if err != nil || !found {
		return "", ""
	}
	after := task.LastSequence - 512
	if after < 0 {
		after = 0
	}
	events, err := host.tasks.Events(taskID, after, 512)
	if err != nil {
		return "", ""
	}
	class, detail := "", ""
	for _, event := range events {
		if candidate := anyString(event.Payload["error_class"]); candidate != "" {
			class = candidate
			detail = anyString(event.Payload["text"])
		}
	}
	return class, detail
}

// handleProviderFailure is finishManagedChild's provider lane: park, advance
// the chain, or settle with role-honest terminal classes. handled=false means
// the failure was not provider-shaped and the caller keeps today's mapping.
func (host *orchestrationManagedHost) handleProviderFailure(run store.ManagedRun, event TaskEvent) (bool, error) {
	class, providerDetail := host.providerFailureForTask(event.TaskID)
	if class == "" {
		return false, nil
	}
	now := time.Now().Unix()
	policy := providerRetryPolicyFor(run.Role)
	binding, found, err := host.ix.ManagedBinding(run.BindingID)
	if err != nil || !found {
		return true, host.completeProviderTerminal(run, event, class, providerDetail,
			"binding is no longer available; the parked signal cannot relaunch", now)
	}
	recovery := providerRecoveryText(class, providerDetail)
	ledger := providerOutageLedgerFrom(run.Detail)
	if len(ledger.Attempts) == 0 {
		// Seed the original launch as attempt 1 so the ledger is the one
		// complete history of every task this run ever spawned.
		original := providerOutageTurn{Attempt: 1, RouteIndex: 0,
			Runtime: binding.Runtime, Model: binding.Model, TaskID: run.ChildTaskID}
		if task, found, err := host.tasks.Task(run.ChildTaskID); err == nil && found {
			original.Runtime = task.Runtime
			if task.RequestedSettings != nil {
				original.Model = task.RequestedSettings.Model
				original.ThinkingEffort = &task.RequestedSettings.Effort
			}
		}
		ledger.Attempts = []providerOutageTurn{original}
	}
	ledger.Attempts[len(ledger.Attempts)-1].Class = class
	ledger.Class, ledger.ProviderDetail = class, truncate(providerDetail, 500)

	// Terminal honesty: past the role's window (helpers: freshness) the run
	// settles loudly instead of parking forever (R7's companion bound).
	age := time.Duration(now-run.AdmittedAt) * time.Second
	if policy.ParkedWindow > 0 && age > policy.ParkedWindow {
		return true, host.settleProviderWindowLapse(run, event, policy, class, recovery, now)
	}

	routes := bindingRoutes(binding)
	failingIndex := ledger.RouteIndex
	if failingIndex >= len(routes) {
		failingIndex = len(routes) - 1
	}
	// The breaker learns about the route that actually failed (Slice D),
	// keyed runtime+model (R4) so only that route trips.
	host.recordProviderFailure(routes[failingIndex].Runtime, routes[failingIndex].Model, class, providerDetail)
	attemptsOnRoute := 0
	for _, turn := range ledger.Attempts {
		if turn.RouteIndex == ledger.RouteIndex {
			attemptsOnRoute++
		}
	}
	// provider_quota never self-heals inside a retry window (R11): advance
	// the chain immediately instead of burning same-route attempts.
	advance := class == providerErrorQuota || attemptsOnRoute >= policy.MaxSameRoute
	if advance && ledger.RouteIndex+1 < len(routes) {
		ledger.RouteIndex++
		ledger.NextEligibleAt = now // next cadence tick relaunches on the new route
	} else if advance {
		// Chain exhausted: stay parked for the operator's reroute action,
		// re-probing the last route slowly until the window lapses.
		ledger.NextEligibleAt = now + int64((8 * policy.Backoff).Seconds())
	} else {
		backoff := policy.Backoff * (1 << (attemptsOnRoute - 1))
		ledger.NextEligibleAt = now + int64(backoff.Seconds())
	}
	return true, host.ix.ParkManagedRun(run.RunID, ledger.into(run.Detail), class, recovery, now)
}

// settleProviderWindowLapse is the ladder's honest bottom rung. A helper
// preserves whatever partial draft the dead child produced — the operator
// gets a draft, never an auto-sent stale reply.
func (host *orchestrationManagedHost) settleProviderWindowLapse(run store.ManagedRun, event TaskEvent,
	policy providerRetryPolicy, class, recovery string, now int64) error {
	detail := providerOutageLedgerFrom(run.Detail).into(run.Detail)
	if run.Role == "helper" {
		if partial, err := host.finalTaskMessage(event.TaskID); err == nil && partial != "" {
			detail["partial_draft"] = truncate(partial, 65536)
		}
	}
	message := providerTerminalMessage(run.Role, class)
	return host.ix.CompleteManagedRun(run.RunID, "failed", "", message, nil, detail,
		policy.TerminalClass, recovery, now)
}

func (host *orchestrationManagedHost) completeProviderTerminal(run store.ManagedRun, event TaskEvent,
	class, providerDetail, reason string, now int64) error {
	detail := providerOutageLedgerFrom(run.Detail).into(run.Detail)
	return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil, detail,
		class, reason, now)
}

func providerRecoveryText(class, providerDetail string) string {
	prefix := map[string]string{
		providerErrorQuota:       "Provider quota reached",
		providerErrorUnavailable: "Provider unavailable",
		providerErrorAuth:        "Provider authentication required",
	}[class]
	if prefix == "" {
		prefix = "Provider failure"
	}
	if providerDetail == "" {
		return prefix + "."
	}
	return truncate(prefix+": "+providerDetail, 2000)
}

func providerTerminalMessage(role, class string) string {
	switch role {
	case "helper":
		return "The reply window lapsed while the provider was down; any partial draft is preserved for the operator and was NOT sent."
	case "reviewer":
		return "This review DID NOT HAPPEN — the provider stayed down past the review window. Absence of findings here is absence of a review."
	default:
		return "The provider stayed down past the retry window; this signal was never observed by the agent."
	}
}

// relaunchParkedRunsOnce is the cadence-driven ladder engine: one bounded
// pass per coordinator sweep, relaunching every eligible parked run on its
// current route with a fresh attempt-scoped task key. No new loop, no new
// scanner — exactly the emitNaturalSignalsOnce discipline. Retries never
// re-admit, so loop/hop/group budgets are untouched (plan invariant 3).
func (host *orchestrationManagedHost) relaunchParkedRunsOnce() {
	// Helper-session liveness and pending-signal drain ride the same sweep,
	// AFTER the parked ladder so a slot behind a lapsed park drains on this
	// pass, not the next (helper-persistent-session plan D4).
	defer host.reconcileHelperSessionsOnce()
	parked, err := host.ix.ParkedManagedRuns(25)
	if err != nil {
		host.setProblem("Parked-run relaunch pass is failing: " + err.Error())
		return
	}
	now := time.Now().Unix()
	for _, run := range parked {
		ledger := providerOutageLedgerFrom(run.Detail)
		if ledger.NextEligibleAt > now {
			continue
		}
		if err := host.relaunchParkedRun(run, ledger); err != nil {
			log.Printf("parked run %s relaunch failed: %v", run.RunID, err)
		}
	}
}

// relaunchParkedRun launches one parked run's next attempt. Route selection,
// prompt recomposition from the PINNED profile revision, and the reroute
// attribution all live here.
func (host *orchestrationManagedHost) relaunchParkedRun(run store.ManagedRun, ledger providerOutageLedger) error {
	now := time.Now().Unix()
	binding, found, err := host.ix.ManagedBinding(run.BindingID)
	if err != nil {
		return err
	}
	if !found || binding.State != "enabled" {
		return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil,
			ledger.into(run.Detail), ledger.Class,
			"binding was removed or disabled while this run was parked", now)
	}
	routes := bindingRoutes(binding)
	if ledger.RouteIndex >= len(routes) {
		ledger.RouteIndex = len(routes) - 1 // chain shrank while parked: clamp, honestly re-probing the last route
	}
	// Destination (managed-turn-profile-limits plan §4.1 point 3), judged by the
	// run's PINNED revision: a forbidden route is passed over for the next
	// permitted one, and with none left the run ends — its own branch, never
	// the R8 skip, which can leave a run parked forever (R2-12). A revision
	// that cannot be read fails in composeRelaunchPrompt below, as before.
	compiled := profilefs.CompiledProfile{}
	revision, revisionErr := host.profiles.GetRevision(run.ProfileID, run.ProfileSourceDigest, run.ProfileBundleDigest)
	revisionRead := revisionErr == nil && revision.Normalized != nil
	if revisionRead {
		compiled = *revision.Normalized
	}
	// Route (team rest-of-release plan §5.3): a primary route that cannot be read
	// refuses the run; a fallback entry that cannot be read is skipped and named in
	// the outage record. Nothing falls back to a runtime's default model.
	permitted, destinationProblem := -1, ""
	for index := ledger.RouteIndex; index < len(routes); index++ {
		if host.chainRouteMissing(routes[index]) {
			if index == 0 {
				return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil, ledger.into(run.Detail),
					routeRefusalMissing, "route_missing: the model route of this place can no longer be read; choose another model route for it.", now)
			}
			ledger.noteSkipped(index, routes[index].RouteID, routeRefusalMissing)
			continue
		}
		if revisionRead {
			if problem := managedRouteDestinationProblem(compiled, routes[index]); problem != "" {
				if destinationProblem == "" {
					destinationProblem = problem
				}
				continue
			}
		}
		permitted = index
		break
	}
	if permitted < 0 {
		if destinationProblem != "" {
			return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil, ledger.into(run.Detail),
				"destination_locality", destinationProblem, now)
		}
		return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil, ledger.into(run.Detail),
			routeRefusalMissing, "route_missing: no remaining fallback of this place has a model route that can be read.", now)
	}
	ledger.RouteIndex = permitted
	route := routes[ledger.RouteIndex]
	// The same refusals as the first attempt (runStartRefusal): a place held by its
	// adoption since the run parked, or a route the rulebook no longer admits, starts
	// no further attempt. The place stays on; the run ends typed.
	refusal, observed := host.attemptStartRefusal(binding, compiled, route.RouteID, route.Mode)
	if refusal != nil {
		return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil, ledger.into(run.Detail),
			refusal.Code, refusal.Message, now)
	}
	if len(observed) > 0 {
		run.Detail["route_rules_observed"] = anySlice(observed)
	}
	prompt, labels, signal, coverage, err := host.composeRelaunchPrompt(run, binding, route.Runtime)
	if coverage != nil {
		run.Detail["context_coverage"] = coverage
	}
	if err != nil {
		return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil,
			ledger.into(run.Detail), "context", err.Error(), now)
	}
	attempt := len(ledger.Attempts) + 1
	// R2: the RUN key never changes; each attempt derives its own task key so
	// the idempotent Create can never return a dead child.
	taskIdempotency := managedID("oridem_", run.IdempotencyKey, fmt.Sprint(attempt), route.Runtime, route.Model)
	host.launchMu.Lock()
	defer host.launchMu.Unlock()
	request := host.helperTurnRequest(run.GroupID, binding.Runtime, route.Runtime, route.Model, route.Mode, binding.ProjectRoot, prompt, route.ThinkingEffort)
	if ledger.RouteIndex > 0 {
		request.effortSource = "fallback"
	}
	child, err := host.createAgentTask(compiled, request, taskIdempotency)
	if err != nil {
		var effortFailure *EffortError
		if errors.As(err, &effortFailure) {
			ledger.Attempts = append(ledger.Attempts, providerOutageTurn{Attempt: attempt,
				RouteIndex: ledger.RouteIndex, Runtime: route.Runtime, Model: route.Model,
				ThinkingEffort: route.ThinkingEffort, Class: "configuration"})
			return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil, ledger.into(run.Detail), "configuration", "Review this route's thinking effort: "+effortFailure.Message, now)
		}
		// A non-provider launch failure on a chain entry skips it (R8) —
		// recorded, then the next cadence pass tries the next route or the
		// exhausted-chain slow probe.
		ledger.Attempts = append(ledger.Attempts, providerOutageTurn{Attempt: attempt,
			RouteIndex: ledger.RouteIndex, Runtime: route.Runtime, Model: route.Model, ThinkingEffort: route.ThinkingEffort,
			TaskID: "", Class: "launch_failed"})
		if ledger.RouteIndex+1 < len(routes) {
			ledger.RouteIndex++
		}
		ledger.NextEligibleAt = now + 60
		return host.ix.ParkManagedRun(run.RunID, ledger.into(run.Detail), ledger.Class,
			providerRecoveryText(ledger.Class, ledger.ProviderDetail), now)
	}
	ledger.Attempts = append(ledger.Attempts, providerOutageTurn{Attempt: attempt,
		RouteIndex: ledger.RouteIndex, Runtime: route.Runtime, Model: route.Model, ThinkingEffort: route.ThinkingEffort, TaskID: child.ID})
	detail := ledger.into(run.Detail)
	// A timeout marker names the previous attempt's child; the new attempt
	// starts without one (plan §4.2, RT-1).
	delete(detail, "timeout")
	detail["signal"], detail["labels"] = signal, anySlice(labels)
	detail["context_coverage"] = coverage
	// Attribution (plan invariant 2): a run executing off its primary route
	// says so, with the provider class that caused the move.
	if ledger.RouteIndex > 0 {
		detail["rerouted_from"] = map[string]any{"runtime": binding.Runtime,
			"model": binding.Model, "class": ledger.Class}
	} else {
		delete(detail, "rerouted_from")
	}
	return host.ix.RelaunchManagedRun(run.RunID, child.ID,
		managedID("orel_", run.RunID, child.ID), detail, now)
}

// composeRelaunchPrompt rebuilds the agent prompt exactly the way the
// original launch did: pinned profile revision, the run's pinned signal, the
// group's identity for source/session context. The label manifest is
// re-pinned with the recomposed prompt so decode-time containment matches
// what was actually supplied (ART-09).
func (host *orchestrationManagedHost) composeRelaunchPrompt(run store.ManagedRun,
	binding store.ManagedBinding, routeRuntime string) (string, []string, string, []orchestration.ContextCoverage, error) {
	detailSignal := anyString(run.Detail["signal"])
	if detailSignal == "" {
		return "", nil, "", nil, errors.New("parked run carries no pinned signal")
	}
	revision, err := host.profiles.GetRevision(run.ProfileID, run.ProfileSourceDigest, run.ProfileBundleDigest)
	if err != nil || revision.Normalized == nil {
		return "", nil, "", nil, errors.New("pinned managed profile is unavailable for relaunch")
	}
	compiled := *revision.Normalized
	if validateManagedProfile(compiled) != nil {
		return "", nil, "", nil, errors.New("pinned profile is not a managed orchestration agent")
	}
	stagePrompt, selected := profileSignalSelectors(compiled)[detailSignal]
	if !selected {
		return "", nil, "", nil, errors.New("pinned profile no longer selects the parked signal")
	}
	group, foundGroup, err := host.ix.ManagedGroup(run.GroupID)
	if err != nil || !foundGroup {
		return "", nil, "", nil, errors.New("managed group is unavailable for relaunch")
	}
	source := orchestration.ManagedSource{TaskID: run.SourceTaskID, Runtime: group.RootRuntime,
		CatalogSessionID: group.RootCatalogSessionID, NativeSessionID: group.RootNativeSessionID,
		ProjectRoot: group.ProjectRoot, EventID: run.SourceEventID, Sequence: sourceSequence(run.Detail),
		TranscriptSeq: sourceTranscriptSeq(run.Detail)}
	if persistentAgent(compiled) && helperTurnResumes(group, binding.Runtime, routeRuntime) {
		source.TranscriptSince = group.HelperTranscriptSeq
	}
	extras, contextErr := host.agentPromptContext(compiled, &group, source)
	if contextErr != nil {
		return "", nil, "", extras.Coverage, contextErr
	}
	prompt, labels, promptErr := orchestration.BuildAgentPrompt(orchestration.AgentPromptProfile{
		ID: compiled.ID, Type: compiled.AgentType(), Instructions: stagePrompt,
		MaxInputBytes: compiled.Limits.MaxInputBytes, MaxOutputBytes: compiled.Limits.MaxOutputBytes,
		AllowedProfiles: compiled.AllowedProfiles, DeclaredTags: binding.DeclaredTags, GrantedAuthority: binding.Authority}, extras.Source, detailSignal, extras.Items)
	return prompt, labels, detailSignal, extras.Coverage, promptErr
}

// ── Circuit breaker and operator reroute (Slices C/D) ────────────────────────

// providerBreakerTripThreshold is CONFIGURATION: provider-class failures on
// one route inside a single process before new admissions park without
// launching (no doomed vendor spawns, one banner instead of N failed runs).
const providerBreakerTripThreshold = 2

type providerBreakerState struct {
	Failures  int
	TrippedAt int64
	Class     string
	Detail    string
}

func providerRouteKey(runtime, model string) string { return runtime + "\x00" + model }

func (host *orchestrationManagedHost) recordProviderFailure(runtime, model, class, detail string) {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.breaker == nil {
		host.breaker = map[string]*providerBreakerState{}
	}
	key := providerRouteKey(runtime, model)
	state := host.breaker[key]
	if state == nil {
		state = &providerBreakerState{}
		host.breaker[key] = state
	}
	state.Failures++
	state.Class, state.Detail = class, truncate(detail, 500)
	if state.Failures >= providerBreakerTripThreshold && state.TrippedAt == 0 {
		state.TrippedAt = time.Now().Unix()
	}
}

// recordProviderSuccess is the half-open reset: any completed attempt on a
// route proves the provider answers again.
func (host *orchestrationManagedHost) recordProviderSuccess(runtime, model string) {
	host.mu.Lock()
	defer host.mu.Unlock()
	delete(host.breaker, providerRouteKey(runtime, model))
}

func (host *orchestrationManagedHost) providerBreakerTripped(runtime, model string) (string, string, bool) {
	host.mu.RLock()
	defer host.mu.RUnlock()
	state := host.breaker[providerRouteKey(runtime, model)]
	if state == nil || state.TrippedAt == 0 {
		return "", "", false
	}
	return state.Class, state.Detail, true
}

// providerOutageFact is one tripped route's banner row on the agents
// projection: which route, since when, the vendor's own recovery words, and
// how many runs sit parked behind it.
type providerOutageFact struct {
	Runtime   string `json:"runtime"`
	Model     string `json:"model,omitempty"`
	Class     string `json:"class"`
	Detail    string `json:"detail,omitempty"`
	TrippedAt int64  `json:"tripped_at"`
	Parked    int    `json:"parked"`
}

func (host *orchestrationManagedHost) providerOutageFacts() []providerOutageFact {
	host.mu.RLock()
	facts := []providerOutageFact{}
	for key, state := range host.breaker {
		if state.TrippedAt == 0 {
			continue
		}
		runtimeName, model, _ := strings.Cut(key, "\x00")
		facts = append(facts, providerOutageFact{Runtime: runtimeName, Model: model,
			Class: state.Class, Detail: state.Detail, TrippedAt: state.TrippedAt})
	}
	host.mu.RUnlock()
	if len(facts) == 0 {
		return facts
	}
	parked, err := host.ix.ParkedManagedRuns(200)
	if err == nil {
		for _, run := range parked {
			ledger := providerOutageLedgerFrom(run.Detail)
			for index := range facts {
				if turnRoute := ledger.currentRoute(host, run); turnRoute.Runtime == facts[index].Runtime &&
					turnRoute.Model == facts[index].Model {
					facts[index].Parked++
				}
			}
		}
	}
	sort.Slice(facts, func(i, j int) bool {
		return facts[i].Runtime+facts[i].Model < facts[j].Runtime+facts[j].Model
	})
	return facts
}

// currentRoute resolves the route a parked run is currently pointed at.
func (ledger providerOutageLedger) currentRoute(host *orchestrationManagedHost, run store.ManagedRun) store.ManagedRoute {
	binding, found, err := host.ix.ManagedBinding(run.BindingID)
	if err != nil || !found {
		return store.ManagedRoute{}
	}
	routes := bindingRoutes(binding)
	index := ledger.RouteIndex
	if index >= len(routes) {
		index = len(routes) - 1
	}
	return routes[index]
}

// rerouteDecision is one parked run's outcome under the operator's reroute:
// per-role policy applies per run (red-team R6) — a stale helper becomes a
// draft for the operator instead of relaunching.
type rerouteDecision struct {
	RunID    string             `json:"run_id"`
	Role     string             `json:"role"`
	Action   string             `json:"action"` // "reroute" | "stale_draft" | "no_route"
	Target   store.ManagedRoute `json:"target,omitempty"`
	bindings store.ManagedBinding
}

// planProviderReroute computes what the operator's reroute WOULD do to every
// parked run currently pointed at (runtime, model) — the preview and the
// apply share this one decision function; the preview digest pins it.
func (host *orchestrationManagedHost) planProviderReroute(runtimeName, model string) ([]rerouteDecision, error) {
	parked, err := host.ix.ParkedManagedRuns(200)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	decisions := []rerouteDecision{}
	for _, run := range parked {
		ledger := providerOutageLedgerFrom(run.Detail)
		binding, found, bindingErr := host.ix.ManagedBinding(run.BindingID)
		if bindingErr != nil || !found || binding.State != "enabled" {
			continue
		}
		routes := bindingRoutes(binding)
		index := ledger.RouteIndex
		if index >= len(routes) {
			index = len(routes) - 1
		}
		if routes[index].Runtime != runtimeName || routes[index].Model != model {
			continue
		}
		decision := rerouteDecision{RunID: run.RunID, Role: run.Role, bindings: binding}
		policy := providerRetryPolicyFor(run.Role)
		age := time.Duration(now-run.AdmittedAt) * time.Second
		switch {
		case run.Role == "helper" && policy.Freshness > 0 && age > policy.Freshness:
			decision.Action = "stale_draft"
		case host.nextReadableRoute(routes, index+1) >= 0:
			// The target is a chain entry, referenced by route (plan §5.3); an entry
			// whose route cannot be read is passed over, and the relaunch names it.
			decision.Action = "reroute"
			decision.Target = routes[host.nextReadableRoute(routes, index+1)]
		default:
			decision.Action = "no_route"
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

// nextReadableRoute is the first chain index at or after from whose model route can be
// read, or -1.
func (host *orchestrationManagedHost) nextReadableRoute(routes []store.ManagedRoute, from int) int {
	for index := from; index < len(routes); index++ {
		if !host.chainRouteMissing(routes[index]) {
			return index
		}
	}
	return -1
}

func rerouteDigest(decisions []rerouteDecision) string {
	body, _ := json.Marshal(decisions)
	return managedID("orrd_", string(body))
}

// applyProviderReroute executes a confirmed reroute plan: rerouting runs
// advance their route index and become immediately eligible; stale helpers
// settle as stale_unsent drafts (never a late auto-send); runs with no next
// route stay parked awaiting a chain edit.
func (host *orchestrationManagedHost) applyProviderReroute(decisions []rerouteDecision) (rerouted, staleDrafts, unrouted int, err error) {
	now := time.Now().Unix()
	for _, decision := range decisions {
		run, found, runErr := host.ix.ManagedRun(decision.RunID)
		if runErr != nil || !found || run.State != "parked" {
			continue // settled or relaunched since the preview; the digest already bounded drift
		}
		ledger := providerOutageLedgerFrom(run.Detail)
		switch decision.Action {
		case "reroute":
			ledger.RouteIndex++
			ledger.NextEligibleAt = now
			// The run is already parked; only its ledger moves — the next
			// cadence pass relaunches it on the operator-chosen route.
			if mergeErr := host.ix.MergeManagedRunDetail(run.RunID, map[string]any{"provider_outage": ledger}); mergeErr != nil {
				err = mergeErr
				continue
			}
			rerouted++
		case "stale_draft":
			policy := providerRetryPolicyFor(run.Role)
			if settleErr := host.settleProviderWindowLapse(run, TaskEvent{TaskID: run.ChildTaskID}, policy, ledger.Class,
				providerRecoveryText(ledger.Class, ledger.ProviderDetail), now); settleErr != nil {
				err = settleErr
				continue
			}
			staleDrafts++
		default:
			unrouted++
		}
	}
	return rerouted, staleDrafts, unrouted, err
}
