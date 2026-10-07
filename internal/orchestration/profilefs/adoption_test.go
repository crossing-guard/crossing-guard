package profilefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Team adoption of shared agents (team rest-of-release plan §4.1 decisions 3–5;
// criteria 52–55, 73).

const (
	testOrganization = "org_acme"
	testScope        = "organization"
)

func otherAgentSource(t *testing.T, id, version, instructions string) []byte {
	t.Helper()
	source, err := Duplicate(validReviewerSource(version, instructions), id, "Agent "+id, "A second agent for adoption tests.")
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func adoptTeam(t *testing.T, owner *Owner, organization, scope, bundle string, documents ...TeamDocument) AdoptTeamResult {
	t.Helper()
	plan, err := owner.PlanTeam(organization, scope, documents)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.AdoptTeam(AdoptTeamCommand{OrganizationID: organization, Scope: scope, BundleID: bundle,
		Documents: documents, ExpectedStateToken: plan.StateToken})
	if err != nil {
		t.Fatalf("adopt %s: %v", bundle, err)
	}
	return result
}

func selectLocally(t *testing.T, owner *Owner, source []byte) SelectResult {
	t.Helper()
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Select(commandFromPreview(source, preview))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func selectionBytes(t *testing.T, dataDir, profileID string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "orchestration", "profiles", "selections", profileKey(profileID)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// filesUnder lists every regular file under the profile root, for "the adoption
// record is the only file removed".
func filesUnder(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join(dataDir, "orchestration", "profiles")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() == "mutation.lock" {
			return err
		}
		raw, readErr := os.ReadFile(path)
		out[strings.TrimPrefix(path, root)] = string(raw)
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Decision 3, first case: no selection → written by the adoption, inert, listed.
func TestAdoptTeamWritesASelectionForANewAgent(t *testing.T) {
	owner, _ := newTestOwner(t)
	source := validReviewerSource("1.0.0", "Shared instructions.\n")
	result := adoptTeam(t, owner, testOrganization, testScope, "bnd_1", TeamDocument{Name: "reviewer", Source: source})
	if len(result.Documents) != 1 || result.Documents[0].Outcome != TeamDocumentNew {
		t.Fatalf("documents = %+v", result.Documents)
	}
	detail, err := owner.Get("command-reviewer")
	if err != nil || detail.Current.SelectedBy != SelectedByTeamAdoption || detail.Source != string(source) ||
		detail.RuntimeEffects || detail.SelectionState != "selected_inert" {
		t.Fatalf("detail = %+v, %v", detail, err)
	}
	adoption, found, err := owner.Adoption(testOrganization, testScope)
	if err != nil || !found || adoption.BundleID != "bnd_1" || len(adoption.Documents) != 1 ||
		!adoption.Lists("command-reviewer", detail.Current.SourceDigest, detail.Current.BundleDigest) {
		t.Fatalf("adoption = %+v found=%v err=%v", adoption, found, err)
	}
	origin, found, err := owner.Provenance("command-reviewer")
	if err != nil || !found || origin.SelectedBy != SelectedByTeamAdoption || origin.Adoption == nil || origin.Released != nil {
		t.Fatalf("provenance = %+v %v %v", origin, found, err)
	}
	if got, ok, _ := owner.AdoptionOfRevision("command-reviewer", detail.Current.SourceDigest, detail.Current.BundleDigest); !ok || got.Scope != testScope {
		t.Fatalf("a place on the adopted revision belongs to the adoption: %+v %v", got, ok)
	}
}

// Decision 3, second case, and criterion 53: the lead's own selection is
// byte-for-byte unchanged by adopting and by un-adopting, and Un-adopt removes only
// the adoption record file.
func TestLeadsOwnSelectionIsUntouchedByAdoptAndUnadopt(t *testing.T) {
	owner, dataDir := newTestOwner(t)
	source := validReviewerSource("1.0.0", "The lead's own agent.\n")
	local := selectLocally(t, owner, source)
	before := selectionBytes(t, dataDir, "command-reviewer")
	beforeFiles := filesUnder(t, dataDir)

	result := adoptTeam(t, owner, testOrganization, testScope, "bnd_1", TeamDocument{Name: "reviewer", Source: source})
	if result.Documents[0].Outcome != TeamDocumentUnchanged {
		t.Fatalf("same digests must write nothing: %+v", result.Documents)
	}
	if string(selectionBytes(t, dataDir, "command-reviewer")) != string(before) {
		t.Fatal("adopting the bundle the lead published changed the lead's selection bytes")
	}
	if _, ok, _ := owner.AdoptionOfRevision("command-reviewer", local.Detail.Current.SourceDigest, local.Detail.Current.BundleDigest); ok {
		t.Fatal("the lead's own places record no adoption key")
	}
	afterAdopt := filesUnder(t, dataDir)
	added := []string{}
	for path := range afterAdopt {
		if _, existed := beforeFiles[path]; !existed {
			added = append(added, path)
		}
	}
	if len(added) != 1 || !strings.Contains(added[0], adoptionsDirectory) {
		t.Fatalf("adoption must add only its record: %v", added)
	}

	removed, found, err := owner.Unadopt(testOrganization, testScope)
	if err != nil || !found || removed.BundleID != "bnd_1" {
		t.Fatalf("unadopt = %+v %v %v", removed, found, err)
	}
	afterUnadopt := filesUnder(t, dataDir)
	if len(afterUnadopt) != len(beforeFiles) {
		t.Fatalf("un-adopt must remove only the adoption record: %d files, want %d", len(afterUnadopt), len(beforeFiles))
	}
	for path, body := range beforeFiles {
		if afterUnadopt[path] != body {
			t.Fatalf("%s changed across adopt and un-adopt", path)
		}
	}
	// The lead can still edit their own agent.
	if _, err := owner.Select(commandAfter(t, owner, validReviewerSource("1.1.0", "Edited.\n"))); err != nil {
		t.Fatalf("the lead's own agent stays editable: %v", err)
	}
}

func commandAfter(t *testing.T, owner *Owner, source []byte) SelectCommand {
	t.Helper()
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	return commandFromPreview(source, preview)
}

// Decision 3, third case: an adoption-written selection with different digests gets
// a new current revision, and the plan carries the previous text for the diff.
func TestAdoptTeamUpdatesItsOwnSelection(t *testing.T) {
	owner, _ := newTestOwner(t)
	first := validReviewerSource("1.0.0", "First shared text.\n")
	second := validReviewerSource("1.1.0", "Second shared text.\n")
	adoptTeam(t, owner, testOrganization, testScope, "bnd_1", TeamDocument{Name: "reviewer", Source: first})
	plan, err := owner.PlanTeam(testOrganization, testScope, []TeamDocument{{Name: "reviewer", Source: second}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Documents[0].Outcome != TeamDocumentUpdate || plan.Documents[0].PreviousSource != string(first) || plan.Documents[0].Adopted {
		t.Fatalf("plan = %+v", plan.Documents[0])
	}
	adoptTeam(t, owner, testOrganization, testScope, "bnd_2", TeamDocument{Name: "reviewer", Source: second})
	detail, _ := owner.Get("command-reviewer")
	if detail.Current.Version != "1.1.0" || len(detail.History) != 1 || detail.History[0].Version != "1.0.0" ||
		detail.Current.SelectedBy != SelectedByTeamAdoption {
		t.Fatalf("detail = %+v", detail)
	}
	adoption, _, _ := owner.Adoption(testOrganization, testScope)
	if adoption.BundleID != "bnd_2" || !adoption.Lists("command-reviewer", detail.Current.SourceDigest, detail.Current.BundleDigest) ||
		adoption.Lists("command-reviewer", detail.History[0].SourceDigest, detail.History[0].BundleDigest) {
		t.Fatalf("the record must list the new revision only: %+v", adoption)
	}
	// Adopting the same bundle again is a no-op by digest.
	again := adoptTeam(t, owner, testOrganization, testScope, "bnd_2", TeamDocument{Name: "reviewer", Source: second})
	if again.Documents[0].Outcome != TeamDocumentUnchanged {
		t.Fatalf("again = %+v", again.Documents)
	}
}

// Decision 4 and criterion 54: a member's own agent with the same id is never
// replaced; the document is named; the rest of the bundle adopts; the member's
// selection and draft are byte-identical.
func TestAdoptTeamCollisionLeavesTheMembersAgentAndAdoptsTheRest(t *testing.T) {
	owner, dataDir := newTestOwner(t)
	mine := validReviewerSource("1.0.0", "My own reviewer.\n")
	selectLocally(t, owner, mine)
	before := filesUnder(t, dataDir)

	theirs := validReviewerSource("2.0.0", "The team's reviewer with my id.\n")
	other := otherAgentSource(t, "team-helper", "1.0.0", "A team helper.\n")
	result := adoptTeam(t, owner, testOrganization, testScope, "bnd_1",
		TeamDocument{Name: "reviewer", Source: theirs}, TeamDocument{Name: "helper", Source: other})
	if result.Documents[0].Outcome != TeamDocumentCollision || result.Documents[0].ProfileID != "command-reviewer" ||
		result.Documents[1].Outcome != TeamDocumentNew {
		t.Fatalf("documents = %+v", result.Documents)
	}
	after := filesUnder(t, dataDir)
	for path, body := range before {
		if after[path] != body {
			t.Fatalf("the member's own file %s changed", path)
		}
	}
	detail, _ := owner.Get("command-reviewer")
	if detail.Source != string(mine) || detail.Current.SelectedBy != SelectedByLocalClient {
		t.Fatalf("the member's agent was replaced: %+v", detail.Current)
	}
	if _, err := owner.Get("team-helper"); err != nil {
		t.Fatalf("the rest of the bundle must adopt: %v", err)
	}
	adoption, _, _ := owner.Adoption(testOrganization, testScope)
	if len(adoption.Documents) != 1 || adoption.Documents[0].ProfileID != "team-helper" {
		t.Fatalf("a collided document is not listed: %+v", adoption.Documents)
	}
}

// A draft for an agent never published also uses the id.
func TestAdoptTeamTreatsAnUnpublishedDraftAsACollision(t *testing.T) {
	owner, _ := newTestOwner(t)
	draft := validReviewerSource("0.1.0", "Still drafting.\n")
	if _, err := owner.PutDraft(DraftCommand{ProfileID: "command-reviewer", Source: draft,
		ExpectedStateToken: DraftAbsentToken("command-reviewer")}); err != nil {
		t.Fatal(err)
	}
	result := adoptTeam(t, owner, testOrganization, testScope, "bnd_1",
		TeamDocument{Name: "reviewer", Source: validReviewerSource("1.0.0", "Team text.\n")})
	if result.Documents[0].Outcome != TeamDocumentCollision {
		t.Fatalf("documents = %+v", result.Documents)
	}
	if _, err := owner.Get("command-reviewer"); ProblemCode(err) != "not_found" {
		t.Fatalf("no selection may be written over a draft: %v", err)
	}
	kept, found, _ := owner.Draft("command-reviewer")
	if !found || kept.Source != string(draft) {
		t.Fatalf("the draft must be untouched: %+v", kept)
	}
}

// Criterion 54's failure path: a stale state token refuses the adoption and nothing
// changes.
func TestAdoptTeamRefusesAStaleStateTokenAndChangesNothing(t *testing.T) {
	owner, dataDir := newTestOwner(t)
	documents := []TeamDocument{{Name: "reviewer", Source: validReviewerSource("1.0.0", "Team text.\n")},
		{Name: "helper", Source: otherAgentSource(t, "team-helper", "1.0.0", "Helper.\n")}}
	plan, err := owner.PlanTeam(testOrganization, testScope, documents)
	if err != nil {
		t.Fatal(err)
	}
	// The member imports their own agent under one of the ids after the offer was shown.
	selectLocally(t, owner, validReviewerSource("9.0.0", "Mine, imported meanwhile.\n"))
	before := filesUnder(t, dataDir)
	_, err = owner.AdoptTeam(AdoptTeamCommand{OrganizationID: testOrganization, Scope: testScope, BundleID: "bnd_1",
		Documents: documents, ExpectedStateToken: plan.StateToken})
	if ProblemCode(err) != "state_conflict" {
		t.Fatalf("a stale token must refuse the adoption, got %v", err)
	}
	after := filesUnder(t, dataDir)
	if len(after) != len(before) {
		t.Fatalf("a refused adoption wrote files: %d → %d", len(before), len(after))
	}
	for path, body := range before {
		if after[path] != body {
			t.Fatalf("%s changed in a refused adoption", path)
		}
	}
	if _, found, _ := owner.Adoption(testOrganization, testScope); found {
		t.Fatal("a refused adoption left a record")
	}
	if _, err := owner.AdoptTeam(AdoptTeamCommand{OrganizationID: testOrganization, Scope: testScope, BundleID: "bnd_1",
		Documents: documents}); ProblemCode(err) != "state_conflict" {
		t.Fatalf("a missing token is refused too, got %v", err)
	}
}

// Un-adopt marks an adoption-written selection released; a later adoption of the
// same scope treats it as its own, and another organization's is a collision
// (criterion 73's failure path).
func TestUnadoptReleasesAndTheSameScopeAdoptsAgain(t *testing.T) {
	owner, _ := newTestOwner(t)
	first := validReviewerSource("1.0.0", "Shared.\n")
	adoptTeam(t, owner, testOrganization, testScope, "bnd_1", TeamDocument{Name: "reviewer", Source: first})
	if _, found, err := owner.Unadopt(testOrganization, testScope); err != nil || !found {
		t.Fatalf("unadopt: %v %v", found, err)
	}
	origin, _, _ := owner.Provenance("command-reviewer")
	if origin.Released == nil || origin.Released.OrganizationID != testOrganization || origin.Released.Scope != testScope || origin.Adoption != nil {
		t.Fatalf("the selection must be marked released with the organization and scope: %+v", origin)
	}
	detail, err := owner.Get("command-reviewer")
	if err != nil || detail.Integrity != "verified" {
		t.Fatalf("a released selection still reads: %+v %v", detail, err)
	}
	if _, ok, _ := owner.AdoptionOfRevision("command-reviewer", detail.Current.SourceDigest, detail.Current.BundleDigest); ok {
		t.Fatal("a released agent belongs to no adoption")
	}
	if _, found, _ := owner.Unadopt(testOrganization, testScope); found {
		t.Fatal("a second un-adopt finds nothing")
	}

	// Another organization carrying the same id collides.
	second := validReviewerSource("1.1.0", "Shared, changed.\n")
	elsewhere := adoptTeam(t, owner, "org_other", testScope, "bnd_x", TeamDocument{Name: "reviewer", Source: second})
	if elsewhere.Documents[0].Outcome != TeamDocumentCollision {
		t.Fatalf("another organization's same-id profile must collide: %+v", elsewhere.Documents)
	}
	// So does another scope of the same organization.
	repository := adoptTeam(t, owner, testOrganization, "repository:repo_1", "bnd_r", TeamDocument{Name: "reviewer", Source: second})
	if repository.Documents[0].Outcome != TeamDocumentCollision {
		t.Fatalf("another scope's same-id profile must collide: %+v", repository.Documents)
	}
	// Re-adopting the very revision it released removes the mark and writes no revision.
	same := adoptTeam(t, owner, testOrganization, testScope, "bnd_1b", TeamDocument{Name: "reviewer", Source: first})
	origin, _, _ = owner.Provenance("command-reviewer")
	if same.Documents[0].Outcome != TeamDocumentUnchanged || origin.Released != nil || origin.Adoption == nil {
		t.Fatalf("the releasing scope takes the same revision up again: %+v %+v", same.Documents, origin)
	}
	if again, _ := owner.Get("command-reviewer"); len(again.History) != 0 {
		t.Fatalf("re-adopting the same revision must write no new revision: %+v", again.History)
	}
	if _, _, err := owner.Unadopt(testOrganization, testScope); err != nil {
		t.Fatal(err)
	}
	// The scope it was released from takes it up again, and the mark is gone.
	back := adoptTeam(t, owner, testOrganization, testScope, "bnd_2", TeamDocument{Name: "reviewer", Source: second})
	if back.Documents[0].Outcome != TeamDocumentUpdate {
		t.Fatalf("the releasing scope treats the selection as its own: %+v", back.Documents)
	}
	origin, _, _ = owner.Provenance("command-reviewer")
	if origin.Released != nil || origin.Adoption == nil || origin.Adoption.BundleID != "bnd_2" {
		t.Fatalf("after re-adoption: %+v", origin)
	}
}

// After a relink elsewhere the earlier organization's adoption stays, and the new
// organization's same-id profile is a collision (criterion 73).
func TestAnotherOrganizationsSameIDProfileCollidesWhileAdopted(t *testing.T) {
	owner, _ := newTestOwner(t)
	source := validReviewerSource("1.0.0", "Organization A's agent.\n")
	adoptTeam(t, owner, "org_a", testScope, "bnd_a", TeamDocument{Name: "reviewer", Source: source})
	result := adoptTeam(t, owner, "org_b", testScope, "bnd_b",
		TeamDocument{Name: "reviewer", Source: validReviewerSource("3.0.0", "Organization B's agent.\n")})
	if result.Documents[0].Outcome != TeamDocumentCollision {
		t.Fatalf("documents = %+v", result.Documents)
	}
	detail, _ := owner.Get("command-reviewer")
	if detail.Source != string(source) {
		t.Fatal("organization A's agent was replaced")
	}
	kept, found, _ := owner.Adoption("org_a", testScope)
	if !found || kept.BundleID != "bnd_a" {
		t.Fatalf("organization A's adoption must be untouched: %+v", kept)
	}
	all, err := owner.Adoptions()
	if err != nil || len(all) != 2 || all[0].OrganizationID != "org_a" || all[1].OrganizationID != "org_b" || len(all[1].Documents) != 0 {
		t.Fatalf("adoptions = %+v %v", all, err)
	}
}

// OD-21: an adopted agent is read-only; the refusal names Duplicate, and the
// duplicate is the member's own, editable agent.
func TestAdoptedAgentIsReadOnlyAndDuplicateIsTheWay(t *testing.T) {
	owner, _ := newTestOwner(t)
	shared := validReviewerSource("1.0.0", "Shared.\n")
	adoptTeam(t, owner, testOrganization, testScope, "bnd_1", TeamDocument{Name: "reviewer", Source: shared})
	edited := validReviewerSource("1.1.0", "A local edit.\n")
	_, err := owner.Select(commandAfter(t, owner, edited))
	if ProblemCode(err) != "adopted_read_only" {
		t.Fatalf("a local select over an adopted agent must be refused, got %v", err)
	}
	var refusal *Problem
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Recovery, "Duplicate") {
		t.Fatalf("the refusal must say Duplicate is the way: %+v", refusal)
	}
	if _, err := owner.PutDraft(DraftCommand{ProfileID: "command-reviewer", Source: edited,
		ExpectedStateToken: DraftAbsentToken("command-reviewer")}); ProblemCode(err) != "adopted_read_only" {
		t.Fatalf("a draft over an adopted agent must be refused, got %v", err)
	}
	detail, _ := owner.Get("command-reviewer")
	if detail.Source != string(shared) {
		t.Fatal("a refused edit changed the adopted agent")
	}
	duplicate, err := Duplicate(shared, "my-reviewer", "My reviewer", "My own copy.")
	if err != nil {
		t.Fatal(err)
	}
	own := selectLocally(t, owner, duplicate)
	if own.Detail.Current.SelectedBy != SelectedByLocalClient {
		t.Fatalf("the duplicate is the member's own: %+v", own.Detail.Current)
	}
	if _, ok, _ := owner.AdoptionOfRevision("my-reviewer", own.Detail.Current.SourceDigest, own.Detail.Current.BundleDigest); ok {
		t.Fatal("a place on the duplicate records no adoption key")
	}
}

// A revision that drops an agent releases its selection, so it reads as no longer
// shared, and the previous digests are no longer listed (criterion 74's data).
func TestANewRevisionThatDropsAnAgentReleasesIt(t *testing.T) {
	owner, _ := newTestOwner(t)
	reviewer := TeamDocument{Name: "reviewer", Source: validReviewerSource("1.0.0", "Shared.\n")}
	helper := TeamDocument{Name: "helper", Source: otherAgentSource(t, "team-helper", "1.0.0", "Helper.\n")}
	adoptTeam(t, owner, testOrganization, testScope, "bnd_1", reviewer, helper)
	adoptTeam(t, owner, testOrganization, testScope, "bnd_2", reviewer)
	origin, _, _ := owner.Provenance("team-helper")
	if origin.Released == nil || origin.Released.Scope != testScope {
		t.Fatalf("the dropped agent must be released: %+v", origin)
	}
	adoption, _, _ := owner.Adoption(testOrganization, testScope)
	if len(adoption.Documents) != 1 || adoption.Documents[0].ProfileID != "command-reviewer" {
		t.Fatalf("adoption = %+v", adoption)
	}
}

func TestRefreshAdoptionChangesOnlyTheBundleID(t *testing.T) {
	owner, _ := newTestOwner(t)
	adoptTeam(t, owner, testOrganization, testScope, "bnd_1", TeamDocument{Name: "reviewer", Source: validReviewerSource("1.0.0", "Shared.\n")})
	before, _, _ := owner.Adoption(testOrganization, testScope)
	refreshed, err := owner.RefreshAdoption(testOrganization, testScope, "bnd_2")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.BundleID != "bnd_2" || refreshed.AdoptedAt != before.AdoptedAt || len(refreshed.Documents) != 1 ||
		refreshed.Documents[0] != before.Documents[0] || refreshed.StateToken == before.StateToken {
		t.Fatalf("refreshed = %+v", refreshed)
	}
	if _, err := owner.RefreshAdoption("org_none", testScope, "bnd_2"); ProblemCode(err) != "not_found" {
		t.Fatalf("refreshing nothing must say so, got %v", err)
	}
}

// The selection format gained an optional field. A record written before it reads
// unchanged and keeps its token; an unrelated selection's bytes never move when
// another agent is adopted or released.
func TestOlderSelectionRecordsReadUnchangedAndUnrelatedOnesNeverMove(t *testing.T) {
	owner, dataDir := newTestOwner(t)
	mine := otherAgentSource(t, "my-agent", "1.0.0", "Mine.\n")
	local := selectLocally(t, owner, mine)
	raw := selectionBytes(t, dataDir, "my-agent")
	if strings.Contains(string(raw), "released") {
		t.Fatalf("a never-released record must not carry the field: %s", raw)
	}
	token := local.Detail.StateToken
	adoptTeam(t, owner, testOrganization, testScope, "bnd_1", TeamDocument{Name: "reviewer", Source: validReviewerSource("1.0.0", "Shared.\n")})
	if _, _, err := owner.Unadopt(testOrganization, testScope); err != nil {
		t.Fatal(err)
	}
	if string(selectionBytes(t, dataDir, "my-agent")) != string(raw) {
		t.Fatal("an unrelated selection's bytes moved")
	}
	detail, err := owner.Get("my-agent")
	if err != nil || detail.StateToken != token {
		t.Fatalf("an unrelated selection's token moved: %v %v", detail.StateToken, err)
	}
	if !strings.Contains(string(selectionBytes(t, dataDir, "command-reviewer")), `"released"`) {
		t.Fatal("the released selection must carry the mark on disk")
	}
}

func TestPlanTeamRefusesTwoDocumentsWithOneIDAndAnUnparseableOne(t *testing.T) {
	owner, _ := newTestOwner(t)
	source := validReviewerSource("1.0.0", "Shared.\n")
	if _, err := owner.PlanTeam(testOrganization, testScope, []TeamDocument{{Name: "a", Source: source}, {Name: "b", Source: source}}); ProblemCode(err) != "invalid_profile" {
		t.Fatalf("two documents with one id must be refused, got %v", err)
	}
	if _, err := owner.PlanTeam(testOrganization, testScope, []TeamDocument{{Name: "bad", Source: []byte("not a profile")}}); err == nil {
		t.Fatal("an unparseable document must be refused")
	}
	if _, err := owner.PlanTeam(testOrganization, "team", nil); err == nil {
		t.Fatal("an unknown scope must be refused")
	}
}

// Red-team M7: one revision of an agent listed by two adoptions (the organization's
// bundle and a repository's) stays shared until the last of them lets it go. Un-adopting
// either one used to release it, and the survivor could not take it back.
func TestAProfileTwoAdoptionsListIsReleasedOnlyByTheLastOne(t *testing.T) {
	owner, _ := newTestOwner(t)
	const repositoryScope = "repository:repo_1"
	shared := validReviewerSource("1.0.0", "Shared.\n")
	adoptTeam(t, owner, testOrganization, testScope, "bnd_org", TeamDocument{Name: "reviewer", Source: shared})
	second := adoptTeam(t, owner, testOrganization, repositoryScope, "bnd_repo", TeamDocument{Name: "reviewer", Source: shared})
	if second.Documents[0].Outcome != TeamDocumentUnchanged {
		t.Fatalf("a second scope carrying the same revision lists it unchanged: %+v", second.Documents)
	}
	detail, err := owner.Get("command-reviewer")
	if err != nil {
		t.Fatal(err)
	}

	if _, found, err := owner.Unadopt(testOrganization, repositoryScope); err != nil || !found {
		t.Fatalf("unadopt the repository scope: %v %v", found, err)
	}
	origin, _, _ := owner.Provenance("command-reviewer")
	if origin.Released != nil {
		t.Fatalf("un-adopting one of two adoptions released an agent the other still lists: %+v", origin.Released)
	}
	if adoption, ok, _ := owner.AdoptionOfRevision("command-reviewer", detail.Current.SourceDigest, detail.Current.BundleDigest); !ok || adoption.Scope != testScope {
		t.Fatalf("the surviving adoption still owns the revision: %+v %v", adoption, ok)
	}

	if _, found, err := owner.Unadopt(testOrganization, testScope); err != nil || !found {
		t.Fatalf("unadopt the organization scope: %v %v", found, err)
	}
	origin, _, _ = owner.Provenance("command-reviewer")
	if origin.Released == nil || origin.Released.Scope != testScope {
		t.Fatalf("the last adoption to let go releases the agent: %+v", origin)
	}
}
