package daemon

import (
	"path/filepath"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// TestSkillNameBecomesAnEntity pins D6: a Skill tool call used to tag anonymously
// (skill=use) so we recorded THAT a skill ran, never WHICH. Now the skill name is a
// first-class entity (skill:<name>) with the skill tag folded onto it, so "which
// skills did this session use" is answerable via the entity graph like files/mcp.
func TestSkillNameBecomesAnEntity(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	if err := g.Observe(Observation{SessionID: "s", Tool: "Skill",
		Skill: "example-data-proxy", TS: 10, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}

	// The normalizer must resolve the skill as the target entity.
	n := Normalize(Observation{Tool: "Skill", Skill: "example-data-proxy"})
	if n.TargetKind != "skill" || n.TargetIdentity != "example-data-proxy" {
		t.Fatalf("skill not resolved as an entity: %+v", n)
	}
	wantID := engine.EntityID("skill", "example-data-proxy")
	if n.TargetID != wantID {
		t.Fatalf("skill entity id = %q, want %q", n.TargetID, wantID)
	}

	// The entity exists and answers "which session used this skill".
	rep, err := g.EntityReport(wantID, entityEventCap)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Found {
		t.Fatal("skill entity not recorded — D6 still anonymous")
	}
	if len(rep.Touches) != 1 || rep.Touches[0].SessionID != "s" {
		t.Fatalf("skill entity does not name the session that used it: %+v", rep.Touches)
	}
	// The skill tag folded onto the entity (scope:resource), not only the session.
	var hasSkill bool
	for _, sr := range rep.State {
		if sr.Key == "skill" {
			hasSkill = true
		}
	}
	if !hasSkill {
		t.Error("skill tag did not fold onto the skill entity (detector needs scope:resource)")
	}

	// And it is enumerable via the entity listing, filtered by the new kind.
	ents, err := ix.ListEntities("skill", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Entity.Identity != "example-data-proxy" {
		t.Fatalf("skill not listed under kind=skill: %+v", ents)
	}
}
