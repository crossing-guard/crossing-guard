// Package guardcli is the enforcement CLI surface (the former cp experiment,
// folded at M4 slice B): the hook shim, dry-run check, engine tags, and hook
// installation.
//
//	install [--settings PATH]   write supported lifecycle hooks into vendor settings
//	hook                        phase-aware shim: decide before tools, collect afterward
//	check "<command>"           dry-run the rules against a command (no agent)
//	tags  '<event-json>'        classify an event via the shared engine
//
// Rules load from rules.json next to the binary ($CG_RULES overrides).
// Honest limits (unchanged from the gitguard PoC): matching is a best-effort
// regex on the Bash command string — a hard guarantee needs a non-bypassable
// backstop (e.g. remote branch protection) behind it.
package guardcli

import (
	"bytes"
	"context"
	"crossing-guard/internal/observation"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/approvalchoice"
	"crossing-guard/internal/detectorselection"
	"crossing-guard/internal/rulebook"
)

// MaxApprovalResponseBytes bounds a decoded approval wait result.
const MaxApprovalResponseBytes = 64 << 10

const usage = `usage:
  install [--settings PATH]   write supported lifecycle hooks into vendor settings
  hook                        phase-aware shim (decides only on PreToolUse)
  govern-hook                 headless decision shim: evaluate one tool call, answer JSON
  collect-hook                collection-only shim (records, never governs)
  check "<command>"           dry-run the rules against a command
  tags  '<event-json>'        classify an event → tags + water mark + decision
  coverage                    the compiled honest label per rule (P-COMPILE-2)
  entities [--kind K] [--id X]  governed resources + what we believe and why
  enforce [status|off|on]     stand enforcement down without uninstalling
  chain verify <vendor/id>    recompute a session's event chain; tail checked by the daemon`

// The former format-2 rule types (guard/rule/ruleset with regex `Guards`) are gone.
// Rules are now engine.Policy in ONE format, evaluated by the ONE evaluator
// engine.Decide (ADR 0025). The command-guard layer feeds the raw command in as the
// eval-only `command` tag so a regex rule has something to match.

// detectorsOverlayPath resolves the USER detector overlay: $CG_DETECTORS, else
// the cwd-independent ~/.crossing-guard/policy/detectors.json. It is only an
// OVERLAY — engine.LoadLayered merges it onto the embedded standard library, and
// an absent file is not an error (the shipped default is used). Same absolute-
// fallback discipline as rulebook.Path (hook and daemon run in different cwds).
func detectorsOverlayPath() string {
	if p := os.Getenv("CG_DETECTORS"); p != "" {
		return p
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".crossing-guard", "policy", "detectors.json")
	}
	return "detectors.json"
}

// activeDetectorDocument is the enforcement CLI's one typed detector-resolution
// entry point. A local policy path is compatibility discovery, not an invocation
// selection; only CG_DETECTORS displaces durable state for this process.
func activeDetectorDocument() (*detectorselection.LoadedDetectors, error) {
	return detectorselection.ResolveDetectors(dataDir(), detectorselection.DetectorSurfaceCLI, os.Getenv("CG_DETECTORS"))
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// dataDir is the product data directory (~/.crossing-guard) — the one place the
// daemon and the hook rendezvous without either hardcoding the other's port.
func dataDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".crossing-guard")
	}
	return ".crossing-guard"
}

// DaemonAddrFile is where `crossing-guard serve` publishes its listen address so the hook can
// find it on ANY port. Hardcoding a default port here would silently break every
// non-default daemon — the same class of bug as an unowned manual step.
func DaemonAddrFile() string { return filepath.Join(dataDir(), "daemon-addr") }

func readTrimmed(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// daemonEndpoint resolves the one daemon's address and token — the SINGLE discovery
// path for every hook→daemon call (observe, the stateful consult, the approvals
// inbox). ON BY DEFAULT: the daemon publishes both on start (its address to
// daemon-addr, its token to api-token; internal/daemon/main.go), so the hook finds
// it with no env var, on any port. Env CG_GOVERN / CG_GOVERN_TOKEN override the files
// for non-standard deploys. ok=false means no daemon has announced itself — nothing
// to send to. This is PURE discovery: it does not consult CG_OBSERVE (an observation
// opt-out that must not gate enforcement) — that check lives at the observe call site.
func daemonEndpoint() (addr, token string, ok bool) {
	addr = os.Getenv("CG_GOVERN")
	if addr == "" {
		addr = readTrimmed(DaemonAddrFile())
	}
	if addr == "" {
		return "", "", false // no daemon has ever announced itself — nothing to send to
	}
	token = os.Getenv("CG_GOVERN_TOKEN")
	if token == "" {
		token = readTrimmed(filepath.Join(dataDir(), "api-token"))
	}
	return addr, token, true
}

func daemonEndpointFromDataDir(dataRoot string) (addr, token string, ok bool) {
	dataRoot = strings.TrimSpace(dataRoot)
	if dataRoot == "" {
		return "", "", false
	}
	addr = readTrimmed(filepath.Join(dataRoot, "daemon-addr"))
	if addr == "" {
		return "", "", false
	}
	token = readTrimmed(filepath.Join(dataRoot, "api-token"))
	return addr, token, true
}

// Verdict is one dry-run decision for a shell command.
type Verdict struct {
	Decision string // allow | ask | deny
	Rule     string
	Guard    string
}

// staticInvocationTags projects the exact invocation facts the standalone rulebook
// can govern without a daemon. Tool identity is omitted when a command-only caller
// does not have one; no runtime or tool is guessed.
func staticInvocationTags(tool, command string) []engine.Tag {
	tags := []engine.Tag{{Key: engine.CommandTagKey, Value: command}}
	if tool = engine.BareTool(strings.TrimSpace(tool)); tool != "" {
		tags = append(tags, engine.Tag{Key: engine.ToolTagKey, Value: tool})
	}
	return tags
}

// verdictOf maps an engine decision over standalone invocation rules onto the CLI verdict.
// deny (HardBlock) → deny; ask (ConfirmAndRecord) → ask; anything that proceeds →
// allow. This is the SAME engine.Decide the hook runs — one evaluator, one answer.
func verdictOf(d engine.Decision) Verdict {
	switch d.Mode {
	case engine.HardBlock:
		return Verdict{Decision: "deny", Rule: d.Rule, Guard: d.Rule}
	case engine.ConfirmAndRecord:
		return Verdict{Decision: "ask", Rule: d.Rule, Guard: d.Rule}
	default:
		return Verdict{Decision: "allow"}
	}
}

// CheckCommand dry-runs the active rules against a command via engine.Decide — the
// same evaluator cmdCheck and the command subset of the hook's guard layer use.
func CheckCommand(command string) (Verdict, error) {
	pol, err := rulebook.Load()
	if err != nil {
		return Verdict{}, err
	}
	return verdictOf(engine.Decide(staticInvocationTags("", command), pol)), nil
}

func logLine(entry map[string]any) {
	path := os.Getenv("CG_LOG")
	if path == "" {
		// the accepted relocation ask: decisions survive reboot; the daemon's
		// decisions view already prefers this path over the old /tmp/cp.log
		if h, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(h, ".crossing-guard", "policy", "decisions.jsonl")
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
		} else {
			path = "/tmp/cp.log"
		}
	}
	entry["ts"] = time.Now().Format(time.RFC3339)
	b, _ := json.Marshal(entry)
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		defer f.Close()
		_, _ = f.Write(append(b, '\n'))
	}
}

// observeAsk records "we held this call for a human" BEFORE blocking on one.
//
// Staging the observation until the decision was known (the D19 fix) opened a hole
// on exactly the path that matters most: the runtime SIGKILLs the hook at
// RuntimeHookTimeout while the hook is still held on the approvals inbox, so nothing
// was ever flushed and the action vanished from the log — worse than the empty
// decision it replaced. An `ask` IS a decision at this instant, so record it now.
//
// The action stays staged: the human's resolution is a SECOND governance event.
// That is not double-counting — "held for a human" and "human allowed it" are two
// distinct facts, and the append-only log should carry both.
func observeAsk(reason string) {
	if pendingObserve != nil {
		emitObserve(*pendingObserve, "ask", reason)
	}
}

// askHuman routes a held call to the daemon's approvals inbox and blocks until a
// human decides or the budget lapses. The inbox is the ONE ask channel: the daemon
// is service-managed (ADR 0018), so with it up every ask reaches the console. If no
// daemon is reachable there is nothing to ask — so we FAIL CLOSED (deny, honestly
// labelled) rather than silently allow a guarded action. Returns (allowed, mode,
// reason): mode distinguishes a user decision from each fail-closed cause; reason
// carries the typed override reason when the inbox provided one.
func askHuman(session, runtime, ruleID, mode, message, command, tagSum, boundary string) (bool, string, string) {
	budget, verified := hookAskBudget(runtime)
	if !verified {
		fmt.Fprintf(os.Stderr, "[crossing-guard] %s approval timing is unverified; failing closed (deny)\n", runtime)
		return false, "runtime-budget-unverified-fail-closed", ""
	}
	// Record the hold BEFORE we block: past this line the runtime may kill us.
	observeAsk("held for human: rule " + ruleID)
	return askViaInbox(session, runtime, ruleID, mode, message, command, tagSum, boundary, budget)
}

func hookAskBudget(runtime string) (time.Duration, bool) {
	if runtime == "" {
		return MaxAskBudget, true // compatibility for hooks installed before attribution
	}
	if reporter, ok := hookInstallers[runtime].(HookAskBudgetReporter); ok {
		budget := reporter.HookAskBudget()
		return budget, budget > 0
	}
	return 0, false
}

// The hook's fail-closed budget contract. The load-bearing invariant is
// MaxAskBudget < RuntimeHookTimeout: past the runtime's own hook timeout the
// runtime kills the hook process and FAILS OPEN, so our fail-CLOSED deadline
// must land first. These live here (the hook side) and are imported by the
// daemon's approvals inbox, so both halves share ONE source of truth — an
// earlier copy in each package let the ceiling drift silently. The invariant
// is pinned by TestAskBudgetBeatsRuntimeTimeout, not just this comment.
const (
	RuntimeHookTimeout = 60 * time.Second // runtimes kill the hook here → fail OPEN
	MaxAskBudget       = 55 * time.Second // our fail-CLOSED must beat that
	AskClientSlack     = 5 * time.Second  // HTTP client waits a little past the budget
)

// RuntimeApprovalRequest is the provider-neutral daemon request used by a
// runtime callback bridge. It deliberately carries no provider transport or
// raw executable configuration.
type RuntimeApprovalRequest struct {
	Runtime          string
	TaskID           string
	CatalogSessionID string
	NativeSessionID  string
	ToolCallID       string
	ToolName         string
	Summary          string
	// Prompts are the questions the held call is asking its approver, if any.
	// They are a bounded projection for display and validation; the exact tool
	// input never leaves the bridge.
	Prompts []approvalchoice.ChoicePrompt
	Timeout time.Duration
}

type RuntimeApprovalResult struct {
	Decision string
	Reason   string
	// Selections is the approver's answer to Prompts. Present only on an
	// allowed decision for a call that carried prompts.
	Selections []approvalchoice.ChoiceSelection
	// PromptsCompleteness says what the inbox did with the prompts that were
	// sent: "complete" carried them, "truncated" dropped them for exceeding the
	// operator's ceilings, empty means none were sent. A bridge that sent
	// prompts and reads "truncated" knows the approver never saw the options.
	PromptsCompleteness string
}

type approvalWireRequest struct {
	Origin           string                        `json:"origin,omitempty"`
	Session          string                        `json:"session,omitempty"`
	Runtime          string                        `json:"runtime,omitempty"`
	TaskID           string                        `json:"task_id,omitempty"`
	CatalogSessionID string                        `json:"catalog_session_id,omitempty"`
	NativeSessionID  string                        `json:"native_session_id,omitempty"`
	ToolCallID       string                        `json:"tool_call_id,omitempty"`
	ToolName         string                        `json:"tool_name,omitempty"`
	Rule             string                        `json:"rule,omitempty"`
	Mode             string                        `json:"mode,omitempty"`
	Message          string                        `json:"message,omitempty"`
	Command          string                        `json:"command,omitempty"`
	Summary          string                        `json:"summary,omitempty"`
	Prompts          []approvalchoice.ChoicePrompt `json:"prompts,omitempty"`
	FiredTags        []string                      `json:"fired_tags,omitempty"`
	Boundary         string                        `json:"boundary,omitempty"`
	TimeoutMS        int                           `json:"timeout_ms"`
}

func requestApproval(ctx context.Context, in approvalWireRequest, timeout time.Duration,
	endpoint func() (string, string, bool)) (RuntimeApprovalResult, error) {
	addr, token, ok := endpoint()
	if !ok {
		return RuntimeApprovalResult{}, errors.New("approval daemon unavailable")
	}
	body, err := json.Marshal(in)
	if err != nil {
		return RuntimeApprovalResult{}, errors.New("approval request encoding failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+"/api/v1/approvals/request", bytes.NewReader(body))
	if err != nil {
		return RuntimeApprovalResult{}, errors.New("approval request creation failed")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-CG-Token", token)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return RuntimeApprovalResult{}, fmt.Errorf("approval request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return RuntimeApprovalResult{}, fmt.Errorf("approval daemon refused request: HTTP %d", resp.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxApprovalResponseBytes+1))
	if err != nil || len(responseBody) > MaxApprovalResponseBytes {
		return RuntimeApprovalResult{}, errors.New("approval daemon returned an invalid response")
	}
	var out RuntimeApprovalResult
	if err := json.Unmarshal(responseBody, &out); err != nil {
		return RuntimeApprovalResult{}, errors.New("approval daemon returned an invalid response")
	}
	switch out.Decision {
	case "allowed", "denied", "expired":
		return out, nil
	default:
		return RuntimeApprovalResult{}, errors.New("approval daemon returned an unknown decision")
	}
}

// RequestRuntimeApproval uses the same daemon rendezvous and canonical endpoint
// as hook approvals while preserving the runtime callback's own deadline.
func requestRuntimeApproval(ctx context.Context, in RuntimeApprovalRequest,
	endpoint func() (string, string, bool)) (RuntimeApprovalResult, error) {
	if in.Timeout <= 0 || in.Timeout > 10*time.Minute {
		return RuntimeApprovalResult{}, errors.New("invalid runtime approval timeout")
	}
	return requestApproval(ctx, approvalWireRequest{
		Origin: "runtime_tool", Runtime: in.Runtime, TaskID: in.TaskID,
		CatalogSessionID: in.CatalogSessionID, NativeSessionID: in.NativeSessionID,
		ToolCallID: in.ToolCallID, ToolName: in.ToolName, Summary: in.Summary,
		Prompts: in.Prompts, TimeoutMS: int(in.Timeout.Milliseconds()),
	}, in.Timeout+AskClientSlack, endpoint)
}

func RequestRuntimeApproval(ctx context.Context, in RuntimeApprovalRequest) (RuntimeApprovalResult, error) {
	return requestRuntimeApproval(ctx, in, daemonEndpoint)
}

// RequestRuntimeApprovalFromDataDir targets the exact daemon-owned data root
// carried by launch context. The path is non-secret; the bridge reads the
// protected token file itself so credentials never enter provider argv or env.
func RequestRuntimeApprovalFromDataDir(ctx context.Context, dataRoot string, in RuntimeApprovalRequest) (RuntimeApprovalResult, error) {
	return requestRuntimeApproval(ctx, in, func() (string, string, bool) {
		return daemonEndpointFromDataDir(dataRoot)
	})
}

// askViaInbox posts the held call to the daemon and blocks until a human decides or
// the budget lapses. It discovers the daemon the ONE way (daemonEndpoint: env
// override → the published daemon-addr/api-token files), so a plain install with the
// daemon up reaches the console with no per-session env. Every non-decision outcome —
// no daemon, unreachable, refused, malformed, or expired — FAILS CLOSED (deny) before
// the runtime's own fail-open timeout can fire; there is no dialog fallback.
func askViaInbox(session, runtime, ruleID, mode, message, command, tagSum, boundary string, safeBudget time.Duration) (allowed bool, via, reason string) {
	budgetMs := int(safeBudget.Milliseconds())
	if v := os.Getenv("CG_ASK_BUDGET_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			// hard cap: an override past MaxAskBudget would push our deadline
			// past the runtime's hook timeout and silently convert fail-closed
			// into fail-open (red-team finding)
			if ceilMs := int(safeBudget.Milliseconds()); n > ceilMs {
				n = ceilMs
			}
			budgetMs = n
		}
	}
	out, err := requestApproval(context.Background(), approvalWireRequest{
		Origin: "policy_hook", Session: session, Runtime: runtime, Rule: ruleID, Mode: mode,
		Message: message, Command: command, FiredTags: strings.Split(tagSum, ", "),
		Boundary: boundary, TimeoutMS: budgetMs,
	}, time.Duration(budgetMs)*time.Millisecond+AskClientSlack, daemonEndpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[crossing-guard] approvals inbox unreachable (%v); failing closed (deny)\n", err)
		return false, "inbox-unreachable-fail-closed", ""
	}
	switch out.Decision {
	case "allowed":
		return true, "inbox-user-allow", out.Reason
	case "denied":
		return false, "inbox-user-deny", out.Reason
	default: // expired — fail closed, honestly labeled
		return false, "inbox-expired-fail-closed", ""
	}
}

// emitDeny is the ONE choke point for a blocked action, so the log write lives here
// rather than at four call sites where a fifth would forget it.
//
// It is also where the enforcement switch is honored. Standing enforcement down must
// NOT create a blind spot: the action is recorded as allowed, with the reason naming
// the rule that would have fired, so "what did turning it off cost me?" stays
// answerable. Checking here rather than at each rule means a future decision path
// cannot forget the switch.
func emitDeny(reason string) {
	if off, why := enforcementDisabled(); off {
		observeDecision("allow", "WOULD BLOCK ("+reason+") — enforcement off: "+why)
		fmt.Fprintf(os.Stderr, "[crossing-guard] enforcement OFF — allowed an action that would be blocked: %s\n", reason)
		return // no hookSpecificOutput: the tool proceeds
	}
	observeDecision("deny", reason)
	// The runtime's own installer encodes the block where it publishes one;
	// the historical Claude-shaped envelope stays for runtimes that do not.
	if pendingDenyRuntime != "" {
		if encoder, ok := hookInstallers[pendingDenyRuntime].(HookDecisionEncoder); ok {
			fmt.Println(string(encoder.EncodeHookDeny(pendingDenyEvent, reason)))
			return
		}
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
	fmt.Println(string(b))
}

// pendingDenyRuntime/Event are the staged action's runtime and raw event so
// emitDeny can ask that runtime's installer for its envelope.
var pendingDenyRuntime, pendingDenyEvent string

type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	// RawHookEventName is the event name exactly as the runtime sent it,
	// before canonicalization — the name an installer's encoder echoes back.
	RawHookEventName string `json:"-"`
	// Carrier says this invocation can print injected context for its
	// runtime: the governed lane when the installer publishes a context
	// encoder for this event, the collection lane when its plugin captured
	// stdout (--carrier). It rides every envelope so the daemon hands over
	// pending helper messages only where they can land.
	Carrier        bool   `json:"-"`
	SessionID      string `json:"session_id"`
	Source         string `json:"source"`
	ToolName       string `json:"tool_name"`
	Cwd            string `json:"cwd"`
	ToolUseID      string `json:"tool_use_id"`
	CallID         string `json:"call_id"`
	TranscriptPath string `json:"transcript_path"`
	// NotificationType is the vendor's sub-type on an attention event
	// (permission prompt vs idle nudge). It is recorded as provenance; the
	// installer's matcher, not this code, decides which sub-types fire at all.
	NotificationType string `json:"notification_type"`
	// Runtime does NOT come from the payload — the runtimes do not send it. It is
	// filled from our own --runtime flag, which the installer bakes into the command
	// it writes into that vendor's config.
	Runtime string `json:"-"`
	// ActionID is collector-owned correlation for one attempted tool action. It is
	// deliberately generic: lower observation/governance code does not know which
	// optional upper-layer consumers may correlate the committed evidence.
	ActionID         string          `json:"-"`
	ToolInput        toolInput       `json:"-"`
	RawToolInput     json.RawMessage `json:"-"`
	RawEnvelopeBytes int             `json:"-"`
	RawToolResponse  json.RawMessage `json:"-"`
	ToolIsError      bool            `json:"-"`
	ToolError        string          `json:"-"`
	DurationMS       int64           `json:"-"`
}

type toolInput struct {
	Command          commandField `json:"command"`
	FilePath         string       `json:"file_path"`
	Path             string       `json:"path"`
	NotebookPath     string       `json:"notebook_path"`
	URL              string       `json:"url"`
	Content          string       `json:"content"`    // Write body
	OldString        string       `json:"old_string"` // Edit match body
	NewString        string       `json:"new_string"` // Edit body
	ReplaceAll       bool         `json:"replace_all"`
	Skill            string       `json:"skill"` // Skill tool: WHICH skill (D6)
	WorkingDirectory string       `json:"working_directory"`
}

func (in *hookInput) UnmarshalJSON(b []byte) error {
	var wire struct {
		HookEventName  string          `json:"hook_event_name"`
		SessionID      string          `json:"session_id"`
		ConversationID string          `json:"conversation_id"`
		Source         string          `json:"source"`
		ToolName       string          `json:"tool_name"`
		Cwd            string          `json:"cwd"`
		ToolUseID      string          `json:"tool_use_id"`
		CallID         string          `json:"call_id"`
		TranscriptPath string          `json:"transcript_path"`
		ToolInput      json.RawMessage `json:"tool_input"`
		ToolResponse   json.RawMessage `json:"tool_response"`
		ToolResult     json.RawMessage `json:"tool_result"`
		ToolOutput     json.RawMessage `json:"tool_output"`
		IsError        bool            `json:"is_error"`
		Error          any             `json:"error"`
		ErrorMessage   string          `json:"error_message"`
		DurationMS     *int64          `json:"duration_ms"`
		Duration       *int64          `json:"duration"`
		WorkspaceRoots []string        `json:"workspace_roots"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	if wire.SessionID != "" && wire.ConversationID != "" && wire.SessionID != wire.ConversationID {
		return fmt.Errorf("conflicting session_id and conversation_id; refusing to guess session identity")
	}
	if wire.DurationMS != nil && wire.Duration != nil && *wire.DurationMS != *wire.Duration {
		return fmt.Errorf("conflicting duration_ms and duration; refusing to guess result duration")
	}
	in.RawHookEventName = wire.HookEventName
	in.HookEventName = canonicalHookEvent(wire.HookEventName)
	in.SessionID = wire.SessionID
	if in.SessionID == "" {
		in.SessionID = wire.ConversationID
	}
	in.Source, in.ToolName, in.Cwd = wire.Source, wire.ToolName, wire.Cwd
	in.ToolUseID, in.CallID, in.TranscriptPath = wire.ToolUseID, wire.CallID, wire.TranscriptPath
	if wire.DurationMS != nil {
		in.DurationMS = *wire.DurationMS
	} else if wire.Duration != nil {
		in.DurationMS = *wire.Duration
	}
	in.RawToolInput = append(in.RawToolInput[:0], wire.ToolInput...)
	in.RawEnvelopeBytes = len(b)
	in.RawToolResponse = append(in.RawToolResponse[:0], wire.ToolResponse...)
	if len(in.RawToolResponse) == 0 || string(in.RawToolResponse) == "null" {
		in.RawToolResponse = append(in.RawToolResponse[:0], wire.ToolResult...)
	}
	if len(in.RawToolResponse) == 0 || string(in.RawToolResponse) == "null" {
		in.RawToolResponse = append(in.RawToolResponse[:0], wire.ToolOutput...)
	}
	in.ToolIsError = wire.IsError
	if wire.Error != nil {
		in.ToolIsError = true
		in.ToolError = fmt.Sprint(wire.Error)
	}
	if wire.ErrorMessage != "" {
		in.ToolIsError = true
		in.ToolError = wire.ErrorMessage
	}
	if len(wire.ToolInput) > 0 && string(wire.ToolInput) != "null" {
		if err := json.Unmarshal(wire.ToolInput, &in.ToolInput); err != nil {
			return err
		}
	}
	if in.Cwd == "" {
		in.Cwd = boundedWorkspaceCwd(wire.WorkspaceRoots, in.ToolInput)
	}
	return nil
}

func canonicalHookEvent(event string) string {
	switch event {
	case "sessionStart":
		return "SessionStart"
	case "preToolUse":
		return "PreToolUse"
	case "postToolUse":
		return "PostToolUse"
	case "postToolUseFailure":
		return "PostToolUseFailure"
	case "sessionEnd":
		return "SessionEnd"
	default:
		return event
	}
}

func boundedWorkspaceCwd(roots []string, input toolInput) string {
	cleanRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return ""
		}
		cleanRoots = append(cleanRoots, filepath.Clean(root))
	}
	if input.WorkingDirectory != "" && filepath.IsAbs(input.WorkingDirectory) {
		working := filepath.Clean(input.WorkingDirectory)
		matches := 0
		for _, root := range cleanRoots {
			if pathWithinRoot(working, root) {
				matches++
			}
		}
		if matches == 1 {
			return working
		}
	}
	if len(cleanRoots) != 1 {
		return ""
	}
	root := cleanRoots[0]
	for _, candidate := range []string{input.FilePath, input.Path, input.NotebookPath} {
		if candidate != "" && filepath.IsAbs(candidate) && !pathWithinRoot(filepath.Clean(candidate), root) {
			return ""
		}
	}
	return root
}

func pathWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// structuredFilePaths returns only file identities mechanically declared by a tool
// input. Structured native tools use their fields; the bounded shell collector accepts
// only literal operands for explicitly supported commands.
func structuredFilePaths(in hookInput) []string {
	paths := []string{}
	seen := map[string]bool{}
	for _, claim := range structuredResourceClaims(in) {
		if claim.Kind == "file" && claim.Identity != "" && !seen[claim.Identity] {
			seen[claim.Identity] = true
			paths = append(paths, claim.Identity)
		}
	}
	return paths
}

// commandField tolerates both wire shapes: Claude sends the shell command as a
// string; Codex shell tools (local_shell/exec_command) may send an argv array.
type commandField string

func (c *commandField) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*c = commandField(s)
		return nil
	}
	var argv []string
	if json.Unmarshal(b, &argv) == nil {
		*c = commandField(strings.Join(argv, " "))
		return nil
	}
	return nil // unknown shape -> empty command -> no guard matches
}

// engineDecision runs the SHARED engine over a tool-call event (tool identity, path,
// url) and returns its decision — the same classify→decide the tags/audit/checkpoint
// paths use. Returns (nil, false) when engine config is absent (engine is additive; a
// missing detectors/policy file means "engine has nothing to say," not fail-closed —
// the legacy regex path keeps its own fail-closed semantics). Session-scoped predicates
// (e.g. "PII AND non-compliant endpoint" accumulated across a session) need the P3
// ledger daemon; at a bare hook this decides on THIS event's tags only — declared.
func engineDecision(in hookInput) (*engine.Decision, []engine.Tag, *engine.RuleBoundary, bool) {
	// Detectors now ship embedded (LoadLayered is never empty), so the ONLY gate
	// left is policy: with no policy file the engine still says nothing and the
	// legacy regex path keeps its fail-closed semantics — adding the tag library
	// cannot change a decision, only what a present policy has to reason over.
	supplement, err := rulebook.LoadInvocationPolicy()
	if err != nil || !supplement.Available {
		return nil, nil, nil, false
	}
	loadedDetectors, err := activeDetectorDocument()
	if err != nil {
		return nil, nil, nil, false
	}
	dets := loadedDetectors.Detectors
	pol := supplement.Policy
	ev := engine.Event{Tool: engine.BareTool(in.ToolName), Path: in.ToolInput.FilePath,
		Destination: in.ToolInput.URL, Text: string(in.ToolInput.Command), Role: "tool_call"}
	tags := engine.Classify(ev, dets)
	d := engine.Decide(tags, pol)
	// the compiled honest label for the fired rule (P-COMPILE-2) rides
	// along so the decision log carries boundary = detection ∧ reach
	var boundary *engine.RuleBoundary
	if d.Rule != "" { // allow decisions carry no rule; "" must not bind an id-less rule
		for _, r := range pol.Rules {
			if r.ID == d.Rule {
				b := engine.Boundary(r, dets, engine.ReachStop)
				boundary = &b
				break
			}
		}
	}
	return &d, tags, boundary, true
}

// emitObserve writes the complete v1 envelope to a local owner-only spool before its
// bounded synchronous send. The spool survives a stopped/old daemon and is removed only
// after a versioned acknowledgement confirms the exact observation ID.

// pendingObserve holds the action until its decision is known, so ONE event carries
// both. Sending at entry (as this did) meant the log recorded that an action was
// attempted and never what was decided — and since a denied call never reaches the
// vendor transcript, enforcement left no trace anywhere on the machine.
var pendingObserve *hookInput

// observeAttempt stages the action. Nothing is sent until observeDecision runs.
func observeAttempt(in hookInput) {
	pendingDenyRuntime, pendingDenyEvent = in.Runtime, in.RawHookEventName
	if in.RawHookEventName == "" {
		pendingDenyEvent = in.HookEventName
	}
	if in.ActionID == "" {
		id, err := newActionID()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[crossing-guard] action identity unavailable; observation will remain uncorrelated:", err)
		} else {
			in.ActionID = id
		}
	}
	cp := in
	pendingObserve = &cp
}

// observeDecision flushes the staged action WITH its decision. Safe to call more
// than once — only the first wins, so a path that both denies and falls through
// cannot double-log. Callers must reach this on every exit; the deny helpers and
// the allow tail below do.
// exitAllow is the ONE choke point for "the tool proceeds". Pairs with emitDeny so
// both outcomes reach the log through a named funnel; a bare os.Exit(0) on an allow
// path is the bug this replaces.
func exitAllow(reason string) {
	in, deliveries := observeDecision("allow", reason)
	// The allow path stays silent unless the daemon handed this boundary a
	// pending helper message; then the runtime's installer encodes it as
	// context — never a decision key (helper-session-attachment plan D5/H2).
	printHookContext(in, deliveries)
	os.Exit(0)
}

// hookCanCarry reports whether this invocation's runtime publishes a context
// encoder for the raw event — the only case the governed lane can print what
// the daemon hands it.
func hookCanCarry(in hookInput) bool {
	encoder, ok := hookInstallers[in.Runtime].(HookContextEncoder)
	if !ok {
		return false
	}
	event := in.RawHookEventName
	if event == "" {
		event = in.HookEventName
	}
	_, ok = encoder.EncodeHookContext(event, "")
	return ok
}

// printHookContext prints the injected-context envelope for the runtime that
// invoked us, when its installer publishes one for this event. Messages are
// joined in delivery order; the encoder applies the vendor's cap.
func printHookContext(in hookInput, deliveries []observation.Delivery) {
	if len(deliveries) == 0 {
		return
	}
	encoder, ok := hookInstallers[in.Runtime].(HookContextEncoder)
	if !ok {
		fmt.Fprintf(os.Stderr, "[crossing-guard] %d helper message(s) were handed to this boundary but runtime %q publishes no context encoder; they are recorded as delivered and lost here\n", len(deliveries), in.Runtime)
		return
	}
	event := in.RawHookEventName
	if event == "" {
		event = in.HookEventName
	}
	joined := ""
	for index, delivery := range deliveries {
		if index > 0 {
			joined += "\n\n"
		}
		joined += delivery.Message
	}
	if b, ok := encoder.EncodeHookContext(event, joined); ok {
		fmt.Println(string(b))
	}
}

func observeDecision(decision, reason string) (hookInput, []observation.Delivery) {
	if pendingObserve == nil {
		return hookInput{}, nil
	}
	in := *pendingObserve
	pendingObserve = nil
	return in, emitObserve(in, decision, reason)
}

func emitObserve(in hookInput, decision, reason string) []observation.Delivery {
	if os.Getenv("CG_OBSERVE") == "0" {
		return nil // observation opted out — does NOT affect enforcement or discovery
	}
	envelope, err := buildObservationEnvelope(in, decision, reason)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] observation envelope failed:", err)
		return nil
	}
	spoolPath, err := spoolObservation(envelope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] observation spool failed:", err)
		return nil
	}
	addr, tok, ok := daemonEndpoint()
	if !ok {
		return nil
	}
	receipt, err := sendObservation(addr, tok, envelope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] observation pending:", err)
		return nil
	}
	if err := os.Remove(spoolPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "[crossing-guard] observation acknowledged but spool cleanup failed:", err)
	} else if err == nil {
		if syncErr := syncObservationSpoolDir(filepath.Dir(spoolPath)); syncErr != nil {
			fmt.Fprintln(os.Stderr, "[crossing-guard] observation acknowledged but spool directory sync failed:", syncErr)
		}
	}
	return receipt.Deliveries
}

// statefulTimeout bounds the stateful-tier consult. It is deliberately SHORT: the
// consult is on the tool-call path, and a slow or dead daemon must never stall the
// agent. On timeout the hook FAILS OPEN — the tool proceeds (owner decision 2026-07-20).
const statefulTimeout = 400 * time.Millisecond

// statefulVerdict is a daemon-side stateful decision the hook must enforce.
type statefulVerdict struct {
	decision string // deny | ask
	rule     string
	message  string
	reason   string
}

// consultStateful asks the daemon to decide over live session/target state (Phase 3).
// It returns a verdict ONLY when the daemon evaluated the stateful tier AND a rule
// denied/asked. Every failure path — no daemon, timeout, non-200, decode error,
// allow, or not-evaluated — returns nil, which the caller treats as "proceed". That is
// the fail-open guarantee: a daemon problem can never turn into a blocked tool call.
func consultStateful(in hookInput) *statefulVerdict {
	addr, tok, ok := daemonEndpoint()
	if !ok {
		return nil // no daemon → fail open
	}
	content := in.ToolInput.Content
	if content == "" {
		content = in.ToolInput.NewString
	}
	body, _ := json.Marshal(map[string]any{
		"session":    in.SessionID,
		"tool":       in.ToolName,
		"command":    string(in.ToolInput.Command),
		"content":    content,
		"file_path":  in.ToolInput.FilePath,
		"file_paths": structuredFilePaths(in),
		"cwd":        in.Cwd,
		"url":        in.ToolInput.URL,
		"skill":      in.ToolInput.Skill,
		"runtime":    in.Runtime,
	})
	req, err := http.NewRequest("POST", "http://"+addr+"/api/govern/decide", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("X-CG-Token", tok)
	}
	resp, err := (&http.Client{Timeout: statefulTimeout}).Do(req)
	if err != nil {
		return nil // unreachable / timeout → fail open
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var d struct {
		Decision, Rule, Message, Reason string
		Evaluated                       bool
	}
	if json.NewDecoder(resp.Body).Decode(&d) != nil {
		return nil
	}
	if !d.Evaluated || d.Decision == "" || d.Decision == "allow" {
		return nil // nothing stateful fired → proceed
	}
	return &statefulVerdict{decision: d.Decision, rule: d.Rule, message: d.Message, reason: d.Reason}
}

func cmdHook(args []string) {
	var in hookInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		// FAIL-OPEN, on purpose: a payload we cannot parse is most likely a vendor
		// format change, and denying every tool call on a parse error would brick
		// the agent (the same reasoning as the stateful consult's fail-open). But
		// open must not also be SILENT — this line on stderr is the only trace that
		// every call is sailing past an un-parsing guard.
		fmt.Fprintln(os.Stderr, "[crossing-guard] hook payload did not parse — allowing without judging:", err)
		os.Exit(0)
	}
	// WHICH runtime invoked us. The installer wrote this into the command, so it is
	// a fact about the wiring rather than a guess about the payload. Absent (a hook
	// installed before the flag existed) stays EMPTY and is recorded as unknown —
	// never defaulted to a vendor, which would attribute one agent's actions to
	// another in the only record that a blocked action ever existed.
	in.Runtime = flagValue(args, "--runtime")
	in.Carrier = hookCanCarry(in)
	// A turn-boundary hook carries OUR kind in its own command line, so this
	// dispatch never has to know the provider's name for it (decision 1, 2026-09-01).
	if kind := flagValue(args, "--observe"); kind != "" {
		printHookContext(in, emitSessionTurn(in, kind))
		return
	}
	switch in.HookEventName {
	case "SessionStart":
		emitSessionEntry(in)
		return
	case "PostToolUse", "PostToolUseFailure":
		printHookContext(in, emitResult(in))
		return
	case "SessionEnd":
		emitClosure(in)
		return
	case "PreToolUse":
		// Continue through the unchanged policy/decision path below.
	default:
		return
	}
	observeAttempt(in) // staged; flushed WITH the decision by emitDeny/exitAllow
	// The engine tier: detectors → water mark → severity ladder, decided locally on
	// THIS event's tags. Session-scoped accumulation lives in the stateful tier below,
	// consulted from the one daemon (consultStateful).
	d, tags, boundary, _ := engineDecision(in)
	var mark string
	if d != nil {
		mark = engine.WaterMark(tags)
	}
	// ENGINE path (event-local). Maps the severity ladder onto what a hook can
	// actually do at STOP reach.
	if d != nil && d.Decision != "allow" {
		observed := map[string]any{"rule": d.Rule, "mode": string(d.Mode),
			"fired_tags": tagSummary(d.FiredTags), "watermark": mark,
			"session": in.SessionID, "via": "engine-local"}
		if boundary != nil {
			observed["boundary"] = boundary.Label
		}
		switch d.Mode {
		case engine.HardBlock:
			logLine(mergeMap(observed, map[string]any{"decision": "deny",
				"note": "hard-block, non-overridable"}))
			emitDeny(fmt.Sprintf("Blocked by rule %s (Restricted, non-overridable): %s. "+
				"Tags: %s.", d.Rule, d.Message, tagSummary(d.FiredTags)))
			os.Exit(0)
		case engine.ConfirmAndRecord:
			// the hook's honest override capture: inbox-first (typed, attributed
			// reason via the console), dialog fallback (button only). Allow does
			// NOT un-taint; it's recorded as an attributed override.
			bLabel := ""
			if boundary != nil {
				bLabel = boundary.Label
			}
			allowed, mode, reason := askHuman(in.SessionID, in.Runtime, d.Rule, string(d.Mode),
				d.Message, string(in.ToolInput.Command), tagSummary(d.FiredTags), bLabel)
			claimed := "(dialog-button; no typed reason)"
			if reason != "" {
				claimed = reason + " (ATTRIBUTED, NOT VERIFIED)"
			}
			decision := "deny"
			if allowed {
				decision = "allow"
			}
			logLine(mergeMap(observed, map[string]any{"decision": decision,
				"override":  mode,
				"claimed":   claimed,
				"untainted": false}))
			if allowed {
				exitAllow("override: " + mode)
			}
			emitDeny(fmt.Sprintf("Blocked by rule %s: %s. Tags: %s. Confirm to override "+
				"(recorded, does not un-taint the session).", d.Rule, d.Message,
				tagSummary(d.FiredTags)))
			os.Exit(0)
		case engine.WarnAndProceed:
			logLine(mergeMap(observed, map[string]any{"decision": "allow",
				"note": "warn-and-proceed"}))
			// warn = proceed, logged; fall through to the legacy regex guard too
		}
	}
	// Command-guard layer (destructive-rm, curl|sh, force-push …), ADDITIVE to the
	// (The historical name now also includes exact tool-identity rules.) It is additive to
	// the engine layer above and decided by the SAME evaluator (ADR 0025): the shipped
	// default rules are engine.Policy, evaluated by engine.Decide over the exact bare tool
	// identity and raw command. The private regex evaluator is gone.
	//
	// A missing rules.json is not "unconfigured": rulebook.Load falls back to the SHIPPED
	// default, so a fresh install is governed by sane rules rather than denying every
	// tool call (which bricked the agent) or allowing everything. A present-but-broken
	// file fails CLOSED.
	pol, err := rulebook.Load()
	if err != nil {
		emitDeny("crossing-guard could not load its rules; failing closed")
		os.Exit(0)
	}
	command := string(in.ToolInput.Command)
	gd := engine.Decide(staticInvocationTags(in.ToolName, command), pol)
	if gd.Decision == "allow" {
		// Static tier allows. Consult the STATEFUL tier (Phase 3) — decisions over the
		// session's/target's live state that only the daemon holds. FAIL-OPEN: a nil
		// verdict (no daemon, timeout, nothing fired) means proceed, so a daemon problem
		// never blocks a tool call.
		if sv := consultStateful(in); sv != nil {
			enforceStateful(in, sv, command)
		}
		exitAllow("no rule matched") // static + stateful both clear -> silent allow
	}
	allowed := false
	promptMode := "auto" // deny (HardBlock) takes no prompt
	reason := ""
	if gd.Mode != engine.HardBlock {
		allowed, promptMode, reason = askHuman(in.SessionID, in.Runtime, gd.Rule, "ask",
			"Guarded operation ("+gd.Rule+")", command, gd.Rule, "")
	}
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	entry := map[string]any{"command": command, "rule": gd.Rule, "guard": gd.Rule,
		"mode": string(gd.Mode), "decision": decision, "prompt": promptMode}
	if reason != "" {
		entry["claimed"] = reason + " (ATTRIBUTED, NOT VERIFIED)"
	}
	logLine(entry)
	if allowed {
		exitAllow("confirmed by user") // allow: emit nothing, the tool proceeds
	}
	emitDeny(staticDenialReason(gd))
	os.Exit(0)
}

// collectionHookEmitters makes the collection-only dispatch independently
// testable. In particular, there is deliberately no evaluator or decision callback
// in this surface: a runtime wired to collect-hook can record but cannot govern.
type collectionHookEmitters struct {
	action func(hookInput, string, string)
	result func(hookInput)
	entry  func(hookInput)
	close  func(hookInput)
}

func dispatchCollectionHook(in hookInput, emit collectionHookEmitters) {
	switch in.HookEventName {
	case "SessionStart":
		emit.entry(in)
	case "PreToolUse":
		emit.action(in, "", "")
	case "PostToolUse", "PostToolUseFailure":
		emit.result(in)
	case "SessionEnd":
		emit.close(in)
	}
}

func cmdCollectionHook(args []string) {
	var in hookInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] collection payload did not parse — continuing without collection:", err)
		return
	}
	in.Runtime = flagValue(args, "--runtime")
	// --carrier is the plugin's declaration that it captures our stdout and
	// appends what it finds; an older plugin never passes it and never
	// receives a pending message (the record waits for a carrier or expires).
	in.Carrier = hasFlag(args, "--carrier")
	if kind := flagValue(args, "--observe"); kind != "" {
		printCollectionDeliveries(emitSessionTurn(in, kind))
		return
	}
	if in.HookEventName == "PreToolUse" {
		id, err := newActionID()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[crossing-guard] action identity unavailable; observation will remain uncorrelated:", err)
		} else {
			in.ActionID = id
		}
	}
	dispatchCollectionHook(in, collectionHookEmitters{
		action: func(in hookInput, decision, reason string) {
			printCollectionDeliveries(emitObserve(in, decision, reason))
		},
		result: func(in hookInput) { printCollectionDeliveries(emitResult(in)) },
		entry:  emitSessionEntry,
		close:  emitClosure,
	})
}

// printCollectionDeliveries is the collection-hook delivery channel
// (helper-session-attachment plan D5/A8): a collection-lane plugin that
// captured our stdout reads one JSON line and appends the messages through
// its runtime's own session API. Nothing is printed when nothing was handed
// over, so an older plugin that discards stdout is unaffected.
func printCollectionDeliveries(deliveries []observation.Delivery) {
	if len(deliveries) == 0 {
		return
	}
	fmt.Println(string(mustJSON(map[string]any{"crossing_guard_deliveries": deliveries})))
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

// enforceStateful applies a daemon stateful verdict through the SAME funnels as the
// static tier — emitDeny (which honors `enforce off`) for deny, askHuman for ask — so
// the two tiers block identically and a stateful deny is subject to the same override
// and enforcement-switch rules. A deny never returns; an ask that is confirmed does.
func enforceStateful(in hookInput, sv *statefulVerdict, command string) {
	msg := sv.message
	if msg == "" {
		msg = "blocked by a stateful rule over this session's live state"
	}
	if sv.decision == "ask" {
		allowed, mode, reason := askHuman(in.SessionID, in.Runtime, sv.rule, "ask", msg, command, sv.rule, "")
		claimed := "(dialog-button; no typed reason)"
		if reason != "" {
			claimed = reason + " (ATTRIBUTED, NOT VERIFIED)"
		}
		decision := "deny"
		if allowed {
			decision = "allow"
		}
		logLine(map[string]any{"command": command, "rule": sv.rule, "tier": "stateful",
			"decision": decision, "override": mode, "claimed": claimed})
		if allowed {
			return // confirmed — fall back to the caller's exitAllow
		}
		emitDeny(fmt.Sprintf("Blocked by stateful rule %s: %s. Confirm to override (recorded).", sv.rule, msg))
		os.Exit(0)
	}
	logLine(map[string]any{"command": command, "rule": sv.rule, "tier": "stateful", "decision": "deny"})
	emitDeny(fmt.Sprintf("Blocked by stateful rule %s: %s. %s", sv.rule, msg, sv.reason))
	os.Exit(0)
}

// guardMessage prefers the rule's authored message, falling back to its id so a
// denial is never a bare id with no explanation.
func guardMessage(d engine.Decision) string {
	if d.Message != "" {
		return d.Message
	}
	return "guarded operation"
}

// staticDenialReason keeps non-overridable rules from suggesting an override or a
// manual bypass. ConfirmAndRecord remains recoverable through its existing ask path.
func staticDenialReason(d engine.Decision) string {
	reason := fmt.Sprintf("Enforced by rule %s: %s — denied.", d.Rule, guardMessage(d))
	if d.Mode == engine.HardBlock {
		return reason
	}
	return reason + " Ask the user to review the guarded operation before retrying."
}

func cmdCheck(command string) {
	v, err := CheckCommand(command)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if v.Decision == "allow" {
		fmt.Printf("allow    (no rule matched)   %q\n", command)
		return
	}
	verdict := "ASK "
	if v.Decision == "deny" {
		verdict = "DENY"
	}
	fmt.Printf("%s     %s   %q\n", verdict, v.Rule, command)
}

// cmdTags runs the SHARED engine (the same classify → water-mark → decide the hook,
// audit, and checkpoint paths use) against one event. Proves the port is shared, not
// a parallel implementation. Detectors: $CG_DETECTORS or detectors.json; policy:
// $CG_POLICY or policy.json (optional — omit to just see tags).
func cmdTags(eventJSON string) {
	var ev engine.Event
	if err := json.Unmarshal([]byte(eventJSON), &ev); err != nil {
		fmt.Fprintln(os.Stderr, "bad event json:", err)
		os.Exit(1)
	}
	if ev.Role == "" {
		ev.Role = "tool_call" // the common inspection case; role-scoped detectors need it
	}
	loadedDetectors, err := activeDetectorDocument()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load detectors:", err)
		os.Exit(1)
	}
	dets := loadedDetectors.Detectors
	tags := engine.Classify(ev, dets)
	if len(tags) == 0 {
		fmt.Println("tags: (none) — a WEAK negative: absence ≠ safe (design §4)")
	} else {
		fmt.Println("tags:")
		for _, t := range tags {
			cov := "enumerable"
			if !t.Coverage.Enumerable {
				cov = "UNKNOWABLE"
			}
			fmt.Printf("  %s=%s  [%s]  prov=%s  evidence=%s  (coverage: %s)\n",
				t.Key, t.Value, t.Detector, t.Provenance, t.Evidence, cov)
		}
	}
	fmt.Printf("water-mark: %s\n", markOrWeak(engine.WaterMark(tags)))
	if supplement, supplementErr := rulebook.LoadInvocationPolicy(); supplementErr == nil && supplement.Available {
		if pol := supplement.Policy; pol != nil {
			d := engine.Decide(tags, pol)
			fmt.Printf("decision: %s", strings.ToUpper(d.Decision))
			if d.Rule != "" {
				fmt.Printf("  [%s] rule=%s — %s", d.Mode, d.Rule, d.Message)
			}
			fmt.Println()
			for _, m := range d.Menu {
				flag := "•"
				if !m.Live {
					flag = "×"
				}
				fmt.Printf("    [%s] %-20s %s\n", flag, m.Action, m.Why)
			}
			if d.Rule != "" { // allow decisions carry no rule
				for _, r := range pol.Rules {
					if r.ID == d.Rule {
						b := engine.Boundary(r, dets, engine.ReachDryRun)
						fmt.Printf("boundary: %s\n", b.Label)
						break
					}
				}
			}
		}
	}
}

// cmdCoverage prints the compiled honest label for every rule in the engine
// policy (P-COMPILE-2): boundary = detection-coverage ∧ enforcement-reach,
// derived mechanically — never asserted. The legacy regex layer gets its
// static config-grade label alongside.
func cmdCoverage() {
	loadedDetectors, err := activeDetectorDocument()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load detectors:", err)
		os.Exit(1)
	}
	dets := loadedDetectors.Detectors
	supplement, err := rulebook.LoadInvocationPolicy()
	if err != nil || !supplement.Available {
		fmt.Fprintln(os.Stderr, "load policy:", err)
		os.Exit(1)
	}
	pol := supplement.Policy
	for _, b := range engine.CompileBoundaries(pol, dets, engine.ReachStop) {
		fmt.Printf("%-24s [%s]  %s\n", b.Rule, b.Mode, b.DetectionLabel)
		fmt.Printf("    %s\n", b.Label)
		for _, tc := range b.Detection {
			neg := ""
			if tc.Negated {
				neg = "NOT "
			}
			switch {
			case tc.Undetectable:
				fmt.Printf("    - %s%s=%s: UNDETECTABLE — no detector produces this tag\n", neg, tc.Tag, tc.Value)
			case !tc.Enumerable:
				fmt.Printf("    - %s%s=%s: UNKNOWABLE gaps  (detectors: %s)\n", neg, tc.Tag, tc.Value, strings.Join(tc.Detectors, ", "))
			default:
				fmt.Printf("    - %s%s=%s: enumerable  (detectors: %s; gaps: %s)\n",
					neg, tc.Tag, tc.Value, strings.Join(tc.Detectors, ", "), strings.Join(tc.Gaps, "; "))
			}
		}
	}
	if loaded, err := rulebook.LoadDocument(); err == nil && len(loaded.Policy.Rules) > 0 {
		fmt.Printf("%-24s [legacy]  best-effort [config]\n", "rules.json guards")
		fmt.Println("    regex guards on shell-command text; per-tool hook registration;")
		fmt.Printf("    active=%s selection=%s digest=%s\n", loaded.Origin, loaded.Selection, loaded.Digest)
		fmt.Println("    coverage not canary-probed — a hard guarantee needs a non-bypassable backstop")
	}
}

func markOrWeak(m string) string {
	if m == "" {
		return "(none — weak negative, not a clean bill)"
	}
	return m
}

func tagSummary(tags []engine.Tag) string {
	var parts []string
	for _, t := range tags {
		parts = append(parts, t.Key+"="+t.Value)
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

func mergeMap(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// Main is the guardcli entry point: args is everything after the binary name
// (cmd/cp and cmd/crossing-guard both dispatch here). It may os.Exit.
func Main(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	switch args[0] {
	case "hook":
		cmdHook(args[1:])
	case "govern-hook":
		cmdGovernHook(args[1:])
	case "collect-hook":
		cmdCollectionHook(args[1:])
	case "check":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, `usage: cp check "<command>"`)
			os.Exit(1)
		}
		cmdCheck(args[1])
	case "tags":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, `usage: tags '<event-json>'`)
			os.Exit(1)
		}
		cmdTags(args[1])
	case "coverage":
		cmdCoverage()
	case "install":
		cmdInstallHook(args[1:])
	case "enforce":
		cmdEnforce(os.Args[2:])
	case "entities":
		cmdEntities(os.Args[2:])
	case "detectors":
		cmdDetectors(args[1:])
	case "chain":
		cmdChain(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", args[0])
		os.Exit(1)
	}
}

// cmdDetectors is the editable-config surface for the detection library:
//
//	detectors status        typed active/available/selection facts
//	detectors list          the EFFECTIVE set plus its assembly identity
//	detectors preview SRC   exact id/digest change before selection
//	detectors select SRC --yes  pin a complete validated effective snapshot
//	detectors unselect --yes    archive selection and return to cohort baseline
//	detectors init [path]    materialize an inert editable draft seeded from the starter
//	detectors lint <file>    validate a detector document (compiles + declares coverage)
func cmdDetectors(args []string) {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status":
		loaded, err := activeDetectorDocument()
		if err != nil {
			fmt.Fprintln(os.Stderr, "detectors unavailable:", err)
			os.Exit(1)
		}
		printDetectorStatus(loaded)
	case "list":
		loadedDetectors, err := activeDetectorDocument()
		if err != nil {
			fmt.Fprintln(os.Stderr, "load:", err)
			os.Exit(1)
		}
		dets := loadedDetectors.Detectors
		fmt.Printf("effective detectors: %d   origin=%s selection=%s digest=%s\n",
			len(dets), loadedDetectors.Origin, loadedDetectors.Selection, loadedDetectors.Digest)
		fmt.Printf("cohort=%s surface=%s source=%s\n", loadedDetectors.InstallCohort,
			loadedDetectors.Surface, loadedDetectors.Path)
		for _, d := range dets {
			roles := "any"
			if len(d.Roles) > 0 {
				roles = strings.Join(d.Roles, ",")
			}
			cov := "enumerable"
			if !d.Coverage.Enumerable {
				cov = "UNKNOWABLE"
			}
			fmt.Printf("  %-22s %-11s role=%-13s → %s=%s  [%s]\n",
				d.ID, d.Kind, roles, d.Tag.Key, d.Tag.Value, cov)
		}
	case "preview", "select":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: detectors "+sub+" (current|portable-floor|security-observe|legacy|PATH; starter/baseline are compatibility aliases) [--yes]")
			os.Exit(2)
		}
		source, path := detectorSelectionSource(args[1])
		preview, err := detectorselection.PreviewDetectorSelection(dataDir(), detectorselection.DetectorSurfaceCLI, source, path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "preview:", err)
			os.Exit(1)
		}
		printDetectorPreview(preview)
		if sub == "preview" {
			return
		}
		if !stringSliceContains(args[2:], "--yes") {
			fmt.Println("not selected — rerun with --yes after reviewing the exact digest and id changes")
			return
		}
		result, err := detectorselection.SelectDetectors(detectorselection.DetectorSelectRequest{DataRoot: dataDir(),
			Surface: detectorselection.DetectorSurfaceCLI, Source: source, Path: path, Selector: "cli",
			ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken,
			AcknowledgedDigest: preview.ProposedDigest})
		if err != nil {
			fmt.Fprintln(os.Stderr, "select:", err)
			os.Exit(1)
		}
		fmt.Println("selected exact detector assembly:")
		printDetectorStatus(result.Active)
	case "unselect":
		if !stringSliceContains(args[1:], "--yes") {
			fmt.Println("not changed — rerun detectors unselect --yes to archive selection and return to the cohort baseline")
			return
		}
		token, _, err := detectorselection.DetectorSelectionStateToken(dataDir(), detectorselection.DetectorSurfaceCLI)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load durable selection:", err)
			os.Exit(1)
		}
		active, archive, err := detectorselection.UnselectDetectors(dataDir(), detectorselection.DetectorSurfaceCLI, token, "cli")
		if err != nil {
			fmt.Fprintln(os.Stderr, "unselect:", err)
			os.Exit(1)
		}
		fmt.Println("selection archived:", archive)
		printDetectorStatus(active)
	case "init":
		path := detectorsOverlayPath()
		if len(args) > 1 {
			path = args[1]
		}
		if fileExists(path) {
			fmt.Printf("detector draft already exists: %s (not overwriting)\n", path)
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, engine.DefaultDetectorsJSON(), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("wrote inert detector draft (complete starter snapshot; NOT selected) → %s\n", path)
		fmt.Println("review it, then activate exact bytes with: crossing-guard detectors select " + path + " --yes")
	case "lint":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: detectors lint <file>")
			os.Exit(1)
		}
		dets, err := engine.LoadDetectors(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "INVALID:", err)
			os.Exit(1)
		}
		bad := 0
		for _, d := range dets {
			if d.ID == "" {
				fmt.Println("  ✗ a detector has no id (the merge key)")
				bad++
			}
			if d.Disabled {
				continue // a tombstone is just id+disabled; it declares nothing else
			}
			if !d.Coverage.Enumerable && len(d.Coverage.Gaps) == 0 {
				fmt.Printf("  ✗ %s: declares no coverage (not enumerable, no gaps)\n", d.ID)
				bad++
			}
		}
		if bad > 0 {
			fmt.Fprintf(os.Stderr, "%d problem(s)\n", bad)
			os.Exit(1)
		}
		fmt.Printf("OK: %d detectors — all compile and declare coverage\n", len(dets))
	default:
		fmt.Fprintln(os.Stderr, "usage: detectors [status | list | preview | select | unselect | init [path] | lint <file>]")
		os.Exit(1)
	}
}

func detectorSelectionSource(value string) (string, string) {
	switch value {
	case "current", "starter", "baseline", "portable-floor", "security-observe", "legacy":
		return value, ""
	default:
		return "file", value
	}
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func printDetectorPreview(preview detectorselection.DetectorPreview) {
	fmt.Printf("source=%s path=%s proposed=%s detectors=%d\n", preview.Source, preview.SourcePath,
		preview.ProposedDigest, preview.DetectorCount)
	fmt.Printf("reviewed-active=%s state=%s\n", preview.ExpectedActiveDigest, preview.ExpectedStateToken)
	fmt.Printf("added=%s\nremoved=%s\nreplaced=%s\n", strings.Join(preview.Added, ","),
		strings.Join(preview.Removed, ","), strings.Join(preview.Replaced, ","))
	fmt.Println("semantic-reach:", preview.SemanticReach)
}

func printDetectorStatus(loaded *detectorselection.LoadedDetectors) {
	if loaded.InstallProfileCreated {
		fmt.Printf("framework-state-created=%s cohort=%s (no configuration selected)\n",
			filepath.Join(dataDir(), "installation.json"), loaded.InstallCohort)
	}
	fmt.Printf("active=%t selected=%t origin=%s selection=%s\n", loaded.Active, loaded.Selected,
		loaded.Origin, loaded.Selection)
	fmt.Printf("digest=%s state=%s detectors=%d\n", loaded.Digest, loaded.StateToken, len(loaded.Detectors))
	fmt.Printf("cohort=%s surface=%s path=%s\n", loaded.InstallCohort, loaded.Surface, loaded.Path)
	fmt.Printf("starter-available=%s structural-present=%d structural-missing=%s\n",
		loaded.AvailableStarterDigest, len(loaded.StructuralPresent), strings.Join(loaded.StructuralMissing, ","))
	for _, component := range loaded.Components {
		fmt.Printf("component role=%s origin=%s count=%d digest=%s path=%s\n",
			component.Role, component.Origin, component.Count, component.Digest, component.Path)
	}
	if loaded.Displaced != nil {
		fmt.Printf("displaced-selection digest=%s source=%s selected-at=%s\n",
			loaded.Displaced.Digest, loaded.Displaced.Source, loaded.Displaced.SelectedAt)
	}
	if loaded.DisplacedError != "" {
		fmt.Println("displaced-selection-error=" + loaded.DisplacedError)
	}
}
