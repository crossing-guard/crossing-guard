package engine

// The rule compiler's honest label (P-COMPILE-2; ADR 0006 + 0015):
//
//	boundary(rule) = detection-coverage(its predicate's terms × the detector
//	                 set) ∧ enforcement-reach(the caller's channel)
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
//	"inert (cannot fire)"             a required term has no producing
//	                                  detector at all
//
// "confirmed-complete" is deliberately not in the vocabulary: no computation
// can produce it, so no surface can render it (the §9 no-bluffing invariant).

import "sort"

// Canonical reach strings (design §8) — callers pass the one matching their
// channel; anything else is passed through verbatim.
const (
	ReachStop    = "live hook — STOP reach (deny/ask before execution)"
	ReachHarvest = "harvest — post-hoc detect/report only, never a live block"
	ReachDryRun  = "dry-run — no enforcement"
)

// TermCoverage reports how one predicate leaf is detectable.
type TermCoverage struct {
	Tag          string   `json:"tag"`
	Value        string   `json:"value,omitempty"`
	Negated      bool     `json:"negated,omitempty"`
	Detectors    []string `json:"detectors"` // detectors able to emit this term's tag
	Enumerable   bool     `json:"enumerable"`
	Gaps         []string `json:"gaps,omitempty"`
	Undetectable bool     `json:"undetectable,omitempty"` // no detector produces it
}

// RuleBoundary is one rule's computed honest label.
type RuleBoundary struct {
	Rule           string         `json:"rule"`
	Mode           Mode           `json:"mode"`
	Detection      []TermCoverage `json:"detection"`
	DetectionLabel string         `json:"detection_label"`
	CanFire        bool           `json:"can_fire"`
	Reach          string         `json:"reach"`
	Label          string         `json:"label"` // detection ∧ reach, one line
}

// detectionState is a rule's detectability, ordered worst-first for combination.
type detectionState int

const (
	stateInert detectionState = iota
	stateLimited
	stateEnumerable
	stateVacuous // a NOT over a tag nothing produces: always true, trivially covered
)

// Boundary computes one rule's honest label against a detector set and the
// caller's channel reach.
func Boundary(r Rule, dets []Detector, reach string) RuleBoundary {
	terms := termCoverages(r.If, dets, false)
	state := predicateState(r.If, dets, false)
	b := RuleBoundary{Rule: r.ID, Mode: r.Mode, Detection: terms, Reach: reach,
		CanFire: state != stateInert}
	switch state {
	case stateInert:
		b.DetectionLabel = "inert (cannot fire)"
		b.Label = "INERT — a required term has no producing detector; this rule can never fire (" + reach + ")"
	case stateLimited:
		b.DetectionLabel = "coverage-limited (precautionary)"
		b.Label = "PRECAUTIONARY — blocks on a matched detector; detection has unknowable gaps, completeness is NOT claimed ∧ " + reach
	case stateVacuous:
		b.DetectionLabel = "vacuous (no detection involved)"
		b.Label = "VACUOUS — satisfied by absence alone; fires unconditionally, no detector coverage backs it ∧ " + reach
	default: // enumerable
		b.DetectionLabel = "enumerable (declared gaps)"
		b.Label = "DECLARED-GAPS — every term backed by enumerable producers (gaps listed per term) ∧ " + reach
	}
	return b
}

// CompileBoundaries labels every rule in a policy.
func CompileBoundaries(pol *Policy, dets []Detector, reach string) []RuleBoundary {
	out := make([]RuleBoundary, 0, len(pol.Rules))
	for _, r := range pol.Rules {
		out = append(out, Boundary(r, dets, reach))
	}
	return out
}

// producersFor lists detectors able to emit a term's tag. A destination
// detector emits destination-class with a dynamic value — but only the two
// values destClass can actually return; anything else (an authoring typo) has
// no producer. Detectors that can never fire (a pattern with no regex, a
// content detector with no keywords) are not producers.
func producersFor(tag, value string, dets []Detector) []Detector {
	var out []Detector
	for _, d := range dets {
		switch d.Kind {
		case "destination":
			if tag == "destination-class" && (value == "" || value == "in-house" || value == "external") {
				out = append(out, d)
			}
		case "pattern":
			if d.Regex != "" && d.Tag.Key == tag && (value == "" || d.Tag.Value == value) {
				out = append(out, d)
			}
		case "content":
			if len(d.Keywords) > 0 && d.Tag.Key == tag && (value == "" || d.Tag.Value == value) {
				out = append(out, d)
			}
		default:
			if d.Tag.Key == tag && (value == "" || d.Tag.Value == value) {
				out = append(out, d)
			}
		}
	}
	return out
}

const (
	// evalOnlyCommandProducer names the non-detector source of raw command policy
	// input, so command rules are not mislabeled as produced by nothing.
	evalOnlyCommandProducer = "(hook: raw command text, evaluation-only)"
	// evalOnlyToolProducer names the canonical exact tool identity supplied by every
	// parsed invocation that names a tool. It is not a duplicate stored tag.
	evalOnlyToolProducer = "(hook: exact bare tool identity, evaluation-only)"
)

func termCoverage(p Predicate, dets []Detector, negated bool) TermCoverage {
	tc := TermCoverage{Tag: p.Tag, Value: p.Value, Negated: negated, Detectors: []string{}}
	prods := producersFor(p.Tag, p.Value, dets)
	// Evaluation-only command and exact-tool tags have hook producers no detector list
	// contains. Counting them as "no producer" would make live standalone rules appear
	// INERT even while the hook enforces them. A false INERT is worse than no label.
	//
	// The hook-producer is APPENDED to any real producers rather than replacing
	// them: a user overlay may legitimately declare a detector emitting the same key,
	// and short-circuiting here would hide it from the one surface whose job is
	// honest coverage. The appended producer contributes no Enumerable claim —
	// regex over free command text can never claim completeness.
	switch p.Tag {
	case CommandTagKey:
		tc.Detectors = append(tc.Detectors, evalOnlyCommandProducer)
	case ToolTagKey:
		tc.Detectors = append(tc.Detectors, evalOnlyToolProducer)
		tc.Enumerable = true
		tc.Gaps = append(tc.Gaps, "parsed invocation has no tool identity")
	default:
		if len(prods) != 0 {
			break
		}
		tc.Undetectable = true
		return tc
	}
	// The term is missed only when EVERY producer misses. If at least one
	// producer is enumerable, misses of unknowable producers only ADD hits —
	// but the rule's intent spans the union, so one unknowable-only term
	// keeps completeness unclaimable. Per-term: enumerable iff at least one
	// enumerable producer exists; gaps = the enumerable producers' declared
	// lists (the only listable part).
	gapSet := map[string]bool{}
	for _, d := range prods {
		tc.Detectors = append(tc.Detectors, d.ID)
		if d.Coverage.Enumerable {
			tc.Enumerable = true
			for _, g := range d.Coverage.Gaps {
				gapSet[g] = true
			}
		}
	}
	sort.Strings(tc.Detectors)
	for g := range gapSet {
		tc.Gaps = append(tc.Gaps, g)
	}
	sort.Strings(tc.Gaps)
	return tc
}

func termCoverages(p Predicate, dets []Detector, negated bool) []TermCoverage {
	switch {
	case len(p.All) > 0:
		var out []TermCoverage
		for _, c := range p.All {
			out = append(out, termCoverages(c, dets, negated)...)
		}
		return out
	case len(p.Any) > 0:
		var out []TermCoverage
		for _, c := range p.Any {
			out = append(out, termCoverages(c, dets, negated)...)
		}
		return out
	case p.Not != nil:
		return termCoverages(*p.Not, dets, !negated)
	case p.isTerm():
		return []TermCoverage{termCoverage(p, dets, negated)}
	}
	return nil
}

// predicateState folds the tree to one detection state, evaluating in
// negation normal form: under an odd number of enclosing Nots, All folds as
// a disjunction and Any as a conjunction (De Morgan — red-team finding; the
// leaf polarity flip alone was measurably wrong in both directions).
//   - term: no producers → inert (positive) / vacuous (negated: the absence
//     is trivially, permanently true); unknowable-only producers → limited;
//     else enumerable. A negated detectable term inherits its producers'
//     state: a missed detection flips the NOT to a false "true", so
//     unknowable gaps limit confidence in either direction.
//   - empty predicate {} matches nothing (Match returns false) → inert;
//     negated it matches everything → vacuous.
func predicateState(p Predicate, dets []Detector, negated bool) detectionState {
	switch {
	case len(p.All) > 0:
		return foldChildren(p.All, dets, negated, !negated)
	case len(p.Any) > 0:
		return foldChildren(p.Any, dets, negated, negated)
	case p.Not != nil:
		return predicateState(*p.Not, dets, !negated)
	case p.isTerm():
		prods := producersFor(p.Tag, p.Value, dets)
		if len(prods) == 0 {
			// Evaluation-only terms are never inert: the hook itself produces them at
			// decision time. Raw command text remains limited in both polarities because
			// matching free text has unknowable gaps. Exact tool identity is enumerable;
			// its one declared gap is a parsed invocation with no tool name.
			switch p.Tag {
			case CommandTagKey:
				return stateLimited
			case ToolTagKey:
				return stateEnumerable
			}
			if negated {
				return stateVacuous
			}
			return stateInert
		}
		for _, d := range prods {
			if d.Coverage.Enumerable {
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
// one inert child kills the whole; vacuous children are neutral. Disjunctive:
// any child suffices — inert children are dead branches (ignored unless all
// are dead), and a vacuous child makes the whole disjunction fire
// unconditionally.
func foldChildren(children []Predicate, dets []Detector, negated, conjunctive bool) detectionState {
	if conjunctive {
		state := stateVacuous
		for _, c := range children {
			s := predicateState(c, dets, negated)
			if s == stateInert {
				return stateInert
			}
			if s < state {
				state = s
			}
		}
		return state
	}
	state := stateInert
	for _, c := range children {
		s := predicateState(c, dets, negated)
		if s == stateVacuous {
			return stateVacuous
		}
		if s == stateInert {
			continue
		}
		if state == stateInert || s < state {
			state = s
		}
	}
	return state
}
