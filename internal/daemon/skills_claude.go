package daemon

import (
	"fmt"
	"path/filepath"
)

func init() { registerSkillsProvider(claudeSkills{}) }

// claudeDescListingBudget is the char budget Claude's skill listing truncates a
// description at — a vendor fact used to lint and to grade coverage.
const claudeDescListingBudget = 1536

// claudeSkills is the Claude skills coverage provider. Claude has no zero-model
// enumeration seam, so fs presence is the coverage ceiling (no probe).
type claudeSkills struct{}

func (claudeSkills) Name() string        { return "claude" }
func (claudeSkills) CanProbe() bool      { return false }
func (claudeSkills) Probe() *ProbeInfo   { return nil }
func (claudeSkills) Manages(string) bool { return false }

func (claudeSkills) Roots(home, repo string) []skillRoot {
	roots := []skillRoot{{filepath.Join(home, ".claude", "skills"), "user", "claude"}}
	if repo != "" {
		roots = append(roots, skillRoot{filepath.Join(repo, ".claude", "skills"), "repo", "claude"})
	}
	return roots
}

func (claudeSkills) Lint(se *SkillEntry, _ string) []string {
	if se.DescLen > claudeDescListingBudget {
		return []string{fmt.Sprintf("description %d chars > Claude's %d listing budget (truncated)", se.DescLen, claudeDescListingBudget)}
	}
	return nil
}

func (claudeSkills) Coverage(sk *Skill) CoverageCell {
	es := entriesIn(sk, "claude")
	if len(es) == 0 {
		return CoverageCell{State: "not-synced", Grade: "fs", Why: "not present in ~/.claude/skills or .claude/skills",
			Fix: "ln -s <canon>/" + sk.Name + " ~/.claude/skills/" + sk.Name + "  (P-SKILL-7 proven path)"}
	}
	for _, e := range es {
		if e.DescLen > claudeDescListingBudget {
			return CoverageCell{State: "visible", Grade: "fs", Why: fmt.Sprintf("listed, but description truncated at %d chars", claudeDescListingBudget)}
		}
	}
	return CoverageCell{State: "visible", Grade: "fs", Why: "present in a Claude skills dir (no zero-model enumeration seam — fs grade is the ceiling)"}
}
