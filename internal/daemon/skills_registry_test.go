package daemon

import (
	"reflect"
	"testing"
)

// A future provider implements the contract independently of every native adapter.
type futureSkillsFixture struct {
	name  string
	probe bool
}

func (p futureSkillsFixture) Name() string                   { return p.name }
func (p futureSkillsFixture) CanProbe() bool                 { return p.probe }
func (futureSkillsFixture) Roots(string, string) []skillRoot { return nil }
func (futureSkillsFixture) Coverage(*Skill) CoverageCell {
	return CoverageCell{State: "not-synced", Grade: "fs"}
}
func (futureSkillsFixture) Lint(*SkillEntry, string) []string { return nil }
func (futureSkillsFixture) Manages(string) bool               { return false }
func (futureSkillsFixture) Probe() *ProbeInfo                 { return nil }

func TestSkillsReportPublishesRegistryWithEmptyInventory(t *testing.T) {
	original := skillsProviders
	skillsProviders = map[string]SkillsProvider{}
	t.Cleanup(func() { skillsProviders = original })
	registerSkillsProvider(futureSkillsFixture{name: "zeta", probe: true})
	registerSkillsProvider(futureSkillsFixture{name: "alpha"})
	report := scanSkills("")
	if len(report.Skills) != 0 || !reflect.DeepEqual(report.Providers, []SkillsProviderInfo{
		{Runtime: "alpha"}, {Runtime: "zeta", CanProbe: true},
	}) {
		t.Fatalf("report does not reflect neutral registry: %+v", report)
	}
}
