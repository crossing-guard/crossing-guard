package daemon

// Letter-case tests for the board (board-column-letter-case plan §6): a tag
// group has one key, its folded value, and a column or a page read named in
// any letter case reaches it.

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"crossing-guard/internal/sessionquery"
)

// The tag-key pairs are pinned in the browser too (organization.test.mjs,
// foldGroupKey): the two folds must agree on them.
func TestSessionGroupKeyFoldsOnlyTagGroups(t *testing.T) {
	tagKey, err := sessionquery.ParseGroupBy("tag-key:Flow")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"Review": "review", "İstanbul": "istanbul", "ΟΔΟΣ": "οδοσ", "ΑΣ Β": "ασ β", "": ""} {
		if got := sessionGroupKey(tagKey, name); got != want {
			t.Errorf("tag-key group %q is filed under %q, want %q", name, got, want)
		}
		if name == "" {
			continue
		}
		// A row tagged in that spelling is placed under the same key.
		row := organizedRow{query: sessionquery.Row{Tags: []sessionquery.Tag{{Key: "flow", Value: name, Owner: true}}}}
		for placer, keys := range map[string][]string{"any": placementKeys(sessionGroupKeys(row, tagKey)), "owner": placementKeys(sessionGroupKeysOwner(row, tagKey))} {
			if len(keys) != 1 || keys[0] != want {
				t.Errorf("%s placer files %q under %v, want [%s]", placer, name, keys, want)
			}
		}
	}
	for kind, name := range map[string]string{sessionquery.GroupRepository: "/Work/APP", sessionquery.GroupRuntime: "Claude", sessionquery.GroupNone: "X"} {
		if got := sessionGroupKey(sessionquery.GroupBy{Kind: kind}, name); got != name {
			t.Errorf("%s group %q became %q: only tag groups fold", kind, name, got)
		}
	}
}

func TestGroupPageReadsATagGroupInAnyLetterCase(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("flow:review", f.rows[0], f.rows[2])
	f.tag("flow:Building", f.rows[1]) // stored as first spelled
	page := func(params string) sessionGroupPageResponse {
		t.Helper()
		rec := f.do("GET", "/api/sessions?view=group&mode=all&"+params, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", params, rec.Code, rec.Body.String())
		}
		return decodeBody[sessionGroupPageResponse](t, rec)
	}
	byFlow := "&group_by=" + url.QueryEscape("tag-key:flow")
	// owner_groups=1 is ignored by the page read on this base; it guards the
	// owner placer once the board's column read takes it (column overflow plan).
	for _, owner := range []string{"", "&owner_groups=1"} {
		for _, name := range []string{"Review", "review", "REVIEW"} {
			got := page("group=" + name + byFlow + owner)
			if got.Total != 2 || got.Group != "review" {
				t.Fatalf("group=%s%s: total %d, group %q; want the two review sessions under the key \"review\"", name, owner, got.Total, got.Group)
			}
		}
		if got := page("group=building" + byFlow + owner); got.Total != 1 || got.Sessions[0].ID != f.rows[1].ID {
			t.Fatalf("group=building%s: %+v, want the session tagged flow=Building", owner, got.Sessions)
		}
	}
	if got := page("group=" + byFlow); got.Total != 2 {
		t.Fatalf("the unnamed group holds %d sessions, want the two untagged ones", got.Total)
	}
	// The page that holds a selected session is found whatever case names the group.
	selected := page("group=Review&limit=1&selected_runtime=" + f.rows[2].Runtime + "&selected_id=" + url.QueryEscape(f.rows[2].ID) + byFlow)
	if len(selected.Sessions) != 1 || selected.Sessions[0].ID != f.rows[2].ID {
		t.Fatalf("selected page = %+v, want the page holding %s", selected.Sessions, f.rows[2].ID)
	}
	// Only tag groups fold: a repository is a path.
	if got := page("group=/Work/APP&group_by=repository"); got.Total != 0 || got.Group != "/Work/APP" {
		t.Fatalf("repository group in another case: total %d, group %q; want nothing, name kept", got.Total, got.Group)
	}
	if got := page("group=/work/app&group_by=repository"); got.Total != 3 {
		t.Fatalf("repository group = %d, want 3", got.Total)
	}
}

func TestBoardColumnsThatDifferOnlyByLetterCaseAreRefused(t *testing.T) {
	view := SavedSessionView{ID: "b3", Name: "Board", GroupBy: "tag-key:flow"}
	for _, columns := range [][]string{{"Review", "review"}, {"Review", " review "}, {"ΟΔΟΣ", "οδοσ"}} {
		view.Board = &SavedViewBoard{Columns: columns}
		err := validateSessionView(view, sessionQueryLimitsForTest(), sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization))
		if err == nil || !strings.Contains(err.Error(), "letter case") ||
			!strings.Contains(err.Error(), `"`+columns[0]+`"`) || !strings.Contains(err.Error(), `"`+strings.TrimSpace(columns[1])+`"`) {
			t.Fatalf("columns %q: %v, want a refusal naming both spellings", columns, err)
		}
	}
	view.Board = &SavedViewBoard{Columns: []string{"Review", "Done"}}
	if err := validateSessionView(view, sessionQueryLimitsForTest(), sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization)); err != nil {
		t.Fatalf("a mixed-case column is legal: %v", err)
	}
}

func TestBoardColumnsAreStoredTrimmed(t *testing.T) {
	f := flowBoardFixture(t)
	written := putView(f, SavedSessionView{Name: "Flow", Query: "tag:flow=*", GroupBy: "tag-key:flow",
		Board: &SavedViewBoard{Columns: []string{" Review ", "Done"}}})
	if len(written.Views) != 1 || written.Views[0].Board == nil || strings.Join(written.Views[0].Board.Columns, "|") != "Review|Done" {
		t.Fatalf("stored view = %+v, want columns Review|Done", written.Views)
	}
}

// A hand-written file holding the twins is rejected as an entry, by name, and
// blocks saves until it is fixed — as any unreadable entry does (plan D-1).
func TestViewsFileWithCaseTwinColumnsIsRejectedAndRecovers(t *testing.T) {
	f := flowBoardFixture(t)
	created := putView(f, SavedSessionView{Name: "Flow", Query: "tag:flow=*", GroupBy: "tag-key:flow",
		Board: &SavedViewBoard{Columns: []string{"Review", "Done"}}})
	path := sessionViewsPath(f.dataDir)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	twins := strings.Replace(string(good), `"Done"`, `"review"`, 1)
	if twins == string(good) {
		t.Fatalf("fixture file has no Done column: %s", good)
	}
	if err := os.WriteFile(path, []byte(twins), 0o600); err != nil {
		t.Fatal(err)
	}
	read := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil))
	if len(read.Views) != 0 || len(read.Rejected) != 1 || read.Rejected[0].Name != "Flow" ||
		!strings.Contains(read.Rejected[0].Problem, `"Review" and "review"`) {
		t.Fatalf("read = views %+v rejected %+v, want the Flow entry rejected naming both spellings", read.Views, read.Rejected)
	}
	other := sessionViewWriteRequest{StateToken: read.StateToken, View: SavedSessionView{Name: "Other", Query: "tag:flow=*"}}
	if rec := f.do("POST", "/api/session-views", other); rec.Code != 409 {
		t.Fatalf("a save beside a rejected entry = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := putView(f, SavedSessionView{Name: "Other", Query: "tag:flow=*"})
	if len(fixed.Views) != 2 || len(fixed.Rejected) != 0 || fixed.Views[0].ID != created.Views[0].ID {
		t.Fatalf("after the fix = views %+v rejected %+v, want both views and no rejection", fixed.Views, fixed.Rejected)
	}
}
