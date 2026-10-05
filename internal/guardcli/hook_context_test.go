package guardcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"crossing-guard/internal/observation"
)

func decodeHookOutput(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	inner, _ := out["hookSpecificOutput"].(map[string]any)
	if inner == nil {
		t.Fatalf("no hookSpecificOutput: %s", b)
	}
	return inner
}

// The installer owns the envelope, the event set, and the cap (plan A1):
// context is encoded only where the vendor documents it, never on Stop, and
// a denial stays byte-compatible with the historical envelope.
func TestInstallerEncodersOwnContextEventsAndCaps(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		encoder, ok := hookInstallers[runtime].(HookContextEncoder)
		if !ok {
			t.Fatalf("%s publishes no context encoder", runtime)
		}
		for _, event := range []string{"PreToolUse", "PostToolUse", "UserPromptSubmit"} {
			b, ok := encoder.EncodeHookContext(event, "remember this")
			if !ok {
				t.Fatalf("%s refused context at %s", runtime, event)
			}
			inner := decodeHookOutput(t, b)
			if inner["hookEventName"] != event || inner["additionalContext"] != "remember this" {
				t.Fatalf("%s %s: %v", runtime, event, inner)
			}
			if _, present := inner["permissionDecision"]; present {
				t.Fatalf("%s %s: context must never carry a decision key", runtime, event)
			}
		}
		for _, event := range []string{"Stop", "SessionEnd", "Notification", "PreCompact", "preToolUse"} {
			if _, ok := encoder.EncodeHookContext(event, "x"); ok {
				t.Fatalf("%s encoded context at %s", runtime, event)
			}
		}
		long := strings.Repeat("界", 20000)
		b, _ := encoder.EncodeHookContext("PostToolUse", long)
		if got := decodeHookOutput(t, b)["additionalContext"].(string); len(got) > 12000 || !strings.HasSuffix(got, "[truncated by the carrier's byte cap]") || !strings.HasPrefix(got, "界") {
			t.Fatalf("%s cap: len=%d suffix=%q", runtime, len(got), got[len(got)-40:])
		}
		deny := hookInstallers[runtime].(HookDecisionEncoder).EncodeHookDeny("PreToolUse", "because")
		legacy, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "because"}})
		if !bytes.Equal(deny, legacy) {
			t.Fatalf("%s deny envelope changed: %s vs %s", runtime, deny, legacy)
		}
	}
	if _, ok := hookInstallers["cursor"].(HookContextEncoder); ok {
		t.Fatal("cursor publishes a context encoder without a documented surface")
	}
	if _, ok := hookInstallers["opencode"].(HookContextEncoder); ok {
		t.Fatal("opencode delivers through its plugin, not the hook verb")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	fn()
	writer.Close()
	os.Stdout = original
	out, _ := io.ReadAll(reader)
	return string(out)
}

// Generic hook code prints exactly what the runtime's encoder returns:
// nothing without a delivery, nothing for a runtime without an encoder, and
// the raw event name (never the canonicalized one) when it does print.
func TestPrintHookContextIsSilentWithoutDeliveryOrEncoder(t *testing.T) {
	in := hookInput{Runtime: "claude", HookEventName: "PreToolUse", RawHookEventName: "PreToolUse"}
	if out := captureStdout(t, func() { printHookContext(in, nil) }); out != "" {
		t.Fatalf("printed without delivery: %q", out)
	}
	deliveries := []observation.Delivery{{DeliveryID: "odel_1", Message: "first"}, {DeliveryID: "odel_2", Message: "second"}}
	out := captureStdout(t, func() { printHookContext(in, deliveries) })
	inner := decodeHookOutput(t, []byte(strings.TrimSpace(out)))
	if inner["additionalContext"] != "first\n\nsecond" || inner["hookEventName"] != "PreToolUse" {
		t.Fatalf("%v", inner)
	}
	cursor := hookInput{Runtime: "cursor", HookEventName: "PreToolUse", RawHookEventName: "preToolUse"}
	if out := captureStdout(t, func() { printHookContext(cursor, deliveries) }); out != "" {
		t.Fatalf("runtime without encoder printed: %q", out)
	}
	if out := captureStdout(t, func() { printCollectionDeliveries(nil) }); out != "" {
		t.Fatalf("collection channel printed without delivery: %q", out)
	}
	out = captureStdout(t, func() { printCollectionDeliveries(deliveries) })
	var line map[string][]observation.Delivery
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &line); err != nil || len(line["crossing_guard_deliveries"]) != 2 {
		t.Fatalf("%v %q", err, out)
	}
}

// With enforcement stood down, a call a rule would block is an allow boundary:
// the daemon may hand it the session's pending helper messages and record them
// delivered once its reply is written. The hook prints them exactly as
// exitAllow does, and an enforcing deny still prints only its denial
// (enforcement-off-carrier-delivery plan).
func TestWouldBlockAllowPrintsTheHelperMessagesItCarried(t *testing.T) {
	var mu sync.Mutex
	var received []observation.Envelope
	var carried []observation.Delivery
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e observation.Envelope
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Errorf("fake daemon could not decode the envelope: %v", err)
		}
		mu.Lock()
		received = append(received, e)
		reply := carried
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(observation.Receipt{Schema: observation.SchemaV1,
			ObservationID: e.ObservationID, EventID: 1, Deliveries: reply})
	}))
	defer server.Close()
	const reason = "Blocked by rule deny-alpha (Restricted, non-overridable): alpha is restricted."
	deny := func(t *testing.T, enforcing bool, deliveries []observation.Delivery) (string, observation.Envelope) {
		t.Helper()
		governTestHome(t, governTestRules)
		t.Setenv("CG_GOVERN", strings.TrimPrefix(server.URL, "http://")) // after the sandbox, which blanks it
		t.Setenv("CG_GOVERN_TOKEN", "")
		t.Setenv("CG_OBSERVE", "1")
		if !enforcing {
			if err := os.MkdirAll(filepath.Dir(enforcementFile()), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(enforcementFile(), []byte("maintenance window"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		mu.Lock()
		received, carried = nil, deliveries
		mu.Unlock()
		in := governPreToolCall("run alpha now")
		in.Runtime, in.RawHookEventName, in.Carrier = "claude", "PreToolUse", true
		observeAttempt(in)
		stageRule("deny-alpha")
		out := captureStdout(t, func() { emitDeny(reason) })
		// An empty spool proves the daemon acknowledged the observation, so the
		// receipt's deliveries really reached the hook.
		entries, err := os.ReadDir(observationSpoolDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".") {
				t.Fatalf("observation left in the spool, so no receipt was read: %s", entry.Name())
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(received) != 1 {
			t.Fatalf("want one observation sent, got %d", len(received))
		}
		return out, received[0]
	}

	t.Run("enforcement off prints the carried messages as context", func(t *testing.T) {
		out, sent := deny(t, false, []observation.Delivery{{DeliveryID: "odel_1", Message: "first"}, {DeliveryID: "odel_2", Message: "second"}})
		if sent.Decision != "allow" || !sent.Carrier || sent.Rule != "deny-alpha" || !strings.HasPrefix(sent.Reason, "WOULD BLOCK ("+reason+")") {
			t.Fatalf("would-block observation: decision=%q carrier=%v rule=%q reason=%q", sent.Decision, sent.Carrier, sent.Rule, sent.Reason)
		}
		if strings.Count(out, "\n") != 1 {
			t.Fatalf("want exactly one envelope line: %q", out)
		}
		inner := decodeHookOutput(t, []byte(strings.TrimSpace(out)))
		if inner["additionalContext"] != "first\n\nsecond" || inner["hookEventName"] != "PreToolUse" {
			t.Fatalf("context envelope: %v", inner)
		}
		for _, key := range []string{"permissionDecision", "permissionDecisionReason"} {
			if _, present := inner[key]; present {
				t.Fatalf("a would-block allow must never carry %s: %v", key, inner)
			}
		}
	})
	t.Run("enforcement off without messages prints nothing", func(t *testing.T) {
		if out, sent := deny(t, false, nil); out != "" || sent.Decision != "allow" {
			t.Fatalf("printed %q for decision %q", out, sent.Decision)
		}
	})
	t.Run("enforcing deny prints only its denial", func(t *testing.T) {
		out, sent := deny(t, true, []observation.Delivery{{DeliveryID: "odel_1", Message: "first"}})
		want := string(hookInstallers["claude"].(HookDecisionEncoder).EncodeHookDeny("PreToolUse", reason)) + "\n"
		if sent.Decision != "deny" || out != want {
			t.Fatalf("decision=%q output %q, want %q", sent.Decision, out, want)
		}
	})
}

// The governed lane declares itself a carrier only where its installer can
// print context; the collection lane only when its plugin asks (--carrier).
func TestHookCarrierDeclaration(t *testing.T) {
	if !hookCanCarry(hookInput{Runtime: "claude", RawHookEventName: "PreToolUse"}) || hookCanCarry(hookInput{Runtime: "claude", RawHookEventName: "Stop"}) {
		t.Fatal("claude carrier declaration wrong")
	}
	if hookCanCarry(hookInput{Runtime: "cursor", RawHookEventName: "preToolUse"}) || hookCanCarry(hookInput{Runtime: "", HookEventName: "PreToolUse"}) {
		t.Fatal("a runtime without an encoder must not declare itself a carrier")
	}
	if !hasFlag([]string{"collect-hook", "--runtime", "opencode", "--carrier"}, "--carrier") || hasFlag([]string{"--runtime", "opencode"}, "--carrier") {
		t.Fatal("flag detection")
	}
	out, err := json.Marshal(GovernDecision{Decision: "allow", Deliveries: []observation.Delivery{{DeliveryID: "odel_1", Message: "m"}}})
	if err != nil || !strings.Contains(string(out), `"crossing_guard_deliveries":[{"delivery_id":"odel_1","message":"m"}]`) {
		t.Fatalf("decision wire shape: %s %v", out, err)
	}
	if out, _ := json.Marshal(GovernDecision{Decision: "deny"}); strings.Contains(string(out), "crossing_guard_deliveries") {
		t.Fatal("a decision without deliveries must not carry the key")
	}
}

// claudeHookPayloadCases reads raw hook stdin captured from Claude Code
// 2.1.280 (subagent-carrier-boundary-verification.md §1): main thread,
// foreground and background sub-agents, and a `--agent` main thread.
func claudeHookPayloadCases(t *testing.T) []struct {
	Name    string          `json:"name"`
	Carries bool            `json:"carries"`
	Payload json.RawMessage `json:"payload"`
} {
	t.Helper()
	raw, err := os.ReadFile("testdata/claude_2_1_280_hook_payloads.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name    string          `json:"name"`
			Carries bool            `json:"carries"`
			Payload json.RawMessage `json:"payload"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Cases) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	return fixture.Cases
}

// A hook that fires inside a sub-agent names the parent session but prints
// into the sub-agent's own conversation, so it never declares itself a
// carrier; every main-thread hook, including a `--agent` main thread (which
// sets agent_type but not agent_id), carries exactly as before. The payloads
// go through the real decoder: a tag on hookInput alone is never read.
func TestSubagentHookBoundariesNeverCarry(t *testing.T) {
	for _, c := range claudeHookPayloadCases(t) {
		var in hookInput
		if err := json.NewDecoder(bytes.NewReader(c.Payload)).Decode(&in); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		in.Runtime = "claude"
		if got := hookCanCarry(in); got != c.Carries {
			t.Fatalf("%s: carrier %v, want %v (agent_id %q)", c.Name, got, c.Carries, in.AgentID)
		}
		if nested := !c.Carries; nested != (in.AgentID != "") {
			t.Fatalf("%s: decoded agent_id %q", c.Name, in.AgentID)
		}
	}
	// Only a JSON string is an agent id. Another shape must not fail the whole
	// payload (which would let every call through unjudged); it is ignored.
	for _, odd := range []string{`7`, `{"id":"a1"}`, `null`, `["a1"]`} {
		var in hookInput
		payload := `{"hook_event_name":"PreToolUse","session_id":"s1","tool_name":"Bash","tool_input":{"command":"ls"},"agent_id":` + odd + `}`
		if err := json.Unmarshal([]byte(payload), &in); err != nil {
			t.Fatalf("agent_id %s failed the payload: %v", odd, err)
		}
		if in.AgentID != "" || in.ToolName != "Bash" {
			t.Fatalf("agent_id %s decoded as %q", odd, in.AgentID)
		}
	}
	// A runtime without the port is never treated as nested: the field alone
	// changes nothing. Codex has the port since the dated probe of 2026-10-03
	// (TestCodexChildPromptNeverCarries); Cursor has none.
	for name, installer := range hookInstallers {
		if _, reports := installer.(HookSubagentReporter); reports {
			continue
		}
		encoder, ok := installer.(HookContextEncoder)
		if !ok {
			continue
		}
		for _, event := range []string{"PreToolUse", "UserPromptSubmit"} {
			if _, carries := encoder.EncodeHookContext(event, ""); carries && !hookCanCarry(hookInput{Runtime: name, RawHookEventName: event, AgentID: "a1"}) {
				t.Fatalf("%s has no HookSubagentReporter and must ignore agent_id", name)
			}
		}
	}
}

// codexPromptPayload reads one raw UserPromptSubmit payload the 2026-10-03 probe
// recorded from Codex 0.159.2 (run xp-child: a parent that spawned one child
// thread), exactly as the hook received it on stdin.
func codexPromptPayload(t *testing.T, who string) hookInput {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex_0_159_2_user_prompt_" + who + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var in hookInput
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&in); err != nil {
		t.Fatalf("%s: %v", who, err)
	}
	in.Runtime = codexVendor
	in.Carrier = hookCanCarry(in)
	return in
}

// K-1: a Codex child thread fires the prompt event under the PARENT's session
// id, told apart only by agent_id. The hook must report it as unable to carry,
// so the daemon hands it nothing — neither a handoff's brief nor a helper
// message — while the parent's own prompt carries as before. The envelope the
// hook would post is checked, not only the predicate.
func TestCodexChildPromptNeverCarries(t *testing.T) {
	parent, child := codexPromptPayload(t, "parent"), codexPromptPayload(t, "child")
	if parent.SessionID == "" || parent.SessionID != child.SessionID {
		t.Fatalf("the fixture is a child under its parent's session id: %q %q", parent.SessionID, child.SessionID)
	}
	if parent.AgentID != "" || child.AgentID == "" {
		t.Fatalf("only the child carries agent_id: parent %q child %q", parent.AgentID, child.AgentID)
	}
	if !parent.Carrier {
		t.Fatal("the parent's own prompt carries")
	}
	if child.Carrier {
		t.Fatal("a Codex child prompt under the parent's session id declares itself a carrier: it would be handed the parent's pending rows")
	}
	for who, in := range map[string]hookInput{"parent": parent, "child": child} {
		turn, err := buildSessionTurnEnvelope(in, "turn.started")
		if err != nil {
			t.Fatal(err)
		}
		if turn.SessionID != parent.SessionID || turn.Carrier != (who == "parent") {
			t.Fatalf("%s: the posted envelope names the parent's session and carrier=%v", who, turn.Carrier)
		}
	}
	if _, ok := hookInstallers[codexVendor].(HookSubagentReporter); !ok {
		t.Fatal("codex must report a nested call")
	}
}

// §6.5: each installer publishes the kinds at which it can tell a nested call.
// On Codex that is the prompt kind only — whether a child's tool events carry
// agent_id is unmeasured — so no Codex tool event may carry a handoff's brief.
func TestNestedCallKindsArePublishedPerRuntime(t *testing.T) {
	if got := NestedCallKinds(codexVendor); len(got) != 1 || got[0] != "turn.started" {
		t.Fatalf("codex tells a nested call at the prompt kind only: %v", got)
	}
	claude := NestedCallKinds(claudeVendor)
	for _, kind := range []string{"turn.started", "tool.started", "tool.completed"} {
		found := false
		for _, got := range claude {
			found = found || got == kind
		}
		if !found {
			t.Fatalf("claude tells a nested call at %s: %v", kind, claude)
		}
	}
	for name := range hookInstallers {
		if _, reports := hookInstallers[name].(HookSubagentReporter); !reports && len(NestedCallKinds(name)) != 0 {
			t.Fatalf("%s reports no sub-agents and publishes kinds", name)
		}
	}
	if got := NestedCallKinds("no-such-runtime"); len(got) != 0 {
		t.Fatalf("an unknown runtime publishes none: %v", got)
	}
}

// The encoder caps are the helper delivery path's and are not changed by the
// handoff work (team rest-of-release plan §6.5); they are published so the
// owner of handoff.inject_max_bytes validates against them, not a copy.
func TestHookContextCapsArePublishedUnchanged(t *testing.T) {
	caps := HookContextCaps()
	if caps[claudeVendor] != 9000 || caps[codexVendor] != 7000 || len(caps) != 2 {
		t.Fatalf("encoder caps: %v", caps)
	}
	if HookContextJoin != "\n\n" {
		t.Fatalf("the join between carried messages: %q", HookContextJoin)
	}
}

// Only the carrier declaration changes inside a sub-agent: the observation,
// result and session-turn envelopes a sub-agent hook sends are otherwise the
// same evidence it sent before. The deny envelope cannot depend on it (its
// encoder never sees the payload; AgentID's only reader is hookCanCarry); the
// byte check below only keeps that envelope pinned.
func TestSubagentHookChangesOnlyTheCarrierDeclaration(t *testing.T) {
	for _, c := range claudeHookPayloadCases(t) {
		if c.Carries {
			continue
		}
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(c.Payload, &generic); err != nil {
			t.Fatal(err)
		}
		// Both sides go through the same re-encoding so the retained tool
		// input bytes compare equal; only agent_id differs between them.
		withAgent, _ := json.Marshal(generic)
		delete(generic, "agent_id")
		stripped, _ := json.Marshal(generic)
		decode := func(b []byte) hookInput {
			var in hookInput
			if err := json.Unmarshal(b, &in); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			in.Runtime = "claude"
			in.Carrier = hookCanCarry(in)
			return in
		}
		nested, main := decode(withAgent), decode(stripped)
		if nested.Carrier || !main.Carrier {
			t.Fatalf("%s: carrier nested=%v main=%v", c.Name, nested.Carrier, main.Carrier)
		}
		for _, decision := range []string{"allow", "deny"} {
			a, errA := buildObservationEnvelope(nested, decision, "r")
			b, errB := buildObservationEnvelope(main, decision, "r")
			if errA != nil || errB != nil || a.Carrier || !b.Carrier {
				t.Fatalf("%s %s: %v %v carrier %v/%v", c.Name, decision, errA, errB, a.Carrier, b.Carrier)
			}
			a.ObservationID, a.TS, a.QueuedAt, a.Carrier = "", 0, 0, false
			b.ObservationID, b.TS, b.QueuedAt, b.Carrier = "", 0, 0, false
			da, _ := a.Digest()
			db, _ := b.Digest()
			if da != db {
				t.Fatalf("%s %s: sub-agent evidence differs beyond the carrier declaration", c.Name, decision)
			}
		}
		if nested.HookEventName == "PostToolUse" {
			a, errA := buildResultEnvelope(nested)
			b, errB := buildResultEnvelope(main)
			if errA != nil || errB != nil || a.Carrier || !b.Carrier {
				t.Fatalf("%s result: %v %v carrier %v/%v", c.Name, errA, errB, a.Carrier, b.Carrier)
			}
		}
		turn, err := buildSessionTurnEnvelope(nested, "turn.started")
		if err != nil || turn.Carrier {
			t.Fatalf("%s turn: %v carrier %v", c.Name, err, turn.Carrier)
		}
	}
	deny := (claudeInstaller{}).EncodeHookDeny("PreToolUse", "because")
	legacy, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "because"}})
	if !bytes.Equal(deny, legacy) {
		t.Fatalf("deny envelope changed: %s", deny)
	}
}
