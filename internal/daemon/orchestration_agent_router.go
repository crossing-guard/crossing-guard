package daemon

// Agent stage routing, budget policy, and priority arbitration for the managed
// orchestration host (agents redesign plan §2/§10). Routing is DATA: the
// published signal catalog plus the profile's open selector→prompt map decide
// what fires — no stage enum, tag name, or workflow vocabulary lives in code
// (owner rule, 2026-08-29).

import (
	"errors"
	"sort"
	"unicode/utf8"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// shippedAgentDefaults is CONFIGURATION shipped with the product, not code
// truth: the owner-overridable budget defaults resolved when a binding's
// Limits field leaves a value at zero (plan §2, owner decision Q3 — the loop
// budget default is shipped configuration, never a compiled constant). Budget
// POLICY lives here at the host; the store enforces shape only. Zero token
// budgets mean "unenforced" until harvested token telemetry is wired.
// MaxTotal counts TURNS per (binding, source session) since schema 30 made
// the group span the whole source session (helper-persistent-session plan
// D4): with mid-turn signals coalescing, a follower's turn count tracks the
// source's own turn count, and 8 ended the helper one question into a real
// session (red-team finding 6). 48 is the shipped default; the binding's
// Limits override it per agent in Settings → Agents.
var shippedAgentDefaults = store.ManagedLimits{
	MaxTotal:       48,
	MaxActive:      2,
	MaxHops:        1,
	LoopBudget:     5,
	MaxGroupTokens: 0,
	MaxAgentTokens: 0,
}

// enforcedAgentLimits names the binding budgets admission enforces today:
// max_total per (binding, source session) in agentGroupBudget, loop_budget in
// the reply loop. The other stored budgets are validated and kept but not
// checked, so the Agents page offers only these (plan D-9, RT-18).
func enforcedAgentLimits() []string { return []string{"max_total", "loop_budget"} }

// unenforcedProfileLimits names the profile fields the managed path does not
// apply to helpers and followers (only the review path reads them); the page
// states it beside each ceiling instead of presenting it as a limit.
func unenforcedProfileLimits() []string {
	return []string{"timeout", "max_tokens", "failure", "locality"}
}

// resolveAgentLimits resolves one binding's effective budgets: each explicit
// binding override wins; zero falls back to the shipped default configuration.
func resolveAgentLimits(overrides store.ManagedLimits) store.ManagedLimits {
	resolved := shippedAgentDefaults
	if overrides.MaxTotal > 0 {
		resolved.MaxTotal = overrides.MaxTotal
	}
	if overrides.MaxActive > 0 {
		resolved.MaxActive = overrides.MaxActive
	}
	if overrides.MaxHops > 0 {
		resolved.MaxHops = overrides.MaxHops
	}
	if overrides.LoopBudget > 0 {
		resolved.LoopBudget = overrides.LoopBudget
	}
	if overrides.MaxGroupTokens > 0 {
		resolved.MaxGroupTokens = overrides.MaxGroupTokens
	}
	if overrides.MaxAgentTokens > 0 {
		resolved.MaxAgentTokens = overrides.MaxAgentTokens
	}
	return resolved
}

// agentGroupBudget is the resolved admission budget the store requires; the
// store never invents budget numbers (schema 24).
func agentGroupBudget(binding store.ManagedBinding) store.ManagedGroupBudget {
	limits := resolveAgentLimits(binding.Limits)
	return store.ManagedGroupBudget{MaxTotal: limits.MaxTotal, MaxActive: limits.MaxActive}
}

// turnAnchorFor is the seam for harvest's TurnAnchorer capability (plan §6).
// TODO(turn-anchors): another pass lands the TurnAnchorer harvest capability;
// the integrator wires this seam to it. While it returns "", reply anchors stay
// empty and badges attach at the task boundary — identity is never guessed.
var turnAnchorFor = func(runtime, nativeSessionID, taskID string) string { return "" }

// profileSignalSelectors returns the profile's selector→prompt map as data. A
// single-prompt v1 profile maps its one trigger event — already a catalog kind
// on the managed path — to its whole instructions, so v1 profiles stay valid.
func profileSignalSelectors(compiled profilefs.CompiledProfile) map[string]string {
	authored := compiled.Stages
	if len(authored) == 0 {
		authored = map[string]string{compiled.Trigger.Event: compiled.Instructions}
	}
	selectors := make(map[string]string, len(authored))
	if fallback, ok := authored["*"]; ok {
		// A superseded kind (task.completed, task.tool-completed) reports a
		// fact its session-scoped successor also reports; the wildcard takes
		// the successor only, so one fact fires the binding once (plan H3).
		for _, signal := range orchestration.SignalCatalog() {
			if signal.Superseded != "" {
				continue
			}
			selectors[signal.Kind] = fallback
		}
	}
	for signal, prompt := range authored {
		if signal != "*" {
			selectors[signal] = prompt
		}
	}
	return selectors
}

// validateManagedProfile gates a profile onto the managed path. Structural
// shape is fixed (one hop, one depth, no retries); everything trigger-shaped is
// checked against the published signal catalog through the profile's selector
// map — an authored-but-unpublished selector makes the binding incompatible,
// never silently ignored.
func validateManagedProfile(profile profilefs.CompiledProfile) error {
	// Depth stays 1 (a helper's child never spawns) and retries stay 0 — those
	// are version invariants. Hops are the OWNER'S budget (plan §2): the profile
	// may declare any value profilefs accepts; the loop budget governs cycles.
	if profile.Execution != "managed-turn" || profile.Limits.MaxDepth != 1 ||
		profile.Limits.MaxHops < 1 || profile.Limits.MaxRetries != 0 {
		return errors.New("managed profiles require one depth, at least one hop, and no retries")
	}
	agentType := profile.AgentType()
	if agentType != "follower" && agentType != "helper" {
		return errors.New("profile type is not a managed orchestration agent")
	}
	for selector := range profileSignalSelectors(profile) {
		if !orchestration.KnownSignal(selector) {
			return errors.New("profile selector " + selector + " is not a published signal")
		}
	}
	return validateManagedHostSupport(profile)
}

// validateManagedHostSupport refuses compiled values this host does not
// implement for managed turns (managed-turn-profile-limits plan §3): a field
// the profile relies on is enforced or refused here, never silently ignored.
// The refusal is the profile's "cannot be enabled here" reason; the portable
// file stays valid for a host that does support the value.
func validateManagedHostSupport(profile profilefs.CompiledProfile) error {
	if profile.Requirements.Destination.Locality == "no-network-destination" {
		return errors.New("requirements.destination.locality no-network-destination cannot be met by a managed turn: a runtime harness cannot be shown to make no network calls; use local-only with a local route")
	}
	if len(profile.Trigger.States) != 0 {
		return errors.New("managed turns do not filter by task state; remove trigger.states")
	}
	if profile.Trigger.IgnoreOrigin != "self" {
		return errors.New("managed turns never hear their own turns; trigger.ignore-origin must be self")
	}
	if debounce, err := profilefs.Duration(profile.Trigger.Debounce); err != nil || debounce != 0 {
		return errors.New("managed turns coalesce signals while the helper session is busy; a debounce window is not supported, use 0s")
	}
	for _, behavior := range []string{profile.Failure.MissingRequiredContext, profile.Failure.UnavailableCapability,
		profile.Failure.Timeout, profile.Failure.MalformedOutput} {
		if behavior != "record-unavailable" {
			return errors.New("managed turns support failure behavior record-unavailable only")
		}
	}
	if _, err := profilefs.Duration(profile.Limits.Timeout); err != nil {
		return errors.New("limits.timeout is unreadable")
	}
	return nil
}

// managedRouteDestinationProblem judges one route against the pinned profile's
// destination requirement (plan §4.1); "" means the route may run the profile.
// Local is the adapter's claim about the route's endpoint class — never an
// egress proof — and a runtime that makes no claim is not local.
func managedRouteDestinationProblem(profile profilefs.CompiledProfile, route store.ManagedRoute) string {
	locality := profile.Requirements.Destination.Locality
	if locality == "explicit-local-or-remote" {
		return ""
	}
	if locality != "local-only" {
		return boundedRecovery("Profile " + profile.ID + " requires destination " + locality + ", which no managed route can meet.")
	}
	if local, _ := chatRouteIsLocal(ChatRequest{Runtime: route.Runtime, Model: route.Model, Mode: route.Mode}); local {
		return ""
	}
	// The sentence is shown on agent surfaces, so it names no runtime and no model
	// (plan §5.5, §14 Q8): the person picks another route by name in Settings → Models.
	text := "Profile " + profile.ID + " requires a local-only destination, and the chosen model route leaves this machine. " +
		"Choose a model route that stays on this machine, or deploy a profile revision whose requirements.destination.locality is explicit-local-or-remote"
	if chatRuntimeOffersLocalModel(route.Runtime) && profile.AgentType() == "helper" {
		text += " (a helper's reply or correction resumes the source session with the route's model, so a local route works only on sources of the same runtime)"
	}
	return boundedRecovery(text + ".")
}

// bindingDestinationProblem is the first destination problem across a
// binding's routes (primary, then fallbacks) and its allowlisted child
// profiles judged on the primary route; "" when every one may run.
func bindingDestinationProblem(profile profilefs.CompiledProfile, routes []store.ManagedRoute, children []profilefs.CompiledProfile) string {
	for _, route := range routes {
		if problem := managedRouteDestinationProblem(profile, route); problem != "" {
			return problem
		}
	}
	if len(routes) == 0 {
		return ""
	}
	for _, child := range children {
		if problem := managedRouteDestinationProblem(child, routes[0]); problem != "" {
			return problem
		}
	}
	return ""
}

// crossRuntimeResumeText explains why a reply or correction may not resume a
// session on another runtime with a model the binding's adapter runs locally
// (plan §4.1, D-5). sourceRuntime "" means "any other runtime".
func crossRuntimeResumeText(route store.ManagedRoute, sourceRuntime string) string {
	source := "another runtime's session"
	if sourceRuntime != "" {
		source = "the " + chatRuntimeLabel(sourceRuntime) + " session"
	}
	return "This deployment's model (" + chatModelLabel(route.Runtime, route.Model) + ") runs only on " +
		chatRuntimeLabel(route.Runtime) + ", and a reply or correction resumes " + source + " with it."
}

// managedRecoveryLimit is the store's recovery column bound (CHECK
// length(recovery)<=2000); a longer text would fail the insert.
const managedRecoveryLimit = 2000

// boundedRecovery fits recovery text to the column, cutting on a rune boundary.
func boundedRecovery(text string) string {
	if len(text) <= managedRecoveryLimit {
		return text
	}
	cut := managedRecoveryLimit - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// matchedAgent is one enabled binding whose scope and selector map matched a
// live signal, with its pinned compiled profile and the stage prompt the
// selector chose.
type matchedAgent struct {
	binding     store.ManagedBinding
	compiled    profilefs.CompiledProfile
	stagePrompt string
}

// arbitrateAgents implements priority arbitration (owner decision Q1): passive
// followers all run; among helpers only the highest binding priority acts and
// every other matching helper is deferred without consuming budget. Ties break
// on binding ID so arbitration is deterministic.
func arbitrateAgents(matches []matchedAgent) (act []matchedAgent, deferred []matchedAgent, winner string) {
	helperIndex := -1
	for index, match := range matches {
		if match.binding.Role != "helper" {
			act = append(act, match)
			continue
		}
		if helperIndex == -1 {
			helperIndex = index
			continue
		}
		current := matches[helperIndex].binding
		candidate := match.binding
		if candidate.Priority > current.Priority ||
			(candidate.Priority == current.Priority && candidate.BindingID < current.BindingID) {
			deferred = append(deferred, matches[helperIndex])
			helperIndex = index
		} else {
			deferred = append(deferred, match)
		}
	}
	if helperIndex >= 0 {
		act = append(act, matches[helperIndex])
		winner = matches[helperIndex].binding.BindingID
	}
	sort.SliceStable(deferred, func(i, j int) bool {
		return deferred[i].binding.BindingID < deferred[j].binding.BindingID
	})
	return act, deferred, winner
}
