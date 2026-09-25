package daemon

// Agent stage routing, budget policy, and priority arbitration for the managed
// orchestration host (agents redesign plan §2/§10). Routing is DATA: the
// published signal catalog plus the profile's open selector→prompt map decide
// what fires — no stage enum, tag name, or workflow vocabulary lives in code
// (owner rule, 2026-08-29).

import (
	"errors"
	"sort"

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
	return nil
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
