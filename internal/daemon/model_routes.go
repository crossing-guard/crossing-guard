package daemon

// Named model routes inside the daemon (team rest-of-release plan §5). The route owner
// (internal/modelroute) stores routes; this file is what the orchestration hosts, the
// start-up pass and the route API share: resolving a route for a place, the typed
// refusals, the request-path identity a route gives the reviewer, and admission.

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"

	"crossing-guard/engine"
	"crossing-guard/infer"
	"crossing-guard/internal/modelroute"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
)

// Typed refusal codes of the route surface. They are data the surfaces show.
const (
	routeRefusalTypedField = "typed_model_field"
	routeRefusalRequired   = "route_required"
	routeRefusalMissing    = store.RouteProblemMissing
	routeRefusalFamily     = "route_family_mismatch"
	routeRefusalPlaces     = "route_edit_violates_places"
	routeRefusalHeld       = "place_held"
	routeRefusalMove       = "adoption_move_refused"
	// routeRefusalRulebook refuses a bind while the rulebook cannot be read: a route
	// rule that would refuse the route cannot be seen, so nothing is bound past it.
	routeRefusalRulebook = "rulebook_unreadable"
)

// routeRefusal is the typed error of a binding write, a route edit or a run start the
// route rules refuse. Field names what the refusal is about, when it is one field.
type routeRefusal struct {
	Code    string
	Field   string
	Message string
	// Places names the places an edit would break, for routeRefusalPlaces.
	Places []string
}

func (refusal *routeRefusal) Error() string { return refusal.Message }

// typedModelFieldRefusal refuses a binding write that still names where the model call
// goes itself (plan §5.3, criterion 56): the field is named, and so is what to send.
func typedModelFieldRefusal(field string) *routeRefusal {
	return &routeRefusal{Code: routeRefusalTypedField, Field: field,
		Message: field + " is not accepted on a binding write: a place names a model route. Send route_id (create or choose one under Settings → Models)."}
}

// refusedField records that a request carried a field the route surface refuses. It
// accepts any JSON value so the refusal can name the field instead of failing as a
// generic decode error.
type refusedField struct{ present bool }

// UnmarshalJSON notes the field's presence; its value is never used.
func (field *refusedField) UnmarshalJSON([]byte) error {
	field.present = true
	return nil
}

// firstTypedModelField returns the refusal for the first present field, or nil. names
// and fields are parallel.
func firstTypedModelField(prefix string, names []string, fields ...refusedField) error {
	for index, field := range fields {
		if field.present {
			return typedModelFieldRefusal(prefix + names[index])
		}
	}
	return nil
}

// admissionPolicyLoader returns the selected rulebook for a place's project root.
type admissionPolicyLoader func(projectRoot string) (*engine.Policy, error)

// modelRouteState is what every route user of one data directory shares: the lock that
// orders binding writes against route edits and deletes, and the admission rulebook
// loader. A binding write holds the read side from resolving its route to committing
// its row; an edit or a delete holds the write side from its checks to its store
// transaction — so a route is never deleted under a bind that just resolved it, and an
// edit never misses a place written while it ran. The daemon is the only writer of both
// the route directory and the store, so an in-process lock is the whole of it.
type modelRouteState struct {
	guard sync.RWMutex
	mu    sync.Mutex
	// policy is nil until the daemon installs the layered rulebook loader at start
	// (prepareModelRoutes). With none, no rulebook is selected and admission allows —
	// which is also what keeps a test host from reading the real home directory.
	policy admissionPolicyLoader
}

var modelRouteStates sync.Map // clean data directory → *modelRouteState

func modelRouteStateFor(dataDir string) *modelRouteState {
	state, _ := modelRouteStates.LoadOrStore(filepath.Clean(dataDir), &modelRouteState{})
	return state.(*modelRouteState)
}

func (state *modelRouteState) setPolicy(loader admissionPolicyLoader) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.policy = loader
}

func (state *modelRouteState) policyLoader() admissionPolicyLoader {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.policy
}

// modelRoutes is one user's handle on the routes of a data directory: the owner and the
// shared state.
type modelRoutes struct {
	owner *modelroute.Owner
	state *modelRouteState
}

func newModelRoutes(dataDir string) (*modelRoutes, error) {
	owner, err := modelroute.New(dataDir)
	if err != nil {
		return nil, err
	}
	return &modelRoutes{owner: owner, state: modelRouteStateFor(owner.DataDir())}, nil
}

// modelRoutesBeside returns the routes of the data directory a profile owner was opened
// on. Routes and profiles live under one data directory by construction
// (<dataDir>/models/routes, <dataDir>/orchestration/profiles); the profile owner
// publishes its storage root and nothing else, so the data directory is read back from
// it. INTEGRATION: when profilefs gains a DataDir accessor this becomes one call.
func modelRoutesBeside(profiles *profilefs.Owner) (*modelRoutes, error) {
	if profiles == nil {
		return nil, errors.New("a profile owner is required to locate model routes")
	}
	// List reports the storage root even when nothing is stored or it cannot be read.
	listed, _ := profiles.List()
	if listed.StorageRoot == "" {
		return nil, errors.New("the profile owner did not report its storage root")
	}
	return newModelRoutes(filepath.Dir(filepath.Dir(listed.StorageRoot)))
}

// layeredAdmissionPolicy is the production loader: the same layered rulebook the hook
// and the stateful tier load (user, this checkout's repository layer, the organization
// layer), so a route rule is selected exactly where every other rule is.
func layeredAdmissionPolicy(layerStore string) admissionPolicyLoader {
	return func(projectRoot string) (*engine.Policy, error) {
		layered, err := rulebook.LoadLayeredFull(projectRoot, layerStore)
		if err != nil {
			return nil, err
		}
		if layered.LayersErr != nil {
			// The user layer was read and still decides; the team layers were not.
			return layered.Policy, fmt.Errorf("the adoption records could not be read: %w", layered.LayersErr)
		}
		if reason := layered.UnloadableLayer(); reason != "" {
			// An adopted layer's staged rules did not load: its route rules are not in
			// this policy, and admission must not pass as if the team had none.
			return layered.Policy, errors.New(reason)
		}
		return layered.Policy, nil
	}
}

// resolve reads a route and checks its family fits the place (plan §5.4 check 1): a
// managed profile takes a runtime-model route, the stateless reviewer an inference one.
func (routes *modelRoutes) resolve(routeID, family string) (modelroute.Route, error) {
	if routeID == "" {
		return modelroute.Route{}, &routeRefusal{Code: routeRefusalRequired, Field: "route_id",
			Message: "route_id is required: choose a model route for this place (Settings → Models)."}
	}
	route, err := routes.owner.Get(routeID)
	if err != nil {
		if modelroute.IsCode(err, modelroute.CodeNotFound) || modelroute.IsCode(err, modelroute.CodeIntegrity) ||
			modelroute.IsCode(err, modelroute.CodeInvalid) {
			return modelroute.Route{}, &routeRefusal{Code: routeRefusalMissing, Field: "route_id",
				Message: "route_missing: model route " + routeID + " cannot be read. Choose another model route for this place."}
		}
		return modelroute.Route{}, err
	}
	if route.Family != family {
		return modelroute.Route{}, &routeRefusal{Code: routeRefusalFamily, Field: "route_id",
			Message: "Model route " + route.Name + " is a " + route.Family + " route; this place takes a " + family + " route."}
	}
	return route, nil
}

// admit runs route admission for one route on one place (plan §5.4 point 3). When the
// rulebook cannot be read, or only part of it can, the answer is over the rules that
// were read and the read error is returned beside it and logged, never dropped. What
// the error means is the caller's: a bind refuses (rulebookUnreadable); a run start
// proceeds on the answer, as the hook's stateful tier does with a rulebook nobody can
// load; a preview lists it as a problem.
func (routes *modelRoutes) admit(route modelroute.Route, local bool, profileLocality, projectRoot string) (modelroute.Admission, error) {
	loader := routes.state.policyLoader()
	if loader == nil {
		return modelroute.Admit(nil, modelroute.AdmissionFacts{}), nil
	}
	policy, err := loader(projectRoot)
	if err != nil {
		log.Printf("model routes: route admission for %s ran without the whole rulebook: %v", route.Name, err)
	}
	if policy == nil {
		return modelroute.Admit(nil, modelroute.AdmissionFacts{}), err
	}
	return modelroute.Admit(policy, modelroute.FactsFor(route, local, profileLocality, projectRoot)), err
}

// rulebookUnreadable is the typed refusal of a bind made while the rulebook cannot be
// read.
func rulebookUnreadable(err error) *routeRefusal {
	return &routeRefusal{Code: routeRefusalRulebook, Field: "route_id",
		Message: "This place was not saved: the rulebook cannot be read, so the rules that decide which model routes a place may use could not be checked (" + err.Error() + "). Fix the rulebook, then save again."}
}

// managedRouteLocal is the runtime adapter's own claim about a runtime-model route at a
// place's mode — never an egress proof.
func managedRouteLocal(route modelroute.Route, mode string) bool {
	local, _ := chatRouteIsLocal(ChatRequest{Runtime: route.Fields.Runtime, Model: route.Fields.Model, Mode: mode})
	return local
}

func effortFromRoute(effort *modelroute.Effort) *store.ThinkingEffort {
	if effort == nil {
		return nil
	}
	return &store.ThinkingEffort{Kind: effort.Kind, Value: effort.Value}
}

func effortForRoute(effort *store.ThinkingEffort) *modelroute.Effort {
	if effort == nil {
		return nil
	}
	return &modelroute.Effort{Kind: effort.Kind, Value: effort.Value}
}

// routeRevisionFor is a route's current revision in the shape the store's binding rows
// take: the resolved copy of whichever family the route has.
func routeRevisionFor(route modelroute.Route) store.RouteRevision {
	revision := store.RouteRevision{RouteID: route.RouteID, RevisionDigest: route.RevisionDigest}
	if route.Family == modelroute.FamilyInference {
		revision.Review = &store.ReviewRouteCopy{Endpoint: route.Fields.Endpoint, Model: route.Fields.Model}
		return revision
	}
	revision.Managed = &store.ManagedRouteCopy{Runtime: route.Fields.Runtime, Model: route.Fields.Model,
		ThinkingEffort: effortFromRoute(route.Fields.ThinkingEffort)}
	return revision
}

// chainEntryFor is a fallback chain entry on a route: the reference, the resolved copy,
// and the entry's own mode.
func chainEntryFor(route modelroute.Route, mode string) store.ManagedRoute {
	return store.ManagedRoute{RouteID: route.RouteID, Runtime: route.Fields.Runtime, Model: route.Fields.Model,
		ThinkingEffort: effortFromRoute(route.Fields.ThinkingEffort), Mode: mode}
}

// reviewPathIdentity derives a review binding's request-path kind and digest from its
// endpoint and model and its own limits — through the one constructor the reviewer host
// later checks the stored identity against (resolveBinding), so the two cannot differ.
func reviewPathIdentity(binding store.ReviewBinding) (string, string, error) {
	path, err := infer.NewLocalOllamaPath(binding.Endpoint, binding.Model,
		binding.MaxInputBytes, binding.MaxOutputBytes, binding.MaxTokens)
	if err != nil {
		return "", "", err
	}
	return path.Kind, path.Digest, nil
}

// routeEffortProblem validates a runtime-model route's thinking effort against its
// runtime and model the way a turn request would; "" means it may run.
func routeEffortProblem(fields modelroute.Fields) string {
	if fields.ThinkingEffort == nil {
		return ""
	}
	request, err := normalizeEffort(ChatRequest{Runtime: fields.Runtime, Model: fields.Model,
		ThinkingEffort: effortFromRoute(fields.ThinkingEffort)})
	if err == nil {
		_, err = resolveRequestEffort(request)
	}
	if err != nil {
		return err.Error()
	}
	return ""
}
