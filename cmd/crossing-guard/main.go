// crossing-guard — one binary, with package-local verb implementations dispatched here.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"crossing-guard/internal/approvalbridge"
	"crossing-guard/internal/daemon"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/memcli"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sessions", "memory", "attach", "import", "sync", "migrate", "harvest":
		memcli.Main(os.Args[1:], locateMemoryDaemon)
	case "doctor":
		doctorCmd(os.Args[2:])
	case "rules":
		rulesCmd(os.Args[2:])
	case "hook", "govern-hook", "collect-hook", "check", "tags", "coverage", "install", "detectors", "entities", "enforce", "chain":
		guardcli.Main(os.Args[1:])
	case "uninstall":
		uninstall(os.Args[2:])
	case "export":
		export(os.Args[2:])
	case "prune":
		prune(os.Args[2:])
	case "compact":
		compact(os.Args[2:])
	case "import-sessions":
		importSessions(os.Args[2:])
	case "index-memory":
		indexMemory(os.Args[2:])
	case "serve":
		daemon.Main(os.Args[2:])
	case "init":
		initCmd(os.Args[2:])
	case "console":
		console(os.Args[2:])
	case "link":
		linkCmd(os.Args[2:])
	case "unlink":
		unlinkCmd(os.Args[2:])
	case "layers":
		layersCmd(os.Args[2:])
	case "routes":
		routesCmd(os.Args[2:])
	case "agents":
		agentsCmd(os.Args[2:])
	case "org-key":
		orgKeyCmd(os.Args[2:])
	case "bundle":
		bundleCmd(os.Args[2:])
	case "handoff":
		os.Exit(handoffCmd(os.Args[2:]))
	case "verify":
		verify(os.Args[2:])
	case "demo":
		demo(os.Args[2:])
	case "change":
		os.Exit(runChange(os.Args[2:]))
	case "understand":
		os.Exit(runUnderstand(os.Args[2:]))
	case "analyzers":
		os.Exit(runAnalyzers(os.Args[2:]))
	case "mcp":
		os.Exit(mcpCmd(os.Args[2:]))
	case approvalbridge.Command:
		os.Exit(approvalbridge.Main(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

// parseOpenArgs is the exact shared option contract of console and demo, not a
// general command framework.
func parseOpenArgs(command string, args []string) (bool, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var open bool
	flags.BoolVar(&open, "open", false, "open in the default browser")
	flags.BoolVar(&open, "o", false, "open in the default browser")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if flags.NArg() != 0 {
		return false, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return open, nil
}

func mustHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve home dir:", err)
		os.Exit(1)
	}
	return home
}

// dataDir is shared by init, export, and uninstall. It stays in the command shell
// rather than being copied into verb files or exported from an unrelated package.
func dataDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".crossing-guard")
	}
	return ".crossing-guard"
}

var topLevelUsage = `crossing-guard — one memory, one rulebook, every agent

PUBLIC ALPHA CANDIDATE — USER JOURNEY
These are candidate stability labels, not proof that a detected runtime is supported.
Use doctor and verify for this machine's exact evidence.

  init [--dry-run] [--yes]
      start here: inspect changes, consent per runtime, attach, and find the console.

  doctor [--json]
      report service, configuration, platform, and observed per-runtime firing evidence.

  rules status | select (current|starter|none|PATH) [--yes] | unselect [--yes]
      inspect and explicitly select exact rulebook bytes, or return to the cohort baseline.

  check "<command>"
      dry-run the active rule evaluator; this does not prove a vendor hook fires.

  console [--open]
      probe the daemon of record and print or open its authenticated loopback URL.

  verify [--timeout SECONDS]
      watch for real hook firing rather than treating registration as proof.

  demo [--open]
      exercise the canary when the selected rulebook owns it; never selects policy.

  uninstall [--purge]
      remove owned hooks/service; purge additionally deletes retained local product data.

  export [PATH]
      write a consistent store snapshot for backup/current-product use.

  prune (--keep-days N | --before YYYY-MM-DD) [--usage] [--yes]
      preview retention changes; --yes commits them. Export first.
      --usage trims recorded usage calls instead of events.

  compact [--data DIR]
      rewrite the store file without its free pages. Stop the daemon first;
      refuses while one may be using the store.

EXPERIMENTAL SOURCE-VISIBLE TOOLS
These commands work where documented, but output and compatibility may change.

  sessions
      list, show, or search locally indexed vendor sessions.
  memory
      manage the experimental cross-vendor memory store.
  import
      import implemented local sources; run it for exact usage.
  harvest
      rebuild or refresh the local session index.
  change
      record and inspect declarations, snapshots, claims, and verification witnesses.
  understand
      build or inspect immutable source/analyzer/configuration-bound facts.
  link <server-url> [--name <device name>]
      enroll this device with a team server: prints a code you approve in the
      browser, then the daemon sends a device report on its cadence.
  unlink
      revoke this device's key on the team server and forget the link.
  routes list | show <id|name> | create --name <name> --family <family> … | rename <id|name> <name> | delete <id|name>
      named model routes: where a place sends its model calls. A place names a
      route; nothing else names a runtime, a model or an endpoint.
  agents place --profile <agent id> --route <route name|id> [--root <repository>] [--mode <mode>]
      turn an agent on for a repository on a model route.
  layers [--adopt <scope>] [--unadopt <scope>] [--repin <fingerprint>]
      list the team bundles this device verified and adopted; --adopt and
      --unadopt are the explicit local actions after the shown diff; --repin
      trusts a new organization key by the fingerprint the server presents.
  org-key init --out <path> | show --key <path>
      make the organization's signing key as a file you keep (no daemon holds
      it), or print its public half for the team console's Register key form.
  bundle build --scope organization|repository [--agent <id>]... | sign --key <path> <file>
      build the next revision of a shared bundle, unsigned, on a linked device;
      sign it with the organization key, ready to upload in the team console.
  handoff send|list|show|decline|withdraw|close|clean
      hand a session to a teammate and manage what was sent and received; clean
      removes handoff files earlier versions wrote into a checkout. Opening a
      handoff is done in the console.
  analyzers
      inspect and explicitly select exact content-addressed native analyzer modules.
  entities
      inspect governed resources and their evidence.
  tags
      classify a supplied event through the active detector assembly.
  coverage
      print the compiled coverage label per rule.
  detectors
      inspect, preview, select, unselect, initialize, or lint detector documents.
  enforce
      inspect or change the local enforcement stand-down state.
  chain verify <vendor/id>
      recompute a session's event chain; the running daemon checks the tail it holds.
  import-sessions
      backfill historical sessions as explicitly lossy imported events.
  index-memory
      fold local memory documents into the searchable governance store.

INTERNAL / INSTALLED MACHINE INTERFACES
Visible for troubleshooting and integration ownership; not alpha compatibility promises.

  serve [--addr HOST:PORT] [--data DIR]
      run the authenticated loopback daemon and bundled console.
  hook
      runtime-installed policy hook interface.
  govern-hook
      plugin-installed headless decision interface (evaluate one tool call, answer JSON).
  collect-hook
      runtime-installed collection-only hook interface.
  install
      lower-level hook installer; normal first run uses init.
  attach
      lower-level legacy memory-hook attachment; normal first run uses init.
  sync
      installed lifecycle memory/index synchronization.
  migrate
      internal storage migration entrypoint.
  mcp --runtime <runtime>
      the recall tools agent sessions call (stdio MCP server); init --recall
      registers it with each runtime.
` + approvalbridge.Usage

func usage() {
	fmt.Fprint(os.Stderr, topLevelUsage)
}
