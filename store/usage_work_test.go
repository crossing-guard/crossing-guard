package store

import (
	"testing"
)

// testWorkIndex builds the work resolver the daemon builds, over this store's
// sources and the given agents.
func testWorkIndex(t *testing.T, ix *Index, agents map[string]AgentSessionParent) *UsageWorkIndex {
	t.Helper()
	nodes, err := ix.UsageSourceNodes()
	if err != nil {
		t.Fatal(err)
	}
	return NewUsageWorkIndex(nodes, agents)
}

type testSource struct {
	runtime, source, session, alias, parent, role string
	calls                                         []UsageCallRecord
}

func writeTestSource(t *testing.T, ix *Index, src testSource) {
	t.Helper()
	runtime := src.runtime
	if runtime == "" {
		runtime = "rt"
	}
	for index := range src.calls {
		src.calls[index].Runtime, src.calls[index].Source = runtime, src.source
		src.calls[index].SessionID, src.calls[index].ParentSessionID = src.session, src.parent
	}
	if _, err := ix.WriteUsageSource(UsageSourceWrite{Calls: src.calls, State: UsageSourceState{Runtime: runtime,
		Source: src.source, SessionID: src.session, SessionAlias: src.alias, ParentSession: src.parent,
		Role: src.role, Marker: "m", Reader: "rt/usage-1", Complete: true, UpdatedAtMS: 42}}); err != nil {
		t.Fatal(err)
	}
}

// fixture: root has its own call and a Claude-style subagent call; child
// (named by alias, parent named by root's thread id) has a grandchild; an
// agent session in another runtime serves root and has its own child.
func writeWorkFixture(t *testing.T, ix *Index) map[string]AgentSessionParent {
	t.Helper()
	sub := usageCall("", "", "a1", 1001, 2)
	sub.Agent = "agent-7"
	writeTestSource(t, ix, testSource{source: "root-file", session: "root", alias: "root-thread",
		calls: []UsageCallRecord{usageCall("", "", "r1", 1000, 1)}})
	writeTestSource(t, ix, testSource{source: "root-agent", session: "root", role: "general-purpose",
		calls: []UsageCallRecord{sub}})
	writeTestSource(t, ix, testSource{source: "child-file", session: "child", alias: "child-alias",
		parent: "root-thread", role: "guardian", calls: []UsageCallRecord{usageCall("", "", "c1", 1002, 3)}})
	writeTestSource(t, ix, testSource{source: "grand-file", session: "grand", parent: "child-alias",
		calls: []UsageCallRecord{usageCall("", "", "g1", 1003, 4)}})
	writeTestSource(t, ix, testSource{runtime: "other", source: "helper-file", session: "helper-stem",
		alias: "helper-native", calls: []UsageCallRecord{usageCall("", "", "h1", 1004, 5)}})
	writeTestSource(t, ix, testSource{runtime: "other", source: "helper-kid", session: "helper-kid",
		parent: "helper-native", calls: []UsageCallRecord{usageCall("", "", "k1", 1005, 6)}})
	return map[string]AgentSessionParent{"helper-native": {Runtime: "other", Role: "helper", RootRuntime: "rt",
		RootNativeSessionID: "root", Profiles: []string{"recall"}}}
}

func splitView(t *testing.T, ix *Index, work *UsageWorkIndex, self UsageSessionRef, names ...string) map[string][]string {
	t.Helper()
	view := work.View(self, names)
	calls, err := ix.UsageCallsForSessions(view.Sessions)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, call := range calls {
		kind, member := work.Classify(view, call)
		out[kind] = append(out[kind], call.CallID+"@"+member)
	}
	return out
}

func sameCalls(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func TestUsageViewSplitsMainSubagentAndAgentRelativeToTheViewedSession(t *testing.T) {
	ix := openUsageStore(t)
	work := testWorkIndex(t, ix, writeWorkFixture(t, ix))
	root := splitView(t, ix, work, UsageSessionRef{"rt", "root"})
	if !sameCalls(root["main"], "r1@root") ||
		!sameCalls(root["subagent"], "a1@agent-7", "c1@child-alias", "g1@grand") ||
		!sameCalls(root["agent"], "h1@helper-native", "k1@helper-native") {
		t.Fatalf("root view: %+v", root)
	}
	// A child's own page: its own calls are main, as its footer shows (S-2).
	child := splitView(t, ix, work, UsageSessionRef{"rt", "child"})
	if !sameCalls(child["main"], "c1@child") || !sameCalls(child["subagent"], "g1@grand") || len(child["agent"]) != 0 {
		t.Fatalf("child view: %+v", child)
	}
	// An agent session's own page: its calls are main, its child a subagent.
	helper := splitView(t, ix, work, UsageSessionRef{"other", "helper-stem"})
	if !sameCalls(helper["main"], "h1@helper-stem") || !sameCalls(helper["subagent"], "k1@helper-kid") ||
		len(helper["agent"]) != 0 {
		t.Fatalf("agent's own view: %+v", helper)
	}
	if got := work.Root(UsageSessionRef{"other", "helper-kid"}); got != (UsageSessionRef{"rt", "root"}) {
		t.Fatalf("an agent's child rolls up to the served root: %+v", got)
	}
	if work.AsOf(work.View(UsageSessionRef{"rt", "root"}, nil)) != 42 {
		t.Fatal("as-of covers the view's sources")
	}
}

// A helper that served a child session counts on that child's page and on its
// parent's, never on neither (S-2).
func TestUsageAgentServingAChildCountsOnTheChildAndItsParent(t *testing.T) {
	ix := openUsageStore(t)
	agents := writeWorkFixture(t, ix)
	agents["helper-native"] = AgentSessionParent{Runtime: "other", Role: "helper", RootRuntime: "rt",
		RootNativeSessionID: "child"}
	work := testWorkIndex(t, ix, agents)
	for _, self := range []string{"root", "child"} {
		split := splitView(t, ix, work, UsageSessionRef{"rt", self})
		if len(split["agent"]) != 2 {
			t.Fatalf("%s view agents: %+v", self, split)
		}
	}
	if len(splitView(t, ix, work, UsageSessionRef{"rt", "grand"})["agent"]) != 0 {
		t.Fatal("the agent serves child, not grand")
	}
}

// Every call is one kind, and the Usage page's kinds sum to its total.
func TestUsageGlobalClassificationSplitsTheTotalExactly(t *testing.T) {
	ix := openUsageStore(t)
	work := testWorkIndex(t, ix, writeWorkFixture(t, ix))
	groups, err := ix.UsageGroups(UsageQuery{GroupBy: []string{UsageDimDelegation, UsageDimAgentType}, Agents: work.Keys})
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int64{}
	var total int64
	for _, group := range groups {
		calls[group.Key.Delegation+"/"+group.Key.AgentType] += group.Calls
		total += group.Calls
	}
	if total != 6 || calls["main/"] != 1 || calls["subagent/general-purpose"] != 1 || calls["subagent/guardian"] != 1 ||
		calls["subagent/"] != 1 || calls["agent/recall"] != 2 {
		t.Fatalf("split: %+v total=%d", calls, total)
	}
	if _, err := ix.UsageGroups(UsageQuery{Delegation: "delegated"}); err == nil {
		t.Fatal("the retired value is refused")
	}
	agentOnly, err := ix.UsageGroups(UsageQuery{Delegation: UsageDelegationAgent, Agents: work.Keys,
		GroupBy: []string{UsageDimMember}})
	if err != nil || len(agentOnly) != 1 || agentOnly[0].Key.Member != "helper-native" || agentOnly[0].Calls != 2 {
		t.Fatalf("agent member: %+v err=%v", agentOnly, err)
	}
	top, _, err := ix.UsageRootTotals(UsageQuery{}, work, 5)
	if err != nil || len(top) != 1 || top[0].Main.Calls != 1 || top[0].Subagent.Calls != 3 || top[0].Agent.Calls != 2 ||
		top[0].Subagents != 3 || top[0].Agents != 1 || len(top[0].AgentProfiles) != 1 {
		t.Fatalf("root roll-up: %+v err=%v", top, err)
	}
	withCalls, byRuntime, _, err := ix.UsageRootCounts(work)
	if err != nil || withCalls != 1 || byRuntime["rt"] != 1 {
		t.Fatalf("root counts: %d %+v err=%v", withCalls, byRuntime, err)
	}
}

// A session whose sources state different parents still has one root, and a
// parent cycle ends without looping (S-5).
func TestUsageRootsAreOnePerSessionAndCycleSafe(t *testing.T) {
	ix := openUsageStore(t)
	writeTestSource(t, ix, testSource{source: "p1", session: "p1", calls: []UsageCallRecord{usageCall("", "", "x1", 1, 1)}})
	writeTestSource(t, ix, testSource{source: "p2", session: "p2", calls: []UsageCallRecord{usageCall("", "", "x2", 2, 1)}})
	writeTestSource(t, ix, testSource{source: "kid-a", session: "kid", parent: "p2",
		calls: []UsageCallRecord{usageCall("", "", "x3", 3, 1)}})
	writeTestSource(t, ix, testSource{source: "kid-b", session: "kid", parent: "p1",
		calls: []UsageCallRecord{usageCall("", "", "x4", 4, 1)}})
	writeTestSource(t, ix, testSource{source: "loop-a", session: "loop-a", parent: "loop-b"})
	writeTestSource(t, ix, testSource{source: "loop-b", session: "loop-b", parent: "loop-a"})
	work := testWorkIndex(t, ix, nil)
	if got := work.Root(UsageSessionRef{"rt", "kid"}); got != (UsageSessionRef{"rt", "p1"}) {
		t.Fatalf("the smallest parent wins: %+v", got)
	}
	_ = work.Root(UsageSessionRef{"rt", "loop-a"}) // must return
	top, _, err := ix.UsageRootTotals(UsageQuery{}, work, 10)
	if err != nil {
		t.Fatal(err)
	}
	var calls int64
	for _, root := range top {
		calls += root.Main.Calls + root.Subagent.Calls + root.Agent.Calls
	}
	if calls != 4 {
		t.Fatalf("totals unchanged: %d calls across roots %+v", calls, top)
	}
}

// An empty role never overwrites a stored one, in either write, and the
// metadata write touches nothing else (S-1).
func TestUsageRoleIsNeverErasedAndItsWriteTouchesNothingElse(t *testing.T) {
	ix := openUsageStore(t)
	writeTestSource(t, ix, testSource{source: "kid", session: "kid", role: "guardian"})
	writeTestSource(t, ix, testSource{source: "kid", session: "kid"}) // a read that learned no role
	if n, err := ix.UpdateUsageSourceRoles("rt", map[string]string{"kid": ""}); err != nil || n != 0 {
		t.Fatalf("an empty metadata role is skipped: %d %v", n, err)
	}
	states, err := ix.UsageSourceStates("rt")
	if err != nil || states["kid"].Role != "guardian" {
		t.Fatalf("role kept: %+v err=%v", states["kid"], err)
	}
	before := states["kid"]
	if n, err := ix.UpdateUsageSourceRoles("rt", map[string]string{"kid": "Explore"}); err != nil || n != 1 {
		t.Fatalf("update: %d %v", n, err)
	}
	after, _ := ix.UsageSourceStates("rt")
	changed := after["kid"]
	if changed.Role != "Explore" || changed.Epoch != before.Epoch || changed.UpdatedAtMS != before.UpdatedAtMS ||
		changed.Marker != before.Marker || changed.Complete != before.Complete || string(changed.Cursor) != string(before.Cursor) {
		t.Fatalf("metadata write touched more than the role: %+v -> %+v", before, changed)
	}
}

func TestUsageMainSeriesDownsamplesToEachBucketsLastCall(t *testing.T) {
	ix := openUsageStore(t)
	var calls []UsageCallRecord
	for index := int64(0); index < 10; index++ {
		call := usageCall("", "", "s"+string(rune('a'+index)), 1000+index*100, 1)
		call.Input = count(index)
		calls = append(calls, call)
	}
	writeTestSource(t, ix, testSource{source: "src", session: "s", calls: calls})
	all, err := ix.UsageMainSeries(UsageSessionRef{"rt", "s"}, 100)
	if err != nil || len(all) != 10 {
		t.Fatalf("all points: %+v err=%v", all, err)
	}
	few, err := ix.UsageMainSeries(UsageSessionRef{"rt", "s"}, 4)
	if err != nil || len(few) > 4 || few[len(few)-1].AtMS != 1900 {
		t.Fatalf("downsampled: %+v err=%v", few, err)
	}
}

// An agent whose served session has no recorded source stays agent work under
// the session it names, never main (code red-team C-3).
func TestUsageAgentServingAnUnrecordedSessionStaysAgentWork(t *testing.T) {
	ix := openUsageStore(t)
	writeTestSource(t, ix, testSource{runtime: "other", source: "helper", session: "helper-stem", alias: "helper-native",
		calls: []UsageCallRecord{usageCall("", "", "h1", 1, 5)}})
	work := testWorkIndex(t, ix, map[string]AgentSessionParent{"helper-native": {Runtime: "other", RootRuntime: "rt",
		RootNativeSessionID: "not-recorded-yet"}})
	if work.Notes.UnrecordedServed != 1 || len(work.Keys) != 1 || work.Keys[0].Root != (UsageSessionRef{"rt", "not-recorded-yet"}) {
		t.Fatalf("notes %+v keys %+v", work.Notes, work.Keys)
	}
	groups, err := ix.UsageGroups(UsageQuery{GroupBy: []string{UsageDimDelegation}, Agents: work.Keys})
	if err != nil || len(groups) != 1 || groups[0].Key.Delegation != UsageDelegationAgent {
		t.Fatalf("groups: %+v err=%v", groups, err)
	}
	top, _, err := ix.UsageRootTotals(UsageQuery{}, work, 5)
	if err != nil || len(top) != 1 || top[0].Root.SessionID != "not-recorded-yet" || top[0].Main.Calls != 0 || top[0].Agent.Calls != 1 {
		t.Fatalf("rows: %+v err=%v", top, err)
	}
}

// A nested agent's descendant takes its nearest agent ancestor, the same on
// every build; an agent serving an agent is in the served root's view too
// (C-5, C-6).
func TestUsageNestedAgentsAreDeterministicAndViewedToAFixpoint(t *testing.T) {
	ix := openUsageStore(t)
	writeTestSource(t, ix, testSource{source: "root", session: "root", calls: []UsageCallRecord{usageCall("", "", "r1", 1, 1)}})
	writeTestSource(t, ix, testSource{source: "a", session: "agent-a", calls: []UsageCallRecord{usageCall("", "", "a1", 2, 1)}})
	writeTestSource(t, ix, testSource{source: "b", session: "agent-b", parent: "agent-a", calls: []UsageCallRecord{usageCall("", "", "b1", 3, 1)}})
	writeTestSource(t, ix, testSource{source: "k", session: "kid", parent: "agent-b", calls: []UsageCallRecord{usageCall("", "", "k1", 4, 1)}})
	writeTestSource(t, ix, testSource{source: "c", session: "agent-c", calls: []UsageCallRecord{usageCall("", "", "c1", 5, 1)}})
	agents := map[string]AgentSessionParent{
		"agent-a": {Runtime: "rt", RootRuntime: "rt", RootNativeSessionID: "root"},
		"agent-b": {Runtime: "rt", RootRuntime: "rt", RootNativeSessionID: "other-root"},
		"agent-c": {Runtime: "rt", RootRuntime: "rt", RootNativeSessionID: "agent-a"},
	}
	for range 50 {
		work := testWorkIndex(t, ix, agents)
		if key, _ := work.Agent(UsageSessionRef{"rt", "kid"}); key.Agent != "agent-b" {
			t.Fatalf("kid takes its nearest agent: %+v", key)
		}
	}
	work := testWorkIndex(t, ix, agents)
	split := splitView(t, ix, work, UsageSessionRef{"rt", "root"})
	if !sameCalls(split["agent"], "a1@agent-a", "c1@agent-c") {
		t.Fatalf("an agent serving an agent is in the root's view: %+v", split)
	}
}

// A parent cycle is one root, the cycle's smallest session; an alias two
// sessions share resolves to the smaller; a view of an unrecorded session
// still finds children naming its id; an orphan is its own root (C-7, C-8,
// C-10, C-18).
func TestUsageCyclesAliasesUnrecordedParentsAndOrphans(t *testing.T) {
	ix := openUsageStore(t)
	writeTestSource(t, ix, testSource{source: "la", session: "loop-a", parent: "loop-b", calls: []UsageCallRecord{usageCall("", "", "l1", 1, 1)}})
	writeTestSource(t, ix, testSource{source: "lb", session: "loop-b", parent: "loop-a", calls: []UsageCallRecord{usageCall("", "", "l2", 2, 1)}})
	writeTestSource(t, ix, testSource{source: "p2", session: "p2", alias: "shared"})
	writeTestSource(t, ix, testSource{source: "p1", session: "p1", alias: "shared"})
	writeTestSource(t, ix, testSource{source: "sk", session: "shared-kid", parent: "shared"})
	writeTestSource(t, ix, testSource{source: "ok", session: "orphan-kid", parent: "never-recorded",
		calls: []UsageCallRecord{usageCall("", "", "o1", 3, 1)}})
	work := testWorkIndex(t, ix, nil)
	if work.Root(UsageSessionRef{"rt", "loop-a"}) != work.Root(UsageSessionRef{"rt", "loop-b"}) || work.Notes.Cycles != 1 {
		t.Fatalf("one root per cycle: %+v %+v notes=%+v", work.Root(UsageSessionRef{"rt", "loop-a"}),
			work.Root(UsageSessionRef{"rt", "loop-b"}), work.Notes)
	}
	if got := work.Root(UsageSessionRef{"rt", "shared-kid"}); got != (UsageSessionRef{"rt", "p1"}) || work.Notes.SharedAliases != 1 ||
		!work.Notes.Incomplete() {
		t.Fatalf("a shared alias resolves to the smaller session: %+v notes=%+v", got, work.Notes)
	}
	unrecorded := splitView(t, ix, work, UsageSessionRef{"rt", "never-recorded"})
	if !sameCalls(unrecorded["subagent"], "o1@orphan-kid") {
		t.Fatalf("a child naming an unrecorded session's id is in its view: %+v", unrecorded)
	}
	withCalls, _, withoutCalls, err := ix.UsageRootCounts(work)
	if err != nil || withCalls != 2 || withoutCalls != 2 {
		t.Fatalf("roots with calls %d (cycle and orphan), without %d (p1, p2's shared kid rolls into p1): err=%v",
			withCalls, withoutCalls, err)
	}
}
