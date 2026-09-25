package daemon

// Phase 3 — the stateful enforcement tier (governance plan; ADR 0025 §Decision 3).
//
// The static tier is the hook's own business: it evaluates command/action rules
// LOCALLY, needs no daemon, and is always on. This is the STATEFUL tier — decisions
// that depend on the session's folded state (`session:*`) or the target resource's
// state (`target:*`), which only the daemon holds. The hook consults this at decision
// time.
//
// FAIL-OPEN is the law here (owner decision 2026-07-20: "if the daemon fails I don't
// want to stop EVERY tool call"). Two independent guarantees make daemon-down never
// block a tool call:
//   1. Architecture: without the daemon the hook has no session:*/target:* tags, so a
//      stateful rule cannot fire — the hook's local static Decide is all that runs.
//   2. Platform gate: even reachable, the daemon refuses to ARM stateful enforcement on
//      a GOOS whose capability record is not demonstrated (item 8) — it returns "allow".
//
// Only rules that actually reference session:*/target: are evaluated here; a pure
// command rule is the static tier's job and is not double-handled.

import (
	"time"

	"strings"

	"crossing-guard/engine"
	"crossing-guard/internal/rulebook"
)

// sessionPrefix / targetPrefix namespace the two state gates' tags (ADR 0025 §3). Named
// once so a rename can't drift between the tag builders and the stateful-rule detector.
const (
	sessionPrefix = "session:"
	targetPrefix  = "target:"
	// agentPrefix namespaces MODEL-CLAIMED tags from the optional agents layer.
	// The full key form is agent:<binding-id>:<tag> — the agent identity is inside
	// the key so a fired rule's readable output names exactly whose claim acted
	// (C7: a derived fact enters enforcement only through a rule that visibly
	// declares it). Tag names come from profile-declared vocabularies; nothing
	// here knows any tag name.
	agentPrefix = "agent:"
)

// DecideStateful evaluates the STATEFUL rules over the namespaced union of this
// action's tags, the session's folded state (session:*), and the target entity's state
// (target:*). Returns nil when there is nothing stateful to say — no armed platform, no
// stateful rules, or no stateful rule fired — so the caller (and the hook) treats nil
// as "proceed / defer to the static tier".
func (g *Governor) DecideStateful(o Observation) (*engine.Decision, error) {
	// Platform gate (item 8): no stateful enforcement where uninstall/service/ACL are
	// not demonstrated. This is a fail-open-by-platform: an unready GOOS never blocks.
	if !g.platform.StatefulEnforcementReady() {
		return nil, nil
	}
	pol, err := rulebook.Load()
	if err != nil {
		return nil, err
	}
	stateful := statefulRules(pol)
	if len(stateful.Rules) == 0 {
		return nil, nil // nothing references session:/target: — the static tier covers everything
	}

	n := Normalize(o)
	tags := engine.Classify(n.Event, g.dets)
	tags = append(tags, engine.Tag{Key: engine.CommandTagKey, Value: o.Command})
	// session:* — the session's accumulated state (prior actions already folded).
	// FAIL-OPEN on a read error: never evaluate a stateful rule over a tag set we know
	// is INCOMPLETE. A partial set is worse than none — a negation term (`not: session:x`)
	// becomes TRUE when x is merely missing, so a transient DB read error would turn a
	// "deny unless reviewed" rule into a false BLOCK. The handler maps this err to
	// allow/evaluated=false, so a degraded daemon still lets the tool proceed.
	ss, err := g.ix.SessionState(o.SessionID)
	if err != nil {
		return nil, err
	}
	for _, s := range ss {
		tags = append(tags, engine.Tag{Key: sessionPrefix + s.Key, Value: s.Value})
	}
	// target:* — what we believe about the resource this action touches.
	if n.TargetID != "" {
		es, err := g.ix.EntityState(n.TargetID)
		if err != nil {
			return nil, err
		}
		for _, s := range es {
			tags = append(tags, engine.Tag{Key: targetPrefix + s.Key, Value: s.Value})
		}
	}

	// agent:* — model-claimed tags from the optional agents layer. Same fail-open
	// rule as session state: an incomplete tag set is never evaluated, so a read
	// error defers to the static tier rather than inverting a `not: agent:x` term.
	agentTags, err := g.ix.ActiveOrchestrationTags(o.SessionID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	for _, tag := range agentTags {
		// The value is the stored provenance ("model-claimed"), not an invented
		// synonym — the audit trail and any value-matching rule see the same
		// word the tag row carries.
		tags = append(tags, engine.Tag{Key: tag.AgentKey, Value: tag.Provenance})
	}

	// Honor the severity ladder, do not collapse it: engine.Decide folds HardBlock AND
	// ConfirmAndRecord into "block" (the distinction is only in d.Mode), and emits
	// "warn" for WarnAndProceed. Return a verdict ONLY for the two that actually gate —
	// HardBlock (a hard deny) and ConfirmAndRecord (an OVERRIDABLE ask). A warn or a
	// non-firing rule proceeds; without this, a warn rule became a false deny and a
	// confirm rule silently lost its override.
	d := engine.Decide(tags, stateful)
	switch d.Mode {
	case engine.HardBlock, engine.ConfirmAndRecord:
		return &d, nil
	default:
		return nil, nil // warn / silent / allow — defer to the static tier, proceed
	}
}

// statefulRules is the subset whose predicate references session:*/target: state — the
// rules only the daemon can evaluate. A pure command/action rule is excluded: it is the
// hook's local static tier and must not be enforced twice.
func statefulRules(pol *engine.Policy) *engine.Policy {
	out := &engine.Policy{Capabilities: pol.Capabilities}
	for _, r := range pol.Rules {
		if predicateReferencesState(r.If) {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}

// predicateReferencesState reports whether a predicate tree contains any term whose Tag
// is namespaced session:/target: — i.e. a fact only the daemon's fold provides.
func predicateReferencesState(p engine.Predicate) bool {
	for _, c := range p.All {
		if predicateReferencesState(c) {
			return true
		}
	}
	for _, c := range p.Any {
		if predicateReferencesState(c) {
			return true
		}
	}
	if p.Not != nil && predicateReferencesState(*p.Not) {
		return true
	}
	return hasStatePrefix(p.Tag)
}

func hasStatePrefix(tag string) bool {
	return strings.HasPrefix(tag, sessionPrefix) || strings.HasPrefix(tag, targetPrefix) ||
		strings.HasPrefix(tag, agentPrefix)
}
