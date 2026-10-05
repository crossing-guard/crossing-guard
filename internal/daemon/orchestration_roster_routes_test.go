package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"crossing-guard/internal/modelroute"
	"crossing-guard/store"
)

// An agent surface leads an outage with a route's name: the roster gives each tripped
// provider route the names of the routes that resolve to it (plan §14 Q9).
func TestRosterNamesTheRoutesOfAnOutage(t *testing.T) {
	composer := &rosterComposer{routeNames: map[string]modelroute.Route{
		"rte_a": {RouteID: "rte_a", Name: "Fast reader", Family: modelroute.FamilyRuntimeModel,
			Fields: modelroute.Fields{Runtime: "rt-a", Model: "m-1"}},
		"rte_b": {RouteID: "rte_b", Name: "Another reader", Family: modelroute.FamilyRuntimeModel,
			Fields: modelroute.Fields{Runtime: "rt-a", Model: "m-1"}},
		"rte_c": {RouteID: "rte_c", Name: "Elsewhere", Family: modelroute.FamilyRuntimeModel,
			Fields: modelroute.Fields{Runtime: "rt-a", Model: "m-2"}},
		"rte_d": {RouteID: "rte_d", Name: "Reviewer", Family: modelroute.FamilyInference,
			Fields: modelroute.Fields{Model: "m-1"}},
	}, outages: []providerOutageFact{{Runtime: "rt-a", Model: "m-1", Parked: 2}, {Runtime: "rt-z"}}}
	outages := composer.namedOutages()
	if len(outages) != 2 || strings.Join(outages[0].RouteNames, ",") != "Another reader,Fast reader" {
		t.Fatalf("named outages = %+v", outages)
	}
	if outages[1].RouteNames == nil || len(outages[1].RouteNames) != 0 {
		t.Fatalf("an outage no route resolves to must carry an empty list, not null: %+v", outages[1])
	}
	body, err := json.Marshal(outages[0])
	if err != nil || !strings.Contains(string(body), `"route_names":["Another reader","Fast reader"]`) ||
		!strings.Contains(string(body), `"parked":2`) {
		t.Fatalf("outage JSON = %s %v", body, err)
	}
}

// A fallback chain entry is named by its route; an entry whose route can no longer be
// read is marked missing, and one that predates routes is neither named nor missing.
func TestRosterNamesFallbackRoutes(t *testing.T) {
	composer := &rosterComposer{routeNames: map[string]modelroute.Route{
		"rte_a": {RouteID: "rte_a", Name: "Second choice"}}}
	chain := composer.fallbackRoutes([]store.ManagedRoute{
		{RouteID: "rte_a", Runtime: "rt-a", Mode: "plan"}, {RouteID: "rte_gone", Runtime: "rt-a"}, {Runtime: "rt-a"}})
	if len(chain) != 3 || chain[0].RouteName != "Second choice" || chain[0].Mode != "plan" || chain[0].Missing ||
		!chain[1].Missing || chain[1].RouteName != "" || chain[2].Missing || chain[2].RouteName != "" {
		t.Fatalf("fallback routes = %+v", chain)
	}
}

// A place that is on and starts no run needs attention; a place that is off does not.
func TestRosterAttentionForAPlaceThatStartsNoRun(t *testing.T) {
	cases := []struct {
		place rosterPlace
		want  string
	}{
		{rosterPlace{State: "enabled", HeldReason: placeHoldExpired}, attentionPlaceHeld},
		{rosterPlace{State: "enabled", RouteProblem: store.RouteProblemMissing}, attentionRouteMissing},
		{rosterPlace{State: "enabled", RunRefusal: modelroute.AdmissionRefusedCode}, attentionRunRefused},
		{rosterPlace{State: "enabled", RouteProblem: store.RouteProblemMigrationFailed}, ""},
		{rosterPlace{State: "disabled", HeldReason: placeHoldExpired}, ""},
	}
	for _, item := range cases {
		if got := placeStoppedAttention([]rosterPlace{item.place}); got != item.want {
			t.Fatalf("attention for %+v = %q, want %q", item.place, got, item.want)
		}
	}
}

// The roster marks a member's own agent whose id an offered team agent uses (OD-6).
func TestRosterCollisionsComeFromTheOffer(t *testing.T) {
	previous := team
	t.Cleanup(func() { team = previous })
	team = nil
	if got := teamIDCollisions(); len(got) != 0 {
		t.Fatalf("no team link must mean no collisions: %+v", got)
	}
	linker := &teamLinker{}
	linker.doc.Organization.Name = "Harbor Street Labs"
	linker.available = []teamLayerEntry{{Documents: []teamOfferDocument{
		{ProfileID: "release-notes", State: offerDocumentIDInUse}, {ProfileID: "scope-watch", State: "adopted"}}}}
	team = linker
	got := teamIDCollisions()
	if len(got) != 1 || got["release-notes"].OrganizationName != "Harbor Street Labs" {
		t.Fatalf("collisions = %+v", got)
	}
}
