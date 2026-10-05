package daemon

// The model route API (team rest-of-release plan §5.5): list, read, preview, select
// (create or edit) and delete. Settings → Models and the `routes` verb are its callers.
// Every response is a declared type. A route edit is the one multi-owner write here:
// the route file first, then ONE store transaction that moves every place using the
// route — and a crash between the two is repaired at the next start (plan §5.3).

import (
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crossing-guard/internal/modelroute"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// Where a route sends a model call, as an agent surface may say it (plan §5.5).
const (
	routeOnThisMachine     = "on this machine"
	routeLeavesThisMachine = "leaves this machine"
)

// modelRouteView is one route as the API shows it: its name, family and facts, whether
// it stays on this machine, when the migration created it, and the places that use it.
type modelRouteView struct {
	RouteID        string            `json:"route_id,omitempty"`
	Name           string            `json:"name"`
	Family         string            `json:"family"`
	Kind           string            `json:"kind"`
	Fields         modelroute.Fields `json:"fields"`
	Local          bool              `json:"local"`
	Locality       string            `json:"locality"`
	RevisionDigest string            `json:"revision_digest,omitempty"`
	StateToken     string            `json:"state_token,omitempty"`
	CreatedAt      string            `json:"created_at,omitempty"`
	MigratedAt     string            `json:"migrated_at,omitempty"`
	Places         []modelRoutePlace `json:"places"`
}

// modelRoutePlace is one place, or one fallback entry of a place, that uses a route.
type modelRoutePlace struct {
	store.RouteReference
	// Label names the place for a person: the agent and the repository, or the reviewer.
	Label string `json:"label"`
}

type modelRouteListResponse struct {
	Routes   []modelRouteView         `json:"routes"`
	Problems []modelroute.ListProblem `json:"problems"`
	// AbsentStateToken is the state token a create presents.
	AbsentStateToken string `json:"absent_state_token"`
	// PlacesUnavailable is true when the store could not be read, so Places is empty
	// for every route and says nothing.
	PlacesUnavailable bool `json:"places_unavailable"`
}

type modelRouteDetailResponse struct {
	Route modelRouteView `json:"route"`
}

// modelRouteProblem is one typed reason a route request is refused.
type modelRouteProblem struct {
	Code     string   `json:"code"`
	Field    string   `json:"field,omitempty"`
	Message  string   `json:"message"`
	Recovery string   `json:"recovery,omitempty"`
	Places   []string `json:"places,omitempty"`
}

type modelRouteErrorResponse struct {
	Error modelRouteProblem `json:"error"`
}

// modelRouteAdmission is what the selected route rules say about the route on one
// place ("" for the route with no place).
type modelRouteAdmission struct {
	PlaceID string `json:"place_id,omitempty"`
	Label   string `json:"label,omitempty"`
	modelroute.Admission
}

type modelRouteDraftRequest struct {
	RouteID string            `json:"route_id"`
	Name    string            `json:"name"`
	Family  string            `json:"family"`
	Fields  modelroute.Fields `json:"fields"`
}

func (request modelRouteDraftRequest) draft() modelroute.Draft {
	return modelroute.Draft{RouteID: strings.TrimSpace(request.RouteID), Name: request.Name,
		Family: request.Family, Fields: request.Fields}
}

// modelRoutePreviewResponse is what a select would do. It writes nothing.
type modelRoutePreviewResponse struct {
	Route         modelRouteView        `json:"route"`
	PreviewDigest string                `json:"preview_digest"`
	StateToken    string                `json:"state_token"`
	Exists        bool                  `json:"exists"`
	Changed       bool                  `json:"changed"`
	Problems      []modelRouteProblem   `json:"problems"`
	Admission     []modelRouteAdmission `json:"admission"`
}

type modelRouteSelectRequest struct {
	modelRouteDraftRequest
	PreviewDigest      string `json:"preview_digest"`
	ExpectedStateToken string `json:"expected_state_token"`
	Confirmed          bool   `json:"confirmed"`
}

type modelRouteSelectResponse struct {
	Route   modelRouteView `json:"route"`
	Changed bool           `json:"changed"`
	Created bool           `json:"created"`
	// MovedPlaces are the places that now run the new revision, each with a new state
	// token.
	MovedPlaces []string `json:"moved_places"`
}

type modelRouteDeleteRequest struct {
	ExpectedStateToken string `json:"expected_state_token"`
	Confirmed          bool   `json:"confirmed"`
}

type modelRouteDeleteResponse struct {
	Deleted string `json:"deleted"`
}

// modelRouteAPI is the route API's dependencies. index and review are asked per
// request: either host may be absent, and a route can still be listed and created.
type modelRouteAPI struct {
	routes   *modelRoutes
	profiles *profilefs.Owner
	index    func() *store.Index
	review   func() *orchestrationReviewHost
}

// registerModelRouteRoutes registers the five routes for the data directory. managed
// and review may be nil; the store the route API reads is whichever host has one.
func registerModelRouteRoutes(mux *http.ServeMux, dataDir string, profiles *profilefs.Owner,
	managed *orchestrationManagedHost, review *orchestrationReviewHost) {
	routes, err := newModelRoutes(dataDir)
	if err != nil {
		routes = nil
	}
	api := &modelRouteAPI{routes: routes, profiles: profiles,
		index: func() *store.Index {
			if managed != nil {
				return managed.ix
			}
			if review != nil {
				return review.ix
			}
			return nil
		},
		review: func() *orchestrationReviewHost { return review }}
	mux.HandleFunc("GET /api/model-routes", api.guarded(api.handleList))
	mux.HandleFunc("GET /api/model-routes/{id}", api.guarded(api.handleGet))
	mux.HandleFunc("POST /api/model-routes/preview", api.guarded(api.handlePreview))
	mux.HandleFunc("POST /api/model-routes/select", api.guarded(api.handleSelect))
	mux.HandleFunc("DELETE /api/model-routes/{id}", api.guarded(api.handleDelete))
}

func (api *modelRouteAPI) guarded(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if api.routes == nil {
			writeModelRouteError(w, &modelroute.Problem{Code: modelroute.CodeStorage,
				Message: "Model routes are unavailable on this device."})
			return
		}
		handler(w, r)
	}
}

func (api *modelRouteAPI) handleList(w http.ResponseWriter, _ *http.Request) {
	listed, err := api.routes.owner.List()
	if err != nil {
		writeModelRouteError(w, err)
		return
	}
	references, unavailable := api.references()
	response := modelRouteListResponse{Routes: make([]modelRouteView, 0, len(listed.Routes)),
		Problems: listed.Problems, AbsentStateToken: modelroute.AbsentStateToken(), PlacesUnavailable: unavailable}
	for _, route := range listed.Routes {
		response.Routes = append(response.Routes, routeView(route, references))
	}
	writeJSON(w, response)
}

func (api *modelRouteAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	route, err := api.routes.owner.Get(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeModelRouteError(w, err)
		return
	}
	references, _ := api.references()
	writeJSON(w, modelRouteDetailResponse{Route: routeView(route, references)})
}

func (api *modelRouteAPI) handlePreview(w http.ResponseWriter, r *http.Request) {
	var request modelRouteDraftRequest
	if err := decodeManagedJSON(w, r, &request); err != nil {
		writeModelRouteError(w, &modelroute.Problem{Code: modelroute.CodeInvalid, Message: err.Error()})
		return
	}
	preview, err := api.routes.owner.Preview(request.draft())
	if err != nil {
		writeModelRouteError(w, err)
		return
	}
	references, _ := api.references()
	prospective := modelroute.Route{RouteID: preview.Draft.RouteID, Name: preview.Draft.Name, Family: preview.Draft.Family,
		Kind: preview.Kind, Fields: preview.Draft.Fields}
	response := modelRoutePreviewResponse{Route: routeView(prospective, references), PreviewDigest: preview.PreviewDigest,
		StateToken: preview.StateToken, Exists: preview.Exists, Changed: preview.Changed,
		Problems: []modelRouteProblem{}}
	admission, admitErr := api.previewAdmission(prospective, references)
	response.Admission = admission
	if admitErr != nil {
		response.Problems = append(response.Problems, modelRouteProblem{Code: routeRefusalRulebook,
			Message:  "The rulebook cannot be read, so the rules that decide which model routes a place may use could not be checked: " + admitErr.Error(),
			Recovery: "Fix the rulebook, then preview again."})
	}
	if problem := routeEffortProblem(preview.Draft.Fields); problem != "" {
		response.Problems = append(response.Problems, modelRouteProblem{Code: modelroute.CodeInvalid,
			Field: "fields.thinking_effort", Message: problem})
	}
	if refusal := api.placeProblems(prospective); refusal != nil {
		response.Problems = append(response.Problems, modelRouteProblem{Code: refusal.Code,
			Message: refusal.Message, Places: refusal.Places})
	}
	writeJSON(w, response)
}

func (api *modelRouteAPI) handleSelect(w http.ResponseWriter, r *http.Request) {
	var request modelRouteSelectRequest
	if err := decodeManagedJSON(w, r, &request); err != nil {
		writeModelRouteError(w, &modelroute.Problem{Code: modelroute.CodeInvalid, Message: err.Error()})
		return
	}
	response, err := api.selectRoute(modelroute.SelectCommand{Draft: request.draft(),
		ExpectedPreviewDigest: request.PreviewDigest, ExpectedStateToken: request.ExpectedStateToken,
		Confirmed: request.Confirmed})
	if err != nil {
		writeModelRouteError(w, err)
		return
	}
	writeJSON(w, response)
}

// selectRoute creates a route or edits one. An edit runs the profile checks for every
// place that uses the route first (plan §5.4 check 2: an edit that would make any place
// violate its profile is refused, naming the places), then writes the revision file,
// then moves the places in one store transaction. The route write lock is held
// throughout, so no bind interleaves.
func (api *modelRouteAPI) selectRoute(command modelroute.SelectCommand) (modelRouteSelectResponse, error) {
	api.routes.state.guard.Lock()
	preview, err := api.routes.owner.Preview(command.Draft)
	if err != nil {
		api.routes.state.guard.Unlock()
		return modelRouteSelectResponse{}, err
	}
	if problem := routeEffortProblem(preview.Draft.Fields); problem != "" {
		api.routes.state.guard.Unlock()
		return modelRouteSelectResponse{}, &modelroute.Problem{Code: modelroute.CodeInvalid,
			Field: "fields.thinking_effort", Message: problem}
	}
	ix := api.index()
	if preview.Exists {
		if ix == nil {
			api.routes.state.guard.Unlock()
			return modelRouteSelectResponse{}, &modelroute.Problem{Code: modelroute.CodeStorage,
				Message:  "The places that use this model route cannot be read, so it cannot be edited now.",
				Recovery: "Try again once the daemon's store is available."}
		}
		prospective := modelroute.Route{RouteID: preview.Draft.RouteID, Name: preview.Draft.Name,
			Family: preview.Draft.Family, Kind: preview.Kind, Fields: preview.Draft.Fields}
		if refusal := api.placeProblems(prospective); refusal != nil {
			api.routes.state.guard.Unlock()
			return modelRouteSelectResponse{}, refusal
		}
	}
	selected, err := api.routes.owner.Select(command)
	if err != nil {
		api.routes.state.guard.Unlock()
		return modelRouteSelectResponse{}, err
	}
	response := modelRouteSelectResponse{Changed: selected.Changed, Created: selected.Created, MovedPlaces: []string{}}
	reviewMoved := false
	if selected.Changed && !selected.Created && ix != nil {
		applied, applyErr := applyRouteRevisionAfterFile(ix, selected.Route, time.Now().Unix())
		if applyErr != nil {
			// The revision file is written and is the truth; the next start's pass
			// moves the places (plan §5.3). Say so rather than report success.
			api.routes.state.guard.Unlock()
			return modelRouteSelectResponse{}, &modelroute.Problem{Code: modelroute.CodeStorage,
				Message:  "The model route was saved, but the places that use it could not be moved to it: " + applyErr.Error(),
				Recovery: "Restart the daemon: it moves every place to the saved route at start."}
		}
		response.MovedPlaces = applied.ManagedBindingIDs
		if applied.ReviewChanged {
			response.MovedPlaces = append(response.MovedPlaces, store.ReviewBindingID)
			reviewMoved = true
		}
	}
	api.routes.state.guard.Unlock()
	// Outside the route lock: the reviewer's running cache takes its own lock, and a
	// reviewer bind takes the route lock first.
	if reviewMoved {
		if host := api.review(); host != nil {
			host.reloadBinding()
		}
	}
	references, _ := api.references()
	response.Route = routeView(selected.Route, references)
	return response, nil
}

// applyRouteRevisionAfterFile is the store half of a route edit: the one transaction
// that moves every binding and chain entry to the revision the file now holds. It is a
// variable so a test can stop between the file and the transaction, the way a crash
// would (criterion 81).
var applyRouteRevisionAfterFile = func(ix *store.Index, route modelroute.Route, now int64) (store.RouteRevisionApplied, error) {
	return ix.ApplyRouteRevision(routeRevisionFor(route), reviewPathIdentity, now)
}

func (api *modelRouteAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	var request modelRouteDeleteRequest
	if err := decodeManagedJSON(w, r, &request); err != nil {
		writeModelRouteError(w, &modelroute.Problem{Code: modelroute.CodeInvalid, Message: err.Error()})
		return
	}
	if !request.Confirmed {
		writeModelRouteError(w, &modelroute.Problem{Code: modelroute.CodeUnconfirmed, Field: "confirmed",
			Message: "Confirm deleting the model route."})
		return
	}
	routeID := strings.TrimSpace(r.PathValue("id"))
	api.routes.state.guard.Lock()
	defer api.routes.state.guard.Unlock()
	err := api.routes.owner.Delete(routeID, request.ExpectedStateToken, func() ([]string, error) {
		ix := api.index()
		if ix == nil {
			return nil, errors.New("the daemon's store is unavailable")
		}
		references, err := ix.RouteReferences()
		if err != nil {
			return nil, err
		}
		holders := []string{}
		for _, reference := range references {
			if reference.RouteID == routeID {
				holders = append(holders, placeLabel(reference))
			}
		}
		return holders, nil
	})
	if err != nil {
		writeModelRouteError(w, err)
		return
	}
	writeJSON(w, modelRouteDeleteResponse{Deleted: routeID})
}

// references reads every route reference, grouped by route id. unavailable is true when
// there is no store to read or the read failed.
func (api *modelRouteAPI) references() (map[string][]store.RouteReference, bool) {
	grouped := map[string][]store.RouteReference{}
	ix := api.index()
	if ix == nil {
		return grouped, true
	}
	references, err := ix.RouteReferences()
	if err != nil {
		return grouped, true
	}
	for _, reference := range references {
		grouped[reference.RouteID] = append(grouped[reference.RouteID], reference)
	}
	return grouped, false
}

// placeProblems runs, for every place that uses a route, the checks a bind runs
// (plan §5.4 checks 1 and 2) against the route as it WOULD be: the place's mode still a
// read-only mode of the route's runtime, the pinned profile's declared locality, and,
// for the reviewer, a request path that can still be built. nil means every place may
// keep running; otherwise the refusal names the places.
func (api *modelRouteAPI) placeProblems(prospective modelroute.Route) *routeRefusal {
	ix := api.index()
	if ix == nil || prospective.RouteID == "" {
		return nil
	}
	references, err := ix.RouteReferences()
	if err != nil {
		return &routeRefusal{Code: routeRefusalPlaces, Message: "The places that use this model route could not be read; nothing was changed."}
	}
	broken, reasons := []string{}, []string{}
	for _, reference := range references {
		if reference.RouteID != prospective.RouteID {
			continue
		}
		if reason := api.placeProblem(ix, reference, prospective); reason != "" {
			broken = append(broken, placeLabel(reference))
			reasons = append(reasons, placeLabel(reference)+": "+reason)
		}
	}
	if len(broken) == 0 {
		return nil
	}
	sort.Strings(broken)
	return &routeRefusal{Code: routeRefusalPlaces, Places: broken,
		Message: "This change would stop these places from running: " + strings.Join(reasons, " · ") +
			" Nothing was changed. Choose another model route for those places first, or create a new route."}
}

func (api *modelRouteAPI) placeProblem(ix *store.Index, reference store.RouteReference, prospective modelroute.Route) string {
	if reference.Lane == "review" {
		review, found, err := ix.ReviewBinding()
		if err != nil || !found {
			return ""
		}
		review.Endpoint, review.Model = prospective.Fields.Endpoint, prospective.Fields.Model
		if _, _, err := reviewPathIdentity(review); err != nil {
			return "the reviewer needs a literal loopback endpoint and a model"
		}
		return ""
	}
	binding, found, err := ix.ManagedBinding(reference.BindingID)
	if err != nil || !found {
		return ""
	}
	mode := binding.Mode
	if reference.Fallback && reference.Position-1 < len(binding.Routes) {
		mode = binding.Routes[reference.Position-1].Mode
	}
	if _, err := managedReadOnlyMode(prospective.Fields.Runtime, mode); err != nil {
		return "its mode is not a read-only mode of that runtime"
	}
	detail, err := api.profiles.GetRevision(binding.ProfileID, binding.ProfileSourceDigest, binding.ProfileBundleDigest)
	if err != nil || detail.Normalized == nil {
		return ""
	}
	candidate := chainEntryFor(prospective, mode)
	if problem := managedRouteDestinationProblem(*detail.Normalized, candidate); problem != "" {
		return problem
	}
	if reference.Fallback {
		return ""
	}
	for _, child := range binding.AllowedProfiles {
		childDetail, childErr := api.profiles.GetRevision(child.ProfileID, child.SourceDigest, child.BundleDigest)
		if childErr != nil || childDetail.Normalized == nil {
			continue
		}
		if problem := managedRouteDestinationProblem(*childDetail.Normalized, candidate); problem != "" {
			return problem
		}
	}
	return ""
}

// previewAdmission says what the selected route rules would answer for the route on its
// own and on each place that uses it. It decides nothing; bind and run start do. The
// error is the first rulebook read that failed: the answers beside it are then over
// the rules that could be read, and the preview says so as a problem.
func (api *modelRouteAPI) previewAdmission(prospective modelroute.Route, references map[string][]store.RouteReference) ([]modelRouteAdmission, error) {
	local := routeIsLocal(prospective, "")
	bare, readErr := api.routes.admit(prospective, local, "", "")
	out := []modelRouteAdmission{{Admission: bare}}
	ix := api.index()
	for _, reference := range references[prospective.RouteID] {
		locality, mode := "", ""
		if ix != nil && reference.Lane == "managed" {
			if binding, found, err := ix.ManagedBinding(reference.BindingID); err == nil && found {
				mode = binding.Mode
				if detail, detailErr := api.profiles.GetRevision(binding.ProfileID, binding.ProfileSourceDigest, binding.ProfileBundleDigest); detailErr == nil && detail.Normalized != nil {
					locality = detail.Normalized.Requirements.Destination.Locality
				}
			}
		}
		admission, err := api.routes.admit(prospective, routeIsLocal(prospective, mode), locality, reference.ProjectRoot)
		if readErr == nil {
			readErr = err
		}
		out = append(out, modelRouteAdmission{PlaceID: reference.BindingID, Label: placeLabel(reference), Admission: admission})
	}
	return out, readErr
}

// routeIsLocal is whether a route stays on this machine: for a runtime-model route the
// runtime adapter's own claim (never an egress proof), for an inference route the
// loopback check on its endpoint.
func routeIsLocal(route modelroute.Route, mode string) bool {
	if route.Family == modelroute.FamilyInference {
		return modelroute.FactsFor(route, false, "", "").Local
	}
	return managedRouteLocal(route, mode)
}

func routeView(route modelroute.Route, references map[string][]store.RouteReference) modelRouteView {
	view := modelRouteView{RouteID: route.RouteID, Name: route.Name, Family: route.Family, Kind: route.Kind,
		Fields: route.Fields, Local: routeIsLocal(route, ""), Locality: routeLeavesThisMachine,
		RevisionDigest: route.RevisionDigest, StateToken: route.StateToken, CreatedAt: route.CreatedAt,
		MigratedAt: route.MigratedAt, Places: []modelRoutePlace{}}
	if view.Local {
		view.Locality = routeOnThisMachine
	}
	for _, reference := range references[route.RouteID] {
		view.Places = append(view.Places, modelRoutePlace{RouteReference: reference, Label: placeLabel(reference)})
	}
	return view
}

// placeLabel names a place for a person without a model id or an endpoint: the agent
// and the repository's folder name, or the reviewer.
func placeLabel(reference store.RouteReference) string {
	if reference.Lane == "review" {
		return "the reviewer (" + reference.ProfileID + ")"
	}
	label := reference.ProfileID + " in " + filepath.Base(reference.ProjectRoot)
	if reference.Fallback {
		label += " (fallback)"
	}
	return label
}

// writeModelRouteError writes a typed refusal with the status its code calls for.
func writeModelRouteError(w http.ResponseWriter, err error) {
	problem := modelRouteProblem{Code: modelroute.CodeStorage, Message: "Model route storage is unavailable."}
	var routeProblem *modelroute.Problem
	var refusal *routeRefusal
	switch {
	case errors.As(err, &routeProblem):
		problem = modelRouteProblem{Code: routeProblem.Code, Field: routeProblem.Field,
			Message: routeProblem.Message, Recovery: routeProblem.Recovery}
	case errors.As(err, &refusal):
		problem = modelRouteProblem{Code: refusal.Code, Field: refusal.Field, Message: refusal.Message, Places: refusal.Places}
	}
	status := http.StatusInternalServerError
	switch problem.Code {
	case modelroute.CodeInvalid, modelroute.CodeUnconfirmed:
		status = http.StatusUnprocessableEntity
	case modelroute.CodeNotFound:
		status = http.StatusNotFound
	case modelroute.CodeNameTaken, modelroute.CodeStateConflict, modelroute.CodeInUse, modelroute.CodeIntegrity, routeRefusalPlaces:
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, modelRouteErrorResponse{Error: problem})
}
