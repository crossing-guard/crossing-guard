package daemon

import (
	"testing"

	"crossing-guard/internal/modelroute"
	"crossing-guard/store"
)

// testRouteID returns the id of the runtime-model route with these fields on the
// host's data directory, creating it on first use. Tests bind places the way the
// product does: by route, never by a typed runtime or model.
func testRouteID(host *orchestrationManagedHost, runtime, model string, effort *store.ThinkingEffort) string {
	return testRouteIn(host.routes, modelroute.FamilyRuntimeModel,
		modelroute.Fields{Runtime: runtime, Model: model, ThinkingEffort: effortForRoute(effort)})
}

// testInferenceRouteID is testRouteID for the reviewer's inference family.
func testInferenceRouteID(host *orchestrationReviewHost, endpoint, model string) string {
	return testRouteIn(host.routes, modelroute.FamilyInference, modelroute.Fields{Endpoint: endpoint, Model: model})
}

func testRouteIn(routes *modelRoutes, family string, fields modelroute.Fields) string {
	report := modelRouteStartReport{}
	revision, err := migratedRoute(routes, family, fields, &report)
	if err != nil {
		panic("test route: " + err.Error())
	}
	return revision.RouteID
}

// testChain is a fallback chain of routes for the given runtimes, each with its mode.
func testChain(host *orchestrationManagedHost, entries ...store.ManagedRoute) []bindingRouteEntry {
	out := make([]bindingRouteEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, bindingRouteEntry{RouteID: testRouteID(host, entry.Runtime, entry.Model, entry.ThinkingEffort), Mode: entry.Mode})
	}
	return out
}

// mustRoute creates a route through preview and select, the way the API does.
func mustRoute(t *testing.T, routes *modelRoutes, draft modelroute.Draft) modelroute.Route {
	t.Helper()
	preview, err := routes.owner.Preview(draft)
	if err != nil {
		t.Fatalf("preview route %q: %v", draft.Name, err)
	}
	selected, err := routes.owner.Select(modelroute.SelectCommand{Draft: draft, ExpectedPreviewDigest: preview.PreviewDigest,
		ExpectedStateToken: preview.StateToken, Confirmed: true})
	if err != nil {
		t.Fatalf("select route %q: %v", draft.Name, err)
	}
	return selected.Route
}
