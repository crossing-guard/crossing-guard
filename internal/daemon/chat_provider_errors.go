package daemon

// Provider-failure classification (provider-outage plan, Slice A). A managed
// run that dies because its PROVIDER is down/quota-walled is a different fact
// from an agent that did its job badly; these classes carry that distinction
// from the vendor edge into the durable task-event stream, where the
// orchestration layer (Slice B) reads them.
//
// R3 (red-team, folded): classification reads ONLY the vendor's
// transport/error channel — a driver's typed error event or the provider
// CLI's own stderr — never assistant or tool content. A session that merely
// PRINTS a quota message must not steer parking, breakers, or reroutes.
// Anything not positively classified stays an ordinary failure, so a miss
// can only degrade to today's behavior.

import "strings"

const (
	// providerErrorQuota: the provider answered and said "no more" — a
	// usage/rate ceiling with a human-scale recovery (reset or upgrade).
	providerErrorQuota = "provider_quota"
	// providerErrorUnavailable: the provider did not usefully answer —
	// refused, unreachable, gateway-dead, or timed out.
	providerErrorUnavailable = "provider_unavailable"
	// providerErrorAuth: the provider wants credentials; recovery is the
	// existing vendor sign-in lane.
	providerErrorAuth = "provider_auth"
)

// sharedProviderErrorClass classifies vendor-neutral transport shapes.
// Vendor-SPECIFIC phrasings belong in each chat_<vendor>.go error path
// (ADR 0020); backend-provider phrasings shared by several runtimes (the
// Ollama usage-limit wording reaches both the opencode driver and codex's
// local-provider lane) live here. Phrase-based on purpose — no bare status
// digits, which over-match ids and byte counts. Returns "" for anything it
// cannot positively classify.
func sharedProviderErrorClass(line string) string {
	if line == "" {
		return ""
	}
	if isVendorAuthFailure(line) {
		return providerErrorAuth
	}
	s := strings.ToLower(line)
	for _, phrase := range []string{
		"too many requests", "rate limit", "quota exceeded",
		// Ollama cloud's exact wall, measured 2026-08-30: "you (…) have
		// reached your session usage limit, upgrade for higher limits".
		"usage limit",
	} {
		if strings.Contains(s, phrase) {
			return providerErrorQuota
		}
	}
	for _, phrase := range []string{
		"bad gateway", "service unavailable", "gateway timeout",
		"connection refused", "econnrefused", "etimedout", "timed out",
		"no such host", "network is unreachable", "connection reset",
	} {
		if strings.Contains(s, phrase) {
			return providerErrorUnavailable
		}
	}
	return ""
}

// classifiedProviderError decorates one already-typed error event with its
// provider class when the transport text classifies; events that do not
// classify pass through untouched.
func classifiedProviderError(event ChatEvent, transportText string) ChatEvent {
	if class := sharedProviderErrorClass(transportText); class != "" {
		event["error_class"] = class
	}
	return event
}
