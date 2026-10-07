package daemon

// Roster routes (agents-settings-redesign plan §4): the index, one agent's
// detail, and its runs page. Reads only; writes stay on the binding, batch,
// review and draft routes.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// rosterRevision is one stored version of an agent and the places pinned to it.
type rosterRevision struct {
	Version      string   `json:"version"`
	SourceDigest string   `json:"source_digest"`
	BundleDigest string   `json:"bundle_digest"`
	SelectedAt   string   `json:"selected_at"`
	Current      bool     `json:"current"`
	Places       []string `json:"places"`
}

type rosterDetailResponse struct {
	Agent                   rosterAgent           `json:"agent"`
	Current                 *profilefs.Detail     `json:"current,omitempty"`
	SelectionToken          string                `json:"selection_token"`
	DraftToken              string                `json:"draft_token"`
	History                 []rosterRevision      `json:"history"`
	Draft                   *profilefs.Draft      `json:"draft,omitempty"`
	Totals                  store.OutcomeCounts   `json:"totals"`
	TotalsUnavailable       bool                  `json:"totals_unavailable,omitempty"`
	ManagedOption           *managedProfileOption `json:"managed_option,omitempty"`
	ReviewOption            *reviewProfileOption  `json:"review_option,omitempty"`
	Capabilities            []ChatCapability      `json:"capabilities"`
	Signals                 []servedSignal        `json:"signals"`
	ContextKinds            []contextKindOption   `json:"context_kinds"`
	ContextAlways           []contextKindOption   `json:"context_always"`
	TriggerLabel            string                `json:"trigger_label"`
	EnforcedLimits          []string              `json:"enforced_limits"`
	UnenforcedProfileLimits []string              `json:"unenforced_profile_limits"`
	Defaults                map[string]any        `json:"defaults"`
	LiveHelperSessions      map[string]int64      `json:"live_helper_sessions"`
	Outages                 []providerOutageFact  `json:"outages"`
	OverviewRuns            int                   `json:"overview_runs"`
	PageSize                int                   `json:"page_size"`
	StatsDays               int                   `json:"stats_days"`
	PlacesUnavailable       bool                  `json:"places_unavailable"`
	ReviewSlot              reviewSlot            `json:"review_slot"`
}

// reviewSlot says who holds the one review place and the token that replaces
// or creates it, so enabling a reviewer can name the one it replaces.
type reviewSlot struct {
	StateToken string `json:"state_token"`
	ProfileID  string `json:"profile_id,omitempty"`
	State      string `json:"state,omitempty"`
}

type rosterRunSession struct {
	Runtime   string `json:"runtime,omitempty"`
	CatalogID string `json:"catalog_id,omitempty"`
	NativeID  string `json:"native_id,omitempty"`
	Title     string `json:"title,omitempty"`
}

// rosterRun is one decision on the Activity tab: a managed run, or a review
// invocation carried with the history item reviewCard renders.
type rosterRun struct {
	RunID       string             `json:"run_id"`
	PlaceID     string             `json:"place_id"`
	Repository  string             `json:"repository,omitempty"`
	Outcome     string             `json:"outcome"`
	State       string             `json:"state"`
	Action      string             `json:"action,omitempty"`
	Message     string             `json:"message,omitempty"`
	Citations   []string           `json:"citations"`
	Detail      map[string]any     `json:"detail"`
	ErrorClass  string             `json:"error_class,omitempty"`
	Recovery    string             `json:"recovery,omitempty"`
	AdmittedAt  int64              `json:"admitted_at"`
	StartedAt   int64              `json:"started_at,omitempty"`
	CompletedAt int64              `json:"completed_at,omitempty"`
	Version     string             `json:"version,omitempty"`
	Signal      string             `json:"signal,omitempty"`
	Session     rosterRunSession   `json:"session"`
	Review      *reviewHistoryItem `json:"review,omitempty"`
	// AttentionClass and AwaitsOperator: see managedRunProjection.
	AttentionClass string `json:"attention_class,omitempty"`
	AwaitsOperator bool   `json:"awaits_operator,omitempty"`
}

type rosterRunsResponse struct {
	Runs []rosterRun `json:"runs"`
	Next string      `json:"next,omitempty"`
}

func registerOrchestrationRosterRoutes(mux *http.ServeMux, sources rosterSources) {
	mux.HandleFunc("GET /api/orchestration/roster", func(w http.ResponseWriter, r *http.Request) {
		roster, err := newRosterComposer(sources, tzOffsetMinutes(r)).roster()
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, roster)
	})
	mux.HandleFunc("GET /api/orchestration/roster/{profile}", func(w http.ResponseWriter, r *http.Request) {
		detail, found, err := rosterDetail(sources, r.PathValue("profile"), tzOffsetMinutes(r))
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		if !found {
			writeOrchestrationProfileError(w, &profilefs.Problem{Code: "not_found", Field: "profile",
				Message: "This agent was not found.", Recovery: "Return to the Agents list."})
			return
		}
		writeJSON(w, detail)
	})
	mux.HandleFunc("GET /api/orchestration/roster/{profile}/runs", func(w http.ResponseWriter, r *http.Request) {
		page, err := rosterRuns(sources, r.PathValue("profile"), r)
		if err != nil {
			status := http.StatusInternalServerError
			var bad rosterRequestError
			if errors.As(err, &bad) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, page)
	})
}

func tzOffsetMinutes(r *http.Request) int {
	value, err := strconv.Atoi(r.URL.Query().Get("tz_offset_minutes"))
	if err != nil || value < -14*60 || value > 14*60 {
		return 0
	}
	return value
}

func rosterDetail(sources rosterSources, profileID string, offset int) (rosterDetailResponse, bool, error) {
	composer := newRosterComposer(sources, offset)
	roster, err := composer.roster()
	if err != nil {
		return rosterDetailResponse{}, false, err
	}
	var agent *rosterAgent
	for index := range roster.Agents {
		if roster.Agents[index].ProfileID == profileID {
			agent = &roster.Agents[index]
		}
	}
	if agent == nil {
		return rosterDetailResponse{}, false, nil
	}
	config := orchestrationConfig().Roster
	out := rosterDetailResponse{Agent: *agent, SelectionToken: profilefs.SelectionAbsentToken(profileID),
		DraftToken: profilefs.DraftAbsentToken(profileID),
		History:    []rosterRevision{}, ContextKinds: managedContextOptions(), ContextAlways: managedAlwaysContext(), EnforcedLimits: enforcedAgentLimits(),
		UnenforcedProfileLimits: unenforcedProfileLimits(), Defaults: resolvedAgentDefaults(),
		LiveHelperSessions: map[string]int64{}, Outages: composer.outages, OverviewRuns: config.OverviewRuns,
		PageSize: config.PageSize, StatsDays: config.StatsDays, PlacesUnavailable: roster.PlacesUnavailable}
	if out.Outages == nil {
		out.Outages = []providerOutageFact{}
	}
	out.ReviewSlot = reviewSlot{StateToken: store.ReviewBindingAbsentToken()}
	if row := composer.reviewRow; row != nil {
		out.ReviewSlot = reviewSlot{StateToken: row.StateToken, ProfileID: row.ProfileID, State: row.State}
	}
	if agent.Lane == "review" {
		out.ContextKinds, out.ContextAlways = reviewContextOptions(), []contextKindOption{}
	}
	if current, err := sources.profiles.Get(profileID); err == nil {
		out.Current = &current
		out.SelectionToken = current.StateToken
		out.History = revisionsWithPlaces(current, agent.Places)
		if current.Normalized != nil {
			out.TriggerLabel = orchestration.TriggerLabel(current.Normalized.Trigger.Event)
		}
	}
	if draft, found, err := sources.profiles.Draft(profileID); err == nil && found {
		out.Draft = &draft
		out.DraftToken = draft.StateToken
	}
	if option, ok := composer.options[profileID]; ok {
		out.ManagedOption = &option
	}
	if option, ok := composer.reviews[profileID]; ok {
		out.ReviewOption = &option
	}
	out.Capabilities, _ = chatCapabilities()
	if out.Capabilities == nil {
		out.Capabilities = []ChatCapability{}
	}
	taskPumpLive := false
	if sources.managed != nil {
		sources.managed.mu.RLock()
		taskPumpLive = sources.managed.tasks != nil && !sources.managed.closing
		sources.managed.mu.RUnlock()
		placeIDs := []string{}
		for _, place := range agent.Places {
			placeIDs = append(placeIDs, place.PlaceID)
		}
		if live, err := sources.managed.ix.LiveHelperSessions(placeIDs); err == nil {
			out.LiveHelperSessions = live
		}
	}
	totalsErr := errRosterStatsUnavailable
	if agent.Lane == "review" && sources.review != nil {
		out.Totals, totalsErr = sources.review.ix.ReviewOutcomeTotals(store.HistoryScope{ProfileID: profileID})
	} else if agent.Lane != "review" && sources.managed != nil {
		out.Totals, totalsErr = sources.managed.ix.ManagedOutcomeTotals(store.HistoryScope{ProfileID: profileID})
	}
	out.TotalsUnavailable = totalsErr != nil
	out.Signals = servedSignalCatalog(taskPumpLive)
	return out, true, nil
}

func revisionsWithPlaces(current profilefs.Detail, places []rosterPlace) []rosterRevision {
	revisions := append([]profilefs.Revision{current.Current}, current.History...)
	out := make([]rosterRevision, 0, len(revisions))
	for index, revision := range revisions {
		row := rosterRevision{Version: revision.Version, SourceDigest: revision.SourceDigest,
			BundleDigest: revision.BundleDigest, SelectedAt: revision.SelectedAt, Current: index == 0, Places: []string{}}
		for _, place := range places {
			if place.SourceDigest == revision.SourceDigest && place.BundleDigest == revision.BundleDigest {
				row.Places = append(row.Places, place.PlaceID)
			}
		}
		out = append(out, row)
	}
	return out
}

// rosterRuns pages one agent's decisions, newest first. The review place pages
// review invocations; every other place (or none) pages managed runs.
// rosterRequestError marks a runs request the caller got wrong (400); any
// other error is the daemon's (500).
type rosterRequestError struct{ error }

func rosterRuns(sources rosterSources, profileID string, r *http.Request) (rosterRunsResponse, error) {
	query := r.URL.Query()
	config := orchestrationConfig().Roster
	limit := config.PageSize
	if value, err := strconv.Atoi(query.Get("limit")); err == nil && value > 0 && value <= config.PageSize {
		limit = value
	}
	cursor, err := parsePageCursor(query.Get("before"))
	if err != nil {
		return rosterRunsResponse{}, rosterRequestError{err}
	}
	place := strings.TrimSpace(query.Get("place"))
	outcome := strings.TrimSpace(query.Get("outcome"))
	versions := newVersionIndex(sources.profiles)
	if place == store.ReviewBindingID || (place == "" && isReviewer(sources.profiles, profileID)) {
		return reviewRosterRuns(sources, profileID, outcome, cursor, limit, versions)
	}
	if sources.managed == nil {
		return rosterRunsResponse{Runs: []rosterRun{}}, nil
	}
	runs, more, err := sources.managed.ix.ManagedRunsPage(store.HistoryScope{ProfileID: profileID, BindingID: place}, outcome, cursor, limit)
	if err != nil {
		return rosterRunsResponse{}, err
	}
	groups := map[string]store.ManagedGroup{}
	out := rosterRunsResponse{Runs: make([]rosterRun, 0, len(runs))}
	for _, run := range runs {
		group, cached := groups[run.GroupID]
		if !cached {
			group, _, _ = sources.managed.ix.ManagedGroup(run.GroupID)
			groups[run.GroupID] = group
		}
		signal, _ := run.Detail["signal"].(string)
		row := rosterRun{RunID: run.RunID, PlaceID: run.BindingID, Repository: repositoryName(group.ProjectRoot),
			Outcome: run.Outcome, State: run.State, Action: run.Action, Message: run.Message, Citations: run.Citations,
			Detail: run.Detail, ErrorClass: run.ErrorClass, Recovery: run.Recovery, AdmittedAt: run.AdmittedAt,
			StartedAt: run.StartedAt, CompletedAt: run.CompletedAt, Signal: signal,
			Version: versions.version(run.ProfileID, run.ProfileSourceDigest, run.ProfileBundleDigest),
			Session: runSession(sources.managed.ix, group.RootRuntime, group.RootCatalogSessionID, group.RootNativeSessionID)}
		if row.Citations == nil {
			row.Citations = []string{}
		}
		row.AttentionClass, row.AwaitsOperator = runAttention(run.ManagedRun)
		out.Runs = append(out.Runs, row)
	}
	if more && len(runs) > 0 {
		last := runs[len(runs)-1]
		out.Next = strconv.FormatInt(last.AdmittedAt, 10) + ":" + last.RunID
	}
	return out, nil
}

func reviewRosterRuns(sources rosterSources, profileID, outcome string, cursor store.PageCursor, limit int, versions *versionIndex) (rosterRunsResponse, error) {
	if sources.review == nil {
		return rosterRunsResponse{Runs: []rosterRun{}}, nil
	}
	invocations, more, err := sources.review.ix.ReviewInvocationsPage(profileID, outcome, cursor, limit)
	if err != nil {
		return rosterRunsResponse{}, err
	}
	records := make([]store.ReviewInvocation, 0, len(invocations))
	for _, invocation := range invocations {
		records = append(records, invocation.ReviewInvocation)
	}
	items, err := reviewHistoryItems(sources.review.ix, records)
	if err != nil {
		return rosterRunsResponse{}, err
	}
	out := rosterRunsResponse{Runs: make([]rosterRun, 0, len(invocations))}
	for index, invocation := range invocations {
		item := items[index]
		row := rosterRun{RunID: invocation.InvocationID, PlaceID: invocation.BindingID, Outcome: invocation.Outcome,
			State: invocation.State, Action: invocation.Action, Message: invocation.Message, Citations: invocation.Citations,
			Detail: map[string]any{}, ErrorClass: invocation.ErrorClass, AdmittedAt: invocation.AdmittedAt,
			StartedAt: invocation.StartedAt, CompletedAt: invocation.CompletedAt, Signal: invocation.Tool,
			Version: versions.version(invocation.ProfileID, invocation.ProfileSourceDigest, invocation.ProfileBundleDigest),
			Session: runSession(sources.review.ix, invocation.Runtime, invocation.SessionID, ""), Review: &item}
		if row.Citations == nil {
			row.Citations = []string{}
		}
		out.Runs = append(out.Runs, row)
	}
	if more && len(invocations) > 0 {
		last := invocations[len(invocations)-1]
		out.Next = strconv.FormatInt(last.AdmittedAt, 10) + ":" + last.InvocationID
	}
	return out, nil
}

func isReviewer(profiles *profilefs.Owner, profileID string) bool {
	detail, err := profiles.Get(profileID)
	return err == nil && detail.Normalized != nil && detail.Normalized.AgentType() == "reviewer"
}

func parsePageCursor(raw string) (store.PageCursor, error) {
	if raw == "" {
		return store.PageCursor{}, nil
	}
	at, id, ok := strings.Cut(raw, ":")
	value, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || value <= 0 || id == "" {
		return store.PageCursor{}, errBadCursor
	}
	return store.PageCursor{AdmittedAt: value, ID: id}, nil
}

type rosterError string

func (e rosterError) Error() string { return string(e) }

const errBadCursor = rosterError("before must be <admitted_at>:<id> from a previous page")

// runSession names a run's source session by its exact root ids; the title is
// display only, read from the indexed sessions table (never a filesystem scan).
func runSession(ix *store.Index, runtime, catalogID, nativeID string) rosterRunSession {
	out := rosterRunSession{Runtime: runtime, CatalogID: catalogID, NativeID: nativeID}
	for _, id := range []string{catalogID, nativeID} {
		if id == "" {
			continue
		}
		if row, found, err := ix.SessionByID(runtime, id); err == nil && found {
			out.Title = row.Title
			return out
		}
	}
	return out
}

func repositoryName(projectRoot string) string {
	trimmed := strings.TrimRight(projectRoot, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

// versionIndex resolves a run's pinned revision to its version label against
// the run's OWN profile (a delegate's child profile included), cached per page.
type versionIndex struct {
	profiles *profilefs.Owner
	byID     map[string]map[string]string
}

func newVersionIndex(profiles *profilefs.Owner) *versionIndex {
	return &versionIndex{profiles: profiles, byID: map[string]map[string]string{}}
}

func (index *versionIndex) version(profileID, sourceDigest, bundleDigest string) string {
	versions, cached := index.byID[profileID]
	if !cached {
		versions = map[string]string{}
		if detail, err := index.profiles.Get(profileID); err == nil {
			for _, revision := range append([]profilefs.Revision{detail.Current}, detail.History...) {
				versions[revision.SourceDigest+"\x00"+revision.BundleDigest] = revision.Version
			}
		}
		index.byID[profileID] = versions
	}
	return versions[sourceDigest+"\x00"+bundleDigest]
}
