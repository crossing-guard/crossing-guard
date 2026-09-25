package guardcli

import (
	"fmt"
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

// HookDecisionEncoder is the optional port through which a runtime expresses a
// blocked tool call. A runtime without one keeps the historical compatibility
// envelope (see emitDeny).
type HookDecisionEncoder interface {
	EncodeHookDeny(rawEvent, reason string) []byte
}

var hookInstallers = map[string]HookInstaller{}

const installUsage = `usage:
  crossing-guard install [--settings PATH]
  crossing-guard install --codex-home PATH
  crossing-guard install --cursor-config PATH
  crossing-guard install --opencode-config PATH

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
