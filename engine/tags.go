// Package engine is the shared crossing-guard core: detectors → deterministic tags →
// water mark → policy predicate → decision. It is the single implementation the
// hook, check, audit, and checkpoint paths all evaluate through, so they can never
// diverge (the reason the six Python testbeds are ported into ONE Go package).
//
// A tag is a deterministic detector HIT — {key, value, detector, provenance, scope,
// evidence} — never a confidence score. Uncertainty lives in a detector's declared
// Coverage (enumerable vs unknowable gaps), not on the tag. (design §4/§4a)
package engine

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
)

// Provenance is a categorical trust fact, not a gradient (design §4).
type Provenance string

const (
	Observed        Provenance = "observed"         // we saw it directly — not agent-asserted
	IdentityDerived Provenance = "identity-derived" // a role (trusted iff integrated)
	UserAsserted    Provenance = "user-asserted"    // an override reason (attributed)
)

// Tag is a deterministic record that a specific detector matched an event.
type Tag struct {
	Key        string     `json:"key"`
	Value      string     `json:"value"`
	Detector   string     `json:"detector"`
	Provenance Provenance `json:"provenance"`
	Scope      string     `json:"scope"`
	Evidence   string     `json:"evidence,omitempty"`
	Coverage   Coverage   `json:"-"`
}

// identityKey is a tag's dedup identity: same key+value+detector = the same
// fact. Callers that need to compare or dedup tags MUST go through this — an
// earlier hand-joined copy in three places risked the dedup silently breaking
// if the separator or field order drifted in one of them.
func (t Tag) identityKey() string { return t.Key + "|" + t.Value + "|" + t.Detector }

// Coverage is where "how much do we catch" honestly lives — declared once per
// detector: enumerable (gaps are listable/closable) or not (unknowable). A measured
// miss-rate may exist for heuristics; it is the ONLY number, and never a per-tag score.
type Coverage struct {
	Enumerable       bool     `json:"enumerable"`
	Gaps             []string `json:"gaps,omitempty"`
	MeasuredMissRate *float64 `json:"measured_miss_rate,omitempty"`
}

// Detector is one user-declared or built-in rule. Kind selects which fields apply.
type Detector struct {
	Comment string `json:"_comment,omitempty"`
	ID      string `json:"id"`
	Kind    string `json:"kind"` // source | destination | pattern | content

	// source
	Match struct {
		Tool         []string `json:"tool"`
		SourcePrefix []string `json:"source_prefix"`
		PathPrefix   []string `json:"path_prefix"`
	} `json:"match"`

	// destination
	InHouseHosts    []string `json:"in_house_hosts"`
	InHouseSuffixes []string `json:"in_house_suffixes"`
	InHouseCIDRs    []string `json:"in_house_cidrs"`

	// pattern
	Regex string `json:"regex"`
	re    *regexp.Regexp

	// Evidence controls whether a pattern detector's MATCHED TEXT is stored.
	// Default ("" ) = redacted, because evidence is written into the append-only
	// event log and the state fold — permanently, and this is the field a secret
	// pattern matches. A detector must opt IN to "raw", so a new one (including a
	// user-overlay detector we never review) is safe by omission rather than
	// leaking by omission. Benign detectors like vcs.commit declare "raw".
	Evidence string `json:"evidence,omitempty"` // "" (redacted) | "raw"

	// content
	Keywords []string `json:"keywords"`

	// scope + overlay controls (design: layered detector config)
	Roles    []string `json:"roles"`    // event roles this detector applies to; empty = any role
	Disabled bool     `json:"disabled"` // overlay: turn a shipped detector off by id
	Scope    string   `json:"scope"`    // "resource": its tag describes the target entity (folds onto it); else session-only

	Tag      tagSpec  `json:"tag"`
	Coverage Coverage `json:"coverage"`
}

type tagSpec struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Event is what a detector classifies: identity (tool/source/path), a destination,
// and/or raw text. Classify by SOURCE not content wherever possible (design §3).
type Event struct {
	Tool        string `json:"tool"`
	Source      string `json:"source"`
	Path        string `json:"path"`
	Destination string `json:"destination"`
	Text        string `json:"text"`
	// Role is the event's channel/role (assistant|user|thinking|tool_call|
	// tool_result). A detector with a non-empty Roles list only fires when Role
	// is in it — so chat-behavior detectors stay inert on the live tool-call hook
	// and fire only on the audit/transcript path. Empty Role matches only
	// role-agnostic detectors (those declaring no Roles).
	Role string `json:"role"`
}

// LoadDetectors reads a detectors.json ({"detectors":[...]}) and compiles patterns.
func LoadDetectors(path string) ([]Detector, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseDetectors(b)
}

// parseDetectors unmarshals a {"detectors":[...]} document and compiles its
// pattern regexes. Shared by LoadDetectors (a file) and LoadLayered (the
// embedded default + an overlay).
func parseDetectors(b []byte) ([]Detector, error) {
	var wrap struct {
		Detectors []Detector `json:"detectors"`
	}
	if err := json.Unmarshal(b, &wrap); err != nil {
		return nil, err
	}
	if err := CompileDetectors(wrap.Detectors); err != nil {
		return nil, err
	}
	return wrap.Detectors, nil
}

// CompileDetectors is the load-time gate every detector document passes through: it
// compiles pattern regexes and rejects a tag key in a state namespace or one of the two
// invocation keys (every tier decides over detector tags and invocation facts together,
// so a detector emitting `tool` or `command` would answer an invocation term). State keys are
// built only by the daemon's stateful tier (folded session/target state, model claims);
// a detector emitting one would put a state fact into the static tag set, where a
// stateful rule term would match a fact the session never folded.
// Every entry is checked, tombstones included: Classify does not skip Disabled.
func CompileDetectors(dets []Detector) error {
	for i := range dets {
		if IsStateTag(dets[i].Tag.Key) {
			return fmt.Errorf("detector %s: tag key %q is in a reserved state namespace (%s, %s, %s are built by the stateful tier; a detector cannot emit them)",
				dets[i].ID, dets[i].Tag.Key, SessionStatePrefix, TargetStatePrefix, AgentStatePrefix)
		}
		if k := dets[i].Tag.Key; k == CommandTagKey || k == ToolTagKey {
			return fmt.Errorf("detector %s: tag key %q is reserved for the invocation's own facts (engine.InvocationTags); a detector cannot emit it",
				dets[i].ID, k)
		}
		if dets[i].Kind == "pattern" && dets[i].Regex != "" {
			re, err := regexp.Compile(dets[i].Regex)
			if err != nil {
				return fmt.Errorf("detector %s: %w", dets[i].ID, err)
			}
			dets[i].re = re
		}
	}
	return nil
}

func roleMatches(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// Classify runs every detector against one event → deterministic tags. Order and
// semantics mirror detect.py exactly (the parity requirement). A detector whose
// Roles list is non-empty is skipped unless it lists ev.Role — the one addition
// on top of the parity core, and a no-op for any detector that declares no Roles.
func Classify(ev Event, dets []Detector) []Tag {
	var tags []Tag
	for _, d := range dets {
		if len(d.Roles) > 0 && !roleMatches(d.Roles, ev.Role) {
			continue
		}
		switch d.Kind {
		case "source":
			if evd := matchSource(d, ev); evd != "" {
				tags = append(tags, mkTag(d, d.Tag.Key, d.Tag.Value, evd))
			}
		case "destination":
			if ev.Destination != "" {
				val, evd := destClass(d, ev.Destination)
				tags = append(tags, mkTag(d, "destination-class", val, evd))
			}
		case "pattern":
			if d.re != nil {
				if m := d.re.FindString(ev.Text); m != "" {
					tags = append(tags, mkTag(d, d.Tag.Key, d.Tag.Value, patternEvidence(d, m)))
				}
			}
		case "content":
			low := strings.ToLower(ev.Text)
			for _, k := range d.Keywords {
				if strings.Contains(low, strings.ToLower(k)) {
					tags = append(tags, mkTag(d, d.Tag.Key, d.Tag.Value, "kw:"+k))
					break
				}
			}
		}
	}
	return tags
}

func matchSource(d Detector, ev Event) string {
	for _, t := range d.Match.Tool {
		if ev.Tool != "" && ev.Tool == t {
			return "tool=" + ev.Tool
		}
	}
	for _, pre := range d.Match.SourcePrefix {
		if ev.Source != "" && strings.HasPrefix(ev.Source, pre) {
			return "source=" + ev.Source
		}
	}
	for _, pre := range d.Match.PathPrefix {
		if ev.Path != "" && strings.Contains(ev.Path, pre) {
			return "path=" + ev.Path
		}
	}
	return ""
}

// destClass is fail-safe: anything not in the in-house allowlist resolves to external.
// Host extraction goes through the ONE canonicalizer (urlHost) so the host the
// allowlist checks is identical to the host EntityID keys the resource under — an
// earlier inline regex disagreed with urlHost on schemeless/userinfo URLs.
func destClass(d Detector, dest string) (string, string) {
	host := urlHost(dest)
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		for _, c := range d.InHouseCIDRs {
			if _, netw, err := net.ParseCIDR(c); err == nil && netw.Contains(ip) {
				return "in-house", host + " ∈ " + c
			}
		}
		return "external", host + " (ip, not in-house)"
	}
	for _, h := range d.InHouseHosts {
		if host == h {
			return "in-house", host
		}
	}
	for _, suf := range d.InHouseSuffixes {
		if strings.HasSuffix(host, suf) {
			return "in-house", host
		}
	}
	return "external", host + " (not in the in-house allowlist)"
}

func mkTag(d Detector, key, value, evidence string) Tag {
	if key == DataClassKey {
		value = CanonicalDataClass(value) // a pinned or overlay document may carry a legacy spelling
	}
	return Tag{Key: key, Value: value, Detector: d.ID, Provenance: Observed,
		Scope: "event", Evidence: evidence, Coverage: d.Coverage}
}

// redact returns evidence that a pattern matched WITHOUT echoing what it matched.
//
// The previous rule leaked in both directions: a match of 8 bytes or fewer was
// stored verbatim (a short token, kept forever in an append-only log), and a longer
// one kept its first 4 characters — enough to identify the provider from a prefix
// like "sk-a", "ghp_" or "AKIA", and sometimes the account. Neither is acceptable
// for a product whose pitch is data governance; the length alone is enough to tell
// a human the detector fired on something real.
func redact(s string) string { return fmt.Sprintf("[redacted %d chars]", len(s)) }

// patternEvidence applies the detector's declared evidence mode. Unknown values are
// treated as redacted: a typo in an overlay must not silently turn on raw capture.
func patternEvidence(d Detector, match string) string {
	if d.Evidence == "raw" {
		return match
	}
	return redact(match)
}
