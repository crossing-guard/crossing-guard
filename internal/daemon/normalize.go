package daemon

// THE ONE NORMALIZER (governance plan R7).
//
// Turning a raw observation into (classifiable event, verb, target entity) is the
// step that decides what PRIMARY TRUTH says an action was. Phase 1 does it live;
// Phase 4's importer will do it over historical vendor files. If those two ever
// disagree, live state and replayed state diverge — and because tags are FROZEN at
// observe time, the divergence is permanent and undetectable after the fact.
//
// It lived inline in Governor.Observe. The plan said to extract it "now, not at item
// 9 — by then hundreds of thousands of frozen events will have folded through it and
// extracting it becomes a refactor of primary truth." That deadline was missed by
// five items while capture ran, which is exactly why it is item 1 now: every event
// captured against an un-extracted normalizer raises the cost of ever having two
// callers agree.
//
// The rule for anyone adding a caller: normalize HERE. A second place that builds an
// engine.Event, picks a verb, or resolves a target is a second normalizer, whatever
// it is named.

import (
	"strings"

	"crossing-guard/engine"
)

// Normalized is one canonical action, ready to classify and record.
type Normalized struct {
	Event          engine.Event // what the detectors see
	Verb           string       // read | write | exec | egress | search | use
	TargetID       string       // "" when the action targets no single resource
	TargetKind     string       // file | url | mcp
	TargetIdentity string       // the raw identity behind TargetID
}

// Normalize is the single translation from an observation to a canonical action.
// Both the live path and the importer MUST route through it.
func Normalize(o Observation) Normalized {
	id, kind, identity := targetEntity(o)
	return Normalized{
		Event:          normalizedEvent(o),
		Verb:           verbOf(o.Tool),
		TargetID:       id,
		TargetKind:     kind,
		TargetIdentity: identity,
	}
}

// normalizedEvent picks the ONE text channel a detector sees: the shell command if
// present, else the write/edit body — so command patterns and content (secret/PII)
// detectors both get what they need from a single field.
func normalizedEvent(o Observation) engine.Event {
	return engine.ActionEvent(o.Tool, o.Command, o.Content, o.FilePath, o.URL)
}

// targetEntity resolves the single resource an action targets: file > url > skill >
// mcp tool.
//
// A generic shell exec (Bash with a command, no file/url) resolves NO target — its
// command may touch many resources we cannot attribute without parsing it. GOVERNANCE
// IMPACT: resource facts found in a shell command fold onto the session but onto no
// resource entity, so the entity view under-reports what a shell exfil touched. This
// is the blocking dependency for resource-gating exec, not a cosmetic gap.
func targetEntity(o Observation) (id, kind, identity string) {
	switch {
	case o.FilePath != "":
		return engine.EntityID("file", o.FilePath), "file", o.FilePath
	case o.URL != "":
		return engine.EntityID("url", o.URL), "url", o.URL
	// The Skill tool names WHICH skill in its input; make that a first-class entity
	// (D6), so "which skill ran" is answerable via the entity graph like files/mcp,
	// not lost to an anonymous skill=use tag.
	case o.Skill != "":
		return engine.EntityID("skill", o.Skill), "skill", o.Skill
	case strings.HasPrefix(o.Tool, "mcp__"):
		return engine.EntityID("mcp", o.Tool), "mcp", engine.BareTool(o.Tool)
	default:
		return "", "", ""
	}
}

// verbOf maps a vendor tool name to the governance verb. Vendor names are folded via
// engine.BareTool first, so an mcp__-prefixed tool classifies like its bare form.
func verbOf(tool string) string {
	switch engine.BareTool(tool) {
	case "Read":
		return "read"
	case "Write", "Edit", "NotebookEdit", "apply_patch":
		return "write"
	case "Bash", "shell", "local_shell", "exec_command":
		return "exec"
	case "WebFetch":
		return "egress"
	case "WebSearch":
		return "search"
	default:
		return "use"
	}
}
