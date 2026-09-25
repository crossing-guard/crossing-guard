package guardcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/vendorconfig"
)

func init() { registerHookInstaller(codexInstaller{}) }

// codexInstaller writes the supported lifecycle hooks into $CODEX_HOME/hooks.json.
// Event keys are CamelCase — proven live via the app-server hooks/list endpoint
// (2026-07-16): "PreToolUse" registers; "pre_tool_use" is SILENTLY IGNORED (no
// hook, no warning). No tool matcher: Codex shell tools vary; the shim regexes
// the command. HONEST LIMIT: the registered hook is trustStatus:"untrusted" —
// Codex silently skips it until trusted (TUI /hooks) or the bypass flag. Every
// install must be followed by a firing canary; registration != firing.
type codexInstaller struct{}

// codexVendor is this installer's name, in ONE place — it is written into the hook
// command as --runtime and becomes the value the event log attributes actions to.
const codexVendor = "codex"

// codexHookTable is the ONLY place Codex's event names exist. The first four
// are proven to register and fire (PascalCase, on disk). The rest are
// installed on the owner's decision to attach every event the vendor offers,
// but are PROBE-GATED: Codex silently ignores an unknown key and silently
// skips an untrusted one, so no adapter declares these as capabilities until
// a firing canary has seen a row. Until then a session simply has no such
// rows and the status decider reads it honestly as unknown.
type codexHookRow struct {
	Event, Observe string
	Probe          bool
	// Context marks the events at which the vendor documents
	// hookSpecificOutput.additionalContext (hooks page, 2026-09-12):
	// SessionStart, UserPromptSubmit, PreToolUse, PostToolUse. Stop accepts a
	// block decision only.
	Context bool
}

var codexHookTable = []codexHookRow{
	{Event: "SessionStart"},
	{Event: "PreToolUse", Context: true},
	{Event: "PostToolUse", Context: true},
	{Event: "SessionEnd"},
	{Event: "UserPromptSubmit", Observe: "turn.started", Probe: true, Context: true},
	{Event: "Stop", Observe: "turn.ended", Probe: true},
	{Event: "PermissionRequest", Observe: "input.requested", Probe: true},
	{Event: "SubagentStart", Observe: "subagent.started", Probe: true},
	{Event: "SubagentStop", Observe: "subagent.ended", Probe: true},
	{Event: "PreCompact", Observe: "context.compacted", Probe: true},
}

var codexLifecycleHookEvents = func() []string {
	events := make([]string, 0, len(codexHookTable))
	for _, row := range codexHookTable {
		events = append(events, row.Event)
	}
	return events
}()

func codexHookCommandFor(exe, event string) string {
	command := hookCommand(exe, codexVendor)
	for _, row := range codexHookTable {
		if row.Event == event && row.Observe != "" {
			return command + " --observe " + row.Observe
		}
	}
	return command
}

func (codexInstaller) Name() string          { return codexVendor }
func (codexInstaller) SelectFlag() string    { return "--codex-home" }
func (codexInstaller) DefaultConfig() string { return "" }
func (codexInstaller) IsDefault() bool       { return false }

// ResolveConfig: $CODEX_HOME, else ~/.codex — but only if that directory actually
// exists, so a machine without Codex reports "absent" instead of us fabricating a
// config for a runtime the user does not have.
func (codexInstaller) ResolveConfig() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home = homeJoin(".codex")
	}
	if home == "" {
		return ""
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		return ""
	}
	return home
}

// ManualStep is the honest one: Codex registers a hook as trustStatus:"untrusted"
// and SILENTLY skips it — no hook, no warning — until a human trusts it. We cannot
// automate this, so we say it loudly every time rather than let an install look
// complete when nothing will fire.
func (codexInstaller) ManualStep() string {
	return "trust the hook in the Codex TUI (/hooks) — Codex silently skips untrusted " +
		"hooks; then prove it fires with a canary. Registration is NOT firing."
}

// Codex's established hook deadline supports the existing shared wait budget.
// Runtime timing remains provider-owned rather than a default for every new adapter.
func (codexInstaller) HookAskBudget() time.Duration { return MaxAskBudget }

// codexHookContextCap: the vendor's additionalContextLimit defaults to 2,500
// tokens (hooks page, 2026-09-12). 7,000 bytes stays under it for prose in
// Latin scripts (~4 bytes per token); CJK-heavy text (~3 bytes per token)
// lands near the limit, and the vendor spills the excess to disk rather than
// failing the hook.
const codexHookContextCap = 7000

var codexContextEncoding = func() hookContextEncoding {
	events := map[string]bool{}
	for _, row := range codexHookTable {
		if row.Context {
			events[row.Event] = true
		}
	}
	return hookContextEncoding{events: events, capBytes: codexHookContextCap}
}()

func (codexInstaller) EncodeHookContext(rawEvent, context string) ([]byte, bool) {
	return codexContextEncoding.encode(rawEvent, context)
}

func (codexInstaller) EncodeHookDeny(rawEvent, reason string) []byte {
	return hookSpecificOutputDeny(rawEvent, reason)
}

// HookBinary: Codex's config path is its HOME DIRECTORY, so the file to read is
// hooks.json inside it — the same append IsCurrent/Install/Uninstall all do.
func (codexInstaller) HookBinary(codexHome string) string {
	return ourHookBinary(filepath.Join(codexHome, "hooks.json"))
}

// IsCurrent checks hooks.json for our command. Codex takes no tool matcher (its
// shell tool names vary), so presence of the right command is the whole test.
func (codexInstaller) IsCurrent(codexHome, exe string) bool {
	for _, current := range (codexInstaller{}).HookPhaseStatus(codexHome, exe) {
		if !current {
			return false
		}
	}
	return true
}

func (codexInstaller) HookPhaseStatus(codexHome, exe string) map[string]bool {
	path := filepath.Join(codexHome, "hooks.json")
	status := make(map[string]bool, len(codexLifecycleHookEvents))
	for _, event := range codexLifecycleHookEvents {
		owned, current, ok := hookEventStatus(path, event, codexHookCommandFor(exe, event), func(map[string]any) bool { return true })
		status[event] = ok && owned == 1 && current == 1
	}
	return status
}

// Uninstall removes our hooks from $CODEX_HOME/hooks.json. Codex's config path is the
// HOME dir (Install/IsCurrent append hooks.json), so uninstall does the same.
func (codexInstaller) Uninstall(codexHome, exe string) (bool, error) {
	return removeOurHookEvents(filepath.Join(codexHome, "hooks.json"), codexLifecycleHookEvents...)
}

func (codexInstaller) Install(codexHome, exe string) error {
	path := filepath.Join(codexHome, "hooks.json")
	snapshot, err := vendorconfig.Read(path)
	if err != nil {
		return err
	}
	cfg := map[string]any{}
	if snapshot.Exists {
		if err := json.Unmarshal(snapshot.Data, &cfg); err != nil {
			return fmt.Errorf("%s is not valid JSON (%w) — fix it first; refusing to overwrite it", path, err)
		}
	}
	if err := validateHookEventShapes(cfg, codexLifecycleHookEvents...); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	allCurrent := true
	for _, event := range codexLifecycleHookEvents {
		owned, current := hookEventCounts(cfg, event, codexHookCommandFor(exe, event), func(map[string]any) bool { return true })
		allCurrent = allCurrent && owned == 1 && current == 1
	}
	if allCurrent {
		fmt.Println("already installed:", path)
		return nil
	}
	removeOurHooks(&cfg, codexLifecycleHookEvents...)
	for _, event := range codexLifecycleHookEvents {
		appendHookEvent(cfg, event, "", codexHookCommandFor(exe, event))
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	backup, err := vendorconfig.Replace(path, snapshot, append(b, '\n'))
	if err != nil {
		return err
	}
	fmt.Printf("installed %s hooks -> %s\n", strings.Join(codexLifecycleHookEvents, "/"), path)
	fmt.Println("NOTE: the turn-boundary events are installed on the owner's decision but are probe-gated —")
	fmt.Println("      Codex ignores unknown keys silently; re-trust in the TUI (/hooks) and run a canary before relying on them.")
	if backup != "" {
		fmt.Printf("backup (immediate prior) -> %s\n", backup)
	}
	if loaded, loadErr := rulebook.LoadDocument(); loadErr == nil {
		fmt.Printf("rules: %s [%s]\n", loaded.Path, loaded.Selection)
	} else {
		fmt.Printf("rules: unavailable (%v)\n", loadErr)
	}
	fmt.Println("NOTE: Codex silently skips untrusted hooks — verify firing (canary), never assume.")
	return nil
}

func (codexInstaller) NormalizeSessionEntrySource(source string) string {
	switch source {
	case "startup":
		return "start"
	case "resume":
		return "resume"
	case "clear":
		return "context-reset"
	case "compact":
		return "context-compact"
	default:
		return "unknown"
	}
}
