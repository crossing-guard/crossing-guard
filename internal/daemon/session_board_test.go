package daemon

// Board tests (sessions-board plan rev 3): the board config's validation
// rules, and the Owner-provenance grouping that keeps detector facts from
// rendering drag-writable columns.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/sessionquery"
)

func TestBoardViewRequiresTagKeyGrouping(t *testing.T) {
	view := SavedSessionView{ID: "b1", Name: "Board", Query: "",
		GroupBy: "runtime", Board: &SavedViewBoard{Columns: []string{"building"}}}
	err := validateSessionView(view, sessionQueryLimitsForTest(), sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization))
	if err == nil || !strings.Contains(err.Error(), "tag-key") {
		t.Fatalf("board with non-tag-key grouping accepted: %v", err)
	}
	view.GroupBy = "tag-key:flow"
	if err := validateSessionView(view, sessionQueryLimitsForTest(), sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization)); err != nil {
		t.Fatalf("board with tag-key grouping rejected: %v", err)
	}
}

func TestBoardViewColumnValidation(t *testing.T) {
	view := SavedSessionView{ID: "b2", Name: "Board", GroupBy: "tag-key:flow",
		Board: &SavedViewBoard{Columns: []string{"building", "building"}}}
	if err := validateSessionView(view, sessionQueryLimitsForTest(), sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization)); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate column accepted: %v", err)
	}
	view.Board.Columns = nil
	if err := validateSessionView(view, sessionQueryLimitsForTest(), sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization)); err != nil {
		t.Fatalf("a board without declared columns is legal (observed order): %v", err)
	}
}

func TestOwnerProvenanceGroupingExcludesFacts(t *testing.T) {
	groupBy, err := sessionquery.ParseGroupBy("tag-key:flow")
	if err != nil {
		t.Fatal(err)
	}
	row := organizedRow{query: sessionquery.Row{Tags: []sessionquery.Tag{
		{Key: "flow", Value: "building", Owner: true},
		// A detector fact with the SAME key/value pair: must not place.
		{Key: "flow", Value: "review", Owner: false},
	}}}
	keys := placementKeys(sessionGroupKeysOwner(row, groupBy))
	if len(keys) != 1 || keys[0] != "building" {
		t.Fatalf("owner grouping = %v, want [building]", keys)
	}
	// The unfiltered placer still places both (rail compatibility unchanged).
	all := sessionGroupKeys(row, groupBy)
	if len(all) != 2 {
		t.Fatalf("plain grouping changed: %v", all)
	}
}

func TestOwnerProvenanceGroupingIsEmptyWithoutOwnerTags(t *testing.T) {
	groupBy, err := sessionquery.ParseGroupBy("tag-key:work")
	if err != nil {
		t.Fatal(err)
	}
	row := organizedRow{query: sessionquery.Row{Tags: []sessionquery.Tag{
		// Only the detector's WIP fact: no owner tag under this key.
		{Key: "work", Value: "uncommitted", Owner: false},
	}}}
	keys := placementKeys(sessionGroupKeysOwner(row, groupBy))
	if len(keys) != 1 || keys[0] != "" {
		t.Fatalf("facts alone must not place: %v", keys)
	}
}

// The board incident (board-view-query-correctness plan, 2026-09-29): a board
// view saved with tag:flow over sessions tagged flow=building rendered empty,
// because a keyless term is a value term. The grammar is unchanged; every read
// and write that carries the query now carries its note.
func flowBoardFixture(t *testing.T) *organizationFixture {
	t.Helper()
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("flow:building", f.rows[0])
	f.tag("flow:review", f.rows[1])
	return f
}

func putView(f *organizationFixture, view SavedSessionView) sessionViewWriteResponse {
	f.t.Helper()
	token := decodeBody[sessionViewsDocument](f.t, f.do("GET", "/api/session-views", nil)).StateToken
	if view.ID == "" {
		return decodeBody[sessionViewWriteResponse](f.t, f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: token, View: view}))
	}
	return decodeBody[sessionViewWriteResponse](f.t, f.do("PUT", "/api/session-views/"+view.ID, sessionViewWriteRequest{StateToken: token, View: view}))
}

func onlySuggestion(t *testing.T, notes []sessionquery.Note, want string) {
	t.Helper()
	if len(notes) != 1 || notes[0].Suggest != want || !strings.Contains(notes[0].Problem, want) {
		t.Fatalf("notes = %+v, want one suggesting %s", notes, want)
	}
}

func TestOrganizedRailNotesAKeylessTermThatNamesAKey(t *testing.T) {
	f := flowBoardFixture(t)
	read := func(query string) sessionRailResponse {
		return decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&owner_groups=1&group_by=tag-key:flow&query="+url.QueryEscape(query), nil))
	}
	empty := read("tag:flow")
	if len(empty.Repositories) != 0 {
		t.Fatalf("tag:flow still selects the value flow, of which there is none: %+v", empty.Repositories)
	}
	onlySuggestion(t, empty.QueryNotes, "tag:flow=*")
	if fixed := read("tag:flow=*"); len(fixed.QueryNotes) != 0 || len(fixed.Repositories) != 2 {
		t.Fatalf("tag:flow=* is the key term: notes %+v groups %+v", fixed.QueryNotes, fixed.Repositories)
	}
	// Once a value named flow exists the term selects it, and there is nothing to say.
	f.tag("stage:flow", f.rows[2])
	if valued := read("tag:flow"); len(valued.QueryNotes) != 0 {
		t.Fatalf("a value flow exists; got notes %+v", valued.QueryNotes)
	}
}

// The view list's own read is the plain rail with counts=1 (no query, no
// group_by): that is the request that must carry view_notes (RT-1).
func TestViewListCountReadCarriesViewNotes(t *testing.T) {
	f := flowBoardFixture(t)
	incident := putView(f, SavedSessionView{Name: "Flow: opted-in", Query: "tag:flow", GroupBy: "tag-key:flow",
		Board: &SavedViewBoard{Columns: []string{"building", "review"}}})
	id := incident.Views[0].ID
	live := putView(f, SavedSessionView{Name: "live", Query: "tag:flow status:running"}).Views[1].ID
	memory := putView(f, SavedSessionView{Name: "memory", Query: "tag:flow", RecordKind: sessionViewRecordKindMemory}).Views[2].ID

	// Without counts=1 the plain rail is byte-for-byte the pre-notes shape:
	// decode into that shape, re-encode as writeJSON does, compare the bytes.
	plain := f.do("GET", "/api/sessions?view=rail&repository_limit=1", nil)
	var before preNotesRailResponse
	if err := json.Unmarshal(plain.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := json.NewEncoder(&again).Encode(before); err != nil {
		t.Fatal(err)
	}
	if again.String() != plain.Body.String() {
		t.Fatalf("the plain rail without counts=1 must stay as it was:\n got: %s\nwant: %s", plain.Body.String(), again.String())
	}
	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&repository_limit=1&counts=1", nil))
	if count, counted := rail.ViewCounts[id]; !counted || count != 0 {
		t.Fatalf("the incident view counts 0: %+v", rail.ViewCounts)
	}
	onlySuggestion(t, rail.ViewNotes[id], "tag:flow=*")
	if _, counted := rail.ViewCounts[live]; counted {
		t.Fatalf("a live view has no count: %+v", rail.ViewCounts)
	}
	onlySuggestion(t, rail.ViewNotes[live], "tag:flow=*") // RT-5: no count, still a note
	if notes, noted := rail.ViewNotes[memory]; noted {
		t.Fatalf("a memory view is not about these sessions: %+v", notes)
	}
	// The organized rail's counts=1 carries the same notes.
	organized := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&counts=1&query=tag:flow=*", nil))
	onlySuggestion(t, organized.ViewNotes[id], "tag:flow=*")
	if len(organized.QueryNotes) != 0 {
		t.Fatalf("the bar query itself is fine: %+v", organized.QueryNotes)
	}
}

func TestNotesAreWithheldWhenTheTagReadIsCutShort(t *testing.T) {
	f := flowBoardFixture(t)
	putView(f, SavedSessionView{Name: "Flow", Query: "tag:flow"})
	restore := sessionTagsRead
	t.Cleanup(func() { sessionTagsRead = restore; sessionTagSnapshots.drop() })
	sessionTagsRead = func(now time.Time) sessionTagSnapshot {
		snapshot := restore(now)
		snapshot.truncated = true
		return snapshot
	}
	sessionTagSnapshots.drop()
	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&repository_limit=1&counts=1", nil))
	if rail.ViewCounts != nil || rail.ViewNotes != nil {
		t.Fatalf("a cut-short read gives neither counts nor notes: %+v %+v", rail.ViewCounts, rail.ViewNotes)
	}
	organized := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&counts=1&query=tag:flow", nil))
	if organized.QueryNotes != nil || organized.ViewNotes != nil {
		t.Fatalf("a cut-short read gives no notes: %+v %+v", organized.QueryNotes, organized.ViewNotes)
	}
	written := putView(f, SavedSessionView{Name: "Flow again", Query: "tag:flow"})
	if written.ViewNotes != nil {
		t.Fatalf("a cut-short snapshot gives no write-time note: %+v", written.ViewNotes)
	}
}

// The incident's own path: an agent PUT the view by hand. The write succeeds
// (a note never rejects, D-1) and its answer says what is wrong (RT-3).
func TestViewWriteAnswersWithTheWrittenViewsNotes(t *testing.T) {
	f := flowBoardFixture(t)
	board := &SavedViewBoard{Columns: []string{"building", "review"}}
	created := putView(f, SavedSessionView{Name: "Flow: opted-in", Query: "tag:flow=*", GroupBy: "tag-key:flow", Board: board})
	id := created.Views[0].ID
	if created.ViewNotes != nil {
		t.Fatalf("tag:flow=* has nothing to note: %+v", created.ViewNotes)
	}
	broken := putView(f, SavedSessionView{ID: id, Name: "Flow: opted-in", Query: "tag:flow", GroupBy: "tag-key:flow", Board: board})
	if len(broken.Views) != 1 || broken.Views[0].Query != "tag:flow" || broken.StateToken == "" {
		t.Fatalf("the write is saved and answered with the document: %+v", broken.sessionViewsDocument)
	}
	onlySuggestion(t, broken.ViewNotes[id], "tag:flow=*")
	if len(broken.ViewNotes) != 1 {
		t.Fatalf("only the written view is noted: %+v", broken.ViewNotes)
	}
	token := broken.StateToken
	if rec := f.do("PUT", "/api/session-views/"+id, sessionViewWriteRequest{StateToken: token,
		View: SavedSessionView{Name: "x", Query: "bogus:flow"}}); rec.Code != 400 || strings.Contains(rec.Body.String(), "view_notes") {
		t.Fatalf("a rejected write is unchanged: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do("PUT", "/api/session-views/"+id, sessionViewWriteRequest{StateToken: "stale", View: broken.Views[0]}); rec.Code != 409 || strings.Contains(rec.Body.String(), "view_notes") {
		t.Fatalf("a stale write is still a plain conflict: %d %s", rec.Code, rec.Body.String())
	}
	memory := putView(f, SavedSessionView{Name: "memory", Query: "tag:flow", RecordKind: sessionViewRecordKindMemory})
	if memory.ViewNotes != nil {
		t.Fatalf("a memory view's write is not noted against sessions: %+v", memory.ViewNotes)
	}
	token = memory.StateToken
	reordered := f.do("PUT", "/api/session-views/order", sessionViewOrderRequest{StateToken: token, Order: []string{memory.Views[1].ID, id}})
	if reordered.Code != 200 || strings.Contains(reordered.Body.String(), "view_notes") {
		t.Fatalf("a reorder answers with the document alone: %d %s", reordered.Code, reordered.Body.String())
	}
}

// preNotesRailResponse is sessionRailResponse as it was before query and view
// notes existed, field for field.
type preNotesRailResponse struct {
	Activity        json.RawMessage   `json:"activity"`
	RepositoryTotal int               `json:"repository_total"`
	Repositories    []json.RawMessage `json:"repositories"`
	RuntimeHealth   []json.RawMessage `json:"runtime_health,omitempty"`
	GroupBy         string            `json:"group_by,omitempty"`
	ViewCounts      map[string]int    `json:"view_counts,omitempty"`
	MoreTextMatches bool              `json:"more_text_matches,omitempty"`
}

// PUT replaces the stored view: a field the writer leaves out is deleted. The
// console therefore sends back the whole view it holds (viewCommit); this pins
// the contract that choice rests on (settings-views-full-view-save plan §2) so
// it is not quietly turned into a merge.
func TestBoardViewPutReplacesTheWholeView(t *testing.T) {
	f := flowBoardFixture(t)
	board := &SavedViewBoard{Columns: []string{"building", "review"}, EmptyColumns: true}
	created := putView(f, SavedSessionView{Name: "Flow: opted-in", Query: "tag:flow=*", GroupBy: "tag-key:flow", Board: board})
	id := created.Views[0].ID
	kept := putView(f, SavedSessionView{ID: id, Name: "Flow: opted-in", Query: "tag:flow=*", GroupBy: "tag-key:flow", Sort: "oldest", Board: board})
	if got := kept.Views[0]; got.Board == nil || len(got.Board.Columns) != 2 || !got.Board.EmptyColumns || got.Sort != "oldest" {
		t.Fatalf("a PUT carrying the board keeps it: %+v", got)
	}
	dropped := putView(f, SavedSessionView{ID: id, Name: "Flow: opted-in", Query: "tag:flow=*", GroupBy: "tag-key:flow", Sort: "oldest"})
	if got := dropped.Views[0]; got.Board != nil {
		t.Fatalf("a PUT without the board replaces the view and drops it: %+v", got.Board)
	}
}

// A board column's cards are placed by the rule its header count used
// (board-column-overflow plan §2.2): a session only a detector fact puts
// under the key is no card. The rail's page keeps any-provenance placement.
func TestBoardColumnPagePlacesByOwnerTags(t *testing.T) {
	groupBy, err := sessionquery.ParseGroupBy("tag-key:flow")
	if err != nil {
		t.Fatal(err)
	}
	row := func(id string, owner bool) organizedRow {
		r := organizedRow{query: sessionquery.Row{Tags: []sessionquery.Tag{{Key: "flow", Value: "building", Owner: owner}}}}
		r.rail.ID = id
		return r
	}
	rows := []organizedRow{row("owned", true), row("observed", false)}
	request := sessionQueryRequest{groupBy: groupBy}
	page := sessionGroupPageRequest{group: "building", mode: "all", limit: 10}
	if got := titlesOfIDs(buildSessionGroupPage(rows, nil, nil, presenceOpenSet{}, request, page, 6)); got != "observed,owned" && got != "owned,observed" {
		t.Fatalf("the rail page places by any provenance: %s", got)
	}
	page.placer = sessionGroupKeysOwner
	column := buildSessionGroupPage(rows, nil, nil, presenceOpenSet{}, request, page, 6)
	if got := titlesOfIDs(column); got != "owned" || column.Total != 1 {
		t.Fatalf("the board column must place by owner tags only: %s total %d", got, column.Total)
	}
}

func placementKeys(placements []sessionPlacement) []string {
	keys := make([]string, 0, len(placements))
	for _, placement := range placements {
		keys = append(keys, placement.key)
	}
	return keys
}

func titlesOfIDs(page sessionGroupPageResponse) string {
	ids := make([]string, 0, len(page.Sessions))
	for _, s := range page.Sessions {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ",")
}

// owner_groups=1 on the group page places rows by owner tags. With no limit
// it is the board's column read and takes daemon.json's board_column_cards_max
// (board-column-overflow plan §2.2); a named limit — a board view's rail page —
// is answered up to the larger of that bound and the ordinary page ceiling
// (board-rail-owner-paging plan §2.1). Without it the page is unchanged.
func TestBoardColumnPageRidesTheConfiguredBound(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("flow:building", f.rows[:4]...)
	f.facet(f.rows[4].ID, "flow", "building", 1000) // placed by a detector fact only
	if err := os.WriteFile(filepath.Join(f.dataDir, "daemon.json"), []byte(`{"session_organization":{"board_column_cards_max":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateConsoleConfig()
	t.Cleanup(invalidateConsoleConfig)
	column := "/api/sessions?view=group&mode=all&group=building&group_by=tag-key:flow&query=" + url.QueryEscape("tag:flow=*")
	board := decodeBody[sessionGroupPageResponse](t, f.do("GET", column+"&owner_groups=1", nil))
	if len(board.Sessions) != 3 || board.Total != 4 || board.Limit != 3 {
		t.Fatalf("the column read takes the configured bound and counts owner-placed sessions only: %d rows, total %d, limit %d",
			len(board.Sessions), board.Total, board.Limit)
	}
	if page := decodeBody[sessionGroupPageResponse](t, f.do("GET", column+"&owner_groups=1&limit=2", nil)); len(page.Sessions) != 2 {
		t.Fatalf("a smaller limit is honoured: %d rows", len(page.Sessions))
	}
	if page := decodeBody[sessionGroupPageResponse](t, f.do("GET", column+"&owner_groups=1&limit=4", nil)); len(page.Sessions) != 4 || page.Total != 4 {
		t.Fatalf("a named limit past a small column bound is answered, by owner placement: %d rows, total %d", len(page.Sessions), page.Total)
	}
	if rec := f.do("GET", column+"&owner_groups=1&limit=50", nil); rec.Code != 200 {
		t.Fatalf("the ordinary page ceiling is accepted under a small column bound: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do("GET", column+"&owner_groups=1&limit=51", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "limit is outside") {
		t.Fatalf("a limit past both the column bound and the page ceiling is refused: %d %s", rec.Code, rec.Body.String())
	}
	if rail := decodeBody[sessionGroupPageResponse](t, f.do("GET", column, nil)); len(rail.Sessions) != 5 || rail.Limit != defaultSessionPageSize {
		t.Fatalf("the rail page keeps its own default and any-provenance placement: %d rows, limit %d", len(rail.Sessions), rail.Limit)
	}
	if rec := f.do("GET", column+"&limit=51", nil); rec.Code != 400 {
		t.Fatalf("the rail page keeps its own ceiling: %d", rec.Code)
	}
	byRepository := "/api/sessions?view=group&mode=all&group=/work/app&group_by=repository&owner_groups=1"
	if page := decodeBody[sessionGroupPageResponse](t, f.do("GET", byRepository, nil)); page.Limit != defaultSessionPageSize {
		t.Fatalf("owner_groups=1 outside a tag-key grouping is ignored, as on the rail: limit %d", page.Limit)
	}
}

// At the default column bound the bound is the ceiling: a named limit may
// reach it and no further (board-rail-owner-paging plan, invariant 4).
func TestBoardColumnPageCeilingIsTheLargerBound(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("flow:building", f.rows[:2]...)
	column := "/api/sessions?view=group&mode=all&group=building&group_by=tag-key:flow&owner_groups=1&query=" + url.QueryEscape("tag:flow=*")
	config, _, _, err := loadConsoleConfig(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	bound := config.SessionOrganization.BoardColumnCardsMax
	if bound <= maxSessionPageSize {
		t.Fatalf("the default column bound %d no longer exceeds the page ceiling; this test needs a new premise", bound)
	}
	if rec := f.do("GET", fmt.Sprintf("%s&limit=%d", column, bound), nil); rec.Code != 200 {
		t.Fatalf("a limit at the column bound is accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do("GET", fmt.Sprintf("%s&limit=%d", column, bound+1), nil); rec.Code != 400 {
		t.Fatalf("a limit past the column bound is refused: %d", rec.Code)
	}
}

// A board view's rail counts its groups by owner tags, and each group's page
// read with owner_groups=1 lists exactly the sessions that count named — the
// group with no value included. A session only a detector fact places is in
// that group; without owner_groups its page leaves the session out, and when
// the fact names a value no owner tag uses, no page lists it at all
// (board-rail-owner-paging plan §1, invariant 2).
func TestBoardRailGroupsAndTheirPagesAgree(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("flow:building", f.rows[:2]...)
	f.facet(f.rows[2].ID, "flow", "building", 1000) // a fact naming an owner group
	f.facet(f.rows[3].ID, "flow", "review", 1000)   // a fact naming a value no owner tag uses
	query := "&group_by=tag-key:flow&query=" + url.QueryEscape("tag:flow=*")
	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&owner_groups=1"+query, nil))
	if len(rail.Repositories) != 2 {
		t.Fatalf("owner placement gives the owner's group and the group with no value: %+v", rail.Repositories)
	}
	keys := map[string]bool{}
	for _, group := range rail.Repositories {
		keys[group.Key] = true
	}
	if !keys["building"] || !keys[""] {
		t.Fatalf("the groups are building and the one with no value; review, which only a fact names, is no group: %+v", rail.Repositories)
	}
	// limit=15 is the rail's own page size.
	page := func(key, owner string) sessionGroupPageResponse {
		return decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&limit=15&group="+url.QueryEscape(key)+owner+query, nil))
	}
	for _, group := range rail.Repositories {
		if got := page(group.Key, "&owner_groups=1"); got.Total != group.Total || len(got.Sessions) != group.Total {
			t.Fatalf("group %q: the heading counts %d, its page holds %d of total %d", group.Key, group.Total, len(got.Sessions), got.Total)
		}
	}
	if ids := titlesOfIDs(page("", "&owner_groups=1")); ids != f.rows[2].ID+","+f.rows[3].ID && ids != f.rows[3].ID+","+f.rows[2].ID {
		t.Fatalf("both fact-only sessions are in the group with no value: %s", ids)
	}
	if got := page("", ""); got.Total != 0 {
		t.Fatalf("without owner_groups the same page leaves the fact-only sessions out: total %d", got.Total)
	}
	if got := page("building", ""); got.Total != 3 {
		t.Fatalf("without owner_groups a fact places a session beside the owner's: total %d", got.Total)
	}
}
