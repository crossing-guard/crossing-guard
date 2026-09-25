package main

// `crossing-guard verify` — does the hook actually FIRE?
//
// Everything else in this product can only tell you a hook is REGISTERED, which
// is a fact about a config file. Codex registers a hook as untrusted and then
// silently skips it; a Claude plugin can install enabled-with-dead-hooks. In both
// cases every surface reports success and nothing is governed. Registration is
// not firing, and this is the only verb that knows the difference.
//
// It does NOT drive the runtime to produce a tool call. That was the obvious
// implementation: spawn `claude -p` and watch. It spends the user's subscription
// quota during install, needs an OAuth session the daemon's own startup note says
// cannot refresh in every context, and adds a vendor-automation surface that
// breaks on every vendor change. Instead this WATCHES: it takes a baseline from
// the event log, asks you to do one ordinary thing in your agent, and confirms
// the event it sees. Zero quota, no vendor automation, and it verifies the real
// path rather than a simulation of it.

import (
	"flag"
	"fmt"
	"os"
	"time"

	"crossing-guard/internal/daemon"
	"crossing-guard/internal/guardcli"
	"crossing-guard/store"
)

// runtimesReport decodes /api/govern/runtimes using the store's OWN row type:
// this wire shape existed in three hand-rolled copies (store, here, doctor), two
// of which had already drifted on whether `configured` was checked.
type runtimesReport struct {
	Configured bool                `json:"configured"`
	Runtimes   []store.RuntimeStat `json:"runtimes"`
}

const (
	// defaultWatchWindow is how long verify waits for a human to make one tool
	// call in their agent — generous because the instruction asks them to switch
	// windows and do something.
	defaultWatchWindow = 120 * time.Second
	// pollInterval paces the event-log reads during the watch. The query is a
	// full-table GROUP BY, cheap at current sizes; this is politeness, not load-
	// bearing protection.
	pollInterval = 2 * time.Second
)

func verify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	timeoutSecs := fs.Int("timeout", int(defaultWatchWindow/time.Second),
		"seconds to watch the event log for a hook fire")
	_ = fs.Parse(args)
	if *timeoutSecs <= 0 {
		fmt.Fprintln(os.Stderr, "--timeout takes a positive number of seconds")
		os.Exit(2)
	}
	wait := time.Duration(*timeoutSecs) * time.Second

	loc, err := daemon.LocateConsole()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if problem := daemon.ProbeConsole(loc); problem != "" {
		fmt.Fprintln(os.Stderr, problem)
		fmt.Fprintln(os.Stderr, "\nWithout the daemon nothing is captured, so nothing can be verified.")
		os.Exit(1)
	}

	// Only ATTACHED runtimes are candidates — a hook must exist in the config for
	// "does it fire" to be a meaningful question. Consented-but-detached (someone
	// removed the hook by hand) is reported separately and immediately: waiting a
	// full watch window to then say "registered and not running" about a runtime
	// that is not registered at all would be a false diagnosis on a timer. One
	// detection pass, reused everywhere below — re-detecting per failure re-read
	// every vendor config and could disagree with this list mid-run.
	detected := guardcli.DetectRuntimes()
	var want []string
	for _, r := range detected {
		if !r.Installed {
			continue
		}
		if r.Consented && !r.Attached {
			fmt.Printf("%-8s consented, but its hook is GONE from the config — not watchable. Repair: crossing-guard init\n", r.Name)
			continue
		}
		if r.Attached {
			want = append(want, r.Name)
		}
	}
	if len(want) == 0 {
		fmt.Println("No runtime is attached, so there is nothing to verify.")
		fmt.Println("Attach one with: crossing-guard init")
		return
	}

	base, err := fetchRuntimes(loc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read the event log:", err)
		os.Exit(1)
	}
	baseline := map[string]int{}
	for _, s := range base.Runtimes {
		baseline[s.Runtime] = s.Events
	}

	fmt.Println("Verifying that the guard hook FIRES — not that it is installed.")
	fmt.Println("In each agent below, do one ordinary thing (list a directory, read a file).")
	fmt.Printf("Watching the event log for up to %s.\n", wait)
	if note := unattributedNote(base.Runtimes); note != "" {
		fmt.Println("(" + note + ")")
	}
	fmt.Println()
	for _, name := range want {
		fmt.Printf("  %-8s waiting for its first tool call…  (%d events recorded so far)\n", name, baseline[name])
	}

	deadline := time.Now().Add(wait)
	fired := map[string]bool{}
	for time.Now().Before(deadline) && len(fired) < len(want) {
		time.Sleep(pollInterval)
		cur, err := fetchRuntimes(loc)
		if err != nil {
			continue // a transient read must not end the watch
		}
		counts := map[string]int{}
		for _, s := range cur.Runtimes {
			counts[s.Runtime] = s.Events
		}
		for _, name := range want {
			if fired[name] || counts[name] <= baseline[name] {
				continue
			}
			fired[name] = true
			fmt.Printf("\n  %-8s FIRING — %d new event(s) observed. This runtime is governed.\n",
				name, counts[name]-baseline[name])
		}
	}

	fmt.Println()
	exit := 0
	for _, name := range want {
		if fired[name] {
			continue
		}
		exit = 1
		fmt.Printf("  %-8s NOT VERIFIED — no event arrived while watching.\n", name)
		// The honest reasons, in the order they actually occur. Never "it works,
		// probably": an unverified hook is exactly the state this verb exists to name.
		fmt.Println("           Either you did not use it during the watch, or its hook is")
		fmt.Println("           registered and not running.")
		for _, r := range detected {
			if r.Name == name && r.Manual != "" {
				fmt.Println("           MOST LIKELY: " + r.Manual)
			}
		}
	}
	if exit == 0 {
		fmt.Println("Every attached runtime fired. Enforcement is live, and proven by events.")
	}
	os.Exit(exit)
}

// fetchRuntimes is a thin wrapper over the ONE daemon client (Location.GetJSON),
// adding only this endpoint's semantic check: an unconfigured governor means
// nothing is captured, so nothing can be verified.
func fetchRuntimes(loc daemon.Location) (runtimesReport, error) {
	var out runtimesReport
	if err := loc.GetJSON("/api/govern/runtimes", &out); err != nil {
		return out, err
	}
	if !out.Configured {
		return out, fmt.Errorf("the governor is not configured — nothing is being captured")
	}
	return out, nil
}

// unattributedNote explains the runtime:"" bucket wherever it is shown, so a large
// unattributed count reads as history rather than as a broken install.
func unattributedNote(stats []store.RuntimeStat) string {
	for _, s := range stats {
		if s.Runtime == "" && s.Events > 0 {
			return fmt.Sprintf("%d event(s) predate per-runtime attribution and are recorded as unknown",
				s.Events)
		}
	}
	return ""
}
