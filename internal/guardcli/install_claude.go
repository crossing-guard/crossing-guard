package guardcli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/vendorconfig"
	"crossing-guard/internal/vendorpaths"
)

func init() { registerHookInstaller(claudeInstaller{}) }

// claudeMatchAll is Claude Code's documented match-ALL matcher ("*", "" and an
// omitted field are equivalent). Named because the value is load-bearing: it is the
// difference between governing every tool call and governing only Bash.
const claudeMatchAll = "*"

// claudeVendor is this installer's name, in ONE place: it is written into the hook
// command as --runtime, so it becomes the value the event log attributes actions
// to. A literal repeated across Name/Install/IsCurrent could drift, and the drift
// would show up as events attributed to a runtime nobody installed.
const claudeVendor = "claude"

// claudeHookTable is the ONLY place Claude's lifecycle event names exist.
// Each row maps one vendor event to the matcher it needs and, for turn-
// boundary events, to OUR kind — which the installer bakes into the command as
// `--observe <kind>` so the binary never has to know the vendor's name for it.
// Owner decision 2026-09-01: install every lifecycle event the vendor offers.
type claudeHookRow struct {
	Event, Matcher, Observe string
	// Context marks the events at which the vendor documents
	// hookSpecificOutput.additionalContext (hooks reference, 2026-09-12):
	// UserPromptSubmit, PreToolUse, PostToolUse, PostToolUseFailure. Stop is
	// deliberately NOT one — context there continues the turn.
	Context bool
}

var claudeHookTable = []claudeHookRow{
	{Event: "SessionStart", Matcher: claudeSessionEntryMatcher},
	{Event: "PreToolUse", Matcher: claudeMatchAll, Context: true},
	{Event: "PostToolUse", Matcher: claudeMatchAll, Context: true},
	{Event: "PostToolUseFailure", Matcher: claudeMatchAll, Context: true},
	{Event: "SessionEnd"},
	{Event: "UserPromptSubmit", Observe: "turn.started", Context: true},
	{Event: "Stop", Observe: "turn.ended"},
	// Only a real ask is an ask: the idle nudge Claude fires a minute after
	// every reply would otherwise re-raise "needs your input" forever.
	{Event: "Notification", Matcher: claudeNotificationMatcher, Observe: "input.requested"},
	{Event: "SubagentStop", Observe: "subagent.ended"},
	{Event: "PreCompact", Observe: "context.compacted"},
}

var claudeLifecycleHookEvents = func() []string {
	events := make([]string, 0, len(claudeHookTable))
	for _, row := range claudeHookTable {
		events = append(events, row.Event)
	}
	return events
}()

const (
	claudeSessionEntryMatcher = "startup|resume|clear|compact"
	claudeNotificationMatcher = "permission_prompt|elicitation_dialog"
)

func claudeHookRowFor(event string) claudeHookRow {
	for _, row := range claudeHookTable {
		if row.Event == event {
			return row
		}
	}
	return claudeHookRow{Event: event}
}

// claudeHookCommandFor is the exact command written for one event.
func claudeHookCommandFor(exe, event string) string {
	command := hookCommand(exe, claudeVendor)
	if row := claudeHookRowFor(event); row.Observe != "" {
		command += " --observe " + row.Observe
	}
	return command
}

// claudeInstaller merges match-ALL action/result hooks and a session-close hook into
// Claude settings.json.
type claudeInstaller struct{}

func (claudeInstaller) Name() string       { return claudeVendor }
func (claudeInstaller) SelectFlag() string { return "--settings" }
func (claudeInstaller) DefaultConfig() string {
	return filepath.FromSlash(vendorpaths.ClaudeSettingsRelative)
}
func (claudeInstaller) IsDefault() bool { return true }

// ResolveConfig returns the potential user-level settings target. Its existence
// does not establish client presence; installing into a fresh file remains valid.
func (claudeInstaller) ResolveConfig() string {
	home := homeJoin()
	if home == "" {
		return ""
	}
	return filepath.Join(home, filepath.FromSlash(vendorpaths.ClaudeSettingsRelative))
}

// RuntimePresent uses the same passive executable lookup as the chat launcher.
func (claudeInstaller) RuntimePresent() bool {
	_, err := ResolveRuntimeBinary(claudeVendor, "")
	return err == nil
}

// ManualStep: none — Claude honors an installed hook immediately.
func (claudeInstaller) ManualStep() string { return "" }

// Claude's established hook deadline is the source of the existing shared budget.
// Declaring it keeps a future runtime from inheriting that timing by omission.
func (claudeInstaller) HookAskBudget() time.Duration { return MaxAskBudget }

// claudeHookContextCap: the vendor caps hook output strings at 10,000
// characters (hooks reference, 2026-09-12); stay under it in bytes.
const claudeHookContextCap = 9000

var claudeContextEncoding = func() hookContextEncoding {
	events := map[string]bool{}
	for _, row := range claudeHookTable {
		if row.Context {
			events[row.Event] = true
		}
	}
	return hookContextEncoding{events: events, capBytes: claudeHookContextCap}
}()

func (claudeInstaller) hookContextCap() int { return claudeHookContextCap }

func (claudeInstaller) EncodeHookContext(rawEvent, context string) ([]byte, bool) {
	return claudeContextEncoding.encode(rawEvent, context)
}

// HookRunsInSubagent: Claude hooks fired inside a sub-agent (Agent/Task tool,
// foreground or background) carry the parent's session_id and transcript_path
// plus agent_id; main-thread hooks, including a `--agent` main thread, carry no
// agent_id (probed on 2.1.280, 2026-09-25; testdata/claude_2_1_280_*.json).
// The vendor's own schema says the same: "Absent for the main thread, even in
// --agent sessions. Use this field (not agent_type)".
func (claudeInstaller) HookRunsInSubagent(agentID string) bool { return agentID != "" }

// NestedCallKinds: the same probe saw agent_id on a sub-agent's tool hooks
// (pre and post), and a sub-agent fires no prompt event at all (measured on
// 2.1.289, 2026-10-03), so a nested call is told apart at every kind that
// carries context.
func (claudeInstaller) NestedCallKinds() []string {
	return []string{"turn.started", "tool.started", "tool.completed"}
}

func (claudeInstaller) EncodeHookDeny(rawEvent, reason string) []byte {
	return hookSpecificOutputDeny(rawEvent, reason)
}

// HookBinary: for Claude the config path IS the settings file.
func (claudeInstaller) HookBinary(settingsPath string) string {
	return ourHookBinary(settingsPath)
}

// IsCurrent requires BOTH our command and match-ALL coverage. A hook registered
// with a narrow matcher (e.g. the historical "Bash") is present but blind, which is
// the failure that let 14 Edit calls through unobserved — so it counts as NOT current.
func (claudeInstaller) IsCurrent(settingsPath, exe string) bool {
	for _, current := range (claudeInstaller{}).HookPhaseStatus(settingsPath, exe) {
		if !current {
			return false
		}
	}
	return true
}

func (claudeInstaller) HookPhaseStatus(settingsPath, exe string) map[string]bool {
	status := make(map[string]bool, len(claudeLifecycleHookEvents))
	for _, event := range claudeLifecycleHookEvents {
		owned, current, ok := hookEventStatus(settingsPath, event, claudeHookCommandFor(exe, event), claudeMatcherIsCurrent(event))
		status[event] = ok && owned == 1 && current == 1
	}
	return status
}

// Uninstall removes only our lifecycle handlers from settings.json, preserving foreign
// handlers, matcher groups, and every other top-level key.
func (claudeInstaller) Uninstall(settingsPath, exe string) (bool, error) {
	return removeOurHookEvents(settingsPath, claudeLifecycleHookEvents...)
}

func (claudeInstaller) Install(settingsPath, exe string) error {
	snapshot, err := vendorconfig.Read(settingsPath)
	if err != nil {
		return err
	}
	settings := map[string]any{}
	if snapshot.Exists {
		// A corrupt settings.json must be an ERROR, not an empty merge base: the
		// swallowed version left `settings` empty and the write below replaced the
		// user's whole file with a hook-only config — install destroying the very
		// config it promised to preserve. memcli's adapter already refuses here;
		// the two writers to this file must agree that corrupt means stop.
		if err := json.Unmarshal(snapshot.Data, &settings); err != nil {
			return fmt.Errorf("%s is not valid JSON (%w) — fix it first; refusing to overwrite it", settingsPath, err)
		}
	}
	if err := validateHookEventShapes(settings, claudeLifecycleHookEvents...); err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	allCurrent := true
	for _, event := range claudeLifecycleHookEvents {
		owned, current := hookEventCounts(settings, event, claudeHookCommandFor(exe, event), claudeMatcherIsCurrent(event))
		allCurrent = allCurrent && owned == 1 && current == 1
	}
	if allCurrent {
		fmt.Println("already installed:", settingsPath)
		return nil
	}
	// "*" is the documented match-ALL matcher (equivalently "" or omitted). It must be
	// all tools: a Bash-only matcher never sees Edit/Write/WebFetch/MCP, which is how a
	// file could be rewritten 14 times with the guard installed and observe nothing.
	// MCP tools (mcp__server__tool) do fire PreToolUse and are covered by "*".
	removeOurHooks(&settings, claudeLifecycleHookEvents...)
	for _, row := range claudeHookTable {
		appendHookEvent(settings, row.Event, row.Matcher, claudeHookCommandFor(exe, row.Event))
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	backup, err := vendorconfig.Replace(settingsPath, snapshot, append(b, '\n'))
	if err != nil {
		return err
	}
	fmt.Printf("installed %s hooks -> %s\n", strings.Join(claudeLifecycleHookEvents, "/"), settingsPath)
	if backup != "" {
		fmt.Printf("backup (immediate prior) -> %s\n", backup)
	}
	if loaded, loadErr := rulebook.LoadDocument(); loadErr == nil {
		fmt.Printf("rules: %s [%s]\n", loaded.Path, loaded.Selection)
	} else {
		fmt.Printf("rules: unavailable (%v)\n", loadErr)
	}
	fmt.Printf("run:   claude --settings %s\n", settingsPath)
	return nil
}

// claudeMatcherIsCurrent judges an entry's matcher against the table row for
// its event — keyed per event, because a Notification entry without its
// sub-type matcher is not current even though a PreToolUse one would be.
func claudeMatcherIsCurrent(event string) func(map[string]any) bool {
	row := claudeHookRowFor(event)
	return func(entry map[string]any) bool {
		matcher, present := entry["matcher"]
		value, _ := matcher.(string)
		switch row.Matcher {
		case "", claudeMatchAll:
			return !present || value == claudeMatchAll || value == ""
		default:
			return present && value == row.Matcher
		}
	}
}

func (claudeInstaller) NormalizeSessionEntrySource(source string) string {
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
