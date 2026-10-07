package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

const (
	relatedParentID   = "eeeeeeee-7777-7777-7777-777777777777"
	relatedBackground = "a1111111111111111" // depth 1, launched in the background
	relatedForeground = "a2222222222222222" // depth 1, no launch record
	relatedNested     = "a3333333333333333" // depth 2, launched by relatedBackground
)

// writeRelatedClaudeFixture builds a Claude parent under home with three
// subagents: a background agent the parent transcript also records as
// async-launched (the source of the double listing), a foreground agent, and a
// nested agent whose sidecar names the background agent as its launcher. The
// parent also messages the background agent.
func writeRelatedClaudeFixture(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-work-repo")
	subagents := filepath.Join(dir, relatedParentID, "subagents")
	if err := os.MkdirAll(subagents, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","uuid":"rec-1","sessionId":"` + relatedParentID + `","cwd":"/work/repo","message":{"role":"user","content":"go"}}` + "\n" +
		`{"type":"user","uuid":"rec-2","sessionId":"` + relatedParentID + `","toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"` + relatedBackground + `"},"message":{"role":"user","content":[{"type":"tool_result","content":"launched"}]}}` + "\n" +
		`{"type":"assistant","uuid":"rec-3","sessionId":"` + relatedParentID + `","message":{"role":"assistant","content":[{"type":"tool_use","name":"SendMessage","input":{"to":"` + relatedBackground + `","summary":"status?"}}]}}` + "\n"
	path := filepath.Join(dir, relatedParentID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecars := map[string]string{
		relatedBackground: `{"agentType":"Explore","description":"map the flow","spawnDepth":1}`,
		relatedForeground: `{"agentType":"general-purpose","description":"write the tests","spawnDepth":1}`,
		relatedNested:     `{"agentType":"general-purpose","description":"read one file","spawnDepth":2,"parentAgentId":"` + relatedBackground + `"}`,
	}
	for id, sidecar := range sidecars {
		transcript := `{"type":"user","isSidechain":true,"sessionId":"` + relatedParentID + `","agentId":"` + id + `","message":{"role":"user","content":"work"}}` + "\n"
		if err := os.WriteFile(filepath.Join(subagents, "agent-"+id+".jsonl"), []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(subagents, "agent-"+id+".meta.json"), []byte(sidecar), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func observedRowsFor(t *testing.T, runtime, path string) []relatedSession {
	t.Helper()
	summary, ok := harvest.SummarizeFile(runtime, path)
	if !ok {
		t.Fatalf("SummarizeFile(%s) failed", path)
	}
	facts, _ := harvest.Lineage(summary)
	edges, _, err := harvest.Edges(summary)
	if err != nil {
		t.Fatal(err)
	}
	self := relatedSelfIDs(relatedIdentityAlternates(summary.ID, summary, true), summary, true)
	return observedRelatedRows(summary.Runtime, self, facts, edges)
}

// TestObservedRelatedRowsListEachSubagentOnce: a background agent stated by
// both the subagents directory and the parent's launch record is one row that
// keeps the sidecar role and gains the launch anchor; a nested agent sits
// under its launcher, never as a direct child; no agent id is a link.
func TestObservedRelatedRowsListEachSubagentOnce(t *testing.T) {
	path := writeRelatedClaudeFixture(t, t.TempDir())
	rows := observedRowsFor(t, "claude", path)

	spawned := map[string]relatedSession{}
	var messaged []relatedSession
	for _, row := range rows {
		switch row.Kind {
		case harvest.EdgeKindSpawned:
			if _, dup := spawned[row.SessionID]; dup {
				t.Fatalf("subagent %s listed twice: %+v", row.SessionID, rows)
			}
			spawned[row.SessionID] = row
		case harvest.EdgeKindMessaged:
			messaged = append(messaged, row)
		}
		if row.Openable {
			t.Fatalf("an agent id is not a session, yet the row is openable: %+v", row)
		}
		if row.SessionID == relatedParentID {
			t.Fatalf("a row names the open session itself: %+v", row)
		}
	}
	if len(spawned) != 3 {
		t.Fatalf("spawned rows = %d, want one per subagents file (3): %+v", len(spawned), rows)
	}
	background := spawned[relatedBackground]
	if background.Direction != "child" || background.Role != "Explore" || background.Description != "map the flow" ||
		background.Anchor != "rec-2" || background.Runtime != "claude" {
		t.Fatalf("background agent row = %+v", background)
	}
	if foreground := spawned[relatedForeground]; foreground.Direction != "child" || foreground.Anchor != "" {
		t.Fatalf("foreground agent row = %+v", foreground)
	}
	if nested := spawned[relatedNested]; nested.Direction != "descendant" || nested.Via != relatedBackground {
		t.Fatalf("nested agent row = %+v, want a descendant via its launcher", nested)
	}
	if len(messaged) != 1 || messaged[0].SessionID != relatedBackground || messaged[0].Direction != "peer" {
		t.Fatalf("messaged rows = %+v", messaged)
	}
}

// TestObservedRelatedRowsCodexParentNeverNamesItself: a codex rollout's
// catalog id is its file stem, but its edges are written under the thread id.
// A targetless wait is the session's own, and a list_threads read keeps its
// raw, unresolved label — neither becomes a link back to the open session.
func TestObservedRelatedRowsCodexParentNeverNamesItself(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const thread = "aaaaaaaa-1111-1111-1111-111111111111"
	const child = "bbbbbbbb-2222-2222-2222-222222222222"
	body := `{"timestamp":"2026-08-29T10:00:00Z","type":"session_meta","payload":{"session_id":"` + thread + `","id":"` + thread + `","cwd":"/work/repo"}}` + "\n" +
		`{"timestamp":"2026-08-29T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-1","item":{"type":"SubAgentActivity","kind":"started","agent_thread_id":"` + child + `"}}}` + "\n" +
		`{"timestamp":"2026-08-29T10:00:05Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-2","item":{"type":"CollabAgentToolCall","tool":"wait","status":"completed","sender_thread_id":"` + thread + `","receiver_thread_ids":[],"agents_states":{}}}}` + "\n" +
		`{"timestamp":"2026-08-29T10:00:07Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-3","item":{"type":"DynamicToolCall","namespace":"codex_app","tool":"list_threads","status":"completed"}}}` + "\n"
	path := filepath.Join(t.TempDir(), "rollout-2026-08-29T10-00-00-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := observedRowsFor(t, "codex", path)
	byKind := map[string]relatedSession{}
	for _, row := range rows {
		if row.SessionID == thread {
			t.Fatalf("a row names the open session's own thread: %+v", row)
		}
		byKind[row.Kind] = row
	}
	if spawned := byKind[harvest.EdgeKindSpawned]; spawned.SessionID != child || spawned.Direction != "child" || !spawned.Openable {
		t.Fatalf("spawned row = %+v, want an openable child", spawned)
	}
	if wait := byKind[harvest.EdgeKindWaitedOn]; wait.Direction != "self" || wait.SessionID != "" || wait.Openable {
		t.Fatalf("targetless wait = %+v, want the session's own, not a link", wait)
	}
	if read := byKind[harvest.EdgeKindReadContextOf]; read.Label != "list_threads" || !read.Unresolved || read.Openable {
		t.Fatalf("list_threads row = %+v, want its raw unresolved label", read)
	}
}

// TestObservedRelatedRowsOpenCodeChildHasOneParent: OpenCode states a child's
// parent through lineage AND as a spawned edge; one relationship, one row.
func TestObservedRelatedRowsOpenCodeChildHasOneParent(t *testing.T) {
	summary := harvest.SessionSummary{Runtime: "opencode", ID: "ses_child", ParentID: "ses_parent"}
	facts, ok := harvest.Lineage(summary)
	if !ok {
		t.Fatal("no lineage for an OpenCode child")
	}
	edges, _, err := harvest.Edges(summary)
	if err != nil || len(edges) == 0 {
		t.Fatalf("edges = %+v err=%v", edges, err)
	}
	rows := observedRelatedRows(summary.Runtime, map[string]bool{"ses_child": true}, facts, edges)
	if len(rows) != 1 || rows[0].Direction != "parent" || rows[0].SessionID != "ses_parent" || !rows[0].Openable {
		t.Fatalf("rows = %+v, want one openable parent row", rows)
	}
}

// TestCausedRelatedRowsOneRowPerSession: many runs into one helper session are
// one row carrying the newest state and the run and reply counts; a run that
// resumed the open session is a self row; a run whose session is not known yet
// stays its own row; nothing without a runtime or id is a link.
func TestCausedRelatedRowsOneRowPerSession(t *testing.T) {
	const source, helper = "11111111-aaaa-aaaa-aaaa-111111111111", "22222222-bbbb-bbbb-bbbb-222222222222"
	run := func(id string, updated int64, child, childRuntime, state, reply string) store.CausedRelation {
		return store.CausedRelation{RelationshipID: "rel-" + id, RunID: "run-" + id, Role: "helper", State: state,
			ParentSessionID: source, ParentRuntime: "claude", ChildSessionID: child, ChildRuntime: childRuntime,
			ReplyTaskID: reply, UpdatedAt: updated}
	}
	relations := []store.CausedRelation{
		run("1", 10, helper, "codex", "completed", ""),
		run("3", 30, helper, "codex", "running", ""),
		run("2", 20, helper, "codex", "completed", "task-reply"),
		run("4", 40, source, "claude", "completed", ""), // a reply run resumed the source itself
		run("5", 50, source, "claude", "completed", ""),
		run("6", 60, "", "", "admitted", ""), // child task has no session yet
		run("7", 70, "", "", "admitted", ""),
	}
	rows := causedRelatedRows(relations, map[string]bool{source: true})
	if len(rows) != 4 {
		t.Fatalf("rows = %+v, want helper, self and two pending rows", rows)
	}
	var helperRow, selfRow relatedSession
	pending := 0
	for _, row := range rows {
		switch {
		case row.SessionID == helper:
			helperRow = row
		case row.Direction == "self":
			selfRow = row
		case row.SessionID == "":
			pending++
			if row.Openable || row.Kind != "reviewed-by" || row.Direction != "child" || row.Runs != 1 {
				t.Fatalf("pending row = %+v", row)
			}
		default:
			t.Fatalf("unexpected row %+v", row)
		}
		if row.SessionID == source {
			t.Fatalf("a caused row names the open session: %+v", row)
		}
	}
	if helperRow.Kind != "reviewed-by" || helperRow.Runs != 3 || helperRow.Replies != 1 ||
		helperRow.State != "running" || helperRow.RunID != "run-3" || !helperRow.Openable {
		t.Fatalf("helper row = %+v, want 3 runs, 1 reply, newest state running", helperRow)
	}
	if selfRow.Kind != "resumed-by" || selfRow.Runs != 2 || selfRow.Openable {
		t.Fatalf("self row = %+v", selfRow)
	}
	if pending != 2 {
		t.Fatalf("pending rows = %d, want one per run", pending)
	}

	// From the helper's side the source is one parent row, whatever mix of
	// claim and reply runs produced it.
	fromHelper := causedRelatedRows(relations[:3], map[string]bool{helper: true})
	if len(fromHelper) != 1 || fromHelper[0].Kind != "reviews" || fromHelper[0].Direction != "parent" ||
		fromHelper[0].SessionID != source || fromHelper[0].Runs != 3 || fromHelper[0].Replies != 1 {
		t.Fatalf("rows from the helper = %+v", fromHelper)
	}
}

// TestSessionRelatedEndpointOnAFakeHome drives the route: each subagent once,
// no links for agent ids, and an honest note when the ledger cannot answer.
func TestSessionRelatedEndpointOnAFakeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRelatedClaudeFixture(t, home)
	prior := governor
	governor = nil
	t.Cleanup(func() { governor = prior })

	recorder := httptest.NewRecorder()
	handleSessionRelated(recorder, httptest.NewRequest(http.MethodGet,
		"/api/session/related?runtime=claude&id="+relatedParentID, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response relatedSessionsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	for _, row := range response.Related {
		if row.Kind == harvest.EdgeKindSpawned {
			spawned++
		}
		if row.Openable {
			t.Fatalf("openable row for an agent id: %+v", row)
		}
	}
	if spawned != 3 {
		t.Fatalf("spawned rows = %d, want 3: %+v", spawned, response.Related)
	}
	if !strings.Contains(response.Coverage, "orchestration store is not open") {
		t.Fatalf("coverage = %q, want the unavailable ledger named", response.Coverage)
	}
	if !strings.Contains(recorder.Body.String(), `"openable":false`) {
		t.Fatal("openable must always be present on the wire")
	}
}

// TestSessionRelatedKeepsEveryNote: a missing session and an unavailable
// ledger are both stated; neither hides the other.
func TestSessionRelatedKeepsEveryNote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	prior := governor
	governor = nil
	t.Cleanup(func() { governor = prior })
	recorder := httptest.NewRecorder()
	handleSessionRelated(recorder, httptest.NewRequest(http.MethodGet,
		"/api/session/related?runtime=claude&id=99999999-9999-9999-9999-999999999999", nil))
	var response relatedSessionsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Coverage, "session not found") || !strings.Contains(response.Coverage, "orchestration store is not open") {
		t.Fatalf("coverage = %q, want both notes", response.Coverage)
	}
}

// TestCollectCausedRelationsCountsARunOnce: a relationship the ledger returns
// under two identity alternates is one run; a read that stopped at its limit
// is stated; a failed read yields no rows at all, only the note.
func TestCollectCausedRelationsCountsARunOnce(t *testing.T) {
	shared := store.CausedRelation{RelationshipID: "rel-shared", ChildSessionID: "s", ChildRuntime: "codex", Role: "helper"}
	other := store.CausedRelation{RelationshipID: "rel-other", ChildSessionID: "s", ChildRuntime: "codex", Role: "helper"}
	batches := map[string][]store.CausedRelation{"a": {shared}, "b": {shared, other}}
	read := func(id string, _ int) ([]store.CausedRelation, error) { return batches[id], nil }
	relations, notes := collectCausedRelations(read, []string{"a", "b"})
	if len(relations) != 2 || len(notes) != 0 {
		t.Fatalf("relations = %+v notes=%v, want 2 and no note", relations, notes)
	}
	rows := causedRelatedRows(relations, map[string]bool{"p": true})
	if len(rows) != 1 || rows[0].Runs != 2 {
		t.Fatalf("rows = %+v, want one row of 2 runs", rows)
	}

	full := make([]store.CausedRelation, causedRelationsReadLimit)
	for i := range full {
		full[i] = store.CausedRelation{RelationshipID: "rel-" + strconv.Itoa(i)}
	}
	_, notes = collectCausedRelations(func(string, int) ([]store.CausedRelation, error) { return full, nil }, []string{"a"})
	if len(notes) != 1 || !strings.Contains(notes[0], "only the newest 100 per identity were read") {
		t.Fatalf("truncation notes = %v", notes)
	}

	calls := 0
	failing := func(id string, _ int) ([]store.CausedRelation, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("database is locked")
		}
		return batches[id], nil
	}
	relations, notes = collectCausedRelations(failing, []string{"a", "b"})
	if relations != nil || len(notes) != 1 || !strings.Contains(notes[0], "caused relations unavailable: database is locked") {
		t.Fatalf("failed read = %+v notes=%v, want no rows and the failure named", relations, notes)
	}
}

// TestCodexChildNeverAnswersToItsParentsThread: a codex subagent rollout
// carries its parent's thread id, which names the parent. It is not one of the
// child's identities, so the parent is a parent row and never "self".
func TestCodexChildNeverAnswersToItsParentsThread(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const parent = "aaaaaaaa-1111-1111-1111-111111111111"
	const child = "bbbbbbbb-2222-2222-2222-222222222222"
	body := `{"timestamp":"2026-08-29T10:01:00Z","type":"session_meta","payload":{"session_id":"` + parent +
		`","id":"` + child + `","parent_thread_id":"` + parent + `","cwd":"/work/repo",` +
		`"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `","depth":1,"agent_role":"reviewer"}}}}}` + "\n"
	path := filepath.Join(t.TempDir(), "rollout-2026-08-29T10-01-00-"+child+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, ok := harvest.SummarizeFile("codex", path)
	if !ok || summary.ThreadID != parent {
		t.Fatalf("fixture summary = %+v, want a child carrying the parent's thread id", summary)
	}
	alternates := relatedIdentityAlternates(summary.ID, summary, true)
	self := relatedSelfIDs(alternates, summary, true)
	if containsString(alternates, parent) || self[parent] {
		t.Fatalf("alternates=%v self=%v include the parent's thread", alternates, self)
	}
	if !self[child] {
		t.Fatalf("self = %v, want the child's own id", self)
	}
	rows := observedRowsFor(t, "codex", path)
	if len(rows) != 1 || rows[0].Direction != "parent" || rows[0].SessionID != parent || !rows[0].Openable {
		t.Fatalf("rows = %+v, want one openable parent row", rows)
	}
	// A reply run on the parent thread (child task = parent session) is the
	// parent's own affair, never a "resumed-by · self" row on the child.
	caused := causedRelatedRows([]store.CausedRelation{{RelationshipID: "rel-r", Role: "helper",
		ParentSessionID: parent, ParentRuntime: "codex", ChildSessionID: parent, ChildRuntime: "codex"}}, self)
	if len(caused) != 1 || caused[0].Direction == "self" {
		t.Fatalf("caused rows = %+v, want no self row on the child", caused)
	}
}

// TestCausedRowFallbackAndRuntimeLessIDs: a relation neither endpoint of which
// is this session keeps the historical orientation, and an id without a
// runtime is never a link.
func TestCausedRowFallbackAndRuntimeLessIDs(t *testing.T) {
	self := map[string]bool{"me": true}
	child := causedRow(store.CausedRelation{ParentSessionID: "p", ParentRuntime: "codex",
		ChildSessionID: "22222222-bbbb-bbbb-bbbb-222222222222", ChildRuntime: "codex"}, self)
	if child.Kind != "reviewed-by" || child.Direction != "child" || !child.Openable {
		t.Fatalf("fallback with a child = %+v", child)
	}
	parent := causedRow(store.CausedRelation{ParentSessionID: "11111111-aaaa-aaaa-aaaa-111111111111", ParentRuntime: "codex"}, self)
	if parent.Kind != "reviews" || parent.Direction != "parent" || !parent.Openable {
		t.Fatalf("fallback without a child = %+v", parent)
	}
	runtimeless := causedRow(store.CausedRelation{ParentSessionID: "me",
		ChildSessionID: "22222222-bbbb-bbbb-bbbb-222222222222"}, self)
	if runtimeless.SessionID == "" || runtimeless.Openable {
		t.Fatalf("a child without a runtime = %+v, want listed but not openable", runtimeless)
	}
}
