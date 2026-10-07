package guardcli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// HookInstaller is the vendor seam. Implementations self-register from
// install_<vendor>.go, so orchestration never names a vendor.
type HookInstaller interface {
	Name() string
	SelectFlag() string
	DefaultConfig() string
	IsDefault() bool
	Install(configPath, selfPath string) error
	ResolveConfig() string
	IsCurrent(configPath, selfPath string) bool
	ManualStep() string
	// HookBinary reports the configured binary independently of IsCurrent. A stale
	// path is still evidence of prior consent and a missing binary is a silent guard
	// failure, so a boolean cannot carry the diagnosis.
	HookBinary(configPath string) string
	// Uninstall recognizes historical hook spellings and removes only our entries;
	// complete reversal is a deployability requirement, not optional cleanup.
	Uninstall(configPath, selfPath string) (removed bool, err error)
}

// HookPhaseReporter makes partial lifecycle installation visible without widening
// the installer contract for a future runtime that cannot expose phase detail.
type HookPhaseReporter interface {
	HookPhaseStatus(configPath, selfPath string) map[string]bool
}

// CollectionOnlyInstaller marks an attachment that records runtime facts but has no
// policy decision surface. Setup/status callers use this to avoid calling it governed.
// The answer may depend on which artifact is installed (the OpenCode version gate
// installs either a decision-lane or a collection-only plugin), so the installed
// config path rides along; "" means nothing is attached and the answer describes
// the posture a connect would apply now.
type CollectionOnlyInstaller interface {
	CollectionOnly(configPath string) bool
}

// SessionEntryNormalizer keeps runtime-native entry-source vocabulary at the edge.
// Unknown values remain retained in the envelope and normalize to "unknown".
type SessionEntryNormalizer interface {
	NormalizeSessionEntrySource(source string) string
}

// RuntimePresenceReporter separates an installed client from a surviving config file.
// The latter must remain discoverable for uninstall, but must not certify that the
// vendor application is installed.
type RuntimePresenceReporter interface {
	RuntimePresent() bool
}

// RuntimeDisplayNamer names a runtime for people when nothing else does: no chat
// capability and no connection descriptor carries its name.
type RuntimeDisplayNamer interface {
	DisplayName() string
}

// HookAskBudgetReporter carries a measured safe Crossing Guard wait budget for a
// runtime. A non-positive value means ask/confirm is not verified and must fail closed
// immediately. Every named enforcing runtime must declare this capability; only
// historical unattributed hooks retain the established compatibility budget.
type HookAskBudgetReporter interface {
	HookAskBudget() time.Duration
}

// HookContextEncoder is the optional port through which a runtime accepts
// injected context at a hook boundary (helper-session-attachment plan D5).
// The encoder receives the runtime's own RAW event name as the hook received
// it and owns both the envelope shape and its byte cap; generic hook code
// prints exactly what it returns and nothing when it returns false.
type HookContextEncoder interface {
	EncodeHookContext(rawEvent, context string) ([]byte, bool)
}

// HookSubagentReporter is the optional port through which a runtime says that
// one hook invocation ran inside a sub-agent: a nested conversation whose hooks
// name the parent session. Context printed there reaches only the sub-agent,
// so such an invocation never carries helper messages addressed to the session
// (subagent-carrier-boundary plan). The runtime receives the payload's
// nested-agent id as sent; a runtime without the port is never treated as nested.
type HookSubagentReporter interface {
	HookRunsInSubagent(agentID string) bool
}

// HookNestedCallKinds is the optional port through which a runtime publishes
// the observation kinds — the framework's vocabulary, never a vendor event
// name — at which its hook can tell a nested call from the session's own. A
// message that must never reach a child (a handoff's brief) is carried only at
// those kinds; at any other kind the hook's "can carry" says nothing about
// nesting. A runtime without the port publishes none.
type HookNestedCallKinds interface {
	NestedCallKinds() []string
}

// NestedCallKinds lists the kinds at which runtime's hook can tell a nested
// call: empty for a runtime that reports no sub-agents.
func NestedCallKinds(runtime string) []string {
	installer := hookInstallers[runtime]
	if _, reports := installer.(HookSubagentReporter); !reports {
		return nil
	}
	if kinds, ok := installer.(HookNestedCallKinds); ok {
		return append([]string(nil), kinds.NestedCallKinds()...)
	}
	return nil
}

// HookContextCaps is each runtime's byte cap on context injected at one hook
// boundary: what its encoder cuts to. A value written to be whole at a boundary
// must fit the smallest of these, so the owner of such a value reads them here
// instead of copying a number.
func HookContextCaps() map[string]int {
	caps := map[string]int{}
	for name, installer := range hookInstallers {
		if capped, ok := installer.(hookContextCapReporter); ok {
			caps[name] = capped.hookContextCap()
		}
	}
	return caps
}

// hookContextCapReporter is implemented by an installer whose encoder has a
// byte cap.
type hookContextCapReporter interface {
	hookContextCap() int
}

// HookContextJoin separates the messages one boundary carries.
const HookContextJoin = "\n\n"

// LifecycleHookOwner reports whose installation a runtime's lifecycle hook is
// under this home: the binary its hook entry names and OwnershipOf's answer for
// it against self ("self", "foreign", "dead", or "none" when no entry exists).
// It is the rule by which a daemon may edit that runtime's settings for another
// feature: only the installation whose hook the runtime already runs.
func LifecycleHookOwner(runtime, self string) (ownership, binary string) {
	installer, ok := hookInstallers[runtime]
	if !ok {
		return "none", ""
	}
	config := installer.ResolveConfig()
	if config == "" {
		return "none", ""
	}
	binary = installer.HookBinary(config)
	return OwnershipOf(binary, self), binary
}

// LifecycleHookConfig is the settings location LifecycleHookOwner read for a runtime
// under this home: a file, or for a runtime whose settings are a directory, that
// directory. "" when the runtime is unknown or not present.
func LifecycleHookConfig(runtime string) string {
	installer, ok := hookInstallers[runtime]
	if !ok {
		return ""
	}
	return installer.ResolveConfig()
}

// HookDecisionEncoder is the optional port through which a runtime expresses a
// blocked tool call. A runtime without one keeps the historical compatibility
// envelope (see emitDeny).
type HookDecisionEncoder interface {
	EncodeHookDeny(rawEvent, reason string) []byte
}

// HookInputDecoder normalizes a runtime's native wire format at the vendor edge.
// The event is supplied by the installed command, not inferred from JSON fields.
type HookInputDecoder interface {
	DecodeHookInput(input io.Reader, event string) (hookInput, error)
}

var hookInstallers = map[string]HookInstaller{}

const installUsage = `usage:
  crossing-guard install [--settings PATH]
  crossing-guard install --codex-home PATH
  crossing-guard install --cursor-config PATH
  crossing-guard install --opencode-config PATH
  crossing-guard install --antigravity-hooks PATH

Installs or repairs one runtime's supported lifecycle hooks while preserving foreign
configuration. OpenCode is collection-only; its PreToolUse callback never enters the
decision path. Existing files receive an immediate-prior .crossing-guard.bak.`

func registerHookInstaller(installer HookInstaller) { hookInstallers[installer.Name()] = installer }

func normalizeSessionEntry(runtime, source string) string {
	installer := hookInstallers[runtime]
	if normalizer, ok := installer.(SessionEntryNormalizer); ok {
		if kind := normalizer.NormalizeSessionEntrySource(source); kind != "" {
			return kind
		}
	}
	return "unknown"
}

func installerNames() []string {
	names := make([]string, 0, len(hookInstallers))
	for name := range hookInstallers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func homeJoin(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

func cmdInstallHook(args []string) {
	if installHelpRequested(args) {
		fmt.Println(installUsage)
		return
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve own path:", err)
		os.Exit(1)
	}
	self, _ = filepath.Abs(self)
	var chosen HookInstaller
	var config string
	for _, installer := range hookInstallers {
		if value, ok := installFlagValue(args, installer.SelectFlag()); ok {
			chosen, config = installer, value
		}
	}
	if chosen == nil {
		for _, installer := range hookInstallers {
			if installer.IsDefault() {
				chosen, config = installer, installer.DefaultConfig()
			}
		}
	}
	if chosen == nil {
		fmt.Fprintln(os.Stderr, "no hook installer available")
		os.Exit(1)
	}
	if err := InstallFor(chosen.Name(), config, self); err != nil {
		fmt.Fprintln(os.Stderr, "install failed:", err)
		os.Exit(1)
	}
}

func installHelpRequested(args []string) bool {
	for _, argument := range args {
		if argument == "--help" || argument == "-h" {
			return true
		}
	}
	return false
}
