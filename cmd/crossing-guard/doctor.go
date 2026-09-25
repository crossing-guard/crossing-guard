package main

// `crossing-guard doctor` — the support surface, and the one place that answers
// "am I actually governed?" without asking you to believe anything.
//
// It used to report memory injection ONLY. That made it the wrong shape for the
// question people bring to it: a user whose hooks were silently dead, whose daemon
// had stopped, or whose enforcement had been stood down would read a clean-looking
// memory report and conclude the system was fine.
//
// Every line here carries its own basis, and the distinction that matters most is
// REGISTERED vs FIRING. A config file proves someone installed a hook. Only an
// event in the log proves the hook ran. A runtime that has never produced an event
// is reported as unverified — never as protected — because that is the state Codex
// lands in by default when its hook is untrusted, and the whole product is an
// argument against reporting it any other way.

import (
	"crossing-guard/harvest"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"crossing-guard/internal/daemon"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/memcli"
	"crossing-guard/store"
)

type doctorReport struct {
	Service     serviceHealth          `json:"service"`
	Capture     captureHealth          `json:"capture"`
	Enforcement enforcementHealth      `json:"enforcement"`
	Runtimes    []runtimeHealth        `json:"runtimes"`
	Config      []configFile           `json:"config"`
	Platform    daemon.PlatformSupport `json:"platform"`
	Memory      memcli.DoctorReport    `json:"memory"`
	// MemoryImport is the daemon's last import of vendor auto-memory into the
	// store (daemon-memory-import plan D6); nil until the daemon has run one.
	MemoryImport *daemon.MemoryImportState `json:"memory_import,omitempty"`
	// ViewProfiles are the console's transcript view modules in use, each with
	// its origin, and the module files that failed (transcript-view-profiles plan).
	ViewProfiles viewProfilesHealth `json:"view_profiles"`
	// Capabilities is the per-runtime capability parity matrix (agents redesign
	// §4): which optional harvest capabilities each registered runtime actually
	// implements, plus the interface_revision naming the provider CLI each
	// adapter was verified against (natural-session plan, Slice A), so a
	// silently half-implemented vendor is visible here instead of as missing
	// edges with no explanation.
	Capabilities map[string]harvest.CLICapability `json:"capabilities"`
}

type viewProfilesHealth struct {
	Profiles []daemon.ResolvedViewProfile  `json:"profiles"`
	Rejected []daemon.ViewProfileRejection `json:"rejected"`
}

type serviceHealth struct {
	Running bool   `json:"running"`
	Addr    string `json:"addr,omitempty"`
	DataDir string `json:"data_dir,omitempty"`
	Source  string `json:"source,omitempty"`
	Problem string `json:"problem,omitempty"`
	URL     string `json:"console_url,omitempty"`
}

type captureHealth struct {
	Configured      bool   `json:"configured"`
	TotalEvents     int64  `json:"total_events"`
	LastEventAgeSec int64  `json:"last_event_age_sec"`
	ObserveFailures int64  `json:"observe_failures"`
	Note            string `json:"note,omitempty"`
}

type enforcementHealth struct {
	On     bool   `json:"on"`
	Reason string `json:"reason,omitempty"`
	Rules  string `json:"rules"`
}

type runtimeHealth struct {
	Name string `json:"name"`
	// Status is the honest ladder, in increasing order of evidence:
	//   not installed → not governed → attached, never verified → ENFORCING
	// Only the last is backed by an event, and only the last may be read as "this
	// agent is guarded".
	Status     string          `json:"status"`
	Config     string          `json:"config,omitempty"`
	Events     int             `json:"events"`
	LastFired  int64           `json:"last_fired,omitempty"`
	Manual     string          `json:"manual,omitempty"`
	HookPhases map[string]bool `json:"hook_phases,omitempty"`
}

type configFile struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Missing string `json:"missing_means,omitempty"` // what is UNAVAILABLE without it
}

func doctorCmd(args []string) {
	// flag.FlagSet, not a hand-rolled loop: the hand-rolled version accepted
	// `--recent` only BARE — `doctor --recent 5`, the exact form the memory
	// doctor's own usage documents, hit the default arm and exited 2. Five verbs
	// had five hand-rolled loops with three different value-flag semantics; only
	// the hook's hot path has a reason not to use the standard parser.
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the full report as JSON (the BYO-GUI shape)")
	recent := fs.Int("recent", 5, "recent sessions to check per vendor for memory injection")
	_ = fs.Parse(args) // ExitOnError: a bad flag prints usage and exits

	rep := doctorReport{
		Enforcement:  enforcementSection(),
		Config:       configSection(),
		Platform:     daemon.CurrentPlatformSupport(),
		Memory:       memcli.MemoryDoctor(*recent),
		Capabilities: harvest.CapabilityMatrix(),
	}
	if state, found, err := daemon.ReadMemoryImportState(dataDir()); err == nil && found {
		rep.MemoryImport = &state
	}
	rep.ViewProfiles.Profiles, rep.ViewProfiles.Rejected = daemon.LoadViewProfiles(dataDir())
	var unattributed string
	rep.Service, rep.Capture, rep.Runtimes, unattributed = liveSections()

	if *asJSON {
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "encode report:", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
		return
	}
	printDoctor(rep, unattributed)
}

func enforcementSection() enforcementHealth {
	on, reason, rules := guardcli.EnforcementStatus()
	return enforcementHealth{On: on, Reason: reason, Rules: rules}
}

// configSection reports the config files and, for each absent one, what is
// UNAVAILABLE — not that something is broken. A missing file that costs nothing
// reported as an error is how a healthy install gets read as a sick one; the
// daemon's own startup log does exactly that today and it is misleading.
func configSection() []configFile {
	d := dataDir()
	files := []struct{ path, missing string }{
		{filepath.Join(d, "policy", "rules.json"),
			"no legacy user file; inspect available/selected/active state with `crossing-guard rules status`"},
		{filepath.Join(d, "policy", "detectors.json"),
			"no legacy overlay file; inspect configured and running assemblies with `crossing-guard detectors status`"},
		{filepath.Join(d, "policy-engine.json"),
			"your ACTIVE enforcement rules are used for the compiled coverage report — which is " +
				"what it should describe; a separate file here would be a second copy free to drift"},
	}
	out := make([]configFile, 0, len(files))
	for _, f := range files {
		_, err := os.Stat(f.path)
		cf := configFile{Path: f.path, Present: err == nil}
		if !cf.Present {
			cf.Missing = f.missing
		}
		out = append(out, cf)
	}
	return out
}

// liveSections asks the running daemon. Everything here degrades honestly: with no
// daemon we still report the runtimes we can see from their configs, and say
// plainly that firing cannot be established without it. A FAILED fetch degrades
// the same way — the first version swallowed both fetch errors, and a doctor
// whose evidence query failed then asserted "0 events on record" and "no event
// has EVER arrived": confident facts fabricated from an error, in the one tool
// whose job is refusing to do exactly that.
func liveSections() (serviceHealth, captureHealth, []runtimeHealth, string) {
	svc := serviceHealth{}
	loc, err := daemon.LocateConsole()
	if err != nil {
		svc.Problem = err.Error()
		return svc, captureHealth{Note: "no daemon — nothing is being captured"}, runtimesFromConfig(nil), ""
	}
	svc.Addr, svc.DataDir, svc.Source = loc.Addr, loc.DataDir, loc.Source
	if problem := daemon.ProbeConsole(loc); problem != "" {
		svc.Problem = problem
		return svc, captureHealth{Note: "the daemon is not answering — capture cannot be confirmed"},
			runtimesFromConfig(nil), ""
	}
	svc.Running, svc.URL = true, loc.URL()

	capture := captureHealth{}
	var health struct {
		Configured      bool  `json:"configured"`
		LastEventTS     int64 `json:"last_event_ts"`
		TotalEvents     int64 `json:"total_events"`
		ObserveFailures int64 `json:"observe_failures"`
	}
	if err := loc.GetJSON("/api/govern/health", &health); err != nil {
		capture.Note = "could not read capture health (" + err.Error() + ") — no verdict"
	} else {
		capture.Configured, capture.TotalEvents = health.Configured, health.TotalEvents
		capture.ObserveFailures = health.ObserveFailures
		if health.LastEventTS > 0 {
			capture.LastEventAgeSec = time.Now().Unix() - health.LastEventTS
		} else {
			capture.Note = "nothing has ever been captured"
		}
	}

	// Decoded into the store's own type: this is the third consumer of the wire
	// shape, and the two hand-rolled copies had already drifted (one checked
	// `configured`, one did not).
	var runtimes struct {
		Runtimes []store.RuntimeStat `json:"runtimes"`
	}
	if err := loc.GetJSON("/api/govern/runtimes", &runtimes); err != nil {
		// No evidence is NOT zero evidence: with the fetch failed, the ladder must
		// say "cannot verify", never "never fired".
		return svc, capture, runtimesFromConfig(nil), ""
	}
	seen := map[string]store.RuntimeStat{}
	for _, s := range runtimes.Runtimes {
		seen[s.Runtime] = s
	}
	// The unattributed bucket is part of the answer, not noise to drop: hiding it
	// makes a log that is 90% unattributed look fully attributed (the store's own
	// documented invariant, which the first doctor silently violated).
	unattributed := ""
	if u, ok := seen[""]; ok && u.Events > 0 {
		unattributed = fmt.Sprintf("%d event(s) predate per-runtime attribution and are recorded as unknown", u.Events)
	}
	return svc, capture, runtimesFromConfig(func(name string) (int, int64) {
		v := seen[name]
		return v.Events, v.LastSeen
	}), unattributed
}

// runtimesFromConfig builds the per-runtime ladder. evidence is nil when no daemon
// is reachable, in which case NOTHING may be reported as enforcing — the absence of
// evidence is reported as absence of evidence.
func runtimesFromConfig(evidence func(string) (int, int64)) []runtimeHealth {
	var out []runtimeHealth
	for _, r := range guardcli.DetectRuntimes() {
		h := runtimeHealth{Name: r.Name, Config: r.Config, Manual: r.Manual, HookPhases: r.HookPhases}
		switch {
		case !r.Installed:
			h.Status = "not installed on this machine"
		case !r.Consented && !r.Attached:
			h.Status = "installed, NOT governed — run: crossing-guard init"
		// The consent record says yes, the config carries no hook: someone removed
		// it by hand (or the config was replaced). Without this rung the state fell
		// through to "attached…" wordings — and with stale historical events, all
		// the way to "ENFORCING" — for a runtime that is not even registered. Past
		// events prove only that it USED to work.
		case r.Consented && !r.Attached:
			h.Status = "consented, but the hook is GONE from its config — NOT guarded. Repair: crossing-guard init"
		// Checked BEFORE the no-daemon case: whether the hook's binary exists is a
		// filesystem fact, knowable with the daemon down, and it is the most severe
		// state a runtime can be in. Ordering it after "cannot verify" hid the one
		// diagnosis that does not need verifying.
		case r.Attached && !r.BinaryPresent:
			// The dangerous one, and the reason HookBinary returns a path: the config
			// still looks installed, the agent keeps running, and nothing is guarded.
			// Reported ahead of any event evidence, because past events prove only
			// that it USED to work.
			h.Status = "BROKEN — its hook invokes " + r.HookBinary +
				", which no longer exists. This runtime is NOT guarded. Repair: crossing-guard init"
		case evidence == nil:
			h.Status = "attached; cannot verify firing without the daemon"
		default:
			h.Events, h.LastFired = evidence(r.Name)
			switch {
			case h.Events == 0:
				h.Status = "attached, NEVER VERIFIED — no event has ever arrived from it"
			default:
				h.Status = "ENFORCING — proven by events, not by config"
				// r.HookBinary is guaranteed non-empty here (the Consented && !Attached
				// rung above catches the hookless case — without it this printed the
				// garbled "(via , not the binary…)").
				if !r.Current {
					// Not a fault: another install, or doctor run from a scratch build.
					// Worth saying, never worth alarming about.
					h.Status += " (via " + r.HookBinary + ", not the binary running this check)"
				}
			}
		}
		out = append(out, h)
	}
	return out
}

func printDoctor(rep doctorReport, unattributed string) {
	fmt.Println("crossing-guard doctor")

	fmt.Println("\nSERVICE")
	if rep.Service.Running {
		fmt.Printf("  running   %s   (%s)\n", rep.Service.Addr, rep.Service.Source)
		fmt.Printf("  console   %s\n", rep.Service.URL)
	} else {
		fmt.Printf("  NOT RUNNING\n  %s\n", rep.Service.Problem)
	}

	fmt.Println("\nCAPTURE")
	if rep.Capture.Note != "" { // any note preempts the count — it says why there is no verdict
		fmt.Printf("  %s\n", rep.Capture.Note)
	} else {
		fmt.Printf("  %d events on record; last one %s ago\n",
			rep.Capture.TotalEvents, humanAge(rep.Capture.LastEventAgeSec))
	}
	if rep.Capture.ObserveFailures > 0 {
		fmt.Printf("  WARNING: %d action(s) were accepted and could NOT be persisted — capture is losing data\n",
			rep.Capture.ObserveFailures)
	}

	fmt.Println("\nENFORCEMENT")
	if rep.Enforcement.On {
		fmt.Printf("  ON        rules: %s\n", rep.Enforcement.Rules)
	} else {
		fmt.Printf("  OFF       %s\n", rep.Enforcement.Reason)
		fmt.Println("            actions are still observed and recorded; re-enable with: crossing-guard enforce on")
	}

	fmt.Println("\nRUNTIMES   (registered is not firing — only an event proves a hook ran)")
	for _, r := range rep.Runtimes {
		fmt.Printf("  %-8s %s\n", r.Name, r.Status)
		if len(r.HookPhases) > 0 {
			phases := make([]string, 0, len(r.HookPhases))
			for phase := range r.HookPhases {
				phases = append(phases, phase)
			}
			sort.Strings(phases)
			fmt.Print("           phases:")
			for _, phase := range phases {
				fmt.Printf(" %s=%t", phase, r.HookPhases[phase])
			}
			fmt.Println()
		}
		if r.Events > 0 {
			fmt.Printf("           %d event(s), last %s ago\n", r.Events, humanAge(time.Now().Unix()-r.LastFired))
		}
		if r.Manual != "" && r.Events == 0 {
			fmt.Printf("           %s\n", r.Manual)
		}
	}
	if unattributed != "" {
		fmt.Println("  (" + unattributed + ")")
	}
	fmt.Println("  verify firing yourself with: crossing-guard verify")

	fmt.Println("\nADAPTERS   (interface each provider adapter was verified against)")
	for _, name := range harvest.RuntimeNames() {
		cap := rep.Capabilities[name]
		revision := cap.InterfaceRevision
		if revision == "" {
			revision = "not published"
		}
		fmt.Printf("  %-8s interface %s\n", name, revision)
	}

	fmt.Println("\nCONFIG")
	for _, c := range rep.Config {
		if c.Present {
			fmt.Printf("  present   %s\n", c.Path)
			continue
		}
		fmt.Printf("  absent    %s\n            %s\n", c.Path, c.Missing)
	}

	fmt.Printf("\nPLATFORM (%s)\n", rep.Platform.GOOS)
	fmt.Printf("  service=%s uninstall=%s store-acl=%s stateful-enforcement=%v\n",
		rep.Platform.Service, rep.Platform.Uninstall, rep.Platform.StoreACL,
		rep.Platform.StatefulEnforcementReady())
	if rep.Platform.Note != "" {
		fmt.Printf("  %s\n", rep.Platform.Note)
	}

	fmt.Println("\nVIEW PROFILES   (console transcript modules)")
	for _, profile := range rep.ViewProfiles.Profiles {
		fmt.Printf("  %-14s %s\n", profile.ID, profile.Origin)
	}
	for _, rejection := range rep.ViewProfiles.Rejected {
		fmt.Printf("  skipped %s: %s\n", rejection.Path, rejection.Error)
	}

	fmt.Println("\nMEMORY   (injection is a SEPARATE attachment from the guard hook)")
	fmt.Printf("  store: %s (%d records, %d pending)\n",
		rep.Memory.Store, rep.Memory.Records, rep.Memory.Pending)
	if rep.MemoryImport != nil {
		fmt.Printf("  daemon import: last %s (%s) — %d imported, %d skipped, %d indexed, %d errors%s\n",
			rep.MemoryImport.LastAt, rep.MemoryImport.Reason, rep.MemoryImport.Imported, rep.MemoryImport.Skipped,
			rep.MemoryImport.Indexed, rep.MemoryImport.Errors, doctorErrorSuffix(rep.MemoryImport.Error))
	} else {
		fmt.Println("  daemon import: none recorded yet (runs on session end and the lifecycle sweep)")
	}
	for _, v := range rep.Memory.Vendors {
		// A count of verified injections is meaningless where nothing injects, and
		// printing "0/5 verified" beside "not attached" invites reading the zero as
		// a failure rather than as an absence.
		if !v.Attached {
			fmt.Printf("  %-8s %s\n", v.Vendor, v.Status)
			continue
		}
		fmt.Printf("  %-8s %d/%d recent sessions verified → %s\n",
			v.Vendor, v.Injected, v.Checked, v.Status)
	}
}

func humanAge(sec int64) string {
	switch {
	case sec < 0:
		return "0s"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh", sec/3600)
	}
	return fmt.Sprintf("%dd", sec/86400)
}
