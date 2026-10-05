package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAntigravitySkillsScopeAndPluginOwnership(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	p := antigravitySkills{}
	pluginSkill := filepath.Join(home, ".gemini", "antigravity-cli", "plugins", "fixture", "skills", "example")
	if err := os.MkdirAll(pluginSkill, 0o700); err != nil {
		t.Fatal(err)
	}
	roots := p.Roots(home, repo)
	if len(roots) != 4 || !p.Manages(pluginSkill) || p.Manages(filepath.Join(home, ".gemini", "antigravity-cli", "plugins-neighbor", "example")) || p.CanProbe() {
		t.Fatalf("roots/ownership/probe=%+v", roots)
	}
	for _, tc := range []struct{ class, scope, state string }{
		{"agents", "user", "not-synced"}, {"agents", "repo", "visible"},
		{"antigravity", "user", "visible"}, {"claude", "repo", "not-synced"},
	} {
		cell := p.Coverage(&Skill{Entries: []SkillEntry{{DirClass: tc.class, Scope: tc.scope}}})
		if cell.State != tc.state || cell.Grade != "fs" {
			t.Fatalf("%+v: %+v", tc, cell)
		}
	}
}
