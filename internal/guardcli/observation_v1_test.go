package guardcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/observation"
)

func TestHookInputRetainsUnknownToolInputAndBuildsExactClaims(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	payload := fmt.Sprintf(`{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Edit","cwd":%q,"tool_use_id":"call-1","tool_input":{"file_path":"src/a.go","new_string":"x","future":{"nested":true}}}`, repo)
	var in hookInput
	if err := json.Unmarshal([]byte(payload), &in); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(in.RawToolInput) || string(in.RawToolInput) != `{"file_path":"src/a.go","new_string":"x","future":{"nested":true}}` {
		t.Fatalf("raw tool input=%s", in.RawToolInput)
	}
	in.Runtime = "claude"
	in.ActionID = "act_0123456789abcdef0123456789abcdef"
	e, err := buildObservationEnvelope(in, "allow", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.ActionID != in.ActionID || e.NativeCallID != "call-1" || e.ToolInputCompleteness != "complete" || len(e.ResourceClaims) != 1 {
		t.Fatalf("envelope=%+v", e)
	}
	c := e.ResourceClaims[0]
	if c.RawIdentity != "src/a.go" || c.Identity != filepath.Join(repo, "src", "a.go") ||
		c.SourceField != "tool_input.file_path" || c.Operation != "write" {
		t.Fatalf("claim=%+v", c)
	}
}

func TestOneActionIdentitySpansAskAndResolutionObservations(t *testing.T) {
	pendingObserve = nil
	t.Cleanup(func() { pendingObserve = nil })
	observeAttempt(hookInput{SessionID: "s", Runtime: "claude", ToolName: "Bash",
		RawToolInput: json.RawMessage(`{"command":"inspect workspace"}`)})
	if pendingObserve == nil || len(pendingObserve.ActionID) != 36 || !strings.HasPrefix(pendingObserve.ActionID, "act_") {
		t.Fatalf("pending action=%+v", pendingObserve)
	}
	held, err := buildObservationEnvelope(*pendingObserve, "ask", "held")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := buildObservationEnvelope(*pendingObserve, "allow", "confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if held.ActionID != resolved.ActionID || held.ObservationID == resolved.ObservationID {
		t.Fatalf("held=%+v resolved=%+v", held, resolved)
	}
	firstDigest, _ := held.Digest()
	held.ActionID = "act_ffffffffffffffffffffffffffffffff"
	changedDigest, _ := held.Digest()
	if firstDigest == changedDigest {
		t.Fatal("action identity was excluded from observation digest")
	}
}

func TestApplyPatchClaimsPreserveOperationAndSourceOrder(t *testing.T) {
	var in hookInput
	in.SessionID, in.Runtime, in.ToolName = "s", "codex", "apply_patch"
	in.Cwd = filepath.Join(string(filepath.Separator), "repo")
	in.ToolInput.Command = commandField("*** Begin Patch\n*** Update File: a.go\n*** Delete File: old.go\n*** Add File: new.go\n*** End Patch")
	in.RawToolInput = json.RawMessage(`{"command":"patch"}`)
	e, err := buildObservationEnvelope(in, "allow", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.ResourceClaims) != 3 {
		t.Fatalf("claims=%+v", e.ResourceClaims)
	}
	for i, want := range []string{"tool_input.command.*** Update File", "tool_input.command.*** Delete File", "tool_input.command.*** Add File"} {
		if e.ResourceClaims[i].Ordinal != i || e.ResourceClaims[i].SourceField != want || e.ResourceClaims[i].Operation != "patch" {
			t.Fatalf("claim[%d]=%+v", i, e.ResourceClaims[i])
		}
	}
}

func TestObservationSpoolIsOwnerOnlyAndReplayable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	e := observation.Envelope{Schema: observation.SchemaV1, ObservationID: "obs_0123456789abcdef0123456789abcdef",
		CollectorID: observation.CollectorPreTool, SessionID: "s", Tool: "Read",
		ToolInputCompleteness: "unavailable", QueuedAt: 1, DeliveryAttempts: 1, DeliveryMode: "direct"}
	path, err := spoolObservation(e)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spool mode=%o", info.Mode().Perm())
	}
	entries, err := pendingObservations()
	if err != nil || len(entries) != 1 || entries[0] != path {
		t.Fatalf("pending=%v err=%v", entries, err)
	}
	got, err := readSpooledObservation(path)
	if err != nil || got.ObservationID != e.ObservationID {
		t.Fatalf("read=%+v err=%v", got, err)
	}
	e.Tool = "Write"
	if _, err := spoolObservation(e); err == nil {
		t.Fatal("observation id collision overwrote queued evidence")
	}
	got, err = readSpooledObservation(path)
	if err != nil || got.Tool != "Read" {
		t.Fatalf("queued evidence was overwritten: %+v err=%v", got, err)
	}
}

func TestOversizeToolInputKeepsMetadataWithoutInvalidJSONPrefix(t *testing.T) {
	var in hookInput
	in.SessionID, in.Runtime, in.ToolName = "s", "claude", "future"
	in.RawToolInput = json.RawMessage(`{"blob":"` + strings.Repeat("x", observation.MaxRetainedInput) + `"}`)
	e, err := buildObservationEnvelope(in, "allow", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.ToolInputCompleteness != "metadata-only" || e.ToolInput != nil || e.ToolInputBytes != len(in.RawToolInput) || e.ToolInputDigest == "" {
		t.Fatalf("oversize envelope=%+v", e)
	}
}

func TestObservationSpoolCapacityRefusesWithoutDeletingEvidence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pending-crash")
	if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkObservationSpoolCapacity(dir, 1, 1, 1024); err == nil {
		t.Fatal("full file-count capacity accepted")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "evidence" {
		t.Fatalf("existing evidence changed: %q err=%v", got, err)
	}
}

func TestDeclaredPathPreservesWhitespaceAndRefusesNULResolution(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	spaced := declaredClaim(hookInput{Cwd: repo}, "file", " spaced.go ", "read", "tool_input.file_path")
	if spaced.RawIdentity != " spaced.go " || spaced.Identity != filepath.Join(repo, " spaced.go ") || spaced.Completeness != "complete" {
		t.Fatalf("spaced claim=%+v", spaced)
	}
	nul := declaredClaim(hookInput{Cwd: repo}, "file", "bad\x00.go", "read", "tool_input.file_path")
	if nul.RawIdentity != "bad\x00.go" || nul.Identity != "" || nul.Completeness != "unresolved" || nul.Resolution != "nul-byte" {
		t.Fatalf("NUL claim=%+v", nul)
	}
}

func TestCommonCodingToolResourceContracts(t *testing.T) {
	repo := filepath.Join(string(filepath.Separator), "repo")
	cases := []struct {
		tool, raw, kind, operation, source string
	}{
		{"Read", "a.go", "file", "read", "tool_input.file_path"},
		{"Edit", "a.go", "file", "write", "tool_input.file_path"},
		{"Write", "a.go", "file", "write", "tool_input.file_path"},
		{"NotebookEdit", "n.ipynb", "file", "write", "tool_input.notebook_path"},
		{"Grep", "src", "path", "search", "tool_input.path"},
		{"Glob", "src", "path", "search", "tool_input.path"},
		{"WebFetch", "https://example.invalid/x", "url", "connect", "tool_input.url"},
		{"Skill", "review-php", "skill", "use", "tool_input.skill"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			in := hookInput{SessionID: "s", ToolName: tc.tool, Cwd: repo}
			switch tc.source {
			case "tool_input.file_path":
				in.ToolInput.FilePath = tc.raw
			case "tool_input.notebook_path":
				in.ToolInput.NotebookPath = tc.raw
			case "tool_input.path":
				in.ToolInput.Path = tc.raw
			case "tool_input.url":
				in.ToolInput.URL = tc.raw
			case "tool_input.skill":
				in.ToolInput.Skill = tc.raw
			}
			claims := structuredResourceClaims(in)
			if len(claims) != 1 || claims[0].Kind != tc.kind || claims[0].Operation != tc.operation || claims[0].SourceField != tc.source {
				t.Fatalf("claims=%+v", claims)
			}
		})
	}
	shell := hookInput{ToolName: "Bash", Cwd: repo, ToolInput: toolInput{Command: "sed -n '1p' a.go"},
		RawToolInput: json.RawMessage(`{"command":"sed -n '1p' a.go"}`)}
	if claims := structuredResourceClaims(shell); len(claims) != 1 || claims[0].RawIdentity != "a.go" || claims[0].Operation != "read" {
		t.Fatalf("shell command claims=%+v", claims)
	}
}
