package engine

import "strings"

// The two enforcement tiers (ADR 0025 §3) split ONE policy by whether a rule's
// predicate references state. The static tier (the hook, no daemon) evaluates the
// rules that do not; the daemon's stateful tier evaluates the rules that do. The split
// is by construction, not by tag absence: a static tag set never carries session:,
// target: or agent: keys, so a NEGATED state term there is vacuously true, and a rule
// like `all:[command~git push, not: session:reviewed]` would block every push whatever
// the review state. Excluding the rule is the only honest static answer.
//
// This is fail-open because no rule's fail_mode is read today: with the daemon down, no
// state rule fires. StaticTier is the seam where a future `fail_mode: closed` state rule
// would be re-admitted as a daemon-unreachable deny (governance-model.md, fail-closed).

// StaticTier returns the rules an evaluation without folded state may decide: every
// rule whose predicate references no state term and no route: fact, in order (Decide
// keeps the first of equal-strength rules), with the same capabilities. The input is
// not modified.
//
// route: is the third key class (OD-25): a hook call holds no route fact, so a route
// rule is excluded here for the same reason a state rule is, and is decided only by
// route admission at bind and at run start.
func StaticTier(p *Policy) *Policy {
	if p == nil {
		return nil
	}
	out := &Policy{Capabilities: p.Capabilities}
	for _, r := range p.Rules {
		if !ReferencesState(r.If) && !ReferencesRoute(r.If) {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}

// RouteTier returns the rules route admission decides: every rule whose predicate
// reads a route: fact, in order, with the same capabilities. It is the complement of
// both hook tiers for that key class; the input is not modified.
func RouteTier(p *Policy) *Policy {
	if p == nil {
		return nil
	}
	out := &Policy{Capabilities: p.Capabilities}
	for _, r := range p.Rules {
		if ReferencesRoute(r.If) {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}

// Gates reports whether the rule can stop an action — a hard deny or an overridable
// ask. Warn, observe and allow rules proceed; the stateful tier returns a verdict only
// for gating rules, so only those can make a preview's ALLOW differ from the live call.
func (r Rule) Gates() bool {
	switch r.effectiveMode() {
	case HardBlock, ConfirmAndRecord:
		return true
	}
	return false
}

// InvocationTags projects the exact invocation facts every tier decides over: the raw
// command, and the bare tool identity when there is one (no tool is guessed). The hook's
// static tier and the daemon's stateful tier build them here so a `tool=` term means
// the same thing in both.
func InvocationTags(tool, command string) []Tag {
	tags := []Tag{{Key: CommandTagKey, Value: command}}
	if tool = BareTool(strings.TrimSpace(tool)); tool != "" {
		tags = append(tags, Tag{Key: ToolTagKey, Value: tool})
	}
	return tags
}

// ActionEvent is the one event every tier classifies for an action: the tool, the one
// text channel a detector sees (the shell command if present, else the write/edit
// body — so command patterns and content detectors both read a single field), the path
// and the destination. The hook, the daemon's normalizer and the previews build it here,
// so "the same action" classifies to the same tags wherever it is judged.
func ActionEvent(tool, command, content, path, url string) Event {
	text := command
	if text == "" {
		text = content
	}
	return Event{Tool: BareTool(tool), Path: path, Destination: url, Text: text, Role: LiveEventRole}
}

// ActionTags is the tag set every tier decides one action over: the event's detector
// tags plus the invocation facts. The tool is the event's.
func ActionTags(ev Event, dets []Detector, command string) []Tag {
	return append(Classify(ev, dets), InvocationTags(ev.Tool, command)...)
}

// DetectorsRead narrows a detector set to the detectors whose fact some rule of the
// given policies reads. A tier that decides over those rules needs no other detector's
// tag: classifying with the rest could not change a decision, and on a large write body
// every pattern is a full scan. A destination detector's fact is destination-class.
// Order is kept; a nil policy reads nothing.
func DetectorsRead(dets []Detector, pols ...*Policy) []Detector {
	var out []Detector
	for _, d := range dets {
		key := d.Tag.Key
		if d.Kind == "destination" {
			key = "destination-class"
		}
		if policiesRead(key, pols) {
			out = append(out, d)
		}
	}
	return out
}

func policiesRead(key string, pols []*Policy) bool {
	is := func(k string) bool { return k == key }
	for _, pol := range pols {
		if pol == nil {
			continue
		}
		for _, r := range pol.Rules {
			if ReferencesKey(r.If, is) {
				return true
			}
		}
	}
	return false
}

// ReadsDetectorFacts reports whether any rule of the policies has a term that is
// neither an invocation fact nor state: the only terms a detector could answer.
func ReadsDetectorFacts(pols ...*Policy) bool {
	is := func(k string) bool { return !isInvocationKey(k) && !IsStateTag(k) }
	for _, pol := range pols {
		if pol == nil {
			continue
		}
		for _, r := range pol.Rules {
			if ReferencesKey(r.If, is) {
				return true
			}
		}
	}
	return false
}

// The blind spots of each evaluation site (Unknown): the terms it cannot answer because
// it never produces their facts. They restate the producer-channel table in boundary.go
// from the evaluator's side; TestUnknownsMatchTheChannelTable keeps the two together.

func isInvocationKey(key string) bool { return key == CommandTagKey || key == ToolTagKey }

// UnknownInvocation: a site with detector tags but no single invocation — the `tags`
// dry run and the session ledger. It has no raw command and no tool identity.
func UnknownInvocation(term Predicate) bool { return isInvocationKey(term.Tag) }

// UnknownWithoutDetectors: a site whose detector document could not load. Only the
// invocation's own facts are there to read.
func UnknownWithoutDetectors(term Predicate) bool { return !isInvocationKey(term.Tag) }

// UnknownTarget: the stateful tier on an action that resolves no single target (a shell
// command): there is no entity whose state could answer a target: term.
func UnknownTarget(term Predicate) bool { return strings.HasPrefix(term.Tag, TargetStatePrefix) }

// UnknownAgent: a site that could not read the session's agent: keys.
func UnknownAgent(term Predicate) bool { return strings.HasPrefix(term.Tag, AgentStatePrefix) }

// UnknownInPreview is a command preview's blind spot (`check`, the console's Test a
// command, the canaries): it names no tool, path or destination. The tool term is
// unanswerable, and so is any detector fact that a detector reading one of those could
// emit (kinds source and destination). Pattern and content detectors read only the
// text, which a preview has.
func UnknownInPreview(dets []Detector) Unknown {
	return func(term Predicate) bool {
		if term.Tag == ToolTagKey {
			return true
		}
		for _, d := range detectorProducers(term, dets, channelEngine) {
			if d.Kind == "source" || d.Kind == "destination" {
				return true
			}
		}
		return false
	}
}

// UnknownAtHarvest is the audit's blind spot: it judges a whole past session, so it has
// no single invocation or target, and it cannot read a declared state fact that any
// accepting producer writes live only.
func UnknownAtHarvest(sp StateProducers) Unknown {
	return func(term Predicate) bool {
		if isInvocationKey(term.Tag) || strings.HasPrefix(term.Tag, TargetStatePrefix) {
			return true
		}
		for _, p := range sp.Producers {
			if p.Tag != term.Tag || !p.Live || p.Harvest {
				continue
			}
			for _, v := range p.Values {
				if termAccepts(term, v) {
					return true
				}
			}
		}
		return false
	}
}
