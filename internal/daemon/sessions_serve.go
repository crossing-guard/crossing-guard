package daemon

import (
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/sessionactivity"
	"crossing-guard/internal/sessionquery"
	"crossing-guard/store"
)

const (
	defaultSessionPageSize = 15
	maxSessionPageSize     = 50
	defaultRepositoryLimit = 200
	maxRepositoryLimit     = 500
)

type sessionRepository struct {
	// Label is set only where Key is not fit to show as written (a tag group's
	// key is case-folded); the browser shows Label when present, else Key.
	Label                string    `json:"label,omitempty"`
	Key                  string    `json:"key"`
	LaunchCwd            string    `json:"launch_cwd,omitempty"`
	Total                int       `json:"total"`
	OpenTotal            int       `json:"open_total"`
	PresenceUnknownTotal int       `json:"presence_unknown_total,omitempty"`
	Newest               time.Time `json:"newest"`
	NewestOpen           time.Time `json:"newest_open,omitempty"`
}

type sessionRailResponse struct {
	Activity        sessionactivity.Capability   `json:"activity"`
	RepositoryTotal int                          `json:"repository_total"`
	Repositories    []sessionRepository          `json:"repositories"`
	RuntimeHealth   []harvest.RuntimeScanOutcome `json:"runtime_health,omitempty"`
	// ChangeEvidenceProblem says why sessions known only from recorded change
	// evidence are missing from this answer (the store could not be read);
	// absent when the store was read or does not exist yet.
	ChangeEvidenceProblem string `json:"change_evidence_problem,omitempty"`
	// GroupBy is set when the groups are not repositories (a filtered rail may
	// group by runtime or by one of the owner's tag keys).
	GroupBy string `json:"group_by,omitempty"`
	// ViewCounts answers counts=1: sessions per saved view, by view id. A view
	// that cannot be counted truthfully is absent rather than approximated.
	ViewCounts map[string]int `json:"view_counts,omitempty"`
	// QueryNotes are the query's own notes (sessionquery.Query.Notes) over
	// the tags this read bound it against. ViewNotes answer counts=1 beside
	// ViewCounts: notes per saved view, by view id. Neither is sent when the
	// tags could not be read in full.
	QueryNotes []sessionquery.Note            `json:"query_notes,omitempty"`
	ViewNotes  map[string][]sessionquery.Note `json:"view_notes,omitempty"`
	// MoreTextMatches: see sessionGroupPageResponse.
	MoreTextMatches bool `json:"more_text_matches,omitempty"`
	// PlacementCounts and PlacementNotes answer a board_view read of a board
	// that has placement rules: per rule, in the view's order, how many of
	// the view's sessions the rule placed; and the notes on the rules' queries.
	PlacementCounts []int            `json:"placement_counts,omitempty"`
	PlacementNotes  []boardRuleNotes `json:"placement_notes,omitempty"`
	// MatchTotal answers a filtered read: how many sessions the query matched,
	// before grouping and the group limit. It is absent when the number would
	// not hold, under the rule ViewCounts follows: a query that is not durable,
	// or tags read in part. A pointer, so "matched none" is 0 and not absent.
	MatchTotal *int `json:"match_total,omitempty"`
}

type sessionPageResponse struct {
	Repository string                      `json:"repository"`
	Mode       string                      `json:"mode"`
	Total      int                         `json:"total"`
	Offset     int                         `json:"offset"`
	Limit      int                         `json:"limit"`
	Sessions   []railSession               `json:"sessions"`
	Activity   *sessionactivity.Capability `json:"activity,omitempty"`
	// AgentChildren folds agent sessions (child sessions of managed runs)
	// under their parent rail row, keyed by the parent's session id (G-5:
	// agent runs never render as flat rail rows; Total counts non-agent
	// sessions only). Only parents present on this page carry entries.
	// Decorated at the daemon layer so harvest stays orchestration-free.
	AgentChildren map[string][]agentChildSession `json:"agent_children,omitempty"`
	// NativeChildren folds native (vendor-observed) subagent sessions under
	// their parent rail row, keyed by the parent's session id. Provenance is
	// separate from AgentChildren (caused) — the two classes never merge
	// (native-session-lineage-plan §3).
	NativeChildren map[string][]nativeChildSession `json:"native_children,omitempty"`
}

// agentChildSession is one agent session folded under its parent rail row.
type agentChildSession struct {
	SessionSummary
	AgentRole string `json:"agent_role"`
}

// nativeChildSession is one native subagent session folded under its parent
// rail row. NativeRole is the vendor-published role label; NativeKind the
// vendor-neutral lineage kind.
type nativeChildSession struct {
	SessionSummary
	NativeRole string `json:"native_role"`
	NativeKind string `json:"native_kind"`
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	handleSessionsWith(w, r, ScanSessions, currentPresenceOpenSet)
}

func handleSessionsWith(w http.ResponseWriter, r *http.Request, scan func() []SessionSummary, openSet func(time.Time) presenceOpenSet) {
	view := r.URL.Query().Get("view")
	if view == "" {
		// Compatibility contract: Governance and older clients consume the bare array.
		writeJSON(w, scan())
		return
	}
	switch view {
	case "rail":
		limit, ok := boundedQueryInt(w, r, "repository_limit", defaultRepositoryLimit, maxRepositoryLimit)
		if !ok {
			return
		}
		if organizedRequest(r) {
			handleOrganizedRail(w, r, scan, openSet, limit)
			return
		}
		sessions := scan()
		presence := openSet(time.Now())
		groups := buildSessionRepositories(sessions, presence)
		total := len(groups)
		if len(groups) > limit {
			groups = groups[:limit]
		}
		views := requestedViewCounts(r, sessions, presence)
		writeJSON(w, sessionRailResponse{
			Activity: presence.Capability, RepositoryTotal: total, Repositories: groups,
			RuntimeHealth:         sessionScanCoalescer.runtimeHealth(),
			ChangeEvidenceProblem: sessionScanCoalescer.changeEvidenceProblem(),
			ViewCounts:            views.Counts, ViewNotes: views.Notes,
		})
	case "group":
		handleOrganizedGroup(w, r, scan, openSet)
	case "repository":
		key := r.URL.Query().Get("repository")
		if key == "" || len(key) > 4096 {
			http.Error(w, "repository is required and must be at most 4096 bytes", http.StatusBadRequest)
			return
		}
		mode := r.URL.Query().Get("mode")
		if mode != "all" && mode != "open" {
			http.Error(w, "mode must be all or open", http.StatusBadRequest)
			return
		}
		offset, ok := boundedQueryInt(w, r, "offset", 0, int(^uint(0)>>1))
		if !ok {
			return
		}
		limit, ok := boundedQueryInt(w, r, "limit", defaultSessionPageSize, maxSessionPageSize)
		if !ok {
			return
		}
		sessions := scan()
		presence := unknownPresence("Open-session observation was not requested for this page.")
		if mode == "open" {
			presence = openSet(time.Now())
		}
		selectedRuntime := r.URL.Query().Get("selected_runtime")
		selectedID := r.URL.Query().Get("selected_id")
		if len(selectedRuntime) > 64 || len(selectedID) > 4096 {
			http.Error(w, "selected session identity is too long", http.StatusBadRequest)
			return
		}
		page, found := buildSessionPage(sessions, presence, key, mode, offset, limit, selectedRuntime, selectedID)
		if !found {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		decorateSessionPage(&page)
		writeJSON(w, page)
	default:
		http.Error(w, "view must be rail, repository or group", http.StatusBadRequest)
	}
}

func boundedQueryInt(w http.ResponseWriter, r *http.Request, name string, fallback, max int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > max || (name == "limit" && n == 0) || (name == "repository_limit" && n == 0) {
		http.Error(w, name+" is outside the supported range", http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

// isOpen answers the rail's only openness question, from the published set and
// nothing else. The capability check is load-bearing: an unavailable reading
// closes every row even when the set is populated, so "we do not know" never
// renders as "nothing is open".
//
// Freshness grade is not consulted — see presenceOpenSetFrom for why the fading
// tail counts as open and where the fade is configured.
func (p presenceOpenSet) isOpen(s SessionSummary) bool {
	if p.Capability.Status != "available" {
		return false
	}
	return p.Open[presenceCatalogKey(s.Runtime, s.ID)]
}

func buildSessionRepositories(sessions []SessionSummary, presence presenceOpenSet) []sessionRepository {
	// Native subagents are excluded from the rail header count (RT2), matching
	// the page Total. Caused-agent children were already absent from the page
	// path but are NOT partitioned here — that pre-existing divergence is not
	// widened by this change; native children are, to keep the new fold honest.
	sessions = partitionNativeMainRows(sessions)
	groups := map[string]*sessionRepository{}
	for _, s := range sessions {
		key := s.RepositoryKey
		if key == "" {
			key = harvest.RepositoryGroupKey(s)
		}
		group := groups[key]
		if group == nil {
			group = &sessionRepository{Key: key}
			if filepath.IsAbs(key) && key != "/" {
				group.LaunchCwd = key
			}
			groups[key] = group
		}
		group.Total++
		if group.Newest.IsZero() || s.Modified.After(group.Newest) {
			group.Newest = s.Modified
		}
		if presence.isOpen(s) {
			group.OpenTotal++
			if group.NewestOpen.IsZero() || s.Modified.After(group.NewestOpen) {
				group.NewestOpen = s.Modified
			}
		} else if s.ActivityStatus == "unknown" {
			group.PresenceUnknownTotal++
		}
	}
	out := make([]sessionRepository, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.OpenTotal > 0) != (b.OpenTotal > 0) {
			return a.OpenTotal > 0
		}
		if a.OpenTotal > 0 && !a.NewestOpen.Equal(b.NewestOpen) {
			return a.NewestOpen.After(b.NewestOpen)
		}
		if (a.Key == harvest.NoProjectKey) != (b.Key == harvest.NoProjectKey) {
			return b.Key == harvest.NoProjectKey
		}
		if !a.Newest.Equal(b.Newest) {
			return a.Newest.After(b.Newest)
		}
		return strings.ToLower(a.Key) < strings.ToLower(b.Key)
	})
	return out
}

// sessionIdentityAlternates returns every id something about one session row
// may be recorded under: artifact id, vendor meta id, vendor thread id (g4
// plan §3 — the store keys agent children by the TASK row's native id, which
// for codex is the thread id, so every alternate must be offered; nothing is
// inferred). These are lookup keys, never a membership decision: a subagent
// rollout carries its parent's thread id, which names the parent. Watching,
// candidate lookup and agent-session classification keep all of them, because
// a subagent's activity is recorded under that thread (child-thread-identity
// plan D-1). Whatever must name the session itself uses sessionIdentities.
func sessionIdentityAlternates(row SessionSummary) []string {
	ids := make([]string, 0, 3)
	for _, id := range []string{row.ID, row.MetaID, row.ThreadID} {
		if id == "" {
			continue
		}
		duplicate := false
		for _, seen := range ids {
			if seen == id {
				duplicate = true
				break
			}
		}
		if !duplicate {
			ids = append(ids, id)
		}
	}
	return ids
}

// sessionIdentities returns the ids that name this session: the alternates
// its runtime's MatchID accepts, its own artifact id first. A subagent
// rollout's thread id names its parent, so it is left out; a primary keeps
// every alternate it has.
func sessionIdentities(row SessionSummary) []string {
	alternates := sessionIdentityAlternates(row)
	ids := make([]string, 0, len(alternates))
	for _, id := range alternates {
		if id == row.ID || harvest.MatchID(row, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// agentSessionParentsRead resolves which native session ids are agent child
// sessions and who their parent is. A test may stub it; failures and an
// absent governor degrade to nil — the rail stays flat and honestly
// undecorated, never an error.
var agentSessionParentsRead = func(ids []string) map[string]store.AgentSessionParent {
	if governor == nil || governor.ix == nil || len(ids) == 0 {
		return nil
	}
	parents, err := governor.ix.AgentSessionParents(ids)
	if err != nil {
		return nil
	}
	return parents
}

// partitionAgentSessions removes agent sessions from the pageable rows and
// folds each under its parent by exact root-id equality (G-5). An agent
// session whose parent is not among the remaining rows renders nowhere on
// the rail — the strip and Related sessions own it. Rows arrive sorted, so
// child lists inherit recency order.
func partitionAgentSessions(rows []SessionSummary) ([]SessionSummary, map[string][]agentChildSession) {
	ids := make([]string, 0, len(rows)*2)
	for _, row := range rows {
		ids = append(ids, sessionIdentityAlternates(row)...)
	}
	return partitionAgentSessionsUsing(rows, agentSessionParentsRead(ids))
}

// agentParentOf says whether a session is a Crossing Guard agent's own
// session (a managed run's child or a group's helper session) and whose,
// matching any of the row's identity alternates.
func agentParentOf(row SessionSummary, parents map[string]store.AgentSessionParent) (store.AgentSessionParent, bool) {
	for _, id := range sessionIdentityAlternates(row) {
		if parent, ok := parents[id]; ok {
			return parent, true
		}
	}
	return store.AgentSessionParent{}, false
}

// partitionAgentSessionsUsing is the fold itself, over parents the caller
// already holds. The filtered rail reads every parent once for the whole list
// instead of asking per 200 ids.
func partitionAgentSessionsUsing(rows []SessionSummary, parents map[string]store.AgentSessionParent) ([]SessionSummary, map[string][]agentChildSession) {
	if len(parents) == 0 {
		return rows, nil
	}
	parentOf := func(row SessionSummary) (store.AgentSessionParent, bool) { return agentParentOf(row, parents) }
	main := make([]SessionSummary, 0, len(rows))
	agents := make([]SessionSummary, 0)
	rowIDByIdentity := map[string]string{}
	runtimeByRowID := map[string]string{}
	for _, row := range rows {
		if _, ok := parentOf(row); ok {
			agents = append(agents, row)
			continue
		}
		main = append(main, row)
		// A root id folds under the row it names: a newer subagent row also
		// carries its parent's thread id, and must not take the parent's agents.
		for _, id := range sessionIdentities(row) {
			if _, taken := rowIDByIdentity[id]; !taken {
				rowIDByIdentity[id] = row.ID
			}
		}
		runtimeByRowID[row.ID] = row.Runtime
	}
	if len(agents) == 0 {
		return main, nil
	}
	children := map[string][]agentChildSession{}
	for _, row := range agents {
		parent, _ := parentOf(row)
		for _, rootID := range []string{parent.RootCatalogSessionID, parent.RootNativeSessionID} {
			if rootID == "" {
				continue
			}
			parentRowID, ok := rowIDByIdentity[rootID]
			if !ok {
				continue
			}
			if parent.RootRuntime != "" && runtimeByRowID[parentRowID] != parent.RootRuntime {
				continue
			}
			children[parentRowID] = append(children[parentRowID], agentChildSession{SessionSummary: row, AgentRole: parent.Role})
			break
		}
	}
	return main, children
}

// isNativeChild is the one native-child predicate: a row whose own runtime
// reports a parent (vendor-observed lineage). Shared by partitionNativeMainRows
// and partitionNativeChildren so the header count and the page partition can
// never disagree.
func isNativeChild(row SessionSummary) bool {
	facts, ok := harvest.Lineage(row)
	return ok && facts.Parent.ID != "" && facts.Parent.Runtime == row.Runtime
}

// partitionNativeMainRows returns the non-native-child rows only, for the rail
// header count. It shares the child test with partitionNativeChildren so the
// header and page counts cannot disagree.
func partitionNativeMainRows(rows []SessionSummary) []SessionSummary {
	main := make([]SessionSummary, 0, len(rows))
	for _, row := range rows {
		if isNativeChild(row) {
			continue
		}
		main = append(main, row)
	}
	return main
}

// partitionNativeChildren removes native (vendor-observed) subagent rows from
// the pageable rows and folds each under its nearest top-level ancestor, keyed
// by that ancestor's session id. Provenance stays separate from the caused
// agent fold (partitionAgentSessionsUsing): a row the caused fold already
// removed never reaches here, so the two maps are disjoint.
//
// Parent resolution walks parent_id up through child rows to the first
// non-child row (RT1 depth-2), using separate non-child and child identity maps
// so a child's own thread id — which equals its parent's thread id for codex —
// can never shadow the parent.
func partitionNativeChildren(rows []SessionSummary) ([]SessionSummary, map[string][]nativeChildSession) {
	children := []SessionSummary{}
	main := make([]SessionSummary, 0, len(rows))
	parentID := map[string]bool{} // a child claims this identity as one of its alternates
	nonChildRowID := map[string]string{}
	// childByOwnID resolves a parent_id to the child whose OWN identity it names,
	// keyed by MetaID (fallback ID) only — never ThreadID, which for codex equals
	// the parent's thread id and would let a sibling shadow the absent parent (PW2).
	childByOwnID := map[string]SessionSummary{}
	for _, row := range rows {
		if !isNativeChild(row) {
			main = append(main, row)
			for _, id := range sessionIdentityAlternates(row) {
				if _, taken := nonChildRowID[id]; !taken {
					nonChildRowID[id] = row.ID
				}
			}
			continue
		}
		children = append(children, row)
		for _, id := range sessionIdentityAlternates(row) {
			parentID[id] = true
		}
		own := row.MetaID
		if own == "" {
			own = row.ID
		}
		childByOwnID[own] = row
	}
	if len(children) == 0 {
		return rows, nil
	}
	// Map each child to its nearest top-level ancestor's row id, walking
	// parent_id up through child rows.
	childParentRowID := map[string]string{} // child row id -> top-level ancestor row id
	for _, child := range children {
		facts, _ := harvest.Lineage(child)
		parent := facts.Parent.ID
		seen := map[string]bool{}
		for {
			if rowID, ok := nonChildRowID[parent]; ok {
				childParentRowID[child.ID] = rowID
				break
			}
			if seen[parent] || !parentID[parent] {
				break // orphan: parent resolves to no scanned non-child row
			}
			seen[parent] = true
			next, ok := childByOwnID[parent]
			if !ok {
				break
			}
			nf, _ := harvest.Lineage(next)
			parent = nf.Parent.ID
		}
	}
	folded := map[string][]nativeChildSession{}
	for _, child := range children {
		rowID, ok := childParentRowID[child.ID]
		if !ok {
			continue
		}
		facts, _ := harvest.Lineage(child)
		folded[rowID] = append(folded[rowID], nativeChildSession{
			SessionSummary: child, NativeRole: facts.Role, NativeKind: facts.Kind,
		})
	}
	return main, folded
}

func buildSessionPage(sessions []SessionSummary, presence presenceOpenSet, key, mode string, offset, limit int, selectedRuntime, selectedID string) (sessionPageResponse, bool) {
	rows := make([]SessionSummary, 0)
	found := false
	for _, s := range sessions {
		sessionKey := s.RepositoryKey
		if sessionKey == "" {
			sessionKey = harvest.RepositoryGroupKey(s)
		}
		if sessionKey != key {
			continue
		}
		found = true
		rows = append(rows, s)
	}
	if !found {
		return sessionPageResponse{}, false
	}
	sortSessionSummaries(rows)
	// G-5: agent sessions fold under their parents BEFORE the mode filter and
	// pagination — the fold is an annotation of the parent, mode-invariant,
	// and Total counts non-agent sessions only, so pager math stays flat.
	main, children := partitionAgentSessions(rows)
	// Native subagents fold the same way, after the caused fold, so a row the
	// caused fold removed never reaches here (disjoint provenance, RT4).
	main, nativeChildren := partitionNativeChildren(main)
	if mode == "open" {
		filtered := main[:0]
		for _, s := range main {
			if presence.isOpen(s) {
				filtered = append(filtered, s)
			}
		}
		main = filtered
	}
	rows = main
	total := len(rows)
	if mode == "all" && selectedRuntime != "" && selectedID != "" {
		for i, row := range rows {
			match := row.Runtime == selectedRuntime && (row.ID == selectedID || harvest.MatchID(row, selectedID))
			// A selected AGENT session lands on its parent's page with the
			// fold expanded (g4 plan §3a) — never an unfindable selection.
			if !match {
				for _, child := range children[row.ID] {
					if child.Runtime == selectedRuntime && (child.ID == selectedID || harvest.MatchID(child.SessionSummary, selectedID)) {
						match = true
						break
					}
				}
			}
			if !match {
				for _, child := range nativeChildren[row.ID] {
					if child.Runtime == selectedRuntime && (child.ID == selectedID || harvest.MatchID(child.SessionSummary, selectedID)) {
						match = true
						break
					}
				}
			}
			if match {
				offset = (i / limit) * limit
				break
			}
		}
	}
	if total == 0 {
		offset = 0
	} else if offset >= total {
		offset = ((total - 1) / limit) * limit
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := sessionPageResponse{
		Repository: key, Mode: mode, Total: total, Offset: offset, Limit: limit,
		Sessions: railSessions(rows[offset:end]),
	}
	if len(children) > 0 {
		pageChildren := map[string][]agentChildSession{}
		for _, row := range page.Sessions {
			if fold := children[row.ID]; len(fold) > 0 {
				pageChildren[row.ID] = fold
			}
		}
		if len(pageChildren) > 0 {
			page.AgentChildren = pageChildren
		}
	}
	if len(nativeChildren) > 0 {
		pageNative := map[string][]nativeChildSession{}
		for _, row := range page.Sessions {
			if fold := nativeChildren[row.ID]; len(fold) > 0 {
				pageNative[row.ID] = fold
			}
		}
		if len(pageNative) > 0 {
			page.NativeChildren = pageNative
		}
	}
	if mode == "open" {
		capability := presence.Capability
		page.Activity = &capability
	}
	return page, true
}

// railSessions wraps summaries as rail rows. Decoration happens after, in the
// handler, so the page builder stays a pure function of the scan.
func railSessions(rows []SessionSummary) []railSession {
	out := make([]railSession, 0, len(rows))
	for _, row := range rows {
		out = append(out, railSession{SessionSummary: row})
	}
	return out
}

func sortSessionSummaries(rows []SessionSummary) {
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Modified.Equal(rows[j].Modified) {
			return rows[i].Modified.After(rows[j].Modified)
		}
		if rows[i].Runtime != rows[j].Runtime {
			return rows[i].Runtime < rows[j].Runtime
		}
		return rows[i].ID < rows[j].ID
	})
}
