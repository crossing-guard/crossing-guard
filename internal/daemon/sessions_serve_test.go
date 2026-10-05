package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

func sessionFixture(key, runtime, id string, modified time.Time) SessionSummary {
	return SessionSummary{
		Runtime: runtime, ID: id, Cwd: key, RepositoryKey: key,
		Path: filepath.Join("/sessions", runtime, id+".jsonl"), Modified: modified,
	}
}

// openFixture expresses "the presence service published these rows as open" in
// the rail's own key space (runtime + catalog id), which is what the rail now
// reads. It replaces the old harvest.ActivityObservation fixtures: those spoke
// filesystem paths, and the rail no longer decides openness from paths.
func openFixture(rows ...SessionSummary) presenceOpenSet {
	open := map[string]bool{}
	for _, row := range rows {
		open[presenceCatalogKey(row.Runtime, row.ID)] = true
	}
	return presenceOpenSet{
		Capability: sessionactivity.Capability{Status: "available", Detail: "presence lane"},
		Open:       open,
	}
}

func TestBuildSessionRepositoriesOrdersOpenThenRecent(t *testing.T) {
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	oldOpen := sessionFixture("/repo/open", "codex", "open", now.Add(-time.Hour))
	recent := sessionFixture("/repo/recent", "claude", "recent", now)
	noProject := sessionFixture(harvest.NoProjectKey, "claude", "none", now.Add(time.Hour))
	groups := buildSessionRepositories([]SessionSummary{recent, oldOpen, noProject}, openFixture(oldOpen))
	if len(groups) != 3 || groups[0].Key != "/repo/open" || groups[1].Key != "/repo/recent" || groups[2].Key != harvest.NoProjectKey {
		t.Fatalf("unexpected repository order: %#v", groups)
	}
}

func TestBuildSessionPageIsStableAndOpenPopulationIsComplete(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	rows := make([]SessionSummary, 0, 20)
	var openRow SessionSummary
	for i := 0; i < 20; i++ {
		s := sessionFixture("/repo", "codex", string(rune('a'+i)), base.Add(-time.Duration(i)*time.Minute))
		rows = append(rows, s)
		if i == 17 {
			openRow = s
		}
	}
	presence := openFixture(openRow)
	page, found := buildSessionPage(rows, presence, "/repo", "all", 15, 15, "", "")
	if !found || page.Total != 20 || len(page.Sessions) != 5 || page.Sessions[0].ID != "p" {
		t.Fatalf("unexpected all page: %#v", page)
	}
	openPage, found := buildSessionPage(rows, presence, "/repo", "open", 0, 15, "", "")
	if !found || openPage.Total != 1 || len(openPage.Sessions) != 1 || openPage.Sessions[0].ID != "r" {
		t.Fatalf("open population was filtered from an all page: %#v", openPage)
	}
}

func TestBuildSessionPageClampsStaleOffsetToLastPage(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	rows := []SessionSummary{
		sessionFixture("/repo", "codex", "a", base),
		sessionFixture("/repo", "codex", "b", base.Add(-time.Minute)),
	}
	page, found := buildSessionPage(rows, presenceOpenSet{}, "/repo", "all", 30, 15, "", "")
	if !found || page.Offset != 0 || len(page.Sessions) != 2 {
		t.Fatalf("stale offset did not recover to last page: %#v", page)
	}
}

func TestBuildSessionPageMovesToSelectedSession(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	rows := make([]SessionSummary, 0, 20)
	for i := 0; i < 20; i++ {
		rows = append(rows, sessionFixture("/repo", "codex", string(rune('a'+i)), base.Add(-time.Duration(i)*time.Minute)))
	}
	page, found := buildSessionPage(rows, presenceOpenSet{}, "/repo", "all", 0, 15, "codex", "r")
	if !found || page.Offset != 15 || len(page.Sessions) != 5 || page.Sessions[2].ID != "r" {
		t.Fatalf("selected session did not choose containing page: %#v", page)
	}
}

func TestHandleSessionsRejectsInvalidBoundsBeforeScan(t *testing.T) {
	for _, target := range []string{
		"/api/sessions?view=rail&repository_limit=0",
		"/api/sessions?view=repository&repository=/repo&mode=all&offset=-1",
		"/api/sessions?view=repository&repository=/repo&mode=all&limit=51",
		"/api/sessions?view=repository&repository=/repo&mode=other",
	} {
		req := httptest.NewRequest("GET", target, nil)
		rec := httptest.NewRecorder()
		handleSessions(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
}

func TestHandleSessionsStructuredRailAndSelectedPage(t *testing.T) {
	base := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	rows := make([]SessionSummary, 0, 20)
	for i := 0; i < 20; i++ {
		rows = append(rows, sessionFixture("/repo", "codex", string(rune('a'+i)), base.Add(-time.Duration(i)*time.Minute)))
	}
	scan := func() []SessionSummary { return append([]SessionSummary(nil), rows...) }
	presence := openFixture(rows[17])
	openSet := func(time.Time) presenceOpenSet { return presence }

	railReq := httptest.NewRequest("GET", "/api/sessions?view=rail&repository_limit=200", nil)
	railRec := httptest.NewRecorder()
	handleSessionsWith(railRec, railReq, scan, openSet)
	if railRec.Code != 200 {
		t.Fatalf("rail status = %d: %s", railRec.Code, railRec.Body.String())
	}
	var rail sessionRailResponse
	if err := json.Unmarshal(railRec.Body.Bytes(), &rail); err != nil {
		t.Fatal(err)
	}
	if rail.RepositoryTotal != 1 || len(rail.Repositories) != 1 || rail.Repositories[0].Total != 20 || rail.Repositories[0].OpenTotal != 1 {
		t.Fatalf("unexpected rail response: %#v", rail)
	}

	pageReq := httptest.NewRequest("GET", "/api/sessions?view=repository&repository=/repo&mode=all&offset=0&limit=15&selected_runtime=codex&selected_id=r", nil)
	pageRec := httptest.NewRecorder()
	handleSessionsWith(pageRec, pageReq, scan, openSet)
	if pageRec.Code != 200 {
		t.Fatalf("page status = %d: %s", pageRec.Code, pageRec.Body.String())
	}
	var page sessionPageResponse
	if err := json.Unmarshal(pageRec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Offset != 15 || len(page.Sessions) != 5 || page.Sessions[2].ID != "r" {
		t.Fatalf("unexpected selected page response: %#v", page)
	}
}

func TestHandleSessionsCompatibilityShapeRemainsArray(t *testing.T) {
	row := sessionFixture("/repo", "codex", "a", time.Now())
	req := httptest.NewRequest("GET", "/api/sessions", nil)
	rec := httptest.NewRecorder()
	handleSessionsWith(rec, req, func() []SessionSummary { return []SessionSummary{row} }, nil)
	var response []SessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || len(response) != 1 {
		t.Fatalf("compatibility response is not a session array: %v %s", err, rec.Body.String())
	}
}

// G-5 (g4 plan §3a): agent sessions never render as flat rail rows — they
// fold under their parent by exact root-id equality across ALL identity
// alternates (artifact id, vendor meta id, vendor thread id), Total counts
// non-agent rows only, and an orphan agent session renders nowhere.
func TestBuildSessionPagePartitionsAgentSessionsUnderParents(t *testing.T) {
	restore := agentSessionParentsRead
	t.Cleanup(func() { agentSessionParentsRead = restore })
	agentSessionParentsRead = func(ids []string) map[string]store.AgentSessionParent {
		return map[string]store.AgentSessionParent{
			"agent-1": {Role: "follower", RootRuntime: "codex", RootNativeSessionID: "thread-p"},
			"agent-2": {Role: "helper", RootRuntime: "codex", RootCatalogSessionID: "parent-meta"},
			"orphan":  {Role: "helper", RootRuntime: "codex", RootNativeSessionID: "absent-parent"},
		}
	}
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	parent := sessionFixture("/repo", "codex", "parent", base)
	parent.MetaID = "parent-meta"
	parent.ThreadID = "thread-p"
	agent1 := sessionFixture("/repo", "codex", "agent-1", base.Add(-time.Minute))
	agent2 := sessionFixture("/repo", "codex", "agent-2", base.Add(-2*time.Minute))
	orphan := sessionFixture("/repo", "codex", "orphan", base.Add(-3*time.Minute))
	other := sessionFixture("/repo", "codex", "other", base.Add(-4*time.Minute))
	page, found := buildSessionPage([]SessionSummary{parent, agent1, agent2, orphan, other},
		presenceOpenSet{}, "/repo", "all", 0, 15, "", "")
	if !found || page.Total != 2 || len(page.Sessions) != 2 {
		t.Fatalf("agent sessions were not partitioned out of the pageable rows: %#v", page)
	}
	fold := page.AgentChildren["parent"]
	if len(fold) != 2 || fold[0].ID != "agent-1" || fold[0].AgentRole != "follower" || fold[1].ID != "agent-2" || fold[1].AgentRole != "helper" {
		t.Fatalf("fold did not attach by thread and meta identity: %#v", page.AgentChildren)
	}
	for _, row := range page.Sessions {
		if row.ID == "agent-1" || row.ID == "agent-2" || row.ID == "orphan" {
			t.Fatalf("agent session rendered as a flat rail row: %s", row.ID)
		}
	}
	if _, ok := page.AgentChildren["orphan"]; ok {
		t.Fatal("orphan agent session must render nowhere on the rail")
	}
}

// A selected AGENT session lands on its parent's page (offset math matches
// children), so opening one from the strip never strands the rail.
func TestBuildSessionPageSelectedAgentChildLandsOnParentPage(t *testing.T) {
	restore := agentSessionParentsRead
	t.Cleanup(func() { agentSessionParentsRead = restore })
	agentSessionParentsRead = func(ids []string) map[string]store.AgentSessionParent {
		return map[string]store.AgentSessionParent{
			"agent-x": {Role: "helper", RootRuntime: "codex", RootNativeSessionID: "r"},
		}
	}
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	rows := make([]SessionSummary, 0, 21)
	for i := 0; i < 20; i++ {
		rows = append(rows, sessionFixture("/repo", "codex", string(rune('a'+i)), base.Add(-time.Duration(i)*time.Minute)))
	}
	rows = append(rows, sessionFixture("/repo", "codex", "agent-x", base.Add(-30*time.Minute)))
	page, found := buildSessionPage(rows, presenceOpenSet{}, "/repo", "all", 0, 15, "codex", "agent-x")
	if !found || page.Offset != 15 || page.Total != 20 {
		t.Fatalf("selected agent child did not choose the parent's page: %#v", page)
	}
	if len(page.AgentChildren["r"]) != 1 || page.AgentChildren["r"][0].ID != "agent-x" {
		t.Fatalf("selected agent child missing from the parent's fold: %#v", page.AgentChildren)
	}
}

// The fold is mode-invariant: partition happens on the full group rows, so an
// open parent still carries its (closed) agent children in open mode and the
// open Total counts non-agent open rows only.
func TestBuildSessionPageFoldIsModeInvariant(t *testing.T) {
	restore := agentSessionParentsRead
	t.Cleanup(func() { agentSessionParentsRead = restore })
	agentSessionParentsRead = func(ids []string) map[string]store.AgentSessionParent {
		return map[string]store.AgentSessionParent{
			"agent-y": {Role: "follower", RootRuntime: "codex", RootNativeSessionID: "parent"},
		}
	}
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	parent := sessionFixture("/repo", "codex", "parent", base)
	agent := sessionFixture("/repo", "codex", "agent-y", base.Add(-time.Minute))
	page, found := buildSessionPage([]SessionSummary{parent, agent}, openFixture(parent), "/repo", "open", 0, 15, "", "")
	if !found || page.Total != 1 || len(page.Sessions) != 1 || page.Sessions[0].ID != "parent" {
		t.Fatalf("open mode total must count non-agent open rows only: %#v", page)
	}
	if len(page.AgentChildren["parent"]) != 1 || page.AgentChildren["parent"][0].ID != "agent-y" {
		t.Fatalf("open-mode parent lost its fold: %#v", page.AgentChildren)
	}
}

// nativeChildFixture builds a codex guardian child with its ParentID pointing
// at the parent's thread id.
func nativeChildFixture(key, runtime, id, parentID, role, kind string, modified time.Time) SessionSummary {
	s := sessionFixture(key, runtime, id, modified)
	s.ParentID = parentID
	s.LineageKind = kind
	s.LineageRole = role
	return s
}

// Native subagents fold under their nearest top-level ancestor, provenance
// separate from the caused agent fold; Total counts non-child rows only.
func TestBuildSessionPageFoldsNativeSubagents(t *testing.T) {
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	parent := sessionFixture("/repo", "codex", "parent", base)
	parent.ThreadID = "thread-p"
	child := nativeChildFixture("/repo", "codex", "child", "thread-p", "guardian", "native-subagent", base.Add(-time.Minute))
	other := sessionFixture("/repo", "codex", "other", base.Add(-2*time.Minute))
	page, found := buildSessionPage([]SessionSummary{parent, child, other}, presenceOpenSet{}, "/repo", "all", 0, 15, "", "")
	if !found || page.Total != 2 || len(page.Sessions) != 2 {
		t.Fatalf("native child was not partitioned out of the pageable rows: %#v", page)
	}
	fold := page.NativeChildren["parent"]
	if len(fold) != 1 || fold[0].ID != "child" || fold[0].NativeRole != "guardian" || fold[0].NativeKind != "native-subagent" {
		t.Fatalf("native fold did not attach by parent identity: %#v", page.NativeChildren)
	}
	for _, row := range page.Sessions {
		if row.ID == "child" {
			t.Fatalf("native child rendered as a flat rail row: %s", row.ID)
		}
	}
	if page.AgentChildren != nil {
		t.Fatalf("native child leaked into the caused agent fold: %#v", page.AgentChildren)
	}
}

// A depth-2 native child (parent_id resolves to another child, not a top-level
// row) still folds under the nearest top-level ancestor (RT1), never dropped.
func TestBuildSessionPageFoldsDepthTwoNativeChild(t *testing.T) {
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	top := sessionFixture("/repo", "codex", "top", base)
	top.ThreadID = "thread-top"
	// mid is a native child of top (parent_id = thread-top).
	mid := nativeChildFixture("/repo", "codex", "mid", "thread-top", "review-php", "native-thread-spawn", base.Add(-time.Minute))
	mid.MetaID = "mid-meta"
	// deep is a native child whose parent_id resolves to mid's MetaID.
	deep := nativeChildFixture("/repo", "codex", "deep", "mid-meta", "guardian", "native-subagent", base.Add(-2*time.Minute))
	page, found := buildSessionPage([]SessionSummary{top, mid, deep}, presenceOpenSet{}, "/repo", "all", 0, 15, "", "")
	if !found || page.Total != 1 || len(page.Sessions) != 1 || page.Sessions[0].ID != "top" {
		t.Fatalf("depth-2 native children were not folded under the top-level ancestor: %#v", page)
	}
	fold := page.NativeChildren["top"]
	if len(fold) != 2 {
		t.Fatalf("expected both native children under top, got %#v", page.NativeChildren)
	}
}

// A native child whose parent resolves to no scanned row renders nowhere on the
// rail (orphan), and the rail header count excludes native children (RT2).
func TestBuildSessionPageOrphanNativeChildAndHeaderCount(t *testing.T) {
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	parent := sessionFixture("/repo", "codex", "parent", base)
	parent.ThreadID = "thread-p"
	child := nativeChildFixture("/repo", "codex", "child", "thread-p", "guardian", "native-subagent", base.Add(-time.Minute))
	orphan := nativeChildFixture("/repo", "codex", "orphan", "missing-parent", "guardian", "native-subagent", base.Add(-2*time.Minute))
	page, found := buildSessionPage([]SessionSummary{parent, child, orphan}, presenceOpenSet{}, "/repo", "all", 0, 15, "", "")
	if !found || page.Total != 1 || len(page.Sessions) != 1 {
		t.Fatalf("orphan native child must render nowhere and Total must count non-child only: %#v", page)
	}
	if len(page.NativeChildren["parent"]) != 1 {
		t.Fatalf("parent lost its native fold: %#v", page.NativeChildren)
	}
	groups := buildSessionRepositories([]SessionSummary{parent, child, orphan}, presenceOpenSet{})
	if len(groups) != 1 || groups[0].Total != 1 {
		t.Fatalf("rail header count must exclude native children: %#v", groups)
	}
}

// A selected native child lands on its parent's page (offset math matches
// children), mirroring selected-agent-child behavior.
func TestBuildSessionPageSelectedNativeChildLandsOnParentPage(t *testing.T) {
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	rows := make([]SessionSummary, 0, 21)
	for i := 0; i < 20; i++ {
		rows = append(rows, sessionFixture("/repo", "codex", string(rune('a'+i)), base.Add(-time.Duration(i)*time.Minute)))
	}
	// parent is row "a"; its native child is older, so it would page elsewhere.
	rows[0].ThreadID = "thread-a"
	child := nativeChildFixture("/repo", "codex", "native-x", "thread-a", "guardian", "native-subagent", base.Add(-30*time.Minute))
	rows = append(rows, child)
	page, found := buildSessionPage(rows, presenceOpenSet{}, "/repo", "all", 0, 15, "codex", "native-x")
	if !found || page.Offset != 0 || page.Total != 20 {
		t.Fatalf("selected native child did not choose the parent's page: %#v", page)
	}
	if len(page.NativeChildren["a"]) != 1 || page.NativeChildren["a"][0].ID != "native-x" {
		t.Fatalf("selected native child missing from the parent's fold: %#v", page.NativeChildren)
	}
}

// PW2: two sibling children share the parent's thread id (codex children carry
// their parent's thread id). When the parent row is absent from the scan, a
// child's parent_id must never resolve to its sibling's ThreadID — both must
// resolve to orphans, never to each other.
func TestBuildSessionPageSiblingChildrenDoNotShadowAbsentParent(t *testing.T) {
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	shared := "thread-parent"
	childA := nativeChildFixture("/repo", "codex", "child-a", shared, "guardian", "native-subagent", base.Add(-time.Minute))
	childA.MetaID = "meta-a"
	childB := nativeChildFixture("/repo", "codex", "child-b", shared, "guardian", "native-subagent", base.Add(-2*time.Minute))
	childB.MetaID = "meta-b"
	// Both children carry the parent's thread id; the parent row is absent.
	page, found := buildSessionPage([]SessionSummary{childA, childB}, presenceOpenSet{}, "/repo", "all", 0, 15, "", "")
	if !found || page.Total != 0 || len(page.Sessions) != 0 {
		t.Fatalf("sibling children must not fold under each other when the parent is absent: %#v", page)
	}
	if page.NativeChildren != nil {
		t.Fatalf("no native fold should exist without a top-level parent: %#v", page.NativeChildren)
	}
}
