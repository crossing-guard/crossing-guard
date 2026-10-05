package daemon

// The Agents roster (agents-settings-redesign plan §4, §6): one row per agent
// (profile id) composed from the profile owner, its drafts, the managed
// bindings (its places) and the review binding. Composition lives here, once,
// so the browser renders facts instead of joining six payloads. Every read is
// best-effort per source: a missing managed host leaves profiles and drafts
// listed and says places cannot be read.

import (
	"errors"
	"path/filepath"
	"sort"
	"time"

	"crossing-guard/infer"
	"crossing-guard/internal/modelroute"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// errRosterStatsUnavailable stands for a lane whose host is not running.
var errRosterStatsUnavailable = errors.New("run history unavailable")

// Attention reason codes (plan §6). The page words them; the daemon decides.
const (
	attentionRevisionUnavailable = "revision_unavailable"
	attentionProfileProblem      = "profile_problem"
	attentionProviderOutage      = "provider_outage"
	attentionRecentFailures      = "recent_failures"
	attentionFlowCeiling         = "flow_ceiling_breach"
	// A place that is on and starts no run (team rest-of-release plan §4.1 decision 5,
	// §5.3, §5.4): held by its adoption, its model route unreadable, or its last run
	// start refused.
	attentionPlaceHeld    = "place_held"
	attentionRouteMissing = "route_missing"
	attentionRunRefused   = "run_refused"
)

type rosterPlace struct {
	PlaceID           string                `json:"place_id"`
	Lane              string                `json:"lane"`
	State             string                `json:"state"`
	ProjectRoot       string                `json:"project_root,omitempty"`
	Repository        string                `json:"repository,omitempty"`
	ScopeRuntime      string                `json:"scope_runtime,omitempty"`
	ScopeSession      string                `json:"scope_session,omitempty"`
	WatchNatural      bool                  `json:"watch_natural"`
	Runtime           string                `json:"runtime,omitempty"`
	Model             string                `json:"model,omitempty"`
	ThinkingEffort    *store.ThinkingEffort `json:"thinking_effort,omitempty"`
	Mode              string                `json:"mode,omitempty"`
	Endpoint          string                `json:"endpoint,omitempty"`
	Local             bool                  `json:"local"`
	Version           string                `json:"version,omitempty"`
	RevisionAvailable bool                  `json:"revision_available"`
	IsCurrent         bool                  `json:"is_current"`
	SourceDigest      string                `json:"source_digest"`
	BundleDigest      string                `json:"bundle_digest"`
	StateToken        string                `json:"state_token"`
	UpdatedAt         int64                 `json:"updated_at"`
	// The named model route the place runs on (team rest-of-release plan §5.5). An
	// agent surface shows RouteName and RouteLocal — "on this machine" or "leaves this
	// machine" — and nothing else of a route; Runtime, Model and Endpoint above stay
	// for the readers that already use them. RouteProblem is "" or a typed problem
	// (route_missing, migration_failed).
	RouteID      string `json:"route_id,omitempty"`
	RouteName    string `json:"route_name,omitempty"`
	RouteLocal   bool   `json:"route_local"`
	RouteProblem string `json:"route_problem,omitempty"`
	// HeldReason is why this place starts no run while it stays on — its team
	// adoption expired or no longer shares its version — or "".
	HeldReason string `json:"held_reason,omitempty"`
	// RunRefusal is the typed code of the last run start the review lane refused
	// (admission_refused, or a hold reason), or "". Managed places record a refused
	// run as a run row instead.
	RunRefusal string `json:"run_refusal,omitempty"`
	// Managed lane.
	GrantedAuthority []string             `json:"granted_authority,omitempty"`
	AutoAction       bool                 `json:"auto_action"`
	Priority         int64                `json:"priority"`
	DeclaredTags     []string             `json:"declared_tags,omitempty"`
	Limits           store.ManagedLimits  `json:"limits"`
	Routes           []store.ManagedRoute `json:"routes,omitempty"`
	// FallbackRoutes is the fallback chain as an agent surface may show it: each
	// entry's route by name, in chain order, beside Routes' stored copy.
	FallbackRoutes []rosterFallbackRoute `json:"fallback_routes,omitempty"`
	// Review lane.
	Effect                string `json:"effect,omitempty"`
	RuntimeFilter         string `json:"runtime_filter,omitempty"`
	ApprovalSubdeadlineMS int    `json:"approval_subdeadline_ms,omitempty"`
	AnswerChoicePrompts   bool   `json:"answer_choice_prompts"`
	TimeoutMS             int    `json:"timeout_ms,omitempty"`
}

// rosterFallbackRoute is one fallback chain entry by its route's name. Missing is true
// when the entry names a route that can no longer be read (the outage reroute passes
// over it); an entry with no route yet has neither a name nor Missing.
type rosterFallbackRoute struct {
	RouteID    string `json:"route_id,omitempty"`
	RouteName  string `json:"route_name,omitempty"`
	RouteLocal bool   `json:"route_local"`
	Mode       string `json:"mode,omitempty"`
	Missing    bool   `json:"missing,omitempty"`
}

// rosterOutage is one tripped provider route with the names of the model routes that
// resolve to it, so an agent surface can lead with a route's name and show no model id
// (plan §14 Q9). RouteNames is empty when no readable route resolves to the outage.
type rosterOutage struct {
	providerOutageFact
	RouteNames []string `json:"route_names"`
}

type rosterAgent struct {
	ProfileID          string              `json:"profile_id"`
	Name               string              `json:"name"`
	Description        string              `json:"description"`
	Version            string              `json:"version,omitempty"`
	AgentType          string              `json:"agent_type"`
	Lane               string              `json:"lane,omitempty"`
	HasDraft           bool                `json:"has_draft"`
	DraftOnly          bool                `json:"draft_only"`
	Problem            *profilefs.Problem  `json:"problem,omitempty"`
	Compatible         bool                `json:"compatible"`
	IncompatibleReason string              `json:"incompatible_reason,omitempty"`
	Places             []rosterPlace       `json:"places"`
	Stats              store.OutcomeWindow `json:"stats"`
	StatsUnavailable   bool                `json:"stats_unavailable,omitempty"`
	Attention          string              `json:"attention,omitempty"`
	// Origin is where the agent's current selection came from when a team adoption
	// wrote it (plan §4.1 decision 5); nil for an agent of the member's own.
	Origin *placeAdoptionOrigin `json:"origin,omitempty"`
	// Collision is set on a member's own agent whose id an offered team agent also
	// uses (OD-6): that team agent will not be adopted and this one is never replaced.
	Collision *rosterCollision `json:"collision,omitempty"`
}

type rosterResponse struct {
	Agents            []rosterAgent           `json:"agents"`
	StatsDays         int                     `json:"stats_days"`
	TZOffsetMinutes   int                     `json:"tz_offset_minutes"`
	PlacesUnavailable bool                    `json:"places_unavailable"`
	Problems          []profilefs.ListProblem `json:"problems"`
	// Outages are the tripped provider routes, each with its parked runs
	// (provider-outage plan Slice D); the index offers their reroute.
	Outages []rosterOutage `json:"outages"`
}

// rosterSources carries the owners one composition reads. managed or review
// may be nil (host unavailable).
type rosterSources struct {
	profiles *profilefs.Owner
	managed  *orchestrationManagedHost
	review   *orchestrationReviewHost
}

type rosterComposer struct {
	sources   rosterSources
	now       int64
	offset    int64
	config    RosterConfig
	bindings  []store.ManagedBinding
	placesErr bool
	options   map[string]managedProfileOption
	reviews   map[string]reviewProfileOption
	reviewRow *store.ReviewBinding
	outages   []providerOutageFact
	// routeNames is every readable route's name by id, read once per composition.
	routeNames map[string]modelroute.Route
	// collisions is every offered team agent id a member's own agent uses.
	collisions map[string]rosterCollision
	// managedAdoptions and reviewAdoptions are each lane's adoption owner.
	managedAdoptions placeAdoptions
	reviewAdoptions  placeAdoptions
}

func newRosterComposer(sources rosterSources, tzOffsetMinutes int) *rosterComposer {
	composer := &rosterComposer{sources: sources, now: time.Now().Unix(), offset: int64(tzOffsetMinutes) * 60,
		config: orchestrationConfig().Roster, options: map[string]managedProfileOption{},
		reviews: map[string]reviewProfileOption{}, routeNames: map[string]modelroute.Route{},
		managedAdoptions: noPlaceAdoptions{}, reviewAdoptions: noPlaceAdoptions{},
		collisions: teamIDCollisions()}
	if routes, err := modelRoutesBeside(sources.profiles); err == nil {
		if listed, listErr := routes.owner.List(); listErr == nil {
			for _, route := range listed.Routes {
				composer.routeNames[route.RouteID] = route
			}
		}
	}
	if sources.managed != nil {
		composer.managedAdoptions = sources.managed.placeAdoptions()
	}
	if sources.review != nil {
		sources.review.mu.RLock()
		composer.reviewAdoptions = sources.review.adoptions
		sources.review.mu.RUnlock()
	}
	if sources.managed != nil {
		bindings, err := sources.managed.ix.ManagedBindings(false)
		composer.bindings, composer.placesErr = bindings, err != nil
		composer.outages = sources.managed.providerOutageFacts()
	} else {
		composer.placesErr = true
	}
	if options, err := managedProfileOptions(sources.profiles); err == nil {
		for _, option := range options {
			composer.options[option.ProfileID] = option
		}
	}
	if options, err := reviewProfileOptions(sources.profiles); err == nil {
		for _, option := range options {
			composer.reviews[option.ProfileID] = option
		}
	}
	if sources.review != nil {
		if binding, found, err := sources.review.ix.ReviewBinding(); err == nil && found {
			composer.reviewRow = &binding
		}
	}
	return composer
}

// roster lists every agent: selected profiles, never-published drafts, and
// bindings whose profile is gone.
func (composer *rosterComposer) roster() (rosterResponse, error) {
	listed, err := composer.sources.profiles.List()
	if err != nil {
		return rosterResponse{}, err
	}
	drafts, err := composer.sources.profiles.Drafts()
	if err != nil {
		return rosterResponse{}, err
	}
	draftByID := map[string]profilefs.Draft{}
	for _, draft := range drafts {
		draftByID[draft.ProfileID] = draft
	}
	ids := map[string]bool{}
	agents := []rosterAgent{}
	for _, summary := range listed.Profiles {
		ids[summary.ProfileID] = true
		_, hasDraft := draftByID[summary.ProfileID]
		agents = append(agents, composer.agentFromSummary(summary, hasDraft))
	}
	for _, draft := range drafts {
		if !ids[draft.ProfileID] {
			ids[draft.ProfileID] = true
			agents = append(agents, composer.agentFromDraft(draft))
		}
	}
	for _, binding := range composer.bindings {
		if !ids[binding.ProfileID] {
			ids[binding.ProfileID] = true
			agents = append(agents, composer.orphanAgent(binding.ProfileID))
		}
	}
	if composer.reviewRow != nil && !ids[composer.reviewRow.ProfileID] {
		agents = append(agents, composer.orphanAgent(composer.reviewRow.ProfileID))
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].ProfileID < agents[j].ProfileID })
	outages := composer.namedOutages()
	return rosterResponse{Agents: agents, StatsDays: composer.config.StatsDays,
		TZOffsetMinutes: int(composer.offset / 60), PlacesUnavailable: composer.placesErr,
		Problems: append(append([]profilefs.ListProblem{}, orchestrationConfigProblems()...), listed.Problems...), Outages: outages}, nil
}

func (composer *rosterComposer) agentFromSummary(summary profilefs.ProfileSummary, hasDraft bool) rosterAgent {
	agent := rosterAgent{ProfileID: summary.ProfileID, Name: summary.Name, Description: summary.Description,
		Version: summary.Version, HasDraft: hasDraft, Problem: summary.Problem}
	if detail, err := composer.sources.profiles.GetRevision(summary.ProfileID, summary.SourceDigest, summary.BundleDigest); err == nil && detail.Normalized != nil {
		agent.AgentType = detail.Normalized.AgentType()
	}
	composer.fill(&agent)
	return agent
}

// origin is the adoption that wrote an agent's current selection, asked of whichever
// lane's adoption owner knows it.
func (composer *rosterComposer) origin(profileID string) *placeAdoptionOrigin {
	for _, adoptions := range []placeAdoptions{composer.managedAdoptions, composer.reviewAdoptions} {
		if origin, ok := adoptions.Origin(profileID); ok {
			return &origin
		}
	}
	return nil
}

// routeFacts fills what an agent surface may show of a place's route: its name and
// whether it stays on this machine. A place with no route yet says only the latter.
func (composer *rosterComposer) routeFacts(place *rosterPlace, routeID, problem string, local bool) {
	place.RouteID, place.RouteProblem, place.RouteLocal = routeID, problem, local
	if route, ok := composer.routeNames[routeID]; ok {
		place.RouteName = route.Name
	}
}

// fallbackRoutes names each fallback chain entry's route for an agent surface.
func (composer *rosterComposer) fallbackRoutes(chain []store.ManagedRoute) []rosterFallbackRoute {
	out := make([]rosterFallbackRoute, 0, len(chain))
	for _, entry := range chain {
		local, _ := chatRouteIsLocal(ChatRequest{Runtime: entry.Runtime, Model: entry.Model, Mode: entry.Mode})
		item := rosterFallbackRoute{RouteID: entry.RouteID, RouteLocal: local, Mode: entry.Mode}
		if route, ok := composer.routeNames[entry.RouteID]; ok {
			item.RouteName = route.Name
		} else if entry.RouteID != "" {
			item.Missing = true
		}
		out = append(out, item)
	}
	return out
}

// namedOutages gives each tripped provider route the names of the model routes that
// resolve to its runtime and model, sorted.
func (composer *rosterComposer) namedOutages() []rosterOutage {
	out := make([]rosterOutage, 0, len(composer.outages))
	for _, outage := range composer.outages {
		named := rosterOutage{providerOutageFact: outage, RouteNames: []string{}}
		for _, route := range composer.routeNames {
			if route.Family == modelroute.FamilyRuntimeModel && route.Fields.Runtime == outage.Runtime && route.Fields.Model == outage.Model {
				named.RouteNames = append(named.RouteNames, route.Name)
			}
		}
		sort.Strings(named.RouteNames)
		out = append(out, named)
	}
	return out
}

func (composer *rosterComposer) agentFromDraft(draft profilefs.Draft) rosterAgent {
	agent := rosterAgent{ProfileID: draft.ProfileID, Name: draft.ProfileID, HasDraft: true, DraftOnly: true}
	if draft.Normalized != nil {
		agent.Name, agent.Description = draft.Normalized.Name, draft.Normalized.Description
		agent.Version, agent.AgentType = draft.Normalized.Version, draft.Normalized.AgentType()
	}
	composer.fill(&agent)
	return agent
}

func (composer *rosterComposer) orphanAgent(profileID string) rosterAgent {
	agent := rosterAgent{ProfileID: profileID, Name: profileID}
	composer.fill(&agent)
	return agent
}

// fill adds the lane, compatibility, places, stats and attention.
func (composer *rosterComposer) fill(agent *rosterAgent) {
	if agent.AgentType == "" {
		agent.AgentType = "unknown"
	}
	switch agent.AgentType {
	case "reviewer":
		agent.Lane = "review"
		option, ok := composer.reviews[agent.ProfileID]
		agent.Compatible, agent.IncompatibleReason = ok && option.Compatible, option.Reason
	case "follower", "helper":
		agent.Lane = "managed"
		option, ok := composer.options[agent.ProfileID]
		agent.Compatible, agent.IncompatibleReason = ok && option.Compatible, option.Reason
	}
	agent.Places = composer.places(agent.ProfileID)
	agent.Origin = composer.origin(agent.ProfileID)
	if collision, ok := composer.collisions[agent.ProfileID]; ok && agent.Origin == nil {
		agent.Collision = &collision
	}
	// A window that cannot be read is said to be unavailable, never zero runs.
	scope := store.HistoryScope{ProfileID: agent.ProfileID}
	var window store.OutcomeWindow
	err := errRosterStatsUnavailable
	if agent.Lane == "review" && composer.sources.review != nil {
		window, err = composer.sources.review.ix.ReviewOutcomeWindow(scope, composer.now, composer.offset, composer.config.StatsDays)
	} else if agent.Lane != "review" && composer.sources.managed != nil {
		window, err = composer.sources.managed.ix.ManagedOutcomeWindow(scope, composer.now, composer.offset, composer.config.StatsDays)
	}
	agent.Stats, agent.StatsUnavailable = window, err != nil
	agent.Attention = composer.attention(*agent)
}

func (composer *rosterComposer) places(profileID string) []rosterPlace {
	out := []rosterPlace{}
	current, _ := composer.sources.profiles.Get(profileID)
	for _, binding := range composer.bindings {
		if binding.ProfileID != profileID {
			continue
		}
		place := rosterPlace{PlaceID: binding.BindingID, Lane: "managed", State: binding.State,
			ProjectRoot: binding.ProjectRoot, Repository: filepath.Base(binding.ProjectRoot),
			ScopeRuntime: binding.ScopeRuntime, ScopeSession: binding.ScopeSession, WatchNatural: binding.WatchNatural,
			Runtime: binding.Runtime, Model: binding.Model, ThinkingEffort: binding.ThinkingEffort, Mode: binding.Mode,
			SourceDigest: binding.ProfileSourceDigest, BundleDigest: binding.ProfileBundleDigest,
			StateToken: binding.StateToken, UpdatedAt: binding.UpdatedAt,
			GrantedAuthority: append([]string{}, binding.Authority...), AutoAction: binding.AutoAction,
			Priority: binding.Priority, DeclaredTags: append([]string{}, binding.DeclaredTags...),
			Limits: binding.Limits, Routes: binding.Routes, FallbackRoutes: composer.fallbackRoutes(binding.Routes)}
		local, _ := chatRouteIsLocal(ChatRequest{Runtime: binding.Runtime, Model: binding.Model, Mode: binding.Mode})
		composer.routeFacts(&place, binding.RouteID, binding.RouteProblem, local)
		place.HeldReason = composer.managedAdoptions.HoldReason(binding.AdoptionKey, binding.ProfileID,
			binding.ProfileSourceDigest, binding.ProfileBundleDigest)
		composer.pin(&place, current)
		out = append(out, place)
	}
	if row := composer.reviewRow; row != nil && row.ProfileID == profileID {
		place := rosterPlace{PlaceID: row.BindingID, Lane: "review", State: row.State, Model: row.Model,
			Endpoint: row.Endpoint, SourceDigest: row.ProfileSourceDigest, BundleDigest: row.ProfileBundleDigest,
			StateToken: row.StateToken, UpdatedAt: row.UpdatedAt, Effect: row.Effect,
			RuntimeFilter: row.RuntimeFilter, ApprovalSubdeadlineMS: row.ApprovalSubdeadlineMS,
			AnswerChoicePrompts: row.AnswerChoicePrompts, TimeoutMS: row.TimeoutMS}
		if _, err := infer.CanonicalLoopbackEndpoint(row.Endpoint); err == nil {
			place.Local = true
		}
		composer.routeFacts(&place, row.RouteID, row.RouteProblem, place.Local)
		place.HeldReason = composer.reviewAdoptions.HoldReason(row.AdoptionKey, row.ProfileID,
			row.ProfileSourceDigest, row.ProfileBundleDigest)
		if refusal := composer.sources.review.runRefusal(); refusal != nil {
			place.RunRefusal = refusal.Code
		}
		composer.pin(&place, current)
		out = append(out, place)
	}
	return out
}

// pin resolves a place's pinned revision to its version, and whether it is
// the agent's current one.
func (composer *rosterComposer) pin(place *rosterPlace, current profilefs.Detail) {
	detail, err := composer.sources.profiles.GetRevision(current.ProfileID, place.SourceDigest, place.BundleDigest)
	if current.ProfileID == "" || err != nil {
		return
	}
	place.RevisionAvailable = true
	place.Version = detail.Current.Version
	place.IsCurrent = current.Current.SourceDigest == place.SourceDigest && current.Current.BundleDigest == place.BundleDigest
}

func (composer *rosterComposer) attention(agent rosterAgent) string {
	enabled := []string{}
	for _, place := range agent.Places {
		if !place.RevisionAvailable {
			return attentionRevisionUnavailable
		}
		if place.State == "enabled" && place.Lane == "managed" {
			enabled = append(enabled, place.PlaceID)
		}
	}
	if agent.Problem != nil {
		return attentionProfileProblem
	}
	if code := placeStoppedAttention(agent.Places); code != "" {
		return code
	}
	for _, place := range agent.Places {
		for _, outage := range composer.outages {
			if place.State == "enabled" && place.Lane == "managed" && outage.Runtime == place.Runtime && outage.Model == place.Model {
				return attentionProviderOutage
			}
		}
	}
	// Flow ceiling breach is a FIRST-CLASS failure kind (orchestration-flows
	// pilot pass-2 C1): any single settled run with error_class
	// flow_ceiling_breach counts directly — one breach interleaved with
	// successes must surface, not wait out the all-failed threshold.
	if agent.Lane == "managed" && composer.sources.managed != nil {
		breached, err := composer.sources.managed.ix.FlowCeilingBreachActive(agent.ProfileID)
		if err == nil && breached {
			return attentionFlowCeiling
		}
	}
	threshold := composer.config.AttentionFailedRuns
	var recent []string
	var err error
	switch {
	case agent.Lane == "review" && composer.sources.review != nil && reviewPlaceEnabled(agent.Places):
		recent, err = composer.sources.review.ix.RecentSettledReviewOutcomes(agent.ProfileID, threshold)
	case agent.Lane == "managed" && composer.sources.managed != nil && len(enabled) > 0:
		recent, err = composer.sources.managed.ix.RecentSettledOutcomes(agent.ProfileID, enabled, threshold)
	}
	if err != nil || len(recent) < threshold {
		return ""
	}
	for _, outcome := range recent {
		if outcome != store.OutcomeFailed {
			return ""
		}
	}
	return attentionRecentFailures
}

// placeStoppedAttention is the attention code for the first place that is on and starts
// no run: held by its adoption, its model route unreadable, or its last run refused.
func placeStoppedAttention(places []rosterPlace) string {
	for _, place := range places {
		if place.State != "enabled" {
			continue
		}
		switch {
		case place.HeldReason != "":
			return attentionPlaceHeld
		case place.RouteProblem == routeRefusalMissing:
			return attentionRouteMissing
		case place.RunRefusal != "":
			return attentionRunRefused
		}
	}
	return ""
}

func reviewPlaceEnabled(places []rosterPlace) bool {
	for _, place := range places {
		if place.Lane == "review" && place.State == "enabled" {
			return true
		}
	}
	return false
}

// governorPinSource is the pin source for one profile's history trims. It is
// never nil — a nil source means "no pins, proceed" to profilefs — so without
// the governor's store it answers an error and the trim is refused.
func governorPinSource(profileID string) profilefs.PinSource {
	return func() ([]profilefs.RevisionRef, error) {
		if governor == nil {
			return nil, errors.New("the governor did not start, so the places running this agent cannot be read")
		}
		return pinnedRevisions(governor.ix, profileID)
	}
}

// pinnedRevisions lists the revisions still in use for one agent — places
// that run it and helpers that may launch it as a child — read from the store
// itself (a host that failed to start still has bindings), so a selection never
// evicts one of them. A read error is returned, never treated as "no pins".
func pinnedRevisions(ix *store.Index, profileID string) ([]profilefs.RevisionRef, error) {
	out := []profilefs.RevisionRef{}
	bindings, err := ix.ManagedBindings(false)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if binding.ProfileID == profileID {
			out = append(out, profilefs.RevisionRef{SourceDigest: binding.ProfileSourceDigest,
				BundleDigest: binding.ProfileBundleDigest, Holder: filepath.Base(binding.ProjectRoot)})
		}
		for _, child := range binding.AllowedProfiles {
			if child.ProfileID == profileID {
				out = append(out, profilefs.RevisionRef{SourceDigest: child.SourceDigest,
					BundleDigest: child.BundleDigest, Holder: binding.ProfileID + " (may launch it)"})
			}
		}
	}
	review, found, err := ix.ReviewBinding()
	if err != nil {
		return nil, err
	}
	if found && review.ProfileID == profileID {
		out = append(out, profilefs.RevisionRef{SourceDigest: review.ProfileSourceDigest,
			BundleDigest: review.ProfileBundleDigest, Holder: "the reviewer"})
	}
	return out, nil
}
