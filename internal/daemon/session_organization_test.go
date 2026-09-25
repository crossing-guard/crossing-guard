package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// organizationFixture stands up a real store behind the governor and a fixed
// session scan, which is everything the tag and view routes touch.
type organizationFixture struct {
	t       *testing.T
	ix      *store.Index
	rows    []SessionSummary
	dataDir string
}

func newOrganizationFixture(t *testing.T, rows ...SessionSummary) *organizationFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dataDir := t.TempDir()
	priorIndexPath := resolvedIndexPath
	setIndexPath(dataDir)
	ix, err := store.Open(filepath.Join(dataDir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	prior := governor
	governor = NewGovernor(ix, nil)
	fixture := &organizationFixture{t: t, ix: ix, rows: rows, dataDir: dataDir}
	sessionScanCoalescer = &scanCoalescer{result: rows, done: time.Now().Add(time.Hour)}
	sessionTagSnapshots.drop()
	t.Cleanup(func() {
		governor = prior
		resolvedIndexPath = priorIndexPath
		_ = ix.Close()
		sessionScanCoalescer = &scanCoalescer{}
		sessionTagSnapshots.drop()
	})
	return fixture
}

func (f *organizationFixture) scan() []SessionSummary {
	return append([]SessionSummary(nil), f.rows...)
}

func (f *organizationFixture) do(method, target string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	}
	mux := http.NewServeMux()
	registerSessionOrganizationRoutes(mux)
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		handleSessionsWith(w, r, f.scan, func(time.Time) presenceOpenSet { return openFixture() })
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, target, reader))
	return rec
}

func (f *organizationFixture) tag(value string, rows ...SessionSummary) {
	f.t.Helper()
	refs := make([]sessionRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, sessionRef{Runtime: row.Runtime, ID: row.ID})
	}
	key, val, keyed := strings.Cut(value, ":")
	tag := store.SessionOwnerTagValue{Value: value}
	if keyed {
		tag = store.SessionOwnerTagValue{Key: key, Value: val}
	}
	if rec := f.do("POST", "/api/session-tags", sessionTagsRequest{Sessions: refs, Apply: []store.SessionOwnerTagValue{tag}}); rec.Code != 200 {
		f.t.Fatalf("tag %q: %d %s", value, rec.Code, rec.Body.String())
	}
}

func (f *organizationFixture) facet(sessionID, key, value string, at int64) {
	f.t.Helper()
	tx, err := f.ix.BeginGov()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := tx.UpsertSessionState(sessionID, store.StateRow{Key: key, Value: value, Detector: key + "." + value, Provenance: "observed"}, at); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
	sessionTagSnapshots.drop()
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	return out
}

func titles(rows []railSession) string {
	var out []string
	for _, row := range rows {
		out = append(out, row.Title)
	}
	return strings.Join(out, ",")
}

func organizationRows() []SessionSummary {
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	row := func(repo, runtime, id, title string, daysAgo int) SessionSummary {
		s := sessionFixture(repo, runtime, id, base.Add(-time.Duration(daysAgo)*24*time.Hour))
		s.Title = title
		return s
	}
	return []SessionSummary{
		row("/work/oms", "claude", "walmart", "Walmart plan", 2),
		row("/work/oms", "claude", "routing", "Routing plan", 9),
		row("/work/oms", "claude", "shipped", "Shipped plan", 4),
		row("/work/pool", "claude", "diligence", "Due diligence", 7),
		row("/work/cart", "claude", "untouched", "Never touched", 1),
	}
}

// A fresh installation has no tags, no views and nothing to suggest: the rail
// is today's rail, and the views file does not exist until the owner saves one.
func TestFreshInstallShipsNoViewsNoTagsNoSuggestions(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	views := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil))
	if len(views.Views) != 0 || len(views.Rejected) != 0 || views.Origin != sessionViewsOriginNone {
		t.Fatalf("fresh views = %+v", views)
	}
	if _, err := os.Stat(sessionViewsPath(f.dataDir)); !os.IsNotExist(err) {
		t.Fatalf("reading the views must not create the file: %v", err)
	}
	vocabulary := decodeBody[sessionVocabularyResponse](t, f.do("GET", "/api/session-tags/vocabulary", nil))
	if len(vocabulary.Tags) != 0 || len(vocabulary.Known) != 0 {
		t.Fatalf("fresh vocabulary = %+v", vocabulary)
	}
	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&counts=1", nil))
	if rail.ViewCounts != nil || rail.GroupBy != "" {
		t.Fatalf("fresh rail carries organization it was not asked for: %+v", rail)
	}
}

// The canonical journey: sessions enter a view by what detectors saw, in any
// repository, with no act by the owner; his tag is how one leaves.
func TestViewAdmitsByObservedFactsAcrossRepositoriesAndOwnerTagDismisses(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	for _, id := range []string{"walmart", "routing", "shipped", "diligence"} {
		f.facet(id, "phase", "plan", 1000)
	}
	f.facet("shipped", "vcs", "commit", 2000)

	created := decodeBody[sessionViewsDocument](t, f.do("POST", "/api/session-views", sessionViewWriteRequest{
		StateToken: decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil)).StateToken,
		View:       SavedSessionView{Name: "Planned, not committed", Query: "tag:phase=plan -tag:vcs=commit -tag:approved", Sort: "longest"}}))
	if len(created.Views) != 1 || created.Origin != sessionViewsPath(f.dataDir) {
		t.Fatalf("created = %+v", created)
	}
	viewID := created.Views[0].ID
	count := func() int {
		rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&counts=1", nil))
		return rail.ViewCounts[viewID]
	}
	if got := count(); got != 3 {
		t.Fatalf("count = %d, want the three planned-and-uncommitted sessions across two repositories", got)
	}

	query := url.QueryEscape(created.Views[0].Query)
	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&query="+query, nil))
	total := 0
	for _, group := range rail.Repositories {
		total += group.Total
	}
	if len(rail.Repositories) != 2 || total != 3 {
		t.Fatalf("groups = %+v", rail.Repositories)
	}
	page := decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=/work/oms&query="+query, nil))
	if page.Total != 2 || titles(page.Sessions) != "Walmart plan,Routing plan" {
		t.Fatalf("page = %d %q", page.Total, titles(page.Sessions))
	}

	f.tag("approved", f.rows[0])
	if got := count(); got != 2 {
		t.Fatalf("after the owner's tag the count is %d, want 2", got)
	}
	page = decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=/work/oms&query="+query, nil))
	if titles(page.Sessions) != "Routing plan" {
		t.Fatalf("tagged session still in the view: %q", titles(page.Sessions))
	}
}

func TestTagsShowOnRowsAndTagKeyGroupsAreSubRepositoryGroups(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("Topic:Amazon-Routing", f.rows[1])
	f.tag("topic:walmart", f.rows[0])
	f.tag("topic:amazon-routing", f.rows[3]) // another repository, another spelling
	f.facet("walmart", "phase", "plan", 50)

	page := decodeBody[sessionPageResponse](t, f.do("GET", "/api/sessions?view=repository&repository=/work/oms&mode=all", nil))
	var walmart railSession
	for _, row := range page.Sessions {
		if row.ID == "walmart" {
			walmart = row
		}
	}
	if len(walmart.Tags) != 1 || walmart.Tags[0].Key != "topic" || walmart.Tags[0].Provenance != "user-asserted" || len(walmart.Facts) != 0 {
		t.Fatalf("a plain repository row carries the owner's tags and never detector facts: %+v", walmart)
	}
	header := decodeBody[sessionTagsResponse](t, f.do("GET", "/api/session-tags?runtime=claude&id=walmart", nil))
	if len(header.Sessions[0].Facts) != 1 || header.Sessions[0].Facts[0].Provenance != "observed" {
		t.Fatalf("facts are read in the header, kept apart from tags: %+v", header.Sessions[0])
	}

	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&query="+url.QueryEscape("tag:topic=*")+"&group_by=tag-key:topic", nil))
	if rail.GroupBy != "tag-key:topic" || len(rail.Repositories) != 2 {
		t.Fatalf("rail = %+v", rail)
	}
	for _, group := range rail.Repositories {
		if group.Key == "amazon-routing" && (group.Total != 2 || group.Label != "Amazon-Routing" || group.LaunchCwd != "") {
			t.Fatalf("one tag in two spellings and two repositories is one group, named as first written: %+v", group)
		}
	}
	none := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&group_by=tag-key:topic", nil))
	var unnamed sessionRepository
	for _, group := range none.Repositories {
		if group.Key == "" {
			unnamed = group
		}
	}
	if unnamed.Total != 2 || none.Repositories[len(none.Repositories)-1].Key != "" {
		t.Fatalf("sessions without the key form the last, unnamed group: %+v", none.Repositories)
	}
}

func TestMalformedQueryIsNamedAndEmptiedGroupIsAnEmptyPage(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	rec := f.do("GET", "/api/sessions?view=rail&query="+url.QueryEscape("bogus:x"), nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"bogus:"`) {
		t.Fatalf("malformed query: %d %s", rec.Code, rec.Body.String())
	}
	page := decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=/work/oms&query="+url.QueryEscape("tag:nobody-has-this"), nil))
	if page.Total != 0 || len(page.Sessions) != 0 {
		t.Fatalf("an emptied group is an empty page: %+v", page)
	}
	if rec := f.do("GET", "/api/sessions?view=group&mode=sideways&group=x&group_by=none", nil); rec.Code != 400 {
		t.Fatalf("bad mode accepted: %d", rec.Code)
	}
}

func TestWordsSearchOnlyInsideTheFilter(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("follow-up", f.rows[0], f.rows[3])
	restore := sessionTextSearch
	t.Cleanup(func() { sessionTextSearch = restore })
	var asked []string
	sessionTextSearch = func(words string, ids []string, limit int) ([]store.TranscriptProjectionKey, bool, error) {
		asked = append([]string(nil), ids...)
		return []store.TranscriptProjectionKey{{Runtime: "claude", SessionID: "diligence"}, {Runtime: "codex", SessionID: "walmart"}}, true, nil
	}
	page := decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=&group_by=none&query="+url.QueryEscape("mine:follow-up lawyer"), nil))
	if strings.Join(asked, ",") != "walmart,diligence" {
		t.Fatalf("the search was not confined to the filter's sessions: %v", asked)
	}
	if titles(page.Sessions) != "Due diligence" {
		t.Fatalf("a hit under another runtime's id must not count: %q", titles(page.Sessions))
	}
	if !page.MoreTextMatches {
		t.Fatal("more sessions matched than the limit, and the page does not say so")
	}
	sessionTextSearch = func(string, []string, int) ([]store.TranscriptProjectionKey, bool, error) {
		return nil, false, fmt.Errorf("database is locked")
	}
	if rec := f.do("GET", "/api/sessions?view=rail&query="+url.QueryEscape("mine:follow-up lawyer"), nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a failing text search is the daemon's problem, not a bad query: %d %s", rec.Code, rec.Body.String())
	}
}

// Codex: a child rollout carries its parent's thread id. A tag on the parent
// thread must not appear on the child, and must follow the thread into a
// resumed rollout segment.
func TestCodexTagFollowsTheThreadAndNeverItsChildren(t *testing.T) {
	const thread = "019f3c1a-1111-7222-8333-444455556666"
	const childMeta = "019f3c1a-aaaa-7bbb-8ccc-ddddeeeeffff"
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	parent := sessionFixture("/work/oms", "codex", "rollout-2026-09-10T12-00-00-"+thread, base)
	parent.ThreadID, parent.Title = thread, "Parent"
	child := sessionFixture("/work/oms", "codex", "rollout-2026-09-10T12-05-00-"+childMeta, base.Add(-time.Minute))
	child.ThreadID, child.MetaID, child.LineageKind, child.Title = thread, childMeta, "spawn", "Child"
	if harvest.MatchID(child, thread) {
		t.Fatal("the codex adapter matches a child rollout by its parent's thread id: every tag on a thread would now decorate its children")
	}
	f := newOrganizationFixture(t, parent, child)
	f.tag("follow-up", parent)
	if rec := f.do("PUT", "/api/session-notes", sessionNoteRequest{Session: sessionRef{Runtime: "codex", ID: parent.ID}, Text: "why this waits"}); rec.Code != 200 {
		t.Fatalf("note: %d %s", rec.Code, rec.Body.String())
	}

	decorated := decorateSessions(f.rows, time.Now())
	if len(decorated[0].Tags) != 1 || len(decorated[1].Tags) != 0 {
		t.Fatalf("parent=%+v child=%+v", decorated[0].Tags, decorated[1].Tags)
	}

	resumed := sessionFixture("/work/oms", "codex", "rollout-2026-09-12T09-00-00-"+thread, base.Add(48*time.Hour))
	resumed.ThreadID, resumed.Title = thread, "Parent, resumed"
	f.rows = []SessionSummary{resumed, child}
	sessionTagSnapshots.drop()
	after := decorateSessions(f.rows, time.Now())
	if len(after[0].Tags) != 1 || len(after[1].Tags) != 0 {
		t.Fatalf("the tag did not follow the thread into its new segment: %+v", after)
	}
	if after[0].Note != "why this waits" || after[1].Note != "" {
		t.Fatalf("the note must follow the thread with the tag, and never reach a child: %+v", after)
	}
	if gone := sessionTagSnapshots.get(time.Now()).vanished(f.rows); len(gone) != 0 {
		t.Fatalf("a resumed thread is not a vanished session: %+v", gone)
	}
}

// A tagged session whose transcript the scan no longer finds is still listed,
// still counted, can be opened to its kept text and can be untagged.
func TestTaggedSessionOutlivesItsTranscript(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("follow-up", f.rows[3])
	if rec := f.do("PUT", "/api/session-notes", sessionNoteRequest{Session: sessionRef{Runtime: "claude", ID: "diligence"}, Text: "lawyer call Thursday"}); rec.Code != 200 {
		t.Fatalf("note: %d %s", rec.Code, rec.Body.String())
	}
	// What search kept of the conversation, as the indexer would have written it.
	if _, err := f.ix.ReplaceTranscriptProjection(store.TranscriptProjection{
		Session:    store.SessionRow{Vendor: "claude", ID: "diligence", Title: "Due diligence", CWD: "/work/pool"},
		Generation: "g1", IndexedAt: time.Unix(100, 0), SourceCount: 1,
		Documents: []store.SearchDocument{
			{Order: 0, Kind: "title", Text: "Due diligence", Lineage: "title"},
			{Order: 1, Kind: "user", Text: "Summarise the agreements", Lineage: "event:user"},
			{Order: 2, Kind: "assistant", Text: "The lawyer question remains open.", Lineage: "event:assistant"}}}); err != nil {
		t.Fatal(err)
	}
	f.rows = append(f.rows[:3:3], f.rows[4:]...) // the scan no longer finds its transcript
	sessionScanCoalescer = &scanCoalescer{result: f.rows, done: time.Now().Add(time.Hour)}
	sessionTagSnapshots.drop()

	page := decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=/work/pool&query="+url.QueryEscape("mine:follow-up note:lawyer"), nil))
	if page.Total != 1 || !page.Sessions[0].TranscriptMissing || page.Sessions[0].Title != "Due diligence" || page.Sessions[0].Note != "lawyer call Thursday" {
		t.Fatalf("page = %+v", page)
	}
	detail, err := LoadSession("claude", "diligence")
	if err != nil || detail.Title != "Due diligence" {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	// It says what was observed. "Deleted" would be a guess: an unreadable
	// session store looks exactly the same from here.
	if detail.TranscriptNote != "No transcript file was found for this session. Showing the text that was kept." || strings.Contains(detail.TranscriptNote, "eleted") {
		t.Fatalf("note = %q", detail.TranscriptNote)
	}
	if len(detail.Events) != 2 || detail.Events[0].Kind != "user" || detail.Events[1].Text != "The lawyer question remains open." {
		t.Fatalf("the kept text opens as the conversation, without its title row: %+v", detail.Events)
	}
	if _, err := LoadSession("claude", "never-tagged-never-seen"); err == nil {
		t.Fatal("an unknown session must still be not found")
	}
	if rec := f.do("POST", "/api/session-tags", sessionTagsRequest{Sessions: []sessionRef{{Runtime: "claude", ID: "diligence"}},
		Retract: []store.SessionOwnerTagValue{{Value: "FOLLOW-UP"}}}); rec.Code != 200 {
		t.Fatalf("a vanished session must be untaggable: %d %s", rec.Code, rec.Body.String())
	}
	page = decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=/work/pool&group_by=repository", nil))
	if page.Total != 0 {
		t.Fatalf("with its last tag gone the session is no longer remembered: %+v", page)
	}
}

func TestTagRequestsAreBoundedValidatedAndAllOrNothing(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	refs := []sessionRef{{Runtime: "claude", ID: "walmart"}, {Runtime: "claude", ID: "no-such-session"}}
	if rec := f.do("POST", "/api/session-tags", sessionTagsRequest{Sessions: refs, Apply: []store.SessionOwnerTagValue{{Value: "x"}}}); rec.Code != 404 {
		t.Fatalf("unknown session: %d", rec.Code)
	}
	if tags, _, _ := f.ix.AllActiveSessionOwnerTags(10); len(tags) != 0 {
		t.Fatalf("a refused request tagged something: %+v", tags)
	}
	for _, bad := range []store.SessionOwnerTagValue{{Value: "a=b"}, {Key: "tag", Value: "x"}, {Value: ""}} {
		rec := f.do("POST", "/api/session-tags", sessionTagsRequest{Sessions: refs[:1], Apply: []store.SessionOwnerTagValue{bad}})
		if rec.Code != 400 || strings.Contains(rec.Body.String(), "session owner tag is invalid") {
			t.Fatalf("%+v: %d %q (the owner reads this sentence)", bad, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do("POST", "/api/session-tags", map[string]any{"sessions": refs[:1], "apply": []any{}, "surprise": 1}); rec.Code != 400 {
		t.Fatalf("unknown field accepted: %d", rec.Code)
	}
	many := make([]sessionRef, sessionOrganizationConfig().BulkSelectionMax+1)
	if rec := f.do("POST", "/api/session-tags", sessionTagsRequest{Sessions: many, Apply: []store.SessionOwnerTagValue{{Value: "x"}}}); rec.Code != 400 {
		t.Fatalf("oversized selection accepted: %d", rec.Code)
	}
}

func TestRenameAndPurgeChangeEverySession(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("aproved", f.rows[0], f.rows[1], f.rows[3])
	renamed := decodeBody[sessionTagChangeResponse](t, f.do("POST", "/api/session-tags/rename", sessionTagRenameRequest{
		From: store.SessionOwnerTagValue{Value: "aproved"}, To: store.SessionOwnerTagValue{Value: "approved"}}))
	if renamed.Sessions != 3 {
		t.Fatalf("renamed = %+v", renamed)
	}
	vocabulary := decodeBody[sessionVocabularyResponse](t, f.do("GET", "/api/session-tags/vocabulary", nil))
	if len(vocabulary.Tags) != 1 || vocabulary.Tags[0].Value != "approved" || vocabulary.Tags[0].Sessions != 3 {
		t.Fatalf("vocabulary = %+v", vocabulary.Tags)
	}
	purged := decodeBody[sessionTagChangeResponse](t, f.do("POST", "/api/session-tags/purge", sessionTagPurgeRequest{Tag: store.SessionOwnerTagValue{Value: "approved"}}))
	if purged.Sessions == 0 {
		t.Fatal("purge removed nothing")
	}
	if left := decodeBody[sessionVocabularyResponse](t, f.do("GET", "/api/session-tags/vocabulary", nil)); len(left.Tags) != 0 {
		t.Fatalf("purged tag still suggested: %+v", left.Tags)
	}
}

func TestViewCountsAreWithheldWhenTheyWouldLie(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("follow-up", f.rows[0])
	token := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil)).StateToken
	for _, view := range []SavedSessionView{
		{Name: "durable", Query: "mine:follow-up"},
		{Name: "live", Query: "mine:follow-up status:running"},
		{Name: "words", Query: "mine:follow-up lawyer"},
	} {
		token = decodeBody[sessionViewsDocument](t, f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: token, View: view})).StateToken
	}
	views := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil)).Views
	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&counts=1", nil))
	if len(rail.ViewCounts) != 1 || rail.ViewCounts[views[0].ID] != 1 {
		t.Fatalf("only the durable view is counted: %+v", rail.ViewCounts)
	}

	restore := sessionTagsRead
	t.Cleanup(func() { sessionTagsRead = restore; sessionTagSnapshots.drop() })
	sessionTagsRead = func(now time.Time) sessionTagSnapshot {
		snapshot := restore(now)
		snapshot.truncated = true
		return snapshot
	}
	sessionTagSnapshots.drop()
	if rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&counts=1", nil)); rail.ViewCounts != nil {
		t.Fatalf("a read that hit its bound must show no number, not a low one: %+v", rail.ViewCounts)
	}
}

func TestStoreUnavailableLeavesTheRailStandingWithoutTags(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("follow-up", f.rows[0])
	restore := sessionTagsRead
	t.Cleanup(func() { sessionTagsRead = restore; sessionTagSnapshots.drop() })
	sessionTagsRead = func(time.Time) sessionTagSnapshot { return sessionTagSnapshot{} }
	sessionTagSnapshots.drop()
	page := decodeBody[sessionPageResponse](t, f.do("GET", "/api/sessions?view=repository&repository=/work/oms&mode=all", nil))
	if page.Total != 3 || len(page.Sessions[0].Tags) != 0 {
		t.Fatalf("an unreadable tag store must not take the rail down: %+v", page)
	}
}

func TestSavedViewsAreTheOwnersFile(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	limits := sessionOrganizationConfig()
	get := func() sessionViewsDocument {
		return decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil))
	}
	first := decodeBody[sessionViewsDocument](t, f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: get().StateToken,
		View: SavedSessionView{Name: "Mine", Query: "mine:follow-up", GroupBy: "tag-key:topic", Sort: "longest"}}))

	// A write with the token of an earlier read is refused: a second browser
	// cannot silently undo the first.
	if rec := f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: "stale", View: SavedSessionView{Name: "B", Query: ""}}); rec.Code != 409 {
		t.Fatalf("stale token: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: first.StateToken, View: SavedSessionView{Name: "Bad", Query: "bogus:x"}}); rec.Code != 400 {
		t.Fatalf("a view is validated when written: %d", rec.Code)
	}

	// The owner edits the file by hand: one good entry, one broken one.
	hand := fmt.Sprintf(`{"format_version":1,"views":[
	  {"id":"%s","name":"Mine, renamed by hand","query":"mine:follow-up"},
	  {"id":"by-hand","name":"Typo","query":"bogus:x"},
	  {"id":"extra","name":"Extra field","query":"","colour":"red"}]}`, first.Views[0].ID)
	if err := os.WriteFile(sessionViewsPath(f.dataDir), []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	edited := get()
	if len(edited.Views) != 1 || edited.Views[0].Name != "Mine, renamed by hand" || len(edited.Rejected) != 2 ||
		edited.Rejected[0].Name != "Typo" || !strings.Contains(edited.Rejected[0].Problem, `"bogus:"`) || edited.Rejected[1].Name != "Extra field" {
		t.Fatalf("a hand edit shows at once; a broken entry is named and the others work: %+v", edited)
	}
	// While an entry is unreadable the console must not save over the file.
	if rec := f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: edited.StateToken, View: SavedSessionView{Name: "C", Query: ""}}); rec.Code != 409 {
		t.Fatalf("saving over a hand edit in progress: %d", rec.Code)
	}
	raw, _ := os.ReadFile(sessionViewsPath(f.dataDir))
	if string(raw) != hand {
		t.Fatal("a refused save changed the owner's file")
	}
	if err := os.WriteFile(sessionViewsPath(f.dataDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if broken := loadSessionViews(f.dataDir, limits); len(broken.Views) != 0 || len(broken.Rejected) != 1 || broken.Rejected[0].Index != -1 {
		t.Fatalf("an unreadable file is zero views and one report, every time: %+v", broken)
	}
}

func TestReorderMustNameEveryViewAndDeleteLeavesAnEmptyList(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	document := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil))
	for _, name := range []string{"one", "two", "three"} {
		document = decodeBody[sessionViewsDocument](t, f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: document.StateToken, View: SavedSessionView{Name: name, Query: ""}}))
	}
	ids := []string{document.Views[2].ID, document.Views[0].ID, document.Views[1].ID}
	if rec := f.do("PUT", "/api/session-views/order", sessionViewOrderRequest{StateToken: document.StateToken, Order: ids[:2]}); rec.Code != 409 {
		t.Fatalf("an order that leaves a view out would delete it: %d", rec.Code)
	}
	document = decodeBody[sessionViewsDocument](t, f.do("PUT", "/api/session-views/order", sessionViewOrderRequest{StateToken: document.StateToken, Order: ids}))
	if document.Views[0].Name != "three" {
		t.Fatalf("order = %+v", document.Views)
	}
	for len(document.Views) > 0 {
		document = decodeBody[sessionViewsDocument](t, f.do("DELETE", "/api/session-views/"+document.Views[0].ID+"?state_token="+document.StateToken, nil))
	}
	if again := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil)); len(again.Views) != 0 {
		t.Fatalf("deleting the last view leaves an empty list, not a default: %+v", again)
	}
}

// The display cap is for display. A view must see every tag a session carries:
// capping before matching silently dropped a session with many tags out of its
// views, and let an exclusion wrongly admit it.
func TestTheRowCapNeverHidesATagFromAViewOrTheHeader(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	limit := sessionOrganizationConfig().RowTagsMax
	for i := 0; i <= limit; i++ { // one more than a row shows; the first applied sorts last
		f.tag(fmt.Sprintf("tag-%02d", i), f.rows[0])
	}
	oldest := url.QueryEscape("mine:tag-00")
	page := decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=/work/oms&query="+oldest, nil))
	if page.Total != 1 || len(page.Sessions[0].Tags) != limit {
		t.Fatalf("the tag beyond the row cap must still match, and the row still shows only %d: total=%d tags=%d", limit, page.Total, len(page.Sessions[0].Tags))
	}
	excluded := decodeBody[sessionGroupPageResponse](t, f.do("GET", "/api/sessions?view=group&mode=all&group=/work/oms&query="+url.QueryEscape("-mine:tag-00"), nil))
	if strings.Contains(titles(excluded.Sessions), "Walmart plan") {
		t.Fatal("an exclusion admitted a session because its tag was cut off before matching")
	}
	header := decodeBody[sessionTagsResponse](t, f.do("GET", "/api/session-tags?runtime=claude&id=walmart", nil))
	if len(header.Sessions[0].Tags) != limit+1 {
		t.Fatalf("the header must show, and let the owner remove, every tag: %d", len(header.Sessions[0].Tags))
	}
}

// An unreadable tag store is not "nothing is tagged". A filtered request must
// fail loudly rather than answer "no sessions" (or, for an exclusion, "all of
// them"), and no view may show a number.
func TestUnreadableTagsAreNeverServedAsNoTags(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("follow-up", f.rows[0])
	token := decodeBody[sessionViewsDocument](t, f.do("GET", "/api/session-views", nil)).StateToken
	decodeBody[sessionViewsDocument](t, f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: token, View: SavedSessionView{Name: "v", Query: "-mine:approved"}}))
	restore := sessionTagsRead
	t.Cleanup(func() { sessionTagsRead = restore; sessionTagSnapshots.drop() })
	sessionTagsRead = func(time.Time) sessionTagSnapshot { return sessionTagSnapshot{} }
	sessionTagSnapshots.drop()
	for _, target := range []string{
		"/api/sessions?view=rail&query=" + url.QueryEscape("mine:follow-up"),
		"/api/sessions?view=group&mode=all&group=/work/oms&query=" + url.QueryEscape("-mine:follow-up"),
	} {
		if rec := f.do("GET", target, nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s answered %d from an unreadable tag store: %s", target, rec.Code, rec.Body.String())
		}
	}
	rail := decodeBody[sessionRailResponse](t, f.do("GET", "/api/sessions?view=rail&counts=1", nil))
	if rail.ViewCounts != nil || len(rail.Repositories) != 3 {
		t.Fatalf("the plain rail stands, and shows no number it cannot stand behind: %+v", rail)
	}
}

// One request, one transaction: taking a tag off and putting an unstorable one
// on must change nothing. And the two bounds that multiply inside that
// transaction are both enforced.
func TestAMixedTagChangeIsAllOrNothingAndBounded(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("planning", f.rows[0])
	ref := []sessionRef{{Runtime: "claude", ID: "walmart"}}
	rec := f.do("POST", "/api/session-tags", sessionTagsRequest{Sessions: ref,
		Retract: []store.SessionOwnerTagValue{{Value: "planning"}}, Apply: []store.SessionOwnerTagValue{{Value: "bad*"}}})
	if rec.Code != 400 {
		t.Fatalf("status %d", rec.Code)
	}
	after := decodeBody[sessionTagsResponse](t, f.do("GET", "/api/session-tags?runtime=claude&id=walmart", nil))
	if len(after.Sessions[0].Tags) != 1 || after.Sessions[0].Tags[0].Value != "planning" {
		t.Fatalf("the retract half of a refused request was kept: %+v", after.Sessions[0].Tags)
	}
	many := make([]store.SessionOwnerTagValue, sessionOrganizationConfig().TagsPerRequestMax+1)
	for i := range many {
		many[i] = store.SessionOwnerTagValue{Value: fmt.Sprintf("t%d", i)}
	}
	if rec := f.do("POST", "/api/session-tags", sessionTagsRequest{Sessions: ref, Apply: many}); rec.Code != 400 {
		t.Fatalf("an unbounded tag list was accepted: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/session-tags", sessionTagsRequest{Sessions: []sessionRef{{Runtime: "claude"}}, Apply: many[:1]}); rec.Code != 400 {
		t.Fatalf("a reference with no id is malformed, not missing: %d", rec.Code)
	}
}

// A session nobody tagged serializes exactly as it always has — even when
// detectors have recorded facts about it. Facts belong to the header and to
// filters; on a plain repository page they would change the owner's default
// rail without him having done anything.
func TestDetectorFactsNeverChangeAPlainRepositoryPage(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	before := f.do("GET", "/api/sessions?view=repository&repository=/work/oms&mode=all", nil).Body.String()
	for _, row := range f.rows {
		f.facet(row.ID, "phase", "plan", 100)
		f.facet(row.ID, "fs", "edit", 200)
	}
	after := f.do("GET", "/api/sessions?view=repository&repository=/work/oms&mode=all", nil).Body.String()
	if before != after {
		t.Fatalf("detector facts changed the bytes of a plain page:\nbefore: %s\n after: %s", before, after)
	}
	header := decodeBody[sessionTagsResponse](t, f.do("GET", "/api/session-tags?runtime=claude&id=walmart", nil))
	if len(header.Sessions[0].Facts) != 2 {
		t.Fatalf("the header is where facts are read: %+v", header.Sessions[0])
	}
}

func TestViewsFileRefusesWhatItCouldNotReadBack(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	if err := os.WriteFile(sessionViewsPath(f.dataDir), []byte(`{"format_version":1,"views":[]} trailing`), 0o600); err != nil {
		t.Fatal(err)
	}
	limits := sessionOrganizationConfig()
	if document := loadSessionViews(f.dataDir, limits); len(document.Rejected) != 1 {
		t.Fatalf("text after the document would be erased by the next save, so it must be reported: %+v", document)
	}
	if err := os.Remove(sessionViewsPath(f.dataDir)); err != nil {
		t.Fatal(err)
	}
	token := loadSessionViews(f.dataDir, limits).StateToken
	document := decodeBody[sessionViewsDocument](t, f.do("POST", "/api/session-views", sessionViewWriteRequest{StateToken: token, View: SavedSessionView{Name: "  padded  ", Query: ""}}))
	if document.Views[0].Name != "padded" {
		t.Fatalf("name stored as %q", document.Views[0].Name)
	}
	small := limits
	small.ViewsFileBytesMax = 40
	if _, err := mutateSessionViews(f.dataDir, small, loadSessionViews(f.dataDir, small).StateToken, func(views []SavedSessionView) ([]SavedSessionView, error) { return views, nil }); err == nil {
		t.Fatal("a save the next read would refuse must itself be refused")
	}
}
