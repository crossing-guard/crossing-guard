package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Red-team M9: the adoption/place seam with real objects on both sides. Every other
// test stands a fake on one side (fakeAdoptions for the hosts, recordingPlaces for the
// linker), so nothing failed if wireTeamRestOfRelease connected the wrong things. Here
// a real profilefs adoption (through the linker's own pull and Adopt), a real managed
// host and a real review host are connected by wireTeamRestOfRelease alone, and:
//
//   - a place bound on an adopted agent records the adoption's key, asked through
//     lazyPlaceAdoptions of the linker's own owner;
//   - a handoff built for a session in that repository names the agent in agents[];
//   - Un-adopt, through the route's handler, turns the managed place off in the store
//     and makes the review host re-read its binding (hostAdoptionPlaces), so the
//     reviewer stops at once and not at the next restart.
func TestAdoptionPlaceSeamWithRealOwnersOnBothSides(t *testing.T) {
	rig := newAdoptionRig(t)
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"managed-fixture": managedDynamicFixtureDriver{commandFor: func(ChatRequest) string { return jsonTextCommand("ok") }}}
	t.Cleanup(func() { chatDrivers = original })
	owner := rig.owner()

	// Both sides, real: the hosts are built over the linker's own store and owner.
	taskIndex, err := store.Open(filepath.Join(rig.dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taskIndex.Close() })
	tasks := NewTaskApplicationService(taskStoreRepository{index: taskIndex}, NewTaskExecutionRegistry(), NewTaskSubscriberHub(),
		registeredTaskRuntime, func(string, string) (bool, error) { return true, nil }, rig.dir)
	managed, err := newOrchestrationManagedHost(rig.ix, owner, tasks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(managed.close)
	review, err := newOrchestrationReviewHost(rig.ix, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(review.close)
	priorAgents := handoffAgentsOf
	t.Cleanup(func() { handoffAgentsOf = priorAgents })
	wireTeamRestOfRelease(managed, review, owner)

	// A real adoption of two shared agents: a helper and a reviewer.
	key := testOrgKey(t)
	helperSource, err := profilefs.Duplicate(helperAgentProfileSource(), "team-helper", "Team helper", "Shared helper.")
	if err != nil {
		t.Fatal(err)
	}
	bundle := newTestBundle("bnd_seam_r1", 1, testProfileDocument("team-helper/PROFILE.md", helperSource),
		testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "Shared reviewer.")))
	rig.serve(served{bundle.sign(t, key), key})
	offerOf(t, rig.pull(), bundle.ID)
	rig.adopt("organization", bundle.ID)
	adoptionKey := store.AdoptionKey("org_1", "organization")

	repo := v1TestRepo(t)
	helper, err := owner.Get("team-helper")
	if err != nil {
		t.Fatal(err)
	}
	place, err := managed.putBinding(managedBindingCommand{BindingID: "agent-shared", ProfileID: "team-helper",
		ProfileSourceDigest: helper.Current.SourceDigest, ProfileBundleDigest: helper.Current.BundleDigest, ProjectRoot: repo,
		RouteID: testRouteID(managed, "managed-fixture", "", nil), ScopeRuntime: "managed-fixture",
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-shared")})
	if err != nil {
		t.Fatal(err)
	}
	if place.AdoptionKey != adoptionKey {
		t.Fatalf("a place on an adopted agent records the adoption's key through the wired seam: %q", place.AdoptionKey)
	}
	reviewer, err := owner.Get("team-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	reviewPlace, err := review.putBinding(reviewBindingCommand{ProfileID: "team-reviewer",
		ProfileSourceDigest: reviewer.Current.SourceDigest, ProfileBundleDigest: reviewer.Current.BundleDigest,
		RouteID: testInferenceRouteID(review, "http://127.0.0.1:1", "local-test-model"), TimeoutMS: 1000,
		ExpectedStateToken: store.ReviewBindingAbsentToken()})
	if err != nil {
		t.Fatal(err)
	}
	if reviewPlace.AdoptionKey != adoptionKey {
		t.Fatalf("the reviewer's place records the adoption's key: %q", reviewPlace.AdoptionKey)
	}
	reviewing := func() bool {
		review.mu.RLock()
		defer review.mu.RUnlock()
		return review.runtimeBinding != nil
	}
	if !reviewing() {
		t.Fatal("the review host runs its enabled binding")
	}

	// A handoff from a session in that repository names the shared agent.
	session := &SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: harvest.SessionSummary{Runtime: "managed-fixture", ID: "ses-sender", Cwd: repo}}}
	rec, _, _, err := buildHandoffDocument(handoffDocumentInput{ID: engine.NewTypedID(teamwire.HandoffIDPrefix), CreatedAt: time.Now().UTC().Format(time.RFC3339),
		DeviceID: engine.NewTypedID(engine.DeviceIDPrefix), Session: session, Checkout: handoffCheckout(session), Recipient: engine.NewTypedID("usr"),
		Title: "Carry on", Body: "What is left."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Agents) != 1 || rec.Agents[0].ProfileID != "team-helper" || rec.Agents[0].Name != "Team helper" ||
		rec.Agents[0].SourceDigest != helper.Current.SourceDigest || rec.Agents[0].BundleDigest != helper.Current.BundleDigest {
		t.Fatalf("the handoff's agents[] names the shared agent turned on for the repository, by reference: %+v (place root %q, checkout root %q)", rec.Agents, place.ProjectRoot, handoffCheckout(session).root)
	}

	// Un-adopt through the route: the place is off in the store, and the review host
	// re-read its binding.
	if answer := rig.post(handleTeamUnadopt, `{"scope":"organization"}`); answer.Code != 200 {
		t.Fatalf("unadopt: %d %s", answer.Code, answer.Body)
	}
	if after, found, err := rig.ix.ManagedBinding("agent-shared"); err != nil || !found || after.State == "enabled" {
		t.Fatalf("un-adopt turns the adoption's managed place off: %+v %v", after, err)
	}
	if after, found, err := rig.ix.ReviewBinding(); err != nil || !found || after.State == "enabled" {
		t.Fatalf("un-adopt turns the adoption's reviewer off: %+v %v", after, err)
	}
	if reviewing() {
		t.Fatal("the review host still runs a binding un-adopt turned off: it did not re-read it")
	}
	if agents := handoffAgentsOf(session); len(agents) != 0 {
		t.Fatalf("a place that is off names no shared agent: %+v", agents)
	}
}
