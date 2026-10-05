package daemon

import (
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/vendorpaths"
)

func init() { registerSkillsProvider(antigravitySkills{}) }

// Antigravity exposes documented directories, not a measured zero-model skill
// enumeration API. Presence stays filesystem evidence, never live visibility.
type antigravitySkills struct{}

func (antigravitySkills) Name() string                      { return "antigravity" }
func (antigravitySkills) CanProbe() bool                    { return false }
func (antigravitySkills) Probe() *ProbeInfo                 { return nil }
func (antigravitySkills) Lint(*SkillEntry, string) []string { return nil }

func (antigravitySkills) Roots(home, repo string) []skillRoot {
	roots := []skillRoot{{filepath.Join(home, filepath.FromSlash(vendorpaths.AntigravitySkillsRelative)), "user", "antigravity"}}
	if repo != "" {
		roots = append(roots, skillRoot{filepath.Join(repo, ".agents", "skills"), "repo", "agents"},
			skillRoot{filepath.Join(repo, ".agent", "skills"), "repo", "antigravity"})
	}
	plugins := filepath.Join(home, filepath.FromSlash(vendorpaths.AntigravityPluginsRelative))
	entries, _ := os.ReadDir(plugins)
	for _, entry := range entries {
		if entry.IsDir() {
			roots = append(roots, skillRoot{filepath.Join(plugins, entry.Name(), "skills"), "user", "antigravity"})
		}
	}
	return roots
}

func (antigravitySkills) Manages(dir string) bool {
	root := filepath.Join(homeDir(), filepath.FromSlash(vendorpaths.AntigravityPluginsRelative))
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (antigravitySkills) Coverage(sk *Skill) CoverageCell {
	for _, entry := range sk.Entries {
		if entry.DirClass == "antigravity" || (entry.DirClass == "agents" && entry.Scope == "repo") {
			return CoverageCell{State: "visible", Grade: "fs", Why: "present in a documented Antigravity CLI skills directory; live loading and plugin enablement are unverified"}
		}
	}
	return CoverageCell{State: "not-synced", Grade: "fs", Why: "not present in an Antigravity CLI global, workspace or plugin skills directory"}
}
