package codemap

// The role MECHANISM (console-and-info-panel §6 L4).
//
// L4 role is the only altitude that is not auto-derivable, and this file is
// careful about what it therefore contains: the machinery to EVALUATE a role
// rule, and nothing about what any particular role means. The taxonomy itself
// (entrypoint · controller · service · model · utility · presentation) is one
// more piece of config, our default — not core.
//
// A convention framework bakes L4 into conventions, so a file announces its
// role; a bare language has almost none, so role must be inferred from layout
// and dependency direction. The point of this mechanism is that BOTH arrive the
// same way: as declarative rules over mechanics an adapter already reported.

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
)

// Config is a convention config: declarative data that maps one stack's
// conventions onto a role taxonomy. This is the ONLY place a framework name may
// appear, and it appears as data — a value in a JSON file, never an identifier
// in code.
type Config struct {
	// Name identifies the config for display ("go-conventions", "laravel").
	Name string `json:"name"`
	// Taxonomy is the ordered set of roles this config may assign. Declaring it
	// makes "does this role exist" answerable without scanning every rule.
	Taxonomy []string `json:"taxonomy"`
	// Rules are evaluated IN ORDER; the first match wins. Order is how a config
	// expresses specificity, and keeping it explicit means a reader can see why
	// one rule beat another.
	Rules []RoleRule `json:"rules"`
	// Thresholds a config wants to reason about, named so a rule can reference
	// them instead of burying a number.
	Thresholds map[string]float64 `json:"thresholds,omitempty"`
}

// RoleRule assigns a role when every stated condition holds. Conditions are
// deliberately few and mechanical: core must be able to evaluate them without
// understanding any of the values.
type RoleRule struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// Why is prose shown to a reader when this rule matches. A role assignment
	// that cannot explain itself is a guess wearing a label.
	Why string `json:"why"`

	PathGlob    []string `json:"path_glob,omitempty"`     // any glob matches the unit path
	PathNotGlob []string `json:"path_not_glob,omitempty"` // no glob may match
	ImportsAny  []string `json:"imports_any,omitempty"`   // imports or externals contain any
	ImportsNone []string `json:"imports_none,omitempty"`  // contain none of these
	SignalsAny  []string `json:"signals_any,omitempty"`   // adapter signals contain any
	MinFanIn    *int     `json:"min_fan_in,omitempty"`    // Ca at least
	MaxFanOut   *int     `json:"max_fan_out,omitempty"`   // Ce at most
	MinFanOut   *int     `json:"min_fan_out,omitempty"`   // Ce at least
}

// LoadConfig reads a convention config from disk.
func LoadConfig(pathname string) (*Config, error) {
	body, err := os.ReadFile(pathname)
	if err != nil {
		return nil, fmt.Errorf("convention config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("convention config %s: %w", pathname, err)
	}
	return &c, nil
}

// assignRole evaluates the config against one unit's mechanics. An unmatched
// unit gets RoleUnknown WITH a reason — inventing a role for a file no rule
// describes is exactly the guess-as-measurement failure the provenance
// vocabulary exists to prevent.
func assignRole(cfg *Config, rel string, m *Mechanics, fanIn int) Role {
	if cfg == nil {
		return Role{Source: RoleUnknown, Why: "no convention config is loaded for this project"}
	}
	for _, r := range cfg.Rules {
		if ruleMatches(r, rel, m, fanIn) {
			return Role{Layer: r.Role, Source: RoleFromConvention, Rule: r.Name, Why: r.Why}
		}
	}
	return Role{
		Source: RoleUnknown,
		Why:    "no rule in " + cfg.Name + " matched this unit",
	}
}

func ruleMatches(r RoleRule, rel string, m *Mechanics, fanIn int) bool {
	if len(r.PathGlob) > 0 && !anyGlob(r.PathGlob, rel) {
		return false
	}
	if len(r.PathNotGlob) > 0 && anyGlob(r.PathNotGlob, rel) {
		return false
	}
	deps := append(append([]string{}, m.Imports...), m.External...)
	if len(r.ImportsAny) > 0 && !anyContains(deps, r.ImportsAny) {
		return false
	}
	if len(r.ImportsNone) > 0 && anyContains(deps, r.ImportsNone) {
		return false
	}
	if len(r.SignalsAny) > 0 && !anyContains(m.Signals, r.SignalsAny) {
		return false
	}
	if r.MinFanIn != nil && fanIn < *r.MinFanIn {
		return false
	}
	if r.MaxFanOut != nil && len(m.Imports) > *r.MaxFanOut {
		return false
	}
	if r.MinFanOut != nil && len(m.Imports) < *r.MinFanOut {
		return false
	}
	return true
}

// anyGlob matches a path against globs, supporting the `**` any-depth form that
// path.Match lacks.
func anyGlob(globs []string, rel string) bool {
	for _, g := range globs {
		if matchGlob(g, rel) {
			return true
		}
	}
	return false
}

// matchGlob matches a slash path against a glob where `**` spans any number of
// segments (including zero) and `*` stays within one. Segment-wise rather than
// string-wise, because a single `strings.Cut` on the first `**` cannot express
// `**/static/**` — the shape a config actually needs.
func matchGlob(glob, rel string) bool {
	return matchSegments(strings.Split(glob, "/"), strings.Split(rel, "/"))
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Zero segments consumed, or one-or-more: try every split.
			if matchSegments(pat[1:], seg) {
				return true
			}
			if len(seg) == 0 {
				return false
			}
			return matchSegments(pat, seg[1:])
		}
		if len(seg) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], seg[0]); !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	// A trailing directory glob ("store/**") already consumed the rest above; a
	// fully-consumed pattern must have consumed the whole path too.
	return len(seg) == 0
}

func anyContains(haystack, needles []string) bool {
	for _, h := range haystack {
		for _, n := range needles {
			if strings.Contains(h, n) {
				return true
			}
		}
	}
	return false
}
