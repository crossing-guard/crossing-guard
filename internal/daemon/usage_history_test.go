package daemon

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/usagehistory"
	"crossing-guard/store"
)

func usageCount(value int64) *int64 { return &value }

// usageFixture is a store holding a few recorded calls, a pinned session scan
// and a recorder status.
func usageFixture(t *testing.T, scanned []SessionSummary) *store.Index {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	prior, priorRecorder := governor, daemonUsageRecorder
	governor = NewGovernor(ix, nil)
	daemonUsageRecorder = newUsageRecorderCoordinator(filepath.Join(t.TempDir(), "unused.sqlite"))
	daemonUsageRecorder.status.Store(&usageRecorderStatus{State: usageCoverageCurrent, Discovered: 3, Recorded: 3})
	sessionScanCoalescer = &scanCoalescer{result: scanned, done: time.Now().Add(time.Hour)}
	t.Cleanup(func() {
		governor, daemonUsageRecorder = prior, priorRecorder
		sessionScanCoalescer = &scanCoalescer{}
		_ = ix.Close()
	})
	day := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC).UnixMilli()
	write := func(runtime, source, session, parent string, calls ...store.UsageCallRecord) {
		t.Helper()
		for index := range calls {
			calls[index].Runtime, calls[index].Source, calls[index].Reader = runtime, source, runtime+"/usage-1"
			if calls[index].SessionID == "" {
				calls[index].SessionID = session
			}
		}
		if _, err := ix.WriteUsageSource(store.UsageSourceWrite{Calls: calls, State: store.UsageSourceState{
			Runtime: runtime, Source: source, SessionID: session, ParentSession: parent, Marker: "m",
			Reader: runtime + "/usage-1", Complete: true, UpdatedAtMS: day}}); err != nil {
			t.Fatal(err)
		}
	}
	opus := store.UsageCallRecord{CallID: "o1", FirstAtMS: day, AtMS: day, Model: "big-model", Effort: "high",
		Input: usageCount(10), CacheRead: usageCount(90), CacheWrite: usageCount(0), Output: usageCount(100),
		Reasoning: usageCount(60)}
	sonnet := store.UsageCallRecord{CallID: "s1", Agent: "agent-1", FirstAtMS: day, AtMS: day + 1000,
		Model: "small-model", Effort: "high", Input: usageCount(5), CacheRead: usageCount(45), CacheWrite: usageCount(0),
		Output: usageCount(50), Reasoning: usageCount(10)}
	other := store.UsageCallRecord{CallID: "x1", FirstAtMS: day, AtMS: day, Model: "other-model", Effort: "high",
		Input: usageCount(1), CacheRead: usageCount(0), CacheWrite: usageCount(0), Output: usageCount(3),
		Cost: &store.UsageCost{Amount: 0.25, Unit: "USD", Basis: "runtime"}}
	write("alpha", "src-main", "session-1", "", opus)
	write("alpha", "src-agent", "session-1", "", sonnet)
	write("beta", "src-beta", "session-gone", "", other)
	return ix
}

func TestUsageReportIsBuiltFromRecordedCalls(t *testing.T) {
	usageFixture(t, []SessionSummary{{Runtime: "alpha", ID: "session-1", Title: "Present session"}})
	w := httptest.NewRecorder()
	handleUsageReport(w, httptest.NewRequest("GET", "/api/usage", nil))
	var report UsageReportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Coverage.State != usageCoverageCurrent || report.Coverage.SessionsWithoutSource != 1 {
		t.Fatalf("coverage: %+v", report.Coverage)
	}
	if report.Totals.Calls != 3 || report.Totals.OutputTokens != 153 || *report.Totals.Reasoning != 70 ||
		report.Totals.ReasoningStatedCalls != 2 || report.Sessions == nil || *report.Sessions != 2 {
		t.Fatalf("totals: %+v sessions=%v", report.Totals, report.Sessions)
	}
	if report.ByRuntime["alpha"].Calls != 2 || report.Days["2026-09-22"].Calls != 3 ||
		report.DaysByRuntime["beta"]["2026-09-22"].Output != 3 {
		t.Fatalf("groups: %+v %+v", report.ByRuntime, report.Days)
	}
	if len(report.Cost) != 1 || report.Cost[0].Amount != 0.25 || report.CostCoverage.WithCost != 1 ||
		report.CostCoverage.WithoutCost != 2 {
		t.Fatalf("cost: %+v %+v", report.Cost, report.CostCoverage)
	}
	rows := map[string]UsageSessionRow{}
	for _, row := range report.TopTotal {
		rows[row.ID] = row
	}
	if one := rows["session-1"]; !one.SourcePresent || one.Title != "Present session" || one.Total != 300 ||
		one.Main.Total != 200 || one.Subagent.Total != 100 || one.Subagents != 1 || one.Agent.Calls != 0 {
		t.Fatalf("a root row holds its own calls as main and its subagents' beside them: %+v", one)
	}
	if report.Work["main"].Calls != 2 || report.Work["subagent"].Calls != 1 || report.Coverage.Agents != usageAgentsCurrent {
		t.Fatalf("work split: %+v agents=%s", report.Work, report.Coverage.Agents)
	}
	if gone := rows["session-gone"]; gone.SourcePresent || gone.Title != "" || gone.Total != 4 {
		t.Fatalf("a removed session keeps its figures and no label: %+v", gone)
	}
}

// Presence comes from the recorder's listing: a runtime whose listing failed
// keeps its sessions present, and a session its runtime no longer lists is
// without source (code red-team C-9).
func TestUsageSourcePresenceComesFromTheRecordersListing(t *testing.T) {
	usageFixture(t, nil) // the session scan finds nothing at all
	daemonUsageRecorder.status.Store(&usageRecorderStatus{State: usageCoverageCurrent,
		Present: map[string]map[string]bool{"alpha": {"session-1": true}}, ListingFailed: map[string]bool{"beta": true}})
	w := httptest.NewRecorder()
	handleUsageReport(w, httptest.NewRequest("GET", "/api/usage", nil))
	var report UsageReportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	for _, row := range report.TopTotal {
		if !row.SourcePresent {
			t.Fatalf("%s: listed or unknown sources are present: %+v", row.ID, row)
		}
	}
	if report.Coverage.SessionsWithoutSource != 0 {
		t.Fatalf("a failed listing is not a removed source: %+v", report.Coverage)
	}
	if report.Totals.CacheHitRate == nil || *report.Totals.CacheHitRate <= 0 {
		t.Fatalf("hit rate: %+v", report.Totals)
	}
}

func TestUsageBreakdownGroupsAndRatios(t *testing.T) {
	usageFixture(t, nil)
	w := httptest.NewRecorder()
	handleUsageBreakdown(w, httptest.NewRequest("GET", "/api/usage/breakdown?group=model&group=effort&from=2026-09-01", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var response UsageBreakdownResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.GroupBy) != 3 || response.GroupBy[2] != "runtime" {
		t.Fatalf("effort is always grouped with runtime: %v", response.GroupBy)
	}
	byModel := map[string]UsageBreakdownGroup{}
	for _, group := range response.Groups {
		byModel[group.Key.Model] = group
	}
	big := byModel["big-model"]
	if big.Key.Runtime != "alpha" || big.Key.Effort != "high" || *big.ReasoningShare != 0.6 ||
		*big.ReasoningPerCall != 60 || *big.ContextPerCall != 100 || *big.CacheHitRate != 0.9 {
		t.Fatalf("big model: %+v", big)
	}
	if unstated := byModel["other-model"]; unstated.ReasoningShare != nil || unstated.ReasoningPerCall != nil ||
		unstated.Reasoning.StatedCalls != 0 {
		t.Fatalf("an unstated class has no ratio, never zero: %+v", unstated)
	}
	for _, bad := range []string{"group=bogus", "bucket=month", "from=yesterday", "delegation=some", "delegation=delegated"} {
		w := httptest.NewRecorder()
		handleUsageBreakdown(w, httptest.NewRequest("GET", "/api/usage/breakdown?"+bad, nil))
		if w.Code != 400 {
			t.Fatalf("%s: status %d", bad, w.Code)
		}
	}
	w = httptest.NewRecorder()
	handleUsageBreakdown(w, httptest.NewRequest("GET", "/api/usage/breakdown?group=delegation", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	delegations := map[string]int64{}
	for _, group := range response.Groups {
		delegations[group.Key.Delegation] = group.Calls
	}
	if delegations["main"] != 2 || delegations["subagent"] != 1 {
		t.Fatalf("delegation: %v", delegations)
	}
}

func TestRecordedSessionUsageKeepsDelegatedApartAndLiveLevels(t *testing.T) {
	usageFixture(t, nil)
	usage, ok := recordedSessionUsage(SessionSummary{Runtime: "alpha", ID: "session-1", Model: "live-model", Context: 777})
	if !ok || usage.Turns != 1 || usage.OutputTokens != 100 || usage.Delegated == nil || usage.Delegated.Children != 1 ||
		usage.Delegated.OutputTokens != 50 || usage.Model != "live-model" || usage.Context != 777 || usage.AsOf.IsZero() ||
		usage.Agents != nil {
		t.Fatalf("usage: %+v delegated=%+v", usage, usage.Delegated)
	}
	if _, ok := recordedSessionUsage(SessionSummary{Runtime: "alpha", ID: "never-recorded"}); ok {
		t.Fatal("a session not recorded yet has no usage, not zeros")
	}
}

func TestUsageStatusStates(t *testing.T) {
	cases := []struct {
		result usagehistory.PassResult
		err    error
		want   string
	}{
		{usagehistory.PassResult{Discovered: 2, Recorded: 2}, nil, usageCoverageCurrent},
		{usagehistory.PassResult{Discovered: 2, Recorded: 1, Pending: 1}, nil, usageCoverageCatchingUp},
		{usagehistory.PassResult{Discovered: 2, Recorded: 1, Failed: 1}, nil, usageCoverageIncomplete},
		// A recorder that cannot write leaves the figures readable (C-5).
		{usagehistory.PassResult{}, errors.New("store busy"), usageCoverageIncomplete},
	}
	for _, c := range cases {
		if got := usageStatusFor(c.result, c.err).State; got != c.want {
			t.Fatalf("%+v %v: %s, want %s", c.result, c.err, got, c.want)
		}
	}
	if !usageStatusFor(usagehistory.PassResult{}, errors.New("store busy")).RecorderError {
		t.Fatal("a failed write is reported as the recorder's error")
	}
	var missing *usageRecorderCoordinator
	if status := missing.Status(); status.State != usageCoverageIncomplete || !status.RecorderError {
		t.Fatalf("no recorder: %+v", status)
	}
}

func TestUsageHistoryConfigFile(t *testing.T) {
	dir := t.TempDir()
	config, origin, err := loadUsageHistoryConfig(dir)
	if err != nil || origin != "builtin-default" || config.Recorder.IntervalSeconds != 30 ||
		config.Recorder.ReadBudgetBytes != 256<<20 || config.Report.MaxGroups != 24 || config.Report.TopSessions != 12 {
		t.Fatalf("defaults: %+v %s %v", config, origin, err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(usageHistoryConfigPath(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"format_version":1,"recorder":{"read_budget_bytes":1048576}}`)
	if config, _, err := loadUsageHistoryConfig(dir); err != nil || config.Recorder.ReadBudgetBytes != 1<<20 ||
		config.Recorder.IntervalSeconds != 30 {
		t.Fatalf("a partial file overrides only what it names: %+v %v", config, err)
	}
	for _, bad := range []string{`{"format_version":1,"retention_days":30}`, `{"format_version":1,"report":{"max_groups":0}}`,
		`{"format_version":1,"recorder":{"interval_seconds":9300000000}}`,
		`{"format_version":2}`} {
		write(bad)
		if _, _, err := loadUsageHistoryConfig(dir); err == nil {
			t.Fatalf("refused: %s", bad)
		}
	}
}

func TestUsageRoutesReportAnUnavailableStore(t *testing.T) {
	prior, priorPath, priorRecorder := governor, resolvedIndexPath, daemonUsageRecorder
	t.Cleanup(func() { governor, resolvedIndexPath, daemonUsageRecorder = prior, priorPath, priorRecorder })
	governor, daemonUsageRecorder = nil, nil
	resolvedIndexPath = filepath.Join(t.TempDir(), "missing", "index.sqlite")
	w := httptest.NewRecorder()
	handleUsageReport(w, httptest.NewRequest("GET", "/api/usage", nil))
	var report UsageReportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil || w.Code != 200 ||
		report.Coverage.State != usageCoverageUnavailable || report.TopTotal == nil {
		t.Fatalf("report: code=%d %+v err=%v", w.Code, report.Coverage, err)
	}
	w = httptest.NewRecorder()
	handleUsageBreakdown(w, httptest.NewRequest("GET", "/api/usage/breakdown?group=model", nil))
	var breakdown UsageBreakdownResponse
	if err := json.Unmarshal(w.Body.Bytes(), &breakdown); err != nil || breakdown.Coverage.State != usageCoverageUnavailable {
		t.Fatalf("breakdown: %s", w.Body.String())
	}
}

func TestUsageRecorderCoordinatorReportsAStoreItCannotOpen(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator := newUsageRecorderCoordinator(filepath.Join(blocker, "index.sqlite"))
	coordinator.Start()
	defer coordinator.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !coordinator.Status().RecorderError {
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v, want the recorder's error", coordinator.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if coordinator.Status().State != usageCoverageIncomplete {
		t.Fatalf("state = %s", coordinator.Status().State)
	}
}

// A pass that fails partway keeps the previous listing, so sessions of
// runtimes it never reached do not read as removed (code red-team C-12).
func TestAFailedPassKeepsThePreviousListing(t *testing.T) {
	previous := &usageRecorderStatus{Present: map[string]map[string]bool{"alpha": {"s": true}, "beta": {"t": true}}}
	partial := usagehistory.PassResult{Present: map[string]map[string]bool{"alpha": {"s": true}}}
	kept := keepUsagePresence(previous, usageStatusFor(partial, errors.New("store busy")), errors.New("store busy"))
	if !kept.Present["beta"]["t"] {
		t.Fatalf("presence: %+v", kept.Present)
	}
	fresh := keepUsagePresence(previous, usageStatusFor(partial, nil), nil)
	if fresh.Present["beta"] != nil {
		t.Fatal("a completed pass's listing replaces the previous one")
	}
}

// addUsageAgent records a helper session in another runtime, named by its
// native id through its alias, and makes the classifier read it as an agent
// serving session-1 (or fail, when fail is set).
func addUsageAgent(t *testing.T, ix *store.Index, fail bool) {
	t.Helper()
	day := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC).UnixMilli()
	helper := store.UsageCallRecord{Runtime: "gamma", CallID: "h1", SessionID: "helper-stem", Source: "helper-src",
		FirstAtMS: day, AtMS: day, Model: "helper-model", Input: usageCount(7), CacheRead: usageCount(3),
		CacheWrite: usageCount(0), Output: usageCount(9), Reader: "gamma/usage-1"}
	if _, err := ix.WriteUsageSource(store.UsageSourceWrite{Calls: []store.UsageCallRecord{helper},
		State: store.UsageSourceState{Runtime: "gamma", Source: "helper-src", SessionID: "helper-stem",
			SessionAlias: "helper-native", Marker: "m", Reader: "gamma/usage-1", Complete: true, UpdatedAtMS: day}}); err != nil {
		t.Fatal(err)
	}
	prior := usageAgentSessions
	t.Cleanup(func() { usageAgentSessions = prior })
	usageAgentSessions = func(*store.Index, int) (map[string]store.AgentSessionParent, bool, error) {
		if fail {
			return nil, false, errors.New("orchestration store unreadable")
		}
		return map[string]store.AgentSessionParent{"helper-native": {Runtime: "gamma", Role: "helper",
			RootRuntime: "alpha", RootNativeSessionID: "session-1", Profiles: []string{"recall"}}}, false, nil
	}
}

func getSessionUsage(t *testing.T, runtime, id string) SessionUsageBreakdown {
	t.Helper()
	w := httptest.NewRecorder()
	handleSessionUsage(w, httptest.NewRequest("GET", "/api/session/usage?runtime="+runtime+"&id="+id, nil))
	var response SessionUsageBreakdown
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	return response
}

// The Usage pane and the footer fold the same read: at the same as-of they
// agree (plan invariant 8), and an agent counts under the session it served.
func TestSessionUsageSplitsWorkAndAgreesWithTheFooter(t *testing.T) {
	ix := usageFixture(t, []SessionSummary{{Runtime: "alpha", ID: "session-1", Title: "Present session"}})
	addUsageAgent(t, ix, false)
	pane := getSessionUsage(t, "alpha", "session-1")
	if pane.Coverage.Agents != usageAgentsCurrent || pane.Main.Calls != 1 || pane.Subagent.Calls != 1 ||
		pane.Agent == nil || pane.Agent.Calls != 1 || len(pane.Subagents) != 1 || pane.Subagents[0].ID != "agent-1" ||
		len(pane.Agents) != 1 || pane.Agents[0].ID != "helper-native" || pane.Agents[0].Profiles[0] != "recall" ||
		len(pane.Agents[0].CallTimes) != 1 || len(pane.Main.Series) != 1 || pane.AsOf.IsZero() {
		t.Fatalf("pane: %+v", pane)
	}
	footer, ok := recordedSessionUsage(SessionSummary{Runtime: "alpha", ID: "session-1"})
	if !ok || footer.Turns != pane.Main.Calls || footer.Delegated.Turns != pane.Subagent.Calls || footer.Agents == nil ||
		footer.Agents.Turns != pane.Agent.Calls || footer.Agents.Children != 1 || !footer.AsOf.Equal(pane.AsOf) {
		t.Fatalf("footer %+v disagrees with pane %+v", footer, pane)
	}
	if own := getSessionUsage(t, "gamma", "helper-stem"); own.Main.Calls != 1 || own.Agent == nil || own.Agent.Calls != 0 {
		t.Fatalf("an agent session's own page shows its calls as main: %+v", own)
	}
	w := httptest.NewRecorder()
	handleUsageReport(w, httptest.NewRequest("GET", "/api/usage", nil))
	var report UsageReportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	for _, row := range report.TopTotal {
		if row.ID == "helper-stem" {
			t.Fatalf("an agent session is not a standalone row: %+v", row)
		}
		if row.ID == "session-1" && (row.Agents != 1 || row.Agent.Calls != 1 || row.AgentProfiles[0] != "recall") {
			t.Fatalf("the served row holds its agent: %+v", row)
		}
	}
	if report.Work["agent"].Calls != 1 || report.Sessions == nil || *report.Sessions != 2 {
		t.Fatalf("agent work and root count: %+v sessions=%v", report.Work, report.Sessions)
	}
	types := map[string]UsageTypeRow{}
	for _, row := range report.ByType {
		types[row.Kind+"/"+row.Type] = row
	}
	if types["agent/recall"].Members != 1 || types["agent/recall"].Roots != 1 || types["subagent/no role stated"].Calls != 1 {
		t.Fatalf("by type: %+v", report.ByType)
	}
}

// The report folds its totals, days and split from the one work pass the
// roll-up reads; they equal a direct pass by runtime, kind and day (code
// red-team C-4).
func TestUsageReportFoldsTheWorkPassExactly(t *testing.T) {
	ix := usageFixture(t, nil)
	addUsageAgent(t, ix, false)
	later := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := ix.WriteUsageSource(store.UsageSourceWrite{Calls: []store.UsageCallRecord{{Runtime: "alpha",
		CallID: "o2", SessionID: "session-1", Source: "src-later", FirstAtMS: later, AtMS: later, Model: "big-model",
		Input: usageCount(4), CacheRead: usageCount(6), CacheWrite: usageCount(1), Output: usageCount(2),
		Other: `[{"id":"audio","label":"audio","side":"input","count":5}]`,
		Cost:  &store.UsageCost{Amount: 0.5, Unit: "USD", Basis: "runtime"}, Reader: "alpha/usage-1"}},
		State: store.UsageSourceState{Runtime: "alpha", Source: "src-later", SessionID: "session-1", Marker: "m",
			Reader: "alpha/usage-1", Complete: true, UpdatedAtMS: later}}); err != nil {
		t.Fatal(err)
	}
	report, err := buildUsageReport(daemonUsageRecorder.Status())
	if err != nil {
		t.Fatal(err)
	}
	work, err := currentUsageWork(ix, daemonUsageRecorder.Status().AsOf)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := ix.UsageGroups(store.UsageQuery{GroupBy: []string{store.UsageDimRuntime, store.UsageDimDelegation},
		Bucket: store.UsageBucketDay, Details: store.UsageDetailOther | store.UsageDetailCost, Agents: work.Work.Keys})
	if err != nil {
		t.Fatal(err)
	}
	want := UsageReportResponse{ByRuntime: map[string]UsageReportTotals{}, Days: map[string]UsageDayTotals{},
		DaysByRuntime: map[string]map[string]UsageDayTotals{}, Cost: []harvest.CostTotal{}}
	foldUsageReportGroups(groups, &want, true)
	for name, pair := range map[string][2]any{"totals": {report.Totals, want.Totals}, "runtimes": {report.ByRuntime, want.ByRuntime},
		"days": {report.Days, want.Days}, "days by runtime": {report.DaysByRuntime, want.DaysByRuntime},
		"work": {report.Work, want.Work}, "work by runtime": {report.WorkByRuntime, want.WorkByRuntime},
		"cost": {report.Cost, want.Cost}, "cost coverage": {report.CostCoverage, want.CostCoverage}} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s: folded %+v, direct %+v", name, pair[0], pair[1])
		}
	}
	if len(report.Days) != 2 || report.Totals.Calls != 5 || len(report.Cost) != 1 || report.Work["agent"].Calls != 1 {
		t.Fatalf("fixture not exercised: days %d, calls %d, cost %+v, work %+v", len(report.Days),
			report.Totals.Calls, report.Cost, report.Work)
	}
}

// When the agent read fails, the grand totals stand and the split is
// withheld; no view counts agent calls as main (red-team S-4).
func TestUsageAgentSplitIsWithheldWhenAgentsCannotBeRead(t *testing.T) {
	ix := usageFixture(t, nil)
	addUsageAgent(t, ix, true)
	w := httptest.NewRecorder()
	handleUsageReport(w, httptest.NewRequest("GET", "/api/usage", nil))
	var report UsageReportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Coverage.Agents != usageAgentsUnavailable || report.Totals.Calls != 4 || report.Work != nil ||
		len(report.TopTotal) != 0 || report.ByType != nil || report.Sessions != nil || report.NoData != nil ||
		report.Coverage.SessionsWithoutSource != 3 {
		t.Fatalf("report: agents=%s totals=%+v work=%+v top=%d", report.Coverage.Agents, report.Totals, report.Work,
			len(report.TopTotal))
	}
	w = httptest.NewRecorder()
	handleUsageBreakdown(w, httptest.NewRequest("GET", "/api/usage/breakdown?group=delegation", nil))
	var breakdown UsageBreakdownResponse
	if err := json.Unmarshal(w.Body.Bytes(), &breakdown); err != nil || len(breakdown.Groups) != 0 ||
		breakdown.Coverage.Agents != usageAgentsUnavailable {
		t.Fatalf("breakdown by delegation is withheld: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	handleUsageBreakdown(w, httptest.NewRequest("GET", "/api/usage/breakdown?group=model", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &breakdown); err != nil || len(breakdown.Groups) == 0 {
		t.Fatalf("a breakdown that does not split work still answers: %s", w.Body.String())
	}
	pane := getSessionUsage(t, "alpha", "session-1")
	footer, _ := recordedSessionUsage(SessionSummary{Runtime: "alpha", ID: "session-1"})
	if pane.Agent != nil || pane.Coverage.Agents != usageAgentsUnavailable || footer.Agents != nil || footer.Turns != 1 {
		t.Fatalf("pane %+v footer %+v", pane, footer)
	}
}

func TestUsageHistoryConfigBoundsTheUsagePane(t *testing.T) {
	dir := t.TempDir()
	config, _, err := loadUsageHistoryConfig(dir)
	if err != nil || config.Report.SessionSeriesPoints != 600 || config.Report.SessionAgentTicks != 500 ||
		config.Report.SessionMembersMax != 200 || config.Report.AgentSessionsMax != 100000 {
		t.Fatalf("defaults: %+v %v", config.Report, err)
	}
	for _, bad := range []string{`{"format_version":1,"report":{"session_series_points":20001}}`,
		`{"format_version":1,"report":{"session_members_max":0}}`} {
		if err := os.WriteFile(usageHistoryConfigPath(dir), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadUsageHistoryConfig(dir); err == nil {
			t.Fatalf("refused: %s", bad)
		}
	}
}

// The route bounds a session's lists and counts what it cut; a cut agent read
// makes the split incomplete (code red-team C-18).
func TestSessionUsageBoundsAndAnIncompleteAgentRead(t *testing.T) {
	ix := usageFixture(t, nil)
	addUsageAgent(t, ix, false)
	prior := usageAgentSessions
	usageAgentSessions = func(index *store.Index, limit int) (map[string]store.AgentSessionParent, bool, error) {
		agents, _, err := prior(index, limit)
		return agents, true, err
	}
	usageHistoryConfigOnce.Do(func() {})
	priorConfig := usageHistoryConfigValue
	t.Cleanup(func() { usageHistoryConfigValue = priorConfig })
	usageHistoryConfigValue = defaultUsageHistoryConfig()
	usageHistoryConfigValue.Report.SessionMembersMax = 1
	usageHistoryConfigValue.Report.SessionAgentTicks = 1
	extra := store.UsageCallRecord{Runtime: "alpha", CallID: "s2", SessionID: "session-1", Agent: "agent-2", Source: "src-agent-2",
		FirstAtMS: 5, AtMS: 5, Model: "small-model", Input: usageCount(1), CacheRead: usageCount(0), CacheWrite: usageCount(0),
		Output: usageCount(1), Reader: "alpha/usage-1"}
	if _, err := ix.WriteUsageSource(store.UsageSourceWrite{Calls: []store.UsageCallRecord{extra}, State: store.UsageSourceState{
		Runtime: "alpha", Source: "src-agent-2", SessionID: "session-1", Marker: "m", Reader: "alpha/usage-1",
		Complete: true, UpdatedAtMS: 9}}); err != nil {
		t.Fatal(err)
	}
	helper := store.UsageCallRecord{Runtime: "gamma", CallID: "h2", SessionID: "helper-stem", Source: "helper-src-2",
		FirstAtMS: 6, AtMS: 6, Model: "helper-model", Input: usageCount(1), CacheRead: usageCount(0),
		CacheWrite: usageCount(0), Output: usageCount(1), Reader: "gamma/usage-1"}
	if _, err := ix.WriteUsageSource(store.UsageSourceWrite{Calls: []store.UsageCallRecord{helper}, State: store.UsageSourceState{
		Runtime: "gamma", Source: "helper-src-2", SessionID: "helper-stem", SessionAlias: "helper-native", Marker: "m",
		Reader: "gamma/usage-1", Complete: true, UpdatedAtMS: 9}}); err != nil {
		t.Fatal(err)
	}
	pane := getSessionUsage(t, "alpha", "session-1")
	if len(pane.Subagents) != 1 || pane.SubagentsOmitted != 1 || pane.Subagents[0].ID != "agent-1" ||
		pane.Coverage.Agents != usageAgentsIncomplete {
		t.Fatalf("bounds: %+v", pane)
	}
	if len(pane.Agents) != 1 || len(pane.Agents[0].CallTimes) != 1 || pane.Agents[0].CallTimesOmitted != 1 {
		t.Fatalf("call times past session_agent_ticks: %+v", pane.Agents)
	}
	// A session whose only calls are its subagents' has a footer with no
	// calls of its own ("no calls recorded"), not no footer (S-8).
	only := store.UsageCallRecord{Runtime: "alpha", CallID: "d1", SessionID: "session-2", Agent: "agent-9", Source: "src-d1",
		FirstAtMS: 7, AtMS: 7, Model: "small-model", Input: usageCount(1), CacheRead: usageCount(0), CacheWrite: usageCount(0),
		Output: usageCount(1), Reader: "alpha/usage-1"}
	if _, err := ix.WriteUsageSource(store.UsageSourceWrite{Calls: []store.UsageCallRecord{only}, State: store.UsageSourceState{
		Runtime: "alpha", Source: "src-d1", SessionID: "session-2", Marker: "m", Reader: "alpha/usage-1", Complete: true,
		UpdatedAtMS: 9}}); err != nil {
		t.Fatal(err)
	}
	footer, ok := recordedSessionUsage(SessionSummary{Runtime: "alpha", ID: "session-2"})
	if !ok || footer.Turns != 0 || footer.Delegated == nil || footer.Delegated.Turns != 1 || footer.Delegated.Children != 1 {
		t.Fatalf("no calls of its own: ok=%v %+v", ok, footer)
	}
}
