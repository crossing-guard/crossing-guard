package main

// `crossing-guard init` — the one command a new user has to learn.
//
// Before this, the real first run was: build from source, run `install`, run
// `serve`, then grep a log file for your own console URL. Every stage worked and
// nothing tied them together, so a successful install still left a user with
// nothing to look at.
//
// It drives detect → plan → consent → attach → service → console, it is
// idempotent (re-running is a status/repair surface, not an error), and
// --dry-run prints the whole plan without touching anything. A governance
// product has to model consent in its own installer.
//
// What it deliberately does NOT do: claim the hooks are FIRING. Registration is
// not firing — Codex silently skips untrusted hooks — and the verification verb
// that would prove it is blocked on carrying the runtime name on an observation
// Until that lands, init
// reports registration as registration and names the human step.

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"crossing-guard/harvest"
	"crossing-guard/internal/daemon"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/rulebook"
)

func initCmd(args []string) {
	dryRun, assumeYes, err := parseInitArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard init [--dry-run] [--yes]")
		os.Exit(2)
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve own path:", err)
		os.Exit(1)
	}

	fmt.Println("crossing-guard init — one memory, one rulebook, every agent")
	fmt.Println("  binary: " + self)
	fmt.Println("  data:   " + dataDir() + "   (nothing leaves this machine)")
	if warning := initPlatformWarning(daemon.CurrentPlatformSupport()); warning != "" {
		fmt.Println(warning)
	}

	// A hook records the ABSOLUTE path of the binary that installed it. Run init
	// from a scratch build or a Downloads folder and every hook is repointed at a
	// path that will not exist tomorrow — and a hook whose binary is gone does not
	// fail loudly, it just stops guarding. Enforcement vanishing silently is the
	// exact failure class this product exists to make visible, so it will not be
	// installed from a temporary location without being said out loud.
	if ephemeral, why := ephemeralBinary(self); ephemeral {
		fmt.Println("\n  WARNING — this binary lives in " + why + ".")
		fmt.Println("  Hooks record the path they were installed from, so attaching now points")
		fmt.Println("  every runtime at a binary that may disappear — and a hook whose binary is")
		fmt.Println("  missing stops guarding SILENTLY.")
		fmt.Println("  Install it somewhere durable first (go install ./cmd/crossing-guard), then re-run.")
		if !dryRun && !assumeYes {
			fmt.Println("  Continuing anyway requires --yes.")
			os.Exit(1)
		}
	}

	// ── detect ────────────────────────────────────────────────────────────────
	// Adopt attachments made before the consent record existed, FIRST: without
	// this, a machine that has been governed for weeks reads as "NOT governed"
	// and init offers to attach what is already attached. --dry-run must not
	// write, so it reports the same conclusion from the hook it can see.
	if !dryRun {
		if adopted := guardcli.GrandfatherExistingAttachments(); len(adopted) > 0 {
			fmt.Println("\nadopted existing attachment(s): " + strings.Join(adopted, ", ") +
				"\n  (our hook was already in their config — recorded as consent rather than asking again)")
		}
	}
	runtimes := guardcli.DetectRuntimes()
	fmt.Println("\nRuntimes on this machine:")
	governable := 0
	for _, r := range runtimes {
		// Attached-but-unrecorded IS governed: the hook is in the config and only we
		// could have put it there. Reporting it as ungoverned would be the lie, and
		// GrandfatherExistingAttachments has already fixed the record unless we are
		// in --dry-run.
		governed := r.Consented || r.Attached
		attachedLabel := "GOVERNED"
		pendingLabel := "governed"
		if r.CollectionOnly {
			attachedLabel = "COLLECTING"
			pendingLabel = "collection attached"
		}
		switch {
		case !r.Installed:
			fmt.Printf("  %-8s client not detected", r.Name)
			if _, err := os.Stat(r.Config); r.Config != "" && err == nil {
				fmt.Printf("; configuration remains (%s)", r.Config)
			}
			fmt.Println()
		case governed && r.Current:
			fmt.Printf("  %-8s %s — hook current (%s)\n", r.Name, attachedLabel, r.Config)
		case governed:
			fmt.Printf("  %-8s %s, hook needs repair (%s)\n", r.Name, pendingLabel, r.Config)
			governable++
		default:
			fmt.Printf("  %-8s installed, NOT governed (%s)\n", r.Name, r.Config)
			governable++
		}
	}

	// The corpus is the first-value beat: it is the user's own data, it exists
	// before we change anything, and it costs nothing to show.
	if n, where := corpusSummary(); n > 0 {
		fmt.Printf("\nYour existing sessions: %d found across %s.\n", n, where)
		fmt.Println("They are already yours — indexing them changes no agent config.")
	}
	loadRulebook := rulebook.LoadDocument
	if dryRun {
		loadRulebook = rulebook.LoadDocumentPreview
	}
	if loaded, loadErr := loadRulebook(); loadErr == nil {
		fmt.Printf("\nRulebook: active=%s selection=%s digest=%s\n", loaded.Origin, loaded.Selection, loaded.Digest)
		if !loaded.Selected {
			if loaded.Selection == "unselected-baseline" {
				fmt.Println("  no rulebook selected; this empty baseline cannot ask or deny.")
				fmt.Println("  review available configuration with: crossing-guard rules select safety-starter")
			} else {
				fmt.Println("  compatibility activation is not consent. Select deliberately with:")
				fmt.Println("  crossing-guard rules select current")
			}
		}
	} else {
		fmt.Printf("\nRulebook: UNAVAILABLE — %v\n", loadErr)
	}

	if governable == 0 {
		fmt.Println("\nNothing to attach.")
		if dryRun {
			return // --dry-run promised "touches nothing" — that includes a network probe
		}
		reportService(dryRun)
		reportConsole()
		return
	}

	// ── plan ──────────────────────────────────────────────────────────────────
	fmt.Println("\nWhat attaching does, per runtime:")
	fmt.Println("  · adds the supported action-lifecycle hooks to that runtime's own config file")
	fmt.Println("  · before-tool capture sees the attempted command, file path, or url")
	fmt.Println("  · only the before-tool phase can allow, ask, or deny according to the active rulebook")
	fmt.Println("  · collection-only runtimes record before/after facts but never evaluate, ask, or deny")
	fmt.Println("  · after-tool and session-end phases record results and code-state checkpoints; they never decide")
	fmt.Println("  · attaching a runtime does NOT select policy configuration")
	fmt.Println("  · decisions and retained collection evidence stay local. Remove hooks with: crossing-guard uninstall")
	if dryRun {
		fmt.Println("\n--dry-run: nothing was changed. Files that WOULD be edited:")
		for _, r := range runtimes {
			if r.Installed && !r.Current {
				fmt.Println("  " + r.Config)
			}
		}
		return
	}

	// ── consent, per runtime ──────────────────────────────────────────────────
	// Per-runtime, because attaching Claude has never implied attaching Codex.
	in := bufio.NewReader(os.Stdin)
	attached := 0
	for _, r := range runtimes {
		if !r.Installed || ((r.Consented || r.Attached) && r.Current) {
			continue
		}
		verb := "Attach"
		if r.Consented || r.Attached {
			verb = "Repair"
		}
		if !assumeYes && !ask(in, fmt.Sprintf("\n%s %s (%s)?", verb, r.Name, r.Config)) {
			fmt.Printf("  skipped — %s stays ungoverned\n", r.Name)
			continue
		}
		if err := guardcli.InstallFor(r.Name, r.Config, self); err != nil {
			fmt.Printf("  FAILED — %v\n", err)
			continue
		}
		attached++
		fmt.Printf("  attached: %s\n", r.Config)
		if r.Manual != "" {
			// The honest half. Codex registers a hook as untrusted and then SKIPS it
			// silently, so "installed" here is a claim about a file, not about
			// enforcement. Saying so is the whole product.
			fmt.Printf("  ACTION REQUIRED — %s\n", r.Manual)
		}
	}
	if attached == 0 {
		fmt.Println("\nNothing was attached.")
	}

	reportService(dryRun)
	reportConsole()
}

// initPlatformWarning presents the existing capability record at the front door. It
// deliberately contains no GOOS switch or copied support table: daemon.PlatformSupport
// remains the one truth that doctor, startup, enforcement, and init all consume.
func initPlatformWarning(support daemon.PlatformSupport) string {
	if support.StatefulEnforcementReady() {
		return ""
	}
	return fmt.Sprintf("\nPlatform: %s — LIMITED, not silently treated as macOS\n"+
		"  Hook attachment and local capture may still be configured.\n"+
		"  Stateful enforcement will NOT arm: service=%s, uninstall=%s, store protection=%s.\n"+
		"  Reason: %s",
		support.GOOS, support.Service, support.Uninstall, support.StoreACL, support.Note)
}

func parseInitArgs(args []string) (dryRun, assumeYes bool, err error) {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&dryRun, "dry-run", false, "show the plan without changing anything")
	flags.BoolVar(&dryRun, "n", false, "show the plan without changing anything")
	flags.BoolVar(&assumeYes, "yes", false, "accept runtime attachment prompts")
	flags.BoolVar(&assumeYes, "y", false, "accept runtime attachment prompts")
	if err := flags.Parse(args); err != nil {
		return false, false, err
	}
	if flags.NArg() != 0 {
		return false, false, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return dryRun, assumeYes, nil
}

// ephemeralBinary reports whether this binary sits somewhere it will not stay.
// The temp dir covers `go run`, scratch builds and unpacked archives; Downloads
// covers the honest mistake of running the thing straight out of the browser.
func ephemeralBinary(self string) (bool, string) {
	return guardcli.EphemeralBinary(self)
}

// ask reads one y/n. A non-interactive stdin (CI, a pipe) must not be read as
// consent: absent a human, the answer is no, and --yes is how a script says yes
// on purpose.
func ask(in *bufio.Reader, prompt string) bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Println(prompt + " no (stdin is not a terminal; pass --yes to consent non-interactively)")
		return false
	}
	fmt.Print(prompt + " [y/N] ")
	line, err := in.ReadString('\n')
	if err != nil {
		// stdin passed the terminal test (/dev/null is a character device) but had
		// nothing to give. Declining is right; declining SILENTLY is not — a reader
		// would see a prompt they never got to answer and no reason why.
		fmt.Println("\n  (no answer — stdin ended; pass --yes to consent non-interactively)")
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// reportService installs the background service rather than promising that something
// else will. It used to print "the daemon installs it on first start", which was true
// only while `serve` adopted any service it found. Now that `serve` refuses to take
// over an installation another live binary owns, `init` is the take-over, and this is
// where it happens — otherwise a machine pointing at a stale build would have no
// repair command at all.
func reportService(dryRun bool) {
	fmt.Println("\nBackground service (keeps capture running across logout/reboot):")
	if dryRun {
		fmt.Println("  --dry-run: the service would be installed or taken over for this binary")
		return
	}
	// Preserve whatever the machine is already configured for: taking over a
	// service must not quietly move the store it points at.
	svc := daemon.AdoptService(daemon.ServiceTarget())
	switch svc.Action {
	case "current":
		fmt.Println("  already installed for this binary — " + svc.Path)
	case "installed", "updated":
		fmt.Println("  installed -> " + svc.Path)
		if svc.Detail != "" {
			fmt.Println("  " + svc.Detail)
		}
	case "unsupported":
		fmt.Println("  not managed on this platform — " + svc.Detail)
		fmt.Println("  keep it running another way, or live capture stops when it exits")
	default:
		fmt.Println("  NOT installed — " + svc.Detail)
		fmt.Println("  start it by hand with: crossing-guard serve")
	}
}

// reportConsole closes the loop the product never closed: it says where the
// thing you just installed actually is.
func reportConsole() {
	loc, err := daemon.LocateConsole()
	if err != nil {
		fmt.Println("\nConsole: not running yet. Start it with: crossing-guard serve")
		fmt.Println("Then:    crossing-guard console --open")
		return
	}
	if problem := daemon.ProbeConsole(loc); problem != "" {
		fmt.Println("\nConsole: " + problem)
		return
	}
	fmt.Println("\nConsole: " + loc.URL())
	fmt.Println("Open it any time with: crossing-guard console --open")
	// The second first-run beat. Named rather than performed: a governance tool
	// that stages a block during install — even a harmless one — has taken an
	// action the user did not ask for, on the run where they are deciding whether
	// to trust it.
	fmt.Println("\nWant to see it actually block something? crossing-guard demo")
	// The other half of the product, which init does NOT wire. Enforcement watches
	// what an agent does; memory injection puts your stored memory INTO its context,
	// which is a different thing to consent to and deserves its own yes rather than
	// riding along on this one. Naming it is the fix for it being undiscoverable —
	// it was attachable only by a verb nothing mentioned.
	fmt.Println("Memory injection is a separate attachment (your memory, into the agent's")
	fmt.Println("context): crossing-guard attach <runtime> — status shows in crossing-guard doctor")
}

// corpusSummary counts the session corpora already on disk, per runtime.
//
// It asks the harvest package directly — the layer that owns vendor session
// paths. The first version hardcoded ~/.claude/projects and ~/.codex/sessions,
// and vendor-lint caught it: four vendor tokens in generic code, meaning a new
// runtime would require MODIFYING this function (the open/closed violation ADR
// 0020 exists to prevent). The second version routed through daemon.ScanSessions,
// which is a passthrough — importing the whole daemon for a print-only count.
func corpusSummary() (int, string) {
	counts := map[string]int{}
	for _, s := range harvest.ScanSessions() {
		counts[s.Runtime]++
	}
	total := 0
	names := make([]string, 0, len(counts))
	for name, n := range counts {
		total += n
		names = append(names, name)
	}
	sort.Strings(names) // stable output; map order is randomised per range
	return total, strings.Join(names, " and ")
}
