package guardcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestCursorHookInputNormalizesDocumentedAliases(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "work", "repo")
	working := filepath.Join(repo, "subdir")
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "preToolUse",
		"conversation_id": "cursor-conversation",
		"tool_name":       "Shell",
		"tool_use_id":     "cursor-call",
		"workspace_roots": []string{repo},
		"tool_input": map[string]any{
			"command":           "go test ./...",
			"working_directory": working,
		},
	})
	var input hookInput
	if err := json.Unmarshal(payload, &input); err != nil {
		t.Fatal(err)
	}
	if input.HookEventName != "PreToolUse" || input.SessionID != "cursor-conversation" ||
		input.ToolUseID != "cursor-call" || input.Cwd != working ||
		string(input.ToolInput.Command) != "go test ./..." {
		t.Fatalf("normalized Cursor input = %+v tool=%+v", input, input.ToolInput)
	}
}

func TestCursorResultAliasesRetainFailureDurationAndOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := []byte(`{
  "hook_event_name":"postToolUseFailure",
  "conversation_id":"cursor-conversation",
  "tool_name":"Shell",
  "tool_use_id":"cursor-call",
  "cwd":"/repo",
  "duration":42,
  "error_message":"command failed",
  "tool_output":"{\"exitCode\":1}"
}`)
	var input hookInput
	if err := json.Unmarshal(payload, &input); err != nil {
		t.Fatal(err)
	}
	if input.HookEventName != "PostToolUseFailure" || input.DurationMS != 42 ||
		!input.ToolIsError || input.ToolError != "command failed" ||
		string(input.RawToolResponse) != `"{\"exitCode\":1}"` {
		t.Fatalf("normalized Cursor result = %+v output=%s", input, input.RawToolResponse)
	}
	input.Runtime = cursorVendor
	envelope, err := buildResultEnvelope(input)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Runtime != cursorVendor || envelope.SessionID != "cursor-conversation" ||
		envelope.NativeCallID != "cursor-call" || envelope.NativeCallKind != "tool_use_id" ||
		envelope.State != "failure" || envelope.DurationMS != 42 || envelope.DecodedBytes == 0 {
		t.Fatalf("Cursor result envelope = %+v", envelope)
	}
}

func TestCursorLifecycleUsesExistingEntryAndClosureEnvelopes(t *testing.T) {
	var start hookInput
	if err := json.Unmarshal([]byte(`{
  "hook_event_name":"sessionStart",
  "conversation_id":"cursor-lifecycle",
  "source":"startup",
  "workspace_roots":["/repo"]
}`), &start); err != nil {
		t.Fatal(err)
	}
	start.Runtime = cursorVendor
	entry, err := buildSessionEntryEnvelope(start)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Runtime != cursorVendor || entry.SessionID != "cursor-lifecycle" ||
		entry.HookEventName != "SessionStart" || entry.EntryKind != "start" || entry.Cwd != "/repo" {
		t.Fatalf("Cursor session entry = %+v", entry)
	}

	var end hookInput
	if err := json.Unmarshal([]byte(`{
  "hook_event_name":"sessionEnd",
  "conversation_id":"cursor-lifecycle",
  "cwd":"/repo"
}`), &end); err != nil {
		t.Fatal(err)
	}
	end.Runtime = cursorVendor
	closure, err := buildClosureEnvelope(end)
	if err != nil {
		t.Fatal(err)
	}
	if closure.Runtime != cursorVendor || closure.SessionID != "cursor-lifecycle" ||
		closure.HookEventName != "SessionEnd" || closure.Cwd != "/repo" {
		t.Fatalf("Cursor closure = %+v", closure)
	}
}

func TestCursorHookInputRejectsConflictingAliases(t *testing.T) {
	for _, payload := range []string{
		`{"session_id":"one","conversation_id":"two"}`,
		`{"duration_ms":1,"duration":2}`,
	} {
		var input hookInput
		if err := json.Unmarshal([]byte(payload), &input); err == nil {
			t.Fatalf("conflicting Cursor aliases accepted: %s", payload)
		}
	}
}

func TestCursorWorkspaceFallbackDoesNotGuessAcrossRootsOrOutsideFiles(t *testing.T) {
	repoA := filepath.Join(string(filepath.Separator), "work", "a")
	repoB := filepath.Join(string(filepath.Separator), "work", "b")
	for _, test := range []struct {
		name    string
		payload map[string]any
	}{
		{name: "ambiguous roots", payload: map[string]any{
			"workspace_roots": []string{repoA, repoB}, "tool_input": map[string]any{"file_path": "a.go"},
		}},
		{name: "absolute file outside root", payload: map[string]any{
			"workspace_roots": []string{repoA}, "tool_input": map[string]any{"file_path": filepath.Join(repoB, "b.go")},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(test.payload)
			var input hookInput
			if err := json.Unmarshal(raw, &input); err != nil {
				t.Fatal(err)
			}
			if input.Cwd != "" {
				t.Fatalf("ambiguous workspace guessed cwd %q", input.Cwd)
			}
		})
	}
}

func TestApprovalRequestCarriesRuntimeAndCursorUnknownBudgetFailsClosed(t *testing.T) {
	if budget, verified := hookAskBudget(cursorVendor); verified || budget != 0 {
		t.Fatalf("unmeasured Cursor budget = %v verified=%t", budget, verified)
	}
	allowed, via, _ := askHuman("cursor-session", cursorVendor, "rule", "ask", "message", "command", "tags", "")
	if allowed || via != "runtime-budget-unverified-fail-closed" {
		t.Fatalf("unmeasured Cursor ask allowed=%t via=%q", allowed, via)
	}
	if budget, verified := hookAskBudget("future-runtime"); verified || budget != 0 {
		t.Fatalf("undeclared future runtime inherited budget = %v verified=%t", budget, verified)
	}
	if budget, verified := hookAskBudget(""); !verified || budget != MaxAskBudget {
		t.Fatalf("historical unattributed budget = %v verified=%t", budget, verified)
	}

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"denied"}`))
	}))
	defer server.Close()
	t.Setenv("CG_GOVERN", server.Listener.Addr().String())
	t.Setenv("CG_GOVERN_TOKEN", "token")
	pendingObserve = nil
	allowed, via, _ = askHuman("claude-session", claudeVendor, "rule", "ask", "message", "command", "tags", "")
	if allowed || via != "inbox-user-deny" {
		t.Fatalf("existing runtime ask allowed=%t via=%q", allowed, via)
	}
	if body["runtime"] != claudeVendor || body["session"] != "claude-session" {
		t.Fatalf("approval identity = %+v", body)
	}
}
