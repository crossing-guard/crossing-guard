package main

// `crossing-guard demo` — the guarded near-miss, in one minute.
//
// This product's payoff is insurance-shaped: it arrives at a violation, which may
// be days away. A first run that merely completes successfully leaves the user
// with nothing felt, so the value has to be manufactured once, honestly.
//
// It is manufactured by doing the real thing, not by describing it: the canary
// command goes through the SAME hook binary the runtimes invoke, is judged by the
// SAME rules file the hook enforces, and the denial is recorded in the SAME
// append-only log every other action lands in. Then it hands over the link to
// that record. Nothing here is simulated, and nothing is written that a real
// denial would not have written.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/daemon"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/rulebook"
)

// canaryCommand is built from the marker guardcli exports — ONE definition,
// shared with the shipped rule and pinned by TestShippedRulesDenyTheCanary. The
// first version spelled the marker twice in this file and cited a "shipped rule"
// that did not exist in the shipped set: on every fresh install the demo bailed
// with an unactionable message. The demo still checks the ACTIVE ruleset before
// claiming anything: rules.json is the user's to edit (R7).
var canaryCommand = "echo " + rulebook.CanaryMarker

func demo(args []string) {
	open, err := parseOpenArgs("demo", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard demo [--open]")
		os.Exit(2)
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve own path:", err)
		os.Exit(1)
	}

	loaded, err := rulebook.LoadDocument()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read your rules:", err)
		os.Exit(1)
	}
	pol := loaded.Policy
	if !canaryWouldBeDenied(pol) {
		fmt.Println("Your active ruleset does not DENY the canary, so there is nothing here to")
		fmt.Println("demonstrate without inventing a block your rules would not actually make.")
		fmt.Println("  rules: " + loaded.Path)
		fmt.Println("  selection: " + loaded.Selection)
		fmt.Println("No canary rule is selected, so this check cannot demonstrate runtime enforcement.")
		fmt.Println("Select the starter/current rulebook, or watch")
		fmt.Println("a real block instead with: crossing-guard verify")
		os.Exit(1)
	}

	session := fmt.Sprintf("demo-%d", time.Now().Unix())
	fmt.Println("A guarded near-miss, for real — same hook, same rules, same log.")
	fmt.Printf("\n  the agent tries:  %s\n", canaryCommand)

	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      session,
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": canaryCommand},
	})
	cmd := exec.Command(self, "hook", "--runtime", rulebook.DemoRuntime)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		fmt.Fprintln(os.Stderr, "the hook did not answer:", err)
		os.Exit(1)
	}

	// Show the verdict exactly as the runtime receives it — this IS the interface
	// between us and the agent, and paraphrasing it would hide the one thing a
	// sceptical reader wants to check.
	var resp struct {
		Out struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	// A parse failure is NOT an allow: a crashed hook emitting partial output must
	// not be reported as "your rules did not block it" — that false headline, from
	// the one verb whose job is showing the hook working, would be believed.
	if err := json.Unmarshal(out, &resp); err != nil {
		fmt.Println("\n  the hook's answer could not be parsed (" + err.Error() + ")")
		fmt.Println("  raw answer: " + strings.TrimSpace(string(out)))
		os.Exit(1)
	}
	if resp.Out.Decision == "" {
		fmt.Println("\n  the hook allowed it — your rules did not block the canary.")
		fmt.Println("  raw answer: " + strings.TrimSpace(string(out)))
		os.Exit(1)
	}
	fmt.Printf("  the agent gets:   %s\n", strings.ToUpper(resp.Out.Decision))
	fmt.Printf("                    %s\n", resp.Out.Reason)

	fmt.Println("\nThe command never ran. That refusal is now recorded — and this log is the")
	fmt.Println("only place it exists, because a denied call never reaches the agent's own")
	fmt.Println("transcript at all.")

	loc, lerr := daemon.LocateConsole()
	if lerr != nil || daemon.ProbeConsole(loc) != "" {
		fmt.Println("\nStart the daemon to read the record: crossing-guard serve")
		fmt.Println("  then: crossing-guard console --open")
		return
	}
	link := loc.URL() + "&tab=capture&session=" + session
	fmt.Println("\nRead it here:")
	fmt.Println("  " + link)
	if !open {
		fmt.Println("\n(or run: crossing-guard demo --open)")
		return
	}
	if err := exec.Command("open", link).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "could not launch a browser ("+err.Error()+") — open the link above yourself")
	}
}

// canaryWouldBeDenied asks the ONE evaluator (ADR 0025) what the active ruleset
// actually decides about the canary command — the same call the hook makes. The
// first version grepped a JSON-marshalled predicate for the marker substring,
// which cannot tell deny from ask from a rule wrapped in `not:`; the honest
// answer is the static tier's own decision (guardcli.StaticDecide), over the same
// Bash tool identity the demo's hook run sends. Specifically a hard DENY: an `ask` rule
// would send the demo's hook run to the live approvals inbox and hang there,
// which is not a demo anyone asked for. A warn rule proceeds in the hook, so it
// is no deny either.
func canaryWouldBeDenied(pol *engine.Policy) bool {
	dets, err := guardcli.ActiveDetectors()
	if err != nil {
		dets = nil // only the invocation's own facts are judged; CheckAction says so
	}
	return guardcli.CheckAction("Bash", canaryCommand, dets, pol).Decision.Mode == engine.HardBlock
}
