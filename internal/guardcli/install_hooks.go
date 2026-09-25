package guardcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type HookStatus struct {
	Vendor string `json:"vendor"`
	Path   string `json:"path"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
	Manual string `json:"manual,omitempty"`
}

// foreignHooks names the vendors whose hooks a different, still-present binary owns.
// It only reads: deciding this before the first write is what keeps a skipped machine
// completely untouched.
//
// A recorded owner that no longer exists is NOT foreign. That is how a machine left
// pointing at a deleted scratch build repairs itself the next time the real daemon
// starts, with no human step.
func foreignHooks(self string) map[string]bool {
	foreign := map[string]bool{}
	for _, name := range installerNames() {
		installer := hookInstallers[name]
		config := installer.ResolveConfig()
		if config == "" {
			continue
		}
		if OwnershipOf(installer.HookBinary(config), self) == "foreign" {
			foreign[name] = true
		}
	}
	return foreign
}

// EnsureHooks repairs only vendors with durable consent; discovery is never consent.
// This distinction prevents daemon startup from silently taking over a newly found
// runtime while still keeping an already-approved hook current.
func EnsureHooks() []HookStatus {
	executable, err := os.Executable()
	if err != nil {
		return []HookStatus{{Vendor: "?", Action: "error", Detail: "cannot resolve own path: " + err.Error()}}
	}
	executable, _ = filepath.Abs(executable)

	// Ownership is decided BEFORE anything is written, grandfathered consent
	// included: a hook another live binary owns is not ours to adopt, and
	// recording consent for it would leave a trace of a takeover we then refused
	// to perform.
	foreign := foreignHooks(executable)

	adopted := map[string]bool{}
	if len(foreign) < len(installerNames()) {
		for _, vendor := range GrandfatherExistingAttachments() {
			if !foreign[vendor] {
				adopted[vendor] = true
			}
		}
	}
	var statuses []HookStatus
	for _, name := range installerNames() {
		installer := hookInstallers[name]
		config := installer.ResolveConfig()
		if config == "" {
			statuses = append(statuses, HookStatus{Vendor: name, Action: "absent", Detail: "runtime not installed on this machine"})
			continue
		}
		if foreign[name] {
			statuses = append(statuses, HookStatus{Vendor: name, Path: config, Action: "foreign",
				Detail: "this hook belongs to " + installer.HookBinary(config) + " and was left alone",
				Manual: "to take it over, run: crossing-guard init --yes"})
			continue
		}
		vendorConsent, consented := LoadConsent().Vendors[name]
		if !consented && !adopted[name] {
			statuses = append(statuses, HookStatus{Vendor: name, Path: config, Action: "unconsented",
				Detail: "installed on this machine and NOT governed — nothing was edited",
				Manual: "attach it with: crossing-guard init"})
			continue
		}
		if vendorConsent.Config != "" {
			config = vendorConsent.Config
		}
		status := HookStatus{Vendor: name, Path: config, Manual: installer.ManualStep()}
		if adopted[name] {
			status.Detail = "consent adopted from the existing attachment; "
		}
		if err := withRuntimeConfigLock(name, config, func() error {
			latest, stillConsented := LoadConsent().Vendors[name]
			if !stillConsented {
				status.Action = "unconsented"
				status.Detail = "consent was removed while hooks were being checked — nothing was edited"
				status.Manual = "attach it with: crossing-guard init"
				return nil
			}
			if latest.Config != "" && latest.Config != config {
				status.Action = "error"
				status.Detail = "consented config path changed while hooks were being checked; retry"
				return nil
			}
			if installer.IsCurrent(config, executable) {
				status.Action, status.Manual = "current", ""
				return nil
			}
			if installErr := installer.Install(config, executable); installErr != nil {
				status.Action, status.Detail = "error", status.Detail+installErr.Error()
			} else {
				status.Action, status.Detail = "installed", status.Detail+"hook installed/repaired to match this binary"
			}
			return nil
		}); err != nil {
			status.Action, status.Detail = "error", err.Error()
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func UninstallHooks() []HookStatus {
	executable, err := os.Executable()
	if err != nil {
		return []HookStatus{{Vendor: "?", Action: "error", Detail: "cannot resolve own path: " + err.Error()}}
	}
	executable, _ = filepath.Abs(executable)
	var statuses []HookStatus
	for _, name := range installerNames() {
		installer := hookInstallers[name]
		config := installer.ResolveConfig()
		if config == "" {
			statuses = append(statuses, HookStatus{Vendor: name, Action: "absent", Detail: "runtime not installed on this machine"})
			continue
		}
		status := HookStatus{Vendor: name, Path: config}
		var removed bool
		err := withRuntimeConfigLock(name, config, func() error {
			var uninstallErr error
			removed, uninstallErr = installer.Uninstall(config, executable)
			return uninstallErr
		})
		switch {
		case err != nil:
			status.Action, status.Detail = "error", err.Error()
		case removed:
			status.Action, status.Detail = "removed", "our lifecycle hooks removed; other config left intact"
			if err := ForgetConsent(name); err != nil {
				status.Action = "error"
				status.Detail = "hook removed, but the consent record could not be updated (" + err.Error() +
					") — the daemon will RE-ATTACH on next start. Delete " + consentPath() + " by hand."
			}
		default:
			status.Action, status.Detail = "absent", "our hook was not present — nothing to remove"
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func InstallFor(vendor, config, self string) error {
	installer, ok := hookInstallers[vendor]
	if !ok {
		return fmt.Errorf("unknown runtime %q", vendor)
	}
	return withRuntimeConfigLock(vendor, config, func() error {
		if err := installer.Install(config, self); err != nil {
			return err
		}
		return RecordConsent(vendor, config, self, false)
	})
}

// OwnershipOf answers "whose installation is this" for one recorded binary path.
//
// It lives here because the daemon may import guardcli and not the reverse, so this
// is the one package both the launchd service and the vendor hooks can share. One
// rule, one place: two copies of "is this mine" would drift, and the direction they
// drift in decides whether a machine repairs itself or gets taken over.
//
// Paths are resolved through EvalSymlinks first: /tmp and /private/tmp are the same
// file on macOS, and a false "foreign" would silently stop the installed daemon from
// repairing its own hooks — this guard's own failure, inverted.
func OwnershipOf(recordedOwner, self string) string {
	if strings.TrimSpace(recordedOwner) == "" {
		return "none"
	}
	resolved, err := filepath.EvalSymlinks(recordedOwner)
	if err != nil {
		if _, statErr := os.Stat(recordedOwner); statErr != nil {
			return "dead"
		}
		resolved, _ = filepath.Abs(recordedOwner)
	}
	mine, err := filepath.EvalSymlinks(self)
	if err != nil {
		mine, _ = filepath.Abs(self)
	}
	if resolved == mine {
		return "self"
	}
	return "foreign"
}
