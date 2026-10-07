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
	Group          string                          `json:"group"`
	GroupBy        string                          `json:"group_by"`
	Mode           string                          `json:"mode"`
	Total          int                             `json:"total"`
	Offset         int                             `json:"offset"`
	Limit          int                             `json:"limit"`
	Sessions       []railSession                   `json:"sessions"`
	Activity       *sessionactivity.Capability     `json:"activity,omitempty"`
	AgentChildren  map[string][]agentChildSession  `json:"agent_children,omitempty"`
	NativeChildren map[string][]nativeChildSession `json:"native_children,omitempty"`
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
func organizeSessions(sessions []SessionSummary, presence presenceOpenSet, snapshot sessionTagSnapshot, now time.Time) ([]organizedRow, map[string][]agentChildSession, map[string][]nativeChildSession) {
	gone := snapshot.vanished(sessions)
	goneKeys := map[string]bool{}
	for _, row := range gone {
		goneKeys[presenceCatalogKey(row.Runtime, row.ID)] = true
	}
	rows := append(append([]SessionSummary(nil), sessions...), gone...)
	sortSessionSummaries(rows)
	main, children := partitionAgentSessionsUsing(rows, snapshot.parents)
	main, nativeChildren := partitionNativeChildren(main)
	execution := sessionExecutionRead(now)
	out := make([]organizedRow, 0, len(main))
	for _, summary := range main {
		// Uncapped: a query must see every tag. The display cap is applied only
		// when a row is serialized (listRow), never before matching.
		rail := snapshot.decorate(summary)
		rail.TranscriptMissing = goneKeys[presenceCatalogKey(summary.Runtime, summary.ID)]
		out = append(out, organizedRow{rail: rail, query: queryRow(rail, presence, execution)})
	}
	return out, children, nativeChildren
}

func queryRow(rail railSession, presence presenceOpenSet, execution map[string]string) sessionquery.Row {
	row := sessionquery.Row{Repository: sessionRepositoryKey(rail.SessionSummary), Runtime: rail.Runtime,
		Branch: rail.Branch, Title: rail.Title, Note: rail.Note, TouchedAt: rail.Modified.Unix(),
		Open:   presence.isOpen(rail.SessionSummary),
		Status: execution[presenceCatalogKey(rail.Runtime, rail.ID)], Tags: queryTags(rail),
		Calls: rail.Turns, Lines: rail.Lines}
	return row
}

// queryTags is the one conversion of a decorated row's tags and facts into
// the grammar's tags: the owner's are marked, detector facts never are.
func queryTags(rail railSession) []sessionquery.Tag {
	var tags []sessionquery.Tag
	for _, tag := range rail.Tags {
		tags = append(tags, sessionquery.Tag{Key: tag.Key, Value: tag.Value, At: tag.At,
			Owner: tag.Provenance == string(engine.UserAsserted)})
	}
	for _, fact := range rail.Facts {
		tags = append(tags, sessionquery.Tag{Key: fact.Key, Value: fact.Value, At: fact.At})
	}
	return tags
}

// snapshotVocabulary is the tag vocabulary of a snapshot, for a caller with
// no session scan in hand (a view write). It converts each stored tag with the
// same per-source converters decorate uses, then queryTags. It is every tag
// the snapshot holds, so it may include tags of folded children and vanished
// sessions that a rail read would not bind against; the rail read stays
// authoritative. (Flow membership builds its own tag rows,
// orchestration_flow_membership.go; owner decision D-3's follow-up is where
// that meets this.)
func snapshotVocabulary(snapshot sessionTagSnapshot) sessionquery.Vocabulary {
	var all railSession
	for _, tags := range snapshot.owner {
		for _, tag := range tags {
			all.Tags = append(all.Tags, ownerSessionTag(tag))
		}
	}
	for _, tags := range snapshot.agent {
		for _, tag := range tags {
			all.Tags = append(all.Tags, agentSessionTag(tag))
		}
	}
	for _, facets := range snapshot.facets {
		for _, facet := range facets {
			if facet.Value != "" {
				all.Facts = append(all.Facts, facetSessionTag(facet))
			}
		}
	}
	return sessionquery.NewVocabulary([]sessionquery.Row{{Tags: queryTags(all)}})
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

// matchSessions applies the query, bound against the vocabulary of the same
// rows (organizedVocabulary; the caller keeps it to take the query's notes).
// The words leg searches only inside what the structured terms left, so a
// word that is rare in a view is found even when it ranks far down globally.
// The bool reports that more sessions mention the words than the limit allows.
func matchSessions(rows []organizedRow, vocabulary sessionquery.Vocabulary, request sessionQueryRequest, now time.Time, textLimit int) ([]organizedRow, bool, error) {
	bound, err := request.query.Bind(vocabulary, request.limits, now.Unix())
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

// organizedVocabulary is every tag on the request's top-level rows: what a
// query binds against and what its notes are about.
func organizedVocabulary(rows []organizedRow) sessionquery.Vocabulary {
	vocabularyRows := make([]sessionquery.Row, 0, len(rows))
	for _, row := range rows {
		vocabularyRows = append(vocabularyRows, row.query)
	}
	return sessionquery.NewVocabulary(vocabularyRows)
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

// sessionPlacement is one group a row is filed in, and what filed it there.
// Only a board read fills more than the key.
type sessionPlacement struct {
	key string
	// origin is placedByOwner or placedByRule; empty when nothing but the
	// grouping itself placed the row.
	origin string
	since  int64
	// label is the group's name where it is not the key: a rule's column as
	// the owner declared it.
	label string
	// observed is on an owner placement whose board's rules name another
	// column.
	observed string
}

const (
	placedByOwner = "owner"
	placedByRule  = "rule"
)

// sessionPlacer places a row into groups. It is the seam between a grouping
// and the three reads that must agree on it: the rail's group headings, a
// group's page, and a board column.
type sessionPlacer func(organizedRow, sessionquery.GroupBy) []sessionPlacement

// sessionGroupKeys places a row. A row with several values under a tag key is
// in each of those groups; a row with none is in the empty-named group.
func sessionGroupKeys(row organizedRow, groupBy sessionquery.GroupBy) []sessionPlacement {
	switch groupBy.Kind {
	case sessionquery.GroupRuntime:
		return []sessionPlacement{{key: row.rail.Runtime}}
	case sessionquery.GroupNone:
		return []sessionPlacement{{}}
	case sessionquery.GroupTagKey:
		return tagKeyPlacements(row, groupBy, false)
	}
	return []sessionPlacement{{key: row.query.Repository}}
}

// sessionGroupKeysOwner is the board's grouping (sessions-board plan §3.3):
// identical to sessionGroupKeys for the tag-key kind EXCEPT it considers only
// OWNER-asserted tags — a detector fact sharing the group key can never place
// a row in a column the drag would write a tag into. Two carriers, one
// column: not possible. Non-tag groupings delegate.
func sessionGroupKeysOwner(row organizedRow, groupBy sessionquery.GroupBy) []sessionPlacement {
	if groupBy.Kind != sessionquery.GroupTagKey {
		return sessionGroupKeys(row, groupBy)
	}
	return tagKeyPlacements(row, groupBy, true)
}

// tagKeyPlacements files a row under each value it carries of the grouping's
// key, once per folded value; under the unnamed group when it carries none.
// An owner's tag is the owner's placement, timed by when it was applied.
func tagKeyPlacements(row organizedRow, groupBy sessionquery.GroupBy, ownerOnly bool) []sessionPlacement {
	seen := map[string]bool{}
	var placements []sessionPlacement
	for _, tag := range row.query.Tags {
		if (ownerOnly && !tag.Owner) || strings.ToLower(tag.Key) != groupBy.Key || tag.Value == "" {
			continue
		}
		folded := sessionGroupKey(groupBy, tag.Value)
		if seen[folded] {
			continue
		}
		seen[folded] = true
		placement := sessionPlacement{key: folded}
		if ownerOnly {
			placement.origin, placement.since = placedByOwner, tag.At
		}
		placements = append(placements, placement)
	}
	if len(placements) == 0 {
		return []sessionPlacement{{}}
	}
	return placements
}

// boardRule is one placement rule bound for a request.
type boardRule struct {
	group  string // the declared column's group key
	column string // the column as the owner declared it
	bound  sessionquery.Bound
	notes  []sessionquery.Note
}

// boardPlacement is one board view's placement for one request
// (board-observed-columns plan §2): the owner's tag first, else the first
// rule that matches, else the unnamed group. placed counts, per rule, the
// rows place put in its column; it is meaningful after a read has placed
// every row once, which the rail read does.
type boardPlacement struct {
	rules  []boardRule
	placed []int
}

// boardRuleNotes are one rule's query notes, by the rule's place in the list.
type boardRuleNotes struct {
	Rule   int                 `json:"rule"`
	Column string              `json:"column"`
	Notes  []sessionquery.Note `json:"notes"`
}

// errBoardView is a board_view parameter the request cannot be served under:
// the caller's mistake, a 400.
var errBoardView = errors.New("board_view")

// errBoardTagsCutShort refuses to place by rules over part of the facts.
var errBoardTagsCutShort = errors.New("this board places sessions by rules, and the tags were read only in part: session_organization.tag_index_rows_max was reached, so some sessions would be put in the wrong column")

// requestedBoardView resolves board_view to a saved board view grouped as
// the request is. nil with no error means the request names none.
func requestedBoardView(r *http.Request, request sessionQueryRequest, limits ConsoleSessionOrganization) (*SavedSessionView, error) {
	id := r.URL.Query().Get("board_view")
	if id == "" {
		return nil, nil
	}
	for _, view := range loadSessionViews(sessionViewsDataDir(), limits).Views {
		if view.ID != id {
			continue
		}
		if view.Board == nil || view.RecordKind == sessionViewRecordKindMemory {
			return nil, fmt.Errorf("%w: the view %q is not a board of sessions", errBoardView, view.Name)
		}
		groupBy, err := sessionquery.ParseGroupBy(view.GroupBy)
		if err != nil || groupBy != request.groupBy {
			return nil, fmt.Errorf("%w: the view %q is grouped by %s, not by the request's group_by", errBoardView, view.Name, view.GroupBy)
		}
		return &view, nil
	}
	return nil, fmt.Errorf("%w: no saved view has this id", errBoardView)
}

// bindBoardPlacement binds the view's rules against the vocabulary the view's
// own query was bound against. A rule that cannot be bound (a glob past its
// limit) fails the read, naming the rule: skipping it would silently file its
// sessions under a later rule.
func bindBoardPlacement(view SavedSessionView, groupBy sessionquery.GroupBy, vocabulary sessionquery.Vocabulary, limits sessionquery.Limits, snapshot sessionTagSnapshot, now time.Time) (*boardPlacement, error) {
	rules := view.Board.Placement
	if len(rules) > 0 && !snapshot.countable() {
		return nil, errBoardTagsCutShort
	}
	// A rule names its column in any letter case; the column is shown, and
	// its group labelled, as the owner declared it.
	declared := map[string]string{}
	for _, column := range view.Board.Columns {
		column = strings.TrimSpace(column)
		declared[sessionGroupKey(groupBy, column)] = column
	}
	board := &boardPlacement{placed: make([]int, len(rules))}
	for index, rule := range rules {
		group := sessionGroupKey(groupBy, strings.TrimSpace(rule.Column))
		query, err := sessionquery.Parse(rule.Query, limits)
		if err == nil {
			var bound sessionquery.Bound
			if bound, err = query.Bind(vocabulary, limits, now.Unix()); err == nil {
				board.rules = append(board.rules, boardRule{group: group, column: declared[group],
					bound: bound, notes: query.Notes(vocabulary)})
				continue
			}
		}
		return nil, fmt.Errorf("placement rule %d (%s): %w", index+1, declared[group], err)
	}
	return board, nil
}

// place is the board's sessionPlacer.
func (b *boardPlacement) place(row organizedRow, groupBy sessionquery.GroupBy) []sessionPlacement {
	placements := sessionGroupKeysOwner(row, groupBy)
	pinned := placements[0].origin == placedByOwner
	rule := b.observe(row, groupBy, pinned)
	if !pinned {
		if rule < 0 {
			return placements
		}
		b.placed[rule]++
		matched := b.rules[rule]
		placement := sessionPlacement{key: matched.group, origin: placedByRule, since: matched.bound.InViewSince(row.query)}
		if matched.column != matched.group {
			placement.label = matched.column
		}
		return []sessionPlacement{placement}
	}
	if rule < 0 {
		return placements
	}
	for _, placement := range placements {
		if placement.key == b.rules[rule].group {
			return placements // the owner put it where the rules would
		}
	}
	for index := range placements {
		placements[index].observed = b.rules[rule].column
	}
	return placements
}

// observe is the first rule the row matches, or -1. A row the owner placed
// is judged without his tags under the board's key, so the answer is where
// the row goes when he releases it, and his own placement never feeds it.
func (b *boardPlacement) observe(row organizedRow, groupBy sessionquery.GroupBy, pinned bool) int {
	subject := row.query
	if pinned {
		subject.Tags = make([]sessionquery.Tag, 0, len(row.query.Tags))
		for _, tag := range row.query.Tags {
			if !tag.Owner || strings.ToLower(tag.Key) != groupBy.Key {
				subject.Tags = append(subject.Tags, tag)
			}
		}
	}
	for index, rule := range b.rules {
		if rule.bound.Matches(subject) {
			return index
		}
	}
	return -1
}

// ruleNotes are the notes of the rules that have any.
func (b *boardPlacement) ruleNotes() []boardRuleNotes {
	var out []boardRuleNotes
	for index, rule := range b.rules {
		if len(rule.notes) > 0 {
			out = append(out, boardRuleNotes{Rule: index, Column: rule.column, Notes: rule.notes})
		}
	}
	return out
}

// sessionGroupKey is the key a group of this name is filed under. A tag-key
// group is filed under its value folded to lower case, so that spelling never
// splits a group: a board column declared "Review" reads the group "review".
// Every other grouping's keys are exact (a repository path, a runtime name).
// It does not trim; a caller that takes a name from the owner does.
func sessionGroupKey(groupBy sessionquery.GroupBy, name string) string {
	if groupBy.Kind == sessionquery.GroupTagKey {
		return strings.ToLower(name)
	}
	return name
}

// buildSessionGroups is buildSessionRepositories over matched rows and any
// grouping. Under a tag-key grouping a session with two values is in both
// groups, so group totals may add up to more than the number of sessions; a
// view's count (countSessionViews) is always the number of sessions. Group order is the rail's: open first, then most recent, the
// unnamed group last. The placer is a seam: the default places by any tag
// carrier; the board's owner-provenance placer (sessions-board plan §3.3)
// passes sessionGroupKeysOwner so detector facts cannot render drag-writable
// columns.
func buildSessionGroups(rows []organizedRow, groupBy sessionquery.GroupBy, placer sessionPlacer) []sessionRepository {
	if placer == nil {
		placer = sessionGroupKeys
	}
	groups := map[string]*sessionRepository{}
	for _, row := range rows {
		for _, placement := range placer(row, groupBy) {
			key := placement.key
			group := groups[key]
			if group == nil {
				group = &sessionRepository{Key: key, Label: placement.label}
				if group.Label == "" {
					group.Label = sessionGroupLabel(row, groupBy, key)
				}
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
		if strings.ToLower(tag.Key) == groupBy.Key && sessionGroupKey(groupBy, tag.Value) == key && tag.Value != key {
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
	// placer places rows into groups; nil is sessionGroupKeys. A board column
	// read and a board view's rail page pass sessionGroupKeysOwner, the placer
	// the count above them used.
	placer sessionPlacer
}

// buildSessionGroupPage pages one group. A group that matched nothing answers
// with an empty page, not an error: the owner may have just untagged its last
// session while looking at it.
func buildSessionGroupPage(rows []organizedRow, children map[string][]agentChildSession, nativeChildren map[string][]nativeChildSession, presence presenceOpenSet, request sessionQueryRequest, page sessionGroupPageRequest, maxTags int) sessionGroupPageResponse {
	placer := page.placer
	if placer == nil {
		placer = sessionGroupKeys
	}
	var members []organizedRow
	for _, row := range rows {
		for _, placement := range placer(row, request.groupBy) {
			if placement.key == page.group && (page.mode != "open" || row.query.Open) {
				row.rail.PlacedBy, row.rail.InColumnSince, row.rail.ObservedGroup = placement.origin, placement.since, placement.observed
				members = append(members, row)
				break
			}
		}
	}
	sortOrganizedRows(members, request.sort)
	offset := selectedOffset(members, children, nativeChildren, page)
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
		if fold := nativeChildren[row.rail.ID]; len(fold) > 0 {
			if response.NativeChildren == nil {
				response.NativeChildren = map[string][]nativeChildSession{}
			}
			response.NativeChildren[row.rail.ID] = fold
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
// repository page does, a folded agent child or native child counting as its parent.
func selectedOffset(rows []organizedRow, children map[string][]agentChildSession, nativeChildren map[string][]nativeChildSession, page sessionGroupPageRequest) int {
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
		for _, child := range nativeChildren[row.rail.ID] {
			match = match || selected(child.SessionSummary)
		}
		if match {
			return (index / page.limit) * page.limit
		}
	}
	return page.offset
}

// viewEvaluation is what one read says about every saved view: how many
// sessions it holds, and the notes on its query. A view that cannot be counted
// truthfully has no count; a view whose query has nothing to note has no notes.
type viewEvaluation struct {
	Counts map[string]int
	Notes  map[string][]sessionquery.Note
}

// countSessionViews answers "how many sessions are in each saved view" over
// rows already decorated for this request: one scan, one decoration, one match
// per view. Only a durable view is counted; a view whose query no longer parses
// is skipped here and reported by the views owner. Notes are taken before the
// durability check, so a view with no count (status:, open:, words) still gets
// its note, and never for a memory view, which is not about these sessions.
func countSessionViews(rows []organizedRow, views []SavedSessionView, limits sessionquery.Limits, now time.Time) viewEvaluation {
	vocabulary := organizedVocabulary(rows)
	evaluation := viewEvaluation{Counts: map[string]int{}}
	for _, view := range views {
		query, err := sessionquery.Parse(view.Query, limits)
		if err != nil {
			continue
		}
		if notes := query.Notes(vocabulary); len(notes) > 0 && view.RecordKind != sessionViewRecordKindMemory {
			if evaluation.Notes == nil {
				evaluation.Notes = map[string][]sessionquery.Note{}
			}
			evaluation.Notes[view.ID] = notes
		}
		if !query.Durable() {
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
		evaluation.Counts[view.ID] = count
	}
	return evaluation
}
