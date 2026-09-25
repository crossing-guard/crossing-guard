package harvest

// Session-graph capability tests. Fixtures are synthetic (t.TempDir), never
// the live corpus, and assertions are relational — counts measured against
// what the same test wrote, never hardcoded corpus constants.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	fixtureParentThread = "aaaaaaaa-1111-1111-1111-111111111111"
	fixtureSpawnChild   = "bbbbbbbb-2222-2222-2222-222222222222"
	fixtureGuardChild   = "cccccccc-3333-3333-3333-333333333333"
)

func codexParentMetaLine(thread string) string {
	return `{"timestamp":"2026-08-29T10:00:00Z","type":"session_meta","payload":{"session_id":"` + thread +
		`","id":"` + thread + `","cwd":"/work/repo"}}` + "\n"
}

func codexThreadSpawnMetaLine(parent, child string) string {
	return `{"timestamp":"2026-08-29T10:01:00Z","type":"session_meta","payload":{"session_id":"` + parent +
		`","id":"` + child + `","parent_thread_id":"` + parent + `","cwd":"/work/repo",` +
		`"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent +
		`","depth":1,"agent_path":"/root/reviewer","agent_nickname":"Pauli","agent_role":"review-javascript"}}}}}` + "\n"
}

func codexGuardianMetaLine(parent, child string) string {
	return `{"timestamp":"2026-08-29T10:02:00Z","type":"session_meta","payload":{"session_id":"` + parent +
		`","id":"` + child + `","parent_thread_id":"` + parent + `","cwd":"/work/repo",` +
		`"source":{"subagent":{"other":"guardian"}}}}` + "\n"
}

func summarizeCodexFixture(t *testing.T, name, body string) SessionSummary {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, _, _, ok := codexRuntime{}.Summarize(fileJob{runtime: "codex", path: path})
	if !ok {
		t.Fatalf("Summarize(%s) returned ok=false", name)
	}
	return sum
}

// TestCodexChildCanonicalIDSplit pins the identity split: a rollout whose
// session_meta.source declares a subagent — thread_spawn AND guardian — keys
// by its OWN session_meta.id, while resume stays the parent thread. Every
// assertion is relational against the ids the fixture itself declares.
func TestCodexChildCanonicalIDSplit(t *testing.T) {
	parent := summarizeCodexFixture(t, "rollout-2026-08-29T10-00-00-"+fixtureParentThread,
		codexParentMetaLine(fixtureParentThread))
	spawn := summarizeCodexFixture(t, "rollout-2026-08-29T10-01-00-"+fixtureSpawnChild,
		codexThreadSpawnMetaLine(fixtureParentThread, fixtureSpawnChild))
	guardian := summarizeCodexFixture(t, "rollout-2026-08-29T10-02-00-"+fixtureGuardChild,
		codexGuardianMetaLine(fixtureParentThread, fixtureGuardChild))

	if got := CanonicalID(parent); got != fixtureParentThread {
		t.Fatalf("parent canonical = %q, want its thread id", got)
	}
	for name, child := range map[string]SessionSummary{"thread_spawn": spawn, "guardian": guardian} {
		got := CanonicalID(child)
		if got == CanonicalID(parent) {
			t.Fatalf("%s child folded onto the parent's canonical id %q", name, got)
		}
		if got != child.MetaID {
			t.Fatalf("%s child canonical = %q, want own session_meta.id %q", name, got, child.MetaID)
		}
		decorateSummary(&child)
		if child.ResumeID != fixtureParentThread {
			t.Fatalf("%s child ResumeID = %q, want parent thread (children are not independently resumable)", name, child.ResumeID)
		}
	}
	decorateSummary(&parent)
	if parent.ResumeID != CanonicalID(parent) {
		t.Fatalf("parent ResumeID = %q diverged from canonical %q", parent.ResumeID, CanonicalID(parent))
	}
	if spawn.LineageKind != "native-thread-spawn" || spawn.LineageRole != "review-javascript" ||
		spawn.LineageNickname != "Pauli" || spawn.LineageDepth != 1 || spawn.ParentID != fixtureParentThread {
		t.Fatalf("thread_spawn lineage fields = %+v", spawn)
	}
	if guardian.LineageKind != "native-subagent" || guardian.LineageRole != "guardian" ||
		guardian.ParentID != fixtureParentThread {
		t.Fatalf("guardian lineage fields = %+v", guardian)
	}
	// The SessionLineage capability reports the same facts, vendor-neutrally.
	facts, ok := Lineage(spawn)
	if !ok || facts.Parent.ID != fixtureParentThread || facts.Kind != "native-thread-spawn" ||
		facts.Provenance != EdgeProvenanceObserved || facts.Role != "review-javascript" {
		t.Fatalf("Lineage(spawn) = %+v ok=%v", facts, ok)
	}
	if _, ok := Lineage(parent); ok {
		t.Fatal("parent reported lineage it does not have")
	}
}

// TestCodexReplayedParentHeaderDoesNotRefoldChild: a thread_spawn child
// replays the parent's transcript INCLUDING the parent's own session_meta
// line. The first session_meta is the rollout's own header; a later replayed
// one must not overwrite the child's identity (measured on the real corpus,
// 2026-08-29).
func TestCodexReplayedParentHeaderDoesNotRefoldChild(t *testing.T) {
	child := summarizeCodexFixture(t, "rollout-2026-08-29T10-01-00-"+fixtureSpawnChild,
		codexThreadSpawnMetaLine(fixtureParentThread, fixtureSpawnChild)+
			codexParentMetaLine(fixtureParentThread)) // replayed parent header
	if got := CanonicalID(child); got != fixtureSpawnChild {
		t.Fatalf("replayed parent header re-folded the child: canonical = %q, want %q", got, fixtureSpawnChild)
	}
	if child.LineageKind != "native-thread-spawn" || child.MetaID != fixtureSpawnChild {
		t.Fatalf("child header lost to replay: %+v", child)
	}
}

// TestCodexMatchIDExactVsSuffixAndChildExclusion pins the MatchID contract:
// exact ids first, thread-id queries never match a child (FindAll sorts
// newest-first and a guardian child is often the newest file — matching it
// would hand the child back as the thread's primary), and suffix matching
// requires a full trailing uuid.
func TestCodexMatchIDExactVsSuffixAndChildExclusion(t *testing.T) {
	parent := summarizeCodexFixture(t, "rollout-2026-08-29T10-00-00-"+fixtureParentThread,
		codexParentMetaLine(fixtureParentThread))
	child := summarizeCodexFixture(t, "rollout-2026-08-29T10-02-00-"+fixtureGuardChild,
		codexGuardianMetaLine(fixtureParentThread, fixtureGuardChild))

	if !MatchID(parent, fixtureParentThread) {
		t.Fatal("parent did not match its own thread id")
	}
	if MatchID(child, fixtureParentThread) {
		t.Fatal("thread-id query matched a subagent child — a child must never resolve as the thread's primary")
	}
	if !MatchID(child, fixtureGuardChild) {
		t.Fatal("child did not match its own session_meta.id")
	}
	if !MatchID(child, child.ID) {
		t.Fatal("child did not match its own filename stem")
	}
	if MatchID(parent, fixtureGuardChild) {
		t.Fatal("parent matched a child id")
	}
	if MatchID(parent, fixtureParentThread[24:]) {
		t.Fatal("a short suffix fragment matched — suffix matching must require a full uuid")
	}
}

// TestCodexFindNewestWinsExcludesChildren proves the harvest-side FindAll
// contract end-to-end: with a child rollout NEWER than the parent, a
// thread-id query still resolves the parent segment as primary.
func TestCodexFindNewestWinsExcludesChildren(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "29")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_750_000_000, 0)
	write := func(name, body string, mod time.Time) string {
		path := filepath.Join(dir, name+".jsonl")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("rollout-2026-08-29T10-00-00-"+fixtureParentThread, codexParentMetaLine(fixtureParentThread), base)
	write("rollout-2026-08-29T10-02-00-"+fixtureGuardChild,
		codexGuardianMetaLine(fixtureParentThread, fixtureGuardChild), base.Add(time.Hour))

	matches := FindAll("codex", fixtureParentThread)
	if len(matches) != 1 {
		t.Fatalf("FindAll(thread) = %d matches, want only the parent segment: %+v", len(matches), matches)
	}
	if matches[0].LineageKind != "" || CanonicalID(matches[0]) != fixtureParentThread {
		t.Fatalf("thread query resolved a child as primary: %+v", matches[0])
	}
	if child, ok := Find("codex", fixtureGuardChild); !ok || CanonicalID(child) != fixtureGuardChild {
		t.Fatalf("child not findable by its own id: ok=%v %+v", ok, child)
	}
}

// codexEdgesFixture builds one parent rollout whose real edge records are
// surrounded by the measured false-positive shapes: the item-type words as
// free text inside a response_item envelope and inside CommandExecution
// output. Decoding by substring counted 17 false against 12 real in one real
// file; decoding must be structural (event_msg → item_completed → item.type).
func codexEdgesFixture(parent, child string) string {
	return codexParentMetaLine(parent) +
		// false positive: protocol envelope carrying the words as text
		`{"timestamp":"2026-08-29T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"grep","arguments":"{\"pattern\":\"SubAgentActivity|CollabAgentToolCall\"}"}}` + "\n" +
		// false positive: command output quoting the words
		`{"timestamp":"2026-08-29T10:00:02Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-cmd","item":{"type":"CommandExecution","command":["grep","SubAgentActivity"],"stdout":"CollabAgentToolCall SubAgentActivity DynamicToolCall","status":"completed"}}}` + "\n" +
		// real: one child, two lifecycle sightings — ONE edge, two observations
		`{"timestamp":"2026-08-29T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-1","item":{"type":"SubAgentActivity","kind":"started","agent_thread_id":"` + child + `","agent_path":"/root/reviewer"}}}` + "\n" +
		`{"timestamp":"2026-08-29T10:00:04Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-2","item":{"type":"SubAgentActivity","kind":"interacted","agent_thread_id":"` + child + `","agent_path":"/root/reviewer"}}}` + "\n" +
		// real: two waits, receivers ALWAYS empty in the corpus — empty target
		`{"timestamp":"2026-08-29T10:00:05Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-2","item":{"type":"CollabAgentToolCall","tool":"wait","status":"completed","sender_thread_id":"` + parent + `","receiver_thread_ids":[],"receiver_agents":[],"agents_states":{}}}}` + "\n" +
		`{"timestamp":"2026-08-29T10:00:06Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-3","item":{"type":"CollabAgentToolCall","tool":"wait","status":"completed","sender_thread_id":"` + parent + `","receiver_thread_ids":[],"agents_states":{"` + child + `":"running"}}}}` + "\n" +
		// real: reading other threads' context
		`{"timestamp":"2026-08-29T10:00:07Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-3","item":{"type":"DynamicToolCall","namespace":"codex_app","tool":"list_threads","status":"completed"}}}` + "\n"
}

func codexEdgesForFixture(t *testing.T, body string) []Edge {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-2026-08-29T10-00-00-"+fixtureParentThread+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, _, _, ok := codexRuntime{}.Summarize(fileJob{runtime: "codex", path: path})
	if !ok {
		t.Fatal("Summarize failed")
	}
	edges, supported, err := Edges(sum)
	if err != nil || !supported {
		t.Fatalf("Edges: supported=%v err=%v", supported, err)
	}
	return edges
}

// TestCodexEdgesIgnoreEnvelopeFalsePositives is the decode-hazard regression:
// the fixture carries the item-type words as free text in two shapes that a
// substring decoder would count; the structural decoder must not.
func TestCodexEdgesIgnoreEnvelopeFalsePositives(t *testing.T) {
	edges := codexEdgesForFixture(t, codexEdgesFixture(fixtureParentThread, fixtureSpawnChild))
	byKind := map[string]int{}
	for _, edge := range edges {
		byKind[edge.Kind]++
		if edge.Provenance != EdgeProvenanceObserved {
			t.Fatalf("edge provenance = %q, want observed", edge.Provenance)
		}
	}
	// Exactly the three relationships the fixture states — the false-positive
	// lines contribute nothing.
	if len(edges) != 3 || byKind[EdgeKindSpawned] != 1 || byKind[EdgeKindWaitedOn] != 1 ||
		byKind[EdgeKindReadContextOf] != 1 {
		t.Fatalf("edges by kind = %v (total %d), want one spawned, one waited_on, one read_context_of", byKind, len(edges))
	}
	if byKind[EdgeKindMessaged] != 0 {
		t.Fatal("codex emitted a messaged edge — no corpus evidence exists for that mapping")
	}
}

// TestCodexSubAgentActivityLifecycleIsOneEdgeManyObservations pins the
// lifecycle rule: N sightings of one child are ONE spawned edge whose
// observations carry the vendor's lifecycle words and per-turn anchors.
func TestCodexSubAgentActivityLifecycleIsOneEdgeManyObservations(t *testing.T) {
	edges := codexEdgesForFixture(t, codexEdgesFixture(fixtureParentThread, fixtureSpawnChild))
	var spawned *Edge
	for i := range edges {
		if edges[i].Kind == EdgeKindSpawned {
			if spawned != nil {
				t.Fatalf("duplicate spawned edges: %+v", edges)
			}
			spawned = &edges[i]
		}
	}
	if spawned == nil {
		t.Fatal("no spawned edge")
	}
	if spawned.To.ID != fixtureSpawnChild || spawned.From.ID != fixtureParentThread {
		t.Fatalf("spawned endpoints = %+v", spawned)
	}
	if len(spawned.Observations) != 2 {
		t.Fatalf("spawned observations = %d, want one per sighting (2)", len(spawned.Observations))
	}
	if spawned.Observations[0].Note != "started" || spawned.Observations[1].Note != "interacted" {
		t.Fatalf("lifecycle notes = %+v", spawned.Observations)
	}
	if spawned.Anchor != spawned.Observations[0].Anchor || spawned.Anchor == "" {
		t.Fatalf("edge anchor %q != first observation anchor %q", spawned.Anchor, spawned.Observations[0].Anchor)
	}
	if spawned.Observations[0].Anchor == spawned.Observations[1].Anchor {
		t.Fatal("distinct turns produced equal anchors")
	}
}

// TestCodexWaitedOnHasEmptyTarget pins the honest-target rule: the corpus
// never populates receiver_thread_ids, so the edge says "waited", not "waited
// for X" — and agents_states rides along when present.
func TestCodexWaitedOnHasEmptyTarget(t *testing.T) {
	edges := codexEdgesForFixture(t, codexEdgesFixture(fixtureParentThread, fixtureSpawnChild))
	for _, edge := range edges {
		if edge.Kind != EdgeKindWaitedOn {
			continue
		}
		if edge.To.ID != "" || edge.To.Unresolved {
			t.Fatalf("waited_on target = %+v, want an honestly empty target", edge.To)
		}
		if len(edge.Observations) != 2 {
			t.Fatalf("waited_on observations = %d, want one per wait (2)", len(edge.Observations))
		}
		if edge.Observations[0].Note != "" {
			t.Fatalf("empty agents_states produced note %q", edge.Observations[0].Note)
		}
		if !strings.Contains(edge.Observations[1].Note, "running") {
			t.Fatalf("populated agents_states not recorded: %+v", edge.Observations[1])
		}
		return
	}
	t.Fatal("no waited_on edge")
}

// TestCodexTurnAnchorPopulatedAndDispatched: the normalizer stamps turn_id as
// the opaque anchor and the TurnAnchor dispatcher resolves it capability-first.
func TestCodexTurnAnchorPopulatedAndDispatched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "29")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := codexParentMetaLine(fixtureParentThread) +
		`{"timestamp":"2026-08-29T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-9","item":{"type":"UserMessage","content":[{"type":"text","text":"anchored prompt"}]}}}` + "\n"
	path := filepath.Join(dir, "rollout-2026-08-29T10-00-00-"+fixtureParentThread+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	events, _, _, err := normalizeCodex(path, transcriptCaps)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if events[0].TurnAnchor != "turn-9" {
		t.Fatalf("TurnAnchor = %q, want the envelope turn_id", events[0].TurnAnchor)
	}
	anchor, found, err := TurnAnchor("codex", fixtureParentThread, events[0].Seq)
	if err != nil || !found || anchor != events[0].TurnAnchor {
		t.Fatalf("TurnAnchor dispatch = (%q,%v,%v)", anchor, found, err)
	}
}

// TestClaudeTurnAnchorIsRecordUUID: claude events anchor to the record uuid.
func TestClaudeTurnAnchorIsRecordUUID(t *testing.T) {
	sid := "dddddddd-4444-4444-4444-444444444444"
	path := filepath.Join(t.TempDir(), sid+".jsonl")
	body := `{"type":"user","uuid":"rec-1","sessionId":"` + sid + `","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	events, _, _, err := normalizeClaude(path, transcriptCaps)
	if err != nil || len(events) != 1 || events[0].TurnAnchor != "rec-1" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if anchor, ok := (claudeRuntime{}).TurnAnchor(SessionSummary{}, events[0]); !ok || anchor != "rec-1" {
		t.Fatalf("capability anchor = (%q,%v)", anchor, ok)
	}
}

// writeClaudeParentWithSubagents builds <root>/<slug>/<parent>.jsonl plus a
// subagents directory with the given child ids, returning the parent summary.
func writeClaudeParentWithSubagents(t *testing.T, root, slug, parentID, body string, childIDs []string) SessionSummary {
	t.Helper()
	dir := filepath.Join(root, slug)
	subDir := filepath.Join(dir, parentID, "subagents")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, parentID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, childID := range childIDs {
		if err := os.WriteFile(filepath.Join(subDir, "agent-"+childID+".jsonl"),
			[]byte(`{"type":"user","isSidechain":true,"sessionId":"`+parentID+`","agentId":"`+childID+`","message":{"role":"user","content":"child work"}}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sum, _, _, ok := claudeRuntime{}.Summarize(fileJob{runtime: "claude", path: path})
	if !ok {
		t.Fatal("Summarize failed")
	}
	return sum
}

// TestClaudeLineageEnumeratesSubagentsDirWithoutCataloguing: the capability
// enumerates children from the directory layout while the main collect scan
// keeps them out of the top-level inventory (semantics unchanged this pass).
func TestClaudeLineageEnumeratesSubagentsDirWithoutCataloguing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude", "projects")
	parentID := "eeeeeeee-5555-5555-5555-555555555555"
	childIDs := []string{"a1111111111111111", "a2222222222222222"}
	body := `{"type":"user","uuid":"rec-1","sessionId":"` + parentID + `","cwd":"/work/repo","message":{"role":"user","content":"parent prompt"}}` + "\n"
	sum := writeClaudeParentWithSubagents(t, root, "-work-repo", parentID, body, childIDs)
	// vendor-published labels ride the sidecar
	metaPath := filepath.Join(root, "-work-repo", parentID, "subagents", "agent-"+childIDs[0]+".meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"agentType":"Explore","description":"x","spawnDepth":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	facts, ok := Lineage(sum)
	if !ok || len(facts.Children) != len(childIDs) {
		t.Fatalf("Lineage children = %+v ok=%v, want %d enumerated", facts.Children, ok, len(childIDs))
	}
	byID := map[string]LineageChild{}
	for _, child := range facts.Children {
		byID[child.ID] = child
		if child.Kind != "native-subagent" {
			t.Fatalf("child kind = %q", child.Kind)
		}
	}
	if byID[childIDs[0]].Role != "Explore" || byID[childIDs[0]].Depth != 1 {
		t.Fatalf("sidecar labels not carried: %+v", byID[childIDs[0]])
	}
	if byID[childIDs[1]].Role != "" {
		t.Fatalf("child without sidecar gained a synthesized role: %+v", byID[childIDs[1]])
	}
	// collect-scan semantics unchanged: only the parent is inventoried
	jobs, err := collectClaudeJobs(true)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("collect saw %d jobs (err=%v), want the parent only — child transcripts stay out of the catalog", len(jobs), err)
	}
}

// TestClaudeEdgesSpawnedAndMessaged: spawned from async_launched records,
// messaged from SendMessage — resolved against enumerated children, and kept
// with an unresolved marked target when `to` names no known session.
func TestClaudeEdgesSpawnedAndMessaged(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude", "projects")
	parentID := "ffffffff-6666-6666-6666-666666666666"
	childID := "a3333333333333333"
	body := `{"type":"user","uuid":"rec-1","sessionId":"` + parentID + `","message":{"role":"user","content":"go"}}` + "\n" +
		`{"type":"user","uuid":"rec-2","timestamp":"2026-08-29T10:00:01Z","sessionId":"` + parentID + `","toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"` + childID + `","description":"map flow"},"message":{"role":"user","content":[{"type":"tool_result","content":"launched"}]}}` + "\n" +
		`{"type":"assistant","uuid":"rec-3","sessionId":"` + parentID + `","message":{"role":"assistant","content":[{"type":"tool_use","name":"SendMessage","input":{"to":"` + childID + `","summary":"collect results"}}]}}` + "\n" +
		`{"type":"assistant","uuid":"rec-4","sessionId":"` + parentID + `","message":{"role":"assistant","content":[{"type":"tool_use","name":"SendMessage","input":{"to":"peer-planning-session","summary":"handoff"}}]}}` + "\n"
	sum := writeClaudeParentWithSubagents(t, root, "-work-repo", parentID, body, []string{childID})

	edges, supported, err := Edges(sum)
	if err != nil || !supported {
		t.Fatalf("Edges: supported=%v err=%v", supported, err)
	}
	var spawned, resolved, unresolved int
	for _, edge := range edges {
		switch edge.Kind {
		case EdgeKindSpawned:
			spawned++
			if edge.To.ID != childID || edge.Anchor != "rec-2" {
				t.Fatalf("spawned edge = %+v", edge)
			}
		case EdgeKindMessaged:
			if edge.To.Unresolved {
				unresolved++
				if edge.To.Raw != "peer-planning-session" || edge.To.ID != "" {
					t.Fatalf("unresolved messaged edge = %+v", edge)
				}
			} else {
				resolved++
				if edge.To.ID != childID {
					t.Fatalf("resolved messaged edge = %+v", edge)
				}
			}
		}
	}
	if spawned != 1 || resolved != 1 || unresolved != 1 || len(edges) != 3 {
		t.Fatalf("edges = %+v (spawned=%d resolved=%d unresolved=%d)", edges, spawned, resolved, unresolved)
	}
}

// TestClaudeWorktreeTwinIsNotLineage is the negative fixture: one session id
// written under two project slugs (the worktree twin) is a mirror, not a
// spawn — no lineage claim, no spawned edge.
func TestClaudeWorktreeTwinIsNotLineage(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude", "projects")
	twinID := "99999999-7777-7777-7777-777777777777"
	body := `{"type":"user","uuid":"rec-1","sessionId":"` + twinID + `","message":{"role":"user","content":"same work"}}` + "\n"
	twins := 0
	for _, slug := range []string{"-work-repo", "-work-repo--claude-worktrees-fix"} {
		dir := filepath.Join(root, slug)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, twinID+".jsonl")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		sum, _, _, ok := claudeRuntime{}.Summarize(fileJob{runtime: "claude", path: path})
		if !ok {
			t.Fatal("Summarize failed")
		}
		twins++
		if facts, ok := Lineage(sum); ok {
			t.Fatalf("worktree twin produced a lineage claim: %+v", facts)
		}
		edges, supported, err := Edges(sum)
		if err != nil || !supported || len(edges) != 0 {
			t.Fatalf("worktree twin produced edges: %+v (supported=%v err=%v)", edges, supported, err)
		}
	}
	if twins != 2 {
		t.Fatalf("fixture wrote %d twins, want 2", twins)
	}
}

// TestOpenCodeParentIDIsSpawnedLineage: the first readers of the ParentID the
// opencode adapter has always written.
func TestOpenCodeParentIDIsSpawnedLineage(t *testing.T) {
	child := SessionSummary{Runtime: "opencode", ID: "ses_child", ParentID: "ses_parent"}
	facts, ok := Lineage(child)
	if !ok || facts.Parent.ID != "ses_parent" || facts.Parent.Runtime != "opencode" ||
		facts.Provenance != EdgeProvenanceObserved {
		t.Fatalf("Lineage = %+v ok=%v", facts, ok)
	}
	edges, supported, err := Edges(child)
	if err != nil || !supported || len(edges) != 1 {
		t.Fatalf("Edges = %+v supported=%v err=%v", edges, supported, err)
	}
	if edges[0].Kind != EdgeKindSpawned || edges[0].From.ID != "ses_parent" || edges[0].To.ID != "ses_child" {
		t.Fatalf("spawned edge = %+v", edges[0])
	}
	if _, ok := Lineage(SessionSummary{Runtime: "opencode", ID: "ses_top"}); ok {
		t.Fatal("top-level opencode session claimed lineage")
	}
}

// TestCapabilityMatrixListsEveryRuntime: the matrix carries one row per
// registered runtime and an explicit bool per capability, so a silently
// half-implemented runtime is visible rather than absent.
func TestCapabilityMatrixListsEveryRuntime(t *testing.T) {
	matrix := CapabilityMatrix()
	names := RuntimeNames()
	if len(matrix) != len(names) {
		t.Fatalf("matrix rows = %d, registered runtimes = %d", len(matrix), len(names))
	}
	for _, name := range names {
		row, ok := matrix[name]
		if !ok {
			t.Fatalf("runtime %q missing from matrix", name)
		}
		for _, capability := range []string{"session_lineage", "edge_source", "turn_anchorer", "resume_handle", "session_source", "deep_normalizer", "repository_grouper"} {
			if _, present := row.Capabilities[capability]; !present {
				t.Fatalf("runtime %q row lacks explicit %q entry", name, capability)
			}
		}
	}
	// spot checks against what the adapters actually implement
	for runtime, expected := range map[string]map[string]bool{
		"codex":    {"session_lineage": true, "edge_source": true, "turn_anchorer": true, "resume_handle": true},
		"claude":   {"session_lineage": true, "edge_source": true, "turn_anchorer": true, "resume_handle": false, "repository_grouper": true},
		"opencode": {"session_lineage": true, "edge_source": true, "turn_anchorer": false, "session_source": true},
	} {
		for capability, want := range expected {
			if got := matrix[runtime].Capabilities[capability]; got != want {
				t.Fatalf("matrix[%s][%s] = %v, want %v", runtime, capability, got, want)
			}
		}
	}
	// interface revisions are published per adapter (natural-session plan Slice A)
	for runtime, revision := range map[string]string{
		"claude":   "2.1.212",
		"codex":    "0.149.0-alpha.4.3",
		"opencode": "1.18.0",
	} {
		if got := matrix[runtime].InterfaceRevision; got != revision {
			t.Fatalf("matrix[%s].interface_revision = %q, want %q", runtime, got, revision)
		}
		if got := CLIRevision(runtime); got != revision {
			t.Fatalf("CLIRevision(%q) = %q, want %q", runtime, got, revision)
		}
	}
}
