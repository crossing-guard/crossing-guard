package engine

import (
	"encoding/json"
	"os"
	"regexp"
	"sync"
)

// waterOrder is the monotonic data-class ladder; the water mark is max over it.
var waterOrder = []string{"public", "internal", "source-code", "customer-data",
	"regulated", "personal-data", "credential-material"}

// WaterMark returns the highest data-class among the tags, or "" (a WEAK negative —
// absence is not a clean bill; design §4). A projection of the ledger, not a store.
func WaterMark(tags []Tag) string {
	best := -1
	for _, t := range tags {
		if t.Key != "data-class" {
			continue
		}
		for i, v := range waterOrder {
			if v == t.Value && i > best {
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
	return term.Value == "" || t.Value == term.Value
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
	for _, c := range p.All {
		if err := compilePredicate(c, ruleID); err != nil {
			return err
		}
	}
	for _, c := range p.Any {
		if err := compilePredicate(c, ruleID); err != nil {
			return err
		}
	}
	if p.Not != nil {
		if err := compilePredicate(*p.Not, ruleID); err != nil {
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

// Match evaluates the predicate over a tag set (design §6 compound predicate).
func Match(p Predicate, tags []Tag) bool {
	switch {
	case len(p.All) > 0:
		for _, c := range p.All {
			if !Match(c, tags) {
				return false
			}
		}
		return true
	case len(p.Any) > 0:
		for _, c := range p.Any {
			if Match(c, tags) {
				return true
			}
		}
		return false
	case p.Not != nil:
		return !Match(*p.Not, tags)
	case p.isTerm():
		for _, t := range tags {
			if termMatches(p, t) {
				return true
			}
		}
		return false
	}
	return false
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
	var fired []Rule
	for _, r := range pol.Rules {
		if Match(r.If, tags) {
			fired = append(fired, r)
		}
	}
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
	switch topMode {
	case HardBlock, ConfirmAndRecord:
		dec = "block"
		reach = "HONEST block — STOP-reach channel (design §8); no downgrade"
	case WarnAndProceed:
		dec = "warn"
	}
	d := Decision{Decision: dec, Mode: topMode, Rule: top.ID, Message: top.Message,
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
