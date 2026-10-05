package engine

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// DataClassKey is the tag key the water-mark ladder ranks.
const DataClassKey = "data-class"

// waterOrder is the monotonic data-class ladder; the water mark is max over it.
var waterOrder = []string{"public", "internal", "source-code", "customer-data",
	"regulated", "personal-data", "credential-material"}

// dataClassAliases maps a legacy data-class spelling to its rung on the ladder. The
// shipped email detector emitted "personal" (a copy slip in f7f9e0d) while every other
// owner spelled the class "personal-data"; hash-chained events keep the old spelling
// forever, so it is read through this map rather than rewritten.
// It is bound to the ladder: if the
// ladder ever becomes configuration, these move with it.
var dataClassAliases = map[string]string{"personal": "personal-data"}

// WaterOrder returns a copy of the ladder, lowest rung first.
func WaterOrder() []string { return append([]string(nil), waterOrder...) }

// CanonicalDataClass returns the ladder spelling of a data-class value; a value that is
// not a known alias is returned unchanged.
func CanonicalDataClass(value string) string {
	if canonical, ok := dataClassAliases[value]; ok {
		return canonical
	}
	return value
}

// IsDataClassKey reports whether a tag key carries a data-class value: the bare key or
// a namespaced copy of it (session:data-class, target:data-class).
func IsDataClassKey(key string) bool {
	return key == DataClassKey || strings.HasSuffix(key, ":"+DataClassKey)
}

// DataClassAlias is one legacy spelling and its canonical rung, under Key.
type DataClassAlias struct{ Key, From, To string }

// DataClassAliases lists the legacy spellings, for callers that repair stored folds.
func DataClassAliases() []DataClassAlias {
	out := make([]DataClassAlias, 0, len(dataClassAliases))
	for from, to := range dataClassAliases {
		out = append(out, DataClassAlias{Key: DataClassKey, From: from, To: to})
	}
	return out
}

// WaterMark returns the highest data-class among the tags, or "" (a WEAK negative —
// absence is not a clean bill; design §4). A projection of the ledger, not a store.
func WaterMark(tags []Tag) string {
	best := -1
	for _, t := range tags {
		if t.Key != DataClassKey {
			continue
		}
		value := CanonicalDataClass(t.Value)
		for i, v := range waterOrder {
			if v == value && i > best {
				best = i
			}
		}
	}
	if best < 0 {
		return ""
	}
	return waterOrder[best]
}

// Predicate is a compound AND/OR/NOT tree over tag TERMS. Exactly one field is set.
// A term is {Tag, Value?|Matches?} — Value is an exact match, Matches is a regex over
// the tag's value; both omitted matches any value of that key. Matches is what lets a
// converted format-2 regex guard (`{"tag":"command","matches":"<pattern>"}`) live in
// the one predicate language (ADR 0025 §4) — but it needs a `command` tag in the set
// to match against, which the evaluator injects (see WithCommandTag).
type Predicate struct {
	All     []Predicate `json:"all,omitempty"`
	Any     []Predicate `json:"any,omitempty"`
	Not     *Predicate  `json:"not,omitempty"`
	Tag     string      `json:"tag,omitempty"`
	Value   string      `json:"value,omitempty"`
	Matches string      `json:"matches,omitempty"` // regex over the tag's value
}

func (p Predicate) isTerm() bool { return p.Tag != "" }

// reCache compiles each pattern once. Rules are few and evaluated per hook process,
// but a term can be checked against many tags in one Match, so compiling in the loop
// would be wasteful and — worse — hide a bad pattern until a tag happened to reach it.
var reCache sync.Map // pattern string -> *regexp.Regexp (or nil if it failed to compile)

func compiledRE(pattern string) *regexp.Regexp {
	if v, ok := reCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil // cache the failure so we don't recompile a known-bad pattern
	}
	reCache.Store(pattern, re)
	return re
}

// termMatches reports whether one tag satisfies a leaf term. A Matches term with an
// UNCOMPILABLE pattern matches nothing — never everything — so a bad regex fails safe
// (the rule silently never fires rather than firing on all input). CompilePredicates
// at load time is what surfaces that bad pattern loudly instead.
func termMatches(term Predicate, t Tag) bool {
	if t.Key != term.Tag {
		return false
	}
	if term.Matches != "" {
		re := compiledRE(term.Matches)
		return re != nil && re.MatchString(t.Value)
	}
	if term.Value == "" || t.Value == term.Value {
		return true
	}
	// A data-class term and fact match across the legacy spelling, so a rule written
	// either way keeps matching a fact recorded either way.
	return IsDataClassKey(t.Key) && CanonicalDataClass(t.Value) == CanonicalDataClass(term.Value)
}

// validActions is the closed authoring vocabulary. An action outside it is NOT a
// silent proceed — a rule authored `"action":"block"` (a plausible synonym) or a typo
// `"dney"` would otherwise map to SilentLog and never enforce, the exact silent-stop-
// enforcing failure ADR 0025 exists to prevent. Empty is allowed: it means "no action
// authored", and a rule may instead carry a direct Mode.
var validActions = map[string]bool{
	"": true, "deny": true, "ask": true, "allow": true, "observe": true, "redact": true,
}

// CompilePredicates walks a policy's rules and rejects, at load / validate time: an
// invalid `matches` regex (which would silently never fire) and an unknown `action`
// (which would silently never enforce). This is how ValidateRules gets its format-1
// validation, and why a bad rule is a loud error rather than a dead rule.
func CompilePredicates(pol *Policy) error {
	for _, r := range pol.Rules {
		if !validActions[r.Action] {
			return &BadActionError{RuleID: r.ID, Action: r.Action}
		}
		if err := compilePredicate(r.If, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// BadActionError names the rule and the unrecognized action, so a rejection points at
// what to fix instead of silently disabling enforcement.
type BadActionError struct {
	RuleID string
	Action string
}

func (e *BadActionError) Error() string {
	return "rule " + e.RuleID + ": unknown action " + e.Action +
		" (must be one of deny, ask, allow, observe, redact)"
}

func compilePredicate(p Predicate, ruleID string) error {
	return compileNode(p, ruleID, true)
}

// compileNode rejects the node shapes the evaluator would read differently from the
// author: Judge evaluates the first of all/any/not/tag and ignores the rest, an empty
// node under `not` matches everything, and a value or pattern with no tag constrains
// nothing. An empty ROOT stays legal: it never fires, and legacy migration writes it.
func compileNode(p Predicate, ruleID string, root bool) error {
	var shapes []string
	if len(p.All) > 0 {
		shapes = append(shapes, "all")
	}
	if len(p.Any) > 0 {
		shapes = append(shapes, "any")
	}
	if p.Not != nil {
		shapes = append(shapes, "not")
	}
	if p.isTerm() {
		shapes = append(shapes, "tag")
	}
	switch {
	case len(shapes) > 1:
		return &PredicateShapeError{RuleID: ruleID, Problem: "a predicate node sets " +
			strings.Join(shapes, " and ") + "; a node takes exactly one of all, any, not, tag (nest the others under all)"}
	case len(shapes) == 0 && !root:
		return &PredicateShapeError{RuleID: ruleID, Problem: "an empty predicate node (under not it would match everything); a node takes exactly one of all, any, not, tag"}
	case !p.isTerm() && (p.Value != "" || p.Matches != ""):
		return &PredicateShapeError{RuleID: ruleID, Problem: "value or matches on a node with no tag"}
	case p.Value != "" && p.Matches != "":
		return &PredicateShapeError{RuleID: ruleID, Problem: "term " + p.Tag + " sets both value and matches; a term takes one"}
	}
	for _, c := range p.All {
		if err := compileNode(c, ruleID, false); err != nil {
			return err
		}
	}
	for _, c := range p.Any {
		if err := compileNode(c, ruleID, false); err != nil {
			return err
		}
	}
	if p.Not != nil {
		if err := compileNode(*p.Not, ruleID, false); err != nil {
			return err
		}
	}
	if p.isTerm() && p.Matches != "" {
		if _, err := regexp.Compile(p.Matches); err != nil {
			return &BadPatternError{RuleID: ruleID, Pattern: p.Matches, Err: err}
		}
	}
	return nil
}

// PredicateShapeError names the rule and the malformed node, so a rejection points at
// what to fix instead of a rule that silently means something else.
type PredicateShapeError struct {
	RuleID  string
	Problem string
}

func (e *PredicateShapeError) Error() string { return "rule " + e.RuleID + ": " + e.Problem }

// BadPatternError names the rule and pattern, so a rejection points at what to fix.
type BadPatternError struct {
	RuleID  string
	Pattern string
	Err     error
}

func (e *BadPatternError) Error() string {
	return "rule " + e.RuleID + ": bad regex " + e.Pattern + ": " + e.Err.Error()
}
func (e *BadPatternError) Unwrap() error { return e.Err }

const (
	// CommandTagKey carries raw command text at decision time, so a converted regex
	// guard has something to match. It is evaluation-only and is never folded into
	// stored state.
	CommandTagKey = "command"
	// ToolTagKey carries the exact bare tool identity at decision time. Canonical
	// storage remains event.tool; this evaluation-only projection is not frozen as a
	// second copy of the same fact.
	ToolTagKey = "tool"
)

// LiveEventRole is the role every live classification stamps on the current action:
// the hook's static tier, the daemon's observe/decide path and the ledger. The coverage
// compiler reads it to know which detectors produce on the live channel.
const LiveEventRole = "tool_call"

// The state namespaces (ADR 0025 §3). A stateful rule names a folded fact as
// `session:<key>` (the session's accumulated state), `target:<key>` (the state of the
// resource the current action touches) or `agent:<binding>:<tag>` (a model-claimed tag).
// They are defined once here so the daemon's tag builders, the stateful-rule selector
// and the coverage compiler cannot drift apart.
const (
	SessionStatePrefix = "session:"
	TargetStatePrefix  = "target:"
	AgentStatePrefix   = "agent:"
)

// IsStateTag reports whether a term names folded or claimed state rather than a fact of
// the current action — the terms only the daemon's stateful tier can evaluate.
func IsStateTag(tag string) bool {
	return strings.HasPrefix(tag, SessionStatePrefix) || strings.HasPrefix(tag, TargetStatePrefix) ||
		strings.HasPrefix(tag, AgentStatePrefix)
}

// ReferencesState reports whether any term in a predicate tree is a state term.
func ReferencesState(p Predicate) bool { return ReferencesKey(p, IsStateTag) }

// RouteFactPrefix namespaces the facts of a named model route (team rest-of-release plan
// §5.4, OD-25). `route:` is a THIRD key class beside action facts and state: its facts
// exist only where a route is bound to a place or a run starts on one, so neither hook
// tier ever holds them. A rule that reads one is evaluated only by the route admission
// evaluator (internal/modelroute.Admit); StaticTier and the daemon's stateful selector
// both exclude it, because over a tag set with no route: key a negated route: term is
// vacuously true and would fire on every tool call.
const RouteFactPrefix = "route:"

// The two facts admission supplies beside the route's own (plan §5.4's table): the
// profile's declared locality and the place's project root. They are admission facts,
// not a key class: a rule is a route rule only when it reads a route: key.
const (
	ProfileLocalityFactKey = "profile:locality"
	ProjectRootFactKey     = "project:root"
)

// IsRouteKey reports whether a term names a fact of a named model route.
func IsRouteKey(key string) bool { return strings.HasPrefix(key, RouteFactPrefix) }

// ReferencesRoute reports whether any term in a predicate tree reads a route: fact.
func ReferencesRoute(p Predicate) bool { return ReferencesKey(p, IsRouteKey) }

// IsAdmissionKey reports whether route admission supplies a fact under this key: the
// route's own facts and the two beside them. Admission answers nothing else.
func IsAdmissionKey(key string) bool {
	return IsRouteKey(key) || key == ProfileLocalityFactKey || key == ProjectRootFactKey
}

// MixedRouteRuleError names a rule that reads a route: fact beside a term route
// admission cannot answer (a command, a tool, a detector fact, or session:, target: or
// agent: state). No evaluation site holds both, so the rule could only ever fire by
// absence; it is refused when the document is written (OD-25).
type MixedRouteRuleError struct {
	RuleID string
	Key    string
}

func (e *MixedRouteRuleError) Error() string {
	return "rule " + e.RuleID + ": a rule that reads a route: fact may read only route:, " +
		ProfileLocalityFactKey + " and " + ProjectRootFactKey + " facts; it also reads " + e.Key
}

// ValidateRouteRules refuses every rule that mixes a route: term with a term route
// admission cannot answer. A rule with no route: term is not its business.
func ValidateRouteRules(p *Policy) error {
	if p == nil {
		return nil
	}
	for _, r := range p.Rules {
		if !ReferencesRoute(r.If) {
			continue
		}
		foreign := ""
		// AnyTerm walks every shape of a node, not only the one Judge evaluates.
		AnyTerm(r.If, func(term Predicate) bool {
			if IsAdmissionKey(term.Tag) {
				return false
			}
			foreign = term.Tag
			return true
		})
		if foreign != "" {
			return &MixedRouteRuleError{RuleID: r.ID, Key: foreign}
		}
	}
	return nil
}

// ReferencesKey reports whether any term in a predicate tree has a key is accepts.
func ReferencesKey(p Predicate, is func(key string) bool) bool {
	return AnyTerm(p, func(term Predicate) bool { return is(term.Tag) })
}

// AnyTerm reports whether any term in a predicate tree satisfies is. It walks every
// shape of a node, not only the one Judge evaluates.
func AnyTerm(p Predicate, is func(term Predicate) bool) bool {
	for _, c := range p.All {
		if AnyTerm(c, is) {
			return true
		}
	}
	for _, c := range p.Any {
		if AnyTerm(c, is) {
			return true
		}
	}
	if p.Not != nil && AnyTerm(*p.Not, is) {
		return true
	}
	return p.isTerm() && is(p)
}

// Truth is one of the evaluator's three answers.
type Truth int8

const (
	No        Truth = iota // definitely does not hold
	Undecided              // the site cannot tell: a term it has no way to answer decides it
	Yes                    // definitely holds
)

// Unknown reports the terms an evaluation site cannot answer: facts it never produces,
// whose absence from its tag set therefore says nothing. nil means it can answer all.
type Unknown func(term Predicate) bool

// AnyUnknown is the union of several sites' blind spots.
func AnyUnknown(us ...Unknown) Unknown {
	return func(term Predicate) bool {
		for _, u := range us {
			if u != nil && u(term) {
				return true
			}
		}
		return false
	}
}

// Match evaluates the predicate over a tag set (design §6 compound predicate) at a site
// that can answer every term.
func Match(p Predicate, tags []Tag) bool { return Judge(p, tags, nil) == Yes }

// Judge is the one evaluator. A term a tag satisfies is Yes whatever the site knows. A
// term nothing satisfies is Undecided when the site cannot answer it, else No. `all` is
// its weakest child, `any` its strongest, and `not` swaps Yes and No and keeps
// Undecided — so an absent fact the site never produces can neither fire a rule through
// a negation nor silently clear one.
//
// A node is read by its first shape in all → any → not → tag order; loaded documents
// carry exactly one (CompilePredicates).
func Judge(p Predicate, tags []Tag, unknown Unknown) Truth {
	switch {
	case len(p.All) > 0:
		out := Yes
		for _, c := range p.All {
			switch Judge(c, tags, unknown) {
			case No:
				return No
			case Undecided:
				out = Undecided
			}
		}
		return out
	case len(p.Any) > 0:
		out := No
		for _, c := range p.Any {
			switch Judge(c, tags, unknown) {
			case Yes:
				return Yes
			case Undecided:
				out = Undecided
			}
		}
		return out
	case p.Not != nil:
		switch Judge(*p.Not, tags, unknown) {
		case Yes:
			return No
		case No:
			return Yes
		}
		return Undecided
	case p.isTerm():
		for _, t := range tags {
			if termMatches(p, t) {
				return Yes
			}
		}
		if unknown != nil && unknown(p) {
			return Undecided
		}
		return No
	}
	return No
}

// Terms flattens a predicate to its leaf terms (for "which tags fired" explanation).
func Terms(p Predicate) []Predicate {
	switch {
	case len(p.All) > 0:
		var out []Predicate
		for _, c := range p.All {
			out = append(out, Terms(c)...)
		}
		return out
	case len(p.Any) > 0:
		var out []Predicate
		for _, c := range p.Any {
			out = append(out, Terms(c)...)
		}
		return out
	case p.Not != nil:
		return Terms(*p.Not)
	case p.isTerm():
		return []Predicate{p}
	}
	return nil
}

// FiredTags returns the tags that satisfied a predicate's terms (deduped).
func FiredTags(p Predicate, tags []Tag) []Tag {
	seen := map[string]bool{}
	var out []Tag
	for _, term := range Terms(p) {
		for _, t := range tags {
			if termMatches(term, t) {
				k := t.identityKey()
				if !seen[k] {
					seen[k] = true
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// --- policy: the severity ladder + resolution menu (design §7) ----------------------

// Mode is the per-rule severity ladder rung.
type Mode string

const (
	HardBlock        Mode = "hard-block"         // non-overridable (Restricted)
	ConfirmAndRecord Mode = "confirm-and-record" // override attributed+logged
	WarnAndProceed   Mode = "warn-and-proceed"
	SilentLog        Mode = "silent-log"
)

func (m Mode) rank() int {
	switch m {
	case HardBlock:
		return 0
	case ConfirmAndRecord:
		return 1
	case WarnAndProceed:
		return 2
	default:
		return 3
	}
}

// Gates reports whether a rule in this mode stops the action (HardBlock denies,
// ConfirmAndRecord asks). Every other mode — warn, silent, an unknown value — proceeds.
// Decide's "block" is its presentation; the hook's tiers and the daemon's stateful tier
// gate on it.
func (m Mode) Gates() bool {
	return m == HardBlock || m == ConfirmAndRecord
}

// FiredWarns returns every warn-and-proceed rule among d's fired rules, in policy
// order, each id once. Decision names only the top rule; a proceeding action records
// every rule that warned on it.
func FiredWarns(d Decision, pol *Policy) []string {
	if d.Decision != "warn" || pol == nil {
		return nil
	}
	var out []string
	for _, r := range pol.Rules {
		if r.effectiveMode() == WarnAndProceed && slices.Contains(d.AllFired, r.ID) &&
			!slices.Contains(out, r.ID) {
			out = append(out, r.ID)
		}
	}
	return out
}

// Layer is the distribution tier a rule arrived by (team plan §5.16, schema 38): the
// user's own selection, a repository-scoped bundle, or an organization bundle. The
// loader sets it on every rule at load time; Decide copies the winning rule's layer
// onto the Decision so the event, the chain hash, and the wire can each carry it.
// Empty means unknown — never guessed (the same honesty rule as Decision.Rule).
type Layer string

const (
	LayerUser         Layer = "user"
	LayerRepository   Layer = "repository"
	LayerOrganization Layer = "organization"
)

type Rule struct {
	ID string `json:"id"`
	// Action is the PREFERRED authored decision vocabulary (ADR 0025 §2a): the flat enum
	// deny/ask/allow/observe/redact that format 2, format 3, and the console all speak.
	// The engine maps it to Mode internally (effectiveMode), so the wired redact/route/
	// confirm menu survives one authored field. Mode is the ladder-native LEGACY path,
	// still honored when set directly and still authored by engine/testdata — so tooling
	// must not assume every rule carries Action. Collapsing to one is the eventual move.
	Action    string            `json:"action,omitempty"`
	Mode      Mode              `json:"mode,omitempty"`
	If        Predicate         `json:"if"`
	Flippable map[string]string `json:"flippable,omitempty"` // tag-key -> resolution (route/redact)
	Message   string            `json:"message,omitempty"`
	// Severity is report presentation metadata. It never changes effectiveMode or
	// Decide; action remains the only authored behavior vocabulary. Audit carries it
	// so migrated report-only rules do not need a second rule type.
	Severity string `json:"severity,omitempty"`
	// These are carried for Phase 3 / display and are UNREAD by Decide today (honest
	// forward-carry, not live behavior): Intent (plain-language statement, format 3),
	// and format 3's FailMode/Scope/Runtimes/Backstop (ADR 0025 §5).
	Intent   string   `json:"intent,omitempty"`
	FailMode string   `json:"fail_mode,omitempty"` // per-rule daemon-down behavior
	Scope    string   `json:"scope,omitempty"`
	Runtimes []string `json:"runtimes,omitempty"`
	Backstop string   `json:"backstop,omitempty"`
	// Layer is the distribution tier this rule arrived by (team plan §5.16). It is
	// JSON-`-` on the rule document — a layer is a fact about WHERE a rule came from,
	// not authored content, so it never serializes into a digest-computed document.
	// The layered loader assigns it at read time.
	Layer Layer `json:"-"`
}

// effectiveMode resolves the authored decision to the engine's severity ladder. A
// directly-authored Mode wins (ladder-native policies); otherwise Action maps in.
// An unmapped/absent decision is silent-log → proceed, matching the pre-0025 default
// for a rule that declared no severity.
func (r Rule) effectiveMode() Mode {
	if r.Mode != "" {
		return r.Mode
	}
	switch r.Action {
	case "deny":
		return HardBlock
	case "ask":
		return ConfirmAndRecord
	case "redact":
		return ConfirmAndRecord // overridable, and carries the redact menu
	default: // allow, observe, "" — proceed (allow-precedence is out of this conversion's scope)
		return SilentLog
	}
}

// Capabilities of the current channel — drives which resolutions are LIVE (design §8).
type Capabilities struct {
	Stop             bool `json:"stop"`
	RedactAvailable  bool `json:"redact_available"`
	GatewayAvailable bool `json:"gateway_available"`
}

type Policy struct {
	Capabilities Capabilities `json:"capabilities"`
	Rules        []Rule       `json:"rules"`
}

func LoadPolicy(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	// Reject a bad `matches` regex at load, not silently at decision time — a rule
	// that never fires is exactly the silent-stop-enforcing failure ADR 0025 warns of.
	if err := CompilePredicates(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Resolution is one menu item; Live=false means honestly greyed (channel can't do it).
type Resolution struct {
	Action string `json:"action"`
	Flips  string `json:"flips,omitempty"`
	Live   bool   `json:"live"`
	Why    string `json:"why"`
}

// Decision is the checkpoint output: what happened + why + the honest menu.
type Decision struct {
	Decision     string       `json:"decision"` // allow | warn | block
	Mode         Mode         `json:"mode,omitempty"`
	Rule         string       `json:"rule,omitempty"`
	Layer        Layer        `json:"layer,omitempty"` // the tier the winning rule arrived by; empty = unknown
	Message      string       `json:"message,omitempty"`
	FiredTags    []Tag        `json:"fired_tags,omitempty"`
	AllFired     []string     `json:"all_fired_rules,omitempty"`
	Menu         []Resolution `json:"menu,omitempty"`
	WeakNegative bool         `json:"weak_negative,omitempty"`
	ReachNote    string       `json:"reach_note,omitempty"`
}

// Decide evaluates policy over tags at a STOP-reach channel. Strongest-severity rule
// wins the action; the menu is keyed to flippable terms AND live capabilities.
func Decide(tags []Tag, pol *Policy) Decision {
	d, _ := DecideSeeing(tags, pol, nil)
	return d
}

// Seen is what one evaluation found beside its decision: the rules that fired, and the
// rules the site could not decide (Judge's Undecided). An undecided rule does not fire.
type Seen struct {
	Fired     []Rule
	Undecided []Rule
}

// GatingUndecided counts the undecided rules that could stop the action: the ones that
// can make another site's answer differ from this one.
func (s Seen) GatingUndecided() int {
	n := 0
	for _, r := range s.Undecided {
		if r.Gates() {
			n++
		}
	}
	return n
}

// Asking lists the fired confirm-class rules, first occurrence of each id, in order.
func (s Seen) Asking() []Rule {
	var out []Rule
	seen := map[string]bool{}
	for _, r := range s.Fired {
		if r.effectiveMode() == ConfirmAndRecord && !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}

// DecideSeeing is Decide at a site with blind spots: only a rule judged Yes fires.
func DecideSeeing(tags []Tag, pol *Policy, unknown Unknown) (Decision, Seen) {
	var seen Seen
	for _, r := range pol.Rules {
		switch Judge(r.If, tags, unknown) {
		case Yes:
			seen.Fired = append(seen.Fired, r)
		case Undecided:
			seen.Undecided = append(seen.Undecided, r)
		}
	}
	d := decideFired(seen.Fired, tags, pol)
	return d, seen
}

func decideFired(fired []Rule, tags []Tag, pol *Policy) Decision {
	if len(fired) == 0 {
		return Decision{Decision: "allow", WeakNegative: len(tags) == 0}
	}
	top := fired[0]
	for _, r := range fired[1:] {
		if r.effectiveMode().rank() < top.effectiveMode().rank() {
			top = r
		}
	}
	var all []string
	for _, r := range fired {
		all = append(all, r.ID)
	}
	topMode := top.effectiveMode()
	dec := "allow"
	reach := "proceeds, logged"
	switch {
	case topMode.Gates():
		dec = "block"
		reach = "HONEST block — STOP-reach channel (design §8); no downgrade"
	case topMode == WarnAndProceed:
		dec = "warn"
	}
	d := Decision{Decision: dec, Mode: topMode, Rule: top.ID, Layer: top.Layer, Message: top.Message,
		FiredTags: FiredTags(top.If, tags), AllFired: all, ReachNote: reach}
	if dec == "block" {
		d.Menu = menu(top, pol.Capabilities)
	}
	return d
}

func menu(r Rule, caps Capabilities) []Resolution {
	var items []Resolution
	for tag, kind := range r.Flippable {
		switch kind {
		case "redact":
			items = append(items, Resolution{Action: "redact", Flips: tag,
				Live: caps.RedactAvailable,
				Why: pick(caps.RedactAvailable, "sensitive part never enters; clean",
					"greyed — no output-redaction on this channel")})
		case "route":
			items = append(items, Resolution{Action: "route", Flips: tag,
				Live: caps.GatewayAvailable,
				Why: pick(caps.GatewayAvailable,
					"send to a compliant endpoint; genuine compliance, no debt",
					"greyed — we don't own a gateway on this channel")})
		}
	}
	switch r.effectiveMode() {
	case ConfirmAndRecord:
		items = append(items, Resolution{Action: "confirm-and-record", Live: true,
			Why: "proceed with an attributed, logged, REPORTED reason; does NOT un-taint"})
	case HardBlock:
		items = append(items, Resolution{Action: "confirm-and-record", Live: false,
			Why: "NOT offered — Restricted class, non-overridable"})
	}
	items = append(items, Resolution{Action: "cancel", Live: true, Why: "nothing happens"})
	return items
}

func pick(b bool, y, n string) string {
	if b {
		return y
	}
	return n
}
