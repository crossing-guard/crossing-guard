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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/approvalchoice"
	"crossing-guard/internal/detectorselection"
	"crossing-guard/internal/platform"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
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
// engine's evaluator (engine.Judge, ADR 0025). The command-guard layer feeds the raw command in as the
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
	Layer    string // the distribution tier the deciding rule arrived by; "" = unknown
}

// Static is what one static evaluation found.
type Static struct {
	Decision engine.Decision
	// Policy is the rules that were evaluated, so a caller binding the fired rule's
	// boundary looks only at those.
	Policy *engine.Policy
	// StateRules counts the GATING rules left to the daemon's stateful tier (they read
	// session:/target:/agent: state); Undecided counts the gating rules this site could
	// not decide because a term reads a fact it never produces. Either can make the live
	// call differ from this answer. Observe/warn rules never block and are not counted.
	StateRules int
	Undecided  int
	Seen       engine.Seen
}

// StaticDecide is the static tier's only evaluator call. It decides over the rules a
// daemon-free evaluation can judge (engine.StaticTier: no session:/target:/agent: term),
// with the site's blind spots: a term the site cannot answer leaves its rule undecided,
// and an undecided rule does not fire. Every static site goes through here; a scan test
// pins that no other code in the module calls the engine's evaluators outside the
// recorded owners, so a new lane cannot reintroduce a rule that fires by absence.
func StaticDecide(tags []engine.Tag, pol *engine.Policy, unknown engine.Unknown) Static {
	out := Static{Policy: engine.StaticTier(pol)}
	if pol != nil {
		for _, r := range pol.Rules {
			if r.Gates() && engine.ReferencesState(r.If) {
				out.StateRules++
			}
		}
	}
	out.Decision, out.Seen = engine.DecideSeeing(tags, out.Policy, unknown)
	out.Undecided = out.Seen.GatingUndecided()
	return out
}

// verdictOf maps an engine decision over standalone invocation rules onto the CLI verdict.
// deny (HardBlock) → deny; ask (ConfirmAndRecord) → ask; anything that proceeds →
// allow. This is the SAME evaluator the hook runs — one evaluator, one answer.
func verdictOf(d engine.Decision) Verdict {
	switch d.Mode {
	case engine.HardBlock:
		return Verdict{Decision: "deny", Rule: d.Rule, Guard: d.Rule, Layer: string(d.Layer)}
	case engine.ConfirmAndRecord:
		return Verdict{Decision: "ask", Rule: d.Rule, Guard: d.Rule, Layer: string(d.Layer)}
	default:
		return Verdict{Decision: "allow"}
	}
}

// ActiveDetectors resolves the detector document this process's hook would classify
// with. A daemon passes its own governor's set to the check functions instead: this
// loader reads the hook's install profile.
func ActiveDetectors() ([]engine.Detector, error) {
	loaded, err := activeDetectorDocument()
	if err != nil {
		return nil, err
	}
	return loaded.Detectors, nil
}

// Unjudged is what a dry run could not evaluate (Static's two counts).
type Unjudged struct {
	StateRules int
	Undecided  int
}

// CheckCommand dry-runs the active rules against a command through StaticDecide — the
// same evaluator cmdCheck and the hook's tiers use.
func CheckCommand(command string, dets []engine.Detector) (Verdict, error) {
	v, _, err := CheckCommandStatic(command, dets)
	return v, err
}

// CheckCommandStatic is CheckCommand plus what the dry run could not evaluate, so a
// preview can say that its ALLOW is not the whole answer.
func CheckCommandStatic(command string, dets []engine.Detector) (Verdict, Unjudged, error) {
	pol, err := rulebook.Load()
	if err != nil {
		return Verdict{}, Unjudged{}, err
	}
	st := CheckAction("", command, dets, pol)
	return verdictOf(st.Decision), Unjudged{StateRules: st.StateRules, Undecided: st.Undecided}, nil
}

// CheckAction decides a shell command as a dry run. With no tool named it is a command
// preview: the tool, and every detector fact that needs the tool, a path or a
// destination, are undecidable there (engine.UnknownInPreview). nil dets means the
// detector document is unavailable: only the invocation's own facts are readable.
func CheckAction(tool, command string, dets []engine.Detector, pol *engine.Policy) Static {
	ev := engine.ActionEvent(tool, command, "", "", "")
	var unknown engine.Unknown
	tags := engine.InvocationTags(tool, command)
	if dets == nil {
		unknown = engine.UnknownWithoutDetectors
	} else {
		tags = engine.ActionTags(ev, dets, command)
	}
	if ev.Tool == "" {
		unknown = engine.AnyUnknown(unknown, engine.UnknownInPreview(dets))
	}
	return StaticDecide(tags, pol, unknown)
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
	// ONE deadline per invocation: the runtime's hook timeout runs from when it started
	// us, not from this prompt. A second prompt (a stateful ask after a confirmed static
	// one) gets what the first left; with nothing left the call is denied here, without
	// asking — a zero budget reads as the full one at the inbox, and a prompt that
	// outlives the runtime's timeout lets the call proceed unasked.
	budget -= time.Since(hookStarted)
	if budget <= 0 {
		fmt.Fprintf(os.Stderr, "[crossing-guard] no approval time left in this hook run for rule %s; failing closed (deny)\n", ruleID)
		return false, "ask-budget-exhausted-fail-closed", ""
	}
	// Record the hold BEFORE we block: past this line the runtime may kill us.
	observeAsk("held for human: rule " + ruleID)
	return askViaInbox(session, runtime, ruleID, mode, message, command, tagSum, boundary, budget)
}

// hookStarted is when this process began: the runtime's hook timeout is counted from
// its start of us, so every prompt's budget is measured from here.
var hookStarted = time.Now()

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
	// Action, Targets, and ApprovalReason are bounded display facts supplied by
	// the runtime adapter. Authority and lifetime stay daemon-owned: a provider
	// cannot widen an approval by changing presentation text.
	Action         string
	Targets        []string
	ApprovalReason string
	// OfferExactRunGrant asks the daemon to offer its server-owned, task-bound
	// exact-match option. GrantToken is the opaque capability returned by an
	// earlier interactive selection in this same runtime run.
	OfferExactRunGrant bool
	GrantToken         string
	// Prompts are the questions the held call is asking its approver, if any.
	// They are a bounded projection for display and validation; the exact tool
	// input never leaves the bridge.
	Prompts []approvalchoice.ChoicePrompt
	Timeout time.Duration
}

type RuntimeApprovalResult struct {
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	GrantID    string `json:"grant_id,omitempty"`
	GrantToken string `json:"grant_token,omitempty"`
	// Selections is the approver's answer to Prompts. Present only on an
	// allowed decision for a call that carried prompts.
	Selections []approvalchoice.ChoiceSelection `json:"selections,omitempty"`
	// PromptsCompleteness says what the inbox did with the prompts that were
	// sent: "complete" carried them, "truncated" dropped them for exceeding the
	// operator's ceilings, empty means none were sent. A bridge that sent
	// prompts and reads "truncated" knows the approver never saw the options.
	PromptsCompleteness string `json:"prompts_completeness,omitempty"`
}

type approvalWireRequest struct {
	Origin             string                        `json:"origin,omitempty"`
	Session            string                        `json:"session,omitempty"`
	Runtime            string                        `json:"runtime,omitempty"`
	TaskID             string                        `json:"task_id,omitempty"`
	CatalogSessionID   string                        `json:"catalog_session_id,omitempty"`
	NativeSessionID    string                        `json:"native_session_id,omitempty"`
	ToolCallID         string                        `json:"tool_call_id,omitempty"`
	ToolName           string                        `json:"tool_name,omitempty"`
	Rule               string                        `json:"rule,omitempty"`
	Mode               string                        `json:"mode,omitempty"`
	Message            string                        `json:"message,omitempty"`
	Command            string                        `json:"command,omitempty"`
	Summary            string                        `json:"summary,omitempty"`
	Action             string                        `json:"action,omitempty"`
	Targets            []string                      `json:"targets,omitempty"`
	ApprovalReason     string                        `json:"approval_reason,omitempty"`
	OfferExactRunGrant bool                          `json:"offer_exact_run_grant,omitempty"`
	GrantToken         string                        `json:"grant_token,omitempty"`
	Prompts            []approvalchoice.ChoicePrompt `json:"prompts,omitempty"`
	FiredTags          []string                      `json:"fired_tags,omitempty"`
	Boundary           string                        `json:"boundary,omitempty"`
	TimeoutMS          int                           `json:"timeout_ms"`
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
		Action: in.Action, Targets: in.Targets, ApprovalReason: in.ApprovalReason,
		OfferExactRunGrant: in.OfferExactRunGrant, GrantToken: in.GrantToken,
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

// ReleaseRuntimeApprovalGrantFromDataDir invalidates one opaque run capability.
// It is best-effort cleanup for the owning runtime protocol; a later run cannot
// reuse the token because it never receives it and task identity is also checked.
func ReleaseRuntimeApprovalGrantFromDataDir(ctx context.Context, dataRoot, grantToken string) error {
	if grantToken == "" {
		return nil
	}
	addr, token, ok := daemonEndpointFromDataDir(dataRoot)
	if !ok {
		return errors.New("approval daemon unavailable")
	}
	body, _ := json.Marshal(map[string]string{"grant_token": grantToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+"/api/v1/approvals/grant/revoke", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-CG-Token", token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("approval daemon refused grant release: HTTP %d", resp.StatusCode)
	}
	return nil
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
	}, time.Duration(budgetMs)*time.Millisecond+askClientSlack(time.Duration(budgetMs)*time.Millisecond), daemonEndpoint)
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

// askClientSlack is how long the HTTP client waits past a prompt's budget for the
// inbox's own expiry answer: AskClientSlack, capped to what is left before the
// runtime's hook timeout so a hung daemon cannot carry the wait past it.
func askClientSlack(budget time.Duration) time.Duration {
	left := RuntimeHookTimeout - time.Since(hookStarted) - budget
	if left < 0 {
		return 0
	}
	return min(AskClientSlack, left)
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
		in, deliveries := observeDecision("allow", "WOULD BLOCK ("+reason+") — enforcement off: "+why)
		fmt.Fprintf(os.Stderr, "[crossing-guard] enforcement OFF — allowed an action that would be blocked: %s\n", reason)
		// This is an allow boundary, so the daemon may have handed it pending
		// helper messages and recorded them delivered. Print them exactly as
		// exitAllow does — context only, never a decision key — or they are lost.
		printHookContext(in, deliveries)
		return // no decision key: the tool proceeds
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
	// TurnKind is an optional boundary established by a runtime's native decoder.
	TurnKind string `json:"-"`
	// Carrier says this invocation can print injected context into the
	// session's own conversation: the governed lane when the installer
	// publishes a context encoder for this event and does not report the
	// invocation as inside a sub-agent, the collection lane when its plugin
	// captured stdout (--carrier). It rides every envelope so the daemon hands over
	// pending helper messages only where they can land.
	Carrier bool `json:"-"`
	// AgentID is the payload's nested-agent id. Its meaning belongs to the
	// runtime's installer (HookSubagentReporter), never to this code.
	AgentID        string `json:"-"`
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
	ActionID string `json:"-"`
	// Rule is staged by stageRule once a rule produces or asks for this action's
	// decision; every observation flushed afterwards — the ask, then its resolution —
	// carries it, so the rule is a field and no longer only prose inside the reason.
	Rule string `json:"-"`
	// Layer is staged beside Rule (schema 38): the distribution tier the staged rule
	// arrived by — user, repository, or organization. The same staging reasoning; the
	// tier that DECIDED rides with the rule that decided.
	Layer            string          `json:"-"`
	LayerReasons     string          `json:"-"`
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
		AgentID        json.RawMessage `json:"agent_id"`
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
		// Raw so a value of the wrong shape cannot fail the whole payload.
		NotificationType json.RawMessage `json:"notification_type"`
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
	// Only a JSON string is an agent id. Any other shape is ignored rather than
	// failing the whole payload, which would let every call through unjudged.
	var agentID string
	if json.Unmarshal(wire.AgentID, &agentID) == nil {
		in.AgentID = agentID
	}
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
	// Only a JSON string is a sub-type. It is provenance, so any other shape is
	// ignored: failing the payload would drop the turn it carries (and, were
	// the field ever sent on a tool call, let that call through unjudged).
	var notificationType string
	if json.Unmarshal(wire.NotificationType, &notificationType) == nil {
		in.NotificationType = notificationType
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

// engineRules is what the hook's engine tier decides over, and which rule sets that
// includes.
type engineRules struct {
	Policy  *engine.Policy // nil: the engine tier does not run
	Reasons []string
	// User, Team, Invocation: the engine tier's policy includes the user rulebook's
	// rules, this checkout's team layers, the invocation file's rules.
	User, Team, Invocation bool
}

// hookEngineRules is the one statement of which rules the hook's engine tier loads,
// shared by the tier itself (engineDecision) and `coverage`, so the label cannot drift
// from the evaluator. It runs only when the invocation file loads. The team layers
// concatenate onto the user policy (item 3c): this checkout's repository layer ++ the
// organization layer, each stamped with its tier, the load reasons riding every
// observation. The user layer never depends on team parsing: a layered load error
// leaves the invocation file deciding alone, and the reasons say so.
//
// An invocation file with rules is the COMPATIBILITY layer some installs ride beside the
// user document (postwork M-1's fix): its rules go FIRST, the loader's TEAM rules
// (repository ++ organization, known from the layered load's own user-prefix count —
// never a second Load) follow. The user document's own rules are NOT re-appended: the
// loader already carried them, and doubling them would let a repeated ask outrank the
// user's deny. Ties keep the first occurrence. When the invocation file IS the user
// document, its rules are the user rules. With zero rules the layered policy decides.
func hookEngineRules(supplement *rulebook.InvocationPolicy, supplementErr error, layered rulebook.LayeredPolicy, layeredErr error) engineRules {
	switch {
	case supplementErr != nil || supplement == nil || !supplement.Available:
		return engineRules{}
	case layeredErr != nil:
		return engineRules{Policy: supplement.Policy, Invocation: true,
			Reasons: []string{"team layers could not load: " + layeredErr.Error()}}
	case supplement.Policy != nil && len(supplement.Policy.Rules) > 0:
		teamRules := append([]engine.Rule{}, layered.Policy.Rules[layered.UserRuleCount:]...)
		combined := *layered.Policy
		combined.Rules = append(append([]engine.Rule{}, supplement.Policy.Rules...), teamRules...)
		return engineRules{Policy: &combined, Reasons: layered.Reasons, Invocation: true, Team: true,
			User: isRulebookFile(supplement, layered.UserPath, layered.UserDigest)}
	default:
		return engineRules{Policy: layered.Policy, Reasons: layered.Reasons, User: true, Team: true}
	}
}

// hookAction is the one action a hook invocation judges: its tags, built once and
// decided over by every static tier (engine.ActionTags).
type hookAction struct {
	tags []engine.Tag
	// dets is nil when the detector document could not load. The tags are then the
	// invocation's own facts alone and unknown says so, so a rule reading a detector
	// fact is left undecided rather than judged over a tag set known to be incomplete.
	dets    []engine.Detector
	unknown engine.Unknown
}

// hookContent is the body of a write-shaped call: the Write content, else the Edit
// replacement. The hook classifies it and sends it to the stateful consult.
func hookContent(in hookInput) string {
	if in.ToolInput.Content != "" {
		return in.ToolInput.Content
	}
	return in.ToolInput.NewString
}

// actionOf builds the action's tags for the rule sets that will decide it. It
// classifies only with the detectors whose facts those rules read: a rulebook of
// command and tool rules (the shipped one) loads no detector document and scans no
// write body, and a rule on one fact costs that fact's detectors alone.
func actionOf(in hookInput, pols ...*engine.Policy) hookAction {
	command := string(in.ToolInput.Command)
	if !engine.ReadsDetectorFacts(pols...) {
		return hookAction{tags: engine.InvocationTags(in.ToolName, command), dets: []engine.Detector{}}
	}
	loaded, err := activeDetectorDocument()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] detectors unloadable; rules reading detector facts are not decided:", err)
		return hookAction{tags: engine.InvocationTags(in.ToolName, command), unknown: engine.UnknownWithoutDetectors}
	}
	ev := engine.ActionEvent(in.ToolName, command, hookContent(in), in.ToolInput.FilePath, in.ToolInput.URL)
	dets := engine.DetectorsRead(loaded.Detectors, pols...)
	return hookAction{tags: engine.ActionTags(ev, dets, command), dets: loaded.Detectors}
}

// commandWatermark is the water mark an engine-tier log line carries: the highest data
// class of this call's command, path and destination under the whole detector library —
// what the line has always recorded, whichever detectors the rules themselves read.
func commandWatermark(in hookInput) string {
	loaded, err := activeDetectorDocument()
	if err != nil {
		return ""
	}
	ev := engine.ActionEvent(in.ToolName, string(in.ToolInput.Command), "", in.ToolInput.FilePath, in.ToolInput.URL)
	return engine.WaterMark(engine.Classify(ev, loaded.Detectors))
}

// undecidedReason is the staged reason a tier adds when it left gating rules undecided.
func undecidedReason(tier string, st Static) []string {
	if st.Undecided == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%s: %d deny/ask rule(s) not decided (a term reads a fact unavailable to this call)", tier, st.Undecided)}
}

// loadEngineTier loads the hook's engine tier's rule set: the invocation file's rules
// and the layers hookEngineRules joins to them. ok is false when there is no invocation
// file — the engine is additive, and a missing file means "the engine has nothing to
// say," not fail-closed.
func loadEngineTier(in hookInput) (engineRules, bool) {
	supplement, err := rulebook.LoadInvocationPolicy()
	if err != nil || !supplement.Available {
		return engineRules{}, false
	}
	// The team layers concatenate onto the user policy (item 3c): this checkout's
	// repository layer ++ the organization layer, each stamped with its tier, the
	// load reasons riding every observation. The user layer never depends on team
	// parsing: a layers load error leaves the user policy deciding, and the
	// reasons say so.
	layered, layerLoadErr := rulebook.LoadLayeredFull(in.Cwd, dataDir())
	return hookEngineRules(supplement, nil, layered, layerLoadErr), true
}

// engineDecision decides the engine tier's rule set over the action's tags.
// Session-scoped predicates need the daemon's stateful tier; here the decision is over
// THIS action's tags only.
func engineDecision(loaded engineRules, act hookAction) (Static, *engine.RuleBoundary) {
	st := StaticDecide(act.tags, loaded.Policy, act.unknown)
	stageLayerReasons(loaded.Reasons)
	// the compiled honest label for the fired rule (P-COMPILE-2) rides
	// along so the decision log carries boundary = detection ∧ reach
	var boundary *engine.RuleBoundary
	// allow decisions carry no rule ("" must not bind an id-less rule); with no detector
	// document a label would read INERT for a rule that just fired.
	if st.Decision.Rule != "" && act.unknown == nil {
		dets := act.dets
		if len(dets) == 0 { // no rule read a detector fact: the label still names every producer
			if loadedDets, err := activeDetectorDocument(); err == nil {
				dets = loadedDets.Detectors
			}
		}
		for _, r := range st.Policy.Rules {
			if r.ID == st.Decision.Rule {
				// The hook reads no store on its hot path: the direct-fact catalog is
				// known, model claims are not (an agent: term reads unverified).
				// The engine tier fired it, on its own tags: label that tier alone.
				b := engine.BoundaryOn(r, dets, store.StateProducersFor(nil, 0), engine.ReachStop, engine.TierEngine)
				boundary = &b
				break
			}
		}
	}
	return st, boundary
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

// stageRule records which rule is producing, or asking for, the staged action's decision.
// It rides beside the staged action (as pendingDenyRuntime does) because every decision
// funnel takes prose only; threading an id through fifteen signatures would be the same
// fact with more places to forget it. A warn-and-proceed rule is deliberately NOT staged:
// it does not produce the decision, and the tier after it may.
func stageRule(ruleID string) {
	if pendingObserve != nil {
		pendingObserve.Rule = ruleID
	}
}

// stageLayerReasons records the layered loader's notes (a expired layer, one that
// failed to parse, a checkout whose repository layer is not yet staged) so every
// observation of this action carries WHY the team layers did or did not apply.
func stageLayerReasons(reasons []string) {
	if pendingObserve == nil || len(reasons) == 0 {
		return
	}
	// The reasons ride their OWN staged fact, never the tier field (postwork
	// M-4): the tier is Layer's enum, and storableLayer drops anything else, so
	// a reasons string there was silently discarded.
	pendingObserve.LayerReasons = strings.Join(reasons, "; ")
}

// addStagedReasons appends to the staged reasons instead of replacing them: what a
// tier could not decide rides beside why the team layers did or did not apply.
func addStagedReasons(groups ...[]string) {
	if pendingObserve == nil {
		return
	}
	for _, reasons := range groups {
		for _, r := range reasons {
			if pendingObserve.LayerReasons != "" {
				pendingObserve.LayerReasons += "; "
			}
			pendingObserve.LayerReasons += r
		}
	}
}

// stageLayer records the distribution tier the staged rule arrived by (schema 38).
// Always called beside stageRule: the tier that decided rides with the rule that
// decided, on the ask, the resolution, and every would-block record.
func stageLayer(layer string) {
	if pendingObserve != nil {
		pendingObserve.Layer = layer
	}
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
// the daemon hands it — and whether that context would reach the session's own
// conversation. A hook that fires inside a sub-agent names the parent session,
// but anything it prints lands in the sub-agent's separate conversation, so it
// never carries a message addressed to the session (subagent-carrier-boundary
// plan).
func hookCanCarry(in hookInput) bool {
	installer := hookInstallers[in.Runtime]
	if reporter, ok := installer.(HookSubagentReporter); ok && reporter.HookRunsInSubagent(in.AgentID) {
		return false
	}
	encoder, ok := installer.(HookContextEncoder)
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
			joined += HookContextJoin
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
	layer    string // the distribution tier the deciding rule arrived by; "" = unknown
}

// consultStateful asks the daemon to decide over live session/target state (Phase 3).
// It returns a verdict ONLY when the daemon evaluated the stateful tier AND a rule
// denied/asked, and beside it how many gating rules the daemon could not decide. Every failure path — no daemon, timeout, non-200, decode error,
// allow, or not-evaluated — returns nil, which the caller treats as "proceed". That is
// the fail-open guarantee: a daemon problem can never turn into a blocked tool call.
func consultStateful(in hookInput) (*statefulVerdict, int) {
	addr, tok, ok := daemonEndpoint()
	if !ok {
		return nil, 0 // no daemon → fail open
	}
	body, _ := json.Marshal(map[string]any{
		"session":    in.SessionID,
		"tool":       in.ToolName,
		"command":    string(in.ToolInput.Command),
		"content":    hookContent(in),
		"file_path":  in.ToolInput.FilePath,
		"file_paths": structuredFilePaths(in),
		"cwd":        in.Cwd,
		"url":        in.ToolInput.URL,
		"skill":      in.ToolInput.Skill,
		"runtime":    in.Runtime,
	})
	req, err := http.NewRequest("POST", "http://"+addr+"/api/govern/decide", bytes.NewReader(body))
	if err != nil {
		return nil, 0
	}
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("X-CG-Token", tok)
	}
	resp, err := (&http.Client{Timeout: statefulTimeout}).Do(req)
	if err != nil {
		return nil, 0 // unreachable / timeout → fail open
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0
	}
	var d struct {
		Decision, Rule, Message, Reason, Layer string
		Evaluated                              bool
		Undecided                              int
	}
	if json.NewDecoder(resp.Body).Decode(&d) != nil {
		return nil, 0
	}
	if !d.Evaluated || d.Decision == "" || d.Decision == "allow" {
		return nil, d.Undecided // nothing stateful fired → proceed
	}
	return &statefulVerdict{decision: d.Decision, rule: d.Rule, message: d.Message, reason: d.Reason, layer: d.Layer}, d.Undecided
}

// readHookInput decodes one hook payload the way the installed command for runtime
// does: through the runtime's own decoder when it has one. The runtime is the
// installer's flag, never a guess from the payload; whether this invocation can carry
// injected context is decided here, once, from the decoded payload.
func readHookInput(payload io.Reader, runtime, event string) (hookInput, error) {
	var in hookInput
	var err error
	if decoder, ok := hookInstallers[runtime].(HookInputDecoder); ok {
		in, err = decoder.DecodeHookInput(payload, event)
	} else {
		err = json.NewDecoder(payload).Decode(&in)
	}
	if err != nil {
		return hookInput{}, err
	}
	in.Runtime = runtime
	in.Carrier = hookCanCarry(in)
	return in, nil
}

func cmdHook(args []string) {
	// WHICH runtime invoked us. The installer wrote this into the command, so it is
	// a fact about the wiring rather than a guess about the payload. Absent (a hook
	// installed before the flag existed) stays EMPTY and is recorded as unknown —
	// never defaulted to a vendor, which would attribute one agent's actions to
	// another in the only record that a blocked action ever existed.
	in, decodeErr := readHookInput(os.Stdin, flagValue(args, "--runtime"), flagValue(args, "--event"))
	if decodeErr != nil {
		// FAIL-OPEN, on purpose: a payload we cannot parse is most likely a vendor
		// format change, and denying every tool call on a parse error would brick
		// the agent (the same reasoning as the stateful consult's fail-open). But
		// open must not also be SILENT — this line on stderr is the only trace that
		// every call is sailing past an un-parsing guard.
		fmt.Fprintln(os.Stderr, "[crossing-guard] hook payload did not parse — allowing without judging:", decodeErr)
		os.Exit(0)
	}
	// A turn-boundary hook carries OUR kind in its own command line, so this
	// dispatch never has to know the provider's name for it (decision 1, 2026-09-01).
	kind := flagValue(args, "--observe")
	if kind == "" {
		kind = in.TurnKind
	}
	if kind != "" {
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
	command := string(in.ToolInput.Command)
	// DECIDE, THEN ASK ONCE. Both static tiers are decided over the one action tag set
	// before anything is emitted: the engine tier (the invocation file's rule set, when
	// there is one) and the standalone tier (the user rulebook ++ this checkout's
	// repository layer ++ the organization layer). Session-scoped accumulation lives in
	// the stateful tier, consulted from the one daemon after them.
	//
	// A missing rules.json is not "unconfigured": rulebook.Load falls back to the SHIPPED
	// default, so a fresh install is governed by sane rules rather than denying every
	// tool call (which bricked the agent) or allowing everything. A present-but-broken
	// file fails CLOSED.
	engineSet, engineRan := loadEngineTier(in)
	pol, layerReasons, err := rulebook.LoadLayered(in.Cwd, dataDir())
	if err != nil {
		emitDeny("crossing-guard could not load its rules; failing closed")
		os.Exit(0)
	}
	act := actionOf(in, engineSet.Policy, pol)
	var eng Static
	var boundary *engine.RuleBoundary
	if engineRan {
		eng, boundary = engineDecision(engineSet, act)
	}
	alone := StaticDecide(act.tags, pol, act.unknown)
	// The load reasons (expiry, unloadable, not-yet-staged) ride every observation's
	// reason so a person sees why a team rule did or did not apply.
	stageLayerReasons(layerReasons)
	addStagedReasons(undecidedReason("engine tier", eng), undecidedReason("standalone tier", alone))

	d := eng.Decision
	observed := map[string]any{"rule": d.Rule, "mode": string(d.Mode),
		"fired_tags": tagSummary(d.FiredTags), "session": in.SessionID, "via": "engine-local"}
	if engineRan && d.Decision != "allow" {
		observed["watermark"] = commandWatermark(in)
	}
	if boundary != nil {
		observed["boundary"] = boundary.Label
	}
	gd := alone.Decision

	// 1. A hard block from either tier denies at once: nothing is asked first, so a
	// confirm can never be given for a call a non-overridable rule stops.
	if engineRan && d.Mode == engine.HardBlock {
		stageRule(d.Rule)
		stageLayer(string(d.Layer))
		logLine(mergeMap(observed, map[string]any{"decision": "deny",
			"note": "hard-block, non-overridable"}))
		emitDeny(fmt.Sprintf("Blocked by rule %s (Restricted, non-overridable): %s. "+
			"Tags: %s.", d.Rule, d.Message, tagSummary(d.FiredTags)))
		os.Exit(0)
	}
	if gd.Mode == engine.HardBlock {
		stageRule(gd.Rule)
		stageLayer(string(gd.Layer))
		logLine(map[string]any{"command": command, "rule": gd.Rule, "guard": gd.Rule,
			"mode": string(gd.Mode), "decision": "deny", "prompt": "auto"})
		emitDeny(staticDenialReason(gd))
		os.Exit(0)
	}

	// 2. Warns. Each rule that warned is logged once, from whichever tier saw it first,
	// and named in the allow reason; a warn never stages a rule or ends the pipeline.
	var warned []string
	if engineRan && d.Mode == engine.WarnAndProceed {
		logLine(mergeMap(observed, map[string]any{"decision": "allow",
			"note": "warn-and-proceed"}))
		warned, _ = noteWarn(warned, d.Rule)
	}
	for _, rule := range engine.FiredWarns(gd, alone.Policy) {
		var fresh bool
		if warned, fresh = noteWarn(warned, rule); fresh {
			logLine(map[string]any{"command": command, "rule": rule, "guard": rule,
				"mode": string(engine.WarnAndProceed), "decision": "allow",
				"note": "warn-and-proceed", "tier": "static", "all_fired": gd.AllFired})
		}
	}

	// 3. ONE prompt for every confirm-class rule that fired, engine tier first. The
	// inbox request carries one rule — the first, which is also the one staged — and the
	// message names the others. Allow does NOT un-taint; it is an attributed override.
	confirmed := ""
	engineAsks := engineRan && d.Mode == engine.ConfirmAndRecord
	if engineAsks || gd.Mode == engine.ConfirmAndRecord {
		var asking []string
		if engineAsks {
			asking = ruleIDs(eng.Seen.Asking())
		}
		if gd.Mode == engine.ConfirmAndRecord {
			for _, id := range ruleIDs(alone.Seen.Asking()) {
				if !slices.Contains(asking, id) {
					asking = append(asking, id)
				}
			}
		}
		also := alsoAsking(asking[1:])
		var allowed bool
		var mode, reason string
		if engineAsks {
			stageRule(d.Rule)
			stageLayer(string(d.Layer))
			bLabel := ""
			if boundary != nil {
				bLabel = boundary.Label
			}
			allowed, mode, reason = askHuman(in.SessionID, in.Runtime, d.Rule, string(d.Mode),
				d.Message+also, command, tagSummary(d.FiredTags), bLabel)
		} else {
			stageRule(gd.Rule)
			stageLayer(string(gd.Layer))
			allowed, mode, reason = askHuman(in.SessionID, in.Runtime, gd.Rule, "ask",
				"Guarded operation ("+gd.Rule+")"+also, command, gd.Rule, "")
		}
		decision := "deny"
		if allowed {
			decision = "allow"
		}
		// One log line per tier that asked, each in its own shape.
		if engineAsks {
			claimed := "(dialog-button; no typed reason)"
			if reason != "" {
				claimed = reason + " (ATTRIBUTED, NOT VERIFIED)"
			}
			logLine(mergeMap(observed, map[string]any{"decision": decision,
				"override": mode, "claimed": claimed, "untainted": false, "asked": asking}))
		}
		if gd.Mode == engine.ConfirmAndRecord {
			entry := map[string]any{"command": command, "rule": gd.Rule, "guard": gd.Rule,
				"mode": string(gd.Mode), "decision": decision, "prompt": mode, "asked": asking}
			if reason != "" {
				entry["claimed"] = reason + " (ATTRIBUTED, NOT VERIFIED)"
			}
			logLine(entry)
		}
		if !allowed {
			if engineAsks {
				emitDeny(fmt.Sprintf("Blocked by rule %s: %s. Tags: %s. Confirm to override "+
					"(recorded, does not un-taint the session).", d.Rule, d.Message,
					tagSummary(d.FiredTags)))
			} else {
				emitDeny(staticDenialReason(gd))
			}
			os.Exit(0)
		}
		confirmed = "confirmed by user"
		if engineAsks {
			confirmed = "override: " + mode
		}
	}

	// 4. The STATEFUL tier (Phase 3) — decisions over the session's/target's live state
	// that only the daemon holds — is consulted after every static allow AND after every
	// confirmed static ask: a confirm never skips a state rule. FAIL-OPEN: a nil verdict
	// (no daemon, timeout, nothing fired) means proceed, so a daemon problem never
	// blocks a tool call.
	sv, statefulUndecided := consultStateful(in)
	if sv != nil {
		enforceStateful(in, sv, command, confirmed) // never returns
	}
	reason := proceedReason(warned) // nothing gated: proceed, naming any warns
	if confirmed != "" {
		reason = confirmed
		if len(warned) > 0 { // a warn beside a confirmed ask is still named
			reason += "; " + proceedReason(warned)
		}
	}
	if statefulUndecided > 0 {
		reason += fmt.Sprintf("; %d stateful deny/ask rule(s) not decided (this call has no single target)", statefulUndecided)
	}
	exitAllow(reason)
}

func ruleIDs(rules []engine.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.ID)
	}
	return out
}

// alsoAsking names the other confirm-class rules a single prompt covers, bounded so the
// inbox's message limit is never the reason an ask fails closed.
func alsoAsking(rest []string) string {
	const shown = 3
	if len(rest) == 0 {
		return ""
	}
	rest = slices.Clone(rest)
	more := ""
	if len(rest) > shown {
		more = fmt.Sprintf(" (+%d more)", len(rest)-shown)
		rest = rest[:shown]
	}
	for i, id := range rest {
		if len(id) > 96 {
			rest[i] = id[:96] + "…"
		}
	}
	return " Also asking: " + strings.Join(rest, ", ") + more + "."
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
// and enforcement-switch rules. Nothing returns: a deny exits through emitDeny and a
// confirmed ask exits through exitAllow naming the rule. confirmed is the static
// tiers' own override when one was already given for this call; the prompt here takes
// what is left of the invocation's one approval budget (askHuman).
func enforceStateful(in hookInput, sv *statefulVerdict, command, confirmed string) {
	stageRule(sv.rule)
	stageLayer(string(engine.Layer(sv.layer)))
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
			// The resolution names its rule in prose as well as in the staged field, so the
			// stored row never reads "no rule matched" beside a rule_id.
			reason := "confirmed by user (stateful rule " + sv.rule + ")"
			if confirmed != "" {
				reason += "; static tier: " + confirmed
			}
			exitAllow(reason)
		}
		emitDeny(fmt.Sprintf("Blocked by stateful rule %s: %s. Confirm to override (recorded).", sv.rule, msg))
		os.Exit(0)
	}
	logLine(map[string]any{"command": command, "rule": sv.rule, "tier": "stateful", "decision": "deny"})
	emitDeny(fmt.Sprintf("Blocked by stateful rule %s: %s. %s", sv.rule, msg, sv.reason))
	os.Exit(0)
}

// noteWarn records that rule warned on this action; fresh is false when an earlier
// tier already warned it, so the caller does not log the same warn twice.
func noteWarn(warned []string, rule string) (_ []string, fresh bool) {
	if slices.Contains(warned, rule) {
		return warned, false
	}
	return append(warned, rule), true
}

// proceedReason is the recorded reason of an action no tier stopped: the warned
// rules by id, so the stored row never reads "no rule matched" after a warn fired.
func proceedReason(warned []string) string {
	if len(warned) == 0 {
		return "no rule matched"
	}
	return "warn-and-proceed: rule " + strings.Join(warned, ", ")
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
	dets, detErr := ActiveDetectors()
	if detErr != nil {
		fmt.Fprintln(os.Stderr, "detectors unloadable; rules reading detector facts are not decided:", detErr)
		dets = nil
	}
	v, unjudged, err := CheckCommandStatic(command, dets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	switch v.Decision {
	case "allow":
		fmt.Printf("allow    (no rule matched)   %q\n", command)
	case "deny":
		fmt.Printf("DENY     %s   %q\n", v.Rule, command)
	default:
		fmt.Printf("ASK      %s   %q\n", v.Rule, command)
	}
	if unjudged.StateRules > 0 {
		fmt.Printf("note: %d deny/ask state rule(s) not evaluated here — the daemon's stateful tier decides them\n", unjudged.StateRules)
	}
	if unjudged.Undecided > 0 {
		why := "they read the tool, a path or a destination, and a command preview names none"
		if dets == nil {
			why = "they read a detector fact or the tool, and the detector document did not load"
		}
		fmt.Printf("note: %d deny/ask rule(s) could not be decided here — %s\n", unjudged.Undecided, why)
	}
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
			// A bare event names no invocation: a command or tool term is undecided here.
			st := StaticDecide(tags, pol, engine.UnknownInvocation)
			d, static, excluded := st.Decision, st.Policy, st.StateRules
			fmt.Printf("decision: %s", strings.ToUpper(d.Decision))
			if d.Rule != "" {
				fmt.Printf("  [%s] rule=%s — %s", d.Mode, d.Rule, d.Message)
			}
			fmt.Println()
			if excluded > 0 {
				// This file is the invocation policy, which the daemon never reads —
				// so unlike `check`, no tier evaluates these (a known limitation).
				fmt.Printf("note: %d deny/ask state rule(s) in this policy are not evaluated by any tier (the daemon's stateful tier reads only the user rulebook)\n", excluded)
			}
			if st.Undecided > 0 {
				fmt.Printf("note: %d deny/ask rule(s) could not be decided here — they read the raw command or the tool identity, which a tags dry run does not have\n", st.Undecided)
			}
			for _, m := range d.Menu {
				flag := "•"
				if !m.Live {
					flag = "×"
				}
				fmt.Printf("    [%s] %-20s %s\n", flag, m.Action, m.Why)
			}
			if d.Rule != "" { // allow decisions carry no rule
				for _, r := range static.Rules {
					if r.ID == d.Rule {
						b := engine.Boundary(r, dets, store.StateProducersFor(nil, 0), engine.ReachDryRun)
						fmt.Printf("boundary: %s\n", b.Label)
						break
					}
				}
			}
		}
	}
}

// cmdCoverage prints the compiled honest label for every rule the live tiers load
// (P-COMPILE-2): boundary = detection-coverage ∧ enforcement-reach, derived
// mechanically — never asserted.
func cmdCoverage() {
	cwd, _ := os.Getwd()
	if err := writeCoverage(os.Stdout, cwd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// writeCoverage labels the rule sets the live tiers load, each at the reach of the
// tiers that load it (stateful-tier reach plan D-4): the active rulebook as the hook's
// standalone tier layers it for cwd — its user rules are also the daemon's stateful
// tier's, armed or not by this platform's capability record — then, when this
// environment resolves one, the invocation compatibility file, which only the hook's
// engine tier loads. A rulebook that cannot load is an error: the hook fails closed on
// it. An unreadable invocation file is a note: no tier evaluates it.
func writeCoverage(w io.Writer, cwd string) error {
	loadedDetectors, err := activeDetectorDocument()
	if err != nil {
		return fmt.Errorf("load detectors: %w", err)
	}
	dets := loadedDetectors.Detectors
	doc, err := rulebook.LoadDocument()
	if err != nil {
		return fmt.Errorf("load rulebook: %w", err)
	}
	layered, err := rulebook.LoadLayeredFull(cwd, dataDir())
	if err != nil {
		return fmt.Errorf("load rulebook: %w", err)
	}
	sp := coverageStateProducers()
	invocation, invocationErr := rulebook.LoadInvocationPolicy()
	hookEngine := hookEngineRules(invocation, invocationErr, layered, nil)
	user := engine.LiveRuleSet{Kind: engine.SetUser, Tier: engine.StatefulUnarmed, HookEngine: engineLoad(hookEngine.User)}
	armed := "NOT armed on this platform (" + runtime.GOOS + ")"
	if platform.Current().StatefulEnforcementReady() {
		user.Tier, armed = engine.StatefulArmed, "armed on this platform — evaluated by the daemon when it runs and resolves this same rulebook (same HOME / CG_RULES)"
	}
	fmt.Fprintf(w, "rulebook %s (%s, digest %s)\n", doc.Path, doc.Origin, doc.Digest)
	fmt.Fprintf(w, "  stateful tier: %s\n", armed)
	fmt.Fprintf(w, "  hook engine tier: %s\n", hookEngineStatus(invocation, invocationErr, hookEngine.User, false))
	for _, reason := range layered.Reasons {
		fmt.Fprintf(w, "  team layers: %s\n", reason)
	}
	rules := layered.Policy.Rules
	if layered.UserRuleCount == 0 {
		fmt.Fprintln(w, "  no rules")
	}
	writeBoundaries(w, engine.CompileLiveBoundaries(&engine.Policy{Rules: rules[:layered.UserRuleCount]}, dets, sp, user))
	if team := rules[layered.UserRuleCount:]; len(team) > 0 {
		// The stateful tier loads the adopted team layers for a session in this
		// checkout (stateful-tier-layered-policy plan), so they sit at the user
		// rulebook's tier; the audit still loads only the user rulebook.
		fmt.Fprintf(w, "team layers for %s (the stateful tier loads them for sessions in this checkout)\n", cwd)
		fmt.Fprintf(w, "  hook engine tier: %s\n", hookEngineStatus(invocation, invocationErr, hookEngine.Team, true))
		writeBoundaries(w, engine.CompileLiveBoundaries(&engine.Policy{Rules: team}, dets, sp,
			engine.LiveRuleSet{Kind: engine.SetTeam, Tier: user.Tier, HookEngine: engineLoad(hookEngine.Team)}))
	}
	switch {
	case invocationErr != nil:
		fmt.Fprintf(w, "invocation file %s unreadable: %v — no tier evaluates it\n", rulebook.InvocationPolicyPath(), invocationErr)
	case !invocation.Available:
	case isRulebookFile(invocation, doc.Path, doc.Digest):
		fmt.Fprintf(w, "invocation file %s (%s) is the rulebook itself — labeled above\n", invocation.Path, invocation.Origin)
	default:
		fmt.Fprintf(w, "invocation file %s (%s; only the hook's engine tier loads it, for hooks that resolve this same file)\n",
			invocation.Path, invocation.Origin)
		writeBoundaries(w, engine.CompileLiveBoundaries(invocation.Policy, dets, sp,
			engine.LiveRuleSet{Kind: engine.SetInvocation}))
	}
	return nil
}

// engineLoad maps whether the hook's engine tier includes a rule set, in this process's
// environment, onto the label's input.
func engineLoad(loads bool) engine.EngineLoad {
	if loads {
		return engine.EngineLoads
	}
	return engine.EngineSkips
}

// hookEngineStatus is the section header line saying whether the hook's engine tier
// loads a rule set here, and why — the invocation file this environment resolves. The
// engine tier loads the team layers whenever that file loads, and the user rulebook only
// when it has no rules or is the rulebook itself (hookEngineRules).
func hookEngineStatus(inv *rulebook.InvocationPolicy, invErr error, loads, team bool) string {
	set, when := "this rulebook", "only when it has no rules or is the rulebook itself"
	if team {
		set, when = "the team layers", "whenever it loads"
	}
	switch {
	case invErr != nil:
		return "does not run here — invocation file " + rulebook.InvocationPolicyPath() + " is unreadable"
	case !inv.Available:
		return "does not run here — no invocation file at " + inv.Path + " (" + inv.Origin + "); a hook that resolves one loads " + set + " " + when
	case loads:
		return "loads " + set + " here — invocation file " + inv.Path + " (" + inv.Origin + ")"
	default:
		return "does not load " + set + " here — invocation file " + inv.Path + " (" + inv.Origin + ") has its own rules"
	}
}

// isRulebookFile reports whether the invocation file is the file the user rulebook
// loaded its rules from: the same file AND the same content. A path match alone is not
// enough — an unselected install reports the legacy path while deciding over the
// baseline (postwork PW-5).
func isRulebookFile(inv *rulebook.InvocationPolicy, userPath, userDigest string) bool {
	return inv.Digest != "" && inv.Digest == userDigest && sameFile(inv.Path, userPath)
}

// sameFile reports whether two paths name one file, resolving relative paths and links.
func sameFile(a, b string) bool {
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
}

// writeBoundaries prints each rule's label and its per-term coverage rows.
func writeBoundaries(w io.Writer, bs []engine.RuleBoundary) {
	for _, b := range bs {
		fmt.Fprintf(w, "%-24s [%s]  %s\n", b.Rule, b.Mode, b.DetectionLabel)
		fmt.Fprintf(w, "    %s\n", b.Label)
		if len(b.Tiers) > 0 {
			var parts []string
			for _, v := range b.Tiers {
				part := v.Tier + ": " + v.DetectionLabel
				if v.Loads != "yes" {
					part += " (loads: " + v.Loads + ")"
				}
				parts = append(parts, part)
			}
			fmt.Fprintf(w, "    tiers: %s\n", strings.Join(parts, "; "))
		}
		for _, tc := range b.Detection {
			neg := ""
			if tc.Negated {
				neg = "NOT "
			}
			switch {
			case tc.Unverified:
				fmt.Fprintf(w, "    - %s%s=%s: UNVERIFIED — %s\n", neg, tc.Tag, tc.Value, tc.Note)
			case tc.Undetectable && tc.Note != "":
				fmt.Fprintf(w, "    - %s%s=%s: UNDETECTABLE — %s\n", neg, tc.Tag, tc.Value, tc.Note)
			case tc.Undetectable && len(b.Tiers) > 0:
				fmt.Fprintf(w, "    - %s%s=%s: UNDETECTABLE — no tier that loads this rule has a producer for it\n", neg, tc.Tag, tc.Value)
			case tc.Undetectable:
				fmt.Fprintf(w, "    - %s%s=%s: UNDETECTABLE — no detector or declared state producer emits this tag\n", neg, tc.Tag, tc.Value)
			case !tc.Enumerable:
				fmt.Fprintf(w, "    - %s%s=%s: UNKNOWABLE gaps  (producers: %s)%s\n", neg, tc.Tag, tc.Value, strings.Join(tc.Detectors, ", "), termTiers(tc))
				if len(tc.Limits) > 0 {
					fmt.Fprintf(w, "      known limits (not exhaustive): %s\n", strings.Join(tc.Limits, "; "))
				}
			default:
				fmt.Fprintf(w, "    - %s%s=%s: enumerable  (producers: %s; gaps: %s)%s\n",
					neg, tc.Tag, tc.Value, strings.Join(tc.Detectors, ", "), strings.Join(tc.Gaps, "; "), termTiers(tc))
			}
		}
	}
}

// termTiers is a term row's suffix naming the tiers in which the term has a producer.
func termTiers(tc engine.TermCoverage) string {
	if len(tc.Tiers) == 0 {
		return ""
	}
	return "  (tiers: " + strings.Join(tc.Tiers, ", ") + ")"
}

// coverageStateProducers reads the non-detector producers `coverage` labels with: the
// direct-fact catalog, and the model-claim producers from the governance store the
// hook's daemon writes (read-only, as `entities` reads it). The note names the store
// it read, so a label computed from a store that is not the daemon's is visible; when
// no store can be read, agent: terms stay unverified rather than a guessed INERT.
func coverageStateProducers() engine.StateProducers {
	path := store.IndexPath("", homeDir())
	if _, err := os.Stat(path); err != nil {
		sp := store.StateProducersFor(nil, 0)
		sp.AgentClaimsNote = "no governance store at " + path
		return sp
	}
	ix, err := store.OpenRO(path)
	if err != nil {
		sp := store.StateProducersFor(nil, 0)
		sp.AgentClaimsNote = "the governance store at " + path + " could not be opened: " + err.Error()
		return sp
	}
	defer ix.Close()
	sp := store.StateProducersFor(ix, time.Now().Unix())
	sp.AgentClaimsNote += " (" + path + ")"
	return sp
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
		if t.Key == engine.CommandTagKey {
			// The raw command is its own field of every log line and prompt; in this
			// comma-joined summary it would split into false tags at the inbox.
			continue
		}
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
		count, problems, err := lintDetectorFile(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "INVALID:", err)
			os.Exit(1)
		}
		for _, p := range problems {
			fmt.Println("  ✗ " + p)
		}
		if len(problems) > 0 {
			fmt.Fprintf(os.Stderr, "%d problem(s)\n", len(problems))
			os.Exit(1)
		}
		fmt.Printf("OK: %d detectors — all compile and declare coverage\n", count)
	default:
		fmt.Fprintln(os.Stderr, "usage: detectors [status | list | preview | select | unselect | init [path] | lint <file>]")
		os.Exit(1)
	}
}

// lintDetectorFile is `detectors lint`'s check, without the printing and exit: a load
// error (bad JSON, bad regex, a reserved state-namespace tag key — engine.CompileDetectors)
// fails the whole document; otherwise each missing id or undeclared coverage is a problem.
func lintDetectorFile(path string) (int, []string, error) {
	dets, err := engine.LoadDetectors(path)
	if err != nil {
		return 0, nil, err
	}
	var problems []string
	for _, d := range dets {
		if d.ID == "" {
			problems = append(problems, "a detector has no id (the merge key)")
		}
		if d.Disabled {
			continue // a tombstone is just id+disabled; it declares nothing else
		}
		if !d.Coverage.Enumerable && len(d.Coverage.Gaps) == 0 {
			problems = append(problems, d.ID+": declares no coverage (not enumerable, no gaps)")
		}
	}
	return len(dets), problems, nil
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
