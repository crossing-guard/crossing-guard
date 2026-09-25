package guardcli

// The enforcement switch.
//
// Until now there was no way to turn enforcement off. CG_OBSERVE=0 disables
// OBSERVATION — the opposite of what someone blocked mid-task wants — and deleting
// the hook does not work, because the daemon self-heals vendor hooks on every start
// and is KeepAlive-managed, so it puts them back. A tool that governs your agents
// must be something you can stand down without uninstalling it or fighting it.
//
// Two deliberate properties:
//
//  1. OFF still observes, and still RECORDS what it would have done. The decision is
//     written as "allow" with a reason naming the rule that would have fired, so the
//     log answers "what did this cost me?" and turning enforcement off never creates
//     a blind spot. Governance data is the product; the block is a policy on top.
//  2. It is a FILE, not an env var. Hooks run as subprocesses of an agent you did not
//     launch from this shell, so an env var cannot be set where it is needed. A file
//     in the data dir works for every runtime, survives restarts, and is inspectable.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/internal/rulebook"
)

// enforcementFile holds the switch. Absent = enforcing (the safe default: a missing
// or unreadable file must never silently disable the guard).
func enforcementFile() string { return filepath.Join(dataDir(), "policy", "enforcement-off") }

// enforcementDisabled reports whether the operator has stood enforcement down, and
// why. Any read error means ENFORCING — failing open here would let a permissions
// problem quietly turn the guard off.
func enforcementDisabled() (bool, string) {
	b, err := os.ReadFile(enforcementFile())
	if err != nil {
		return false, ""
	}
	reason := strings.TrimSpace(string(b))
	if reason == "" {
		reason = "no reason given"
	}
	return true, reason
}

// EnforcementStatus reports whether the guard is standing, why not if not, and
// which rules file is active — so a surface that claims "you are governed" can
// check rather than assume. Exported because `doctor` must never re-derive this:
// a second copy of "absent file means enforcing" is a second chance to get the
// safe default backwards.
func EnforcementStatus() (on bool, reason, rules string) {
	off, why := enforcementDisabled()
	loaded, err := rulebook.LoadDocument()
	if err != nil {
		return !off, why, "(unavailable: " + err.Error() + ")"
	}
	return !off, why, loaded.Path + " [" + loaded.Selection + "]"
}

func cmdEnforce(args []string) {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "status":
		off, reason := enforcementDisabled()
		if off {
			fmt.Printf("enforcement: OFF\n  reason: %s\n  file:   %s\n\n"+
				"Actions are still observed and recorded. Blocks that WOULD have fired are\n"+
				"logged as allowed, with the rule named — so nothing is hidden while it is off.\n"+
				"Re-enable with: crossing-guard enforce on\n", reason, enforcementFile())
			return
		}
		_, _, rules := EnforcementStatus()
		fmt.Printf("enforcement: ON\n  rules: %s\n\nStand down with: crossing-guard enforce off [reason]\n", rules)

	case "off":
		reason := "turned off from the CLI"
		if len(args) > 1 {
			reason = strings.Join(args[1:], " ")
		}
		stamped := fmt.Sprintf("%s (%s)", reason, time.Now().Format(time.RFC3339))
		if err := os.MkdirAll(filepath.Dir(enforcementFile()), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "cannot create policy dir: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(enforcementFile(), []byte(stamped+"\n"), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "cannot disable enforcement: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("enforcement: OFF — %s\n\nNothing is blocked from now on. Actions are still observed,\n"+
			"and anything that would have been blocked is recorded as such.\nRe-enable with: crossing-guard enforce on\n", stamped)

	case "on":
		if err := os.Remove(enforcementFile()); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "cannot re-enable enforcement: %v\n", err)
			os.Exit(1)
		}
		_, _, rules := EnforcementStatus()
		fmt.Printf("enforcement: ON — rules from %s\n", rules)

	default:
		fmt.Fprintf(os.Stderr, "usage: crossing-guard enforce [status|off [reason]|on]\n")
		os.Exit(2)
	}
}
