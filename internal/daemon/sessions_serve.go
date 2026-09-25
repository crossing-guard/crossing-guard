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
	Label      string    `json:"label,omitempty"`
	Key        string    `json:"key"`
	LaunchCwd  string    `json:"launch_cwd,omitempty"`
	Total      int       `json:"total"`
	OpenTotal  int       `json:"open_total"`
	Newest     time.Time `json:"newest"`
	NewestOpen time.Time `json:"newest_open,omitempty"`
}

type sessionRailResponse struct {
	Activity        sessionactivity.Capability `json:"activity"`
	RepositoryTotal int                        `json:"repository_total"`
	Repositories    []sessionRepository        `json:"repositories"`
	// GroupBy is set when the groups are not repositories (a filtered rail may
	// group by runtime or by one of the owner's tag keys).
	GroupBy string `json:"group_by,omitempty"`
	// ViewCounts answers counts=1: sessions per saved view, by view id. A view
	// that cannot be counted truthfully is absent rather than approximated.
	ViewCounts map[string]int `json:"view_counts,omitempty"`
	// MoreTextMatches: see sessionGroupPageResponse.
	MoreTextMatches bool `json:"more_text_matches,omitempty"`
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
}

// agentChildSession is one agent session folded under its parent rail row.
type agentChildSession struct {
	SessionSummary
	AgentRole string `json:"agent_role"`
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
		writeJSON(w, sessionRailResponse{
			Activity: presence.Capability, RepositoryTotal: total, Repositories: groups,
			ViewCounts: requestedViewCounts(r, sessions, presence),
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

// sessionIdentityAlternates returns one session row's exact identity forms:
// artifact id, vendor meta id, vendor thread id (g4 plan §3 — the store keys
// agent children by the TASK row's native id, which for codex is the thread
// id, so every alternate must be offered; nothing is inferred).
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

// partitionAgentSessionsUsing is the fold itself, over parents the caller
// already holds. The filtered rail reads every parent once for the whole list
// instead of asking per 200 ids.
func partitionAgentSessionsUsing(rows []SessionSummary, parents map[string]store.AgentSessionParent) ([]SessionSummary, map[string][]agentChildSession) {
	if len(parents) == 0 {
		return rows, nil
	}
	parentOf := func(row SessionSummary) (store.AgentSessionParent, bool) {
		for _, id := range sessionIdentityAlternates(row) {
			if parent, ok := parents[id]; ok {
				return parent, true
			}
		}
		return store.AgentSessionParent{}, false
	}
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
		for _, id := range sessionIdentityAlternates(row) {
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
