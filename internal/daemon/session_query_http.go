package daemon

// HTTP for the filtered rail. The pipeline itself is session_query_serve.go.

import (
	"errors"
	"net/http"
	"time"

	"crossing-guard/internal/sessionquery"
)

const (
	maxSessionGroupBytes    = 4096
	maxSessionRuntimeBytes  = 64
	maxSessionIdentityBytes = 4096
)

// tagsReadable refuses a filtered request when the tags could not be read. A
// filter answered from an unreadable store would show "no session is tagged x"
// — or, for an exclusion, every session — as though it were true.
func tagsReadable(w http.ResponseWriter, snapshot sessionTagSnapshot) bool {
	if snapshot.available {
		return true
	}
	http.Error(w, "tags cannot be read right now", http.StatusServiceUnavailable)
	return false
}

// writeSessionQueryError tells the owner's mistake from the daemon's: only a
// query that could not be understood is a 400.
func writeSessionQueryError(w http.ResponseWriter, err error) {
	if errors.Is(err, sessionquery.ErrQuery) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusServiceUnavailable)
}

// organizedRequest reports a rail request that asks for the filtered pipeline.
// Absent both parameters, the rail is served exactly as it always was.
func organizedRequest(r *http.Request) bool {
	return r.URL.Query().Get("query") != "" || r.URL.Query().Get("group_by") != ""
}

// decorateSessionPage adds tags and notes to a repository page built the old
// way. Rows nobody tagged are left exactly as they were.
func decorateSessionPage(page *sessionPageResponse) {
	snapshot := sessionTagSnapshots.get(time.Now())
	maxTags := sessionOrganizationConfig().RowTagsMax
	for index, row := range page.Sessions {
		page.Sessions[index] = snapshot.decorate(row.SessionSummary).listRow(maxTags)
	}
}

type sessionScan func() []SessionSummary
type sessionOpenSet func(time.Time) presenceOpenSet

func handleOrganizedRail(w http.ResponseWriter, r *http.Request, scan sessionScan, openSet sessionOpenSet, limit int) {
	limits := sessionOrganizationConfig()
	request, err := parseSessionQueryRequest(r, sessionQueryLimits(limits))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	presence := openSet(now)
	snapshot := sessionTagSnapshots.get(now)
	if !tagsReadable(w, snapshot) {
		return
	}
	rows, _ := organizeSessions(scan(), presence, snapshot, now)
	matched, more, err := matchSessions(rows, request, now, limits.TextHitLimit)
	if err != nil {
		writeSessionQueryError(w, err)
		return
	}
	groups := buildSessionGroups(matched, request.groupBy)
	total := len(groups)
	if len(groups) > limit {
		groups = groups[:limit]
	}
	response := sessionRailResponse{Activity: presence.Capability, RepositoryTotal: total, Repositories: groups,
		GroupBy: request.groupBy.String(), MoreTextMatches: more}
	if r.URL.Query().Get("counts") == "1" && snapshot.countable() {
		response.ViewCounts = countSessionViews(rows, loadSessionViews(sessionViewsDataDir(), limits).Views, request.limits, now)
	}
	writeJSON(w, response)
}

// requestedViewCounts serves counts=1 on the unfiltered rail. It decorates and
// folds the whole list, which the plain rail otherwise never pays for.
func requestedViewCounts(r *http.Request, sessions []SessionSummary, presence presenceOpenSet) map[string]int {
	if r.URL.Query().Get("counts") != "1" {
		return nil
	}
	limits := sessionOrganizationConfig()
	views := loadSessionViews(sessionViewsDataDir(), limits).Views
	now := time.Now()
	snapshot := sessionTagSnapshots.get(now)
	// No number is better than a wrong one: an unreadable tag store is not
	// "nothing is tagged", and a read cut short is not the whole truth.
	if len(views) == 0 || !snapshot.countable() {
		return nil
	}
	rows, _ := organizeSessions(sessions, presence, snapshot, now)
	return countSessionViews(rows, views, sessionQueryLimits(limits), now)
}

func handleOrganizedGroup(w http.ResponseWriter, r *http.Request, scan sessionScan, openSet sessionOpenSet) {
	limits := sessionOrganizationConfig()
	request, err := parseSessionQueryRequest(r, sessionQueryLimits(limits))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page := sessionGroupPageRequest{group: r.URL.Query().Get("group"), mode: r.URL.Query().Get("mode"),
		selectedRuntime: r.URL.Query().Get("selected_runtime"), selectedID: r.URL.Query().Get("selected_id")}
	if len(page.group) > maxSessionGroupBytes || len(page.selectedRuntime) > maxSessionRuntimeBytes || len(page.selectedID) > maxSessionIdentityBytes {
		http.Error(w, "group or selected session identity is too long", http.StatusBadRequest)
		return
	}
	if page.mode != "all" && page.mode != "open" {
		http.Error(w, "mode must be all or open", http.StatusBadRequest)
		return
	}
	var ok bool
	if page.offset, ok = boundedQueryInt(w, r, "offset", 0, int(^uint(0)>>1)); !ok {
		return
	}
	if page.limit, ok = boundedQueryInt(w, r, "limit", defaultSessionPageSize, maxSessionPageSize); !ok {
		return
	}
	now := time.Now()
	presence := unknownPresence("Open-session observation was not requested for this page.")
	if page.mode == "open" || !request.query.Durable() {
		presence = openSet(now)
	}
	snapshot := sessionTagSnapshots.get(now)
	if !tagsReadable(w, snapshot) {
		return
	}
	rows, children := organizeSessions(scan(), presence, snapshot, now)
	matched, more, err := matchSessions(rows, request, now, limits.TextHitLimit)
	if err != nil {
		writeSessionQueryError(w, err)
		return
	}
	response := buildSessionGroupPage(matched, children, presence, request, page, limits.RowTagsMax)
	response.MoreTextMatches = more
	writeJSON(w, response)
}
