package daemon

// The usage routes (token-usage-analytics plan §3.7) and the recorded usage a
// session's detail shows. Every figure comes from the calls the usage
// recorder wrote to the store; the in-memory scan contributes only titles and
// whether a session's source is still on disk. Response types are declared
// here because the API is the product boundary (ADR 0022).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// UsageCoverage says how much of the discovered sources the figures cover.
// Calls counts in cost_coverage; sessions elsewhere.
type UsageCoverage struct {
	State                 string `json:"state"`
	SourcesDiscovered     int    `json:"sources_discovered"`
	SourcesRecorded       int    `json:"sources_recorded"`
	SourcesPending        int    `json:"sources_pending"`
	SourcesFailed         int    `json:"sources_failed"`
	SessionsWithoutSource int    `json:"sessions_without_source"`
	// RecorderError is true when the recorder's last pass could not write:
	// the figures are readable but not advancing.
	RecorderError bool      `json:"recorder_error,omitempty"`
	AsOf          time.Time `json:"as_of,omitzero"`
	// Agents is how complete the split into Crossing Guard agents' work is:
	// current, incomplete (the agent read was cut) or unavailable (it failed,
	// and the agent split is withheld).
	Agents string `json:"agents,omitempty"`
	// AgentNotes counts what the agent split met: agents with nothing
	// recorded yet, agents serving an unrecorded session, parent cycles and
	// shared aliases. Absent when all are zero.
	AgentNotes *store.UsageAgentNotes `json:"agent_notes,omitempty"`
}

// UsageReportTotals is a runtime-neutral total. Calls are model calls (the
// former `turns` counted transcript lines for some runtimes).
type UsageReportTotals struct {
	Calls                int64                `json:"calls"`
	InputTokens          int64                `json:"input_tokens"`
	OutputTokens         int64                `json:"output_tokens"`
	CacheRead            int64                `json:"cache_read"`
	CacheCreate          int64                `json:"cache_create"`
	CacheHitRate         *float64             `json:"cache_hit_rate,omitempty"`
	Reasoning            *int64               `json:"reasoning_tokens,omitempty"`
	ReasoningStatedCalls int64                `json:"reasoning_stated_calls"`
	Other                []harvest.TokenCount `json:"other,omitempty"`
	// Total is every stated token: the four classes and other classes.
	Total int64 `json:"total"`
}

// UsageDayTotals is one UTC day's calls.
type UsageDayTotals struct {
	Input       int64                `json:"input"`
	Output      int64                `json:"output"`
	CacheRead   int64                `json:"cache_read"`
	CacheCreate int64                `json:"cache_create"`
	Calls       int64                `json:"calls"`
	Other       []harvest.TokenCount `json:"other,omitempty"`
}

// UsageRowWork is one kind of a root session's work.
type UsageRowWork struct {
	Calls        int64 `json:"calls"`
	Total        int64 `json:"total"`
	OutputTokens int64 `json:"output_tokens"`
}

// UsageSessionRow is one root session in a top-sessions table: its own calls
// as main, and its subtree's and its agents' calls beside them (session usage
// breakdown plan P-4). SourcePresent is false once the vendor removed the
// session's source; the row then carries no title (plan D-8).
type UsageSessionRow struct {
	Main          UsageRowWork        `json:"main"`
	Subagent      UsageRowWork        `json:"subagent"`
	Agent         UsageRowWork        `json:"agent"`
	Subagents     int                 `json:"subagents"`
	Agents        int                 `json:"agents"`
	AgentProfiles []string            `json:"agent_profiles,omitempty"`
	Runtime       string              `json:"runtime"`
	ID            string              `json:"id"`
	Title         string              `json:"title"`
	Project       string              `json:"project"`
	Modified      time.Time           `json:"modified"`
	Total         int64               `json:"total"`
	Context       int64               `json:"context"`
	HitRate       *float64            `json:"hit_rate,omitempty"`
	Model         string              `json:"model,omitempty"`
	Cost          []harvest.CostTotal `json:"cost,omitempty"`
	SourcePresent bool                `json:"source_present"`
}

// UsageTypeRow is one subagent or agent type across sessions: a subagent
// source's vendor role, or an agent's run profiles. Members counts distinct
// subagents or agents; Roots the root sessions they worked for.
type UsageTypeRow struct {
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Runtime string `json:"runtime"`
	Members int    `json:"members"`
	Roots   int    `json:"roots"`
	UsageReportTotals
}

// UsageReportResponse is GET /api/usage. Work splits the totals by kind
// (main, subagent, agent); it, WorkByRuntime, ByType and the root rows are
// withheld when coverage.agents is unavailable.
type UsageReportResponse struct {
	Coverage      UsageCoverage                           `json:"coverage"`
	Totals        UsageReportTotals                       `json:"totals"`
	Work          map[string]UsageReportTotals            `json:"work,omitempty"`
	WorkByRuntime map[string]map[string]UsageReportTotals `json:"work_by_runtime,omitempty"`
	ByType        []UsageTypeRow                          `json:"by_type,omitempty"`
	ByTypeOmitted int                                     `json:"by_type_omitted,omitempty"`
	Sessions      *int64                                  `json:"sessions,omitempty"`
	NoData        *int64                                  `json:"no_data,omitempty"`
	ByRuntime     map[string]UsageReportTotals            `json:"by_runtime"`
	Days          map[string]UsageDayTotals               `json:"days"`
	DaysByRuntime map[string]map[string]UsageDayTotals    `json:"days_by_runtime"`
	TopContext    []UsageSessionRow                       `json:"top_context"`
	TopTotal      []UsageSessionRow                       `json:"top_total"`
	Cost          []harvest.CostTotal                     `json:"cost"`
	CostCoverage  harvest.CostCoverage                    `json:"cost_coverage"` // in calls
}

// UsageClassTotal is a class summed over the calls that stated it.
type UsageClassTotal struct {
	Sum         int64 `json:"sum"`
	StatedCalls int64 `json:"stated_calls"`
}

// UsageBreakdownKey is one group's key; dimensions not grouped by are empty.
type UsageBreakdownKey struct {
	Runtime    string `json:"runtime,omitempty"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Client     string `json:"client,omitempty"`
	Delegation string `json:"delegation,omitempty"`
	Member     string `json:"member,omitempty"`
	Session    string `json:"session,omitempty"`
	AgentType  string `json:"agent_type,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
}

// UsagePartTotal is one labelled part of a well-known class.
type UsagePartTotal struct {
	Of    string `json:"of"`
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// UsageCostLine is runtime-stated cost in one unit and basis, and how many
// calls stated it.
type UsageCostLine struct {
	Unit   string  `json:"unit"`
	Basis  string  `json:"basis"`
	Amount float64 `json:"amount"`
	Calls  int64   `json:"calls"`
}

// UsageReaderCalls counts calls by the reader that produced them (ADR 0026).
type UsageReaderCalls struct {
	Reader string `json:"reader"`
	Calls  int64  `json:"calls"`
}

// UsageBreakdownGroup is one group of GET /api/usage/breakdown. A ratio is
// absent, never zero, when its denominator is zero or unstated.
type UsageBreakdownGroup struct {
	Key                 UsageBreakdownKey    `json:"key"`
	Calls               int64                `json:"calls"`
	Sessions            int64                `json:"sessions"`
	Input               UsageClassTotal      `json:"input"`
	CacheRead           UsageClassTotal      `json:"cache_read"`
	CacheWrite          UsageClassTotal      `json:"cache_write"`
	Output              UsageClassTotal      `json:"output"`
	Reasoning           UsageClassTotal      `json:"reasoning"`
	ReasoningShare      *float64             `json:"reasoning_share,omitempty"`
	ReasoningShareCalls int64                `json:"reasoning_share_calls"`
	ReasoningPerCall    *float64             `json:"reasoning_per_call,omitempty"`
	ContextPerCall      *float64             `json:"context_per_call,omitempty"`
	ContextCalls        int64                `json:"context_calls"`
	CacheHitRate        *float64             `json:"cache_hit_rate,omitempty"`
	Parts               []UsagePartTotal     `json:"parts,omitempty"`
	Other               []harvest.TokenCount `json:"other,omitempty"`
	Cost                []UsageCostLine      `json:"cost,omitempty"`
	Readers             []UsageReaderCalls   `json:"readers,omitempty"`
}

// UsageWindow is the breakdown's UTC date window; empty ends are open.
type UsageWindow struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// UsageBreakdownResponse is GET /api/usage/breakdown.
type UsageBreakdownResponse struct {
	Coverage      UsageCoverage         `json:"coverage"`
	Window        UsageWindow           `json:"window"`
	GroupBy       []string              `json:"group_by"`
	Bucket        string                `json:"bucket,omitempty"`
	Groups        []UsageBreakdownGroup `json:"groups"`
	OmittedGroups int                   `json:"omitted_groups"`
}

func registerUsageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/usage", handleUsageReport)
	mux.HandleFunc("GET /api/usage/breakdown", handleUsageBreakdown)
	mux.HandleFunc("GET /api/session/usage", handleSessionUsage)
}

// usageReadIndex is the long-lived store handle when the governor holds one
// (plan S2-15), else a read-only open closed after the request.
func usageReadIndex() (*store.Index, func(), error) {
	if governor != nil && governor.ix != nil {
		return governor.ix, func() {}, nil
	}
	index, err := store.OpenRO(indexPath())
	if err != nil {
		return nil, nil, err
	}
	return index, func() { _ = index.Close() }, nil
}

func usageCoverageFrom(status usageRecorderStatus) UsageCoverage {
	return UsageCoverage{State: status.State, SourcesDiscovered: status.Discovered,
		SourcesRecorded: status.Recorded, SourcesPending: status.Pending, SourcesFailed: status.Failed,
		RecorderError: status.RecorderError, AsOf: status.AsOf}
}

func handleUsageReport(w http.ResponseWriter, _ *http.Request) {
	report, err := buildUsageReport(daemonUsageRecorder.Status())
	if err != nil {
		report = UsageReportResponse{Coverage: UsageCoverage{State: usageCoverageUnavailable},
			ByRuntime: map[string]UsageReportTotals{}, Days: map[string]UsageDayTotals{},
			DaysByRuntime: map[string]map[string]UsageDayTotals{}, TopContext: []UsageSessionRow{},
			TopTotal: []UsageSessionRow{}, Cost: []harvest.CostTotal{}}
	}
	writeJSON(w, report)
}

// buildUsageReport answers GET /api/usage from one grouped pass over the
// recorded calls (by runtime, kind and UTC day), a by-type pass, the root
// counts and one root ranking pass (code red-team C-3). When the agent split
// is unavailable, the grand totals stand and the split is withheld (S-4).
func buildUsageReport(status usageRecorderStatus) (UsageReportResponse, error) {
	index, release, err := usageReadIndex()
	if err != nil {
		return UsageReportResponse{}, err
	}
	defer release()
	report := UsageReportResponse{Coverage: usageCoverageFrom(status), ByRuntime: map[string]UsageReportTotals{},
		Days: map[string]UsageDayTotals{}, DaysByRuntime: map[string]map[string]UsageDayTotals{},
		Cost: []harvest.CostTotal{}, TopContext: []UsageSessionRow{}, TopTotal: []UsageSessionRow{}}
	work, err := currentUsageWork(index, status.AsOf)
	if err != nil {
		return report, err
	}
	setUsageAgentCoverage(&report.Coverage, work)
	scanned, present := usageSessionPresence(status)
	if err := countUsageSessionsWithoutSource(index, present, &report); err != nil {
		return report, err
	}
	if work.Agents == usageAgentsUnavailable {
		// The totals stand without the split; the root counts and rows need it (S-4).
		groups, err := index.UsageGroups(store.UsageQuery{GroupBy: []string{store.UsageDimRuntime},
			Bucket: store.UsageBucketDay, Details: store.UsageDetailOther | store.UsageDetailCost})
		if err != nil {
			return report, err
		}
		foldUsageReportGroups(groups, &report, false)
		return report, nil
	}
	groups, err := index.UsageWorkGroups(store.UsageQuery{}, work.Work)
	if err != nil {
		return report, err
	}
	foldUsageReportGroups(groups, &report, true)
	sessions, _, noData, err := index.UsageRootCounts(work.Work)
	if err != nil {
		return report, err
	}
	report.Sessions, report.NoData = &sessions, &noData
	fillUsageByType(groups, work.Work, &report)
	return report, fillUsageTopSessions(index, groups, work.Work, scanned, present, &report)
}

// setUsageAgentCoverage reports how complete the agent split is: a cut read,
// or a parent cycle or shared alias the classifier met, is incomplete (C-3).
func setUsageAgentCoverage(coverage *UsageCoverage, work usageWork) {
	coverage.Agents = work.Agents
	if work.Work == nil {
		return
	}
	notes := work.Work.Notes
	if notes != (store.UsageAgentNotes{}) {
		coverage.AgentNotes = &notes
	}
	if coverage.Agents == usageAgentsCurrent && notes.Incomplete() {
		coverage.Agents = usageAgentsIncomplete
	}
}

// usageTotal accumulates groups into one total.
type usageTotal struct {
	group store.UsageGroup
	cost  map[[2]string]*store.UsageCostSum
}

func (t *usageTotal) add(group store.UsageGroup) {
	sum := &t.group
	sum.Calls += group.Calls
	sum.Total += group.Total
	for _, class := range []struct{ into, from *store.UsageClassSum }{{&sum.Input, &group.Input},
		{&sum.CacheRead, &group.CacheRead}, {&sum.CacheWrite, &group.CacheWrite}, {&sum.Output, &group.Output},
		{&sum.Reasoning, &group.Reasoning}} {
		class.into.Sum += class.from.Sum
		class.into.Stated += class.from.Stated
	}
	sum.Other = append(sum.Other, group.Other...)
	if t.cost == nil {
		t.cost = map[[2]string]*store.UsageCostSum{}
	}
	for _, line := range group.Cost {
		key := [2]string{line.Unit, line.Basis}
		if t.cost[key] == nil {
			t.cost[key] = &store.UsageCostSum{Unit: line.Unit, Basis: line.Basis}
		}
		t.cost[key].Amount += line.Amount
		t.cost[key].Calls += line.Calls
	}
}

// addUsageDay adds one group's figures to a day's totals.
func addUsageDay(into UsageDayTotals, day UsageDayTotals) UsageDayTotals {
	into.Input, into.Output = into.Input+day.Input, into.Output+day.Output
	into.CacheRead, into.CacheCreate = into.CacheRead+day.CacheRead, into.CacheCreate+day.CacheCreate
	into.Calls += day.Calls
	into.Other = harvest.AddTokenCounts(into.Other, append([]harvest.TokenCount(nil), day.Other...))
	return into
}

func foldUsageReportGroups(groups []store.UsageGroup, report *UsageReportResponse, split bool) {
	all := &usageTotal{}
	runtimes := map[string]*usageTotal{}
	kinds := map[string]*usageTotal{}
	runtimeKinds := map[[2]string]*usageTotal{}
	for _, group := range groups {
		all.add(group)
		if runtimes[group.Key.Runtime] == nil {
			runtimes[group.Key.Runtime] = &usageTotal{}
		}
		runtimes[group.Key.Runtime].add(group)
		if split {
			pair := [2]string{group.Key.Runtime, group.Key.Delegation}
			if kinds[pair[1]] == nil {
				kinds[pair[1]] = &usageTotal{}
			}
			if runtimeKinds[pair] == nil {
				runtimeKinds[pair] = &usageTotal{}
			}
			kinds[pair[1]].add(group)
			runtimeKinds[pair].add(group)
		}
		day := usageDayTotals(group)
		report.Days[group.Key.Bucket] = addUsageDay(report.Days[group.Key.Bucket], day)
		if report.DaysByRuntime[group.Key.Runtime] == nil {
			report.DaysByRuntime[group.Key.Runtime] = map[string]UsageDayTotals{}
		}
		byRuntime := report.DaysByRuntime[group.Key.Runtime]
		byRuntime[group.Key.Bucket] = addUsageDay(byRuntime[group.Key.Bucket], day)
	}
	report.Totals = usageReportTotals(all.group)
	for runtime, total := range runtimes {
		report.ByRuntime[runtime] = usageReportTotals(total.group)
	}
	if split {
		report.Work, report.WorkByRuntime = map[string]UsageReportTotals{}, map[string]map[string]UsageReportTotals{}
		for kind, total := range kinds {
			report.Work[kind] = usageReportTotals(total.group)
		}
		for pair, total := range runtimeKinds {
			if report.WorkByRuntime[pair[0]] == nil {
				report.WorkByRuntime[pair[0]] = map[string]UsageReportTotals{}
			}
			report.WorkByRuntime[pair[0]][pair[1]] = usageReportTotals(total.group)
		}
	}
	keys := make([][2]string, 0, len(all.cost))
	for key := range all.cost {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, key := range keys {
		line := all.cost[key]
		report.Cost = append(report.Cost, harvest.CostTotal{Unit: line.Unit, Basis: line.Basis, Amount: line.Amount})
		report.CostCoverage.WithCost += int(line.Calls)
	}
	report.CostCoverage.WithoutCost = int(all.group.Calls) - report.CostCoverage.WithCost
}

func usageReportTotals(group store.UsageGroup) UsageReportTotals {
	totals := UsageReportTotals{Calls: group.Calls, InputTokens: group.Input.Sum, OutputTokens: group.Output.Sum,
		CacheRead: group.CacheRead.Sum, CacheCreate: group.CacheWrite.Sum,
		ReasoningStatedCalls: group.Reasoning.Stated, Other: usageOtherTotals(group.Other), Total: group.Total,
		CacheHitRate: usageRatio(group.CacheRead.Sum, group.Input.Sum+group.CacheRead.Sum+group.CacheWrite.Sum)}
	if group.Reasoning.Stated > 0 {
		reasoning := group.Reasoning.Sum
		totals.Reasoning = &reasoning
	}
	return totals
}

func usageDayTotals(group store.UsageGroup) UsageDayTotals {
	return UsageDayTotals{Input: group.Input.Sum, Output: group.Output.Sum, CacheRead: group.CacheRead.Sum,
		CacheCreate: group.CacheWrite.Sum, Calls: group.Calls, Other: usageOtherTotals(group.Other)}
}

// usageOtherTotals sums other classes by id.
func usageOtherTotals(other []store.UsageOtherSum) []harvest.TokenCount {
	var out []harvest.TokenCount
	for _, item := range other {
		out = harvest.AddTokenCounts(out, []harvest.TokenCount{{ID: item.ID, Label: item.Label, Side: item.Side,
			Count: item.Count}})
	}
	return out
}

// usageSessionPresence is the live scan by session, and whether a session's
// source is still on disk: from the recorder's last listing, so a failed
// session scan never strips labels (code red-team C-9); before the first
// pass, from the live scan.
func usageSessionPresence(status usageRecorderStatus) (map[[2]string]SessionSummary, func(runtime, session string) bool) {
	scanned := map[[2]string]SessionSummary{}
	for _, session := range ScanSessions() {
		scanned[[2]string{session.Runtime, session.ID}] = session
	}
	present := func(runtime, session string) bool {
		if status.Present == nil {
			_, found := scanned[[2]string{runtime, session}]
			return found
		}
		return status.ListingFailed[runtime] || status.Present[runtime][session]
	}
	return scanned, present
}

func countUsageSessionsWithoutSource(index *store.Index, present func(runtime, session string) bool,
	report *UsageReportResponse) error {
	keys, err := index.UsageSessionKeys()
	if err != nil {
		return err
	}
	for key := range keys {
		if !present(key[0], key[1]) {
			report.Coverage.SessionsWithoutSource++
		}
	}
	return nil
}

// fillUsageTopSessions ranks root sessions and labels them; titles come from
// the live scan.
func fillUsageTopSessions(index *store.Index, workGroups []store.UsageGroup, work *store.UsageWorkIndex,
	scanned map[[2]string]SessionSummary, present func(runtime, session string) bool, report *UsageReportResponse) error {
	byTotal, byContext, err := index.UsageRootTotalsFrom(store.UsageQuery{}, workGroups, work,
		usageHistoryConfig().Report.TopSessions)
	if err != nil {
		return err
	}
	for _, total := range byTotal {
		report.TopTotal = append(report.TopTotal, usageSessionRow(total, scanned, present))
	}
	for _, total := range byContext {
		report.TopContext = append(report.TopContext, usageSessionRow(total, scanned, present))
	}
	return nil
}

func usageRowWork(sum store.UsageWorkSum) UsageRowWork {
	return UsageRowWork{Calls: sum.Calls, Total: sum.Total, OutputTokens: sum.Output.Sum}
}

func usageSessionRow(total store.UsageRootTotal, scanned map[[2]string]SessionSummary,
	present func(runtime, session string) bool) UsageSessionRow {
	root := total.Root
	input := total.Main.Input.Sum + total.Subagent.Input.Sum + total.Agent.Input.Sum
	read := total.Main.CacheRead.Sum + total.Subagent.CacheRead.Sum + total.Agent.CacheRead.Sum
	write := total.Main.CacheWrite.Sum + total.Subagent.CacheWrite.Sum + total.Agent.CacheWrite.Sum
	row := UsageSessionRow{Runtime: root.Runtime, ID: root.SessionID, Total: total.Total,
		Context: total.LastContext, Model: total.LastModel, Modified: time.UnixMilli(total.LastAtMS).UTC(),
		HitRate: usageRatio(read, input+read+write), Main: usageRowWork(total.Main),
		Subagent: usageRowWork(total.Subagent), Agent: usageRowWork(total.Agent), Subagents: total.Subagents,
		Agents: total.Agents, AgentProfiles: total.AgentProfiles}
	for _, line := range total.Cost {
		row.Cost = append(row.Cost, harvest.CostTotal{Unit: line.Unit, Basis: line.Basis, Amount: line.Amount})
	}
	row.SourcePresent = present(root.Runtime, root.SessionID)
	if session, found := scanned[[2]string{root.Runtime, root.SessionID}]; found && row.SourcePresent {
		row.Title, row.Project, row.Modified = session.Title, session.Project, session.Modified
	}
	return row
}

// usageTypeKey is one by-type row's identity.
type usageTypeKey struct{ runtime, kind, agentType string }

// fillUsageByType folds subagent and agent work by type from the shared work
// groups, counting distinct members and the roots they worked for, so
// "sessions" means the same thing for every runtime (red-team R-8).
func fillUsageByType(groups []store.UsageGroup, work *store.UsageWorkIndex, report *UsageReportResponse) {
	totals := map[usageTypeKey]*usageTotal{}
	members := map[usageTypeKey]map[string]bool{}
	roots := map[usageTypeKey]map[store.UsageSessionRef]bool{}
	for _, group := range groups {
		if group.Key.Delegation == store.UsageDelegationMain {
			continue
		}
		key := usageTypeKey{runtime: group.Key.Runtime, kind: group.Key.Delegation, agentType: group.Key.AgentType}
		if totals[key] == nil {
			totals[key], members[key], roots[key] = &usageTotal{}, map[string]bool{}, map[store.UsageSessionRef]bool{}
		}
		totals[key].add(group)
		members[key][group.Key.Member] = true
		roots[key][work.Root(store.UsageSessionRef{Runtime: group.Key.Runtime, SessionID: group.Key.Session})] = true
	}
	for key, total := range totals {
		label := key.agentType
		if label == "" {
			label = "no role stated"
		}
		report.ByType = append(report.ByType, UsageTypeRow{Type: label, Kind: key.kind, Runtime: key.runtime,
			Members: len(members[key]), Roots: len(roots[key]), UsageReportTotals: usageReportTotals(total.group)})
	}
	sort.Slice(report.ByType, func(i, j int) bool {
		if report.ByType[i].Total != report.ByType[j].Total {
			return report.ByType[i].Total > report.ByType[j].Total
		}
		return report.ByType[i].Type+report.ByType[i].Runtime < report.ByType[j].Type+report.ByType[j].Runtime
	})
	if limit := usageHistoryConfig().Report.MaxGroups; len(report.ByType) > limit {
		report.ByTypeOmitted, report.ByType = len(report.ByType)-limit, report.ByType[:limit]
	}
}

// usageDateLayout is how the breakdown's window is written: UTC dates.
const usageDateLayout = "2006-01-02"

func handleUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	query, window, err := parseUsageBreakdownQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	response := UsageBreakdownResponse{Coverage: usageCoverageFrom(daemonUsageRecorder.Status()), Window: window,
		GroupBy: query.GroupBy, Bucket: query.Bucket, Groups: []UsageBreakdownGroup{}}
	index, release, err := usageReadIndex()
	if err != nil {
		response.Coverage = UsageCoverage{State: usageCoverageUnavailable}
		writeJSON(w, response)
		return
	}
	defer release()
	work, err := currentUsageWork(index, daemonUsageRecorder.Status().AsOf)
	if err != nil {
		response.Coverage = UsageCoverage{State: usageCoverageUnavailable}
		writeJSON(w, response)
		return
	}
	setUsageAgentCoverage(&response.Coverage, work)
	if work.Agents == usageAgentsUnavailable && usageQuerySplitsWork(query) {
		writeJSON(w, response) // the split is withheld, never shown with agent calls as main (S-4)
		return
	}
	query.Agents = work.Work.Keys
	groups, err := index.UsageGroups(query)
	if err != nil {
		response.Coverage = UsageCoverage{State: usageCoverageUnavailable}
		writeJSON(w, response)
		return
	}
	limit := usageHistoryConfig().Report.MaxGroups
	if len(groups) > limit {
		response.OmittedGroups = len(groups) - limit
		groups = groups[:limit]
	}
	for _, group := range groups {
		response.Groups = append(response.Groups, usageBreakdownGroup(group))
	}
	writeJSON(w, response)
}

// usageQuerySplitsWork reports whether a breakdown depends on the agent split.
func usageQuerySplitsWork(query store.UsageQuery) bool {
	if query.Delegation != "" {
		return true
	}
	for _, dimension := range query.GroupBy {
		switch dimension {
		case store.UsageDimDelegation, store.UsageDimMember, store.UsageDimAgentType:
			return true
		}
	}
	return false
}

func parseUsageBreakdownQuery(r *http.Request) (store.UsageQuery, UsageWindow, error) {
	values := r.URL.Query()
	query := store.UsageQuery{Runtime: values.Get("runtime"), Delegation: values.Get("delegation"),
		Bucket: values.Get("bucket")}
	window := UsageWindow{From: values.Get("from"), To: values.Get("to")}
	for _, bound := range []struct {
		value  string
		target *int64
	}{{window.From, &query.FromMS}, {window.To, &query.ToMS}} {
		if bound.value == "" {
			continue
		}
		day, err := time.Parse(usageDateLayout, bound.value)
		if err != nil {
			return query, window, fmt.Errorf("from and to are UTC dates (YYYY-MM-DD): %q", bound.value)
		}
		*bound.target = day.UTC().UnixMilli()
	}
	grouped := map[string]bool{}
	for _, dimension := range values["group"] {
		grouped[dimension] = true
	}
	// Effort and client values mean different things per runtime, so they are
	// never merged across runtimes (plan §3.7, red-team T-RT11).
	if grouped[store.UsageDimEffort] || grouped[store.UsageDimClient] {
		grouped[store.UsageDimRuntime] = true
	}
	for dimension := range grouped {
		query.GroupBy = append(query.GroupBy, dimension)
	}
	sort.Strings(query.GroupBy)
	if query.GroupBy == nil {
		query.GroupBy = []string{}
	}
	// Validate now so a bad dimension or filter is the caller's error.
	if err := store.ValidateUsageQuery(query); err != nil {
		return query, window, err
	}
	return query, window, nil
}

func usageBreakdownGroup(group store.UsageGroup) UsageBreakdownGroup {
	key := group.Key
	out := UsageBreakdownGroup{Key: UsageBreakdownKey{Runtime: key.Runtime, Model: key.Model, Effort: key.Effort,
		Client: key.Client, Delegation: key.Delegation, Member: key.Member, Session: key.Session,
		AgentType: key.AgentType, Bucket: key.Bucket},
		Calls: group.Calls, Sessions: group.Sessions,
		Input:               usageClassTotal(group.Input),
		CacheRead:           usageClassTotal(group.CacheRead),
		CacheWrite:          usageClassTotal(group.CacheWrite),
		Output:              usageClassTotal(group.Output),
		Reasoning:           usageClassTotal(group.Reasoning),
		ReasoningShareCalls: group.ShareCalls, ContextCalls: group.ContextCalls,
		Other: usageOtherTotals(group.Other)}
	out.ReasoningShare = usageRatio(group.ShareReasoning, group.ShareOutput)
	out.ReasoningPerCall = usageRatio(group.Reasoning.Sum, group.Reasoning.Stated)
	out.ContextPerCall = usageRatio(group.ContextSum, group.ContextCalls)
	// A hit rate needs every call to state all three input classes.
	if group.Input.Stated == group.Calls && group.CacheRead.Stated == group.Calls && group.CacheWrite.Stated == group.Calls {
		out.CacheHitRate = usageRatio(group.CacheRead.Sum, group.Input.Sum+group.CacheRead.Sum+group.CacheWrite.Sum)
	}
	for _, part := range group.Parts {
		out.Parts = append(out.Parts, UsagePartTotal{Of: part.Of, ID: part.ID, Label: part.Label, Count: part.Count})
	}
	for _, line := range group.Cost {
		out.Cost = append(out.Cost, UsageCostLine{Unit: line.Unit, Basis: line.Basis, Amount: line.Amount, Calls: line.Calls})
	}
	for _, reader := range group.Readers {
		out.Readers = append(out.Readers, UsageReaderCalls{Reader: reader.Reader, Calls: reader.Calls})
	}
	return out
}

func usageClassTotal(sum store.UsageClassSum) UsageClassTotal {
	return UsageClassTotal{Sum: sum.Sum, StatedCalls: sum.Stated}
}

func usageRatio(numerator, denominator int64) *float64 {
	if denominator <= 0 {
		return nil
	}
	ratio := float64(numerator) / float64(denominator)
	return &ratio
}

func usageCalls(records []store.UsageCallRecord) []harvest.UsageCall {
	calls := make([]harvest.UsageCall, 0, len(records))
	for _, record := range records {
		call := harvest.UsageCall{ID: record.CallID, Session: record.SessionID, Agent: record.Agent,
			ParentSession: record.ParentSessionID, FirstAt: time.UnixMilli(record.FirstAtMS),
			At: time.UnixMilli(record.AtMS), Model: record.Model, Effort: record.Effort, Client: record.Client,
			ContextWindow: record.ContextWindow,
			TokenClasses: harvest.TokenClasses{Input: record.Input, CacheRead: record.CacheRead,
				CacheWrite: record.CacheWrite, Output: record.Output, Reasoning: record.Reasoning}}
		if record.Cost != nil {
			call.Cost = &harvest.Cost{Amount: record.Cost.Amount, Unit: record.Cost.Unit, Basis: record.Cost.Basis}
		}
		if strings.TrimSpace(record.Other) != "" {
			_ = json.Unmarshal([]byte(record.Other), &call.Other) // the store wrote this canonical form itself
		}
		calls = append(calls, call)
	}
	return calls
}
