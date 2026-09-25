package guardcli

// The OpenCode decision lane (natural-session plan, Slice C): a decision-
// capable verb for the OpenCode plugin's tool.execute.before hook. It reuses
// the ONE evaluator (static engine tier + stateful consult) that Claude/Codex
// hooks use — no second rule engine, no second policy format.
//
// Output contract (stdout, one JSON object):
//   {"decision":"allow"|"deny","reason":"...","error":"..."}
//
// Honest failure model (owner rule: TRY and report, never silently skip):
//   - An internal failure (rules unloadable, evaluator error) records a typed
//     governance error to stderr AND the spool, then returns
//     allow-with-recorded-error — fail-open, but the error is DURABLE and
//     VISIBLE, never dropped.
//   - An "ask"-class rule firing in this lane cannot prompt: OpenCode's hook
//     timeout is unmeasured, so the inbox's fail-closed budget contract cannot
//     be verified for it (the same condition hookAskBudget fails closed on).
//     The decision is deny-with-reason and the reason says it was a
//     confirm-class rule (plan deviation 2, recorded 2026-08-30).
//   - enforcementDisabled() keeps parity with the hook lane: every deny path
//     funnels through governDeny, which observes and reports allow with the
//     would-block reason when the operator stood enforcement down.

import (
	"encoding/json"
	"fmt"
	"os"

	"crossing-guard/engine"
	"crossing-guard/internal/observation"
	"crossing-guard/internal/rulebook"
)

// GovernDecision is the wire shape the OpenCode plugin parses. Deliveries are
// pending helper messages the daemon handed to this allow boundary; the plugin
// appends them through its own session API (the collection-hook delivery
// channel, helper-session-attachment plan D5).
type GovernDecision struct {
	Decision   string                 `json:"decision"`
	Reason     string                 `json:"reason,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Deliveries []observation.Delivery `json:"crossing_guard_deliveries,omitempty"`
}

// cmdGovernHook evaluates one PreToolUse-shaped payload and answers the
// decision the plugin should enforce. It is the OpenCode lane's `hook`:
// same evaluator, JSON output, headless ask handling.
func cmdGovernHook(args []string) {
	var in hookInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		emitGovernError("govern payload did not parse", err)
		return
	}
	in.Runtime = flagValue(args, "--runtime")
	in.Carrier = hasFlag(args, "--carrier")
	if in.HookEventName == "" {
		in.HookEventName = "PreToolUse"
	}
	if in.HookEventName != "PreToolUse" {
		// Only the decision phase is governed; other phases belong to
		// collect-hook. Answer allow without judgment, honestly.
		emitGovern(governJSON("allow", "phase not governed by the decision lane"))
		return
	}
	emitGovern(governHookPreToolUse(in))
}

// governHookPreToolUse is the decision path, separated for tests: static
// engine tier, then the standalone command tier, then the stateful consult.
// It stages the observation first (observeAttempt) so every decision funnel
// flushes the attempt WITH its decision to the durable spool — the same
// staging contract cmdHook uses.
func governHookPreToolUse(in hookInput) GovernDecision {
	observeAttempt(in)
	pol, err := rulebook.Load()
	if err != nil {
		observeGovernError(in, "rules unloadable", err)
		_, deliveries := observeDecision("allow", "governance error (rules unloadable): call allowed and recorded")
		return GovernDecision{Decision: "allow",
			Reason:     "crossing-guard governance error (rules): call allowed and recorded",
			Error:      err.Error(),
			Deliveries: deliveries}
	}
	command := string(in.ToolInput.Command)
	if d, _, _, _ := engineDecision(in); d != nil && d.Decision != "allow" {
		switch d.Mode {
		case engine.HardBlock:
			return governDeny(fmt.Sprintf("Blocked by rule %s (Restricted, non-overridable): %s",
				d.Rule, d.Message))
		case engine.ConfirmAndRecord:
			return governDeny(fmt.Sprintf("Blocked by confirm-class rule %s: %s (this headless lane cannot prompt; re-run in a governed console task to confirm)",
				d.Rule, d.Message))
		case engine.WarnAndProceed:
			// warn = proceed, recorded; the standalone tier below still runs.
		}
	}
	if gd := engine.Decide(staticInvocationTags(in.ToolName, command), pol); gd.Decision != "allow" {
		if gd.Mode == engine.HardBlock {
			return governDeny(fmt.Sprintf("Blocked by rule %s (Restricted, non-overridable): %s",
				gd.Rule, guardMessage(gd)))
		}
		return governDeny(fmt.Sprintf("Blocked by confirm-class rule %s: %s (this headless lane cannot prompt; re-run in a governed console task to confirm)",
			gd.Rule, guardMessage(gd)))
	}
	if sv := consultStateful(in); sv != nil {
		if verdict, denied := statefulHeadlessDenial(sv); denied {
			return governDeny(verdict)
		}
	}
	_, deliveries := observeDecision("allow", "no rule matched")
	decision := governJSON("allow", "no rule matched")
	decision.Deliveries = deliveries
	return decision
}

// statefulHeadlessDenial maps a daemon stateful verdict onto the headless
// lane: deny and ask both deny with the recorded reason (an unconfirmable ask
// fails closed, same as the hook lane's unverified-budget path). consultStateful
// already returned nil for allow/not-evaluated, so reaching here means a rule fired.
func statefulHeadlessDenial(sv *statefulVerdict) (string, bool) {
	msg := sv.message
	if msg == "" {
		msg = "blocked by a stateful rule over this session's live state"
	}
	if sv.decision == "ask" {
		return fmt.Sprintf("Blocked by confirm-class session-state rule %s: %s (this headless lane cannot prompt; re-run in a governed console task to confirm)",
			sv.rule, msg), true
	}
	return fmt.Sprintf("Blocked by session-state rule %s: %s", sv.rule, msg), true
}

// governDeny is the one deny funnel for this lane — the emitDeny analogue. It
// honors the enforcement switch (standing enforcement down reports allow with
// the would-block reason, recorded) and flushes the staged observation with
// the final decision.
func governDeny(reason string) GovernDecision {
	if off, why := enforcementDisabled(); off {
		_, deliveries := observeDecision("allow", "WOULD BLOCK ("+reason+") — enforcement off: "+why)
		fmt.Fprintf(os.Stderr, "[crossing-guard] enforcement OFF — allowed an action that would be blocked: %s\n", reason)
		decision := governJSON("allow", "enforcement is off — would block: "+reason)
		decision.Deliveries = deliveries
		return decision
	}
	observeDecision("deny", reason)
	return governJSON("deny", reason)
}

func governJSON(decision, reason string) GovernDecision {
	return GovernDecision{Decision: decision, Reason: reason}
}

func emitGovern(decision GovernDecision) {
	body, _ := json.Marshal(decision)
	fmt.Println(string(body))
}

// emitGovernError reports an internal lane failure honestly: stderr, the
// durable decisions log, and the wire error field — then allow. Fail-open is
// the v1 mode, but the error is durable and visible, never silently dropped.
func emitGovernError(message string, err error) {
	detail := message
	if err != nil {
		detail = message + ": " + err.Error()
	}
	fmt.Fprintln(os.Stderr, "[crossing-guard] governance error (call allowed and recorded):", detail)
	logLine(map[string]any{"governance_error": "lane-failure", "detail": detail})
	emitGovern(GovernDecision{Decision: "allow", Reason: "governance error recorded", Error: detail})
}

// observeGovernError records the typed governance error into the durable
// decisions log so the console's diagnostics can render OpenCode lane
// failures — a governance error must outlive the process that hit it.
func observeGovernError(in hookInput, class string, err error) {
	entry := map[string]any{"governance_error": class, "runtime": in.Runtime,
		"session": in.SessionID, "tool": in.ToolName}
	if err != nil {
		entry["detail"] = err.Error()
	}
	logLine(entry)
}
