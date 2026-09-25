package daemon

// The filtered rail (session-organization implementation plan §4.3, §6). A
// request that carries a query or a grouping runs this pipeline; a request that
// carries neither runs the functions in sessions_serve.go untouched, so today's
// rail is byte-for-byte what it was.
//
//	scan → + sessions only the owner's tag remembers → sort → fold agent
//	children under parents (all parents read once) → decorate → match →
//	[words: search confined to the matches] → group → totals | one group's page
//
// Filtering happens after the agent fold, on top-level rows only, so a group's
// total and its pages always agree and a folded child never matches alone.

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/internal/sessionactivity"
	"crossing-guard/internal/sessionquery"
	"crossing-guard/store"
)

// sessionGroupPageResponse is one group's page. It is its own type rather than
// sessionPageResponse because a group key may be a tag value or a runtime, and
// calling that "repository" would be false.
type sessionGroupPageResponse struct {
	Group         string                         `json:"group"`
	GroupBy       string                         `json:"group_by"`
	Mode          string                         `json:"mode"`
	Total         int                            `json:"total"`
	Offset        int                            `json:"offset"`
	Limit         int                            `json:"limit"`
	Sessions      []railSession                  `json:"sessions"`
	Activity      *sessionactivity.Capability    `json:"activity,omitempty"`
	AgentChildren map[string][]agentChildSession `json:"agent_children,omitempty"`
	// MoreTextMatches reports that more sessions mention the search words than
	// text_hit_limit allows, so the list is the best matches, not all of them.
	MoreTextMatches bool `json:"more_text_matches,omitempty"`
}

// organizedRow is a top-level rail row with everything a query can ask of it.
type organizedRow struct {
	rail  railSession
	query sessionquery.Row
}

type sessionQueryRequest struct {
	query   sessionquery.Query
	groupBy sessionquery.GroupBy
	sort    string
	limits  sessionquery.Limits
}

func sessionQueryLimits(o ConsoleSessionOrganization) sessionquery.Limits {
	return sessionquery.Limits{QueryBytes: o.QueryBytesMax, Terms: o.QueryTermsMax, GlobExpansion: o.GlobExpansionMax}
}

// parseSessionQueryRequest reads query, group_by and sort. Every problem comes
// back as the sentence the owner sees under the filter bar.
func parseSessionQueryRequest(r *http.Request, limits sessionquery.Limits) (sessionQueryRequest, error) {
	request := sessionQueryRequest{limits: limits}
	var err error
	if request.query, err = sessionquery.Parse(r.URL.Query().Get("query"), limits); err != nil {
		return request, err
	}
	if request.groupBy, err = sessionquery.ParseGroupBy(r.URL.Query().Get("group_by")); err != nil {
		return request, err
	}
	request.sort, err = sessionquery.ParseSort(r.URL.Query().Get("sort"))
	return request, err
}

// sessionExecutionRead reports the status decider's execution word per session
// (keyed like the open set). A session with no published frame is absent, and
// the grammar reads absence as the decider's own "unknown".
var sessionExecutionRead = func(now time.Time) map[string]string {
	service := sessionActivityService()
	if service == nil {
		return nil
	}
	out := map[string]string{}
	for _, item := range service.Snapshot().Items {
		if item.CatalogSessionID == "" || (!item.ExpiresAt.IsZero() && now.After(item.ExpiresAt)) {
			continue
		}
		out[presenceCatalogKey(item.Runtime, item.CatalogSessionID)] = item.Execution
	}
	return out
}

// organizeSessions builds the decorated top-level rows and the agent fold.
func organizeSessions(sessions []SessionSummary, presence presenceOpenSet, snapshot sessionTagSnapshot, now time.Time) ([]organizedRow, map[string][]agentChildSession) {
	gone := snapshot.vanished(sessions)
	goneKeys := map[string]bool{}
	for _, row := range gone {
		goneKeys[presenceCatalogKey(row.Runtime, row.ID)] = true
	}
	rows := append(append([]SessionSummary(nil), sessions...), gone...)
	sortSessionSummaries(rows)
	main, children := partitionAgentSessionsUsing(rows, snapshot.parents)
	execution := sessionExecutionRead(now)
	out := make([]organizedRow, 0, len(main))
	for _, summary := range main {
		// Uncapped: a query must see every tag. The display cap is applied only
		// when a row is serialized (listRow), never before matching.
		rail := snapshot.decorate(summary)
		rail.TranscriptMissing = goneKeys[presenceCatalogKey(summary.Runtime, summary.ID)]
		out = append(out, organizedRow{rail: rail, query: queryRow(rail, presence, execution)})
	}
	return out, children
}

func queryRow(rail railSession, presence presenceOpenSet, execution map[string]string) sessionquery.Row {
	row := sessionquery.Row{Repository: sessionRepositoryKey(rail.SessionSummary), Runtime: rail.Runtime,
		Branch: rail.Branch, Title: rail.Title, Note: rail.Note, TouchedAt: rail.Modified.Unix(),
		Open:   presence.isOpen(rail.SessionSummary),
		Status: execution[presenceCatalogKey(rail.Runtime, rail.ID)]}
	for _, tag := range rail.Tags {
		row.Tags = append(row.Tags, sessionquery.Tag{Key: tag.Key, Value: tag.Value, At: tag.At,
			Owner: tag.Provenance == string(engine.UserAsserted)})
	}
	for _, fact := range rail.Facts {
		row.Tags = append(row.Tags, sessionquery.Tag{Key: fact.Key, Value: fact.Value, At: fact.At})
	}
	return row
}

func sessionRepositoryKey(s SessionSummary) string {
	if s.RepositoryKey != "" {
		return s.RepositoryKey
	}
	return harvest.RepositoryGroupKey(s)
}

// sessionTextSearch finds which of the given sessions mention the words, and
// whether more matched than the limit. It is a seam so tests need no index.
var sessionTextSearch = func(words string, sessionIDs []string, limit int) ([]store.TranscriptProjectionKey, bool, error) {
	ix, err := store.OpenRO(indexPath())
	if err != nil {
		return nil, false, err
	}
	defer ix.Close()
	return ix.SessionsMatchingText(words, sessionIDs, limit)
}

// errSessionTextUnavailable marks a failure of the text search itself, which is
// the daemon's problem and must not be reported as the owner's bad query.
var errSessionTextUnavailable = errors.New("transcript search is unavailable")

// matchSessions applies the query. The words leg searches only inside what the
// structured terms left, so a word that is rare in a view is found even when it
// ranks far down globally.
// The bool reports that more sessions mention the words than the limit allows.
func matchSessions(rows []organizedRow, request sessionQueryRequest, now time.Time, textLimit int) ([]organizedRow, bool, error) {
	vocabularyRows := make([]sessionquery.Row, 0, len(rows))
	for _, row := range rows {
		vocabularyRows = append(vocabularyRows, row.query)
	}
	bound, err := request.query.Bind(sessionquery.NewVocabulary(vocabularyRows), request.limits, now.Unix())
	if err != nil {
		return nil, false, err
	}
	matched := make([]organizedRow, 0, len(rows))
	for _, row := range rows {
		if bound.Matches(row.query) {
			row.rail.InViewSince = bound.InViewSince(row.query)
			matched = append(matched, row)
		}
	}
	words := request.query.Words()
	if len(words) == 0 {
		return matched, false, nil
	}
	return confineToTextHits(matched, strings.Join(words, " "), textLimit)
}

func confineToTextHits(rows []organizedRow, words string, limit int) ([]organizedRow, bool, error) {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, harvest.CanonicalID(row.rail.SessionSummary))
	}
	hits, truncated, err := sessionTextSearch(words, ids, limit)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", errSessionTextUnavailable, err)
	}
	hit := map[string]bool{}
	for _, h := range hits {
		hit[presenceCatalogKey(h.Runtime, h.SessionID)] = true
	}
	out := rows[:0:0]
	for _, row := range rows {
		if hit[presenceCatalogKey(row.rail.Runtime, harvest.CanonicalID(row.rail.SessionSummary))] {
			out = append(out, row)
		}
	}
	return out, truncated, nil
}

// sessionGroupKeys places a row. A row with several values under a tag key is
// in each of those groups; a row with none is in the empty-named group.
func sessionGroupKeys(row organizedRow, groupBy sessionquery.GroupBy) []string {
	switch groupBy.Kind {
	case sessionquery.GroupRuntime:
		return []string{row.rail.Runtime}
	case sessionquery.GroupNone:
		return []string{""}
	case sessionquery.GroupTagKey:
		seen := map[string]bool{}
		var keys []string
		for _, tag := range row.query.Tags {
			folded := strings.ToLower(tag.Value)
			if strings.ToLower(tag.Key) == groupBy.Key && tag.Value != "" && !seen[folded] {
				seen[folded] = true
				keys = append(keys, folded)
			}
		}
		if len(keys) == 0 {
			return []string{""}
		}
		return keys
	}
	return []string{row.query.Repository}
}

// buildSessionGroups is buildSessionRepositories over matched rows and any
// grouping. Under a tag-key grouping a session with two values is in both
// groups, so group totals may add up to more than the number of sessions; a
// view's count (countSessionViews) is always the number of sessions. Group order is the rail's: open first, then most recent, the
// unnamed group last.
func buildSessionGroups(rows []organizedRow, groupBy sessionquery.GroupBy) []sessionRepository {
	groups := map[string]*sessionRepository{}
	for _, row := range rows {
		for _, key := range sessionGroupKeys(row, groupBy) {
			group := groups[key]
			if group == nil {
				group = &sessionRepository{Key: key, Label: sessionGroupLabel(row, groupBy, key)}
				groups[key] = group
			}
			group.Total++
			modified := row.rail.Modified
			if group.Newest.IsZero() || modified.After(group.Newest) {
				group.Newest = modified
			}
			if row.query.Open {
				group.OpenTotal++
				if group.NewestOpen.IsZero() || modified.After(group.NewestOpen) {
					group.NewestOpen = modified
				}
			}
		}
	}
	out := make([]sessionRepository, 0, len(groups))
	for _, group := range groups {
		if groupBy.Kind == sessionquery.GroupRepository && strings.HasPrefix(group.Key, "/") && group.Key != "/" {
			group.LaunchCwd = group.Key
		}
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool { return sessionGroupBefore(out[i], out[j]) })
	return out
}

// sessionGroupLabel is a tag group's name as the owner spelled the tag; group
// keys themselves are folded so that spelling never splits a group.
func sessionGroupLabel(row organizedRow, groupBy sessionquery.GroupBy, key string) string {
	if groupBy.Kind != sessionquery.GroupTagKey {
		return ""
	}
	for _, tag := range row.query.Tags {
		if strings.ToLower(tag.Key) == groupBy.Key && strings.ToLower(tag.Value) == key && tag.Value != key {
			return tag.Value
		}
	}
	return ""
}

func sessionGroupBefore(a, b sessionRepository) bool {
	if (a.OpenTotal > 0) != (b.OpenTotal > 0) {
		return a.OpenTotal > 0
	}
	if a.OpenTotal > 0 && !a.NewestOpen.Equal(b.NewestOpen) {
		return a.NewestOpen.After(b.NewestOpen)
	}
	unnamed := func(g sessionRepository) bool { return g.Key == "" || g.Key == harvest.NoProjectKey }
	if unnamed(a) != unnamed(b) {
		return unnamed(b)
	}
	if !a.Newest.Equal(b.Newest) {
		return a.Newest.After(b.Newest)
	}
	return strings.ToLower(a.Key) < strings.ToLower(b.Key)
}

type sessionGroupPageRequest struct {
	group, mode                 string
	offset, limit               int
	selectedRuntime, selectedID string
}

// buildSessionGroupPage pages one group. A group that matched nothing answers
// with an empty page, not an error: the owner may have just untagged its last
// session while looking at it.
func buildSessionGroupPage(rows []organizedRow, children map[string][]agentChildSession, presence presenceOpenSet, request sessionQueryRequest, page sessionGroupPageRequest, maxTags int) sessionGroupPageResponse {
	var members []organizedRow
	for _, row := range rows {
		for _, key := range sessionGroupKeys(row, request.groupBy) {
			if key == page.group && (page.mode != "open" || row.query.Open) {
				members = append(members, row)
				break
			}
		}
	}
	sortOrganizedRows(members, request.sort)
	offset := selectedOffset(members, children, page)
	total := len(members)
	if total == 0 {
		offset = 0
	} else if offset >= total {
		offset = ((total - 1) / page.limit) * page.limit
	}
	end := min(offset+page.limit, total)
	response := sessionGroupPageResponse{Group: page.group, GroupBy: request.groupBy.String(), Mode: page.mode,
		Total: total, Offset: offset, Limit: page.limit, Sessions: make([]railSession, 0, end-offset)}
	for _, row := range members[offset:end] {
		response.Sessions = append(response.Sessions, row.rail.listRow(maxTags))
		if fold := children[row.rail.ID]; len(fold) > 0 {
			if response.AgentChildren == nil {
				response.AgentChildren = map[string][]agentChildSession{}
			}
			response.AgentChildren[row.rail.ID] = fold
		}
	}
	if page.mode == "open" {
		capability := presence.Capability
		response.Activity = &capability
	}
	return response
}

// sortOrganizedRows orders a group. Rows arrive newest-first from the scan's
// stable sort, which "newest" keeps.
func sortOrganizedRows(rows []organizedRow, order string) {
	switch order {
	case sessionquery.SortOldest:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].rail.Modified.Before(rows[j].rail.Modified) })
	case sessionquery.SortLongest:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].rail.InViewSince < rows[j].rail.InViewSince })
	}
}

// selectedOffset moves to the page holding the selected session, as the
// repository page does, a folded agent child counting as its parent.
func selectedOffset(rows []organizedRow, children map[string][]agentChildSession, page sessionGroupPageRequest) int {
	if page.mode != "all" || page.selectedRuntime == "" || page.selectedID == "" {
		return page.offset
	}
	selected := func(s SessionSummary) bool {
		return s.Runtime == page.selectedRuntime && (s.ID == page.selectedID || harvest.MatchID(s, page.selectedID))
	}
	for index, row := range rows {
		match := selected(row.rail.SessionSummary)
		for _, child := range children[row.rail.ID] {
			match = match || selected(child.SessionSummary)
		}
		if match {
			return (index / page.limit) * page.limit
		}
	}
	return page.offset
}

// countSessionViews answers "how many sessions are in each saved view" over
// rows already decorated for this request: one scan, one decoration, one match
// per view. Only a durable view is counted; a view whose query no longer parses
// is skipped here and reported by the views owner.
func countSessionViews(rows []organizedRow, views []SavedSessionView, limits sessionquery.Limits, now time.Time) map[string]int {
	vocabularyRows := make([]sessionquery.Row, 0, len(rows))
	for _, row := range rows {
		vocabularyRows = append(vocabularyRows, row.query)
	}
	vocabulary := sessionquery.NewVocabulary(vocabularyRows)
	counts := map[string]int{}
	for _, view := range views {
		query, err := sessionquery.Parse(view.Query, limits)
		if err != nil || !query.Durable() {
			continue
		}
		bound, err := query.Bind(vocabulary, limits, now.Unix())
		if err != nil {
			continue
		}
		count := 0
		for _, row := range rows {
			if bound.Matches(row.query) {
				count++
			}
		}
		counts[view.ID] = count
	}
	return counts
}
