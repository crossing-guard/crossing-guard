package daemon

// Whose work a session's usage is (session usage breakdown plan §5.3): the
// session's own calls (main), its native subagents' calls, and the calls of
// Crossing Guard agents working for it. One classifier snapshot per recorder
// pass (rule 7) answers every request; a session's footer and its Usage pane
// fold the same read, so they agree at the same as-of (invariant 8).

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// Agent classification states, reported as coverage.agents.
const (
	usageAgentsCurrent     = "current"
	usageAgentsIncomplete  = "incomplete"
	usageAgentsUnavailable = "unavailable"
)

// usageWork is one classifier snapshot. Agents is how complete the agent
// split is; when it is unavailable, Work knows no agents and every view and
// report must withhold the agent split rather than count agent work as main.
type usageWork struct {
	Work   *store.UsageWorkIndex
	Agents string
}

// usageWorkCache builds the snapshot once per recorder pass. Its staleness is
// bounded by recorder.interval_seconds; it adds no tunable.
var usageWorkCache struct {
	mu    sync.Mutex
	index *store.Index
	asOf  time.Time
	built bool
	value usageWork
}

// usageAgentSessions reads which sessions are Crossing Guard agents. A
// variable so a test can make the read fail.
var usageAgentSessions = func(index *store.Index, limit int) (map[string]store.AgentSessionParent, bool, error) {
	return index.AllAgentSessionParents(limit)
}

// currentUsageWork returns the snapshot for the recorder's latest pass,
// building it under one lock so concurrent requests share one build.
func currentUsageWork(index *store.Index, asOf time.Time) (usageWork, error) {
	usageWorkCache.mu.Lock()
	defer usageWorkCache.mu.Unlock()
	if usageWorkCache.built && usageWorkCache.index == index && usageWorkCache.asOf.Equal(asOf) {
		return usageWorkCache.value, nil
	}
	nodes, err := index.UsageSourceNodes()
	if err != nil {
		return usageWork{}, err
	}
	work := usageWork{Agents: usageAgentsCurrent}
	agents, cut, err := usageAgentSessions(index, usageHistoryConfig().Report.AgentSessionsMax)
	switch {
	case err != nil:
		agents, work.Agents = nil, usageAgentsUnavailable
	case cut:
		work.Agents = usageAgentsIncomplete
	}
	work.Work = store.NewUsageWorkIndex(nodes, agents)
	usageWorkCache.index, usageWorkCache.asOf, usageWorkCache.built, usageWorkCache.value = index, asOf, true, work
	return work, nil
}

// usageSessionRead is one session's view and its classified calls: kind[i]
// and member[i] describe calls[i].
type usageSessionRead struct {
	work   usageWork
	view   store.UsageView
	self   store.UsageSessionRef
	kinds  map[string][]store.UsageCallRecord
	calls  []store.UsageCallRecord
	kind   []string
	member []string
}

// readSessionUsage reads everything a session's usage covers once.
func readSessionUsage(index *store.Index, s SessionSummary, status usageRecorderStatus) (usageSessionRead, error) {
	work, err := currentUsageWork(index, status.AsOf)
	if err != nil {
		return usageSessionRead{}, err
	}
	// Delegated calls are those of sessions whose parent is one of THIS session's
	// names. A subagent's thread id names its parent, whose other children are the
	// subagent's siblings, not its delegates (child-thread-identity plan).
	var names []string
	for _, name := range sessionIdentities(s) {
		if name != s.ID {
			names = append(names, name)
		}
	}
	self := store.UsageSessionRef{Runtime: s.Runtime, SessionID: s.ID}
	read := usageSessionRead{work: work, self: self, view: work.Work.View(self, names),
		kinds: map[string][]store.UsageCallRecord{}}
	calls, err := index.UsageCallsForSessions(read.view.Sessions)
	if err != nil {
		return usageSessionRead{}, err
	}
	read.calls = calls
	for _, call := range calls {
		kind, member := work.Work.Classify(read.view, call)
		read.kinds[kind] = append(read.kinds[kind], call)
		read.kind, read.member = append(read.kind, kind), append(read.member, member)
	}
	return read, nil
}

func (read usageSessionRead) asOf() time.Time {
	if ms := read.work.Work.AsOf(read.view); ms > 0 {
		return time.UnixMilli(ms).UTC()
	}
	return time.Time{}
}

// members groups one kind's calls by who did the work, in first-call order.
func (read usageSessionRead) members(kind string) ([]string, map[string][]store.UsageCallRecord) {
	byMember := map[string][]store.UsageCallRecord{}
	var order []string
	for index, call := range read.calls {
		if read.kind[index] != kind {
			continue
		}
		member := read.member[index]
		if byMember[member] == nil {
			order = append(order, member)
		}
		byMember[member] = append(byMember[member], call)
	}
	return order, byMember
}

// recordedSessionUsage is a session's recorded usage for its detail (plan
// D-6): its own calls, and apart from them its subagents' and its agents'. The
// model and context levels stay the live summary's. ok is false when nothing
// is recorded for the session yet.
func recordedSessionUsage(s SessionSummary) (*harvest.SessionUsage, bool) {
	if governor == nil || governor.ix == nil {
		return nil, false
	}
	read, err := readSessionUsage(governor.ix, s, daemonUsageRecorder.Status())
	if err != nil || len(read.calls) == 0 {
		return nil, false
	}
	usage := harvest.FoldCalls(usageCalls(read.kinds[store.UsageDelegationMain]))
	if usage == nil {
		usage = &harvest.SessionUsage{} // agents or subagents but no own call: "no calls recorded"
	}
	for _, part := range []struct {
		kind string
		into **harvest.SessionUsage
	}{{store.UsageDelegationSubagent, &usage.Delegated}, {store.UsageDelegationAgent, &usage.Agents}} {
		if folded := harvest.FoldCalls(usageCalls(read.kinds[part.kind])); folded != nil {
			order, _ := read.members(part.kind)
			folded.Children = len(order)
			*part.into = folded
		}
	}
	usage.AsOf = read.asOf()
	if s.Model != "" {
		usage.Model = s.Model
	}
	if s.Context > 0 {
		usage.Context = s.Context
	}
	return usage, true
}

// SessionUsageKind is one kind of work's totals. Total is every stated token,
// the Usage page's Total (plan D-6). A ratio is absent, never zero, when its
// denominator is zero.
type SessionUsageKind struct {
	Calls                int                  `json:"calls"`
	InputTokens          int64                `json:"input_tokens"`
	CacheRead            int64                `json:"cache_read"`
	CacheCreate          int64                `json:"cache_create"`
	OutputTokens         int64                `json:"output_tokens"`
	Reasoning            *int64               `json:"reasoning_tokens,omitempty"`
	ReasoningStatedCalls int                  `json:"reasoning_stated_calls"`
	CacheHitRate         *float64             `json:"cache_hit_rate,omitempty"`
	Other                []harvest.TokenCount `json:"other,omitempty"`
	Total                int64                `json:"total"`
	FirstAt              time.Time            `json:"first_at,omitzero"`
	LastAt               time.Time            `json:"last_at,omitzero"`
	PeakContext          int64                `json:"peak_context"`
	Models               []string             `json:"models,omitempty"`
}

// SessionUsageMember is one subagent: a Claude-style agent or a child session,
// named by the id Related sessions uses for it (invariant 7).
type SessionUsageMember struct {
	ID      string `json:"id"`
	Runtime string `json:"runtime"`
	Role    string `json:"role,omitempty"`
	// Parent is the member this one hangs from inside the view; "" at the
	// view's top level or when no parent is stated.
	Parent string `json:"parent,omitempty"`
	// Depth is the runtime's stated depth; absent when it states none.
	Depth       *int   `json:"depth,omitempty"`
	Description string `json:"description,omitempty"`
	SessionUsageKind
}

// SessionUsageAgent is one Crossing Guard agent working for the session.
type SessionUsageAgent struct {
	ID               string   `json:"id"`
	Runtime          string   `json:"runtime"`
	Role             string   `json:"role,omitempty"`
	Profiles         []string `json:"profiles,omitempty"`
	Openable         bool     `json:"openable"`
	CallTimes        []int64  `json:"call_times"`
	CallTimesOmitted int      `json:"call_times_omitted"`
	SessionUsageKind
}

// SessionUsageMain is the session's own calls.
type SessionUsageMain struct {
	SessionUsageKind
	Series []store.UsageSeriesPoint `json:"series"`
	// SeriesUnavailable is true when the series could not be read; the
	// figures above still stand (C-17).
	SeriesUnavailable bool `json:"series_unavailable,omitempty"`
}

// SessionUsageBreakdown is GET /api/session/usage.
type SessionUsageBreakdown struct {
	Coverage         UsageCoverage        `json:"coverage"`
	AsOf             time.Time            `json:"as_of,omitzero"`
	Main             SessionUsageMain     `json:"main"`
	Subagent         SessionUsageKind     `json:"subagent"`
	Agent            *SessionUsageKind    `json:"agent,omitempty"`
	Subagents        []SessionUsageMember `json:"subagents"`
	SubagentsOmitted int                  `json:"subagents_omitted"`
	Agents           []SessionUsageAgent  `json:"agents"`
	AgentsOmitted    int                  `json:"agents_omitted"`
}

func handleSessionUsage(w http.ResponseWriter, r *http.Request) {
	runtime, id := r.URL.Query().Get("runtime"), r.URL.Query().Get("id")
	if runtime == "" || id == "" {
		http.Error(w, "runtime and id are required", http.StatusBadRequest)
		return
	}
	status := daemonUsageRecorder.Status()
	response := SessionUsageBreakdown{Coverage: usageCoverageFrom(status), Subagents: []SessionUsageMember{},
		Agents: []SessionUsageAgent{}, Main: SessionUsageMain{Series: []store.UsageSeriesPoint{}}}
	index, release, err := usageReadIndex()
	if err != nil {
		response.Coverage = UsageCoverage{State: usageCoverageUnavailable}
		writeJSON(w, response)
		return
	}
	defer release()
	summary, found := harvest.Find(runtime, id)
	if !found {
		summary = SessionSummary{Runtime: runtime, ID: id} // the transcript may be gone; the record is not
	}
	lineage := harvest.LineageFacts{}
	if found {
		lineage, _ = harvest.Lineage(summary) // only a found session's own files; never a path from the query (C-15)
	}
	read, err := readSessionUsage(index, summary, status)
	if err != nil {
		response.Coverage = UsageCoverage{State: usageCoverageUnavailable}
		writeJSON(w, response)
		return
	}
	cfg := usageHistoryConfig().Report
	setUsageAgentCoverage(&response.Coverage, read.work)
	response.AsOf = read.asOf()
	response.Main.SessionUsageKind = usageKindOf(read.kinds[store.UsageDelegationMain])
	response.Main.Series, err = index.UsageMainSeries(read.self, cfg.SessionSeriesPoints)
	response.Main.SeriesUnavailable = err != nil
	if response.Main.Series == nil {
		response.Main.Series = []store.UsageSeriesPoint{}
	}
	response.Subagent = usageKindOf(read.kinds[store.UsageDelegationSubagent])
	response.Subagents, response.SubagentsOmitted = sessionUsageSubagents(read, lineage, cfg.SessionMembersMax)
	if read.work.Agents != usageAgentsUnavailable {
		agent := usageKindOf(read.kinds[store.UsageDelegationAgent])
		response.Agent = &agent
		response.Agents, response.AgentsOmitted = sessionUsageAgents(read, cfg.SessionMembersMax, cfg.SessionAgentTicks)
	}
	writeJSON(w, response)
}

// usageKindOf folds one kind's calls with the session aggregator.
func usageKindOf(records []store.UsageCallRecord) SessionUsageKind {
	kind := SessionUsageKind{}
	folded := harvest.FoldCalls(usageCalls(records))
	if folded == nil {
		return kind
	}
	kind.Calls, kind.InputTokens, kind.CacheRead = folded.Turns, folded.InputTokens, folded.CacheRead
	kind.CacheCreate, kind.OutputTokens, kind.Reasoning = folded.CacheCreate, folded.OutputTokens, folded.Reasoning
	kind.ReasoningStatedCalls, kind.Other = folded.ReasoningStatedCalls, folded.Other
	kind.CacheHitRate = usageRatio(folded.CacheRead, folded.InputTokens+folded.CacheRead+folded.CacheCreate)
	kind.Total = folded.InputTokens + folded.CacheRead + folded.CacheCreate + folded.OutputTokens
	for _, other := range folded.Other {
		kind.Total += other.Count
	}
	models := map[string]bool{}
	for index, record := range records {
		if index == 0 || record.FirstAtMS < kind.FirstAt.UnixMilli() {
			kind.FirstAt = time.UnixMilli(record.FirstAtMS).UTC()
		}
		kind.LastAt = time.UnixMilli(max(kind.LastAt.UnixMilli(), record.AtMS)).UTC()
		context := derefInt64(record.Input) + derefInt64(record.CacheRead) + derefInt64(record.CacheWrite)
		kind.PeakContext = max(kind.PeakContext, context)
		if record.Model != "" && !models[record.Model] {
			models[record.Model] = true
			kind.Models = append(kind.Models, record.Model)
		}
	}
	sort.Strings(kind.Models)
	return kind
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// sessionUsageSubagents lists the view's subagents, largest by Total first,
// with the labels lineage states for them at request time (never stored).
// Only the members kept after the cut are looked up (C-15).
func sessionUsageSubagents(read usageSessionRead, facts harvest.LineageFacts, limit int) ([]SessionUsageMember, int) {
	order, byMember := read.members(store.UsageDelegationSubagent)
	children := map[string]harvest.LineageChild{}
	for _, child := range facts.Children {
		children[child.ID] = child
	}
	out := make([]SessionUsageMember, 0, len(order))
	for _, id := range order {
		calls := byMember[id]
		out = append(out, SessionUsageMember{ID: id, Runtime: calls[0].Runtime, SessionUsageKind: usageKindOf(calls),
			Role: read.work.Work.SourceRole(calls[0].Runtime, calls[0].Source)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	omitted := 0
	if len(out) > limit {
		out, omitted = out[:limit], len(out)-limit
	}
	sessions := map[string][]string{} // child sessions kept, by runtime
	for index := range out {
		first := byMember[out[index].ID][0]
		if first.Agent != "" {
			if child, ok := children[first.Agent]; ok && child.Depth > 0 {
				out[index].Depth = &child.Depth
			}
			continue
		}
		out[index].Parent = sessionUsageParentID(read, store.UsageSessionRef{Runtime: first.Runtime, SessionID: first.SessionID})
		sessions[first.Runtime] = append(sessions[first.Runtime], first.SessionID)
	}
	// One listing per runtime for the kept children, never a scan per member.
	found := map[[2]string]harvest.SessionSummary{}
	for runtime, ids := range sessions {
		for id, summary := range harvest.FindListed(runtime, ids) {
			found[[2]string{runtime, id}] = summary
		}
	}
	for index := range out {
		first := byMember[out[index].ID][0]
		if child, ok := found[[2]string{first.Runtime, first.SessionID}]; ok && first.Agent == "" {
			if lineage, ok := harvest.Lineage(child); ok && lineage.Depth > 0 {
				out[index].Depth = &lineage.Depth
			}
		}
	}
	return out, omitted
}

// sessionUsageParentID names a child session's parent as a member, when the
// parent is itself in the view below the viewed session.
func sessionUsageParentID(read usageSessionRead, session store.UsageSessionRef) string {
	parent, ok := read.work.Work.Parent(read.view, session)
	if !ok {
		return ""
	}
	if alias := read.work.Work.Alias(parent); alias != "" {
		return alias
	}
	return parent.SessionID
}

// sessionUsageAgents lists the agents serving the view, largest by Total
// first, each with its call times (every n-th past the bound).
func sessionUsageAgents(read usageSessionRead, limit, ticks int) ([]SessionUsageAgent, int) {
	order, byMember := read.members(store.UsageDelegationAgent)
	out := make([]SessionUsageAgent, 0, len(order))
	for _, id := range order {
		calls := byMember[id]
		key, _ := read.work.Work.Agent(store.UsageSessionRef{Runtime: calls[0].Runtime, SessionID: calls[0].SessionID})
		agent := SessionUsageAgent{ID: id, Runtime: key.Session.Runtime, Role: key.Role, Profiles: key.Profiles,
			SessionUsageKind: usageKindOf(calls), CallTimes: []int64{}}
		step := (len(calls) + ticks - 1) / ticks
		for index, call := range calls {
			if index%max(step, 1) == 0 {
				agent.CallTimes = append(agent.CallTimes, call.AtMS)
			}
		}
		agent.CallTimesOmitted = len(calls) - len(agent.CallTimes)
		out = append(out, agent)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	omitted := 0
	if len(out) > limit {
		out, omitted = out[:limit], len(out)-limit
	}
	// Openable: one listing per runtime for the kept agents (C-15).
	ids := map[string][]string{}
	for _, agent := range out {
		ids[agent.Runtime] = append(ids[agent.Runtime], agent.ID)
	}
	found := map[[2]string]bool{}
	for runtime, list := range ids {
		for id := range harvest.FindListed(runtime, list) {
			found[[2]string{runtime, id}] = true
		}
	}
	for index := range out {
		out[index].Openable = found[[2]string{out[index].Runtime, out[index].ID}]
	}
	return out, omitted
}
