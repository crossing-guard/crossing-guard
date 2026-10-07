package daemon

// Board placement rules (board-observed-columns plan): the owner's tag places
// a session first, else the first rule its facts match, else no column; the
// rail read, the column read and the rail's page agree; and a rule the reads
// could not decide alike is refused when the view is saved.

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/sessionquery"
)

// saveBoardView saves a board view through the console's own route and
// returns it as stored.
func saveBoardView(t *testing.T, f *organizationFixture, view SavedSessionView) SavedSessionView {
	t.Helper()
	token := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil)).StateToken
	document := decodeBody[sessionViewsDocument](t, f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: token, View: view}))
	return document.Views[len(document.Views)-1]
}

func stageBoard(rules ...SavedViewPlacement) SavedSessionView {
	return SavedSessionView{Name: "Stages", GroupBy: "tag-key:stage",
		Board: &SavedViewBoard{Columns: []string{"building", "Shipped", "done"}, EmptyColumns: true, Placement: rules}}
}

func boardReads(view SavedSessionView) (rail string, column func(string) string) {
	query := "&group_by=" + url.QueryEscape(view.GroupBy) + "&query=" + url.QueryEscape(view.Query) + "&board_view=" + view.ID
	return "/api/sessions?view=rail" + query, func(group string) string {
		return "/api/sessions?view=group&mode=all&group=" + url.QueryEscape(group) + query
	}
}

func TestBoardPlacesByOwnerTagThenFirstRule(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.facet(f.rows[0].ID, "fs", "edit", 1000)
	f.facet(f.rows[0].ID, "vcs", "push", 2000) // both rules match: the first in the list wins
	f.facet(f.rows[1].ID, "fs", "edit", 3000)
	f.tag("stage:done", f.rows[2])
	f.facet(f.rows[2].ID, "vcs", "push", 4000) // the owner placed it; the rules name another column
	f.tag("stage:building", f.rows[3])
	f.facet(f.rows[3].ID, "fs", "edit", 5000) // the owner placed it where the rules would
	view := saveBoardView(t, f, stageBoard(
		SavedViewPlacement{Column: "Shipped", Query: "tag:vcs=push"},
		SavedViewPlacement{Column: "building", Query: "tag:fs=edit"}))
	railURL, column := boardReads(view)

	rail := decodeBody[sessionRailResponse](t, f.do("GET", railURL, nil))
	totals, labels := map[string]int{}, map[string]string{}
	for _, group := range rail.Repositories {
		totals[group.Key], labels[group.Key] = group.Total, group.Label
	}
	if len(totals) != 4 || totals["shipped"] != 1 || totals["building"] != 2 || totals["done"] != 1 || totals[""] != 1 {
		t.Fatalf("groups = %v", totals)
	}
	if labels["shipped"] != "Shipped" {
		t.Fatalf("a group a rule fills carries the declared spelling: %q", labels["shipped"])
	}
	if len(rail.PlacementCounts) != 2 || rail.PlacementCounts[0] != 1 || rail.PlacementCounts[1] != 1 {
		t.Fatalf("each rule placed one session; the owner's two are not counted: %v", rail.PlacementCounts)
	}

	// Every heading's count is its column's total and its rows.
	rows := map[string]railSession{}
	for key, total := range totals {
		page := decodeBody[sessionGroupPageResponse](t, f.do("GET", column(key), nil))
		if page.Total != total || len(page.Sessions) != total {
			t.Fatalf("column %q: heading %d, page total %d, rows %d", key, total, page.Total, len(page.Sessions))
		}
		for _, row := range page.Sessions {
			rows[row.ID] = row
		}
	}
	if got := rows[f.rows[0].ID]; got.PlacedBy != placedByRule || got.InColumnSince != 2000 || got.ObservedGroup != "" {
		t.Fatalf("rule-placed row: %q since %d observed %q", got.PlacedBy, got.InColumnSince, got.ObservedGroup)
	}
	if got := rows[f.rows[1].ID]; got.PlacedBy != placedByRule || got.InColumnSince != 3000 {
		t.Fatalf("second rule's row: %q since %d", got.PlacedBy, got.InColumnSince)
	}
	if got := rows[f.rows[2].ID]; got.PlacedBy != placedByOwner || got.InColumnSince == 0 || got.ObservedGroup != "Shipped" {
		t.Fatalf("a row the owner placed says where the rules would put it: %q since %d observed %q", got.PlacedBy, got.InColumnSince, got.ObservedGroup)
	}
	if got := rows[f.rows[3].ID]; got.PlacedBy != placedByOwner || got.ObservedGroup != "" {
		t.Fatalf("the owner and the rules agree, so nothing is observed elsewhere: %q observed %q", got.PlacedBy, got.ObservedGroup)
	}
	if got := rows[f.rows[4].ID]; got.PlacedBy != "" || got.InColumnSince != 0 {
		t.Fatalf("a row nothing places is in the unnamed group, placed by nothing: %+v", got)
	}
	if page := decodeBody[sessionGroupPageResponse](t, f.do("GET", column("SHIPPED"), nil)); page.Total != 1 {
		t.Fatalf("a rule's column is read in any letter case: %d", page.Total)
	}
}

// A board with no rules is placed exactly as owner_groups=1 places it.
func TestBoardViewWithoutRulesReadsAsOwnerGroups(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("stage:building", f.rows[:2]...)
	f.facet(f.rows[2].ID, "stage", "building", 1000)
	view := saveBoardView(t, f, stageBoard())
	railURL, column := boardReads(view)
	owner := func(target string) string {
		return strings.Replace(target, "&board_view="+view.ID, "&owner_groups=1", 1)
	}
	if board, plain := f.do("GET", railURL, nil), f.do("GET", owner(railURL), nil); board.Code != 200 || board.Body.String() != plain.Body.String() {
		t.Fatalf("rail differs:\n%s\n%s", board.Body.String(), plain.Body.String())
	}
	for _, group := range []string{"building", ""} {
		board, plain := f.do("GET", column(group), nil), f.do("GET", owner(column(group)), nil)
		if board.Code != 200 || board.Body.String() != plain.Body.String() {
			t.Fatalf("column %q differs:\n%s\n%s", group, board.Body.String(), plain.Body.String())
		}
	}
	if page := decodeBody[sessionGroupPageResponse](t, f.do("GET", column("building"), nil)); page.Limit != defaultConsoleConfig().SessionOrganization.BoardColumnCardsMax {
		t.Fatalf("a board_view column read takes the column bound: limit %d", page.Limit)
	}
}

func TestBoardViewParameterIsRefusedWhenItCannotBeServed(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	board := saveBoardView(t, f, stageBoard())
	list := saveBoardView(t, f, SavedSessionView{Name: "List", Query: "tag:fs=edit"})
	for name, target := range map[string]string{
		"no such view":       "/api/sessions?view=rail&group_by=tag-key:stage&board_view=view_missing",
		"not a board":        "/api/sessions?view=rail&group_by=tag-key:stage&board_view=" + list.ID,
		"another grouping":   "/api/sessions?view=rail&group_by=runtime&board_view=" + board.ID,
		"another key":        "/api/sessions?view=group&mode=all&group=building&group_by=tag-key:flow&board_view=" + board.ID,
		"column, not a view": "/api/sessions?view=group&mode=all&group=building&group_by=tag-key:stage&board_view=nope",
	} {
		if rec := f.do("GET", target, nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "board_view") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// Placing by rules over tags read only in part would file sessions in the
// wrong column, so that read is refused and names the budget; a board with no
// rules needs no facts and still answers.
func TestBoardWithRulesRefusesTagsReadInPart(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.facet(f.rows[0].ID, "fs", "edit", 1000)
	ruled := saveBoardView(t, f, stageBoard(SavedViewPlacement{Column: "building", Query: "tag:fs=edit"}))
	plain := stageBoard()
	plain.Name = "Pins only"
	plain = saveBoardView(t, f, plain)
	restore := sessionTagsRead
	t.Cleanup(func() { sessionTagsRead = restore; sessionTagSnapshots.drop() })
	sessionTagsRead = func(now time.Time) sessionTagSnapshot {
		snapshot := restore(now)
		snapshot.truncated = true
		return snapshot
	}
	sessionTagSnapshots.drop()
	railURL, column := boardReads(ruled)
	for _, target := range []string{railURL, column("building")} {
		if rec := f.do("GET", target, nil); rec.Code != 503 || !strings.Contains(rec.Body.String(), "tag_index_rows_max") {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
	railURL, _ = boardReads(plain)
	if rec := f.do("GET", railURL, nil); rec.Code != 200 {
		t.Fatalf("a board without rules still answers: %d %s", rec.Code, rec.Body.String())
	}
}

// A rule that can no longer be bound fails the read and is named; skipping it
// would file its sessions under a later rule.
func TestBoardRuleThatCannotBeBoundFailsTheRead(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	view := saveBoardView(t, f, stageBoard(SavedViewPlacement{Column: "building", Query: "tag:topic=*o*"}))
	f.facet(f.rows[0].ID, "topic", "one", 1000)
	f.facet(f.rows[1].ID, "topic", "two", 1000)
	if err := os.WriteFile(filepath.Join(f.dataDir, "daemon.json"), []byte(`{"session_organization":{"glob_expansion_max":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateConsoleConfig()
	t.Cleanup(invalidateConsoleConfig)
	railURL, _ := boardReads(view)
	if rec := f.do("GET", railURL, nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "placement rule 1 (building)") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestBoardPlacementRulesAreValidatedAtSave(t *testing.T) {
	limits := sessionQueryLimitsForTest()
	budgets := sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization)
	board := func(change func(*SavedSessionView)) error {
		view := stageBoard(SavedViewPlacement{Column: "Building", Query: "tag:fs=edit touched:<7d"})
		view.ID = "b"
		change(&view)
		return validateSessionView(view, limits, budgets)
	}
	if err := board(func(*SavedSessionView) {}); err != nil {
		t.Fatalf("a durable rule naming a declared column in another letter case, on a key never used, is accepted: %v", err)
	}
	rule := func(column, query string) func(*SavedSessionView) {
		return func(view *SavedSessionView) {
			view.Board.Placement = []SavedViewPlacement{{Column: column, Query: query}}
		}
	}
	many := func(view *SavedSessionView) {
		for len(view.Board.Placement) <= budgets.rules {
			view.Board.Placement = append(view.Board.Placement, SavedViewPlacement{Column: "done", Query: "tag:vcs=push"})
		}
	}
	for name, test := range map[string]struct {
		change func(*SavedSessionView)
		says   string
	}{
		"search words":      {rule("done", "tag:vcs=push designer"), "search words"},
		"status":            {rule("done", "status:running"), "status:"},
		"open":              {rule("done", "open:yes"), "open:"},
		"empty":             {rule("done", "  "), "needs a query"},
		"unparsable":        {rule("done", "touched:7d"), "placement rule 1 (done)"},
		"undeclared column": {rule("review", "tag:vcs=push"), "does not declare"},
		"the pin key":       {rule("done", "-mine:Stage=done"), "writes moves under"},
		"too many rules":    {many, "placement rules"},
		"a column that cannot be a tag": {func(view *SavedSessionView) {
			view.Board.Columns = append(view.Board.Columns, "a=b")
		}, "cannot be a tag"},
		"a key that cannot be a tag": {func(view *SavedSessionView) { view.GroupBy = "tag-key:tag" }, "cannot be a tag"},
		"too many columns": {func(view *SavedSessionView) {
			for index := 0; len(view.Board.Columns) <= budgets.columns; index++ {
				view.Board.Columns = append(view.Board.Columns, "c"+string(rune('a'+index)))
			}
		}, "columns"},
	} {
		if err := board(test.change); err == nil || !strings.Contains(err.Error(), test.says) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Where the rules would put a row the owner placed is judged without his own
// tags under the board's key: a keyless term must not see the pin.
func TestBoardObservedColumnIgnoresTheOwnersPlacement(t *testing.T) {
	groupBy, err := sessionquery.ParseGroupBy("tag-key:stage")
	if err != nil {
		t.Fatal(err)
	}
	row := organizedRow{query: sessionquery.Row{Tags: []sessionquery.Tag{{Key: "stage", Value: "building", Owner: true, At: 7}}}}
	vocabulary := organizedVocabulary([]organizedRow{row})
	view := stageBoard(SavedViewPlacement{Column: "done", Query: "tag:building"})
	board, err := bindBoardPlacement(view, groupBy, vocabulary, sessionQueryLimitsForTest(), sessionTagSnapshot{available: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	placements := board.place(row, groupBy)
	if len(placements) != 1 || placements[0].key != "building" || placements[0].origin != placedByOwner || placements[0].since != 7 || placements[0].observed != "" {
		t.Fatalf("placements = %+v", placements)
	}
	if board.placed[0] != 0 {
		t.Fatalf("a row the owner placed is not counted for a rule: %v", board.placed)
	}
}

// A session with two owner values is in both columns, each timed by its own tag.
func TestBoardPlacementTimesEachOwnerColumn(t *testing.T) {
	groupBy, _ := sessionquery.ParseGroupBy("tag-key:stage")
	row := organizedRow{query: sessionquery.Row{Tags: []sessionquery.Tag{
		{Key: "stage", Value: "building", Owner: true, At: 10}, {Key: "Stage", Value: "Done", Owner: true, At: 20}}}}
	placements := sessionGroupKeysOwner(row, groupBy)
	if len(placements) != 2 || placements[0].since != 10 || placements[1].key != "done" || placements[1].since != 20 {
		t.Fatalf("placements = %+v", placements)
	}
}

func TestConsoleConfigBoundsTheBoardBudgets(t *testing.T) {
	defaults := defaultConsoleConfig().SessionOrganization
	if defaults.BoardColumnsMax != 12 || defaults.BoardPlacementRulesMax != 24 {
		t.Fatalf("defaults: %d columns, %d rules", defaults.BoardColumnsMax, defaults.BoardPlacementRulesMax)
	}
	for name, set := range map[string]func(*ConsoleSessionOrganization, int){
		"board_columns_max":         func(o *ConsoleSessionOrganization, v int) { o.BoardColumnsMax = v },
		"board_placement_rules_max": func(o *ConsoleSessionOrganization, v int) { o.BoardPlacementRulesMax = v },
	} {
		for _, value := range []int{0, 1000} {
			config := defaultConsoleConfig()
			set(&config.SessionOrganization, value)
			if err := config.validate(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s %d accepted: %v", name, value, err)
			}
		}
	}
}

// A rule may name its column in another letter case, with spaces around it
// in a hand-edited file: the group and the observed column are the declared
// one, spelled as declared.
func TestBoardRuleNamesItsColumnInAnyLetterCase(t *testing.T) {
	groupBy, _ := sessionquery.ParseGroupBy("tag-key:stage")
	placed := organizedRow{query: sessionquery.Row{Tags: []sessionquery.Tag{{Key: "vcs", Value: "push", At: 5}}}}
	pinned := organizedRow{query: sessionquery.Row{Tags: []sessionquery.Tag{
		{Key: "vcs", Value: "push", At: 5}, {Key: "stage", Value: "building", Owner: true, At: 9}}}}
	view := stageBoard(SavedViewPlacement{Column: " SHIPPED ", Query: "tag:vcs=push"})
	board, err := bindBoardPlacement(view, groupBy, organizedVocabulary([]organizedRow{placed, pinned}), sessionQueryLimitsForTest(), sessionTagSnapshot{available: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := board.place(placed, groupBy); len(got) != 1 || got[0].key != "shipped" || got[0].label != "Shipped" || got[0].since != 5 {
		t.Fatalf("rule placement = %+v", got)
	}
	if got := board.place(pinned, groupBy); len(got) != 1 || got[0].key != "building" || got[0].observed != "Shipped" {
		t.Fatalf("owner placement = %+v", got)
	}
}

// A rule whose key-less term names a key gets the same note a view's query
// gets, on the board's rail read and on the write that saved it.
func TestBoardRuleNotesRideTheReadAndTheWrite(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.facet(f.rows[0].ID, "vcs", "push", 1000)
	token := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil)).StateToken
	view := stageBoard(SavedViewPlacement{Column: "done", Query: "tag:vcs=push"}, SavedViewPlacement{Column: "building", Query: "tag:vcs"})
	written := decodeBody[sessionViewWriteResponse](t, f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: token, View: view}))
	if len(written.PlacementNotes) != 1 || written.PlacementNotes[0].Rule != 1 || written.PlacementNotes[0].Column != "building" || len(written.PlacementNotes[0].Notes) != 1 {
		t.Fatalf("write notes = %+v", written.PlacementNotes)
	}
	railURL, _ := boardReads(written.Views[0])
	rail := decodeBody[sessionRailResponse](t, f.do("GET", railURL, nil))
	if len(rail.PlacementNotes) != 1 || rail.PlacementNotes[0].Rule != 1 || rail.PlacementNotes[0].Notes[0].Suggest != "tag:vcs=*" {
		t.Fatalf("read notes = %+v", rail.PlacementNotes)
	}
}

// A board entry the daemon cannot use is one rejected entry; the views beside
// it still load. The board's key is checked even when no column is declared.
func TestRejectedBoardEntryLeavesOtherViewsLoaded(t *testing.T) {
	dataDir := t.TempDir()
	file := `{"format_version":1,"views":[
	 {"id":"ok","name":"Fine","query":"tag:fs=edit"},
	 {"id":"bad","name":"Bad rule","query":"","group_by":"tag-key:stage","board":{"columns":["done"],"placement":[{"column":"done","query":"open:yes"}]}},
	 {"id":"key","name":"Bad key","query":"","group_by":"tag-key:tag","board":{}}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "session-views.json"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	document := loadSessionViews(dataDir, defaultConsoleConfig().SessionOrganization)
	if len(document.Views) != 1 || document.Views[0].ID != "ok" || len(document.Rejected) != 2 {
		t.Fatalf("views %+v rejected %+v", document.Views, document.Rejected)
	}
	if !strings.Contains(document.Rejected[0].Problem, "placement rule 1 (done)") || !strings.Contains(document.Rejected[1].Problem, "cannot be a tag key") {
		t.Fatalf("rejections = %+v", document.Rejected)
	}
	// A budget lowered below what a saved board uses rejects that entry alone.
	small := defaultConsoleConfig().SessionOrganization
	small.BoardPlacementRulesMax = 1
	two := `{"format_version":1,"views":[{"id":"b","name":"Two rules","query":"","group_by":"tag-key:stage","board":{"columns":["done"],
	 "placement":[{"column":"done","query":"tag:vcs=push"},{"column":"done","query":"tag:vcs=commit"}]}}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "session-views.json"), []byte(two), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadSessionViews(dataDir, small); len(got.Views) != 0 || len(got.Rejected) != 1 || !strings.Contains(got.Rejected[0].Problem, "at most 1 placement rules") {
		t.Fatalf("lowered budget: views %+v rejected %+v", got.Views, got.Rejected)
	}
}

func TestBoardViewParameterRefusesAMemoryView(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	memory := stageBoard()
	memory.RecordKind = sessionViewRecordKindMemory
	memory = saveBoardView(t, f, memory)
	railURL, _ := boardReads(memory)
	if rec := f.do("GET", railURL, nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "board_view") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// calls: and lines: are answered from the row's own numbers over HTTP: in a
// filter, in a saved view's count, and in a placement rule. A session kept
// without its transcript has neither number, so a floor leaves it out
// (board-in-flight plan §2.1).
func TestSizeTermsFilterCountAndPlace(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	for index, calls := range []int{40, 2, 12, 0, 1} {
		f.rows[index].Turns, f.rows[index].Lines = calls, calls*10
	}
	rescan := func() {
		sessionScanCoalescer = &scanCoalescer{result: f.rows, done: time.Now().Add(time.Hour)}
		sessionTagSnapshots.drop()
	}
	rescan()
	total := func(query string) int {
		t.Helper()
		page := decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=&group_by=none&query="+url.QueryEscape(query), nil))
		return page.Total
	}
	if got := total("calls:>5"); got != 2 {
		t.Fatalf("calls:>5 = %d sessions, want 2", got)
	}
	if got := total("lines:<20 calls:>0"); got != 1 {
		t.Fatalf("lines:<20 calls:>0 = %d, want 1", got)
	}
	if rec := f.do("GET", "/api/sessions?view=rail&group_by=none&query="+url.QueryEscape("calls:5"), nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "write a count like calls:>5") {
		t.Fatalf("a malformed count is refused with its form: %d %s", rec.Code, rec.Body.String())
	}

	view := stageBoard(SavedViewPlacement{Column: "building", Query: "calls:>5"})
	view.Query = "calls:>1"
	view = saveBoardView(t, f, view)
	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&counts=1", nil))
	if rail.ViewCounts[view.ID] != 3 {
		t.Fatalf("a view with a size term is counted: %+v", rail.ViewCounts)
	}
	railURL, _ := boardReads(view)
	board := decodeBody[sessionRailResponse](t, f.do("GET", railURL, nil))
	if len(board.PlacementCounts) != 1 || board.PlacementCounts[0] != 2 {
		t.Fatalf("a rule with a size term places: %v", board.PlacementCounts)
	}

	// The long session is tagged, then its transcript is no longer found.
	f.tag("follow-up", f.rows[0])
	f.rows = f.rows[1:]
	rescan()
	if kept := total("mine:follow-up"); kept != 1 {
		t.Fatalf("the tagged session outlives its transcript: %d", kept)
	}
	if kept := total("mine:follow-up calls:>0"); kept != 0 {
		t.Fatalf("with no transcript there is no call count, so a floor leaves it out: %d", kept)
	}
	if kept := total("mine:follow-up lines:>0"); kept != 0 {
		t.Fatalf("nor a line count: %d", kept)
	}
}

// A flow sees a member's tags and facts, not its size: a stage query with a
// size term would read zero for every member, so the flow is refused.
func TestFlowRefusesSizeTerms(t *testing.T) {
	flow := SavedFlow{ID: "pilot", Name: "Pilot", MembershipTags: []string{"flow:building"},
		Stages: []FlowStage{{ID: "building", Name: "Building", Membership: "tag:phase=plan"}}}
	if err := validateSavedFlow(flow); err != nil {
		t.Fatal(err)
	}
	flow.Stages[0].Membership = "tag:phase=plan calls:<5"
	if err := validateSavedFlow(flow); err == nil || !strings.Contains(err.Error(), `stage "Building" membership`) || !strings.Contains(err.Error(), "calls: or lines:") {
		t.Fatalf("membership with a size term: %v", err)
	}
	flow.Stages[0].Membership = "tag:phase=plan"
	flow.Stages = append(flow.Stages, FlowStage{ID: "review", Name: "Review", Membership: "tag:phase=red-team"})
	flow.Stages[0].Transitions = []FlowStageTransit{{Kind: "session.turn-ended", When: "lines:>50", To: "review"}}
	if err := validateSavedFlow(flow); err == nil || !strings.Contains(err.Error(), "transition when") || !strings.Contains(err.Error(), "calls: or lines:") {
		t.Fatalf("a transition with a size term: %v", err)
	}
}
