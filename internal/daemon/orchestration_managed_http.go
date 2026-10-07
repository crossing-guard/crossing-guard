package daemon

// Managed orchestration HTTP. The agent-shaped surface
// (/api/orchestration/agents, group notes, tags, and the managed projection)
// is the one operator API; run action gates are agent-type-based
// (reviewer/follower/helper). Published defaults come from the resolved
// shipped configuration, never from route-local literals.

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// managedProfileOption publishes the agent taxonomy type, never the profile's
// raw role vocabulary — the console reasons in reviewer/follower/helper only.
type managedProfileOption struct {
	ProfileID          string   `json:"profile_id"`
	Name               string   `json:"name"`
	Version            string   `json:"version"`
	AgentType          string   `json:"agent_type"`
	SourceDigest       string   `json:"source_digest"`
	BundleDigest       string   `json:"bundle_digest"`
	Compatible         bool     `json:"compatible"`
	Reason             string   `json:"reason,omitempty"`
	RequestedAuthority []string `json:"requested_authority"`
	AllowedProfiles    []string `json:"allowed_profiles"`
	// NotApplied names compiled limits this host accepts but cannot apply to
	// managed turns (managed-turn-profile-limits plan §4.7), so the card
	// states them instead of implying enforcement.
	NotApplied []managedNotApplied `json:"not_applied"`
}

// managedNotApplied is one compiled field the host does not apply, with the
// host's own reason — the console renders it and never interprets the field.
type managedNotApplied struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// managedTurnNotApplied lists the compiled limits a managed turn does not
// apply on this host: the runtime harness makes its own model calls, so a
// per-call token cap has no enforcement point (plan §4.7, D-3).
var managedTurnNotApplied = []managedNotApplied{{Field: "limits.max-tokens",
	Reason: "Managed turns are bounded in final claim size (max-output-bytes) and wall time (timeout), not in tokens: the runtime makes its own model calls."}}

// agentProjection is one roster row on the agent-shaped surface: the binding
// with its profile's display identity and a bounded prompt excerpt.
type agentProjection struct {
	store.ManagedBinding
	// ProjectRootKey is the folder identity of project_root (projectRootKey),
	// compared with a session's cwd_key; an opaque value, never a path.
	ProjectRootKey     string `json:"project_root_key,omitempty"`
	ProfileName        string `json:"profile_name"`
	ProfileDescription string `json:"profile_description"`
	PromptExcerpt      string `json:"prompt_excerpt"`
	// ProfileProblem and DestinationProblem judge the binding's PINNED
	// revision (managed-turn-profile-limits plan §4.1, RT-6): why this binding
	// cannot run here, even when the profile's current revision could.
	ProfileProblem     string            `json:"profile_problem,omitempty"`
	DestinationProblem string            `json:"destination_problem,omitempty"`
	Destination        *agentDestination `json:"destination,omitempty"`
}

// agentDestination states the pinned profile's destination requirement and
// each route's locality as its runtime declares it — a route class, never an
// egress proof (plan §4.1).
type agentDestination struct {
	Required string                  `json:"required"`
	Routes   []agentRouteDestination `json:"routes"`
}

type agentRouteDestination struct {
	Runtime string `json:"runtime"`
	Model   string `json:"model"`
	Label   string `json:"label"`
	Local   bool   `json:"local"`
	Basis   string `json:"basis,omitempty"`
}

// agentBindingJudgement fills the pinned-revision facts of one roster row.
func agentBindingJudgement(row *agentProjection, compiled profilefs.CompiledProfile, profiles *profilefs.Owner) {
	if err := validateManagedProfile(compiled); err != nil {
		row.ProfileProblem = err.Error()
	}
	routes := bindingRoutes(row.ManagedBinding)
	children := []profilefs.CompiledProfile{}
	for _, ref := range row.AllowedProfiles {
		if child, err := profiles.GetRevision(ref.ProfileID, ref.SourceDigest, ref.BundleDigest); err == nil && child.Normalized != nil {
			children = append(children, *child.Normalized)
		}
	}
	row.DestinationProblem = bindingDestinationProblem(compiled, routes, children)
	destination := &agentDestination{Required: compiled.Requirements.Destination.Locality, Routes: []agentRouteDestination{}}
	for _, route := range routes {
		local, basis := chatRouteIsLocal(ChatRequest{Runtime: route.Runtime, Model: route.Model, Mode: route.Mode})
		destination.Routes = append(destination.Routes, agentRouteDestination{Runtime: route.Runtime, Model: route.Model,
			Label: chatRuntimeLabel(route.Runtime) + " · " + chatModelLabel(route.Runtime, route.Model), Local: local, Basis: basis})
	}
	row.Destination = destination
}

// managedBindingResponse is the answer to a binding write: the stored row.
type managedBindingResponse struct {
	Binding store.ManagedBinding `json:"binding"`
}

// bindingWriteStatus maps a binding write error onto its HTTP status: a stale
// token is a conflict; everything else is a request the daemon refused.
func bindingWriteStatus(err error) int {
	if errors.Is(err, store.ErrManagedBindingConflict) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// managedRunProjection carries the helper's project root beside each run so
// the GUI binds the helper's ref resolver, never the open session's (plan §5).
type managedRunProjection struct {
	store.ManagedRun
	ProjectRoot string `json:"project_root"`
	// AttentionClass and AwaitsOperator are the claim contract's answer for a
	// settled claim run (escalation-delivery plan §6.1): "ask", "draft" or
	// "", and whether it waits on the operator's confirmation. Readers use
	// these flags; none keeps its own list of actions (RT-11).
	AttentionClass string `json:"attention_class,omitempty"`
	AwaitsOperator bool   `json:"awaits_operator,omitempty"`
}

// runAttention classifies one run for the owner: only a completed claim run
// (not a reply, correction or delegate child) carries a class.
func runAttention(run store.ManagedRun) (string, bool) {
	if run.State != "completed" || run.Kind != "" {
		return "", false
	}
	state := ""
	if receipt, ok := run.Detail["delivery"].(map[string]any); ok {
		state, _ = receipt["state"].(string)
	}
	return orchestration.ClaimAttention(run.Action, state)
}

const agentPromptExcerptRunes = 280

// resolvedAgentDefaults publishes the shipped budget configuration (owner Q3:
// values are configuration, not frozen facts). max_depth and retries mirror
// the structural managed-profile shape validateManagedProfile enforces.
func resolvedAgentDefaults() map[string]any {
	limits := resolveAgentLimits(store.ManagedLimits{})
	return map[string]any{
		"max_depth": 1, "retries": 0,
		"max_hops": limits.MaxHops, "max_active": limits.MaxActive, "max_total": limits.MaxTotal,
		"loop_budget": limits.LoopBudget, "max_group_tokens": limits.MaxGroupTokens,
		"max_agent_tokens": limits.MaxAgentTokens,
	}
}

// servedSignal pairs a published catalog entry with whether a live pump in
// THIS daemon currently serves it. The catalog says what a signal IS; served
// says what runs here today, so a profile naming a real-but-unserved kind
// reads "incompatible here" instead of silently never firing.
type servedSignal struct {
	orchestration.Signal
	Served bool `json:"served"`
	// ServedBy names which runtimes emit this signal through which evidence
	// class today (red-team M9): the catalog says what a signal IS;
	// served says whether a pump runs; served_by says WHERE and HOW HONESTLY
	// — the natural end fact is hook-exact for claude/codex and absent for
	// opencode in v1, and one boolean must not overstate that difference.
	ServedBy []servedSignalRuntime `json:"served_by,omitempty"`
}

type servedSignalRuntime struct {
	Runtime     string `json:"runtime"`
	Evidence    string `json:"evidence"`
	Observation string `json:"observation,omitempty"`
}

func servedSignalCatalog(taskPumpLive bool) []servedSignal {
	servedSources := map[string]bool{
		// The managed pump translates runtime task events onto catalog kinds.
		"task-events": taskPumpLive,
		// Approval asks flow through the delegated-review lane.
		"approvals": true,
		// Session-activity signals now have an emitter (natural-session plan
		// Slice B): durable lifecycle rows translate onto catalog kinds. The
		// per-runtime evidence differs — an end fact is hook-exact where the
		// closure lane exists and ABSENT for opencode in v1 — so ServedBy
		// carries the honest per-runtime truth.
		"session-activity": true,
	}
	catalog := orchestration.SignalCatalog()
	out := make([]servedSignal, 0, len(catalog))
	for _, signal := range catalog {
		row := servedSignal{Signal: signal, Served: servedSources[signal.Source]}
		if signal.Source == "session-activity" {
			row.ServedBy = sessionActivityServedBy(signal.Kind)
		}
		out = append(out, row)
	}
	return out
}

// sessionActivityServedBy publishes the per-runtime evidence classes for one
// session-activity signal kind (red-team M9): which runtime emits it and
// through which hook-exact or presence-derived class. The facts live on each
// vendor adapter (harvest.ActivityEvidenceReporter, ADR 0020) — a runtime with
// no entry for a kind serves no such fact (red-team B3: an absent end hook
// means NO end fact, stated, never guessed).
func sessionActivityServedBy(kind string) []servedSignalRuntime {
	out := []servedSignalRuntime{}
	for _, name := range harvest.RuntimeNames() {
		if class, ok := harvest.ActivityEvidence(name)[kind]; ok {
			out = append(out, servedSignalRuntime{Runtime: name, Evidence: class})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func registerOrchestrationManagedRoutes(mux *http.ServeMux, host *orchestrationManagedHost, profiles *profilefs.Owner) {
	requireHost := func(w http.ResponseWriter) bool {
		if host == nil {
			http.Error(w, "managed orchestration unavailable", http.StatusServiceUnavailable)
			return false
		}
		return true
	}

	putBindingHandler := func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		var request struct {
			ProfileID           string                `json:"profile_id"`
			ProfileSourceDigest string                `json:"profile_source_digest"`
			ProfileBundleDigest string                `json:"profile_bundle_digest"`
			ScopeRuntime        string                `json:"scope_runtime"`
			ScopeSession        string                `json:"scope_session"`
			ProjectRoot         string                `json:"project_root"`
			RouteID             string                `json:"route_id"`
			Mode                string                `json:"mode"`
			GrantedAuthority    []string              `json:"granted_authority"`
			AutoAction          bool                  `json:"auto_action"`
			WatchNatural        bool                  `json:"watch_natural"`
			Routes              []bindingRouteRequest `json:"routes"`
			Priority            int64                 `json:"priority"`
			DeclaredTags        []string              `json:"declared_tags"`
			Limits              store.ManagedLimits   `json:"limits"`
			State               string                `json:"state"`
			ExpectedStateToken  string                `json:"expected_state_token"`
			Confirmed           bool                  `json:"confirmed"`
			typedModelFields
		}
		if err := decodeManagedJSON(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A write that still sends a runtime, model or endpoint is refused by name
		// (plan §5.3, criterion 56): a place names a model route.
		if err := request.refusal(""); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		chain, err := chainFromRequest("routes", request.Routes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !request.Confirmed {
			http.Error(w, "explicit binding confirmation is required", http.StatusUnprocessableEntity)
			return
		}
		binding, err := host.putBinding(managedBindingCommand{BindingID: strings.TrimSpace(r.PathValue("binding")), ProfileID: request.ProfileID, ProfileSourceDigest: request.ProfileSourceDigest, ProfileBundleDigest: request.ProfileBundleDigest, ScopeRuntime: request.ScopeRuntime, ScopeSession: request.ScopeSession, ProjectRoot: request.ProjectRoot, RouteID: strings.TrimSpace(request.RouteID), Mode: request.Mode, GrantedAuthority: request.GrantedAuthority, AutoAction: request.AutoAction, WatchNatural: request.WatchNatural, Routes: chain, Priority: request.Priority, DeclaredTags: request.DeclaredTags, Limits: request.Limits, State: request.State, ExpectedStateToken: request.ExpectedStateToken})
		if err != nil {
			http.Error(w, err.Error(), bindingWriteStatus(err))
			return
		}
		writeJSON(w, managedBindingResponse{Binding: binding})
	}

	disableBindingHandler := func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		var request struct {
			ExpectedStateToken string `json:"expected_state_token"`
			Confirmed          bool   `json:"confirmed"`
		}
		if err := decodeManagedJSON(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !request.Confirmed {
			http.Error(w, "explicit disable confirmation is required", http.StatusUnprocessableEntity)
			return
		}
		binding, err := host.ix.DisableManagedBinding(strings.TrimSpace(r.PathValue("binding")), request.ExpectedStateToken, timeNowUnix())
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, managedBindingResponse{Binding: binding})
	}

	mux.HandleFunc("GET /api/orchestration/managed/settings", func(w http.ResponseWriter, _ *http.Request) {
		if !requireHost(w) {
			return
		}
		bindings, err := host.ix.ManagedBindings(false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		options, err := managedProfileOptions(profiles)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		capabilities, _ := chatCapabilities()
		host.mu.RLock()
		problem := host.problem
		runtimeReady := host.tasks != nil && !host.closing
		host.mu.RUnlock()
		// absent_state_tokens publishes the creation CAS token for each
		// UNBOUND follower/helper-compatible profile's per-profile binding id
		// (G-4: importing a profile creates the agent's card; its one Enable
		// creates `agent-<profile_id>` under this token). Published by the
		// daemon to avoid duplicating the store's digest scheme in JS; never
		// emitted for an id that already has a binding, so a collision
		// CAS-conflicts instead of silently rebinding (g4 plan §2.3).
		bound := map[string]bool{}
		for _, binding := range bindings {
			bound[binding.BindingID] = true
		}
		absent := map[string]string{}
		for _, option := range options {
			if !option.Compatible || (option.AgentType != "follower" && option.AgentType != "helper") {
				continue
			}
			bindingID := "agent-" + option.ProfileID
			if !bound[bindingID] {
				absent[bindingID] = store.ManagedBindingAbsentToken(bindingID)
			}
		}
		writeJSON(w, map[string]any{"bindings": bindings, "profiles": options, "chat_capabilities": capabilities, "absent_state_tokens": absent, "runtime_ready": runtimeReady, "problem": problem, "defaults": resolvedAgentDefaults(),
			// Tripped provider routes (provider-outage plan Slice D): one
			// banner row per dead route — since when, the vendor's own
			// recovery words, and the parked count behind it.
			"provider_outages": host.providerOutageFacts()})
	})

	// Operator reroute of parked runs (provider-outage plan Slice C):
	// preview→confirm through the existing short-lived token authority. The
	// preview names every parked run's outcome under per-role policy (R6);
	// the token digest pins that exact plan, so drift between preview and
	// confirm is refused rather than silently applied.
	mux.HandleFunc("POST /api/orchestration/managed/provider-reroute/preview", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		var request struct {
			Runtime string `json:"runtime"`
			Model   string `json:"model"`
		}
		if err := decodeManagedJSON(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		decisions, err := host.planProviderReroute(request.Runtime, request.Model)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		token, expiresAt, err := runtimeIntegrationPreviews.issue(
			providerRouteKey(request.Runtime, request.Model), "provider-reroute", rerouteDigest(decisions))
		if err != nil {
			http.Error(w, "could not create a confirmation token", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"decisions": decisions, "preview_token": token,
			"expires_at": expiresAt.UTC().Format(time.RFC3339),
			"note":       "Preview is read-only. Stale helpers become operator drafts, never late auto-sends."})
	})
	mux.HandleFunc("POST /api/orchestration/managed/provider-reroute", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		var request struct {
			Runtime      string `json:"runtime"`
			Model        string `json:"model"`
			PreviewToken string `json:"preview_token"`
			Confirmed    bool   `json:"confirmed"`
		}
		if err := decodeManagedJSON(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !request.Confirmed || request.PreviewToken == "" {
			http.Error(w, "a current preview token and explicit confirmation are required", http.StatusBadRequest)
			return
		}
		entry, err := runtimeIntegrationPreviews.consume(request.PreviewToken,
			providerRouteKey(request.Runtime, request.Model), "provider-reroute")
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		decisions, err := host.planProviderReroute(request.Runtime, request.Model)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if rerouteDigest(decisions) != entry.Digest {
			http.Error(w, "parked runs changed since the preview; preview again", http.StatusConflict)
			return
		}
		rerouted, staleDrafts, unrouted, applyErr := host.applyProviderReroute(decisions)
		response := map[string]any{"rerouted": rerouted, "stale_drafts": staleDrafts, "no_route": unrouted}
		if applyErr != nil {
			response["problem"] = applyErr.Error()
		}
		writeJSON(w, response)
	})

	// Agent-shaped surface (plan §3).
	mux.HandleFunc("GET /api/orchestration/agents", func(w http.ResponseWriter, _ *http.Request) {
		if !requireHost(w) {
			return
		}
		bindings, err := host.ix.ManagedBindings(false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		agents := make([]agentProjection, 0, len(bindings))
		for _, binding := range bindings {
			row := agentProjection{ManagedBinding: binding, ProjectRootKey: projectRootKey(binding.ProjectRoot)}
			if detail, detailErr := profiles.GetRevision(binding.ProfileID, binding.ProfileSourceDigest, binding.ProfileBundleDigest); detailErr == nil && detail.Normalized != nil {
				row.ProfileName = detail.Normalized.Name
				row.ProfileDescription = detail.Normalized.Description
				row.PromptExcerpt = truncate(detail.Normalized.Instructions, agentPromptExcerptRunes)
				agentBindingJudgement(&row, *detail.Normalized, profiles)
			}
			agents = append(agents, row)
		}
		host.mu.RLock()
		taskPumpLive := host.tasks != nil && !host.closing
		host.mu.RUnlock()
		writeJSON(w, map[string]any{"agents": agents, "defaults": resolvedAgentDefaults(), "signals": servedSignalCatalog(taskPumpLive)})
	})
	mux.HandleFunc("PUT /api/orchestration/agents/{binding}", putBindingHandler)
	mux.HandleFunc("POST /api/orchestration/agents/{binding}/disable", disableBindingHandler)
	mux.HandleFunc("POST /api/orchestration/agents/batch", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		handleBindingBatch(w, r, host)
	})
	mux.HandleFunc("POST /api/orchestration/agents/binding-id", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		handleNewBindingID(w, r, host)
	})

	mux.HandleFunc("GET /api/orchestration/managed", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		runs, err := host.ix.ManagedRuns(100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		groups, err := host.ix.ManagedGroups(100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		controls, err := host.ix.ManagedControls(100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// A session may be known under several exact identities (artifact id,
		// vendor meta id, vendor thread id) while the group root recorded
		// whichever the task row carried — repeated `session` params match
		// ANY of them against catalog OR native root ids (g4 plan §3).
		// Empty values are ignored; they never widen the filter.
		runtime := r.URL.Query().Get("runtime")
		sessionIDs := make([]string, 0, 3)
		for _, value := range r.URL.Query()["session"] {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				sessionIDs = append(sessionIDs, trimmed)
			}
		}
		if runtime != "" || len(sessionIDs) > 0 {
			matchesSession := func(group store.ManagedGroup) bool {
				if len(sessionIDs) == 0 {
					return true
				}
				for _, id := range sessionIDs {
					if group.RootCatalogSessionID == id || group.RootNativeSessionID == id {
						return true
					}
				}
				return false
			}
			allowed := map[string]bool{}
			filteredGroups := groups[:0]
			for _, group := range groups {
				if runtime != "" && group.RootRuntime != runtime {
					continue
				}
				if !matchesSession(group) {
					continue
				}
				allowed[group.GroupID] = true
				filteredGroups = append(filteredGroups, group)
			}
			groups = filteredGroups
			filteredRuns := runs[:0]
			for _, run := range runs {
				if allowed[run.GroupID] {
					filteredRuns = append(filteredRuns, run)
				}
			}
			runs = filteredRuns
		}
		rootByGroup := map[string]string{}
		for _, group := range groups {
			rootByGroup[group.GroupID] = group.ProjectRoot
		}
		projectedRuns := make([]managedRunProjection, 0, len(runs))
		for _, run := range runs {
			class, awaits := runAttention(run)
			projectedRuns = append(projectedRuns, managedRunProjection{ManagedRun: run, ProjectRoot: rootByGroup[run.GroupID],
				AttentionClass: class, AwaitsOperator: awaits})
		}
		relationships := []map[string]any{}
		for _, group := range groups {
			groupRelationships, relErr := host.ix.ManagedRelationships(group.GroupID, 50)
			if relErr != nil {
				http.Error(w, relErr.Error(), http.StatusInternalServerError)
				return
			}
			relationships = append(relationships, groupRelationships...)
		}
		writeJSON(w, map[string]any{"groups": groups, "runs": projectedRuns, "controls": controls, "relationships": relationships})
	})

	mux.HandleFunc("POST /api/orchestration/groups/{group}/notes", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		var request struct {
			Body string `json:"body"`
		}
		if err := decodeManagedJSON(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body := strings.TrimSpace(request.Body)
		if body == "" || len(body) > 4000 {
			http.Error(w, "provide a nonempty group note up to 4000 bytes", http.StatusUnprocessableEntity)
			return
		}
		groupID := strings.TrimSpace(r.PathValue("group"))
		if _, found, err := host.ix.ManagedGroup(groupID); err != nil || !found {
			http.Error(w, "managed group not found", http.StatusNotFound)
			return
		}
		note := store.ManagedGroupNote{NoteID: managedID("onote_", groupID, body, time.Now().String()), GroupID: groupID, Body: body, CreatedAt: timeNowUnix()}
		if err := host.ix.PutManagedGroupNote(note); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"note": note, "effect": "Injected into this group's FUTURE agent prompts as operator.group_notes; past runs are unchanged."})
	})

	mux.HandleFunc("GET /api/orchestration/groups/{group}/notes", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		notes, err := host.ix.ManagedGroupNotes(strings.TrimSpace(r.PathValue("group")), true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"notes": notes})
	})

	mux.HandleFunc("POST /api/orchestration/notes/{note}/retract", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		if err := host.ix.RetractManagedGroupNote(strings.TrimSpace(r.PathValue("note")), timeNowUnix()); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, map[string]any{"result": "note retracted"})
	})

	mux.HandleFunc("GET /api/orchestration/tags/summary", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		handleTagSummary(w, host.ix)
	})

	mux.HandleFunc("GET /api/orchestration/tags", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		// Exactly one selector: session_id (that session's tags), tag or
		// agent_key (the sessions carrying it, recall-mcp-v1-plan §3.4).
		byTag := strings.TrimSpace(r.URL.Query().Get("tag"))
		byAgent := strings.TrimSpace(r.URL.Query().Get("agent_key"))
		selectors := 0
		for _, set := range []bool{byTag != "", byAgent != "", r.URL.Query().Has("session_id")} {
			if set {
				selectors++
			}
		}
		if selectors > 1 {
			http.Error(w, "name exactly one of session_id, tag or agent_key", http.StatusBadRequest)
			return
		}
		if byTag != "" {
			handleTagLookup(w, host.ix, "tag", byTag)
			return
		}
		if byAgent != "" {
			handleTagLookup(w, host.ix, "agent_key", byAgent)
			return
		}
		// Repeated session_id values are one session's exact identity
		// alternates (g4 plan §3): tags are written under the group's
		// catalog-else-native id, so every alternate is read and the merged
		// set deduped. All-empty input is still a 400, never a wildcard.
		sessionIDs := make([]string, 0, 3)
		seen := map[string]bool{}
		for _, value := range r.URL.Query()["session_id"] {
			trimmed := strings.TrimSpace(value)
			if trimmed != "" && !seen[trimmed] {
				seen[trimmed] = true
				sessionIDs = append(sessionIDs, trimmed)
			}
		}
		if len(sessionIDs) == 0 {
			http.Error(w, "session_id is required", http.StatusBadRequest)
			return
		}
		now := timeNowUnix()
		// One row per agent key across the alternates: writeClaimTags gives
		// the catalog and native row of one claim different tag ids, so the
		// key is (agent_key, tag), and the newest row of it wins.
		merged := []store.OrchestrationTag{}
		mergedIndex := map[string]int{}
		for _, sessionID := range sessionIDs {
			tags, err := host.ix.ActiveOrchestrationTags(sessionID, now)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, tag := range tags {
				key := tag.AgentKey + "\x00" + tag.Tag
				index, found := mergedIndex[key]
				switch {
				case !found:
					mergedIndex[key] = len(merged)
					merged = append(merged, tag)
				case newerOrchestrationTag(tag, merged[index]):
					merged[index] = tag
				}
			}
		}
		sort.SliceStable(merged, func(i, j int) bool { return newerOrchestrationTag(merged[i], merged[j]) })
		writeJSON(w, map[string]any{"tags": merged})
	})

	mux.HandleFunc("POST /api/orchestration/managed/runs/{run}/send", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		var request struct {
			Message   string `json:"message"`
			Confirmed bool   `json:"confirmed"`
		}
		if err := decodeManagedJSON(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		run, found, err := host.ix.ManagedRun(r.PathValue("run"))
		if err != nil || !found {
			http.Error(w, "managed run not found", http.StatusNotFound)
			return
		}
		// Type-based gate: only a helper run's draft_reply claim is sendable.
		if run.Role != "helper" || run.State != "completed" || run.Action != "draft_reply" {
			http.Error(w, "run has no sendable draft", http.StatusConflict)
			return
		}
		if !request.Confirmed || strings.TrimSpace(request.Message) == "" {
			http.Error(w, "confirm a nonempty edited reply", http.StatusUnprocessableEntity)
			return
		}
		// Operator-edited sends are operator speech: no provenance marker (Q5).
		if err := host.resumeParent(run, strings.TrimSpace(request.Message), "manual"); err != nil && !actionStarted(err) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		// The draft was carried: it stops reading as a draft for the owner
		// and as waiting on the operator (escalation-delivery plan §6.1).
		if err := host.settleClaimDelivery(run.RunID, SessionMessageReceipt{State: deliveryStarted, Tier: "none",
			Detail: "The operator sent an edited reply."}, nil); err != nil {
			log.Printf("managed run %s was sent but its receipt could not be recorded: %v", run.RunID, err)
		}
		writeJSON(w, map[string]any{"result": "reply task admitted", "source_run_id": run.RunID})
	})

	mux.HandleFunc("POST /api/orchestration/managed/runs/{run}/act", func(w http.ResponseWriter, r *http.Request) {
		if !requireHost(w) {
			return
		}
		var request struct {
			Confirmed bool `json:"confirmed"`
		}
		if err := decodeManagedJSON(w, r, &request); err != nil || !request.Confirmed {
			http.Error(w, "explicit confirmation is required", http.StatusUnprocessableEntity)
			return
		}
		run, found, err := host.ix.ManagedRun(r.PathValue("run"))
		if err != nil || !found {
			http.Error(w, "managed run not found", http.StatusNotFound)
			return
		}
		binding, found, err := host.ix.ManagedBinding(run.BindingID)
		if err != nil || !found {
			http.Error(w, "binding unavailable", http.StatusConflict)
			return
		}
		// Type-based gate: only helper runs carry operator-confirmable actions.
		switch {
		case run.Role == "helper" && run.Action == "launch_profile":
			child, _ := run.Detail["child_profile_id"].(string)
			err = host.launchHelperChild(run, binding, child, run.Message)
		case run.Role == "helper" && run.Action == "request_interrupt":
			err = host.interruptSource(run, run.Message)
		default:
			http.Error(w, "run has no pending action", http.StatusConflict)
			return
		}
		if err != nil && !actionStarted(err) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		// The operator's confirmation carried the proposal: its receipt
		// leaves not_requested (escalation-delivery plan §4).
		if err := host.settleClaimDelivery(run.RunID, SessionMessageReceipt{State: deliveryStarted, Tier: "none",
			Detail: "The operator confirmed the action and it was admitted."}, nil); err != nil {
			log.Printf("managed run %s acted but its receipt could not be recorded: %v", run.RunID, err)
		}
		writeJSON(w, map[string]any{"result": "action admitted through the existing owner"})
	})

	mux.HandleFunc("POST /api/orchestration/managed/runs/{run}/resume", func(w http.ResponseWriter, r *http.Request) {
		if host == nil || host.tasks == nil {
			http.Error(w, "managed runtime tasks unavailable", http.StatusServiceUnavailable)
			return
		}
		var request struct {
			Message   string `json:"message"`
			Confirmed bool   `json:"confirmed"`
		}
		if err := decodeManagedJSON(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		run, found, err := host.ix.ManagedRun(r.PathValue("run"))
		if err != nil || !found {
			http.Error(w, "managed run not found", http.StatusNotFound)
			return
		}
		if run.Role != "helper" || run.State != "completed" || !request.Confirmed || strings.TrimSpace(request.Message) == "" {
			http.Error(w, "confirm a nonempty correction for a completed run", http.StatusUnprocessableEntity)
			return
		}
		source, found, err := host.tasks.Task(run.SourceTaskID)
		if err != nil || !found {
			http.Error(w, "exact source task unavailable", http.StatusConflict)
			return
		}
		if !terminalTaskLifecycle(source.Lifecycle) {
			http.Error(w, "interrupt or finish the source task before resuming it", http.StatusConflict)
			return
		}
		if err := host.resumeParent(run, strings.TrimSpace(request.Message), "correction"); err != nil && !actionStarted(err) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, map[string]any{"result": "corrective resume task admitted", "source_run_id": run.RunID})
	})
}

func managedProfileOptions(owner *profilefs.Owner) ([]managedProfileOption, error) {
	listed, err := owner.List()
	if err != nil {
		return nil, err
	}
	out := []managedProfileOption{}
	for _, summary := range listed.Profiles {
		detail, detailErr := owner.GetRevision(summary.ProfileID, summary.SourceDigest, summary.BundleDigest)
		option := managedProfileOption{ProfileID: summary.ProfileID, Name: summary.Name, Version: summary.Version, SourceDigest: summary.SourceDigest, BundleDigest: summary.BundleDigest, RequestedAuthority: []string{}, AllowedProfiles: []string{}, NotApplied: []managedNotApplied{}}
		if detailErr == nil && detail.Normalized != nil {
			if detail.Normalized.Execution == "managed-turn" {
				option.NotApplied = append(option.NotApplied, managedTurnNotApplied...)
			}
			option.AgentType = detail.Normalized.AgentType()
			option.RequestedAuthority = append([]string(nil), detail.Normalized.Authority...)
			option.AllowedProfiles = append([]string(nil), detail.Normalized.AllowedProfiles...)
			detailErr = validateManagedProfile(*detail.Normalized)
		}
		option.Compatible = detailErr == nil
		if detailErr != nil {
			option.Reason = detailErr.Error()
		}
		out = append(out, option)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentType+out[i].Name < out[j].AgentType+out[j].Name })
	return out, nil
}

func decodeManagedJSON(w http.ResponseWriter, r *http.Request, target any, maxBytes ...int64) error {
	limit := int64(64 << 10)
	if len(maxBytes) > 0 {
		limit = maxBytes[0]
	}
	if limit > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain exactly one JSON object")
	}
	return nil
}
func timeNowUnix() int64 { return time.Now().Unix() }

// newerOrchestrationTag is the tag read's own order (applied_at DESC, tag_id
// DESC — the ORDER BY of store.ActiveOrchestrationTags; keep the two in step),
// for merging one session's identity alternates.
func newerOrchestrationTag(a, b store.OrchestrationTag) bool {
	if a.AppliedAt != b.AppliedAt {
		return a.AppliedAt > b.AppliedAt
	}
	return a.TagID > b.TagID
}
