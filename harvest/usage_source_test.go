package harvest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// claudeUsageLine is one assistant line of a Claude transcript fixture.
func claudeUsageLine(session, messageID, model, ts string, input, output, thinking int64) string {
	usage := `{"input_tokens":` + itoa(input) + `,"cache_read_input_tokens":100,"cache_creation_input_tokens":30,` +
		`"output_tokens":` + itoa(output) + `,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20}`
	if thinking >= 0 {
		usage += `,"output_tokens_details":{"thinking_tokens":` + itoa(thinking) + `}`
	}
	usage += `}`
	return `{"type":"assistant","sessionId":"` + session + `","timestamp":"` + ts + `","effort":"high","version":"2.1.280",` +
		`"message":{"id":"` + messageID + `","model":"` + model + `","content":[{"type":"text","text":"x"}],"usage":` + usage + `}}`
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAllUsage(t *testing.T, source UsageSource, ref UsageSourceRef, budget int64) ([]UsageCall, []byte) {
	t.Helper()
	var calls []UsageCall
	var cursor []byte
	for pass := 0; pass < 1000; pass++ {
		batch, err := source.ReadUsage(context.Background(), UsageReadRequest{Source: ref, Cursor: cursor, Budget: budget})
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, batch.Calls...)
		cursor = batch.Cursor
		if batch.Complete {
			return calls, cursor
		}
	}
	t.Fatal("a source never completed: the reader made no progress")
	return nil, nil
}

func usageRefFor(t *testing.T, source UsageSource, session string) []UsageSourceRef {
	t.Helper()
	refs, err := source.UsageSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []UsageSourceRef
	for _, ref := range refs {
		if ref.Session == session {
			out = append(out, ref)
		}
	}
	return out
}

func TestClaudeUsageOneCallPerMessageLastLineWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	session := "11111111-1111-1111-1111-111111111111"
	path := filepath.Join(home, ".claude", "projects", "-work-repo", session+".jsonl")
	writeLines(t, path,
		// identical repeat: one API response written as three content-block lines
		claudeUsageLine(session, "msg_a", "claude-opus-5", "2026-09-24T10:00:00Z", 5, 40, 12),
		claudeUsageLine(session, "msg_a", "claude-opus-5", "2026-09-24T10:00:00Z", 5, 40, 12),
		claudeUsageLine(session, "msg_a", "claude-opus-5", "2026-09-24T10:00:00Z", 5, 40, 12),
		// zero placeholder, then the real line of the same id
		claudeUsageLine(session, "msg_b", "claude-opus-5", "2026-09-24T10:01:00Z", 0, 0, 0),
		claudeUsageLine(session, "msg_b", "claude-opus-5", "2026-09-24T10:01:05Z", 7, 50, -1),
		// a synthetic failure placeholder is not a model call
		claudeUsageLine(session, "msg_c", "<synthetic>", "2026-09-24T10:02:00Z", 1, 1, -1),
		// copied history from another session is not this session's call
		claudeUsageLine("22222222-2222-2222-2222-222222222222", "msg_x", "claude-opus-5", "2026-09-24T09:00:00Z", 9, 9, -1),
	)
	refs := usageRefFor(t, claudeRuntime{}, session)
	if len(refs) != 1 {
		t.Fatalf("sources: %+v", refs)
	}
	calls, _ := readAllUsage(t, claudeRuntime{}, refs[0], 1<<20)
	if len(calls) != 2 {
		t.Fatalf("want one call per message id, got %d: %+v", len(calls), calls)
	}
	a, b := calls[0], calls[1]
	if a.ID != "msg_a" || *a.Output != 40 || *a.Reasoning != 12 || *a.Input != 5 || *a.CacheRead != 100 ||
		*a.CacheWrite != 30 || a.Effort != "high" || a.Client != "2.1.280" || a.Delegated() {
		t.Fatalf("call a: %+v", a)
	}
	if len(a.Parts) != 2 || a.Parts[0].Of != TokenClassCacheWrite || a.Parts[0].Count+a.Parts[1].Count != 30 {
		t.Fatalf("cache lifetimes are parts of cache-write: %+v", a.Parts)
	}
	if b.ID != "msg_b" || *b.Output != 50 || b.Reasoning != nil ||
		!b.FirstAt.Equal(parseUsageTime("2026-09-24T10:01:00Z")) || !b.At.Equal(parseUsageTime("2026-09-24T10:01:05Z")) {
		t.Fatalf("the placeholder is not a call, but its time is the call's first line: %+v", b)
	}
}

func TestClaudeUsageStreamingSnapshotAcrossPasses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	session := "33333333-3333-3333-3333-333333333333"
	path := filepath.Join(home, ".claude", "projects", "-work-repo", session+".jsonl")
	first := claudeUsageLine(session, "msg_s", "claude-sonnet-5", "2026-09-24T10:00:00Z", 3, 2, -1)
	last := claudeUsageLine(session, "msg_s", "claude-sonnet-5", "2026-09-24T10:00:09Z", 3, 90, 60)
	writeLines(t, path, first, last)
	ref := usageRefFor(t, claudeRuntime{}, session)[0]
	// A budget that ends the first read after the first line: the id straddles
	// two reads, and the second read carries the complete figures.
	batch, err := claudeRuntime{}.ReadUsage(context.Background(),
		UsageReadRequest{Source: ref, Budget: int64(len(first) + 1)})
	if err != nil || batch.Complete || len(batch.Calls) != 1 || *batch.Calls[0].Output != 2 {
		t.Fatalf("first read: %+v err=%v", batch, err)
	}
	second, err := claudeRuntime{}.ReadUsage(context.Background(),
		UsageReadRequest{Source: ref, Cursor: batch.Cursor, Budget: 1 << 20})
	if err != nil || !second.Complete || len(second.Calls) != 1 || second.Calls[0].ID != "msg_s" ||
		*second.Calls[0].Output != 90 || *second.Calls[0].Reasoning != 60 {
		t.Fatalf("second read carries the last line: %+v err=%v", second, err)
	}
}

func TestClaudeUsageSubagentsAndTwins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := "44444444-4444-4444-4444-444444444444"
	projects := filepath.Join(home, ".claude", "projects")
	writeLines(t, filepath.Join(projects, "-work-repo", parent+".jsonl"),
		claudeUsageLine(parent, "msg_p", "claude-opus-5", "2026-09-24T10:00:00Z", 1, 10, 5))
	// Subagent lines carry the PARENT's session id; nested agents sit beside them.
	writeLines(t, filepath.Join(projects, "-work-repo", parent, "subagents", "agent-a1.jsonl"),
		claudeUsageLine(parent, "msg_s1", "claude-sonnet-5", "2026-09-24T10:00:01Z", 1, 20, 8))
	writeLines(t, filepath.Join(projects, "-work-repo", parent, "subagents", "agent-a2.jsonl"),
		claudeUsageLine(parent, "msg_s2", "claude-haiku-4-5", "2026-09-24T10:00:02Z", 1, 30, -1),
		claudeUsageLine("55555555-5555-5555-5555-555555555555", "msg_other", "claude-haiku-4-5",
			"2026-09-24T10:00:03Z", 1, 1, -1))
	// A worktree twin: the same stem in another project directory.
	writeLines(t, filepath.Join(projects, "-work-repo-wt", parent+".jsonl"),
		claudeUsageLine(parent, "msg_p", "claude-opus-5", "2026-09-24T10:00:00Z", 1, 10, 5))
	refs := usageRefFor(t, claudeRuntime{}, parent)
	if len(refs) != 4 {
		t.Fatalf("four sources: two twins and two subagent files, got %+v", refs)
	}
	keys := map[string]bool{}
	agents := map[string]bool{}
	for _, ref := range refs {
		if keys[ref.Key] || strings.Contains(ref.Key, "/") {
			t.Fatalf("keys are distinct and never path text: %+v", refs)
		}
		keys[ref.Key] = true
		calls, _ := readAllUsage(t, claudeRuntime{}, ref, 1<<20)
		for _, call := range calls {
			if call.Session != parent {
				t.Fatalf("every call belongs to the parent session: %+v", call)
			}
			agents[call.Agent] = true
			if call.Agent != "" && !call.Delegated() {
				t.Fatalf("a subagent call is delegated: %+v", call)
			}
		}
	}
	if !agents[""] || !agents["a1"] || !agents["a2"] || len(agents) != 3 {
		t.Fatalf("agents: %v", agents)
	}
}

// codexTokenCountLine is one token_count event with a running total and the
// call's own figures.
func codexTokenCountLine(ts string, total, input, cached, output, reasoning int64) string {
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":{` +
		`"total_token_usage":{"total_tokens":` + itoa(total) + `},"last_token_usage":{"input_tokens":` + itoa(input) +
		`,"cached_input_tokens":` + itoa(cached) + `,"cache_write_input_tokens":0,"output_tokens":` + itoa(output) +
		`,"reasoning_output_tokens":` + itoa(reasoning) + `},"model_context_window":258400}}}`
}

func TestCodexUsageCarriedInRestartAndRepeat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := "abcdef00-aaaa-7bbb-8ccc-00000000000a"
	parent := "abcdef00-aaaa-7bbb-8ccc-000000000009"
	path := filepath.Join(home, ".codex", "sessions", "2026", "09", "11", "rollout-2026-09-11T11-57-43-"+key+".jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-09-11T11:57:43Z","type":"session_meta","payload":{"id":"`+key+`","session_id":"`+parent+
			`","cli_version":"0.154.0","source":{"subagent":{"other":"guardian"}}}}`,
		`{"timestamp":"2026-09-11T11:57:44Z","type":"turn_context","payload":{"model":"gpt-6-astra","effort":"low"}}`,
		// carried in: the first total already holds the parent's history
		codexTokenCountLine("2026-09-11T11:57:45Z", 97624, 1000, 800, 50, 10),
		// repeated event: the same total is not a new call
		codexTokenCountLine("2026-09-11T11:57:46Z", 97624, 1000, 800, 50, 10),
		// a restart: the running total starts over, the call is still one call
		codexTokenCountLine("2026-09-11T11:58:00Z", 2000, 1900, 1500, 100, 40),
	)
	refs := usageRefFor(t, codexRuntime{}, "rollout-2026-09-11T11-57-43-"+key)
	if len(refs) != 1 || refs[0].Key != key || refs[0].SessionAlias != key {
		t.Fatalf("key from the filename: %+v", refs)
	}
	batch, err := codexRuntime{}.ReadUsage(context.Background(), UsageReadRequest{Source: refs[0], Budget: 1 << 20})
	if err != nil || len(batch.Calls) != 2 || batch.ParentSession != parent {
		t.Fatalf("calls: %+v err=%v", batch, err)
	}
	first, restarted := batch.Calls[0], batch.Calls[1]
	if !strings.HasPrefix(first.ID, parent+":") || first.ID == restarted.ID || *first.Input != 200 || *first.CacheRead != 800 || *first.Output != 50 ||
		*first.Reasoning != 10 || first.Model != "gpt-6-astra" || first.Effort != "low" || first.Client != "0.154.0" ||
		first.ParentSession != parent || *first.ContextWindow != 258400 {
		t.Fatalf("carried-in call: %+v", first)
	}
	if !strings.HasPrefix(restarted.ID, parent+":") || *restarted.Input != 400 || *restarted.Output != 100 {
		t.Fatalf("restart call: %+v", restarted)
	}
}

func TestCodexUsageLongLineAcrossPassBoundaryKeepsIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := "abcdef00-aaaa-7bbb-8ccc-0000000000f1"
	path := filepath.Join(home, ".codex", "sessions", "2026", "09", "11", "rollout-2026-09-11T00-00-00-"+key+".jsonl")
	long := `{"timestamp":"2026-09-11T00:00:02Z","type":"response_item","payload":{"type":"message","content":"` +
		strings.Repeat("x", int(MaxLifecycleRecordBytes)+(1<<20)) + `"}}`
	writeLines(t, path,
		`{"timestamp":"2026-09-11T00:00:00Z","type":"session_meta","payload":{"id":"`+key+`","cli_version":"0.153.4"}}`,
		codexTokenCountLine("2026-09-11T00:00:01Z", 100, 60, 20, 40, 5),
		long,
		codexTokenCountLine("2026-09-11T00:00:03Z", 300, 150, 50, 50, 7),
	)
	ref := usageRefFor(t, codexRuntime{}, "rollout-2026-09-11T00-00-00-"+key)[0]
	whole, _ := readAllUsage(t, codexRuntime{}, ref, 1<<30)
	// A budget smaller than the long line forces a pass boundary inside it.
	pieces, _ := readAllUsage(t, codexRuntime{}, ref, 1<<20)
	if len(whole) != 2 || len(pieces) != 2 {
		t.Fatalf("whole=%+v pieces=%+v", whole, pieces)
	}
	for index := range whole {
		if whole[index].ID != pieces[index].ID || *whole[index].Output != *pieces[index].Output {
			t.Fatalf("call ids must not depend on pass boundaries: %s vs %s", whole[index].ID, pieces[index].ID)
		}
	}
	if whole[0].ID == whole[1].ID || !strings.HasPrefix(whole[1].ID, key+":") {
		t.Fatalf("each call has its own id in the rollout's family: %s %s", whole[0].ID, whole[1].ID)
	}
}

func TestFileUsageRewriteDetection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	session := "66666666-6666-6666-6666-666666666666"
	path := filepath.Join(home, ".claude", "projects", "-work-repo", session+".jsonl")
	lines := []string{claudeUsageLine(session, "msg_0", "claude-opus-5", "2026-09-24T10:00:00Z", 1, 1, -1)}
	writeLines(t, path, lines...)
	ref := usageRefFor(t, claudeRuntime{}, session)[0]
	_, cursor := readAllUsage(t, claudeRuntime{}, ref, 1<<20)
	// Grow from under the prefix window to well past it: appends are never a rewrite.
	for index := 1; int64(len(strings.Join(lines, "\n"))) < 2*SourcePrefixWindow; index++ {
		lines = append(lines, claudeUsageLine(session, "msg_"+strconv.Itoa(index), "claude-opus-5",
			"2026-09-24T10:00:00Z", 1, 1, -1))
		writeLines(t, path, lines...)
		batch, err := claudeRuntime{}.ReadUsage(context.Background(),
			UsageReadRequest{Source: ref, Cursor: cursor, Budget: 1 << 20})
		if err != nil || batch.Restarted {
			t.Fatalf("append %d read as a rewrite: %+v err=%v", index, batch, err)
		}
		cursor = batch.Cursor
	}
	var state fileUsageCursor
	if err := json.Unmarshal(cursor, &state); err != nil || state.DigestLen != SourcePrefixWindow {
		t.Fatalf("the digest window stops at the prefix window: %+v err=%v", state, err)
	}
	// A real rewrite: different content at the start.
	lines[0] = claudeUsageLine(session, "msg_new", "claude-opus-5", "2026-09-24T11:00:00Z", 2, 2, -1)
	writeLines(t, path, lines...)
	batch, err := claudeRuntime{}.ReadUsage(context.Background(),
		UsageReadRequest{Source: ref, Cursor: cursor, Budget: 1 << 30})
	if err != nil || !batch.Restarted || batch.Calls[0].ID != "msg_new" {
		t.Fatalf("a rewrite restarts from the beginning: restarted=%v err=%v", batch.Restarted, err)
	}
}

// A forked child replays its parent's token_count events, restamped at the
// fork instant, then makes its own calls (code red-team C-1). A replayed call must carry its parent's id so the
// store counts it once; the child's own calls must not.
func TestCodexForkReplayRepeatsTheParentsCallIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	family := "abcdef00-aaaa-7bbb-8ccc-000000000004"
	child := "abcdef00-aaaa-7bbb-8ccc-000000000005"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "13")
	parentPath := filepath.Join(dir, "rollout-2026-08-11T08-07-12-"+family+".jsonl")
	childPath := filepath.Join(dir, "rollout-2026-08-13T20-47-08-"+child+".jsonl")
	parentCalls := []string{
		codexTokenCountLine("2026-08-11T08:10:00Z", 1000, 900, 700, 100, 20),
		codexTokenCountLine("2026-08-11T08:20:00Z", 2100, 1000, 800, 100, 30),
	}
	writeLines(t, parentPath, append([]string{
		`{"timestamp":"2026-08-11T08:07:12Z","type":"session_meta","payload":{"id":"` + family + `","session_id":"` + family + `","cli_version":"0.150.0"}}`,
	}, parentCalls...)...)
	fork := "2026-08-14T01:47:08.133Z"
	writeLines(t, childPath,
		`{"timestamp":"`+fork+`","type":"session_meta","payload":{"id":"`+child+`","session_id":"`+family+
			`","forked_from_id":"`+family+`","cli_version":"0.150.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"`+family+`","depth":1}}}}}`,
		// the replay: the parent's events, restamped at the fork instant
		codexTokenCountLine(fork, 1000, 900, 700, 100, 20),
		codexTokenCountLine(fork, 2100, 1000, 800, 100, 30),
		// the child's own call, also stamped at the fork instant
		codexTokenCountLine(fork, 3500, 1300, 1000, 100, 25),
	)
	ids := func(session string) []string {
		ref := usageRefFor(t, codexRuntime{}, session)[0]
		calls, _ := readAllUsage(t, codexRuntime{}, ref, 1<<20)
		out := []string{}
		for _, call := range calls {
			out = append(out, call.ID)
		}
		return out
	}
	parentIDs := ids("rollout-2026-08-11T08-07-12-" + family)
	childIDs := ids("rollout-2026-08-13T20-47-08-" + child)
	if len(parentIDs) != 2 || len(childIDs) != 3 {
		t.Fatalf("parent %v child %v", parentIDs, childIDs)
	}
	if childIDs[0] != parentIDs[0] || childIDs[1] != parentIDs[1] {
		t.Fatalf("a replayed call repeats its parent's id: parent %v child %v", parentIDs, childIDs)
	}
	if childIDs[2] == parentIDs[0] || childIDs[2] == parentIDs[1] {
		t.Fatal("the child's own call has its own id")
	}
}

// The listing states each subagent's vendor role from its sidecar, re-read
// only when the sidecar changes; a missing or malformed sidecar states none
// (session usage breakdown plan §5.1).
func TestClaudeUsageListingCarriesTheSidecarRole(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := "66666666-6666-6666-6666-666666666666"
	dir := filepath.Join(home, ".claude", "projects", "-work-repo", parent, "subagents")
	for _, id := range []string{"a1", "a2", "a3"} {
		writeLines(t, filepath.Join(dir, "agent-"+id+".jsonl"),
			claudeUsageLine(parent, "msg_"+id, "claude-opus-5", "2026-09-24T10:00:01Z", 1, 20, 8))
	}
	writeLines(t, filepath.Join(dir, "agent-a1.meta.json"), `{"agentType":"Explore","parentAgentId":"a0","spawnDepth":2}`)
	writeLines(t, filepath.Join(dir, "agent-a3.meta.json"), `{not json`)
	roles := func() map[string]string {
		out := map[string]string{}
		for _, ref := range usageRefFor(t, claudeRuntime{}, parent) {
			out[claudeSubagentID(ref.locator)] = ref.Role
		}
		return out
	}
	if got := roles(); got["a1"] != "Explore" || got["a2"] != "" || got["a3"] != "" || len(got) != 3 {
		t.Fatalf("roles: %+v", got)
	}
	writeLines(t, filepath.Join(dir, "agent-a2.meta.json"), `{"agentType":"general-purpose"}`)
	if got := roles(); got["a2"] != "general-purpose" || got["a1"] != "Explore" {
		t.Fatalf("a new sidecar is read on the next listing: %+v", got)
	}
	meta, ok := readClaudeSubagentMeta(filepath.Join(dir, "agent-a2.meta.json"))
	if !ok || meta.SpawnDepth != 0 {
		t.Fatalf("an unstated depth stays 0, the lineage's 'not stated': %+v", meta)
	}
}

// Codex states a child's role once, in session_meta; every read returns it,
// including a read that resumes after that line (plan P-3).
func TestCodexUsageCarriesTheRoleAcrossReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := "abcdef00-aaaa-7bbb-8ccc-0000000000f2"
	path := filepath.Join(home, ".codex", "sessions", "2026", "09", "11", "rollout-2026-09-11T11-57-43-"+key+".jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-09-11T11:57:43Z","type":"session_meta","payload":{"id":"`+key+
			`","cli_version":"0.154.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"p","depth":1,"agent_role":"review-javascript"}}}}}`,
		codexTokenCountLine("2026-09-11T11:57:45Z", 100, 100, 0, 5, 1),
		codexTokenCountLine("2026-09-11T11:57:46Z", 200, 100, 0, 5, 1),
	)
	refs := usageRefFor(t, codexRuntime{}, "rollout-2026-09-11T11-57-43-"+key)
	if len(refs) != 1 || refs[0].Role != "" {
		t.Fatalf("a Codex listing never states a role: %+v", refs)
	}
	var cursor []byte
	for pass := 0; pass < 10; pass++ {
		batch, err := codexRuntime{}.ReadUsage(context.Background(), UsageReadRequest{Source: refs[0], Cursor: cursor, Budget: 300})
		if err != nil {
			t.Fatal(err)
		}
		if batch.BytesRead > 0 && batch.Role != "review-javascript" {
			t.Fatalf("pass %d role: %q", pass, batch.Role)
		}
		cursor = batch.Cursor
		if batch.Complete {
			return
		}
	}
	t.Fatal("never completed")
}
