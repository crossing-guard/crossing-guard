package daemon

// Child thread identity. A codex
// subagent rollout carries its parent's thread id; the runtime's MatchID says
// it names the parent. The fixtures are the measured example's real
// session_meta shapes (a parent, its thread_spawn child and its
// guardian sibling), written to a temp dir and summarized by the
// real adapter; the grandchild and the resumed segment follow the same shape.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

const (
	identityThread     = "abcdef00-aaaa-7bbb-8ccc-000000000006"
	identityChild      = "abcdef00-aaaa-7bbb-8ccc-000000000008"
	identitySibling    = "abcdef00-aaaa-7bbb-8ccc-000000000007"
	identityGrandchild = "01a045c9-1111-7222-8333-444455556666"
	identityResumed    = "01a05000-aaaa-7bbb-8ccc-ddddeeeeffff"
	identityCwd        = "/work/repo"
)

type identityFamily struct {
	parent, child, sibling, grandchild, resumed SessionSummary
	root                                        string // the codex sessions dir the rollouts live in
}

func identityParentMeta(thread string) string {
	return `{"timestamp":"2026-08-27T19:47:41.379Z","type":"session_meta","payload":{"session_id":"` + thread +
		`","id":"` + thread + `","timestamp":"2026-08-27T19:47:08.999Z","cwd":"` + identityCwd +
		`","originator":"codex_work_desktop","cli_version":"0.149.0-alpha.4.3","source":"vscode","thread_source":"user",` +
		`"model_provider":"openai","history_mode":"paginated"}}` + "\n"
}

func identitySpawnMeta(sessionID, parent, own string, depth int) string {
	return `{"timestamp":"2026-08-28T00:24:53.312Z","type":"session_meta","payload":{"session_id":"` + sessionID +
		`","id":"` + own + `","forked_from_id":"` + parent + `","parent_thread_id":"` + parent +
		`","timestamp":"2026-08-28T00:24:53.312Z","cwd":"` + identityCwd +
		`","originator":"codex_work_desktop","cli_version":"0.149.0-alpha.4.3",` +
		`"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `","depth":` + strconv.Itoa(depth) +
		`,"agent_path":"/root/cursor_js_final_review","agent_nickname":"Pauli","agent_role":"review-javascript"}}},` +
		`"thread_source":"subagent","agent_nickname":"Pauli","agent_role":"review-javascript",` +
		`"agent_path":"/root/cursor_js_final_review","model_provider":"openai","history_mode":"paginated","multi_agent_version":"v2"}}` + "\n"
}

func identityGuardianMeta(parent, own string) string {
	return `{"timestamp":"2026-08-27T20:14:15.000Z","type":"session_meta","payload":{"session_id":"` + parent +
		`","id":"` + own + `","parent_thread_id":"` + parent + `","cwd":"` + identityCwd +
		`","originator":"codex_work_desktop","cli_version":"0.149.0-alpha.4.3",` +
		`"source":{"subagent":{"other":"guardian"}},"thread_source":"subagent"}}` + "\n"
}

// newIdentityFamily writes the family under $HOME/.codex/sessions (HOME is a
// fresh temp dir) with modification times newest-first: resumed segment,
// parent, then the children, so a scan lists them in that order.
func newIdentityFamily(t *testing.T) identityFamily {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions", "2026", "08", "27")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	write := func(stem, body string, modified time.Time) SessionSummary {
		t.Helper()
		path := filepath.Join(root, stem+".jsonl")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
		summary, ok := harvest.SummarizeFile("codex", path)
		if !ok {
			t.Fatalf("summarize %s", stem)
		}
		return summary
	}
	family := identityFamily{root: root}
	family.resumed = write("rollout-2026-08-29T09-00-00-"+identityResumed, identityParentMeta(identityThread), base.Add(2*time.Hour))
	family.parent = write("rollout-2026-08-27T14-47-08-"+identityThread, identityParentMeta(identityThread), base.Add(time.Hour))
	family.child = write("rollout-2026-08-27T19-24-53-"+identityChild, identitySpawnMeta(identityThread, identityThread, identityChild, 1), base)
	family.sibling = write("rollout-2026-08-27T15-14-15-"+identitySibling, identityGuardianMeta(identityThread, identitySibling), base.Add(-time.Hour))
	family.grandchild = write("rollout-2026-08-27T19-30-00-"+identityGrandchild,
		identitySpawnMeta(identityThread, identityChild, identityGrandchild, 2), base.Add(-2*time.Hour))
	if family.child.ThreadID != identityThread || family.child.MetaID != identityChild || family.child.LineageKind == "" {
		t.Fatalf("fixture child = %+v, want a subagent carrying its parent's thread", family.child)
	}
	return family
}

func TestSessionIdentitiesLeaveOutAChildsParentThread(t *testing.T) {
	family := newIdentityFamily(t)
	cases := []struct {
		name    string
		row     SessionSummary
		want    []string
		lookups []string
	}{
		{"child", family.child, []string{family.child.ID, identityChild}, []string{family.child.ID, identityChild, identityThread}},
		{"guardian sibling", family.sibling, []string{family.sibling.ID, identitySibling}, []string{family.sibling.ID, identitySibling, identityThread}},
		{"primary", family.parent, []string{family.parent.ID, identityThread}, []string{family.parent.ID, identityThread}},
		{"resumed segment", family.resumed, []string{family.resumed.ID, identityThread}, []string{family.resumed.ID, identityThread}},
		{"runtime-less row", SessionSummary{ID: "row-1", ThreadID: "thread-1"}, []string{"row-1", "thread-1"}, []string{"row-1", "thread-1"}},
		{"opencode row", SessionSummary{Runtime: "opencode", ID: "ses_1"}, []string{"ses_1"}, []string{"ses_1"}},
	}
	for _, c := range cases {
		if got := sessionIdentities(c.row); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: sessionIdentities = %v, want %v", c.name, got, c.want)
		}
		// The lookup keys are unchanged: watching, candidate lookup and
		// agent-session classification still see the thread (plan D-1).
		if got := sessionIdentityAlternates(c.row); !reflect.DeepEqual(got, c.lookups) {
			t.Errorf("%s: sessionIdentityAlternates = %v, want %v", c.name, got, c.lookups)
		}
	}
}

func identityStore(t *testing.T) *store.Index {
	t.Helper()
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	prior := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() {
		governor = prior
		_ = ix.Close()
	})
	return ix
}

func identityBinding(t *testing.T, ix *store.Index, id, scope string) {
	t.Helper()
	if _, err := ix.PutManagedBinding(store.ManagedBinding{BindingID: id, State: "enabled", Role: "helper",
		ScopeRuntime: "codex", ScopeSession: scope, ProjectRoot: identityCwd, ProfileID: "profile-" + id,
		ProfileSourceDigest: "sha256-v1:s", ProfileBundleDigest: "sha256-v1:b", Runtime: "managed-fixture",
		Authority: []string{}, AllowedProfiles: []store.ManagedProfileRef{}}, store.ManagedBindingAbsentToken(id), 1); err != nil {
		t.Fatal(err)
	}
}

func watcherIDs(watches []agentWatch) []string {
	ids := []string{}
	for _, watch := range watches {
		ids = append(ids, watch.BindingID)
	}
	return ids
}

// The detail publishes the ids that name the session, and watching keeps the
// thread: a subagent's activity is recorded under its parent's thread, so a
// binding scoped to that thread fires on it (plan D-1).
func TestSessionDetailPublishesIdentitiesAndKeepsThreadWatchers(t *testing.T) {
	family := newIdentityFamily(t)
	ix := identityStore(t)
	identityBinding(t, ix, "agent-thread", identityThread)
	identityBinding(t, ix, "agent-parent-stem", family.parent.ID)

	child := decorateSessionAgents(&SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: family.child}})
	if want := []string{family.child.ID, identityChild}; !reflect.DeepEqual(child.Identities, want) {
		t.Fatalf("child identities = %v, want %v", child.Identities, want)
	}
	if got := watcherIDs(child.WatchedBy); !reflect.DeepEqual(got, []string{"agent-thread"}) {
		t.Fatalf("child watched_by = %v, want the thread's binding only (D-1)", got)
	}
	parent := decorateSessionAgents(&SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: family.parent}})
	if want := []string{family.parent.ID, identityThread}; !reflect.DeepEqual(parent.Identities, want) {
		t.Fatalf("parent identities = %v, want %v", parent.Identities, want)
	}
	if got := watcherIDs(parent.WatchedBy); !reflect.DeepEqual(got, []string{"agent-parent-stem", "agent-thread"}) {
		t.Fatalf("parent watched_by = %v", got)
	}
	body, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"identities":["`+family.child.ID+`","`+identityChild+`"]`) ||
		!strings.Contains(string(body), `"thread_id":"`+identityThread+`"`) {
		t.Fatalf("payload must carry identities and keep thread_id: %s", body)
	}

	governor = nil
	closed := decorateSessionAgents(&SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: family.parent}})
	if want := []string{family.parent.ID, identityThread}; !reflect.DeepEqual(closed.Identities, want) {
		t.Fatalf("with no store the identities are still published: %v", closed.Identities)
	}
}

// A child's delegated usage is its own descendants' calls, never its
// siblings' (measured: the example child showed both siblings' 286,584
// input tokens as delegated). The parent still sums every descendant.
func TestRecordedSessionUsageLeavesOutSiblings(t *testing.T) {
	family := newIdentityFamily(t)
	ix := identityStore(t)
	day := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC).UnixMilli()
	write := func(row SessionSummary, alias, parent string, output int64) {
		t.Helper()
		call := store.UsageCallRecord{Runtime: "codex", Source: row.Path, SessionID: row.ID, Reader: "codex/usage-1",
			CallID: "call-" + alias, FirstAtMS: day, AtMS: day, Model: "m",
			Input: usageCount(1), CacheRead: usageCount(0), CacheWrite: usageCount(0), Output: usageCount(output)}
		if _, err := ix.WriteUsageSource(store.UsageSourceWrite{Calls: []store.UsageCallRecord{call},
			State: store.UsageSourceState{Runtime: "codex", Source: row.Path, SessionID: row.ID, SessionAlias: alias,
				ParentSession: parent, Marker: "m", Reader: "codex/usage-1", Complete: true, UpdatedAtMS: day}}); err != nil {
			t.Fatal(err)
		}
	}
	write(family.parent, identityThread, "", 1)
	write(family.child, identityChild, identityThread, 10)
	write(family.sibling, identitySibling, identityThread, 100)
	write(family.grandchild, identityGrandchild, identityChild, 1000)

	child, ok := recordedSessionUsage(family.child)
	if !ok || child.OutputTokens != 10 || child.Delegated == nil || child.Delegated.OutputTokens != 1000 {
		t.Fatalf("child usage = %+v delegated = %+v, want its own 10 and its grandchild's 1000", child, child.Delegated)
	}
	sibling, ok := recordedSessionUsage(family.sibling)
	if !ok || sibling.OutputTokens != 100 || (sibling.Delegated != nil && sibling.Delegated.OutputTokens != 0) {
		t.Fatalf("sibling usage = %+v delegated = %+v, want no delegated calls", sibling, sibling.Delegated)
	}
	parent, ok := recordedSessionUsage(family.parent)
	if !ok || parent.Delegated == nil || parent.Delegated.OutputTokens != 1110 {
		t.Fatalf("parent delegated = %+v, want every descendant (1110)", parent.Delegated)
	}
}

// An envelope keyed by the thread id flags the thread's own rows, never a
// subagent that carries the id (measured: 4 of 89 children held the flag and
// none of their primaries did).
func TestChangeEvidenceFlagsTheThreadNotItsSubagents(t *testing.T) {
	family := newIdentityFamily(t)
	t.Setenv("OPENCODE_DATA_HOME", t.TempDir()) // the scan reads every runtime; keep it on the fixture
	previousIndexPath := resolvedIndexPath
	t.Cleanup(func() { resolvedIndexPath = previousIndexPath })
	path := filepath.Join(t.TempDir(), "index.sqlite")
	setIndexPath(filepath.Dir(path))
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	record := &store.ChangeRecord{SessionID: identityThread, RepositoryID: "local-sha256-v1:r", CheckoutID: "checkout-sha256-v1:c",
		RepositoryIdentityKind: "local-sha256", CheckoutRoot: t.TempDir(), SessionRuntimeClaim: "codex", Kind: "declaration",
		EvidenceClass: "claimed", SourceKind: "file", SourceRef: "plan.md", SourceDisplay: "plan.md", SourceDigest: "sha256-v1:x",
		RecordedAt: 10, IntentLabel: "claimed intent", Items: []store.ChangeItem{{Path: "planned.go"}}}
	if err := ix.AppendChange(record); err != nil {
		t.Fatal(err)
	}
	scan := func() (map[string]bool, int) {
		flagged, rollouts := map[string]bool{}, 0
		rows, _ := scanSessionsUncoalesced()
		for _, row := range rows {
			flagged[row.ID] = row.HasChangeEvidence
			if row.Runtime == "codex" && row.Path != "" {
				rollouts++
			}
		}
		return flagged, rollouts
	}
	flagged, rollouts := scan()
	if rollouts != 5 {
		t.Fatalf("scan listed %d codex rollouts, want the 5 fixture rollouts", rollouts)
	}
	if _, synthetic := flagged[identityThread]; synthetic {
		t.Fatal("the thread's envelope became an envelope-only row although its primary is listed")
	}
	for _, sub := range []SessionSummary{family.child, family.sibling, family.grandchild} {
		if flagged[sub.ID] {
			t.Errorf("subagent %s holds the thread's change evidence", sub.ID)
		}
	}
	if !flagged[family.parent.ID] && !flagged[family.resumed.ID] {
		t.Fatalf("no row of the thread holds its change evidence: %v", flagged)
	}

	// With no primary row left, the thread's envelope is an unmatched id: it
	// becomes an envelope-only row, as any unmatched id does, and still never
	// lands on a subagent (measured: 0 such threads on 2026-09-26).
	for _, primary := range []SessionSummary{family.parent, family.resumed} {
		if err := os.Remove(primary.Path); err != nil {
			t.Fatal(err)
		}
	}
	flagged, _ = scan()
	if !flagged[identityThread] {
		t.Fatalf("an envelope for a thread with no primary row must stay visible as its own row: %v", flagged)
	}
	for _, sub := range []SessionSummary{family.child, family.sibling, family.grandchild} {
		if flagged[sub.ID] {
			t.Errorf("subagent %s took the change evidence of a thread with no primary row", sub.ID)
		}
	}
}

// Governance rows keyed by the thread id take a primary's summary; newest
// first, the old last-write-wins map handed them to the oldest subagent.
func TestSessionTitlesMapAThreadToItsPrimary(t *testing.T) {
	family := newIdentityFamily(t)
	rows := []SessionSummary{family.parent, family.child, family.sibling}
	prior := sessionScanCoalescer
	sessionScanCoalescer = &scanCoalescer{result: rows, done: time.Now().Add(time.Hour)}
	t.Cleanup(func() { sessionScanCoalescer = prior })
	titles := sessionTitles()
	if got := titles[identityThread].ID; got != family.parent.ID {
		t.Fatalf("thread titled by %q, want the primary %q", got, family.parent.ID)
	}
	if got := titles[identityChild].ID; got != family.child.ID {
		t.Fatalf("the child's own id titled by %q", got)
	}
}

// An agent session rooted at the thread folds under the primary even when a
// subagent is the thread's newest row; a native child of an agent session is
// still that agent's (its activity is recorded under the agent's thread).
func TestAgentFoldTargetsThePrimaryAndKeepsAgentSubagents(t *testing.T) {
	family := newIdentityFamily(t)
	newest := family.child
	newest.Modified = family.parent.Modified.Add(time.Hour)
	agent := sessionFixture(identityCwd, "claude", "agent-session", family.parent.Modified.Add(-3*time.Hour))
	parents := map[string]store.AgentSessionParent{
		"agent-session": {Role: "helper", RootRuntime: "codex", RootNativeSessionID: identityThread},
	}
	main, children := partitionAgentSessionsUsing([]SessionSummary{newest, family.parent, agent}, parents)
	if len(children[family.parent.ID]) != 1 || len(children[newest.ID]) != 0 {
		t.Fatalf("agent fold = %v, want it under the primary %s", children, family.parent.ID)
	}
	if len(main) != 2 {
		t.Fatalf("main rows = %d, want the primary and its subagent", len(main))
	}

	// The same family where the parent thread IS an agent's session.
	agentThread := map[string]store.AgentSessionParent{
		identityThread: {Role: "helper", RootRuntime: "claude", RootCatalogSessionID: "root-session"},
	}
	root := sessionFixture(identityCwd, "claude", "root-session", family.parent.Modified.Add(-4*time.Hour))
	main, children = partitionAgentSessionsUsing([]SessionSummary{family.parent, family.child, root}, agentThread)
	if len(main) != 1 || main[0].ID != root.ID || len(children[root.ID]) != 2 {
		t.Fatalf("main = %v children = %v, want the agent thread and its subagent folded under the root", main, children)
	}
}
