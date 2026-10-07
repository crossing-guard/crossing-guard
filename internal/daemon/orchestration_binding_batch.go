package daemon

// Batch binding writes and place ids for the Agents pages
// (agents-settings-redesign plan §4, §6). A batch change names the SECTIONS it
// edits; the daemon merges only those onto each place's own current binding,
// so a place that deliberately differs keeps its other settings (D-2, RT-1).
// Every change is built before the one write transaction; turning a place off
// is state-only and never depends on the rest of the row still validating.

import (
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"crossing-guard/store"
)

// bindingPatch holds the sections a change edits; a nil section is kept from
// the place's current binding.
type bindingPatch struct {
	Revision    *bindingRevisionPatch    `json:"revision,omitempty"`
	Model       *bindingModelPatch       `json:"model,omitempty"`
	Permissions *bindingPermissionsPatch `json:"permissions,omitempty"`
	Tags        *[]string                `json:"declared_tags,omitempty"`
	Priority    *int64                   `json:"priority,omitempty"`
	Budgets     *bindingBudgetsPatch     `json:"budgets,omitempty"`
	Fallback    *[]bindingRouteRequest   `json:"fallback,omitempty"`
	Scope       *bindingScopePatch       `json:"scope,omitempty"`
}

type bindingRevisionPatch struct {
	SourceDigest string `json:"source_digest"`
	BundleDigest string `json:"bundle_digest"`
}

// bindingModelPatch is the "model" section of a change: the named model route the place
// runs on, and the place's own mode. The typed fields a route replaced are refused by
// name (plan §5.3).
type bindingModelPatch struct {
	RouteID string `json:"route_id"`
	Mode    string `json:"mode"`
	typedModelFields
}

// typedModelFields are the fields a binding write may no longer carry: where the model
// call goes is a route's business (plan §5.3, criterion 56). They decode so the refusal
// can name the one that was sent.
type typedModelFields struct {
	Runtime        refusedField `json:"runtime"`
	Model          refusedField `json:"model"`
	ThinkingEffort refusedField `json:"thinking_effort"`
	Endpoint       refusedField `json:"endpoint"`
}

// refusal returns the typed refusal for the first typed field present, or nil. prefix
// locates the fields in the request ("", "set.model.", "routes[0].").
func (fields typedModelFields) refusal(prefix string) error {
	return firstTypedModelField(prefix, []string{"runtime", "endpoint", "model", "thinking_effort"},
		fields.Runtime, fields.Endpoint, fields.Model, fields.ThinkingEffort)
}

// bindingRouteRequest is one fallback chain entry in a request: a route and a mode.
type bindingRouteRequest struct {
	RouteID string `json:"route_id"`
	Mode    string `json:"mode"`
	typedModelFields
}

// chainFromRequest refuses any entry that still carries a typed field, then returns
// the entries as command entries.
func chainFromRequest(prefix string, entries []bindingRouteRequest) ([]bindingRouteEntry, error) {
	out := make([]bindingRouteEntry, 0, len(entries))
	for index, entry := range entries {
		if err := entry.refusal(prefix + "[" + strconv.Itoa(index) + "]."); err != nil {
			return nil, err
		}
		out = append(out, bindingRouteEntry{RouteID: strings.TrimSpace(entry.RouteID), Mode: entry.Mode})
	}
	return out, nil
}

type bindingPermissionsPatch struct {
	GrantedAuthority []string `json:"granted_authority"`
	AutoAction       bool     `json:"auto_action"`
}

// bindingBudgetsPatch carries only the budgets the daemon enforces; the rest
// of the stored limits round-trip untouched.
type bindingBudgetsPatch struct {
	MaxTotal   int64 `json:"max_total"`
	LoopBudget int64 `json:"loop_budget"`
}

type bindingScopePatch struct {
	ProjectRoot  string `json:"project_root"`
	ScopeRuntime string `json:"scope_runtime"`
	ScopeSession string `json:"scope_session"`
	WatchNatural bool   `json:"watch_natural"`
}

// bindingPlace is a full new place (op "create").
type bindingPlace struct {
	ProfileID           string                `json:"profile_id"`
	ProfileSourceDigest string                `json:"profile_source_digest"`
	ProfileBundleDigest string                `json:"profile_bundle_digest"`
	ProjectRoot         string                `json:"project_root"`
	ScopeRuntime        string                `json:"scope_runtime"`
	ScopeSession        string                `json:"scope_session"`
	WatchNatural        bool                  `json:"watch_natural"`
	RouteID             string                `json:"route_id"`
	Mode                string                `json:"mode"`
	GrantedAuthority    []string              `json:"granted_authority"`
	AutoAction          bool                  `json:"auto_action"`
	Priority            int64                 `json:"priority"`
	DeclaredTags        []string              `json:"declared_tags"`
	Limits              store.ManagedLimits   `json:"limits"`
	Routes              []bindingRouteRequest `json:"routes"`
	State               string                `json:"state"`
	typedModelFields
}

// typedFieldRefusal names the first typed model field any change of a batch carries.
func typedFieldRefusal(changes []bindingBatchChange) error {
	for _, change := range changes {
		id := strings.TrimSpace(change.BindingID)
		if place := change.Place; place != nil {
			if err := place.refusal(id + ": place."); err != nil {
				return err
			}
			if _, err := chainFromRequest(id+": place.routes", place.Routes); err != nil {
				return err
			}
		}
		if change.Set == nil {
			continue
		}
		if model := change.Set.Model; model != nil {
			if err := model.refusal(id + ": set.model."); err != nil {
				return err
			}
		}
		if fallback := change.Set.Fallback; fallback != nil {
			if _, err := chainFromRequest(id+": set.fallback", *fallback); err != nil {
				return err
			}
		}
	}
	return nil
}

type bindingBatchChange struct {
	BindingID          string        `json:"binding_id"`
	ExpectedStateToken string        `json:"expected_state_token"`
	Op                 string        `json:"op"`
	Set                *bindingPatch `json:"set,omitempty"`
	Place              *bindingPlace `json:"place,omitempty"`
}

type bindingBatchRequest struct {
	Changes      []bindingBatchChange `json:"changes"`
	ValidateOnly bool                 `json:"validate_only"`
	Confirmed    bool                 `json:"confirmed"`
}

// bindingBatchResult is one change's outcome: its written binding, or (with
// validate_only, or when validation failed) why it would be refused.
type bindingBatchResult struct {
	BindingID string                `json:"binding_id"`
	Valid     bool                  `json:"valid"`
	Problem   string                `json:"problem,omitempty"`
	Binding   *store.ManagedBinding `json:"binding,omitempty"`
}

type bindingBatchResponse struct {
	Results []bindingBatchResult `json:"results"`
	Written bool                 `json:"written"`
}

func handleBindingBatch(w http.ResponseWriter, r *http.Request, host *orchestrationManagedHost) {
	var request bindingBatchRequest
	if err := decodeManagedJSON(w, r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !request.Confirmed {
		http.Error(w, "explicit batch confirmation is required", http.StatusUnprocessableEntity)
		return
	}
	limit := orchestrationConfig().Roster.MaxBatch
	if len(request.Changes) == 0 || len(request.Changes) > limit {
		http.Error(w, "a batch carries between 1 and "+strconv.Itoa(limit)+" changes", http.StatusBadRequest)
		return
	}
	// A change that still sends a runtime, model or endpoint is refused by name before
	// anything is resolved (plan §5.3).
	if err := typedFieldRefusal(request.Changes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	changes, results, err := host.resolveBatch(request.Changes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.ValidateOnly || !batchValid(results) {
		status := http.StatusOK
		if !request.ValidateOnly {
			status = http.StatusUnprocessableEntity
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		writeJSON(w, bindingBatchResponse{Results: results})
		return
	}
	writes, err := host.applyBindingChanges(changes)
	if err != nil {
		http.Error(w, err.Error(), bindingWriteStatus(err))
		return
	}
	for index := range writes {
		saved := writes[index].Saved
		results[index].Binding = &saved
	}
	writeJSON(w, bindingBatchResponse{Results: results, Written: true})
}

func batchValid(results []bindingBatchResult) bool {
	for _, result := range results {
		if !result.Valid {
			return false
		}
	}
	return true
}

// resolveBatch turns each requested change into a buildable change and
// validates it (reads only). A malformed request is an error; a change that
// fails validation is reported in its result so the page can name it.
func (host *orchestrationManagedHost) resolveBatch(requested []bindingBatchChange) ([]bindingChange, []bindingBatchResult, error) {
	seen := map[string]bool{}
	changes := make([]bindingChange, 0, len(requested))
	results := make([]bindingBatchResult, 0, len(requested))
	for _, change := range requested {
		id := strings.TrimSpace(change.BindingID)
		if id == "" || seen[id] {
			return nil, nil, errors.New("every change names one distinct binding_id")
		}
		seen[id] = true
		if change.ExpectedStateToken == "" {
			return nil, nil, errors.New(id + ": expected_state_token is required")
		}
		resolved, problem := host.resolveBatchChange(id, change)
		results = append(results, bindingBatchResult{BindingID: id, Valid: problem == "", Problem: problem})
		changes = append(changes, resolved)
	}
	return changes, results, nil
}

func (host *orchestrationManagedHost) resolveBatchChange(id string, change bindingBatchChange) (bindingChange, string) {
	switch change.Op {
	case "disable":
		return bindingChange{disable: true, id: id, expected: change.ExpectedStateToken}, ""
	case "create":
		if change.Place == nil {
			return bindingChange{}, "a created place needs its settings"
		}
		command := commandFromPlace(id, *change.Place, change.ExpectedStateToken)
		if _, err := host.buildBinding(command); err != nil {
			return bindingChange{command: command}, err.Error()
		}
		return bindingChange{command: command}, ""
	case "update", "enable":
		current, found, err := host.ix.ManagedBinding(id)
		if err != nil {
			return bindingChange{}, "this place could not be read; try again"
		}
		if !found {
			return bindingChange{}, "this place no longer exists; reload"
		}
		command := commandFromBinding(current, change.ExpectedStateToken)
		if change.Op == "enable" {
			command.State = "enabled"
		}
		if change.Set != nil {
			applyBindingPatch(&command, *change.Set)
		}
		if _, err := host.buildBinding(command); err != nil {
			return bindingChange{command: command}, err.Error()
		}
		return bindingChange{command: command}, ""
	default:
		return bindingChange{}, "op must be create, update, enable or disable"
	}
}

// commandFromBinding restates a stored binding as the command that would
// write it again unchanged: every field round-trips (child revisions
// included), declared tags stay an explicit list (nil would mean "grant every
// tag the profile may apply"). Priority 0 still means the profile's default.
func commandFromBinding(binding store.ManagedBinding, expected string) managedBindingCommand {
	tags := append([]string{}, binding.DeclaredTags...)
	return managedBindingCommand{BindingID: binding.BindingID, ProfileID: binding.ProfileID,
		ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		ScopeRuntime: binding.ScopeRuntime, ScopeSession: binding.ScopeSession, ProjectRoot: binding.ProjectRoot,
		RouteID: binding.RouteID, keepUnrouted: binding.RouteID == "", Mode: binding.Mode,
		GrantedAuthority: append([]string{}, binding.Authority...), AutoAction: binding.AutoAction,
		Priority: binding.Priority, DeclaredTags: tags, Limits: binding.Limits,
		WatchNatural: binding.WatchNatural, Routes: chainFromBinding(binding.Routes),
		State: binding.State, ExpectedStateToken: expected,
		AllowedProfiles: append([]store.ManagedProfileRef{}, binding.AllowedProfiles...)}
}

// chainFromBinding restates a stored chain: each entry by its route, and an entry that
// predates routes by its stored copy.
func chainFromBinding(routes []store.ManagedRoute) []bindingRouteEntry {
	out := make([]bindingRouteEntry, 0, len(routes))
	for _, route := range routes {
		entry := bindingRouteEntry{RouteID: route.RouteID, Mode: route.Mode}
		if route.RouteID == "" {
			kept := route
			entry.kept = &kept
		}
		out = append(out, entry)
	}
	return out
}

func commandFromPlace(id string, place bindingPlace, expected string) managedBindingCommand {
	// typedFieldRefusal already refused a chain entry with a typed field.
	chain, _ := chainFromRequest("", place.Routes)
	return managedBindingCommand{BindingID: id, ProfileID: place.ProfileID,
		ProfileSourceDigest: place.ProfileSourceDigest, ProfileBundleDigest: place.ProfileBundleDigest,
		ScopeRuntime: place.ScopeRuntime, ScopeSession: place.ScopeSession, ProjectRoot: place.ProjectRoot,
		RouteID: strings.TrimSpace(place.RouteID), Mode: place.Mode, GrantedAuthority: place.GrantedAuthority,
		AutoAction: place.AutoAction, Priority: place.Priority, DeclaredTags: place.DeclaredTags,
		Limits: place.Limits, WatchNatural: place.WatchNatural, Routes: chain, State: place.State,
		ExpectedStateToken: expected}
}

// applyBindingPatch overwrites only the named sections.
func applyBindingPatch(command *managedBindingCommand, patch bindingPatch) {
	if patch.Revision != nil {
		command.ProfileSourceDigest, command.ProfileBundleDigest = patch.Revision.SourceDigest, patch.Revision.BundleDigest
		command.AllowedProfiles = nil // a new version re-pins its children
	}
	if patch.Model != nil {
		// Choosing a route ends the unrouted restatement: the place now names one.
		command.RouteID, command.Mode, command.keepUnrouted = strings.TrimSpace(patch.Model.RouteID), patch.Model.Mode, false
	}
	if patch.Permissions != nil {
		command.GrantedAuthority = append([]string{}, patch.Permissions.GrantedAuthority...)
		command.AutoAction = patch.Permissions.AutoAction
	}
	if patch.Tags != nil {
		command.DeclaredTags = append([]string{}, (*patch.Tags)...)
	}
	if patch.Priority != nil {
		command.Priority = *patch.Priority
	}
	if patch.Budgets != nil {
		command.Limits.MaxTotal, command.Limits.LoopBudget = patch.Budgets.MaxTotal, patch.Budgets.LoopBudget
	}
	if patch.Fallback != nil {
		command.Routes, _ = chainFromRequest("", *patch.Fallback)
	}
	if patch.Scope != nil {
		command.ProjectRoot, command.ScopeRuntime = patch.Scope.ProjectRoot, patch.Scope.ScopeRuntime
		command.ScopeSession, command.WatchNatural = patch.Scope.ScopeSession, patch.Scope.WatchNatural
	}
}

/* ---------- new place ids ---------- */

type newBindingIDRequest struct {
	ProfileID   string `json:"profile_id"`
	ProjectRoot string `json:"project_root"`
}

type newBindingIDResponse struct {
	BindingID          string `json:"binding_id"`
	ExpectedStateToken string `json:"expected_state_token"`
}

// bindingIDSuffixSearch bounds the "-2", "-3", … search for a free place id.
// It is a termination bound, not a policy: the error names it.
const bindingIDSuffixSearch = 99

var bindingIDUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

func handleNewBindingID(w http.ResponseWriter, r *http.Request, host *orchestrationManagedHost) {
	var request newBindingIDRequest
	if err := decodeManagedJSON(w, r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := freeBindingID(request.ProfileID, request.ProjectRoot, func(candidate string) (bool, error) {
		_, found, err := host.ix.ManagedBinding(candidate)
		return found, err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, newBindingIDResponse{BindingID: id, ExpectedStateToken: store.ManagedBindingAbsentToken(id)})
}

// freeBindingID derives "<profile>--<repository basename>" within the binding
// id pattern, suffixing "-2", "-3", … while an id is taken. Only the basename
// is used, never the path.
func freeBindingID(profileID, projectRoot string, taken func(string) (bool, error)) (string, error) {
	if !managedBindingIDPattern.MatchString(profileID) {
		return "", errors.New("profile_id is not a stable id")
	}
	repo := strings.Trim(bindingIDUnsafe.ReplaceAllString(strings.ToLower(filepath.Base(filepath.Clean(projectRoot))), "-"), "-._")
	if repo == "" || repo == "." {
		repo = "place"
	}
	base := profileID + "--" + repo
	for attempt := 1; attempt <= bindingIDSuffixSearch; attempt++ {
		suffix := ""
		if attempt > 1 {
			suffix = "-" + strconv.Itoa(attempt)
		}
		candidate := strings.TrimRight(truncateID(base, 64-len(suffix)), "-._") + suffix
		if !managedBindingIDPattern.MatchString(candidate) {
			return "", errors.New("could not derive a valid place id")
		}
		found, err := taken(candidate)
		if err != nil {
			return "", err
		}
		if !found {
			return candidate, nil
		}
	}
	return "", errors.New("no free place id within " + strconv.Itoa(bindingIDSuffixSearch) + " suffixes")
}

func truncateID(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
