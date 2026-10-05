package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/modelroute"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// routeFixture is a managed host, a review host and the route API over one data
// directory — the shape the daemon runs them in.
type routeFixture struct {
	agentHostFixture
	review  *orchestrationReviewHost
	api     *modelRouteAPI
	mux     *http.ServeMux
	helper  profilefs.Preview
	remote  profilefs.Preview
	checker profilefs.Preview
}

// remoteHelperSource is the helper profile with a destination that allows a remote
// route, under its own id, so one test can place an agent on a route that leaves the
// machine.
func remoteHelperSource() []byte {
	source := strings.Replace(string(helperAgentProfileSource()), "id: design-helper", "id: remote-helper", 1)
	return []byte(strings.Replace(source, "    - managed-turn\n", allowRemoteDestination, 1))
}

func rootThenClaimDriver() managedDynamicFixtureDriver {
	return managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		return jsonTextCommand(helperClaimV2)
	}}
}

func newRouteFixture(t *testing.T) routeFixture {
	t.Helper()
	fixture := routeFixture{agentHostFixture: newAgentHostFixture(t, rootThenClaimDriver())}
	fixture.helper = selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	fixture.remote = selectManagedProfile(t, fixture.owner, remoteHelperSource())
	fixture.checker = selectManagedProfile(t, fixture.owner, compatibleReviewProfileSource())
	review, err := newOrchestrationReviewHost(fixture.host.ix, fixture.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(review.close)
	fixture.review = review
	fixture.api = &modelRouteAPI{routes: fixture.host.routes, profiles: fixture.owner,
		index:  func() *store.Index { return fixture.host.ix },
		review: func() *orchestrationReviewHost { return fixture.review }}
	fixture.mux = http.NewServeMux()
	registerOrchestrationManagedRoutes(fixture.mux, fixture.host, fixture.owner)
	registerOrchestrationReviewRoutes(fixture.mux, fixture.review, fixture.owner)
	registerOrchestrationRosterRoutes(fixture.mux, rosterSources{profiles: fixture.owner, managed: fixture.host, review: fixture.review})
	registerModelRouteRoutes(fixture.mux, fixture.root, fixture.owner, fixture.host, fixture.review)
	return fixture
}

func (fixture routeFixture) call(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	fixture.mux.ServeHTTP(response, httptest.NewRequest(method, path, bytes.NewReader(raw)))
	return response.Code, response.Body.String()
}

// place turns the helper on for the fixture's repository on a route.
func (fixture routeFixture) place(t *testing.T, id string, preview profilefs.Preview, routeID string, chain ...bindingRouteEntry) store.ManagedBinding {
	t.Helper()
	binding, err := fixture.host.putBinding(managedBindingCommand{BindingID: id, ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		RouteID: routeID, Routes: chain, ScopeRuntime: "managed-fixture",
		ExpectedStateToken: store.ManagedBindingAbsentToken(id)})
	if err != nil {
		t.Fatalf("place %s: %v", id, err)
	}
	return binding
}

func (fixture routeFixture) reviewer(t *testing.T, routeID string) store.ReviewBinding {
	t.Helper()
	binding, err := fixture.review.putBinding(reviewBindingCommand{ProfileID: fixture.checker.ProfileID,
		ProfileSourceDigest: fixture.checker.SourceDigest, ProfileBundleDigest: fixture.checker.BundleDigest,
		RouteID: routeID, TimeoutMS: 1000, ExpectedStateToken: store.ReviewBindingAbsentToken()})
	if err != nil {
		t.Fatalf("reviewer: %v", err)
	}
	return binding
}

func (fixture routeFixture) localRoute(t *testing.T, name string) modelroute.Route {
	t.Helper()
	return mustRoute(t, fixture.host.routes, modelroute.Draft{Name: name, Family: modelroute.FamilyRuntimeModel,
		Fields: modelroute.Fields{Runtime: "managed-fixture"}})
}

func (fixture routeFixture) inferenceRoute(t *testing.T, name, endpoint, model string) modelroute.Route {
	t.Helper()
	return mustRoute(t, fixture.host.routes, modelroute.Draft{Name: name, Family: modelroute.FamilyInference,
		Fields: modelroute.Fields{Endpoint: endpoint, Model: model}})
}

// edit changes a route through the API's own select path.
func (fixture routeFixture) edit(t *testing.T, route modelroute.Route, name string, fields modelroute.Fields) (modelRouteSelectResponse, error) {
	t.Helper()
	draft := modelroute.Draft{RouteID: route.RouteID, Name: name, Family: route.Family, Fields: fields}
	preview, err := fixture.host.routes.owner.Preview(draft)
	if err != nil {
		return modelRouteSelectResponse{}, err
	}
	return fixture.api.selectRoute(modelroute.SelectCommand{Draft: draft, ExpectedPreviewDigest: preview.PreviewDigest,
		ExpectedStateToken: preview.StateToken, Confirmed: true})
}

// Criterion 56 (input half): a binding write that sends runtime, model or endpoint is
// refused by name — on every binding write route, in every position a request can carry
// one — even when it also sends a valid route_id.
func TestBindingWritesRefuseTypedModelFieldsByName(t *testing.T) {
	fixture := newRouteFixture(t)
	route := fixture.localRoute(t, "Local")
	reviewRoute := fixture.inferenceRoute(t, "Reviewer", "http://127.0.0.1:1", "local-test-model")
	base := func() map[string]any {
		return map[string]any{"profile_id": fixture.helper.ProfileID, "profile_source_digest": fixture.helper.SourceDigest,
			"profile_bundle_digest": fixture.helper.BundleDigest, "project_root": fixture.root, "route_id": route.RouteID,
			"mode": "", "granted_authority": []string{}, "scope_runtime": "managed-fixture"}
	}
	put := func(extra map[string]any) (int, string) {
		body := base()
		body["expected_state_token"], body["confirmed"] = store.ManagedBindingAbsentToken("agent-typed"), true
		for key, value := range extra {
			body[key] = value
		}
		return fixture.call(t, http.MethodPut, "/api/orchestration/agents/agent-typed", body)
	}
	for field, value := range map[string]any{"runtime": "managed-fixture", "model": "some-model",
		"endpoint": "http://127.0.0.1:1", "thinking_effort": map[string]any{"kind": "inherit"}} {
		status, body := put(map[string]any{field: value})
		if status != http.StatusBadRequest || !strings.Contains(body, field+" is not accepted on a binding write") {
			t.Errorf("PUT with %s: %d %s", field, status, body)
		}
	}
	if status, body := put(map[string]any{"routes": []map[string]any{{"route_id": route.RouteID, "runtime": "managed-fixture"}}}); status != http.StatusBadRequest ||
		!strings.Contains(body, "routes[0].runtime is not accepted") {
		t.Errorf("PUT with a typed chain entry: %d %s", status, body)
	}
	if _, found, _ := fixture.host.ix.ManagedBinding("agent-typed"); found {
		t.Fatal("a refused write created the place")
	}

	batch := func(change map[string]any) (int, string) {
		change["binding_id"], change["expected_state_token"] = "agent-typed", store.ManagedBindingAbsentToken("agent-typed")
		return fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch",
			map[string]any{"confirmed": true, "changes": []map[string]any{change}})
	}
	typedPlace := base()
	typedPlace["model"] = "some-model"
	if status, body := batch(map[string]any{"op": "create", "place": typedPlace}); status != http.StatusBadRequest ||
		!strings.Contains(body, "place.model is not accepted") {
		t.Errorf("batch create with a typed model: %d %s", status, body)
	}
	if status, body := batch(map[string]any{"op": "update", "set": map[string]any{"model": map[string]any{"route_id": route.RouteID, "runtime": "x"}}}); status != http.StatusBadRequest ||
		!strings.Contains(body, "set.model.runtime is not accepted") {
		t.Errorf("batch update with a typed runtime: %d %s", status, body)
	}
	if status, body := batch(map[string]any{"op": "update", "set": map[string]any{"fallback": []map[string]any{{"route_id": route.RouteID, "model": "x"}}}}); status != http.StatusBadRequest ||
		!strings.Contains(body, "set.fallback[0].model is not accepted") {
		t.Errorf("batch update with a typed fallback: %d %s", status, body)
	}

	review := map[string]any{"profile_id": fixture.checker.ProfileID, "profile_source_digest": fixture.checker.SourceDigest,
		"profile_bundle_digest": fixture.checker.BundleDigest, "route_id": reviewRoute.RouteID, "timeout_ms": 1000,
		"runtime_filter": "", "expected_state_token": store.ReviewBindingAbsentToken(), "confirmed": true}
	for _, field := range []string{"endpoint", "model", "runtime"} {
		typed := map[string]any{field: "value"}
		for key, value := range review {
			typed[key] = value
		}
		status, body := fixture.call(t, http.MethodPut, "/api/orchestration/reviews/binding", typed)
		if status != http.StatusBadRequest || !strings.Contains(body, routeRefusalTypedField) || !strings.Contains(body, field+" is not accepted") {
			t.Errorf("review PUT with %s: %d %s", field, status, body)
		}
	}
	if _, found, _ := fixture.host.ix.ReviewBinding(); found {
		t.Fatal("a refused write created the reviewer")
	}

	// The same writes with route_id alone are accepted, and a write with no route is
	// refused as such.
	if status, body := put(nil); status != http.StatusOK {
		t.Fatalf("PUT with route_id: %d %s", status, body)
	}
	if status, body := fixture.call(t, http.MethodPut, "/api/orchestration/reviews/binding", review); status != http.StatusOK {
		t.Fatalf("review PUT with route_id: %d %s", status, body)
	}
	noRoute := base()
	delete(noRoute, "route_id")
	noRoute["expected_state_token"], noRoute["confirmed"] = store.ManagedBindingAbsentToken("agent-none"), true
	if status, body := fixture.call(t, http.MethodPut, "/api/orchestration/agents/agent-none", noRoute); status != http.StatusBadRequest ||
		!strings.Contains(body, "route_id is required") {
		t.Errorf("PUT with no route: %d %s", status, body)
	}
}

// §5.4 check 1: the family fits the profile.
func TestRouteFamilyMustFitThePlace(t *testing.T) {
	fixture := newRouteFixture(t)
	local := fixture.localRoute(t, "Local")
	inference := fixture.inferenceRoute(t, "Reviewer", "http://127.0.0.1:1", "local-test-model")
	_, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-wrong", ProfileID: fixture.helper.ProfileID,
		ProfileSourceDigest: fixture.helper.SourceDigest, ProfileBundleDigest: fixture.helper.BundleDigest,
		ProjectRoot: fixture.root, RouteID: inference.RouteID, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-wrong")})
	var refusal *routeRefusal
	if !errors.As(err, &refusal) || refusal.Code != routeRefusalFamily {
		t.Fatalf("an inference route was bound to a managed place: %v", err)
	}
	_, err = fixture.review.putBinding(reviewBindingCommand{ProfileID: fixture.checker.ProfileID,
		ProfileSourceDigest: fixture.checker.SourceDigest, ProfileBundleDigest: fixture.checker.BundleDigest,
		RouteID: local.RouteID, TimeoutMS: 1000, ExpectedStateToken: store.ReviewBindingAbsentToken()})
	if !errors.As(err, &refusal) || refusal.Code != routeRefusalFamily {
		t.Fatalf("a runtime-model route was bound to the reviewer: %v", err)
	}
}

// Criterion 57: a route is created, picked on a place, edited — every place that uses it
// follows in one step, the reviewer's request-path identity included — and cannot be
// deleted while used. Criterion 82: rename keeps the id and every place.
func TestRouteEditMovesEveryPlaceAndDeleteIsRefusedWhileUsed(t *testing.T) {
	fixture := newRouteFixture(t)
	shared := mustRoute(t, fixture.host.routes, modelroute.Draft{Name: "Shared", Family: modelroute.FamilyRuntimeModel,
		Fields: modelroute.Fields{Runtime: "managed-fixture", Model: "model-a"}})
	other := fixture.localRoute(t, "Other")
	first := fixture.place(t, "agent-one", fixture.remote, shared.RouteID)
	second := fixture.place(t, "agent-two", fixture.remote, other.RouteID, bindingRouteEntry{RouteID: shared.RouteID})
	if first.Model != "model-a" || first.RouteRevisionDigest != shared.RevisionDigest || second.Routes[0].Model != "model-a" {
		t.Fatalf("bind did not write the reference and the resolved copy: %+v %+v", first, second)
	}

	moved, err := fixture.edit(t, shared, "Shared", modelroute.Fields{Runtime: "managed-fixture", Model: "model-b"})
	if err != nil || !moved.Changed || len(moved.MovedPlaces) != 2 {
		t.Fatalf("edit = %+v, %v", moved, err)
	}
	one, _, _ := fixture.host.ix.ManagedBinding("agent-one")
	two, _, _ := fixture.host.ix.ManagedBinding("agent-two")
	if one.Model != "model-b" || one.RouteRevisionDigest != moved.Route.RevisionDigest || one.StateToken == first.StateToken ||
		two.Routes[0].Model != "model-b" || two.Model != "" || two.StateToken == second.StateToken {
		t.Fatalf("places did not follow the route: %+v %+v", one, two)
	}

	renamed, err := fixture.edit(t, shared, "Shared renamed", moved.Route.Fields)
	if err != nil || renamed.Route.RouteID != shared.RouteID || renamed.Route.Name != "Shared renamed" {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	afterRename, _, _ := fixture.host.ix.ManagedBinding("agent-one")
	if afterRename.RouteID != shared.RouteID || afterRename.RouteRevisionDigest != renamed.Route.RevisionDigest || afterRename.Model != "model-b" {
		t.Fatalf("rename lost the place: %+v", afterRename)
	}

	status, body := fixture.call(t, http.MethodDelete, "/api/model-routes/"+shared.RouteID,
		map[string]any{"expected_state_token": renamed.Route.StateToken, "confirmed": true})
	if status != http.StatusConflict || !strings.Contains(body, modelroute.CodeInUse) ||
		!strings.Contains(body, "remote-helper in "+filepath.Base(fixture.root)) {
		t.Fatalf("delete of a used route: %d %s", status, body)
	}
	status, body = fixture.call(t, http.MethodGet, "/api/model-routes", nil)
	var listed modelRouteListResponse
	if err := json.Unmarshal([]byte(body), &listed); status != http.StatusOK || err != nil || len(listed.Routes) != 2 {
		t.Fatalf("list: %d %s %v", status, body, err)
	}
	for _, route := range listed.Routes {
		if route.RouteID == shared.RouteID && (len(route.Places) != 2 || route.Locality != routeOnThisMachine || !route.Local) {
			t.Fatalf("listing lacks the places or the locality: %+v", route)
		}
	}
}

func TestRouteEditRecomputesTheReviewersRequestPath(t *testing.T) {
	fixture := newRouteFixture(t)
	route := fixture.inferenceRoute(t, "Reviewer", "http://127.0.0.1:1", "local-test-model")
	before := fixture.reviewer(t, route.RouteID)
	moved, err := fixture.edit(t, route, "Reviewer", modelroute.Fields{Endpoint: "http://127.0.0.1:2", Model: "other-model"})
	if err != nil || len(moved.MovedPlaces) != 1 {
		t.Fatalf("edit = %+v, %v", moved, err)
	}
	after, _, _ := fixture.host.ix.ReviewBinding()
	kind, digest, err := reviewPathIdentity(after)
	if err != nil || after.Endpoint != "http://127.0.0.1:2" || after.Model != "other-model" ||
		after.RequestPathKind != kind || after.RequestPathDigest != digest || after.RequestPathDigest == before.RequestPathDigest ||
		after.StateToken == before.StateToken {
		t.Fatalf("the reviewer's request-path identity was not recomputed: before=%+v after=%+v (%v)", before, after, err)
	}
	// The host accepts the stored identity (it refuses a binding whose identity does
	// not match), and its running cache already reviews on the new endpoint.
	if _, err := fixture.review.resolveBinding(after); err != nil {
		t.Fatalf("the host refuses the binding the route edit wrote: %v", err)
	}
	fixture.review.mu.RLock()
	running := fixture.review.runtimeBinding
	fixture.review.mu.RUnlock()
	if running == nil || running.path.Endpoint != "http://127.0.0.1:2" || running.record.StateToken != after.StateToken {
		t.Fatalf("the running reviewer did not follow the route: %+v", running)
	}
	if _, err := fixture.review.disableBinding(before.StateToken); !errors.Is(err, store.ErrReviewBindingConflict) {
		t.Fatalf("a sheet holding the old state token was accepted: %v", err)
	}
}

// §5.4 check 2: an edit that would make any place violate its profile is refused,
// naming the places, and nothing is written.
func TestRouteEditThatWouldBreakAPlaceIsRefusedNamingIt(t *testing.T) {
	fixture := newRouteFixture(t)
	route := fixture.localRoute(t, "Local")
	placed := fixture.place(t, "agent-local", fixture.helper, route.RouteID) // design-helper is local-only
	_, err := fixture.edit(t, route, "Local", modelroute.Fields{Runtime: "managed-fixture", Model: fixtureNonLocalModel})
	var refusal *routeRefusal
	if !errors.As(err, &refusal) || refusal.Code != routeRefusalPlaces || len(refusal.Places) != 1 ||
		!strings.Contains(refusal.Places[0], "design-helper in ") {
		t.Fatalf("an edit that breaks a local-only place was not refused naming it: %v", err)
	}
	unchanged, err := fixture.host.routes.owner.Get(route.RouteID)
	if err != nil || unchanged.RevisionDigest != route.RevisionDigest {
		t.Fatalf("a refused edit wrote a revision: %+v, %v", unchanged, err)
	}
	if still, _, _ := fixture.host.ix.ManagedBinding("agent-local"); still.StateToken != placed.StateToken {
		t.Fatalf("a refused edit moved the place: %+v", still)
	}
	status, body := fixture.call(t, http.MethodPost, "/api/model-routes/preview", map[string]any{"route_id": route.RouteID,
		"name": "Local", "family": modelroute.FamilyRuntimeModel,
		"fields": map[string]any{"runtime": "managed-fixture", "model": fixtureNonLocalModel}})
	if status != http.StatusOK || !strings.Contains(body, routeRefusalPlaces) || !strings.Contains(body, routeLeavesThisMachine) {
		t.Fatalf("preview does not show the problem and the locality: %d %s", status, body)
	}
}

// Criterion 81: a kill between the route file write and the binding update is repaired
// at the next start, and the reviewer's request-path identity matches. Fail: a place
// sheet holding the old state token is refused.
func TestRouteEditCrashBetweenFileAndStoreIsRepairedAtStart(t *testing.T) {
	fixture := newRouteFixture(t)
	managedRoute := mustRoute(t, fixture.host.routes, modelroute.Draft{Name: "Shared", Family: modelroute.FamilyRuntimeModel,
		Fields: modelroute.Fields{Runtime: "managed-fixture", Model: "model-a"}})
	reviewRoute := fixture.inferenceRoute(t, "Reviewer", "http://127.0.0.1:1", "local-test-model")
	placed := fixture.place(t, "agent-one", fixture.remote, managedRoute.RouteID)
	reviewer := fixture.reviewer(t, reviewRoute.RouteID)

	// The crash: the store half of the edit never runs.
	original := applyRouteRevisionAfterFile
	applyRouteRevisionAfterFile = func(*store.Index, modelroute.Route, int64) (store.RouteRevisionApplied, error) {
		return store.RouteRevisionApplied{}, errors.New("killed between the file and the transaction")
	}
	t.Cleanup(func() { applyRouteRevisionAfterFile = original })
	if _, err := fixture.edit(t, managedRoute, "Shared", modelroute.Fields{Runtime: "managed-fixture", Model: "model-b"}); !modelroute.IsCode(err, modelroute.CodeStorage) {
		t.Fatalf("a half-applied edit reported success: %v", err)
	}
	if _, err := fixture.edit(t, reviewRoute, "Reviewer", modelroute.Fields{Endpoint: "http://127.0.0.1:2", Model: "other-model"}); !modelroute.IsCode(err, modelroute.CodeStorage) {
		t.Fatalf("a half-applied edit reported success: %v", err)
	}
	applyRouteRevisionAfterFile = original
	stale, _, _ := fixture.host.ix.ManagedBinding("agent-one")
	if stale.Model != "model-a" || stale.StateToken != placed.StateToken {
		t.Fatalf("the simulated crash still moved the place: %+v", stale)
	}

	report, err := reconcileModelRoutes(fixture.host.ix, fixture.host.routes)
	if err != nil || len(report.BindingsRepaired) != 1 || !report.ReviewRepaired {
		t.Fatalf("start-up pass = %+v, %v", report, err)
	}
	repaired, _, _ := fixture.host.ix.ManagedBinding("agent-one")
	currentRoute, _ := fixture.host.routes.owner.Get(managedRoute.RouteID)
	if repaired.Model != "model-b" || repaired.RouteRevisionDigest != currentRoute.RevisionDigest || repaired.StateToken == placed.StateToken {
		t.Fatalf("the place was not repaired to the route file: %+v", repaired)
	}
	repairedReview, _, _ := fixture.host.ix.ReviewBinding()
	kind, digest, _ := reviewPathIdentity(repairedReview)
	if repairedReview.Endpoint != "http://127.0.0.1:2" || repairedReview.RequestPathKind != kind || repairedReview.RequestPathDigest != digest {
		t.Fatalf("the reviewer's request-path identity does not match after repair: %+v", repairedReview)
	}
	if _, err := fixture.review.resolveBinding(repairedReview); err != nil {
		t.Fatalf("the host refuses the repaired reviewer: %v", err)
	}
	// Fail half: a sheet holding the pre-edit token is refused.
	if _, err := fixture.host.ix.DisableManagedBinding("agent-one", placed.StateToken, time.Now().Unix()); !errors.Is(err, store.ErrManagedBindingConflict) {
		t.Fatalf("the old place token still writes: %v", err)
	}
	if _, err := fixture.host.ix.DisableReviewBinding(reviewer.StateToken, time.Now().Unix()); !errors.Is(err, store.ErrReviewBindingConflict) {
		t.Fatalf("the old reviewer token still writes: %v", err)
	}
	// Pure and idempotent: a second pass changes nothing.
	again, err := reconcileModelRoutes(fixture.host.ix, fixture.host.routes)
	if err != nil || len(again.BindingsRepaired) != 0 || again.ReviewRepaired || again.RoutesCreated != 0 || again.BindingsMigrated != 0 {
		t.Fatalf("second pass = %+v, %v", again, err)
	}
	if same, _, _ := fixture.host.ix.ManagedBinding("agent-one"); same.StateToken != repaired.StateToken {
		t.Fatal("an idempotent pass minted a new state token")
	}
}

// Criterion 58: every existing managed binding, chain entry and the review binding
// resolves to a route with the stored values and runs as before. Fail: a binding the
// pass cannot write keeps running with migration_failed.
func TestMigrationGivesEveryTypedBindingARouteWithItsStoredValues(t *testing.T) {
	fixture := newRouteFixture(t)
	effort := &store.ThinkingEffort{Kind: "level", Value: "high"}
	legacy := func(id, runtime, model string, effort *store.ThinkingEffort, chain ...store.ManagedRoute) store.ManagedBinding {
		binding := store.ManagedBinding{BindingID: id, State: "enabled", Role: "helper", ProjectRoot: fixture.root,
			ProfileID: fixture.remote.ProfileID, ProfileSourceDigest: fixture.remote.SourceDigest,
			ProfileBundleDigest: fixture.remote.BundleDigest, Runtime: runtime, Model: model, ThinkingEffort: effort,
			Authority: []string{}, AllowedProfiles: []store.ManagedProfileRef{}, Routes: chain}
		saved, err := fixture.host.ix.PutManagedBinding(binding, store.ManagedBindingAbsentToken(id), 1)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	plain := legacy("legacy-plain", "managed-fixture", "model-a", nil,
		store.ManagedRoute{Runtime: "managed-fixture", Model: "model-c", Mode: ""})
	withEffort := legacy("legacy-effort", "managed-fixture", "model-a", effort)
	twin := legacy("legacy-twin", "managed-fixture", "model-a", nil)
	broken := legacy("legacy-broken", "not a runtime name", "model-a", nil)
	// The reviewer as a build before routes stored it: bound through the host (so its
	// limits and instruction digest are the profile's), then stripped of its route.
	seed := fixture.inferenceRoute(t, "Seed", "http://127.0.0.1:11434", "local-test-model")
	reviewRow := fixture.reviewer(t, seed.RouteID)
	reviewRow.RouteID, reviewRow.RouteRevisionDigest = "", ""
	reviewRow, err := fixture.host.ix.PutReviewBinding(reviewRow, reviewRow.StateToken, 1)
	if err != nil {
		t.Fatal(err)
	}

	report, err := reconcileModelRoutes(fixture.host.ix, fixture.host.routes)
	if err != nil || report.BindingsMigrated != 4 || len(report.MigrationFailed) != 1 || report.MigrationFailed[0] != "legacy-broken" {
		t.Fatalf("migration report = %+v, %v", report, err)
	}
	read := func(id string) store.ManagedBinding {
		binding, found, err := fixture.host.ix.ManagedBinding(id)
		if err != nil || !found {
			t.Fatalf("%s: %v %v", id, found, err)
		}
		return binding
	}
	migratedPlain, migratedEffort, migratedTwin := read("legacy-plain"), read("legacy-effort"), read("legacy-twin")
	if migratedPlain.RouteID == "" || migratedPlain.Routes[0].RouteID == "" || migratedPlain.RouteProblem != "" ||
		migratedPlain.Runtime != plain.Runtime || migratedPlain.Model != plain.Model || migratedPlain.Routes[0].Model != "model-c" ||
		migratedPlain.State != "enabled" {
		t.Fatalf("migrated binding changed what it runs with, or has no route: %+v", migratedPlain)
	}
	if migratedTwin.RouteID != migratedPlain.RouteID {
		t.Fatalf("two bindings with equal fields got different routes: %s %s", migratedTwin.RouteID, migratedPlain.RouteID)
	}
	if migratedEffort.RouteID == migratedPlain.RouteID || migratedEffort.ThinkingEffort == nil || *migratedEffort.ThinkingEffort != *withEffort.ThinkingEffort {
		t.Fatalf("a binding with another effort shares a route or lost its effort: %+v", migratedEffort)
	}
	_ = twin
	plainRoute, err := fixture.host.routes.owner.Get(migratedPlain.RouteID)
	effortRoute, effortErr := fixture.host.routes.owner.Get(migratedEffort.RouteID)
	// Names come from the runtime's and model's labels; the second route with the same
	// labels (the pass visits places in store order) takes the suffix.
	base := chatRuntimeLabel("managed-fixture") + " · model-a"
	names := map[string]bool{plainRoute.Name: true, effortRoute.Name: true}
	if err != nil || effortErr != nil || plainRoute.MigratedAt == "" || !names[base] || !names[base+" (2)"] {
		t.Fatalf("migrated routes are not named from the labels with a suffix on a clash: %q %q (%v %v)",
			plainRoute.Name, effortRoute.Name, err, effortErr)
	}
	migratedReview, _, _ := fixture.host.ix.ReviewBinding()
	if migratedReview.RouteID == "" || migratedReview.Endpoint != reviewRow.Endpoint || migratedReview.Model != reviewRow.Model ||
		migratedReview.RequestPathDigest != reviewRow.RequestPathDigest {
		t.Fatalf("the reviewer was not migrated in place: %+v", migratedReview)
	}
	if _, err := fixture.review.resolveBinding(migratedReview); err != nil {
		t.Fatalf("the migrated reviewer no longer resolves: %v", err)
	}
	// The run-start check of a migrated place passes: it runs as before.
	compiled := fixture.compiled(t, fixture.remote)
	if refusal, _ := fixture.host.runStartRefusal(migratedPlain, compiled); refusal != nil {
		t.Fatalf("a migrated place is refused at run start: %+v", refusal)
	}

	// Fail half: the binding the pass could not write keeps its values, stays on, runs
	// as before, and says migration_failed.
	failed := read("legacy-broken")
	if failed.RouteProblem != store.RouteProblemMigrationFailed || failed.RouteID != "" || failed.State != "enabled" ||
		failed.Runtime != broken.Runtime || failed.Model != broken.Model {
		t.Fatalf("a failed migration disabled or re-pointed the place: %+v", failed)
	}
	if refusal, _ := fixture.host.runStartRefusal(failed, compiled); refusal != nil {
		t.Fatalf("a place the migration could not write no longer runs: %+v", refusal)
	}
	var roster rosterResponse
	status, body := fixture.call(t, http.MethodGet, "/api/orchestration/roster", nil)
	if err := json.Unmarshal([]byte(body), &roster); status != http.StatusOK || err != nil {
		t.Fatalf("roster: %d %v", status, err)
	}
	shown := map[string]rosterPlace{}
	for _, agent := range roster.Agents {
		for _, place := range agent.Places {
			shown[place.PlaceID] = place
		}
	}
	if shown["legacy-broken"].RouteProblem != store.RouteProblemMigrationFailed || shown["legacy-plain"].RouteName != plainRoute.Name ||
		shown["legacy-plain"].RouteID != plainRoute.RouteID || !shown["legacy-plain"].RouteLocal || shown["legacy-plain"].Runtime != "managed-fixture" {
		t.Fatalf("the roster does not show the route facts: %+v %+v", shown["legacy-broken"], shown["legacy-plain"])
	}

	// Idempotent: nothing more to do, and no new routes.
	again, err := reconcileModelRoutes(fixture.host.ix, fixture.host.routes)
	if err != nil || again.RoutesCreated != 0 || again.BindingsMigrated != 0 || len(again.BindingsRepaired) != 0 {
		t.Fatalf("second pass = %+v, %v", again, err)
	}
	if same := read("legacy-plain"); same.StateToken != migratedPlain.StateToken {
		t.Fatal("an idempotent pass minted a new state token")
	}
}

func (fixture routeFixture) compiled(t *testing.T, preview profilefs.Preview) profilefs.CompiledProfile {
	t.Helper()
	detail, err := fixture.owner.GetRevision(preview.ProfileID, preview.SourceDigest, preview.BundleDigest)
	if err != nil || detail.Normalized == nil {
		t.Fatalf("revision: %v", err)
	}
	return *detail.Normalized
}

func removeRouteFile(t *testing.T, routes *modelRoutes, routeID string) {
	t.Helper()
	if err := os.Remove(filepath.Join(routes.owner.Root(), "selections", routeID+".json")); err != nil {
		t.Fatal(err)
	}
}

// Criterion 57, fail half: a removed route file leaves route_missing; the run is
// refused, typed; nothing falls back to a runtime's default model.
func TestMissingPrimaryRouteRefusesRunsTyped(t *testing.T) {
	fixture := newRouteFixture(t)
	route := fixture.localRoute(t, "Local")
	fixture.place(t, "agent-missing", fixture.helper, route.RouteID)
	removeRouteFile(t, fixture.host.routes, route.RouteID)

	report, err := reconcileModelRoutes(fixture.host.ix, fixture.host.routes)
	if err != nil || len(report.RouteMissing) != 1 {
		t.Fatalf("start-up pass = %+v, %v", report, err)
	}
	marked, _, _ := fixture.host.ix.ManagedBinding("agent-missing")
	if marked.RouteProblem != store.RouteProblemMissing || marked.State != "enabled" {
		t.Fatalf("the place was not marked route_missing, or was turned off: %+v", marked)
	}
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-missing", Cwd: fixture.root}, "missing-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 })
	if runs[0].State != "suppressed" || runs[0].ErrorClass != routeRefusalMissing || runs[0].ChildTaskID != "" ||
		!strings.Contains(runs[0].Recovery, "route_missing") {
		t.Fatalf("a place with a missing route ran, or was not refused typed: %+v", runs[0])
	}
	// A write that restates the place is refused too: the route is still gone.
	status, body := fixture.call(t, http.MethodPost, "/api/orchestration/agents/batch", map[string]any{"confirmed": true,
		"changes": []map[string]any{{"binding_id": "agent-missing", "expected_state_token": marked.StateToken, "op": "update",
			"set": map[string]any{"priority": 3}}}})
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "route_missing") {
		t.Fatalf("an edit of a place with a missing route: %d %s", status, body)
	}
}

// Criterion 57, fail half: a reviewer whose route is missing falls back to the person.
func TestMissingReviewerRouteFallsBackToThePerson(t *testing.T) {
	fixture := newRouteFixture(t)
	route := fixture.inferenceRoute(t, "Reviewer", "http://127.0.0.1:1", "local-test-model")
	fixture.reviewer(t, route.RouteID)
	removeRouteFile(t, fixture.host.routes, route.RouteID)
	if _, err := reconcileModelRoutes(fixture.host.ix, fixture.host.routes); err != nil {
		t.Fatal(err)
	}
	marked, _, _ := fixture.host.ix.ReviewBinding()
	if marked.RouteProblem != store.RouteProblemMissing || marked.State != "enabled" {
		t.Fatalf("the reviewer was not marked route_missing: %+v", marked)
	}
	// The next start builds a host that reviews nothing: with no running reviewer an
	// approval is offered to nobody, so the person answers.
	restarted, err := newOrchestrationReviewHost(fixture.host.ix, fixture.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	restarted.mu.RLock()
	running, problem := restarted.runtimeBinding, restarted.startupProblem
	restarted.mu.RUnlock()
	if running != nil || !strings.Contains(problem, "route_missing") || !strings.Contains(problem, "a person answers") {
		t.Fatalf("a reviewer with a missing route is still reviewing: running=%v problem=%q", running != nil, problem)
	}
}

// Criterion 57, fail half: a missing chain entry is skipped by the outage reroute and
// named in the outage record.
func TestMissingChainEntryIsSkippedAndNamedInTheOutageRecord(t *testing.T) {
	pinProviderRetryPolicy(t, immediateRetryPolicy)
	fixture := newAgentHostFixture(t, managedDynamicFixtureDriver{commandFor: func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "root question") {
			return jsonTextCommand("Root done.")
		}
		return quotaFailureCommand
	}})
	chatDrivers["fallback-fixture"] = fallbackFixtureDriver{managedDynamicFixtureDriver{
		commandFor: func(ChatRequest) string { return jsonTextCommand(helperClaimV2) }}}
	preview := selectManagedProfile(t, fixture.owner, helperAgentProfileSource())
	vanishing := testRouteID(fixture.host, "fallback-fixture", "soon-gone", nil)
	if _, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-chain", ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), Priority: 10,
		Routes:             []bindingRouteEntry{{RouteID: vanishing}, {RouteID: testRouteID(fixture.host, "fallback-fixture", "", nil)}},
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-chain")}); err != nil {
		t.Fatal(err)
	}
	removeRouteFile(t, fixture.host.routes, vanishing)
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-chain", Cwd: fixture.root}, "chain-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool { return len(runs) == 1 && runs[0].State == "parked" })
	fixture.host.relaunchParkedRunsOnce()
	completed := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		return len(runs) == 1 && runs[0].State == "completed"
	})[0]
	ledger := providerOutageLedgerFrom(completed.Detail)
	last := ledger.Attempts[len(ledger.Attempts)-1]
	if last.RouteIndex != 2 || last.Runtime != "fallback-fixture" || last.Model != "" {
		t.Fatalf("the reroute did not pass over the missing entry to the next one: %+v", ledger.Attempts)
	}
	if len(ledger.SkippedRoutes) != 1 || ledger.SkippedRoutes[0].RouteID != vanishing ||
		ledger.SkippedRoutes[0].RouteIndex != 1 || ledger.SkippedRoutes[0].Reason != routeRefusalMissing {
		t.Fatalf("the outage record does not name the skipped entry: %+v", ledger.SkippedRoutes)
	}
}

func denyRemoteRoutes(string) (*engine.Policy, error) {
	return &engine.Policy{Rules: []engine.Rule{
		{ID: "no-remote-routes", Action: "deny", Message: "remote routes are not admitted",
			If: engine.Predicate{Tag: engine.RouteFactPrefix + "local", Value: "false"}},
		// A command rule that matches anything: admission must never read it.
		{ID: "any-command", Action: "deny", If: engine.Predicate{Not: &engine.Predicate{Tag: engine.CommandTagKey, Matches: "never-typed"}}},
	}}, nil
}

// Criterion 59: with no route: rule selected, every bind and run proceeds. With a
// rulebook that denies a route:local=false route, the bind and the run are refused
// admission_refused.
func TestAdmissionRefusesBindAndRunStartTyped(t *testing.T) {
	fixture := newRouteFixture(t)
	local := fixture.localRoute(t, "Local")
	remote := mustRoute(t, fixture.host.routes, modelroute.Draft{Name: "Remote", Family: modelroute.FamilyRuntimeModel,
		Fields: modelroute.Fields{Runtime: "managed-fixture", Model: fixtureNonLocalModel}})
	compiled := fixture.compiled(t, fixture.remote)

	// No rulebook, then a rulebook with no route rule: the remote bind proceeds.
	placed := fixture.place(t, "agent-remote", fixture.remote, remote.RouteID)
	fixture.host.routes.state.setPolicy(func(string) (*engine.Policy, error) {
		return &engine.Policy{Rules: []engine.Rule{{ID: "any-command", Action: "deny",
			If: engine.Predicate{Not: &engine.Predicate{Tag: engine.CommandTagKey, Matches: "never-typed"}}}}}, nil
	})
	t.Cleanup(func() { fixture.host.routes.state.setPolicy(nil) })
	if refusal, _ := fixture.host.runStartRefusal(placed, compiled); refusal != nil {
		t.Fatalf("a run was refused with no route rule selected: %+v", refusal)
	}
	second := fixture.place(t, "agent-remote-two", fixture.remote, remote.RouteID)

	fixture.host.routes.state.setPolicy(denyRemoteRoutes)
	_, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-refused", ProfileID: fixture.remote.ProfileID,
		ProfileSourceDigest: fixture.remote.SourceDigest, ProfileBundleDigest: fixture.remote.BundleDigest,
		ProjectRoot: fixture.root, RouteID: remote.RouteID, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-refused")})
	var refused *modelroute.AdmissionRefused
	if !errors.As(err, &refused) || refused.Code() != modelroute.AdmissionRefusedCode || refused.Admission.Rule != "no-remote-routes" {
		t.Fatalf("the bind was not refused admission_refused: %v", err)
	}
	if _, found, _ := fixture.host.ix.ManagedBinding("agent-refused"); found {
		t.Fatal("a refused bind wrote the place")
	}
	// A remote route in the fallback chain is refused too.
	_, err = fixture.host.putBinding(managedBindingCommand{BindingID: "agent-refused", ProfileID: fixture.remote.ProfileID,
		ProfileSourceDigest: fixture.remote.SourceDigest, ProfileBundleDigest: fixture.remote.BundleDigest,
		ProjectRoot: fixture.root, RouteID: local.RouteID, Routes: []bindingRouteEntry{{RouteID: remote.RouteID}},
		ExpectedStateToken: store.ManagedBindingAbsentToken("agent-refused")})
	if !errors.As(err, &refused) {
		t.Fatalf("a refused route in the fallback chain was bound: %v", err)
	}
	// A local route on the same rulebook binds.
	localPlace := fixture.place(t, "agent-local", fixture.remote, local.RouteID)
	// Only one helper acts on a signal; leave the refused place as the only one on.
	for id, token := range map[string]string{"agent-local": localPlace.StateToken, "agent-remote-two": second.StateToken} {
		if _, err := fixture.host.ix.DisableManagedBinding(id, token, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}

	// Run start: the place bound before the rule existed is refused, typed, and the
	// refusal is one visible run row; the place stays on.
	refusal, _ := fixture.host.runStartRefusal(placed, compiled)
	if refusal == nil || refusal.Code != modelroute.AdmissionRefusedCode || !strings.Contains(refusal.Message, "no-remote-routes") {
		t.Fatalf("the run start was not refused admission_refused: %+v", refusal)
	}
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-admit", Cwd: fixture.root}, "admit-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		refusedRuns := 0
		for _, run := range runs {
			if run.ErrorClass == modelroute.AdmissionRefusedCode {
				refusedRuns++
			}
		}
		return refusedRuns >= 1
	})
	for _, run := range runs {
		if run.BindingID == "agent-remote" && (run.State != "suppressed" || run.ErrorClass != modelroute.AdmissionRefusedCode || run.ChildTaskID != "") {
			t.Fatalf("a refused route still ran: %+v", run)
		}
	}
	if still, _, _ := fixture.host.ix.ManagedBinding("agent-remote"); still.State != "enabled" {
		t.Fatalf("a refused run turned the place off: %+v", still)
	}
}

func TestAdmissionObserveProceedsWithTheRuleRecorded(t *testing.T) {
	fixture := newRouteFixture(t)
	remote := mustRoute(t, fixture.host.routes, modelroute.Draft{Name: "Remote", Family: modelroute.FamilyRuntimeModel,
		Fields: modelroute.Fields{Runtime: "managed-fixture", Model: fixtureNonLocalModel}})
	fixture.host.routes.state.setPolicy(func(root string) (*engine.Policy, error) {
		return &engine.Policy{Rules: []engine.Rule{{ID: "watch-remote", Action: "observe",
			If: engine.Predicate{All: []engine.Predicate{{Tag: engine.RouteFactPrefix + "local", Value: "false"},
				{Tag: engine.ProjectRootFactKey, Value: root}, {Tag: engine.ProfileLocalityFactKey, Value: "explicit-local-or-remote"}}}}}}, nil
	})
	t.Cleanup(func() { fixture.host.routes.state.setPolicy(nil) })
	placed := fixture.place(t, "agent-watched", fixture.remote, remote.RouteID)
	refusal, observed := fixture.host.runStartRefusal(placed, fixture.compiled(t, fixture.remote))
	if refusal != nil || len(observed) != 1 || observed[0] != "watch-remote" {
		t.Fatalf("observe must proceed with the rule recorded: refusal=%+v observed=%v", refusal, observed)
	}
}

func TestReviewerAdmissionRefusesBindAndRunStart(t *testing.T) {
	fixture := newRouteFixture(t)
	route := fixture.inferenceRoute(t, "Reviewer", "http://127.0.0.1:1", "local-test-model")
	bound := fixture.reviewer(t, route.RouteID)
	denyInference := func(string) (*engine.Policy, error) {
		return &engine.Policy{Rules: []engine.Rule{{ID: "no-inference", Action: "deny",
			If: engine.Predicate{Tag: engine.RouteFactPrefix + "destination-class", Value: "loopback"}}}}, nil
	}
	fixture.host.routes.state.setPolicy(denyInference)
	t.Cleanup(func() { fixture.host.routes.state.setPolicy(nil) })
	_, err := fixture.review.putBinding(reviewBindingCommand{ProfileID: fixture.checker.ProfileID,
		ProfileSourceDigest: fixture.checker.SourceDigest, ProfileBundleDigest: fixture.checker.BundleDigest,
		RouteID: route.RouteID, TimeoutMS: 1000, ExpectedStateToken: bound.StateToken})
	var refused *modelroute.AdmissionRefused
	if !errors.As(err, &refused) || refused.Admission.Rule != "no-inference" {
		t.Fatalf("the reviewer bind was not refused: %v", err)
	}
	fixture.review.mu.RLock()
	running := *fixture.review.runtimeBinding
	fixture.review.mu.RUnlock()
	if refusal := fixture.review.startRefusal(running, fixture.root); refusal == nil || refusal.Code != modelroute.AdmissionRefusedCode {
		t.Fatalf("the reviewer run start was not refused: %+v", refusal)
	}
	if refusal := fixture.review.runRefusal(); refusal == nil || refusal.Code != modelroute.AdmissionRefusedCode {
		t.Fatalf("the refusal is not remembered for the roster: %+v", refusal)
	}
}

// The stateful tier never holds a route rule either (OD-25).
func TestStatefulRulesExcludeRouteRules(t *testing.T) {
	policy := &engine.Policy{Rules: []engine.Rule{
		{ID: "state", Action: "deny", If: engine.Predicate{Tag: engine.SessionStatePrefix + "reviewed"}},
		{ID: "route", Action: "deny", If: engine.Predicate{Not: &engine.Predicate{Tag: engine.RouteFactPrefix + "local", Value: "true"}}},
		{ID: "mixed", Action: "deny", If: engine.Predicate{All: []engine.Predicate{
			{Tag: engine.SessionStatePrefix + "reviewed"}, {Tag: engine.RouteFactPrefix + "local", Value: "false"}}}},
	}}
	stateful := statefulRules(policy)
	if len(stateful.Rules) != 1 || stateful.Rules[0].ID != "state" {
		t.Fatalf("the stateful tier holds a route rule: %+v", stateful.Rules)
	}
}

// fakeAdoptions is an adoption owner for the seam: profiles in adopted have a key;
// held names the reason a key is held for; movable lists the bundle digests an adopted
// place may move to.
type fakeAdoptions struct {
	adopted map[string]string
	held    map[string]string
	movable map[string]bool
}

func (fake fakeAdoptions) KeyFor(profileID, _, _ string) string { return fake.adopted[profileID] }
func (fake fakeAdoptions) HoldReason(key, _, _, _ string) string {
	if key == "" {
		return ""
	}
	return fake.held[key]
}
func (fake fakeAdoptions) MoveAllowed(_, _, _, bundleDigest string) bool {
	return fake.movable[bundleDigest]
}
func (fake fakeAdoptions) Origin(profileID string) (placeAdoptionOrigin, bool) {
	if fake.adopted[profileID] == "" {
		return placeAdoptionOrigin{}, false
	}
	return placeAdoptionOrigin{OrganizationID: "org_1", OrganizationName: "Example", Scope: "organization", ReadOnly: true}, true
}

func TestAdoptionKeyIsRecordedHeldPlacesStartNoRunAndMovesAreChecked(t *testing.T) {
	fixture := newRouteFixture(t)
	route := fixture.localRoute(t, "Local")
	const key = "org_1\x1forganization"
	adoptions := fakeAdoptions{adopted: map[string]string{fixture.helper.ProfileID: key}, held: map[string]string{}, movable: map[string]bool{}}
	fixture.host.setPlaceAdoptions(adoptions)
	adopted := fixture.place(t, "agent-adopted", fixture.helper, route.RouteID)
	own := fixture.place(t, "agent-own", fixture.remote, route.RouteID)
	if adopted.AdoptionKey != key || own.AdoptionKey != "" {
		t.Fatalf("adoption keys: adopted=%q own=%q", adopted.AdoptionKey, own.AdoptionKey)
	}
	compiled := fixture.compiled(t, fixture.helper)
	if refusal, _ := fixture.host.runStartRefusal(adopted, compiled); refusal != nil {
		t.Fatalf("a current adoption held its place: %+v", refusal)
	}

	// Held: no run starts, the reason is typed and shown, the place stays on.
	adoptions.held[key] = placeHoldExpired
	if refusal, _ := fixture.host.runStartRefusal(adopted, compiled); refusal == nil || refusal.Code != placeHoldExpired {
		t.Fatalf("an expired adoption did not hold its place: %+v", refusal)
	}
	if refusal, _ := fixture.host.runStartRefusal(own, fixture.compiled(t, fixture.remote)); refusal != nil {
		t.Fatalf("a place with no adoption key was held: %+v", refusal)
	}
	rootTask, _, err := fixture.tasks.Create(ChatRequest{Runtime: "managed-fixture", Prompt: "root question", SessionID: "native-held", Cwd: fixture.root}, "held-root")
	if err != nil {
		t.Fatal(err)
	}
	wantTaskState(t, fixture.tasks, rootTask.ID, TaskCompleted, 3*time.Second)
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.BindingID == "agent-adopted" {
				return true
			}
		}
		return false
	})
	for _, run := range runs {
		if run.BindingID == "agent-adopted" && (run.State != "suppressed" || run.ErrorClass != placeHoldExpired || run.ChildTaskID != "") {
			t.Fatalf("a held place started a run: %+v", run)
		}
	}
	var roster rosterResponse
	status, body := fixture.call(t, http.MethodGet, "/api/orchestration/roster", nil)
	if err := json.Unmarshal([]byte(body), &roster); status != http.StatusOK || err != nil {
		t.Fatalf("roster: %d %v", status, err)
	}
	for _, agent := range roster.Agents {
		if agent.ProfileID == fixture.helper.ProfileID {
			if agent.Origin == nil || agent.Origin.OrganizationID != "org_1" || len(agent.Places) != 1 ||
				agent.Places[0].HeldReason != placeHoldExpired || agent.Places[0].State != "enabled" {
				t.Fatalf("the roster does not show the origin and the hold: %+v", agent)
			}
		} else if agent.Origin != nil {
			t.Fatalf("an agent of the member's own shows an origin: %+v", agent)
		}
	}

	// Moving an adopted place to a version its adoption does not list is refused.
	updated := selectManagedProfile(t, fixture.owner, []byte(strings.Replace(string(helperAgentProfileSource()), `version: "1.0.0"`, `version: "1.1.0"`, 1)))
	move := managedBindingCommand{BindingID: "agent-adopted", ProfileID: updated.ProfileID,
		ProfileSourceDigest: updated.SourceDigest, ProfileBundleDigest: updated.BundleDigest, ProjectRoot: fixture.root,
		RouteID: route.RouteID, ScopeRuntime: "managed-fixture", ExpectedStateToken: adopted.StateToken}
	_, err = fixture.host.putBinding(move)
	var refusal *routeRefusal
	if !errors.As(err, &refusal) || refusal.Code != routeRefusalMove {
		t.Fatalf("an adopted place moved to an unlisted version: %v", err)
	}
	adoptions.movable[updated.BundleDigest] = true
	moved, err := fixture.host.putBinding(move)
	if err != nil || moved.AdoptionKey != key || moved.ProfileBundleDigest != updated.BundleDigest {
		t.Fatalf("an allowed move = %+v, %v", moved, err)
	}
}

func TestReviewerAdoptionKeyAndHold(t *testing.T) {
	fixture := newRouteFixture(t)
	route := fixture.inferenceRoute(t, "Reviewer", "http://127.0.0.1:1", "local-test-model")
	const key = "org_1\x1forganization"
	adoptions := fakeAdoptions{adopted: map[string]string{fixture.checker.ProfileID: key}, held: map[string]string{}, movable: map[string]bool{}}
	fixture.review.setPlaceAdoptions(adoptions)
	bound := fixture.reviewer(t, route.RouteID)
	if bound.AdoptionKey != key {
		t.Fatalf("the reviewer did not record its adoption key: %+v", bound)
	}
	fixture.review.mu.RLock()
	running := *fixture.review.runtimeBinding
	fixture.review.mu.RUnlock()
	if refusal := fixture.review.startRefusal(running, fixture.root); refusal != nil {
		t.Fatalf("a current adoption held the reviewer: %+v", refusal)
	}
	adoptions.held[key] = placeHoldVersionGone
	if refusal := fixture.review.startRefusal(running, fixture.root); refusal == nil || refusal.Code != placeHoldVersionGone {
		t.Fatalf("a withdrawn version did not hold the reviewer: %+v", refusal)
	}
	var roster rosterResponse
	status, body := fixture.call(t, http.MethodGet, "/api/orchestration/roster", nil)
	if err := json.Unmarshal([]byte(body), &roster); status != http.StatusOK || err != nil {
		t.Fatalf("roster: %d %v", status, err)
	}
	for _, agent := range roster.Agents {
		if agent.ProfileID != fixture.checker.ProfileID {
			continue
		}
		place := agent.Places[0]
		if place.HeldReason != placeHoldVersionGone || place.RunRefusal != placeHoldVersionGone || place.State != "enabled" ||
			place.RouteName != "Reviewer" || !place.RouteLocal || agent.Origin == nil {
			t.Fatalf("the roster does not show the reviewer's hold and route: %+v origin=%+v", place, agent.Origin)
		}
	}
}

// The route API end to end over HTTP: preview writes nothing, select requires the
// previewed digest, the state token and confirmation, names are unique, and every
// response decodes into its declared type.
func TestModelRouteAPILifecycle(t *testing.T) {
	fixture := newRouteFixture(t)
	draft := map[string]any{"name": "Fast one", "family": modelroute.FamilyRuntimeModel,
		"fields": map[string]any{"runtime": "managed-fixture", "model": "model-a"}}
	status, body := fixture.call(t, http.MethodPost, "/api/model-routes/preview", draft)
	var preview modelRoutePreviewResponse
	if err := json.Unmarshal([]byte(body), &preview); status != http.StatusOK || err != nil || preview.Exists || !preview.Changed ||
		preview.StateToken != modelroute.AbsentStateToken() || len(preview.Admission) != 1 || !preview.Admission[0].Allowed {
		t.Fatalf("preview: %d %s %v", status, body, err)
	}
	if listed, _ := fixture.host.routes.owner.List(); len(listed.Routes) != 0 {
		t.Fatal("preview wrote a route")
	}
	selectBody := func(extra map[string]any) map[string]any {
		out := map[string]any{"preview_digest": preview.PreviewDigest, "expected_state_token": preview.StateToken, "confirmed": true}
		for key, value := range draft {
			out[key] = value
		}
		for key, value := range extra {
			out[key] = value
		}
		return out
	}
	if status, body := fixture.call(t, http.MethodPost, "/api/model-routes/select", selectBody(map[string]any{"confirmed": false})); status != http.StatusUnprocessableEntity || !strings.Contains(body, modelroute.CodeUnconfirmed) {
		t.Fatalf("unconfirmed select: %d %s", status, body)
	}
	if status, body := fixture.call(t, http.MethodPost, "/api/model-routes/select", selectBody(map[string]any{"preview_digest": "sha256-v1:other"})); status != http.StatusConflict || !strings.Contains(body, modelroute.CodeStateConflict) {
		t.Fatalf("select with another digest: %d %s", status, body)
	}
	status, body = fixture.call(t, http.MethodPost, "/api/model-routes/select", selectBody(nil))
	var created modelRouteSelectResponse
	if err := json.Unmarshal([]byte(body), &created); status != http.StatusOK || err != nil || !created.Created ||
		!modelroute.ValidID(created.Route.RouteID) || created.Route.Name != "Fast one" {
		t.Fatalf("select: %d %s %v", status, body, err)
	}
	if status, body := fixture.call(t, http.MethodPost, "/api/model-routes/preview", map[string]any{"name": "  FAST ONE ",
		"family": modelroute.FamilyRuntimeModel, "fields": map[string]any{"runtime": "managed-fixture", "model": "model-b"}}); status != http.StatusConflict ||
		!strings.Contains(body, modelroute.CodeNameTaken) || !strings.Contains(body, "Fast one") {
		t.Fatalf("duplicate name: %d %s", status, body)
	}
	status, body = fixture.call(t, http.MethodGet, "/api/model-routes/"+created.Route.RouteID, nil)
	var detail modelRouteDetailResponse
	if err := json.Unmarshal([]byte(body), &detail); status != http.StatusOK || err != nil || detail.Route.RevisionDigest != created.Route.RevisionDigest {
		t.Fatalf("get: %d %s %v", status, body, err)
	}
	if status, body := fixture.call(t, http.MethodGet, "/api/model-routes/rte_00000000000000000000000000", nil); status != http.StatusNotFound || !strings.Contains(body, modelroute.CodeNotFound) {
		t.Fatalf("get of an unknown route: %d %s", status, body)
	}
	status, body = fixture.call(t, http.MethodDelete, "/api/model-routes/"+created.Route.RouteID,
		map[string]any{"expected_state_token": created.Route.StateToken, "confirmed": true})
	var deleted modelRouteDeleteResponse
	if err := json.Unmarshal([]byte(body), &deleted); status != http.StatusOK || err != nil || deleted.Deleted != created.Route.RouteID {
		t.Fatalf("delete: %d %s %v", status, body, err)
	}
}
