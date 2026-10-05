package daemon

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/modelroute"
	"crossing-guard/internal/rulebook"
	"crossing-guard/ruledoc"
	"crossing-guard/store"
)

// captureLog collects what the daemon logs for the length of a test. The buffer is a
// locked one: the daemon's background jobs log while the test reads, and the log
// package's own lock covers the writes only.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	prior := log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() { log.SetOutput(prior) })
	return buffer
}

// Red-team M1: route admission must not fail open in silence. A rulebook that cannot
// be read refuses a bind, typed, on both lanes; the preview lists it as a problem; a
// place bound while the rulebook could be read still runs, and the read error is
// logged every time.
func TestAnUnreadableRulebookRefusesTheBindAndIsNeverSilent(t *testing.T) {
	fixture := newRouteFixture(t)
	remote := mustRoute(t, fixture.host.routes, modelroute.Draft{Name: "Remote", Family: modelroute.FamilyRuntimeModel,
		Fields: modelroute.Fields{Runtime: "managed-fixture", Model: fixtureNonLocalModel}})
	inference := fixture.inferenceRoute(t, "Reviewer", "http://127.0.0.1:1", "local-test-model")
	placed := fixture.place(t, "agent-before", fixture.remote, remote.RouteID)
	reviewer := fixture.reviewer(t, inference.RouteID)

	logged := captureLog(t)
	fixture.host.routes.state.setPolicy(func(string) (*engine.Policy, error) {
		return nil, errors.New("rules.json: unexpected end of JSON input")
	})
	t.Cleanup(func() { fixture.host.routes.state.setPolicy(nil) })

	_, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-unread", ProfileID: fixture.remote.ProfileID,
		ProfileSourceDigest: fixture.remote.SourceDigest, ProfileBundleDigest: fixture.remote.BundleDigest,
		ProjectRoot: fixture.root, RouteID: remote.RouteID, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-unread")})
	var refusal *routeRefusal
	if !errors.As(err, &refusal) || refusal.Code != routeRefusalRulebook || !strings.Contains(refusal.Message, "the rulebook cannot be read") {
		t.Fatalf("a bind with an unreadable rulebook was not refused, typed: %v", err)
	}
	if _, found, _ := fixture.host.ix.ManagedBinding("agent-unread"); found {
		t.Fatal("a refused bind wrote the place")
	}
	_, err = fixture.review.putBinding(reviewBindingCommand{ProfileID: fixture.checker.ProfileID,
		ProfileSourceDigest: fixture.checker.SourceDigest, ProfileBundleDigest: fixture.checker.BundleDigest,
		RouteID: inference.RouteID, TimeoutMS: 1000, ExpectedStateToken: reviewer.StateToken})
	if !errors.As(err, &refusal) || refusal.Code != routeRefusalRulebook {
		t.Fatalf("the reviewer bind with an unreadable rulebook was not refused, typed: %v", err)
	}

	status, body := fixture.call(t, http.MethodPost, "/api/model-routes/preview", map[string]any{"route_id": remote.RouteID,
		"name": "Remote", "family": modelroute.FamilyRuntimeModel,
		"fields": map[string]any{"runtime": "managed-fixture", "model": fixtureNonLocalModel}})
	if status != http.StatusOK || !strings.Contains(body, `"code":"`+routeRefusalRulebook+`"`) || !strings.Contains(body, "unexpected end of JSON input") {
		t.Fatalf("the preview does not list the unreadable rulebook as a problem: %d %s", status, body)
	}

	// Run start keeps going for a place bound while the rulebook could be read.
	logged.Reset()
	if refused, _ := fixture.host.runStartRefusal(placed, fixture.compiled(t, fixture.remote)); refused != nil {
		t.Fatalf("an unreadable rulebook must not stop a run of a place already bound: %+v", refused)
	}
	if !strings.Contains(logged.String(), "unexpected end of JSON input") || !strings.Contains(logged.String(), "Remote") {
		t.Fatalf("the read error at run start was not logged: %q", logged.String())
	}
}

// Red-team M1: adoption records that cannot be read used to drop every team route rule
// with no error. The production loader now returns the error beside the user layer,
// and admission still decides over the rules that were read.
func TestUnreadableAdoptionRecordsReachAdmissionAsAnError(t *testing.T) {
	userRules := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(userRules, []byte(`{"rules":[{"id":"no-remote-routes","action":"deny","message":"local only","if":{"tag":"route:local","value":"false"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", userRules)
	layerStore := t.TempDir()
	layersPath, _, _ := rulebook.LayersPathsIn(layerStore)
	if err := os.MkdirAll(filepath.Dir(layersPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layersPath, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := layeredAdmissionPolicy(layerStore)(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "adoption records could not be read") {
		t.Fatalf("unreadable adoption records must be an error to admission: %v", err)
	}
	if policy == nil || len(policy.Rules) != 1 {
		t.Fatalf("the user layer that was read must still be returned: %+v", policy)
	}

	routes := &modelRoutes{state: &modelRouteState{}}
	routes.state.setPolicy(layeredAdmissionPolicy(layerStore))
	captureLog(t)
	admission, admitErr := routes.admit(modelroute.Route{RouteID: "rt_1", Name: "Remote", Family: modelroute.FamilyRuntimeModel}, false, "", "")
	if admitErr == nil || admission.Allowed || admission.Rule != "no-remote-routes" {
		t.Fatalf("admission must decide over the rules that were read and return the read error: %+v %v", admission, admitErr)
	}
}

// Red-team M2: a parked run's next attempt answers the same questions the first did.
// A rule written, or an adoption expired, while the run was parked stops the relaunch
// typed, with no second child task; the place stays on.
func TestAParkedRunRelaunchIsRefusedByAdmissionAndByAnAdoptionHold(t *testing.T) {
	cases := []struct {
		name, wantClass string
		arm             func(fixture agentHostFixture, key string, held map[string]string)
	}{
		{name: "admission", wantClass: modelroute.AdmissionRefusedCode, arm: func(fixture agentHostFixture, _ string, _ map[string]string) {
			fixture.host.routes.state.setPolicy(func(string) (*engine.Policy, error) {
				return &engine.Policy{Rules: []engine.Rule{{ID: "no-runtime-routes", Action: "deny", Message: "not admitted",
					If: engine.Predicate{Tag: engine.RouteFactPrefix + "family", Value: modelroute.FamilyRuntimeModel}}}}, nil
			})
		}},
		{name: "hold", wantClass: placeHoldExpired, arm: func(_ agentHostFixture, key string, held map[string]string) {
			held[key] = placeHoldExpired
		}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			pinProviderRetryPolicy(t, immediateRetryPolicy)
			fixture := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
				if strings.Contains(request.Prompt, "root question") {
					return jsonTextCommand("Root done.")
				}
				return classifiedFailureCommand
			}})
			t.Cleanup(func() { fixture.host.routes.state.setPolicy(nil) })
			const key = "org_1\x1forganization"
			preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
			adoptions := fakeAdoptions{adopted: map[string]string{preview.ProfileID: key}, held: map[string]string{}, movable: map[string]bool{}}
			fixture.host.setPlaceAdoptions(adoptions)
			fixture.bindHelper(t, "agent-parked", 10, false, nil, store.ManagedLimits{})
			rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-parked-" + item.name, Cwd: fixture.root}, "parked-root-"+item.name)
			if err != nil {
				t.Fatal(err)
			}
			wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
			parked := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "parked" })[0]

			item.arm(fixture, key, adoptions.held)
			fixture.host.relaunchParkedRunsOnce()
			ended := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State != "parked" })[0]
			if ended.State != "failed" || ended.ErrorClass != item.wantClass {
				t.Fatalf("the relaunch was not refused %s: state=%s class=%s recovery=%q", item.wantClass, ended.State, ended.ErrorClass, ended.Recovery)
			}
			if attempts := providerOutageLedgerFrom(ended.Detail).Attempts; len(attempts) != 1 || ended.ChildTaskID != parked.ChildTaskID {
				t.Fatalf("a refused relaunch started another attempt: %+v child=%s", attempts, ended.ChildTaskID)
			}
			if still, _, _ := fixture.host.ix.ManagedBinding("agent-parked"); still.State != "enabled" {
				t.Fatalf("a refused relaunch turned the place off: %+v", still)
			}
		})
	}
}

// Red-team Low 6: the start-up pass stopped at the first route whose places could not
// be repaired, skipping every later route and the route_missing marks. It now goes on
// past the failure, still marks the places whose route is gone, and returns the
// failure so the pass is reported as unfinished.
func TestStartUpPassGoesOnPastARouteThatFailsAndStillMarksMissingRoutes(t *testing.T) {
	fixture := newRouteFixture(t)
	failing := fixture.localRoute(t, "A failing")
	healthy := fixture.localRoute(t, "B healthy")
	gone := fixture.localRoute(t, "C gone")
	fixture.place(t, "agent-failing", fixture.helper, failing.RouteID)
	fixture.place(t, "agent-healthy", fixture.helper, healthy.RouteID)
	fixture.place(t, "agent-gone", fixture.helper, gone.RouteID)
	removeRouteFile(t, fixture.host.routes, gone.RouteID)

	applied := []string{}
	report, err := reconcileModelRoutesWith(fixture.host.ix, fixture.host.routes, func(revision store.RouteRevision, now int64) (store.RouteRevisionApplied, error) {
		if revision.RouteID == failing.RouteID {
			return store.RouteRevisionApplied{}, errors.New("database is locked")
		}
		applied = append(applied, revision.RouteID)
		return fixture.host.ix.ApplyRouteRevision(revision, reviewPathIdentity, now)
	})
	if err == nil || !strings.Contains(err.Error(), failing.RouteID) || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("the failure must come back, naming the route: %v", err)
	}
	if len(applied) != 1 || applied[0] != healthy.RouteID {
		t.Fatalf("the pass must go on to the routes after the failing one: %v", applied)
	}
	if len(report.RouteMissing) != 1 || report.RouteMissing[0] != "agent-gone" {
		t.Fatalf("places whose route is gone are still marked: %+v", report)
	}
	if marked, _, _ := fixture.host.ix.ManagedBinding("agent-gone"); marked.RouteProblem != store.RouteProblemMissing {
		t.Fatalf("agent-gone was not marked route_missing: %+v", marked)
	}
	if kept, _, _ := fixture.host.ix.ManagedBinding("agent-failing"); kept.RouteProblem != "" {
		t.Fatalf("a place whose route file is there is not missing a route: %+v", kept)
	}
}

// An adopted layer whose staged rule document no longer parses must not let admission
// pass as if the team had no route rules (the confirming review's N-1).
func TestAnUnloadableAdoptedLayerReachesAdmissionAsAnError(t *testing.T) {
	userRules := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(userRules, []byte(`{"rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", userRules)
	layerStore := t.TempDir()
	raw := []byte(`{"rules":[{"id":"team-local-only","action":"deny","message":"local only","if":{"tag":"route:local","value":"false"}}]}`)
	digest := ruledoc.ContentDigest(raw)
	staged, err := rulebook.StageLayer(layerStore, digest, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := rulebook.AdoptBundle(layerStore, rulebook.AdoptedBundle{OrganizationID: "org_1", OrganizationName: "Acme",
		Scope: rulebook.ScopeOrganization, BundleID: "bnd_1", Revision: 1, FailureMode: rulebook.FailOpen,
		ExpiresAt: time.Now().Add(time.Hour), RulebookDigest: digest, SignedDigest: "sha256:s"}); err != nil {
		t.Fatal(err)
	}
	load := layeredAdmissionPolicy(layerStore)
	if policy, err := load(t.TempDir()); err != nil || len(policy.Rules) != 1 {
		t.Fatalf("a layer that loads brings its route rule and no error: %+v %v", policy, err)
	}
	if err := os.WriteFile(staged, []byte(`{not a rule document`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "unloadable") {
		t.Fatalf("an unloadable adopted layer must be an error to admission: %v", err)
	}
}
