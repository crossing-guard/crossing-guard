package memcli

// Package memcli is the memory + session-reference CLI surface (the former
// cpmem experiment, folded at M4 slice A). It layers command parsing over
// the shared packages (harvest, store, memory) and owns attach/doctor for
// the injection hooks.
//
//   sessions list|show|search   unified view over ~/.claude + ~/.codex (read-only)
//   memory   upsert|get|list|search|index|log
//   attach   claude|codex       install SessionStart injection hooks

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"crossing-guard/internal/transcriptindex"
)

// Main is the memcli entry point: args is everything after the binary name
// (cmd/crossing-guard dispatches here). locate finds the daemon the memory read
// verbs read through; the caller owns the client so this package never imports
// the daemon's. It may os.Exit.
func Main(args []string, locate func() (Daemon, error)) {
	locateDaemon = locate
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	rest := args[1:]
	switch args[0] {
	case "sessions":
		cmdSessions(rest)
	case "memory":
		cmdMemory(rest)
	case "attach":
		cmdAttach(rest)
	case "import":
		cmdImport(rest)
	case "sync":
		cmdSync(rest)
	case "migrate":
		cmdMigrate(rest)
	case "harvest":
		cmdHarvest(rest)
	default:
		usage()
		os.Exit(2)
	}
}

func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	project := fs.String("project", "", "import only this repository's memory")
	pending := fs.Bool("pending", false, "route to pending/ instead of tier-A auto-promote")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if len(pos) < 1 || pos[0] != "claude-memory" {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard import claude-memory [--project r] [--pending]")
		os.Exit(2)
	}
	st, err := importClaudeMemory(memoryDir(), *project, *pending)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("import claude-memory: %d projects, %d files → %d imported, %d skipped, %d tombstoned\n",
		st.Projects, st.Files, st.Imported, st.Skipped, st.Tombstoned)
}

// cmdSync is an explicit recovery surface: re-import vendor auto-memory (tier A,
// idempotent/mtime-gated), then incrementally refresh transcript projections. The
// daemon coordinator is the normal transcript refresh owner; legacy memory attachments
// may still invoke this command at a session boundary.
func cmdSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	quiet := fs.Bool("quiet", false, "hook mode: no output unless something changed")
	fs.Parse(args)
	ist, ierr := importClaudeMemory(memoryDir(), "", false)
	hst, herr := refreshTranscriptIndex(transcriptindex.RefreshModeIncremental)
	if ierr != nil || herr != nil {
		fmt.Fprintln(os.Stderr, "sync:", ierr, herr)
		os.Exit(1)
	}
	if !*quiet || ist.Imported > 0 || hst.Stats.Updated > 0 || hst.Stats.Failed > 0 {
		fmt.Printf("sync: %d memories imported; transcripts %d updated, %d skipped, %d failed; coverage %s\n",
			ist.Imported, hst.Stats.Updated, hst.Stats.Skipped, hst.Stats.Failed,
			hst.Coverage.State)
	}
}

func cmdMigrate(args []string) {
	st, err := migrateStore(memoryDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("migrate: %d scanned, %d rewritten to format 1, %d ids de-stuttered\n", st.Scanned, st.Rewritten, st.Renamed)
}

func cmdHarvest(args []string) {
	fs := flag.NewFlagSet("harvest", flag.ExitOnError)
	rebuild := fs.Bool("rebuild", false, "force-refresh transcript projections; preserve the shared store")
	fs.Parse(args)
	mode := transcriptindex.RefreshModeIncremental
	if *rebuild {
		mode = transcriptindex.RefreshModeForce
	}
	result, err := refreshTranscriptIndex(mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st := result.Stats
	fmt.Printf("transcript projection %s: %d discovered, %d planned, %d updated, %d skipped, %d failed, %d remaining, %d orphaned; coverage %s\n",
		indexDB(), st.Discovered, st.Planned, st.Updated, st.Skipped, st.Failed,
		st.Remaining, st.Orphaned, result.Coverage.State)
}

// readHookPayload transports bounded legacy hook input without interpreting
// native fields. Explicit runtime selection does not need or read this input.
func readHookPayload() []byte {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	return raw
}

func printJSON(v any) {
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
}

func usage() {
	fmt.Fprintln(os.Stderr, `crossing-guard — experimental memory + session reference

These commands are source-visible alpha experiments. Their output and storage
contracts may change; use "crossing-guard init" for the supported first-run path.

  crossing-guard sessions list   [--vendor NAME] [--limit N] [--project substr]
  crossing-guard sessions show   <session-id-prefix> [--limit N]
  crossing-guard sessions search <query> [--limit N]

  crossing-guard memory upsert --id <slug> --title <t> --category <c>
                      [--tags a,b] [--aliases x,y] [--repository r] [--source s]
                      (body from stdin or --content)
  crossing-guard memory get <id> [--json] | list [--status s] [--json] | log [--limit N]
  crossing-guard memory search <query> [--why] [--json] [--category c] [--repository r] [--tag t] [--limit N]
  crossing-guard memory new [id]        (template in $EDITOR, checklist included)
  crossing-guard memory edit <id>       ($EDITOR; validate + restamp on save)
  crossing-guard memory note "<text>"   (quick capture -> pending/, curate later)
  crossing-guard memory promote <id>    (pending/ -> store; the human act)
  crossing-guard memory reject <id> --reason "..."  (pending/ -> rejected/)
  crossing-guard memory delete <id>     (remove + tombstone; retained in local history)
  crossing-guard memory verify <id>     (dated attestation: verified YYYY-MM-DD by user)
  crossing-guard memory take-team-version <id>  (give up the edit made here; the team's version returns)
  crossing-guard memory index  [--max-bytes N] [--project r] [--runtime NAME]

  crossing-guard harvest [--rebuild]    (refresh transcript projections; --rebuild forces projection replacement only)
  crossing-guard sync [--quiet]         (recovery: import vendor memory + incremental transcript refresh)
  crossing-guard import claude-memory [--project r] [--pending]
  crossing-guard migrate                (rewrite all records to current format)
  crossing-guard attach claude [--settings PATH]   (default ~/.claude/settings.json)
  crossing-guard attach codex  [--config PATH]     (default ~/.codex/config.toml)
store: $CG_MEMORY_DIR or ~/.crossing-guard/memory; index: $CG_INDEX or ~/.crossing-guard/index.sqlite
(legacy $CPMEM_DIR / $CPMEM_INDEX still read)
memory get/list/search/index read through the running daemon and refuse when the index it
reports is not the index above; the other memory verbs open the index themselves.`)
}

// ---------- sessions ----------

func findSession(prefix string) (Session, bool) {
	for _, s := range ListSessions("") {
		if strings.HasPrefix(s.ID, prefix) {
			return s, true
		}
	}
	return Session{}, false
}

func openEditor(path string) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor) // allow EDITOR="code --wait"
	cmd := exec.Command(parts[0], append(parts[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// ---------- attach ----------

func cmdAttach(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	adapter, ok := adapters[args[0]]
	if !ok {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet(args[0], flag.ExitOnError)
	config := fs.String(adapter.ConfigFlag(), adapter.DefaultConfigPath(), "")
	fs.Parse(args[1:])
	if _, err := attachAt(adapter, *config, self); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ---------- helpers ----------

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// splitPositional lets flags trail positionals (Go's flag pkg stops at the
// first non-flag token): everything before the first "-"-prefixed token is
// positional, the rest goes to the FlagSet.
func splitPositional(args []string) (pos, flags []string) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}
