package guardcli

// Registering the recall MCP server with a runtime (recall-mcp-v1-plan §3.7).
// A separate, recorded yes from the hook yes: init asks it, daemon boot only
// repairs where it was given, and uninstall or disconnect removes every trace
// whatever the record says.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"crossing-guard/internal/recallmcp"
	"crossing-guard/internal/vendorconfig"
)

// Recall registration states, as RecallStatus reports them.
const (
	RecallAbsent  = "absent"  // no entry under our server name
	RecallCurrent = "current" // our entry, this binary, the expected arguments
	RecallStale   = "stale"   // our entry, but another (gone) binary or other arguments
	RecallForeign = "foreign" // an entry under our name that another live program owns
)

// RecallRegistrar is the optional port a runtime's installer implements to
// register the recall server in that runtime's own MCP configuration. Every
// method takes the runtime's hook config (the consented file) and derives the
// MCP file from it, so one consent names both.
type RecallRegistrar interface {
	RecallConfig(hookConfig string) string
	RegisterRecall(hookConfig, self string) error
	// RecallStatus is one of the Recall* states; a file that cannot be read
	// or parsed is an error, never "absent".
	RecallStatus(hookConfig, self string) (string, error)
	UnregisterRecall(hookConfig, self string) (removed bool, err error)
}

func recallRegistrar(vendor string) (RecallRegistrar, bool) {
	registrar, ok := hookInstallers[vendor].(RecallRegistrar)
	return registrar, ok
}

// RecallRuntimes names the runtimes that can register the recall server.
func RecallRuntimes() []string {
	var names []string
	for _, name := range installerNames() {
		if _, ok := recallRegistrar(name); ok {
			names = append(names, name)
		}
	}
	return names
}

// RecallConfigFor is the MCP file a vendor's recall registration edits.
func RecallConfigFor(vendor, hookConfig string) string {
	if registrar, ok := recallRegistrar(vendor); ok {
		return registrar.RecallConfig(hookConfig)
	}
	return ""
}

// RecallStatusFor reports one vendor's registration state.
func RecallStatusFor(vendor, hookConfig, self string) (string, error) {
	registrar, ok := recallRegistrar(vendor)
	if !ok {
		return "", fmt.Errorf("%s cannot register the recall tools", vendor)
	}
	return registrar.RecallStatus(hookConfig, self)
}

// RegisterRecallFor registers the recall server for a vendor whose hooks are
// consented and records the recall yes, under the vendor's config lock. An
// entry another live program owns is never overwritten.
func RegisterRecallFor(vendor, hookConfig, self string) error {
	registrar, ok := recallRegistrar(vendor)
	if !ok {
		return fmt.Errorf("%s cannot register the recall tools", vendor)
	}
	return withRuntimeConfigLock(vendor, hookConfig, func() error {
		if _, consented := LoadConsent().Vendors[vendor]; !consented {
			return fmt.Errorf("%s has no hook consent; attach it first", vendor)
		}
		state, err := registrar.RecallStatus(hookConfig, self)
		if err != nil {
			return err
		}
		if state == RecallForeign {
			return fmt.Errorf("%s already names a %q server that another program owns; left alone",
				registrar.RecallConfig(hookConfig), recallmcp.ServerName)
		}
		if state != RecallCurrent {
			if err := registrar.RegisterRecall(hookConfig, self); err != nil {
				return err
			}
		}
		return RecordRecallConsent(vendor, registrar.RecallConfig(hookConfig))
	})
}

// EnsureRecall repairs the recall registration of every vendor whose recall
// yes is recorded, and touches nothing else: boot never widens consent.
func EnsureRecall() []HookStatus {
	self, err := os.Executable()
	if err != nil {
		return []HookStatus{{Vendor: "?", Action: "error", Detail: "cannot resolve own path: " + err.Error()}}
	}
	self, _ = filepath.Abs(self)
	var statuses []HookStatus
	for _, name := range RecallRuntimes() {
		consent, consented := LoadConsent().Vendors[name]
		if !consented || consent.Recall == nil || consent.Config == "" {
			continue
		}
		registrar, _ := recallRegistrar(name)
		status := HookStatus{Vendor: name, Path: registrar.RecallConfig(consent.Config)}
		lockErr := withRuntimeConfigLock(name, consent.Config, func() error {
			latest, still := LoadConsent().Vendors[name]
			if !still || latest.Recall == nil {
				status.Action, status.Detail = "unconsented", "recall consent was removed while checking — nothing was edited"
				return nil
			}
			state, err := registrar.RecallStatus(consent.Config, self)
			switch {
			case err != nil:
				status.Action, status.Detail = "error", err.Error()
			case state == RecallCurrent:
				status.Action = "current"
			case state == RecallForeign:
				status.Action, status.Detail = "foreign", "another program owns the "+recallmcp.ServerName+" entry; left alone"
			default:
				if err := registrar.RegisterRecall(consent.Config, self); err != nil {
					status.Action, status.Detail = "error", err.Error()
				} else {
					status.Action, status.Detail = "installed", "recall tools registered/repaired to match this binary"
				}
			}
			return nil
		})
		if lockErr != nil {
			status.Action, status.Detail = "error", lockErr.Error()
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// unregisterRecallLocked removes a vendor's recall registration whatever the
// consent record says. The caller holds the vendor's config lock.
func unregisterRecallLocked(vendor, hookConfig, self string) (bool, string, error) {
	registrar, ok := recallRegistrar(vendor)
	if !ok || hookConfig == "" {
		return false, "", nil
	}
	removed, err := registrar.UnregisterRecall(hookConfig, self)
	return removed, registrar.RecallConfig(hookConfig), err
}

// RecallHookConfig is the hook config a vendor's recall registration derives
// from: the consented one when recorded, else the machine default.
func RecallHookConfig(vendor string) string { return recallHookConfig(vendor) }

func recallHookConfig(vendor string) string {
	if consent, ok := LoadConsent().Vendors[vendor]; ok && consent.Config != "" {
		return consent.Config
	}
	if installer, ok := hookInstallers[vendor]; ok {
		return installer.ResolveConfig()
	}
	return ""
}

// uninstallRecall removes every vendor's recall registration, consented or not.
func uninstallRecall(self string) []HookStatus {
	var statuses []HookStatus
	for _, name := range RecallRuntimes() {
		hookConfig := recallHookConfig(name)
		if hookConfig == "" {
			continue
		}
		var removed bool
		var file string
		err := withRuntimeConfigLock(name, hookConfig, func() error {
			var unregisterErr error
			removed, file, unregisterErr = unregisterRecallLocked(name, hookConfig, self)
			return unregisterErr
		})
		switch {
		case err != nil:
			statuses = append(statuses, HookStatus{Vendor: name, Path: file, Action: "error",
				Detail: "recall tools could not be unregistered: " + err.Error()})
			continue
		case removed:
			statuses = append(statuses, HookStatus{Vendor: name, Path: file, Action: "removed",
				Detail: "recall tools unregistered; other config left intact"})
		}
		// Whether or not a hook is removed next, the recall yes goes: a later
		// boot must never re-register what the user just removed.
		if err := ForgetRecallConsent(name); err != nil {
			statuses = append(statuses, HookStatus{Vendor: name, Path: file, Action: "error",
				Detail: "recall tools removed, but the consent record could not be updated (" + err.Error() +
					") — the daemon will RE-REGISTER them on next start. Delete " + consentPath() + " by hand."})
		}
	}
	return statuses
}

// recallArgs is the one registered argument list: `mcp --runtime <vendor>`.
func recallArgs(vendor string) []string {
	return []string{recallmcp.Command, "--runtime", vendor}
}

// recallEntryState classifies the command a runtime has registered under our
// server name. Ownership is anchored on the recall verb and the binary's
// identity, then decided by OwnershipOf exactly as for hooks.
func recallEntryState(command string, args []string, vendor, self string) string {
	if len(args) == 0 || args[0] != recallmcp.Command || !isOurBinary(command) {
		return RecallForeign
	}
	switch OwnershipOf(command, self) {
	case "self":
		if slices.Equal(args, recallArgs(vendor)) {
			return RecallCurrent
		}
		return RecallStale
	case "dead", "none":
		return RecallStale
	}
	return RecallForeign
}

// isOurBinary accepts the binary spellings ourHookBinaryFromCommand accepts.
func isOurBinary(path string) bool {
	return ourHookBinaryFromCommand(fmt.Sprintf("%q hook", path)) != ""
}

// readJSONObject decodes one JSON object file keeping every value's bytes.
// A missing file is an empty object; a corrupt one is an error.
func readJSONObject(path string) (vendorconfig.Snapshot, map[string]json.RawMessage, error) {
	snapshot, err := vendorconfig.Read(path)
	if err != nil {
		return snapshot, nil, err
	}
	top := map[string]json.RawMessage{}
	if snapshot.Exists && len(bytes.TrimSpace(snapshot.Data)) > 0 {
		if err := json.Unmarshal(snapshot.Data, &top); err != nil {
			return snapshot, nil, fmt.Errorf("%s is not valid JSON (%w) — fix it first; refusing to overwrite it", path, err)
		}
	}
	return snapshot, top, nil
}

// writeJSONObject writes the object back: every value survives, but the file
// is re-indented and its top-level keys come back sorted; <, > and & stay
// literal.
func writeJSONObject(path string, snapshot vendorconfig.Snapshot, top map[string]json.RawMessage) error {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(top); err != nil {
		return err
	}
	_, err := vendorconfig.Replace(path, snapshot, out.Bytes())
	return err
}
