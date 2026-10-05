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
// want to stop EVERY tool call") — fail-open because no rule's fail_mode is read today.
// Two independent guarantees make daemon-down never block a tool call:
//   1. Architecture: the hook's static tier EXCLUDES every rule that references
//      session:/target:/agent: state (engine.StaticTier via guardcli.StaticDecide), so a
//      stateful rule cannot fire without the daemon — by construction. Tag absence was
//      not enough: a NEGATED state term is vacuously true over a stateless tag set.
//   2. Platform gate: even reachable, the daemon refuses to ARM stateful enforcement on
//      a GOOS whose capability record is not demonstrated (item 8) — it returns "allow".
//
// Only rules that actually reference session:*/target:*/agent:* are evaluated here; a pure
// command rule is the static tier's job and is not double-handled. They are decided over
// the same action tags as the hook's tiers (engine.ActionTags) plus the state. A target:*
// term on an action with no single target (a shell command) is UNDECIDED, not absent:
// the rule does not fire and the decide response counts it. The rules come from
// the same layered loader the hook uses (user ++ this checkout's repository layer ++
// the organization layer, rulebook.LoadLayeredFull), so layer stamps and precedence
// are the static loader's own. The hook's CG_POLICY invocation file is out of scope:
// it lives in the hook's environment, not the daemon's.

import (
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/rulebook"
)

// sessionPrefix / targetPrefix namespace the two state gates' tags (ADR 0025 §3). They
// alias the engine's one definition, so the tag builders here, the stateful-rule selector
// and the coverage compiler cannot drift. Model-claimed tags from the optional agents
// layer carry the full key agent:<binding-id>:<tag> (engine.AgentStatePrefix) — the agent
// identity is inside the key so a fired rule's readable output names exactly whose claim
// acted (C7: a derived fact enters enforcement only through a rule that visibly declares
// it). Tag names come from profile-declared vocabularies; nothing here knows any tag name.
const (
	sessionPrefix = engine.SessionStatePrefix
	targetPrefix  = engine.TargetStatePrefix
)

// DecideStateful evaluates the STATEFUL rules over the namespaced union of this
// action's tags, the session's folded state (session:*), and the target entity's state
// (target:*). Returns nil when there is nothing stateful to say — no armed platform, no
// stateful rules, or no stateful rule fired — so the caller (and the hook) treats nil
// as "proceed / defer to the static tier".
//
// A team layer that cannot be read contributes no rules (the loader's own semantics);
// that is fail-open because the evaluator is monotone in its rule set — fewer rules can
// never raise a verdict. A relative or empty cwd selects no repository layer: the
// repository index is keyed by the absolute checkout roots the daemon resolved, and a
// relative lookup's root is never one of them.
//
// The int is how many GATING rules were left undecided: a rule reading target:* state on
// an action that resolves no single target (a shell command). There is no entity whose
// state could answer the term, so the rule does not fire — and the count says so, rather
// than a `not: target:x` term reading as true for every shell command.
func (g *Governor) DecideStateful(o Observation) (*engine.Decision, int, error) {
	// Platform gate (item 8): no stateful enforcement where uninstall/service/ACL are
	// not demonstrated. This is a fail-open-by-platform: an unready GOOS never blocks.
	if !g.platform.StatefulEnforcementReady() {
		return nil, 0, nil
	}
	layered, err := rulebook.LoadLayeredFull(o.Cwd, g.layerStore)
	if err != nil {
		return nil, 0, err
	}
	stateful := statefulRules(layered.Policy)
	if len(stateful.Rules) == 0 {
		return nil, 0, nil // nothing references session:/target: — the static tier covers everything
	}

	n := Normalize(o)
	// The same action tags the static tiers decide over (detector tags, the raw command
	// and the bare tool identity no detector emits), so a stateful rule's `tool=` term
	// means here what it means in the hook — and a `not: tool=…` term is not vacuous.
	tags := engine.ActionTags(n.Event, g.dets, o.Command)
	// session:* — the session's accumulated state (prior actions already folded).
	// FAIL-OPEN on a read error: never evaluate a stateful rule over a tag set we know
	// is INCOMPLETE. A partial set is worse than none — a negation term (`not: session:x`)
	// becomes TRUE when x is merely missing, so a transient DB read error would turn a
	// "deny unless reviewed" rule into a false BLOCK. The handler maps this err to
	// allow/evaluated=false, so a degraded daemon still lets the tool proceed.
	ss, err := g.ix.SessionState(o.SessionID)
	if err != nil {
		return nil, 0, err
	}
	for _, s := range ss {
		tags = append(tags, engine.Tag{Key: sessionPrefix + s.Key, Value: s.Value})
	}
	// target:* — what we believe about the resource this action touches.
	if n.TargetID != "" {
		es, err := g.ix.EntityState(n.TargetID)
		if err != nil {
			return nil, 0, err
		}
		for _, s := range es {
			tags = append(tags, engine.Tag{Key: targetPrefix + s.Key, Value: s.Value})
		}
	}

	// agent:* — model-claimed tags from the optional agents layer. Same fail-open
	// rule as session state: an incomplete tag set is never evaluated, so a read
	// error defers to the static tier rather than inverting a `not: agent:x` term.
	// The keys read is uncut for the same reason; the capped row read is display-only.
	agentTags, err := g.ix.ActiveOrchestrationTagKeys(o.SessionID, time.Now().Unix())
	if err != nil {
		return nil, 0, err
	}
	for _, tag := range agentTags {
		// The value is the stored provenance ("model-claimed"), not an invented
		// synonym — the audit trail and any value-matching rule see the same
		// word the tag row carries.
		tags = append(tags, engine.Tag{Key: tag.AgentKey, Value: tag.Provenance})
	}

	// Honor the severity ladder, do not collapse it: the decision folds HardBlock AND
	// ConfirmAndRecord into "block" (the distinction is only in d.Mode), and emits
	// "warn" for WarnAndProceed. Return a verdict ONLY for the two that actually gate —
	// HardBlock (a hard deny) and ConfirmAndRecord (an OVERRIDABLE ask). A warn or a
	// non-firing rule proceeds; without this, a warn rule became a false deny and a
	// confirm rule silently lost its override.
	var unknown engine.Unknown
	if n.TargetID == "" {
		unknown = engine.UnknownTarget
	}
	d, seen := engine.DecideSeeing(tags, stateful, unknown)
	if d.Mode.Gates() {
		return &d, seen.GatingUndecided(), nil
	}
	return nil, seen.GatingUndecided(), nil // warn / silent / allow — defer to the static tier, proceed
}

// statefulRules is the subset whose predicate references session:*/target: state — the
// rules only the daemon can evaluate. A pure command/action rule is excluded: it is the
// hook's local static tier and must not be enforced twice. A rule that reads a route:
// fact is excluded too (OD-25): no tool call carries one, so here a negated route: term
// would be vacuously true. Such a rule is decided only by route admission, and a rule
// that mixes route: with state is refused when written (engine.ValidateRouteRules).
func statefulRules(pol *engine.Policy) *engine.Policy {
	out := &engine.Policy{Capabilities: pol.Capabilities}
	for _, r := range pol.Rules {
		if engine.ReferencesState(r.If) && !engine.ReferencesRoute(r.If) {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}
