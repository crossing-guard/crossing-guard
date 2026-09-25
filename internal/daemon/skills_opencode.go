package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func init() { registerSkillsProvider(opencodeSkills{}) }

// opencodeSkills is the OpenCode skills coverage provider. OpenCode reads .claude
// and .agents natively plus its own dirs; permission.skill in opencode.json
// applies deny rules (last match wins).
type opencodeSkills struct{}

func (opencodeSkills) Name() string        { return "opencode" }
func (opencodeSkills) CanProbe() bool      { return true }
func (opencodeSkills) Manages(string) bool { return false }

func (opencodeSkills) Roots(home, repo string) []skillRoot {
	roots := []skillRoot{{filepath.Join(home, ".config", "opencode", "skills"), "user", "opencode"}}
	if repo != "" {
		roots = append(roots, skillRoot{filepath.Join(repo, ".opencode", "skills"), "repo", "opencode"})
	}
	return roots
}

func (opencodeSkills) Lint(se *SkillEntry, dirName string) []string {
	name := se.Frontmatter["name"]
	if name != "" && name != dirName {
		return []string{fmt.Sprintf("name %q ≠ directory %q (OpenCode drops on mismatch)", name, dirName)}
	}
	return nil
}

func (opencodeSkills) Coverage(sk *Skill) CoverageCell {
	es := entriesIn(sk, "opencode", "claude", "agents")
	if len(es) == 0 {
		return CoverageCell{State: "not-synced", Grade: "fs", Why: "not in any OpenCode-read dir (it reads .claude and .agents natively)"}
	}
	for _, e := range es {
		for _, l := range e.Lints {
			if strings.Contains(l, "invalid YAML") {
				return CoverageCell{State: "dropped-invalid", Grade: "config", Why: l,
					Fix: "quote the value in SKILL.md frontmatter"}
			}
			if strings.Contains(l, "≠ directory") {
				return CoverageCell{State: "dropped-invalid", Grade: "config", Why: l,
					Fix: "rename dir or frontmatter name to match"}
			}
		}
	}
	if cell, matched := openCodeDeny(sk.Name); matched {
		return cell
	}
	return CoverageCell{State: "visible", Grade: "config", Why: "discoverable, no deny rule matched (probe to confirm live)"}
}

// openCodeDeny parses permission.skill from opencode.json in file order and
// applies last-match-wins.
func openCodeDeny(name string) (CoverageCell, bool) {
	for _, p := range []string{
		filepath.Join(homeDir(), ".config", "opencode", "opencode.json"),
		filepath.Join(homeDir(), ".config", "opencode", "config.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		sec := regexp.MustCompile(`"skill"\s*:\s*\{([^}]*)\}`).FindStringSubmatch(string(b))
		if sec == nil {
			continue
		}
		pairRe := regexp.MustCompile(`"([^"]+)"\s*:\s*"(allow|deny|ask)"`)
		decision, matchedPat := "", ""
		for _, m := range pairRe.FindAllStringSubmatch(sec[1], -1) {
			if ok, _ := filepath.Match(m[1], name); ok || m[1] == "*" {
				decision, matchedPat = m[2], m[1]
			}
		}
		if decision == "deny" {
			return CoverageCell{State: "hidden-by-deny", Grade: "config",
				Why: fmt.Sprintf("permission.skill %q: deny (last match wins) in %s", matchedPat, filepath.Base(p))}, true
		}
	}
	return CoverageCell{}, false
}

// Probe runs `opencode debug skill` (lists ALL discovered skills, pre-permission
// filter per the research).
func (opencodeSkills) Probe() *ProbeInfo {
	pi := &ProbeInfo{At: time.Now().UTC().Format(time.RFC3339)}
	if _, err := exec.LookPath("opencode"); err != nil {
		pi.Err = "opencode binary not found on PATH"
		return pi
	}
	cmd := exec.Command("opencode", "debug", "skill")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		pi.Err = err.Error()
		return pi
	}
	// Best-effort: try JSON, then fall back to name-looking tokens.
	var parsed any
	if json.Unmarshal(out, &parsed) == nil {
		collectNames(parsed, pi)
	}
	if len(pi.Names) == 0 {
		re := regexp.MustCompile(`(?m)^\s*(?:[-*]\s+)?([a-z0-9]+(?:-[a-z0-9]+)+)\s*$`)
		for _, m := range re.FindAllStringSubmatch(string(out), -1) {
			pi.Names = append(pi.Names, m[1])
		}
	}
	pi.OK = len(pi.Names) > 0
	if !pi.OK {
		pi.Err = "could not parse skill names from output (first 200 chars: " + truncate(string(out), 200) + ")"
	}
	return pi
}
