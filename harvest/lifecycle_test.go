package harvest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTranscriptResultIdentityUsesCompleteSourceRecord(t *testing.T) {
	first := writeLifecycleFixture(t, "first.jsonl", `{"timestamp":"2026-08-23T10:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_same","status":"completed","output":"same"}}`+"\n")
	second := writeLifecycleFixture(t, "second.jsonl", `{"timestamp":"2026-08-23T10:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_same","status":"failed","output":"same"}}`+"\n")
	one, err := normalizeCodexLifecycle(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := normalizeCodexLifecycle(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Results) != 1 || len(two.Results) != 1 {
		t.Fatalf("result counts = %d/%d", len(one.Results), len(two.Results))
	}
	if one.Results[0].PayloadDigest != two.Results[0].PayloadDigest {
		t.Fatal("same decoded output did not retain the same payload digest")
	}
	if one.Results[0].ObservationID == two.Results[0].ObservationID ||
		one.Results[0].SourceDigest == two.Results[0].SourceDigest {
		t.Fatal("complete-record identity ignored changed status metadata")
	}
}

func TestLifecycleOversizedRecordDiscardsIncrementallyAndCollectsFollowingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.jsonl")
	oversized := append([]byte(`{"payload":"`), bytes.Repeat([]byte("x"), int(MaxLifecycleRecordBytes)+(256<<10))...)
	oversized = append(oversized, []byte(`"}`+"\n")...)
	valid := []byte(`{"type":"assistant","sessionId":"session-after","message":{"content":[]}}` + "\n")
	if err := os.WriteFile(path, append(oversized, valid...), 0o600); err != nil {
		t.Fatal(err)
	}
	request := LifecycleReadRequest{Path: path, SourceGeneration: "generation-a",
		MaxBytes: 256 << 10}
	var firstIssue string
	for turns := 0; turns < 32; turns++ {
		batch, err := normalizeClaudeLifecycleIncremental(request)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.OversizedRecords) > 0 {
			if firstIssue == "" {
				firstIssue = batch.OversizedRecords[0].IssueID
			} else if batch.OversizedRecords[0].IssueID != firstIssue {
				t.Fatal("oversized issue identity changed during discard")
			}
		}
		request.Offset, request.SourceLine, request.State = batch.NextOffset, batch.NextSourceLine, batch.ParserState
		if request.Offset == int64(len(oversized)+len(valid)) {
			if firstIssue == "" || request.SourceLine != 2 {
				t.Fatalf("discard completion = issue %q line %d", firstIssue, request.SourceLine)
			}
			return
		}
	}
	t.Fatalf("oversized record did not unblock following record; offset=%d", request.Offset)
}

func TestLifecycleReportsEachConsecutiveOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "consecutive.jsonl")
	one := append(bytes.Repeat([]byte("a"), int(MaxLifecycleRecordBytes)+1), '\n')
	two := append(bytes.Repeat([]byte("b"), int(MaxLifecycleRecordBytes)+1), '\n')
	valid := []byte(`{"type":"assistant","sessionId":"after-two","message":{"content":[]}}` + "\n")
	body := append(append(append([]byte(nil), one...), two...), valid...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	batch, err := normalizeClaudeLifecycleIncremental(LifecycleReadRequest{Path: path,
		SourceGeneration: "generation-two", MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.OversizedRecords) != 2 || batch.OversizedRecords[0].IssueID ==
		batch.OversizedRecords[1].IssueID {
		t.Fatalf("oversized observations=%+v", batch.OversizedRecords)
	}
	if batch.NextOffset != int64(len(body)) || batch.NextSourceLine != 3 {
		t.Fatalf("batch boundary offset=%d line=%d", batch.NextOffset, batch.NextSourceLine)
	}
}

func TestLifecycleSixtyFourMiBOversizedRecordResumesWithoutRetainingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized-64m.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for written := 0; written < 64<<20; written += len(chunk) {
		if _, err := f.Write(chunk); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
	}
	valid := []byte("\n" + `{"type":"assistant","sessionId":"after-large","message":{"content":[]}}` + "\n")
	if _, err := f.Write(valid); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	request := LifecycleReadRequest{Path: path, SourceGeneration: "generation-large",
		MaxBytes: DefaultLifecycleReadBytes}
	issueID := ""
	for sweep := 0; sweep < 96; sweep++ {
		batch, err := normalizeClaudeLifecycleIncremental(request)
		if err != nil {
			t.Fatal(err)
		}
		if batch.BytesInspected > MaxLifecycleRecordBytes+DefaultLifecycleReadBytes+
			int64(lifecycleReaderBufferBytes) {
			t.Fatalf("sweep retained/inspected too much: %d bytes", batch.BytesInspected)
		}
		if batch.PeakRetainedBytes > MaxLifecycleRecordBytes+DefaultLifecycleReadBytes+
			int64(lifecycleReaderBufferBytes) {
			t.Fatalf("sweep retained allocation bound=%d bytes", batch.PeakRetainedBytes)
		}
		if len(batch.ParserState) > MaxLifecycleStateBytes {
			t.Fatalf("parser state grew to %d bytes", len(batch.ParserState))
		}
		for _, issue := range batch.OversizedRecords {
			if issueID == "" {
				issueID = issue.IssueID
			} else if issue.IssueID != issueID {
				t.Fatalf("issue identity changed across restart: %q != %q", issue.IssueID, issueID)
			}
		}
		// Constructing the next request solely from committed output simulates a
		// collector restart between every bounded sweep.
		request.Offset, request.SourceLine, request.State = batch.NextOffset,
			batch.NextSourceLine, append([]byte(nil), batch.ParserState...)
		if request.Offset == (64<<20)+int64(len(valid)) {
			if issueID == "" || request.SourceLine != 2 {
				t.Fatalf("completion issue=%q line=%d", issueID, request.SourceLine)
			}
			return
		}
	}
	t.Fatalf("64 MiB discard did not reach the following record; offset=%d", request.Offset)
}

func TestBoundedLifecycleStateReportsExactEvictedCall(t *testing.T) {
	calls := map[string]lifecycleCallState{
		"first":  {Tool: string(bytes.Repeat([]byte("a"), MaxLifecycleStateBytes))},
		"second": {Tool: "keep"},
	}
	body, order, evicted, err := boundedLifecycleState(calls, []string{"first", "second"}, "session", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxLifecycleStateBytes || len(evicted) != 1 || evicted[0] != "first" ||
		len(order) != 1 || order[0] != "second" {
		t.Fatalf("bounded state order=%v evicted=%v bytes=%d", order, evicted, len(body))
	}
}

func TestLifecycleIssueIdentitySeparatesRuntimeSourceAndSegment(t *testing.T) {
	call := lifecycleCallState{Tool: "edit", Session: "supplied"}
	base := newEvictedCallObservation("runtime-a", "session", "segment-a", "/one/source.jsonl",
		"generation", "call", call)
	for _, changed := range []EvictedCallObservation{
		newEvictedCallObservation("runtime-b", "session", "segment-a", "/one/source.jsonl",
			"generation", "call", call),
		newEvictedCallObservation("runtime-a", "session", "segment-b", "/one/source.jsonl",
			"generation", "call", call),
		newEvictedCallObservation("runtime-a", "session", "segment-a", "/two/source.jsonl",
			"generation", "call", call),
	} {
		if changed.OccurrenceID == base.OccurrenceID {
			t.Fatalf("eviction identity collided: base=%+v changed=%+v", base, changed)
		}
	}
	path := writeLifecycleFixture(t, "identity.jsonl", "{}\n")
	one, err := newLifecycleRecordReader(LifecycleReadRequest{Path: path, Runtime: "runtime-a",
		SourceSegmentID: "segment-a", SourceGeneration: "generation"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = one.Close() })
	two, err := newLifecycleRecordReader(LifecycleReadRequest{Path: path, Runtime: "runtime-b",
		SourceSegmentID: "segment-a", SourceGeneration: "generation"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = two.Close() })
	if one.oversizedIssueID(0) == two.oversizedIssueID(0) {
		t.Fatal("oversized identity omitted runtime")
	}
}

func writeLifecycleFixture(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeLifecyclePreservesResultAndProposedReplacement(t *testing.T) {
	path := writeLifecycleFixture(t, "session-a.jsonl", `{"type":"assistant","sessionId":"session-a","timestamp":"2026-08-23T10:00:00Z","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Edit","input":{"file_path":"src/a.go","old_string":"old","new_string":"new","replace_all":true}}]}}
{"type":"user","sessionId":"session-a","timestamp":"2026-08-23T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Updated src/a.go","is_error":false}]}}
`)
	got, err := normalizeClaudeLifecycle(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results=%d want 1", len(got.Results))
	}
	r := got.Results[0]
	if r.NativeCallID != "toolu_1" || r.NativeCallKind != "tool_use_id" || r.Tool != "Edit" || r.State != "success" {
		t.Fatalf("unexpected result: %#v", r)
	}
	if r.SourceSegmentID != "session-a" || r.SuppliedSessionID != "session-a" {
		t.Fatalf("identity not preserved: %#v", r)
	}
	if len(r.Effects) != 1 {
		t.Fatalf("effects=%d want 1", len(r.Effects))
	}
	e := r.Effects[0]
	if e.RawIdentity != "src/a.go" || e.Operation != "update" || e.EvidenceSource != "derived-input" || !e.ReplaceAll {
		t.Fatalf("unexpected effect: %#v", e)
	}
	if string(e.ReplacementBefore) != "old" || string(e.ReplacementAfter) != "new" || e.DiffCompleteness != "unavailable" {
		t.Fatalf("replacement was mislabeled or lost: %#v", e)
	}
}

func TestCodexLifecyclePreservesPatchEffectsAndLogicalResultSources(t *testing.T) {
	path := writeLifecycleFixture(t, "rollout-test.jsonl", `{"timestamp":"2026-08-23T10:00:00Z","payload":{"type":"session_meta","payload":{"session_id":"conversation-1"}}}
{"timestamp":"2026-08-23T10:00:01Z","payload":{"type":"response_item","payload":{"type":"function_call","call_id":"call_1","name":"apply_patch","arguments":"*** Begin Patch"}}}
{"timestamp":"2026-08-23T10:00:02Z","payload":{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"Done!"}}}
{"timestamp":"2026-08-23T10:00:03Z","payload":{"type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_1","success":true,"stdout":"Done!","changes":[{"path":"src/a.go","kind":"update","diff":"@@ -1 +1 @@\n-old\n+new\n"},{"path":"src/b.go","kind":"add","content":"package b\n"}]}}}
`)
	got, err := normalizeCodexLifecycle(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanonicalSessionID != "conversation-1" || got.SourceSegmentID != "rollout-test" {
		t.Fatalf("batch identity: %#v", got)
	}
	if len(got.Actions) != 1 || got.Actions[0].NativeCallID != "call_1" ||
		got.Actions[0].NativeCallKind != "call_id" || got.Actions[0].Tool != "apply_patch" ||
		string(got.Actions[0].InputPayload) != "*** Begin Patch" || got.Actions[0].Completeness != "complete" {
		t.Fatalf("outer action not preserved: %#v", got.Actions)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results=%d want two independent source observations", len(got.Results))
	}
	patch := got.Results[1]
	if patch.NativeCallID != "call_1" || patch.NativeCallKind != "patch_call_id" || patch.Tool != "apply_patch" || patch.State != "success" || patch.SourceKind != "vendor-patch-result" {
		t.Fatalf("unexpected patch result: %#v", patch)
	}
	if len(patch.NativeCallAliases) != 1 || patch.NativeCallAliases[0].NativeCallKind != "tool_use_id" ||
		patch.NativeCallAliases[0].NativeCallID != "call_1" || patch.NativeCallAliases[0].Algorithm != "codex-patch-exec-id-v1" {
		t.Fatalf("patch identity alias = %#v", patch.NativeCallAliases)
	}
	if len(patch.Effects) != 2 {
		t.Fatalf("effects=%d want 2", len(patch.Effects))
	}
	if patch.Effects[0].Operation != "update" || patch.Effects[0].DiffCompleteness != "complete" || string(patch.Effects[0].DiffPayload) == "" {
		t.Fatalf("update effect lost: %#v", patch.Effects[0])
	}
	if patch.Effects[1].Operation != "create" || string(patch.Effects[1].ContentPayload) != "package b\n" {
		t.Fatalf("add effect lost: %#v", patch.Effects[1])
	}
}

func TestCodexLifecycleParsesCurrentFlatCustomCallsAndPathKeyedPatchChanges(t *testing.T) {
	path := writeLifecycleFixture(t, "rollout-flat.jsonl", `{"timestamp":"2026-08-23T10:00:00Z","type":"session_meta","payload":{"session_id":"conversation-flat","cwd":"/repo"}}
{"timestamp":"2026-08-23T10:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_flat","name":"exec","input":"patch"}}
{"timestamp":"2026-08-23T10:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","id":"ctco_1","call_id":"call_flat","output":[{"type":"input_text","text":"done"}]}}
{"timestamp":"2026-08-23T10:00:03Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"exec_flat","success":true,"stdout":"Success","stderr":"","changes":{"src/z.go":{"type":"add","unified_diff":"@@ -0,0 +1 @@\n+package z\n"},"src/a.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-old\n+new\n"}}}}
`)
	got, err := normalizeCodexLifecycle(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanonicalSessionID != "conversation-flat" || len(got.Actions) != 1 || len(got.Results) != 2 {
		t.Fatalf("batch = %+v", got)
	}
	if got.Actions[0].NativeCallID != "call_flat" || got.Actions[0].Tool != "exec" ||
		got.Actions[0].MediaType != "text/plain; charset=utf-8" || string(got.Actions[0].InputPayload) != "patch" {
		t.Fatalf("custom action = %+v", got.Actions[0])
	}
	if got.Results[0].NativeCallID != "call_flat" || got.Results[0].Tool != "exec" {
		t.Fatalf("custom output = %+v", got.Results[0])
	}
	patch := got.Results[1]
	if patch.NativeCallID != "exec_flat" || patch.NativeCallKind != "patch_call_id" || patch.Tool != "apply_patch" ||
		len(patch.NativeCallAliases) != 1 || patch.NativeCallAliases[0].NativeCallID != "exec_flat" ||
		patch.StdoutBytes != len("Success") || patch.StderrDigest == "" {
		t.Fatalf("patch metadata = %+v", patch)
	}
	if len(patch.Effects) != 2 || patch.Effects[0].RawIdentity != "src/a.go" || patch.Effects[0].Operation != "update" || patch.Effects[1].RawIdentity != "src/z.go" || patch.Effects[1].Operation != "create" {
		t.Fatalf("ordered effects = %+v", patch.Effects)
	}
}

func TestLifecycleNormalizerIsOptionalRuntimeCapability(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		if _, ok := runtimeFor(runtime).(LifecycleNormalizer); !ok {
			t.Fatalf("%s does not expose LifecycleNormalizer", runtime)
		}
	}
}

func TestClaudeLifecycleIncrementalCommitsOnlyCompleteRecordsAndCarriesCallState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-incremental.jsonl")
	first := []byte(`{"type":"assistant","sessionId":"session-incremental","timestamp":"2026-08-23T10:00:00Z","message":{"content":[{"type":"tool_use","id":"toolu_inc","name":"Edit","input":{"file_path":"src/é.go","old_string":"old","new_string":"new"}}]}}` + "\n")
	partial := []byte(`{"type":"user","sessionId":"session-incremental","timestamp":"2026-08-23T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_inc","content":"upd`)
	if err := os.WriteFile(path, append(append([]byte(nil), first...), partial...), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, err := normalizeClaudeLifecycleIncremental(LifecycleReadRequest{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Results) != 0 || batch.NextOffset != int64(len(first)) || batch.NextSourceLine != 1 {
		t.Fatalf("partial batch advanced or emitted: %+v", batch)
	}
	if len(batch.ParserState) == 0 {
		t.Fatal("pending Edit call state was not retained")
	}
	retainedState, err := decodeLifecycleState(batch.ParserState)
	if err != nil {
		t.Fatal(err)
	}
	pending := retainedState.Calls["toolu_inc"].Effects[0]
	if len(pending.ReplacementBefore) != 0 || len(pending.ReplacementAfter) != 0 ||
		pending.BeforeBytes != 3 || pending.AfterBytes != 3 {
		t.Fatalf("metadata-only parser state retained bodies or lost counts: %+v", pending)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`ated","is_error":false}]}}` + "\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	next, err := normalizeClaudeLifecycleIncremental(LifecycleReadRequest{Path: path,
		Offset: batch.NextOffset, SourceLine: batch.NextSourceLine, State: batch.ParserState})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Results) != 1 || next.Results[0].Tool != "Edit" || len(next.Results[0].Effects) != 1 {
		t.Fatalf("completed incremental result lost call/effect: %+v", next.Results)
	}
	if next.Results[0].Effects[0].RawIdentity != "src/é.go" {
		t.Fatalf("UTF-8 path changed: %+v", next.Results[0].Effects[0])
	}
}

func TestCodexLifecycleIncrementalPreservesCanonicalAndCallAcrossBudgets(t *testing.T) {
	path := writeLifecycleFixture(t, "rollout-budget.jsonl", `{"timestamp":"2026-08-23T10:00:00Z","payload":{"type":"session_meta","payload":{"session_id":"conversation-budget"}}}
{"timestamp":"2026-08-23T10:00:01Z","payload":{"type":"response_item","payload":{"type":"function_call","call_id":"call_budget","name":"apply_patch"}}}
{"timestamp":"2026-08-23T10:00:02Z","payload":{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_budget","output":"Done"}}}
{"timestamp":"2026-08-23T10:00:03Z","payload":{"type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_budget","success":true,"changes":[{"path":"src/a.go","kind":"update","diff":"@@"}]}}}
`)
	first, err := normalizeCodexLifecycleIncremental(LifecycleReadRequest{Path: path, MaxRecords: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Continuation || first.CanonicalSessionID != "conversation-budget" || len(first.Actions) != 1 || len(first.Results) != 0 {
		t.Fatalf("first bounded batch: %+v", first)
	}
	second, err := normalizeCodexLifecycleIncremental(LifecycleReadRequest{Path: path,
		Offset: first.NextOffset, SourceLine: first.NextSourceLine, State: first.ParserState})
	if err != nil {
		t.Fatal(err)
	}
	if second.CanonicalSessionID != "conversation-budget" || len(second.Results) != 2 ||
		second.Results[0].Tool != "apply_patch" || second.Results[1].Tool != "apply_patch" {
		t.Fatalf("second bounded batch lost state: %+v", second)
	}
}

func TestCodexLifecyclePreservesLocalShellActionWithoutInventingToolOrInput(t *testing.T) {
	path := writeLifecycleFixture(t, "rollout-shell.jsonl", `{"timestamp":"2026-08-23T10:00:00Z","payload":{"type":"session_meta","payload":{"session_id":"conversation-shell"}}}
{"timestamp":"2026-08-23T10:00:01Z","payload":{"type":"response_item","payload":{"type":"local_shell_call","call_id":"call_shell","action":{"command":"go test ./..."}}}}
{"timestamp":"2026-08-23T10:00:02Z","payload":{"type":"response_item","payload":{"type":"local_shell_call_output","call_id":"call_shell","output":"ok"}}}
`)
	got, err := normalizeCodexLifecycle(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Actions) != 1 || got.Actions[0].Tool != "local_shell_call" ||
		got.Actions[0].MediaType != "application/json; charset=utf-8" ||
		got.Actions[0].NativeCallID != "call_shell" || got.Actions[0].InputDigest == "" {
		t.Fatalf("local shell action = %+v", got.Actions)
	}
	if len(got.Results) != 1 || got.Results[0].Tool != "local_shell_call" {
		t.Fatalf("local shell result = %+v", got.Results)
	}
}
