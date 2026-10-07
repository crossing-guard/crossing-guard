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
	if errors.Is(err, sessionquery.ErrQuery) || errors.Is(err, errBoardView) {
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
	boardView, err := requestedBoardView(r, request, limits)
	if err != nil {
		writeSessionQueryError(w, err)
		return
	}
	now := time.Now()
	presence := openSet(now)
	snapshot := sessionTagSnapshots.get(now)
	if !tagsReadable(w, snapshot) {
		return
	}
	rows, _, _ := organizeSessions(scan(), presence, snapshot, now)
	vocabulary := organizedVocabulary(rows)
	matched, more, err := matchSessions(rows, vocabulary, request, now, limits.TextHitLimit)
	if err != nil {
		writeSessionQueryError(w, err)
		return
	}
	// The board's grouping (sessions-board plan §3.3): owner-provenance
	// grouping for tag-key groupings, so a detector fact sharing the group
	// key can never place a row in a column a drag would write a tag into.
	// A board view named by board_view adds its placement rules for the rows
	// the owner has not placed (board-observed-columns plan §2.2). The rail's
	// plain tag-key grouping keeps its old shape (compatibility).
	var placer sessionPlacer
	var board *boardPlacement
	if boardView != nil {
		if board, err = bindBoardPlacement(*boardView, request.groupBy, vocabulary, request.limits, snapshot, now); err != nil {
			writeSessionQueryError(w, err)
			return
		}
		placer = board.place
	} else if r.URL.Query().Get("owner_groups") == "1" && request.groupBy.Kind == sessionquery.GroupTagKey {
		placer = sessionGroupKeysOwner
	}
	groups := buildSessionGroups(matched, request.groupBy, placer)
	total := len(groups)
	if len(groups) > limit {
		groups = groups[:limit]
	}
	response := sessionRailResponse{Activity: presence.Capability, RepositoryTotal: total, Repositories: groups,
		GroupBy: request.groupBy.String(), MoreTextMatches: more,
		ChangeEvidenceProblem: sessionScanCoalescer.changeEvidenceProblem()}
	// Notes and counts share one gate: a read cut short could miss the very
	// value that makes a term valid, and no note is better than a wrong one.
	if snapshot.countable() {
		response.QueryNotes = request.query.Notes(vocabulary)
		if request.query.Durable() {
			total := len(matched)
			response.MatchTotal = &total
		}
	}
	if board != nil && len(board.rules) > 0 {
		response.PlacementCounts, response.PlacementNotes = board.placed, board.ruleNotes()
	}
	if r.URL.Query().Get("counts") == "1" && snapshot.countable() {
		evaluation := countSessionViews(rows, loadSessionViews(sessionViewsDataDir(), limits).Views, request.limits, now)
		response.ViewCounts, response.ViewNotes = evaluation.Counts, evaluation.Notes
	}
	writeJSON(w, response)
}

// requestedViewCounts serves counts=1 on the unfiltered rail — the view list's
// own read. It decorates and folds the whole list, which the plain rail
// otherwise never pays for.
func requestedViewCounts(r *http.Request, sessions []SessionSummary, presence presenceOpenSet) viewEvaluation {
	if r.URL.Query().Get("counts") != "1" {
		return viewEvaluation{}
	}
	limits := sessionOrganizationConfig()
	views := loadSessionViews(sessionViewsDataDir(), limits).Views
	now := time.Now()
	snapshot := sessionTagSnapshots.get(now)
	// No number is better than a wrong one: an unreadable tag store is not
	// "nothing is tagged", and a read cut short is not the whole truth.
	if len(views) == 0 || !snapshot.countable() {
		return viewEvaluation{}
	}
	rows, _, _ := organizeSessions(sessions, presence, snapshot, now)
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
	// A tag group is named in any letter case and read by its one key
	// (board-column-letter-case plan §2).
	page.group = sessionGroupKey(request.groupBy, page.group)
	if page.mode != "all" && page.mode != "open" {
		http.Error(w, "mode must be all or open", http.StatusBadRequest)
		return
	}
	var ok bool
	if page.offset, ok = boundedQueryInt(w, r, "offset", 0, int(^uint(0)>>1)); !ok {
		return
	}
	// owner_groups=1 on a tag-key grouping places rows by owner tags, as the
	// rail read that counted the group did. Two callers: the board's column
	// read, which names no limit and so takes the configured column bound
	// (board-column-overflow plan §2.2), and a board view's rail page, which
	// names its own. The ceiling is never below the ordinary page ceiling,
	// so a small column bound cannot refuse the rail
	// (board-rail-owner-paging plan §2.1). Elsewhere it is ignored.
	// board_view names the board view itself: the same bound, and the view's
	// placement rules for rows the owner has not placed
	// (board-observed-columns plan §2.2).
	boardView, err := requestedBoardView(r, request, limits)
	if err != nil {
		writeSessionQueryError(w, err)
		return
	}
	defaultLimit, maxLimit := defaultSessionPageSize, maxSessionPageSize
	if boardView != nil || (r.URL.Query().Get("owner_groups") == "1" && request.groupBy.Kind == sessionquery.GroupTagKey) {
		page.placer = sessionGroupKeysOwner
		defaultLimit, maxLimit = limits.BoardColumnCardsMax, max(limits.BoardColumnCardsMax, maxSessionPageSize)
	}
	if page.limit, ok = boundedQueryInt(w, r, "limit", defaultLimit, maxLimit); !ok {
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
	rows, children, nativeChildren := organizeSessions(scan(), presence, snapshot, now)
	vocabulary := organizedVocabulary(rows)
	matched, more, err := matchSessions(rows, vocabulary, request, now, limits.TextHitLimit)
	if err != nil {
		writeSessionQueryError(w, err)
		return
	}
	if boardView != nil {
		board, err := bindBoardPlacement(*boardView, request.groupBy, vocabulary, request.limits, snapshot, now)
		if err != nil {
			writeSessionQueryError(w, err)
			return
		}
		page.placer = board.place
	}
	response := buildSessionGroupPage(matched, children, nativeChildren, presence, request, page, limits.RowTagsMax)
	response.MoreTextMatches = more
	writeJSON(w, response)
}
