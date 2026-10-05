package guardcli

// Runtime connection is the provider-neutral attach/preview boundary used by the
// console. It composes the existing self-registering installers; it is deliberately
// not another runtime registry and it contains no HTTP or browser behavior.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/vendorconfig"
)

const (
	ConnectionOperationConnect    = "connect"
	ConnectionOperationDisconnect = "disconnect"
)

type RuntimeConnectionSurface struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// RuntimeConnectionDescriptor is presentation-safe provider metadata. It contains no
// argv, callbacks, credentials, raw config, HTML, or browser-supplied paths.
type RuntimeConnectionDescriptor struct {
	Runtime           string                     `json:"runtime"`
	DisplayName       string                     `json:"display_name"`
	ConfigPath        string                     `json:"config_path"`
	ConfigLabel       string                     `json:"config_label"`
	HookPhases        []string                   `json:"hook_phases"`
	Surfaces          []RuntimeConnectionSurface `json:"surfaces"`
	Limitations       []string                   `json:"limitations"`
	VerificationSteps []string                   `json:"verification_steps"`
}

type RuntimeConnectionStatus struct {
	Descriptor    RuntimeConnectionDescriptor `json:"descriptor"`
	State         string                      `json:"state"`
	Detected      bool                        `json:"detected"`
	Consented     bool                        `json:"consented"`
	ConsentOrigin string                      `json:"consent_origin,omitempty"`
	Attached      bool                        `json:"attached"`
	Current       bool                        `json:"current"`
	BinaryPresent bool                        `json:"binary_present"`
	HookBinary    string                      `json:"hook_binary,omitempty"`
	HookPhases    map[string]bool             `json:"hook_phase_status,omitempty"`
	Problem       string                      `json:"problem,omitempty"`
	Revision      string                      `json:"revision"`
}

// RuntimeConnectionPreview is a bounded semantic summary. snapshot and StateDigest
// remain server-side; callers never receive vendor config bytes.
type RuntimeConnectionPreview struct {
	Runtime                  string   `json:"runtime"`
	DisplayName              string   `json:"display_name"`
	Operation                string   `json:"operation"`
	Action                   string   `json:"action"`
	ConfigPath               string   `json:"config_path"`
	HookBinary               string   `json:"hook_binary"`
	WillChange               bool     `json:"will_change"`
	WillCreateFile           bool     `json:"will_create_file"`
	WillCreateBackup         bool     `json:"will_create_backup"`
	ForeignHandlersPreserved int      `json:"foreign_handlers_preserved"`
	OwnedHandlersRemoved     int      `json:"owned_handlers_removed"`
	OwnedHandlersAdded       int      `json:"owned_handlers_added"`
	HookPhases               []string `json:"hook_phases"`
	Limitations              []string `json:"limitations"`
	ConsentPresent           bool     `json:"consent_present"`
	ConsentOrigin            string   `json:"consent_origin,omitempty"`
	Summary                  []string `json:"summary"`
	// RecallConfig names the MCP file a disconnect also edits, when the
	// recall tools are registered there.
	RecallConfig string `json:"recall_config,omitempty"`
	StateDigest  string `json:"-"`
	snapshot     vendorconfig.Snapshot
	descriptor   RuntimeConnectionDescriptor
}

type RuntimeConnectionMutation struct {
	Runtime        string                  `json:"runtime"`
	Operation      string                  `json:"operation"`
	ConfigChanged  bool                    `json:"config_changed"`
	ConsentChanged bool                    `json:"consent_changed"`
	ResidualHook   bool                    `json:"residual_hook"`
	Detail         string                  `json:"detail"`
	Status         RuntimeConnectionStatus `json:"status"`
}

type RuntimeConnectionError struct {
	Kind string
	Err  error
}

func (e *RuntimeConnectionError) Error() string { return e.Err.Error() }
func (e *RuntimeConnectionError) Unwrap() error { return e.Err }

type runtimeConnectionProvider interface {
	RuntimeConnectionDescriptor() RuntimeConnectionDescriptor
	PreviewRuntimeConnection(configPath, executable, operation string) (RuntimeConnectionPreview, error)
}

var (
	recordRuntimeConnectionConsent = RecordConsent
	forgetRuntimeConnectionConsent = ForgetConsent
)

func RuntimeConnections(executable string) []RuntimeConnectionStatus {
	executable = absoluteExecutable(executable)
	consent := LoadConsent()
	out := []RuntimeConnectionStatus{}
	for _, name := range installerNames() {
		installer := hookInstallers[name]
		provider, ok := installer.(runtimeConnectionProvider)
		if !ok {
			continue
		}
		descriptor := cloneRuntimeConnectionDescriptor(provider.RuntimeConnectionDescriptor())
		if err := validateRuntimeConnectionDescriptor(name, descriptor); err != nil {
			if descriptor.Runtime == "" {
				descriptor.Runtime = name
			}
			if descriptor.DisplayName == "" {
				descriptor.DisplayName = name
			}
			if descriptor.ConfigPath == "" {
				descriptor.ConfigPath = "(unavailable)"
			}
			status := RuntimeConnectionStatus{Descriptor: descriptor, State: "preview_blocked",
				Problem: "provider connection descriptor is invalid", Revision: "invalid-descriptor"}
			out = append(out, status)
			continue
		}
		vendorConsent, consented := consent.Vendors[name]
		hookBinary := installer.HookBinary(descriptor.ConfigPath)
		attached := hookBinary != ""
		binaryPresent := attached && fileExists(hookBinary)
		current := installer.IsCurrent(descriptor.ConfigPath, executable)
		detected := true
		if presence, ok := installer.(RuntimePresenceReporter); ok {
			detected = presence.RuntimePresent()
		}
		status := RuntimeConnectionStatus{
			Descriptor: descriptor, Detected: detected, Consented: consented,
			Attached: attached, Current: current, BinaryPresent: binaryPresent,
			HookBinary: hookBinary,
		}
		if vendorConsent.Grandfathered {
			status.ConsentOrigin = "inferred-existing-hook"
		} else if consented {
			status.ConsentOrigin = "explicit"
		}
		if reporter, ok := installer.(HookPhaseReporter); ok {
			status.HookPhases = reporter.HookPhaseStatus(descriptor.ConfigPath, executable)
		}
		if _, err := provider.PreviewRuntimeConnection(descriptor.ConfigPath, executable, ConnectionOperationConnect); err != nil {
			status.State = "preview_blocked"
			status.Problem = boundedConnectionError(err)
		} else {
			status.State, status.Problem = connectionState(status)
		}
		status.Revision = connectionStatusRevision(status)
		out = append(out, status)
	}
	return out
}

func RuntimeConnection(runtimeName, executable string) (RuntimeConnectionStatus, error) {
	for _, status := range RuntimeConnections(executable) {
		if status.Descriptor.Runtime == runtimeName {
			return status, nil
		}
	}
	return RuntimeConnectionStatus{}, connectionError("unknown_runtime", "runtime is not available for connection")
}

func PreviewRuntimeConnection(runtimeName, operation, executable string) (RuntimeConnectionPreview, error) {
	if operation != ConnectionOperationConnect && operation != ConnectionOperationDisconnect {
		return RuntimeConnectionPreview{}, connectionError("invalid_operation", "connection operation must be connect or disconnect")
	}
	installer, ok := hookInstallers[runtimeName]
	if !ok {
		return RuntimeConnectionPreview{}, connectionError("unknown_runtime", "runtime is not registered")
	}
	provider, ok := installer.(runtimeConnectionProvider)
	if !ok {
		return RuntimeConnectionPreview{}, connectionError("unsupported_runtime", "runtime does not advertise a connection experience")
	}
	executable = absoluteExecutable(executable)
	if operation == ConnectionOperationConnect {
		if ephemeral, why := EphemeralBinary(executable); ephemeral {
			return RuntimeConnectionPreview{}, connectionError("unsafe_binary",
				"Crossing Guard is running from "+why+"; install it at a durable path before connecting a runtime")
		}
	}
	descriptor := cloneRuntimeConnectionDescriptor(provider.RuntimeConnectionDescriptor())
	if err := validateRuntimeConnectionDescriptor(runtimeName, descriptor); err != nil {
		return RuntimeConnectionPreview{}, connectionError("invalid_descriptor", "provider connection descriptor is invalid")
	}
	preview, err := provider.PreviewRuntimeConnection(descriptor.ConfigPath, executable, operation)
	if err != nil {
		return RuntimeConnectionPreview{}, connectionError("preview_blocked", boundedConnectionError(err))
	}
	preview.descriptor = descriptor
	preview.Runtime = runtimeName
	preview.DisplayName = descriptor.DisplayName
	preview.ConfigPath = descriptor.ConfigPath
	preview.HookBinary = executable
	preview.Limitations = append([]string(nil), descriptor.Limitations...)
	consent, present := LoadConsent().Vendors[runtimeName]
	preview.ConsentPresent = present
	if consent.Grandfathered {
		preview.ConsentOrigin = "inferred-existing-hook"
	} else if present {
		preview.ConsentOrigin = "explicit"
	}
	if operation == ConnectionOperationDisconnect {
		// Named, never hashed: the runtime rewrites its MCP file constantly, so
		// its bytes in the digest would fail every confirmation. The preview
		// promises only what the disconnect will do.
		if file := RecallConfigFor(runtimeName, descriptor.ConfigPath); file != "" {
			state, err := RecallStatusFor(runtimeName, descriptor.ConfigPath, executable)
			switch {
			case err != nil:
				preview.Limitations = append(preview.Limitations,
					"the recall tools entry in "+file+" could not be read ("+err.Error()+"); disconnecting will not remove it")
			case state == RecallCurrent || state == RecallStale:
				preview.RecallConfig = file
				preview.Summary = append(preview.Summary, "removes the Crossing Guard recall tools from "+file)
			case state == RecallForeign:
				preview.Summary = append(preview.Summary, "leaves the recall tools entry in "+file+" in place: another program owns it")
			}
		}
	}
	preview.StateDigest = runtimeConnectionDigest(preview, consent)
	return preview, nil
}

func connectionState(status RuntimeConnectionStatus) (string, string) {
	name := status.Descriptor.DisplayName
	switch {
	case (status.Consented || status.Attached) && !status.Detected:
		return "needs_attention", name + " is no longer detected; its surviving hook configuration remains available for review or disconnect"
	case !status.Detected:
		return "not_detected", name + " was not detected; nothing was changed"
	case status.Consented && !status.Attached:
		return "needs_attention", name + " consent exists but the owned hook is missing"
	case status.Attached && !status.Consented:
		return "needs_attention", "an owned " + name + " hook exists without connection consent; review it to adopt or disconnect"
	case status.Attached && !status.BinaryPresent:
		return "needs_attention", "the configured Crossing Guard hook binary no longer exists"
	case (status.Consented || status.Attached) && !status.Current:
		return "needs_attention", name + " is connected but its owned hook needs repair"
	case status.Current && status.Attached:
		return "connected_unverified", name + " is connected; configuration is not proof that its hook fires"
	default:
		return "detected", name + " is available and not connected"
	}
}

func connectionStatusRevision(status RuntimeConnectionStatus) string {
	clone := status
	clone.Revision = ""
	raw, _ := json.Marshal(clone)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func cloneRuntimeConnectionDescriptor(in RuntimeConnectionDescriptor) RuntimeConnectionDescriptor {
	in.HookPhases = append([]string(nil), in.HookPhases...)
	in.Surfaces = append([]RuntimeConnectionSurface(nil), in.Surfaces...)
	in.Limitations = append([]string(nil), in.Limitations...)
	in.VerificationSteps = append([]string(nil), in.VerificationSteps...)
	return in
}

func validateRuntimeConnectionDescriptor(runtimeName string, descriptor RuntimeConnectionDescriptor) error {
	if descriptor.Runtime != runtimeName || descriptor.DisplayName == "" ||
		descriptor.ConfigLabel == "" || !filepath.IsAbs(descriptor.ConfigPath) {
		return errors.New("invalid runtime connection descriptor")
	}
	seenSurfaces := map[string]bool{}
	for _, surface := range descriptor.Surfaces {
		if surface.ID == "" || surface.Label == "" || seenSurfaces[surface.ID] {
			return errors.New("invalid runtime connection surface")
		}
		seenSurfaces[surface.ID] = true
	}
	return nil
}

func connectionError(kind, message string) error {
	return &RuntimeConnectionError{Kind: kind, Err: errors.New(message)}
}

func boundedConnectionError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 300 {
		message = message[:300] + "…"
	}
	return message
}

func absoluteExecutable(executable string) string {
	if executable == "" {
		executable, _ = os.Executable()
	}
	absolute, err := filepath.Abs(executable)
	if err == nil {
		return absolute
	}
	return executable
}

// EphemeralBinary is the one owner for paths that would make a hook disappear after a
// restart. CLI init and the connection preview both use it.
func EphemeralBinary(executable string) (bool, string) {
	temporary := os.TempDir()
	for _, prefix := range []string{temporary, "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"} {
		if prefix != "" && strings.HasPrefix(executable, strings.TrimSuffix(prefix, "/")+"/") {
			return true, "a temporary directory"
		}
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(executable, home+"/Downloads/") {
		return true, "your Downloads folder"
	}
	return false, ""
}
