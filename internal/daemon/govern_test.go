package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/store"
)

func has(rows []store.StateRow, key, value string) bool {
	for _, r := range rows {
		if r.Key == key && r.Value == value {
			return true
		}
	}
	return false
}

// TestGovernorObserveFold drives the Phase-1a pipeline end-to-end against the shipped
// detection library: three real-shaped actions → frozen events + a folded session and
// entity state. Hermetic (no ~/.crossing-guard dependency).
func TestGovernorObserveFold(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("") // embedded standard library
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)
	sid := "claude/s1"

	// 1) read a secrets file  2) git commit  3) fetch an external url
	for _, o := range []Observation{
		{SessionID: sid, Tool: "Read", FilePath: "/repo/.env", TS: 1},
		{SessionID: sid, Tool: "Bash", Command: "git commit -m fix", TS: 2},
		{SessionID: sid, Tool: "WebFetch", URL: "https://evil.example.com/x", TS: 3},
	} {
		if err := g.Observe(o); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}

	// Session accumulates its behavior AND the resource facts it touched.
	ss, err := ix.SessionState(sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][2]string{
		{"fs", "read"}, {"area", "secrets"}, // read the .env
		{"vcs", "commit"}, {"exec", "run"}, // git commit
		{"net", "fetch"}, {"destination-class", "external"}, // egress
	} {
		if !has(ss, want[0], want[1]) {
			t.Errorf("session state missing %s=%s; got %v", want[0], want[1], ss)
		}
	}

	// The FILE entity carries the resource fact (area=secrets), not the behavior tags.
	fileID := engine.EntityID("file", "/repo/.env")
	es, err := ix.EntityState(fileID)
	if err != nil {
		t.Fatal(err)
	}
	if !has(es, "area", "secrets") {
		t.Errorf("file entity missing area=secrets; got %v", es)
	}
	if has(es, "vcs", "commit") {
		t.Errorf("behavior tag vcs=commit leaked onto the file entity")
	}

	// The URL entity carries destination-class=external.
	urlID := engine.EntityID("url", "https://evil.example.com/x")
	ue, err := ix.EntityState(urlID)
	if err != nil {
		t.Fatal(err)
	}
	if !has(ue, "destination-class", "external") {
		t.Errorf("url entity missing destination-class=external; got %v", ue)
	}
}

func TestNaturalCodexSummaryJoinsCanonicalGovernanceEvents(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old })
	canonical := "abcdef00-aaaa-7bbb-8ccc-000000000004"
	if err := governor.Observe(Observation{SessionID: canonical, Runtime: "codex", Tool: "apply_patch",
		FilePaths: []string{"/repo/a.go", "/repo/b.go"}, TS: 1}); err != nil {
		t.Fatal(err)
	}
	s := &SessionSummary{Runtime: "codex", ID: "rollout-2026-08-11T08-07-12-" + canonical, ThreadID: canonical}
	facts := liveFootprint(s)
	if facts == nil || facts.ToolCalls != 1 || len(facts.Files) != 2 {
		t.Fatalf("canonical footprint = %+v", facts)
	}
	if facts.Files[0] != "/repo/a.go" || facts.Files[1] != "/repo/b.go" {
		t.Fatalf("canonical targets = %#v", facts.Files)
	}
}

func TestStructuredAbsoluteTargetSupersedesRelativeLegacyTarget(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := NewGovernor(ix, nil)
	if err := g.Observe(Observation{SessionID: "relative", Tool: "Write", FilePath: "a.go",
		FilePaths: []string{"/repo/a.go"}, TS: 1}); err != nil {
		t.Fatal(err)
	}
	events, err := ix.EventsForSession("relative", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TargetEntityID != "file:/repo/a.go" {
		t.Fatalf("primary target = %+v", events)
	}
	touches, err := ix.SessionFileTouches("relative", 10)
	if err != nil {
		t.Fatal(err)
	}
	if touches.DistinctFiles != 1 || len(touches.Touches) != 1 || touches.Touches[0].Identity != "/repo/a.go" {
		t.Fatalf("relative target duplicated: %+v", touches)
	}
}

func TestSessionReportTotalIsNotThePayloadCap(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := NewGovernor(ix, nil)
	old := governor
	governor = g
	t.Cleanup(func() { governor = old })
	for i := 0; i < 3; i++ {
		if err := g.Observe(Observation{SessionID: "capped", Tool: "Bash", TS: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := g.SessionReport("capped", 2)
	if err != nil {
		t.Fatal(err)
	}
	if rep.EventCount != 3 || len(rep.Events) != 2 || !rep.Truncated {
		t.Fatalf("report count/cap = count %d rows %d truncated %v", rep.EventCount, len(rep.Events), rep.Truncated)
	}
	s := &SessionSummary{Runtime: "unknown", ID: "capped"}
	facts := liveFootprint(s)
	if facts == nil || facts.ToolCalls != 3 || facts.AsOf != 3 {
		t.Fatalf("facts aggregate = %+v", facts)
	}
}

func TestSessionReportReturnsOrderedPluralResources(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(store.EventRecord{TS: 10, SessionID: "plural", Runtime: "codex", Verb: "write", Tool: "apply_patch", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for ordinal, path := range []string{"/repo/a.go", "/repo/b.go"} {
		entityID := "file:" + path
		if err := tx.UpsertEntity(entityID, "file", path, 10); err != nil {
			t.Fatal(err)
		}
		if err := tx.AppendEventResource(eventID, ordinal, entityID, "tool_input.command"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rep, err := NewGovernor(ix, nil).SessionReport("plural", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Events) != 1 || rep.Events[0].ID != eventID || rep.ResourceCount != 2 || rep.ResourcesTruncated || len(rep.Resources) != 2 || rep.Resources[0].Identity != "/repo/a.go" || rep.Resources[1].Identity != "/repo/b.go" {
		t.Fatalf("plural-resource session contract = %+v", rep)
	}
}

// TestEntityReportIncludesCurrentPluralResources locks the current observation model:
// an action may carry its only file target in event_resource. Cross-session backlinks
// must not depend on the legacy event.target_entity_id compatibility column.
func TestEntityReportIncludesCurrentPluralResources(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	const path = "/repo/current-resource.go"
	entityID := engine.EntityID("file", path)
	eventID, err := tx.AppendEvent(store.EventRecord{TS: 10, SessionID: "plural-resource",
		Runtime: "fixture", Verb: "write", Tool: "patch", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.UpsertEntity(entityID, "file", path, 10); err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventResource(eventID, 0, entityID, "tool_input.path"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rep, err := NewGovernor(ix, nil).EntityReport(entityID, entityEventCap)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Found || rep.Events != 1 || len(rep.Touches) != 1 ||
		rep.Touches[0].SessionID != "plural-resource" {
		t.Fatalf("current plural resource missing from entity report: %+v", rep)
	}
}

// TestGovernorReplayReal replays real harvested sessions' tool calls through the
// governor and confirms state accumulates — the plan's "run a real session, watch
// state accumulate" check, run offline over history. Set CG_REPLAY_REAL=1.
func TestGovernorReplayReal(t *testing.T) {
	if os.Getenv("CG_REPLAY_REAL") == "" {
		t.Skip("set CG_REPLAY_REAL=1 to replay real harvested sessions")
	}
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, _ := engine.LoadLayered("")
	g := NewGovernor(ix, dets)

	shown := 0
	for _, s := range harvest.ScanSessions() {
		events, _, _, err := harvest.Normalize(s.Runtime, s.Path)
		if err != nil {
			continue
		}
		sid := s.Runtime + "/" + s.ID
		for _, ce := range events {
			if ce.Kind != "tool_call" {
				continue
			}
			cmd, path, url := extractToolInput(ce.Text)
			_ = g.Observe(Observation{SessionID: sid, Tool: ce.Name, Command: cmd, FilePath: path, URL: url, TS: int64(ce.Seq)})
		}
		ss, _ := ix.SessionState(sid)
		if len(ss) > 0 && shown < 5 {
			t.Logf("session %s → %d distinct state facts", s.ID, len(ss))
			shown++
		}
	}
	if shown == 0 {
		t.Fatal("no session produced any state — pipeline broken on real data")
	}
}

// TestObserveRecordsTheDecision pins D19. A blocked call never reaches the vendor
// transcript, so the event log is the ONLY possible record that enforcement fired.
// Before the fix the hook sent its observation BEFORE deciding, so every event
// carried an empty decision and a blocked action left no trace on the machine.
func TestObserveRecordsTheDecision(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	for _, o := range []Observation{
		{SessionID: "claude/s-dec", Tool: "Bash", Command: "git push --force origin main",
			TS: 10, Decision: "deny", Reason: "Blocked by rule git-force-push"},
		{SessionID: "claude/s-dec", Tool: "Read", FilePath: "/repo/README.md",
			TS: 11, Decision: "allow", Reason: "no rule matched"},
	} {
		if err := g.Observe(o); err != nil {
			t.Fatal(err)
		}
	}

	evs, err := ix.EventsForSession("claude/s-dec", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if evs[0].Decision != "deny" {
		t.Fatalf("blocked action recorded with decision=%q — enforcement left no trace", evs[0].Decision)
	}
	if evs[0].Reason == "" {
		t.Fatal("decision recorded without a reason: unauditable")
	}
	if evs[1].Decision != "allow" {
		t.Fatalf("allowed action recorded with decision=%q", evs[1].Decision)
	}
}

// TestEntityReportAnswersWhichSessionTouchedIt pins D4 — the query the governance
// model exists for, and which was unanswerable because the event and entity tables
// had no reader at all.
func TestEntityReportAnswersWhichSessionTouchedIt(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	const doc = "/repo/docs/console-design.md"
	for _, o := range []Observation{
		{SessionID: "sess-A", Tool: "Read", FilePath: doc, TS: 10, Decision: "allow"},
		{SessionID: "sess-B", Tool: "Edit", FilePath: doc, TS: 20, Decision: "allow", Content: "x"},
		{SessionID: "sess-B", Tool: "Read", FilePath: doc, TS: 30, Decision: "allow"},
		{SessionID: "sess-C", Tool: "Read", FilePath: "/repo/other.md", TS: 40, Decision: "allow"},
	} {
		if err := g.Observe(o); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := g.EntityReport(engine.EntityID("file", doc), entityEventCap)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Found || rep.Events != 3 {
		t.Fatalf("found=%v events=%d, want true/3", rep.Found, rep.Events)
	}
	if len(rep.Touches) != 2 {
		t.Fatalf("want 2 sessions touching the doc, got %d", len(rep.Touches))
	}
	// sess-B both edited and read it; the untouched session must not appear.
	var b *EntityTouch
	for i := range rep.Touches {
		if rep.Touches[i].SessionID == "sess-C" {
			t.Fatal("a session that never touched the file was reported")
		}
		if rep.Touches[i].SessionID == "sess-B" {
			b = &rep.Touches[i]
		}
	}
	if b == nil || b.Events != 2 {
		t.Fatalf("sess-B not summarized correctly: %+v", b)
	}
	if len(b.Verbs) != 2 { // write + read
		t.Fatalf("verbs not distinct-collected: %v", b.Verbs)
	}

	// An unobserved resource must SAY it is unknown, not return a blank list.
	miss, err := g.EntityReport(engine.EntityID("file", "/repo/never.md"), entityEventCap)
	if err != nil {
		t.Fatal(err)
	}
	if miss.Found || miss.Note == "" {
		t.Fatalf("unobserved resource returned a silent empty answer: %+v", miss)
	}
}

// TestUnrecordedDecisionsAreLabelledUnknown: events captured before decisions were
// recorded must never read as "allowed". The log cannot be backfilled.
func TestUnrecordedDecisionsAreLabelledUnknown(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, _ := engine.LoadLayered("")
	g := NewGovernor(ix, dets)
	if err := g.Observe(Observation{SessionID: "old", Tool: "Read",
		FilePath: "/repo/legacy.md", TS: 1}); err != nil { // no Decision: pre-D19 shape
		t.Fatal(err)
	}
	rep, err := g.EntityReport(engine.EntityID("file", "/repo/legacy.md"), entityEventCap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Note, "UNKNOWN") {
		t.Fatalf("absent decisions not labelled unknown: note=%q", rep.Note)
	}
}

// TestLoadSessionDeclaresSiblingSegments pins the codex half of D7, which the
// "misfiled, it is resume not duplication" claim got wrong. A codex thread resumed
// across rollouts is ONE conversation in several files (~9% of codex sessions) and
// MatchID resolves the thread id to all of them. Returning whichever the scan
// yielded first showed one segment as if it were the whole thread — silent,
// non-deterministic loss, which is worse than double-counting.
func TestLoadSessionDeclaresSiblingSegments(t *testing.T) {
	// Drive the real resolution logic over a synthetic two-segment thread.
	thread := "abcdef00-aaaa-7bbb-8ccc-000000000002"
	older := SessionSummary{Runtime: "codex", ID: "rollout-A-" + thread, ThreadID: thread,
		Modified: time.Unix(1000, 0), Lines: 665}
	newer := SessionSummary{Runtime: "codex", ID: "rollout-B-" + thread, ThreadID: thread,
		Modified: time.Unix(2000, 0), Lines: 220}

	var matches []SessionSummary
	for _, s := range []SessionSummary{older, newer} {
		if s.Runtime == "codex" && harvest.MatchID(s, thread) {
			matches = append(matches, s)
		}
	}
	if len(matches) != 2 {
		t.Fatalf("thread id must resolve to BOTH segments, got %d", len(matches))
	}
	primary := 0
	for i := range matches {
		if matches[i].Modified.After(matches[primary].Modified) {
			primary = i
		}
	}
	if matches[primary].ID != newer.ID {
		t.Fatalf("primary segment is not the newest: %s", matches[primary].ID)
	}
	// The other segment must be reportable, not dropped.
	var segs []SessionRef
	for i, m := range matches {
		if i != primary {
			segs = append(segs, SessionRef{Runtime: m.Runtime, ID: m.ID, Lines: m.Lines})
		}
	}
	if len(segs) != 1 || segs[0].ID != older.ID {
		t.Fatalf("sibling segment not declared: %+v", segs)
	}
}

// TestFootprintComesFromOurOwnLog replaces TestEnrichmentHonorsDataDir, which pinned
// that sessionsDB() honored --data. There is no sessionsDB() any more: the console
// footprint read an EXPERIMENT's output (experiments/sessionindex wrote sessions.db)
// and went blank when that script stopped running on 2026-07-16, with nothing failing
// and no test catching it — the coupling was invisible because it ran through DATA,
// not imports. The footprint now derives from the governance event log.
func TestFootprintComesFromOurOwnLog(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, _ := engine.LoadLayered("")
	prev := governor
	governor = NewGovernor(ix, dets)
	defer func() { governor = prev }()

	sid := "sess-footprint"
	for _, o := range []Observation{
		{SessionID: sid, Tool: "Read", FilePath: "/repo/a.md", TS: 10, Decision: "allow"},
		{SessionID: sid, Tool: "Edit", FilePath: "/repo/b.md", TS: 20, Decision: "allow", Content: "x"},
		{SessionID: sid, Tool: "Read", FilePath: "/repo/a.md", TS: 30, Decision: "allow"},
	} {
		if err := governor.Observe(o); err != nil {
			t.Fatal(err)
		}
	}

	f := FactsFor(&SessionSummary{ID: sid, Runtime: "claude"})
	if f == nil {
		t.Fatal("no footprint from the live log — the panel would say \"no recorded footprint\"")
	}
	if f.Source != "live" {
		t.Fatalf("footprint source = %q, want \"live\" (never an experiment)", f.Source)
	}
	if f.ToolCalls != 3 {
		t.Fatalf("tool calls = %d, want 3", f.ToolCalls)
	}
	if len(f.FilesCreated) != 1 || f.FilesCreated[0] != "/repo/b.md" {
		t.Fatalf("written files wrong: %v", f.FilesCreated)
	}
	if len(f.Files) != 1 || f.Files[0] != "/repo/a.md" {
		t.Fatalf("read files not deduped: %v", f.Files)
	}
	if f.AsOf != 30 {
		t.Fatalf("as_of = %d, want the newest observation (30)", f.AsOf)
	}

	// A session we never observed must yield nothing, not a fabricated empty record.
	if got := FactsFor(&SessionSummary{ID: "never", Runtime: "claude"}); got != nil {
		t.Fatalf("unobserved session got a footprint: %+v", got)
	}
}

// TestSessionReportSaysWhyItIsEmpty pins the INV-21 half of the session endpoint.
// The endpoint used to return a bare []StateRow, where "never observed" and
// "observed, no state folded" were both `[]` — indistinguishable, so no console
// view could render an honest empty state from it.
func TestSessionReportSaysWhyItIsEmpty(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	for _, o := range []Observation{
		{SessionID: "sess-A", Tool: "Read", FilePath: "/repo/a.md", TS: 10, Decision: "allow"},
		{SessionID: "sess-A", Tool: "Edit", FilePath: "/repo/a.md", TS: 20, Decision: "deny", Content: "x"},
	} {
		if err := g.Observe(o); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := g.SessionReport("sess-A", entityEventCap)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Found || rep.EventCount != 2 {
		t.Fatalf("found=%v event_count=%d, want true/2", rep.Found, rep.EventCount)
	}
	if len(rep.Events) != 2 {
		t.Fatalf("events not returned: %d", len(rep.Events))
	}
	if len(rep.State) == 0 {
		t.Fatal("folded state missing from the report")
	}
	if !rep.DecisionsRecorded {
		t.Fatal("decisions were recorded but the report says they were not")
	}
	if rep.Truncated {
		t.Fatal("2 events reported as truncated")
	}

	// A session we never observed must SAY so, not answer with a blank list.
	miss, err := g.SessionReport("sess-nobody", entityEventCap)
	if err != nil {
		t.Fatal(err)
	}
	if miss.Found || miss.Note == "" {
		t.Fatalf("unobserved session returned a silent empty answer: %+v", miss)
	}
	if miss.Events == nil || miss.State == nil {
		t.Fatal("nil slices serialize as null, not []; the client must not branch on that")
	}

	// Absent decisions are UNKNOWN, never "allowed" — every event before 00032f0
	// carries an empty decision permanently, so the report must not imply otherwise.
	if err := g.Observe(Observation{SessionID: "sess-old", Tool: "Read", FilePath: "/repo/b.md", TS: 5}); err != nil {
		t.Fatal(err)
	}
	old, err := g.SessionReport("sess-old", entityEventCap)
	if err != nil {
		t.Fatal(err)
	}
	if !old.Found {
		t.Fatal("a session with events reported as not found")
	}
	if old.DecisionsRecorded {
		t.Fatal("a session with no recorded decision claimed decisions were recorded")
	}
}

// TestGovernedSessionsListsOnlyWhatWeCaptured pins the rail's source. It must come
// from the event log, not the harvest scan: a harvested session we never observed
// has no governance rows to show and must not appear.
func TestGovernedSessionsListsOnlyWhatWeCaptured(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	for _, o := range []Observation{
		{SessionID: "old", Tool: "Read", FilePath: "/repo/a.md", TS: 10, Decision: "allow"},
		{SessionID: "new", Tool: "Read", FilePath: "/repo/b.md", TS: 90, Decision: "allow"},
		{SessionID: "new", Tool: "Read", FilePath: "/repo/c.md", TS: 95, Decision: "allow"},
	} {
		if err := g.Observe(o); err != nil {
			t.Fatal(err)
		}
	}

	list, err := g.GovernedSessions(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 governed sessions, got %d", len(list))
	}
	if list[0].SessionID != "new" { // newest first
		t.Fatalf("rail order is not newest-first: %+v", list)
	}
	if list[0].Events != 2 || list[0].LastSeen != 95 || list[0].FirstSeen != 90 {
		t.Fatalf("counts/window wrong: %+v", list[0])
	}
}

// TestGovernorHealthDetectsSilentLoss pins D13. Capture liveness must be READABLE:
// the newest event ts is the detector for every drop-shaped loss, and the persist-
// failure counter is the detector for the one loss the log cannot show (the row that
// never landed). A green console over a dead pipeline hid D2 for four days.
func TestGovernorHealthDetectsSilentLoss(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dets, err := engine.LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, dets)

	// Empty log: configured, but nothing captured yet.
	h, err := g.Health()
	if err != nil {
		t.Fatal(err)
	}
	if !h.Configured || h.TotalEvents != 0 || h.LastEventTS != 0 {
		t.Fatalf("empty-log health wrong: %+v", h)
	}
	if h.StartedAt == 0 {
		t.Fatal("started_at not anchored — 'no capture since boot' is unmeasurable")
	}
	healthJSON, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"understanding_offered":0`, `"understanding_started":0`,
		`"understanding_active":0`, `"understanding_missing_root":0`,
		`"understanding_queue_depth":0`} {
		if !strings.Contains(string(healthJSON), field) {
			t.Fatalf("idle understanding health omitted %s: %s", field, healthJSON)
		}
	}

	// Capture two events; the newest ts is the liveness signal.
	for _, o := range []Observation{
		{SessionID: "s", Tool: "Read", FilePath: "/repo/a.md", TS: 100, Decision: "allow"},
		{SessionID: "s", Tool: "Read", FilePath: "/repo/b.md", TS: 200, Decision: "allow"},
	} {
		if err := g.Observe(o); err != nil {
			t.Fatal(err)
		}
	}
	h, _ = g.Health()
	if h.TotalEvents != 2 || h.LastEventTS != 200 {
		t.Fatalf("post-capture health wrong: %+v", h)
	}

	// A persist failure is the loss the log CANNOT show — it must be counted so the
	// surface can say capture is lossy right now.
	if h.ObserveFailures != 0 {
		t.Fatalf("no failures yet, got %d", h.ObserveFailures)
	}
	g.observeFailures.Add(1)
	h, _ = g.Health()
	if h.ObserveFailures != 1 {
		t.Fatalf("persist failure not surfaced: %+v", h)
	}
}
