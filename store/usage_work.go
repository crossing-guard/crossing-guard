package store

// Whose work a recorded call is (session usage breakdown plan §5.2-§5.3).
// A session's usage splits into main (its own calls), subagent (native
// children's calls) and agent (calls of Crossing Guard agent sessions working
// for it). UsageWorkIndex is the one resolver of the parent links sources
// state: roots, a session's subtree, and which sessions are agents and whom
// they serve. The daemon builds it once per recorder pass from
// UsageSourceNodes and AllAgentSessionParents; the SQL classification reads
// its agent keys (usageWith), and views of one session classify in Go.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// UsageSessionRef names one recorded session: a runtime and the session id
// its usage rows carry.
type UsageSessionRef struct {
	Runtime   string `json:"runtime"`
	SessionID string `json:"session_id"`
}

func (s UsageSessionRef) less(other UsageSessionRef) bool {
	if s.Runtime != other.Runtime {
		return s.Runtime < other.Runtime
	}
	return s.SessionID < other.SessionID
}

// UsageAgentKey is one agent session, or a recorded descendant of one, and
// whom it served. Agent is the agent session's native id, the id Related
// sessions uses; Served is the session it worked for, and Root that
// session's root.
type UsageAgentKey struct {
	Session  UsageSessionRef
	Agent    string
	Role     string
	Served   UsageSessionRef
	Root     UsageSessionRef
	Profiles []string
}

// UsageSourceNode is one source's graph facts.
type UsageSourceNode struct {
	Session             UsageSessionRef
	Source              string
	Alias, Parent, Role string
	Complete            bool
	UpdatedAtMS         int64
}

// UsageSourceNodes returns every recorded source's graph facts.
func (ix *Index) UsageSourceNodes() ([]UsageSourceNode, error) {
	rows, err := ix.db.Query(`SELECT runtime,session_id,source,session_alias,parent_session_id,role,complete,
		updated_at_ms FROM usage_source ORDER BY runtime,session_id,source`)
	if err != nil {
		return nil, fmt.Errorf("read usage sources: %w", err)
	}
	defer rows.Close()
	var out []UsageSourceNode
	for rows.Next() {
		var node UsageSourceNode
		if err := rows.Scan(&node.Session.Runtime, &node.Session.SessionID, &node.Source, &node.Alias, &node.Parent,
			&node.Role, &node.Complete, &node.UpdatedAtMS); err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, rows.Err()
}

// UsageAgentNotes counts what the classifier met: agent sessions with no
// recorded source yet (nothing of theirs to classify), agents whose served
// session has none (kept as agent work under the named session), parent
// cycles (each resolved to its smallest session), and aliases two sessions
// share (resolved to the smaller). Cycles and shared aliases make the split
// incomplete (code red-team C-3, C-7, C-8).
type UsageAgentNotes struct {
	UnrecordedAgents int `json:"unrecorded_agents,omitempty"`
	UnrecordedServed int `json:"unrecorded_served,omitempty"`
	Cycles           int `json:"cycles,omitempty"`
	SharedAliases    int `json:"shared_aliases,omitempty"`
}

// Incomplete reports whether the notes mean some work may sit under the
// wrong session.
func (n UsageAgentNotes) Incomplete() bool { return n.Cycles > 0 || n.SharedAliases > 0 }

// usageSessionFacts are one session's facts across its sources.
type usageSessionFacts struct {
	alias       string
	rawParents  []string
	complete    bool
	updatedAtMS int64
}

// UsageWorkIndex resolves parents, roots and agents over the recorded sources.
type UsageWorkIndex struct {
	sessions map[UsageSessionRef]*usageSessionFacts
	names    map[[2]string]UsageSessionRef
	parent   map[UsageSessionRef]UsageSessionRef
	children map[UsageSessionRef][]UsageSessionRef
	agents   map[UsageSessionRef]UsageAgentKey
	roles    map[[2]string]string // (runtime, source) -> role
	// Keys are every agent key, sorted by session.
	Keys  []UsageAgentKey
	Notes UsageAgentNotes
}

// NewUsageWorkIndex builds the resolver from the sources and the agent
// sessions the orchestration store knows (AllAgentSessionParents).
func NewUsageWorkIndex(nodes []UsageSourceNode, agents map[string]AgentSessionParent) *UsageWorkIndex {
	w := &UsageWorkIndex{sessions: map[UsageSessionRef]*usageSessionFacts{}, names: map[[2]string]UsageSessionRef{},
		parent: map[UsageSessionRef]UsageSessionRef{}, children: map[UsageSessionRef][]UsageSessionRef{},
		agents: map[UsageSessionRef]UsageAgentKey{}, roles: map[[2]string]string{}}
	w.addSessions(nodes)
	w.linkParents()
	w.countCycles()
	w.addAgents(agents)
	return w
}

// sortedSessions lists the recorded sessions in a stable order.
func (w *UsageWorkIndex) sortedSessions() []UsageSessionRef {
	out := make([]UsageSessionRef, 0, len(w.sessions))
	for session := range w.sessions {
		out = append(out, session)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

func (w *UsageWorkIndex) addSessions(nodes []UsageSourceNode) {
	for _, node := range nodes {
		facts := w.sessions[node.Session]
		if facts == nil {
			facts = &usageSessionFacts{complete: true}
			w.sessions[node.Session] = facts
		}
		if facts.alias == "" {
			facts.alias = node.Alias
		}
		if node.Role != "" {
			w.roles[[2]string{node.Session.Runtime, node.Source}] = node.Role
		}
		if node.Parent != "" && !slices.Contains(facts.rawParents, node.Parent) {
			facts.rawParents = append(facts.rawParents, node.Parent)
		}
		facts.complete = facts.complete && node.Complete
		facts.updatedAtMS = max(facts.updatedAtMS, node.UpdatedAtMS)
	}
	// A session id names its session before any alias does, so an alias that
	// equals another session's id never captures it; an alias two sessions
	// share names the smaller (C-8).
	ordered := w.sortedSessions()
	for _, session := range ordered {
		w.names[[2]string{session.Runtime, session.SessionID}] = session
	}
	for _, session := range ordered {
		alias := w.sessions[session].alias
		key := [2]string{session.Runtime, alias}
		if alias == "" {
			continue
		}
		if holder, taken := w.names[key]; taken {
			if holder != session {
				w.Notes.SharedAliases++
			}
			continue
		}
		w.names[key] = session
	}
}

// linkParents gives each session one parent: the smallest of the sessions its
// sources name, so a session whose sources disagree still has one root and no
// call is counted twice (S-5). A parent that names no recorded session leaves
// the session an orphan, its own root.
func (w *UsageWorkIndex) linkParents() {
	for session, facts := range w.sessions {
		var chosen *UsageSessionRef
		for _, raw := range facts.rawParents {
			parent, ok := w.Resolve(session.Runtime, raw)
			if !ok || parent == session {
				continue
			}
			if chosen == nil || parent.less(*chosen) {
				chosen = &parent
			}
		}
		if chosen != nil {
			w.parent[session] = *chosen
			w.children[*chosen] = append(w.children[*chosen], session)
		}
	}
	for parent := range w.children {
		sort.Slice(w.children[parent], func(i, j int) bool { return w.children[parent][i].less(w.children[parent][j]) })
	}
}

// Resolve finds the recorded session a name refers to: its session id, or a
// non-empty alias.
func (w *UsageWorkIndex) Resolve(runtime, name string) (UsageSessionRef, bool) {
	if name == "" {
		return UsageSessionRef{}, false
	}
	session, ok := w.names[[2]string{runtime, name}]
	return session, ok
}

// Alias is a session's canonical other id, "" when it has none.
func (w *UsageWorkIndex) Alias(session UsageSessionRef) string {
	if facts := w.sessions[session]; facts != nil {
		return facts.alias
	}
	return ""
}

// SourceRole is a source's recorded vendor role, "" when none is stated. A
// Claude subagent's calls share their parent's session, so a role belongs to
// the source, never the session.
func (w *UsageWorkIndex) SourceRole(runtime, source string) string {
	return w.roles[[2]string{runtime, source}]
}

// nativeRoot walks parent links up to a session with none. Every member of a
// parent cycle gets the cycle's smallest session as its root, so a cycle is
// one row, never one per member (C-7).
func (w *UsageWorkIndex) nativeRoot(session UsageSessionRef) (UsageSessionRef, bool) {
	path := []UsageSessionRef{session}
	at := map[UsageSessionRef]int{session: 0}
	for {
		parent, ok := w.parent[session]
		if !ok {
			return session, false
		}
		if index, seen := at[parent]; seen {
			smallest := path[index]
			for _, member := range path[index:] {
				if member.less(smallest) {
					smallest = member
				}
			}
			return smallest, true
		}
		at[parent] = len(path)
		path = append(path, parent)
		session = parent
	}
}

// countCycles counts distinct parent cycles once.
func (w *UsageWorkIndex) countCycles() {
	cycles := map[UsageSessionRef]bool{}
	for session := range w.sessions {
		if root, cycle := w.nativeRoot(session); cycle {
			cycles[root] = true
		}
	}
	w.Notes.Cycles = len(cycles)
}

func (w *UsageWorkIndex) addAgents(agents map[string]AgentSessionParent) {
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	base := map[UsageSessionRef]UsageAgentKey{}
	for _, id := range ids {
		parent := agents[id]
		session, ok := w.Resolve(parent.Runtime, id)
		if !ok {
			w.Notes.UnrecordedAgents++ // nothing of it is recorded, so nothing to classify
			continue
		}
		served, ok := w.Resolve(parent.RootRuntime, parent.RootCatalogSessionID)
		if !ok {
			served, ok = w.Resolve(parent.RootRuntime, parent.RootNativeSessionID)
		}
		if !ok {
			// The served session has no recorded source (catching up, or pruned):
			// the agent's work stays agent work under the session it names (C-3).
			name := parent.RootCatalogSessionID
			if name == "" {
				name = parent.RootNativeSessionID
			}
			if name == "" {
				w.Notes.UnrecordedAgents++
				continue
			}
			served = UsageSessionRef{Runtime: parent.RootRuntime, SessionID: name}
			w.Notes.UnrecordedServed++
		}
		base[session] = UsageAgentKey{Session: session, Agent: id, Role: parent.Role, Served: served,
			Profiles: parent.Profiles}
	}
	for session, key := range base {
		key.Root = w.agentRoot(key.Served, base)
		w.agents[session] = key
	}
	// An agent session's recorded descendants are its work too: each takes its
	// nearest agent ancestor, the same on every build (C-5).
	for _, session := range w.sortedSessions() {
		if _, own := w.agents[session]; own {
			continue
		}
		if key, ok := w.nearestAgent(session); ok {
			key.Session = session
			w.agents[session] = key
		}
	}
	for _, key := range w.agents {
		w.Keys = append(w.Keys, key)
	}
	sort.Slice(w.Keys, func(i, j int) bool { return w.Keys[i].Session.less(w.Keys[j].Session) })
}

// nearestAgent is the agent key of a session's nearest native ancestor that
// is an agent session.
func (w *UsageWorkIndex) nearestAgent(session UsageSessionRef) (UsageAgentKey, bool) {
	seen := map[UsageSessionRef]bool{session: true}
	for {
		parent, ok := w.parent[session]
		if !ok || seen[parent] {
			return UsageAgentKey{}, false
		}
		if key, isAgent := w.agents[parent]; isAgent && key.Session == parent {
			return key, true
		}
		seen[parent] = true
		session = parent
	}
}

// agentRoot walks from a served session through native parents and through
// agents to a session that is neither.
func (w *UsageWorkIndex) agentRoot(served UsageSessionRef, base map[UsageSessionRef]UsageAgentKey) UsageSessionRef {
	seen := map[UsageSessionRef]bool{}
	session := served
	for {
		root, _ := w.nativeRoot(session)
		next, isAgent := base[root]
		if !isAgent || seen[root] {
			return root
		}
		seen[root] = true
		session = next.Served
	}
}

// descendants lists a session's native descendants, not the session itself.
// A session in stop is neither listed nor walked through.
func (w *UsageWorkIndex) descendants(session UsageSessionRef, stop map[UsageSessionRef]UsageAgentKey) []UsageSessionRef {
	seen := map[UsageSessionRef]bool{session: true}
	queue := []UsageSessionRef{session}
	var out []UsageSessionRef
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range w.children[current] {
			if seen[child] {
				continue
			}
			seen[child] = true
			if _, stopped := stop[child]; stopped {
				continue
			}
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

// Agent reports whether a session is agent work, and its key.
func (w *UsageWorkIndex) Agent(session UsageSessionRef) (UsageAgentKey, bool) {
	key, ok := w.agents[session]
	return key, ok
}

// Root is the session a session's calls roll up to on the Usage page: an
// agent's served root, else its native root (itself for an orphan).
func (w *UsageWorkIndex) Root(session UsageSessionRef) UsageSessionRef {
	if key, ok := w.agents[session]; ok {
		return key.Root
	}
	root, _ := w.nativeRoot(session)
	if key, ok := w.agents[root]; ok {
		return key.Root
	}
	return root
}

// UsageView is what one session's usage covers: its own calls, its native
// subtree, and the agent sessions (with their descendants) serving any
// session in that subtree.
type UsageView struct {
	Self     UsageSessionRef
	Tree     map[UsageSessionRef]bool
	Agents   map[UsageSessionRef]UsageAgentKey
	Sessions []UsageSessionRef
}

// View is session self's view. names are other ids a child may use to name
// self (its catalog meta and thread ids) that no recorded source states.
func (w *UsageWorkIndex) View(self UsageSessionRef, names []string) UsageView {
	view := UsageView{Self: self, Tree: map[UsageSessionRef]bool{self: true}, Agents: map[UsageSessionRef]UsageAgentKey{}}
	starts := []UsageSessionRef{self}
	// A child may name self by its own id even when self has no recorded
	// source yet (C-10).
	names = append([]string{self.SessionID}, names...)
	for session, facts := range w.sessions {
		if session.Runtime != self.Runtime || w.parent[session] != (UsageSessionRef{}) {
			continue
		}
		for _, raw := range facts.rawParents {
			if slices.Contains(names, raw) && session != self {
				view.Tree[session] = true
				starts = append(starts, session)
			}
		}
	}
	// Another agent's sessions are agent work, never this view's subtree. An
	// agent session viewed on its own keeps its own descendants as its
	// subagents (S-2), so its own family is not stopped.
	selfKey, selfIsAgent := w.agents[self]
	ownFamily := func(key UsageAgentKey) bool { return selfIsAgent && key.Agent == selfKey.Agent }
	stop := map[UsageSessionRef]UsageAgentKey{}
	for session, key := range w.agents {
		if session != self && !ownFamily(key) {
			stop[session] = key
		}
	}
	for _, start := range starts {
		for _, descendant := range w.descendants(start, stop) {
			view.Tree[descendant] = true
		}
	}
	// Agents serving the tree, and agents serving those agents, to a fixpoint:
	// the same rule the Usage page's roll-up applies (C-6).
	for added := true; added; {
		added = false
		for session, key := range w.agents {
			if _, in := view.Agents[session]; in || session == self || ownFamily(key) {
				continue
			}
			if _, servesAgent := view.Agents[key.Served]; view.Tree[key.Served] || servesAgent {
				view.Agents[session] = key
				added = true
			}
		}
	}
	for session := range view.Tree {
		view.Sessions = append(view.Sessions, session)
	}
	for session := range view.Agents {
		view.Sessions = append(view.Sessions, session)
	}
	sort.Slice(view.Sessions, func(i, j int) bool { return view.Sessions[i].less(view.Sessions[j]) })
	return view
}

// Classify names a call's kind within a view and who did the work: main is
// the viewed session's own calls; agent is an agent session serving the
// view; everything else in the view is subagent work (S-2).
func (w *UsageWorkIndex) Classify(view UsageView, call UsageCallRecord) (kind, member string) {
	session := UsageSessionRef{Runtime: call.Runtime, SessionID: call.SessionID}
	switch key, isAgent := view.Agents[session]; {
	case session == view.Self && call.Agent == "":
		return UsageDelegationMain, session.SessionID
	case isAgent:
		return UsageDelegationAgent, key.Agent
	case call.Agent != "":
		return UsageDelegationSubagent, call.Agent
	default:
		if alias := w.Alias(session); alias != "" {
			return UsageDelegationSubagent, alias
		}
		return UsageDelegationSubagent, session.SessionID
	}
}

// Parent is the session a view's child hangs from inside the view; false at
// the view's top level (its parent is the viewed session, or outside it).
func (w *UsageWorkIndex) Parent(view UsageView, session UsageSessionRef) (UsageSessionRef, bool) {
	parent, ok := w.parent[session]
	if !ok || parent == view.Self || !view.Tree[parent] {
		return UsageSessionRef{}, false
	}
	return parent, true
}

// AsOf is the latest write over the view's sources (S-6).
func (w *UsageWorkIndex) AsOf(view UsageView) int64 {
	var latest int64
	for _, session := range view.Sessions {
		if facts := w.sessions[session]; facts != nil {
			latest = max(latest, facts.updatedAtMS)
		}
	}
	return latest
}

// UsageCallsForSessions reads every call of the listed sessions, in time order.
func (ix *Index) UsageCallsForSessions(sessions []UsageSessionRef) ([]UsageCallRecord, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(sessions)
	if err != nil {
		return nil, err
	}
	return ix.queryUsageCalls(`SELECT `+usageCallColumns+` FROM usage_call INDEXED BY usage_call_session
		WHERE (runtime,session_id) IN (SELECT json_extract(value,'$.runtime'),json_extract(value,'$.session_id')
			FROM json_each(?))
		ORDER BY at_ms, call_id`, string(body))
}

// UsageSeriesPoint is one own call's time and stated input (its context).
type UsageSeriesPoint struct {
	AtMS    int64 `json:"at_ms"`
	Context int64 `json:"context"`
}

// UsageMainSeries returns a session's own calls as a context series. Above
// maxPoints it keeps each of maxPoints equal time buckets' last call.
func (ix *Index) UsageMainSeries(session UsageSessionRef, maxPoints int) ([]UsageSeriesPoint, error) {
	rows, err := ix.db.Query(`SELECT at_ms,COALESCE(input,0)+COALESCE(cache_read,0)+COALESCE(cache_write,0)
		FROM usage_call INDEXED BY usage_call_session WHERE runtime=? AND session_id=? AND agent=''
		ORDER BY at_ms, call_id`, session.Runtime, session.SessionID)
	if err != nil {
		return nil, fmt.Errorf("read usage series: %w", err)
	}
	defer rows.Close()
	var points []UsageSeriesPoint
	for rows.Next() {
		var point UsageSeriesPoint
		if err := rows.Scan(&point.AtMS, &point.Context); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return downsampleUsageSeries(points, maxPoints), nil
}

func downsampleUsageSeries(points []UsageSeriesPoint, maxPoints int) []UsageSeriesPoint {
	if maxPoints <= 0 || len(points) <= maxPoints {
		return points
	}
	first, last := points[0].AtMS, points[len(points)-1].AtMS
	span := max(last-first, 1)
	out := make([]UsageSeriesPoint, 0, maxPoints)
	bucket := -1
	for _, point := range points {
		index := int((point.AtMS - first) * int64(maxPoints-1) / span)
		if index == bucket && len(out) > 0 {
			out[len(out)-1] = point // the bucket's last call
			continue
		}
		bucket = index
		out = append(out, point)
	}
	return out
}

// UsageWorkSum is one kind of work's sums.
type UsageWorkSum struct {
	Calls                                           int64
	Input, CacheRead, CacheWrite, Output, Reasoning UsageClassSum
	Total                                           int64
}

func (sum *UsageWorkSum) add(group UsageGroup) {
	sum.Calls += group.Calls
	for _, pair := range []struct{ to, from *UsageClassSum }{{&sum.Input, &group.Input},
		{&sum.CacheRead, &group.CacheRead}, {&sum.CacheWrite, &group.CacheWrite}, {&sum.Output, &group.Output},
		{&sum.Reasoning, &group.Reasoning}} {
		pair.to.Sum += pair.from.Sum
		pair.to.Stated += pair.from.Stated
	}
	sum.Total += group.Total
}

// UsageRootTotal is one root session's work, rolled up from its subtree and
// the agents serving it, with the levels of its own latest call.
type UsageRootTotal struct {
	Root                  UsageSessionRef
	Main, Subagent, Agent UsageWorkSum
	Subagents, Agents     int
	AgentProfiles         []string
	Total                 int64
	LastContext, LastAtMS int64
	LastContextWindow     *int64
	LastModel             string
	Cost                  []UsageCostSum
	subagentSet, agentSet map[string]bool
}

// UsageWorkGroups is the one grouped pass the whole Usage page folds from:
// calls by runtime, session, kind, member, agent type and day, with other
// classes and cost. The totals, the days, the roll-up and the by-type card
// each sum these groups in Go; a second pass over the calls doubled the
// page's time at installed scale (code red-team C-4).
func (ix *Index) UsageWorkGroups(q UsageQuery, work *UsageWorkIndex) ([]UsageGroup, error) {
	q.GroupBy, q.Bucket, q.Details, q.Agents = []string{UsageDimRuntime, UsageDimSession, UsageDimDelegation,
		UsageDimMember, UsageDimAgentType}, UsageBucketDay, UsageDetailOther|UsageDetailCost, work.Keys
	return ix.UsageGroups(q)
}

// UsageRootTotals rolls every call up to its root session (P-4) and returns
// the limit largest roots by Total and by their own latest context.
func (ix *Index) UsageRootTotals(q UsageQuery, work *UsageWorkIndex, limit int) (byTotal, byContext []UsageRootTotal, err error) {
	groups, err := ix.UsageWorkGroups(q, work)
	if err != nil {
		return nil, nil, err
	}
	return ix.UsageRootTotalsFrom(q, groups, work, limit)
}

// UsageRootTotalsFrom is UsageRootTotals over groups UsageWorkGroups already read.
func (ix *Index) UsageRootTotalsFrom(q UsageQuery, groups []UsageGroup, work *UsageWorkIndex, limit int) (byTotal,
	byContext []UsageRootTotal, err error) {
	if limit <= 0 {
		return nil, nil, fmt.Errorf("%w: limit %d", ErrUsageQueryInvalid, limit)
	}
	roots := map[UsageSessionRef]*UsageRootTotal{}
	for _, group := range groups {
		session := UsageSessionRef{Runtime: group.Key.Runtime, SessionID: group.Key.Session}
		root := work.Root(session)
		total := roots[root]
		if total == nil {
			total = &UsageRootTotal{Root: root, subagentSet: map[string]bool{}, agentSet: map[string]bool{}}
			roots[root] = total
		}
		total.addGroup(group, work, session)
	}
	if err := ix.fillUsageRootLatest(q, roots); err != nil {
		return nil, nil, err
	}
	all := make([]UsageRootTotal, 0, len(roots))
	for _, total := range roots {
		total.Subagents, total.Agents = len(total.subagentSet), len(total.agentSet)
		sort.Strings(total.AgentProfiles)
		all = append(all, *total)
	}
	byTotal = topUsageRoots(all, limit, func(a, b UsageRootTotal) bool { return a.Total > b.Total })
	byContext = topUsageRoots(all, limit, func(a, b UsageRootTotal) bool { return a.LastContext > b.LastContext })
	return byTotal, byContext, nil
}

func (total *UsageRootTotal) addGroup(group UsageGroup, work *UsageWorkIndex, session UsageSessionRef) {
	switch group.Key.Delegation {
	case UsageDelegationAgent:
		total.Agent.add(group)
		total.agentSet[group.Key.Member] = true
		if key, ok := work.Agent(session); ok {
			for _, profile := range key.Profiles {
				if !slices.Contains(total.AgentProfiles, profile) {
					total.AgentProfiles = append(total.AgentProfiles, profile)
				}
			}
		}
	case UsageDelegationSubagent:
		total.Subagent.add(group)
		total.subagentSet[group.Key.Member] = true
	default:
		// A session's own calls are main on the Usage page only when it is
		// the root; a child's own calls are its root's subagent work.
		if session == total.Root {
			total.Main.add(group)
		} else {
			total.Subagent.add(group)
			if alias := work.Alias(session); alias != "" {
				total.subagentSet[alias] = true
			} else {
				total.subagentSet[session.SessionID] = true
			}
		}
	}
	total.Total += group.Total
	for _, cost := range group.Cost {
		merged := false
		for index := range total.Cost {
			if total.Cost[index].Unit == cost.Unit && total.Cost[index].Basis == cost.Basis {
				total.Cost[index].Amount += cost.Amount
				total.Cost[index].Calls += cost.Calls
				merged = true
			}
		}
		if !merged {
			total.Cost = append(total.Cost, cost)
		}
	}
}

// fillUsageRootLatest sets each root's own latest call levels in one grouped
// pass. With exactly one max() aggregate, SQLite takes the bare columns from
// the row holding it (code red-team C-3); the max is over time then call id,
// so a tie picks the same call every time (C-11).
func (ix *Index) fillUsageRootLatest(q UsageQuery, roots map[UsageSessionRef]*UsageRootTotal) error {
	q.GroupBy, q.Bucket, q.Delegation = nil, "", ""
	scope, err := q.scope()
	if err != nil {
		return err
	}
	rows, err := ix.db.Query(scope.with+`SELECT runtime,session_id,
			COALESCE(input,0)+COALESCE(cache_read,0)+COALESCE(cache_write,0),context_window,model,
			MAX(printf('%015d|%s',at_ms,call_id))
		FROM usage_call WHERE agent='' AND `+scope.where+` GROUP BY runtime,session_id`, scope.args...)
	if err != nil {
		return fmt.Errorf("read usage latest calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var session UsageSessionRef
		var context int64
		var window sql.NullInt64
		var model, latest string
		if err := rows.Scan(&session.Runtime, &session.SessionID, &context, &window, &model, &latest); err != nil {
			return err
		}
		total := roots[session]
		if total == nil {
			continue // not a root: its latest call is not its root's
		}
		total.LastContext, total.LastModel = context, model
		if window.Valid {
			total.LastContextWindow = &window.Int64
		}
		if at, _, found := strings.Cut(latest, "|"); found {
			total.LastAtMS, _ = strconv.ParseInt(at, 10, 64) // written with %015d
		}
	}
	return rows.Err()
}

// topUsageRoots returns the limit first roots under larger, ties broken by
// runtime and session id so the order is stable.
func topUsageRoots(all []UsageRootTotal, limit int, larger func(a, b UsageRootTotal) bool) []UsageRootTotal {
	sorted := append([]UsageRootTotal(nil), all...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if larger(sorted[i], sorted[j]) || larger(sorted[j], sorted[i]) {
			return larger(sorted[i], sorted[j])
		}
		return sorted[i].Root.less(sorted[j].Root)
	})
	return sorted[:min(limit, len(sorted))]
}

// UsageRootCounts counts root sessions: those with any recorded call, in all
// and per runtime, and those whose sources were read to their end with no
// call anywhere under them (the tile's no-data count, S-8).
func (ix *Index) UsageRootCounts(work *UsageWorkIndex) (withCalls int64, byRuntime map[string]int64, withoutCalls int64, err error) {
	rows, err := ix.db.Query(`SELECT DISTINCT runtime,session_id FROM usage_call`)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("count usage sessions: %w", err)
	}
	defer rows.Close()
	called := map[UsageSessionRef]bool{}
	for rows.Next() {
		var session UsageSessionRef
		if err := rows.Scan(&session.Runtime, &session.SessionID); err != nil {
			return 0, nil, 0, err
		}
		called[work.Root(session)] = true
	}
	if err := rows.Err(); err != nil {
		return 0, nil, 0, err
	}
	byRuntime = map[string]int64{}
	for root := range called {
		byRuntime[root.Runtime]++
		withCalls++
	}
	for session, facts := range work.sessions {
		if facts.complete && work.Root(session) == session && !called[session] {
			withoutCalls++
		}
	}
	return withCalls, byRuntime, withoutCalls, nil
}
