package engine

// The rule compiler's honest label (P-COMPILE-2; ADR 0006 + 0015):
//
//	boundary(rule) = detection-coverage(its predicate's terms × the detector
//	                 set and the declared state producers) ∧
//	                 enforcement-reach(the caller's channel)
//
// A live label is the disjunction over the live tiers that load the rule, each
// evaluated on its own tag source (see producerChannel): a rule can fire only
// where one tier can fire its whole predicate.
//
// Everything here is MECHANICAL — derived from declared detector coverage and
// the predicate tree, never asserted. The detection vocabulary is closed:
//
//	"enumerable (declared gaps)"      every reachable term is backed by at
//	                                  least one enumerable producer
//	"coverage-limited (precautionary)" some reachable term is detectable only
//	                                  through unknowable-gap detectors — the
//	                                  rule may block on a hit but can NEVER
//	                                  claim it catches everything
//	"inert (cannot fire)"             in every loading tier a required term
//	                                  has no producer
//	"unverified"                      a required term reads state whose
//	                                  producers this surface cannot see (the
//	                                  caller could not read the binding set),
//	                                  or only a tier whose loading it cannot
//	                                  see could fire the rule; firing is
//	                                  neither claimed nor ruled out
//
// "confirmed-complete" is deliberately not in the vocabulary: no computation
// can produce it, so no surface can render it (the §9 no-bluffing invariant).

import (
	"sort"
	"strings"
)

// Canonical reach strings (design §8) — callers pass the one matching their
// channel; anything else is passed through verbatim.
const (
	ReachStop    = "live hook — STOP reach (deny/ask before execution)"
	ReachHarvest = "harvest — post-hoc detect/report only, never a live block"
	ReachDryRun  = "dry-run — no enforcement"

	// The static reaches: the hook's tiers still decide the rule live, but on this
	// action's tags only — no session, target or agent state ever reaches it. Each names
	// why the daemon's stateful tier does not.
	ReachStopUnarmed      = "live hook — STOP reach, this action's tags only: stateful tier not armed on this platform"
	ReachStopNotLoaded    = "live hook — STOP reach, this action's tags only: the stateful tier does not load this rule set"
	ReachStopStatefulDown = "live hook — STOP reach, this action's tags only: the daemon's stateful tier is not running"
)

// StatefulTier is what the daemon's stateful tier does on this host with the
// state-referencing rules of the sets it loads: the user rulebook and the adopted team
// layers of the session's checkout.
type StatefulTier int

const (
	StatefulArmed   StatefulTier = iota // loads its sets and is armed here
	StatefulUnarmed                     // loads them, but this platform never arms it
	StatefulDown                        // would load them, but is not running (no governor)
)

// SetKind names the rule set a live coverage surface labels. Which live tiers can load a
// set follows from its kind: the standalone and stateful tiers load the user rulebook
// and the team layers, the hook's engine tier the invocation file and — depending on
// that file — the other two.
type SetKind int

const (
	SetUser       SetKind = iota // the user rulebook (rules.json or the shipped default)
	SetTeam                      // this checkout's team layers
	SetInvocation                // the invocation file (CG_POLICY, policy.json)
)

// EngineLoad is whether the hook's engine tier loads a user or team rule set in the
// caller's environment. The zero value is the safe one: a surface that cannot see the
// hook's invocation file never turns a label INERT on that guess.
type EngineLoad int

const (
	EngineUnknown EngineLoad = iota // the caller cannot see the hook's invocation file
	EngineLoads                     // the engine tier loads the set here
	EngineSkips                     // the engine tier does not load the set here
)

// LiveRuleSet describes a rule set a live coverage surface labels.
type LiveRuleSet struct {
	Kind SetKind
	// Tier is read for SetUser and SetTeam: the invocation file never reaches the stateful tier.
	Tier StatefulTier
	// HookEngine is read for SetUser and SetTeam; the engine tier is the only tier that
	// loads SetInvocation.
	HookEngine EngineLoad
}

// Tiers is a set of the live evaluators a rule is labeled over.
type Tiers uint8

const (
	TierEngine     Tiers = 1 << iota // the hook's engine tier: the invocation file's rule set; rules with no state term
	TierStandalone                   // the hook's standalone tier: the user rulebook and team layers; rules with no state term
	TierStateful                     // the daemon's stateful tier: detector tags, command, tool, state; rules with a state term
)

// LiveReach is the reach a live coverage surface labels a rule at: the tiers that load
// it on this host. A rule with no state term is decided by the hook's tiers wherever it
// sits. A rule that reads state is never evaluated by the hook (engine.StaticTier): only
// the stateful tier evaluates it, for the user rulebook and the adopted team layers, and
// it acts only on a gating rule. A non-gating (warn, silent-log) state rule in the user
// rulebook therefore acts only in the audit, which loads that rulebook alone. The hook's
// invocation file never reaches the stateful tier.
func LiveReach(r Rule, set LiveRuleSet) string {
	if !ReferencesState(r.If) {
		return ReachStop
	}
	if set.Kind == SetInvocation {
		return ReachStopNotLoaded
	}
	if set.Kind == SetUser && !gates(r.effectiveMode()) {
		return ReachHarvest
	}
	switch set.Tier {
	case StatefulArmed:
		return ReachStop
	case StatefulUnarmed:
		return ReachStopUnarmed
	default:
		return ReachStopStatefulDown
	}
}

// liveTiers returns the tiers that certainly evaluate a rule of a live set at its reach,
// and whether the engine tier loads it (EngineLoads when it is among them). A harvest
// reach has no live tier. The hook's two tiers evaluate only rules with no state term;
// the stateful tier evaluates only rules that have one, and returns a verdict only for a
// gating rule where it is armed (ReachStop). A state rule anywhere else has no tier.
func liveTiers(r Rule, set LiveRuleSet, reach string) (Tiers, EngineLoad) {
	if !isLiveReach(reach) {
		return 0, EngineSkips
	}
	// A route rule is evaluated only when a place is bound and when a run starts
	// (modelroute.Admit); no tier at the action boundary evaluates it (OD-25), so a
	// coverage surface must not claim the hook stops an action with it.
	if ReferencesRoute(r.If) {
		return 0, EngineSkips
	}
	if ReferencesState(r.If) {
		if set.Kind != SetInvocation && reach == ReachStop && gates(r.effectiveMode()) {
			return TierStateful, EngineSkips
		}
		return 0, EngineSkips
	}
	if set.Kind == SetInvocation {
		return TierEngine, EngineLoads
	}
	t := TierStandalone
	if set.HookEngine == EngineLoads {
		t |= TierEngine
	}
	return t, set.HookEngine
}

// CompileLiveBoundaries labels every rule of a live rule set, each at its LiveReach and
// over the tiers that load it there.
func CompileLiveBoundaries(pol *Policy, dets []Detector, sp StateProducers, set LiveRuleSet) []RuleBoundary {
	out := make([]RuleBoundary, 0, len(pol.Rules))
	for _, r := range pol.Rules {
		reach := LiveReach(r, set)
		tiers, engine := liveTiers(r, set, reach)
		if tiers == 0 && isLiveReach(reach) {
			out = append(out, noTierBoundary(r, dets, sp, reach))
			continue
		}
		out = append(out, boundaryOver(r, dets, sp, reach, tiers, engine))
	}
	return out
}

// TermCoverage reports how one predicate leaf is detectable.
type TermCoverage struct {
	Tag        string   `json:"tag"`
	Value      string   `json:"value,omitempty"`
	Negated    bool     `json:"negated,omitempty"`
	Detectors  []string `json:"detectors"` // detectors able to emit this term's tag
	Enumerable bool     `json:"enumerable"`
	Gaps       []string `json:"gaps,omitempty"`
	// Limits are the declared gaps of the term's unknowable producers: known ways
	// they miss, never a complete list (Gaps holds only the enumerable producers').
	Limits       []string `json:"limits,omitempty"`
	Undetectable bool     `json:"undetectable,omitempty"` // no detector or declared state producer produces it
	// Unverified: no producer is visible AND the term's namespace is unknown to this
	// caller (see StateProducers), so "undetectable" would be a guess. Note says why.
	Unverified bool   `json:"unverified,omitempty"`
	Note       string `json:"note,omitempty"`
	// Tiers names the live tiers labeled for the rule in which this term has a producer
	// (empty at harvest and dry-run reach).
	Tiers []string `json:"tiers,omitempty"`
}

// RuleBoundary is one rule's computed honest label.
type RuleBoundary struct {
	Rule           string         `json:"rule"`
	Mode           Mode           `json:"mode"`
	Detection      []TermCoverage `json:"detection"`
	DetectionLabel string         `json:"detection_label"`
	CanFire        bool           `json:"can_fire"`             // PROVEN able to fire
	Unverified     bool           `json:"unverified,omitempty"` // neither proven able nor unable
	Reach          string         `json:"reach"`
	Label          string         `json:"label"` // detection ∧ reach, one line
	// Tiers is the per-tier verdict behind a live label; the label is their disjunction
	// (empty at harvest and dry-run reach).
	Tiers []TierVerdict `json:"tiers,omitempty"`
}

// TierVerdict is one live tier's own detection state for a rule.
type TierVerdict struct {
	Tier           string `json:"tier"`
	Loads          string `json:"loads"` // "yes", "unknown" (cannot be seen here) or "not here"
	DetectionLabel string `json:"detection_label"`
	CanFire        bool   `json:"can_fire"`
	Unverified     bool   `json:"unverified,omitempty"`
}

// detectionState is a rule's detectability, ordered worst-first for combination.
type detectionState int

const (
	stateInert detectionState = iota
	stateLimited
	stateEnumerable
	stateVacuous // a NOT over a tag nothing produces: always true, trivially covered
	// stateUnverified is outside the worst-first order: a positive term whose producers
	// the caller cannot see. foldChildren handles it explicitly.
	stateUnverified
)

// StateProducer declares a producer of a state term that is not a detector: a session
// fact the daemon authors itself, or a tag a model-claim binding may write. The engine
// names no such tag; its owners declare them (the store's direct-fact catalog and the
// saved bindings' profile-declared tag vocabularies) and pass them in as data.
type StateProducer struct {
	Tag        string   `json:"tag"`    // the full namespaced term key: session:<k> or agent:<binding>:<tag>
	Values     []string `json:"values"` // every value it can emit
	Source     string   `json:"source"` // the producer id a coverage row names
	Enumerable bool     `json:"enumerable"`
	Gaps       []string `json:"gaps,omitempty"`
	Live       bool     `json:"live"`    // the stateful tier can read the fact
	Harvest    bool     `json:"harvest"` // the audit / tags dry-run can read the fact
}

// StateProducers is everything a coverage surface knows about non-detector producers.
// The zero value knows nothing: a live session:/agent: term with no detector producer
// is then UNVERIFIED rather than INERT, because a false INERT is worse than no label.
type StateProducers struct {
	Producers []StateProducer
	// SessionFactsKnown: the daemon's direct session-fact catalog is in Producers.
	SessionFactsKnown bool
	// AgentClaimsKnown: every binding and active claim row was read, so the agent:
	// producers in Producers are complete. AgentClaimsNote says what was read, or why not.
	AgentClaimsKnown bool
	AgentClaimsNote  string
}

// producer is one source a coverage row names: a detector or a declared state producer.
type producer struct {
	id         string
	enumerable bool
	gaps       []string
}

// producerChannel is one evaluator's tag source: which facts can reach a rule's terms
// there. A live label is the disjunction over the channels of the tiers that load the
// rule.
//   - channelEngine, channelStandalone: the hook's two tiers. Both decide over
//     engine.ActionTags: Classify of the action with role "tool_call" (detector tags of
//     any key), the raw command and the exact bare tool identity. No state. They carry
//     the same facts and differ only in the rule sets they load.
//   - channelStateful: the daemon's stateful tier. Classify with role "tool_call"
//     (internal/daemon/normalize.go), the raw command, and state: session:K reads the
//     session fold, which stores every tag of every live event; target:K reads the target
//     entity's fold, which stores only resource-scoped detectors' tags; agent: reads model
//     claims. The raw command and the exact tool identity too (engine.InvocationTags).
//   - channelHarvest: the audit dry-run and the `tags` dry-run. They classify every
//     role, and the audit re-exposes a session's accumulated tags as session:K. Nothing
//     there reads target:K.
type producerChannel int

const (
	channelEngine producerChannel = iota
	channelStandalone
	channelStateful
	channelHarvest
)

// liveTierOrder is the order tiers are reported in, with each tier's channel and name.
var liveTierOrder = []struct {
	tier Tiers
	ch   producerChannel
	name string
}{
	{TierEngine, channelEngine, "hook-engine"},
	{TierStandalone, channelStandalone, "hook-standalone"},
	{TierStateful, channelStateful, "daemon-stateful"},
}

// staticStateNote is the note a state term carries when no labeled tier reads state, so
// UNDETECTABLE is not misread as "no detector emits this".
const staticStateNote = "state is never read at this reach"

// harvestBlindNote is the note a term carries at harvest reach when the dry run has no
// way to read its fact (a single invocation, a target, a live-only session fact).
const harvestBlindNote = "not readable by this dry run"

// isLiveReach reports whether a reach is a live hook reach (ReachStop or a static
// reach). Any other reach string is a post-hoc or dry-run surface.
func isLiveReach(reach string) bool {
	switch reach {
	case ReachStop, ReachStopUnarmed, ReachStopNotLoaded, ReachStopStatefulDown:
		return true
	}
	return false
}

// isStateTerm reports whether a term key names folded state or a model claim.
func isStateTerm(tag string) bool {
	return strings.HasPrefix(tag, SessionStatePrefix) || strings.HasPrefix(tag, TargetStatePrefix) ||
		strings.HasPrefix(tag, AgentStatePrefix)
}

// gates reports whether a mode changes what happens to the action. Only a hard block
// and a confirm do: the daemon's stateful tier returns a verdict for nothing else
// (decide.go), so a warn or silent stateful rule acts only in the audit.
func gates(m Mode) bool { return m == HardBlock || m == ConfirmAndRecord }

// actionVerb is how a precautionary label says what the rule does on a match.
func actionVerb(m Mode) string {
	switch m {
	case HardBlock:
		return "blocks on a matched detector"
	case ConfirmAndRecord:
		return "asks on a matched detector"
	case WarnAndProceed:
		return "warns on a matched detector, never blocks"
	default:
		return "records a matched detector, never blocks"
	}
}

// Boundary computes one rule's honest label against a detector set and the caller's
// reach, over that reach's default tiers. A rule with no state term is labeled over the
// hook's two tiers at any live reach. A rule that reads state is never evaluated by the
// hook: at ReachStop a gating one is labeled over the stateful tier, a non-gating one at
// harvest reach (the stateful tier drops its verdict), and at a static reach no tier
// evaluates it. Anything else is the harvest channel. Coverage surfaces use
// CompileLiveBoundaries instead, which knows which tiers load the set.
func Boundary(r Rule, dets []Detector, sp StateProducers, reach string) RuleBoundary {
	readsState := ReferencesState(r.If)
	// A non-gating state rule acts only in the audit, whoever asks: at ReachStop the
	// stateful tier drops its verdict, and a dry-run caller is told the same (PW-10).
	// A static reach is kept: there the label says no tier evaluates the rule.
	if readsState && !gates(r.effectiveMode()) && (reach == ReachStop || !isLiveReach(reach)) {
		reach = ReachHarvest
	}
	var tiers Tiers
	switch {
	case !isLiveReach(reach):
	case !readsState:
		tiers = TierEngine | TierStandalone
	case reach == ReachStop:
		tiers = TierStateful
	default:
		return noTierBoundary(r, dets, sp, reach)
	}
	return boundaryOver(r, dets, sp, reach, tiers, EngineLoads)
}

// noTierBoundary labels a rule that no live tier evaluates at a live reach: a rule that
// reads state where the stateful tier does not run it. The hook's tiers skip every such
// rule, so it cannot fire whatever the polarity of its state terms.
func noTierBoundary(r Rule, dets []Detector, sp StateProducers, reach string) RuleBoundary {
	b := RuleBoundary{Rule: r.ID, Mode: r.effectiveMode(), Reach: reach,
		Detection:      termCoverages(r.If, producerSources{dets: dets, state: sp}, nil, nil, false),
		DetectionLabel: detectionLabel(stateInert)}
	b.Label = "INERT — no live tier evaluates this rule here: the hook's tiers skip a rule that reads state, and the stateful tier does not run it; this rule can never fire (" + reach + ")"
	return b
}

// BoundaryOn labels a rule over an explicit set of live tiers — e.g. the one tier that
// just fired it. No tiers means the harvest channel.
func BoundaryOn(r Rule, dets []Detector, sp StateProducers, reach string, tiers Tiers) RuleBoundary {
	return boundaryOver(r, dets, sp, reach, tiers, EngineLoads)
}

// CompileBoundaries labels every rule in a policy.
func CompileBoundaries(pol *Policy, dets []Detector, sp StateProducers, reach string) []RuleBoundary {
	out := make([]RuleBoundary, 0, len(pol.Rules))
	for _, r := range pol.Rules {
		out = append(out, Boundary(r, dets, sp, reach))
	}
	return out
}

// boundaryOver is the one labeler. The rule's state is the disjunction of each loading
// tier's own state: it can fire only where one tier can fire the whole predicate. The
// hook's two tiers decide over the same facts, so whether the engine tier loads a set
// here never changes the label: its row is reported with how it loads ("yes", "unknown",
// "not here") and only a tier that certainly loads the rule decides the label.
func boundaryOver(r Rule, dets []Detector, sp StateProducers, reach string, tiers Tiers, engine EngineLoad) RuleBoundary {
	mode := r.effectiveMode()
	b := RuleBoundary{Rule: r.ID, Mode: mode, Reach: reach}
	src := producerSources{dets: dets, state: sp}
	if tiers == 0 {
		src.ch = channelHarvest
		state := predicateState(r.If, src, false)
		b.Detection = termCoverages(r.If, src, []producerChannel{channelHarvest}, nil, false)
		labelBoundary(&b, state)
		return b
	}
	// The engine tier may or may not load the rule here: its row is still shown.
	engineRow := tiers&TierEngine == 0 && (engine == EngineUnknown || engine == EngineSkips) &&
		tiers&TierStandalone != 0
	var rowChans []producerChannel
	var rowNames []string
	var definite []detectionState
	for _, t := range liveTierOrder {
		in := tiers&t.tier != 0
		if !in && !(t.tier == TierEngine && engineRow) {
			continue
		}
		src.ch = t.ch
		st := predicateState(r.If, src, false)
		loads := "yes"
		switch {
		case in:
			definite = append(definite, st)
		case engine == EngineUnknown:
			loads = "unknown"
		default:
			loads = "not here"
		}
		b.Tiers = append(b.Tiers, TierVerdict{Tier: t.name, Loads: loads, DetectionLabel: detectionLabel(st),
			CanFire: canFire(st), Unverified: st == stateUnverified})
		if in || engine == EngineUnknown {
			rowChans, rowNames = append(rowChans, t.ch), append(rowNames, t.name)
		}
	}
	b.Detection = termCoverages(r.If, src, rowChans, rowNames, false)
	labelBoundary(&b, anyOf(definite))
	return b
}

func canFire(s detectionState) bool { return s != stateInert && s != stateUnverified }

// detectionLabel is a detection state's closed-vocabulary name.
func detectionLabel(s detectionState) string {
	switch s {
	case stateUnverified:
		return "unverified (producers not visible here)"
	case stateInert:
		return "inert (cannot fire)"
	case stateLimited:
		return "coverage-limited (precautionary)"
	case stateVacuous:
		return "vacuous (no detection involved)"
	default:
		return "enumerable (declared gaps)"
	}
}

// labelBoundary fills the detection label, the fire flags and the one-line label.
func labelBoundary(b *RuleBoundary, state detectionState) {
	b.CanFire = canFire(state)
	b.Unverified = state == stateUnverified
	b.DetectionLabel = detectionLabel(state)
	reach := b.Reach
	switch state {
	case stateUnverified:
		b.Label = "UNVERIFIED — a term reads state this surface cannot see; firing is neither claimed nor ruled out (" + reach + ")"
	case stateInert:
		if len(b.Tiers) == 0 { // harvest and dry-run: one channel, today's wording
			b.Label = "INERT — a required term has no producer (detector or declared state producer); this rule can never fire (" + reach + ")"
		} else {
			b.Label = "INERT — no tier that loads this rule has a producer for every required term; this rule can never fire (" + reach + ")"
		}
	case stateLimited:
		b.Label = "PRECAUTIONARY — " + actionVerb(b.Mode) + "; detection has unknowable gaps, completeness is NOT claimed ∧ " + reach
	case stateVacuous:
		b.Label = "VACUOUS — satisfied by absence alone; fires unconditionally, no detector coverage backs it ∧ " + reach
	default: // enumerable
		b.Label = "DECLARED-GAPS — every term backed by enumerable producers (gaps listed per term) ∧ " + reach
	}
}

// producerSources is what a term's producers are looked up in: the detector set, the
// declared state producers, and the channel the rule is evaluated on.
type producerSources struct {
	dets  []Detector
	state StateProducers
	ch    producerChannel
}

// producersFor lists what can emit a term's fact on a channel: the hook's evaluation-only
// command/tool producers, detectors, then declared state producers.
//
// The raw command and the exact tool identity reach every live tier
// (engine.InvocationTags), and not the harvest channel. Both accept any value: their
// domain is open.
//
// A state term names a detector tag under its fold's namespace: session:K and target:K
// are produced by K's detectors, restricted to those whose facts reach that fold on the
// channel (see producerChannel). A destination detector emits destination-class with a
// dynamic value — but only the two values destClass can actually return; anything else
// (an authoring typo) has no producer. A Matches term keeps a producer only when its
// pattern accepts a value that producer can emit. Detectors that can never fire (a
// pattern with no regex, a content detector with no keywords) are not producers.
//
// A declared state producer matches a session: or agent: term by its full key, on the
// channels it declares. Declarations never produce target: terms.
func producersFor(term Predicate, src producerSources) []producer {
	var out []producer
	// engine.InvocationTags reaches every live tier (engine.ActionTags); the audit and
	// `tags` dry-runs judge no single invocation and add neither tag.
	hasInvocationTags := src.ch != channelHarvest
	switch {
	case term.Tag == CommandTagKey && hasInvocationTags:
		out = append(out, producer{id: evalOnlyCommandProducer})
	case term.Tag == ToolTagKey && hasInvocationTags:
		out = append(out, producer{id: evalOnlyToolProducer, enumerable: true,
			gaps: []string{"parsed invocation has no tool identity"}})
	}
	for _, d := range detectorProducers(term, src.dets, src.ch) {
		out = append(out, producer{id: d.ID, enumerable: d.Coverage.Enumerable, gaps: d.Coverage.Gaps})
	}
	if (src.ch != channelStateful && src.ch != channelHarvest) ||
		(!strings.HasPrefix(term.Tag, SessionStatePrefix) && !strings.HasPrefix(term.Tag, AgentStatePrefix)) {
		return out
	}
	for _, p := range src.state.Producers {
		if p.Tag != term.Tag || (src.ch == channelStateful && !p.Live) || (src.ch == channelHarvest && !p.Harvest) {
			continue
		}
		for _, v := range p.Values {
			if termAccepts(term, v) {
				out = append(out, producer{id: p.Source, enumerable: p.Enumerable, gaps: p.Gaps})
				break
			}
		}
	}
	return out
}

// unknownProducers reports why a producer-less term's namespace is not known to this
// caller on its channel, or "" when the term's producers are fully known. Only the
// stateful tier reads daemon facts and model claims; no other channel holds them.
func unknownProducers(term Predicate, src producerSources) string {
	if src.ch != channelStateful {
		return ""
	}
	switch {
	case strings.HasPrefix(term.Tag, AgentStatePrefix) && !src.state.AgentClaimsKnown:
		if src.state.AgentClaimsNote != "" {
			return "model-claim producers unknown: " + src.state.AgentClaimsNote
		}
		return "model-claim producers unknown: the binding set was not read"
	case strings.HasPrefix(term.Tag, SessionStatePrefix) && !src.state.SessionFactsKnown:
		return "daemon session-fact producers unknown: the catalog was not supplied"
	}
	return ""
}

// detectorProducers lists the detectors able to emit a term's fact on a channel.
func detectorProducers(term Predicate, dets []Detector, ch producerChannel) []Detector {
	if (ch == channelEngine || ch == channelStandalone) && isStateTerm(term.Tag) {
		return nil
	}
	key, targetOnly := term.Tag, false
	switch {
	case strings.HasPrefix(key, SessionStatePrefix):
		key = strings.TrimPrefix(key, SessionStatePrefix)
	case strings.HasPrefix(key, TargetStatePrefix):
		if ch == channelHarvest {
			return nil
		}
		key, targetOnly = strings.TrimPrefix(key, TargetStatePrefix), true
	}
	var out []Detector
	// valueOf reads a detector's emitted value the way Classify emits it and Match
	// compares it: a data-class value through its ladder spelling.
	valueOf := func(v string) string { return v }
	if IsDataClassKey(key) {
		term.Value, valueOf = CanonicalDataClass(term.Value), CanonicalDataClass
	}
	for _, d := range dets {
		if ch != channelHarvest && len(d.Roles) > 0 && !roleMatches(d.Roles, LiveEventRole) {
			continue
		}
		if targetOnly && d.Scope != "resource" {
			continue
		}
		switch d.Kind {
		case "destination":
			if key == "destination-class" && (termAccepts(term, "in-house") || termAccepts(term, "external")) {
				out = append(out, d)
			}
		case "pattern":
			if d.Regex != "" && d.Tag.Key == key && termAccepts(term, valueOf(d.Tag.Value)) {
				out = append(out, d)
			}
		case "content":
			if len(d.Keywords) > 0 && d.Tag.Key == key && termAccepts(term, valueOf(d.Tag.Value)) {
				out = append(out, d)
			}
		default:
			if d.Tag.Key == key && termAccepts(term, valueOf(d.Tag.Value)) {
				out = append(out, d)
			}
		}
	}
	return out
}

// termAccepts reports whether a term's value constraint admits a value a producer can
// emit. It is termMatches' value half, so the label and the evaluator agree.
func termAccepts(term Predicate, value string) bool {
	if term.Matches != "" {
		re := compiledRE(term.Matches)
		return re != nil && re.MatchString(value)
	}
	return term.Value == "" || term.Value == value
}

const (
	// evalOnlyCommandProducer names the non-detector source of raw command policy
	// input, so command rules are not mislabeled as produced by nothing.
	evalOnlyCommandProducer = "(hook: raw command text, evaluation-only)"
	// evalOnlyToolProducer names the canonical exact tool identity supplied by every
	// parsed invocation that names a tool. It is not a duplicate stored tag.
	evalOnlyToolProducer = "(hook: exact bare tool identity, evaluation-only)"
)

// termCoverage is one term's row over the channels of the labeled tiers: producers are
// merged across them, and names lists the tiers (by the channels' order) in which the
// term has a producer. The row is undetectable only when no labeled tier produces it.
func termCoverage(p Predicate, src producerSources, chans []producerChannel, names []string, negated bool) TermCoverage {
	tc := TermCoverage{Tag: p.Tag, Value: p.Value, Negated: negated, Detectors: []string{}}
	var prods []producer
	seen := map[string]bool{}
	unknown, readsState := "", false
	for i, ch := range chans {
		s := src
		s.ch = ch
		ps := producersFor(p, s)
		if len(ps) > 0 && names != nil {
			tc.Tiers = append(tc.Tiers, names[i])
		}
		for _, d := range ps {
			if !seen[d.id] {
				seen[d.id] = true
				prods = append(prods, d)
			}
		}
		if len(ps) == 0 && unknown == "" {
			unknown = unknownProducers(p, s)
		}
		readsState = readsState || ch == channelStateful || ch == channelHarvest
	}
	if len(prods) == 0 {
		if unknown != "" {
			tc.Unverified, tc.Note = true, unknown
			return tc
		}
		tc.Undetectable = true
		switch {
		case !readsState && isStateTerm(p.Tag):
			tc.Note = staticStateNote
		case len(chans) == 1 && chans[0] == channelHarvest && UnknownAtHarvest(src.state)(p):
			tc.Note = harvestBlindNote
		}
		return tc
	}
	// The term is missed only when EVERY producer misses. If at least one
	// producer is enumerable, misses of unknowable producers only ADD hits —
	// but the rule's intent spans the union, so one unknowable-only term
	// keeps completeness unclaimable. Per-term: enumerable iff at least one
	// enumerable producer exists; gaps = the enumerable producers' declared
	// lists (the only listable part).
	gapSet, limitSet := map[string]bool{}, map[string]bool{}
	for _, d := range prods {
		tc.Detectors = append(tc.Detectors, d.id)
		if d.enumerable {
			tc.Enumerable = true
			for _, g := range d.gaps {
				gapSet[g] = true
			}
			continue
		}
		for _, g := range d.gaps {
			limitSet[g] = true
		}
	}
	sort.Strings(tc.Detectors)
	for g := range gapSet {
		tc.Gaps = append(tc.Gaps, g)
	}
	sort.Strings(tc.Gaps)
	for g := range limitSet {
		tc.Limits = append(tc.Limits, g)
	}
	sort.Strings(tc.Limits)
	return tc
}

func termCoverages(p Predicate, src producerSources, chans []producerChannel, names []string, negated bool) []TermCoverage {
	switch {
	case len(p.All) > 0:
		var out []TermCoverage
		for _, c := range p.All {
			out = append(out, termCoverages(c, src, chans, names, negated)...)
		}
		return out
	case len(p.Any) > 0:
		var out []TermCoverage
		for _, c := range p.Any {
			out = append(out, termCoverages(c, src, chans, names, negated)...)
		}
		return out
	case p.Not != nil:
		return termCoverages(*p.Not, src, chans, names, !negated)
	case p.isTerm():
		return []TermCoverage{termCoverage(p, src, chans, names, negated)}
	}
	return nil
}

// predicateState folds the tree to one detection state on src's channel, evaluating in
// negation normal form: under an odd number of enclosing Nots, All folds as
// a disjunction and Any as a conjunction (De Morgan — red-team finding; the
// leaf polarity flip alone was measurably wrong in both directions).
//   - term: no producers → inert (positive) / vacuous (negated: the absence
//     is trivially, permanently true) — except at harvest reach for a term the dry
//     run cannot read (UnknownAtHarvest): the evaluator leaves it undecided, so it is
//     inert in both polarities; unknowable-only producers → limited;
//     else enumerable. A negated detectable term inherits its producers'
//     state: a missed detection flips the NOT to a false "true", so
//     unknowable gaps limit confidence in either direction. The hook's
//     evaluation-only command/tool producers are producers like any other
//     (producersFor): raw command text is unknowable, exact tool identity
//     enumerable.
//   - a term whose producers the caller cannot see (unknownProducers): positive →
//     unverified; negated → limited, because the NOT fires either way (always, when
//     nothing produces it; in every session without the fact, when something does).
//   - empty predicate {} matches nothing (Match returns false) → inert;
//     negated it matches everything → vacuous.
func predicateState(p Predicate, src producerSources, negated bool) detectionState {
	switch {
	case len(p.All) > 0:
		return foldChildren(p.All, src, negated, !negated)
	case len(p.Any) > 0:
		return foldChildren(p.Any, src, negated, negated)
	case p.Not != nil:
		return predicateState(*p.Not, src, !negated)
	case p.isTerm():
		prods := producersFor(p, src)
		if len(prods) == 0 && src.ch == channelHarvest && UnknownAtHarvest(src.state)(p) {
			// The dry run cannot read this fact: the evaluator leaves the term
			// undecided, so it can never make the rule fire, in either polarity.
			return stateInert
		}
		if len(prods) == 0 {
			if unknownProducers(p, src) != "" {
				if negated {
					return stateLimited
				}
				return stateUnverified
			}
			if negated {
				return stateVacuous
			}
			return stateInert
		}
		for _, d := range prods {
			if d.enumerable {
				return stateEnumerable
			}
		}
		return stateLimited
	}
	// the zero predicate: unsatisfiable positive, tautological negated
	if negated {
		return stateVacuous
	}
	return stateInert
}

// foldChildren combines child states. Conjunctive: every child must hold —
// one inert child kills the whole; an unverified child leaves the whole unverified;
// vacuous children are neutral. Disjunctive: see anyOf.
func foldChildren(children []Predicate, src producerSources, negated, conjunctive bool) detectionState {
	if !conjunctive {
		states := make([]detectionState, 0, len(children))
		for _, c := range children {
			states = append(states, predicateState(c, src, negated))
		}
		return anyOf(states)
	}
	unverified := false
	state := stateVacuous
	for _, c := range children {
		s := predicateState(c, src, negated)
		switch {
		case s == stateInert:
			return stateInert
		case s == stateUnverified:
			unverified = true
		case s < state:
			state = s
		}
	}
	if unverified {
		return stateUnverified
	}
	return state
}

// anyOf is the disjunctive fold — of an any: node's children, and of the tiers that
// load one rule: any one suffices. Inert states are dead branches (ignored unless all
// are dead), a vacuous state makes the whole fire unconditionally, unverified states
// decide only when nothing can fire, and otherwise the weakest firing state governs
// (the rule's intent spans the union). No state at all is inert.
func anyOf(states []detectionState) detectionState {
	unverified := false
	state := stateInert
	for _, s := range states {
		switch {
		case s == stateVacuous:
			return stateVacuous
		case s == stateInert:
			continue
		case s == stateUnverified:
			unverified = true
		case state == stateInert || s < state:
			state = s
		}
	}
	if state == stateInert && unverified {
		return stateUnverified
	}
	return state
}
