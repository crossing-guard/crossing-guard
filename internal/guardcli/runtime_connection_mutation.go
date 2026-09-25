package guardcli

// Runtime connection mutations are serialized with every other hook mutation for the
// same provider/config. Preview revalidation happens inside that lock so browser tokens
// cannot overwrite edits made after the user reviewed the preview.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"crossing-guard/internal/filelock"
)

func ConnectRuntime(runtimeName, executable, expectedStateDigest string) (RuntimeConnectionMutation, error) {
	executable = absoluteExecutable(executable)
	preview, err := PreviewRuntimeConnection(runtimeName, ConnectionOperationConnect, executable)
	if err != nil {
		return RuntimeConnectionMutation{}, err
	}
	if preview.StateDigest != expectedStateDigest {
		return RuntimeConnectionMutation{}, connectionError("preview_changed", "connection preview changed; review it again before connecting")
	}
	installer := hookInstallers[runtimeName]
	result := RuntimeConnectionMutation{Runtime: runtimeName, Operation: ConnectionOperationConnect}
	err = withRuntimeConfigLock(runtimeName, preview.ConfigPath, func() error {
		current, currentErr := PreviewRuntimeConnection(runtimeName, ConnectionOperationConnect, executable)
		if currentErr != nil {
			return currentErr
		}
		if current.StateDigest != expectedStateDigest {
			return connectionError("preview_changed", "connection preview changed; review it again before connecting")
		}
		if current.WillChange {
			if installErr := installer.Install(current.ConfigPath, executable); installErr != nil {
				return connectionError("install_failed", boundedConnectionError(installErr))
			}
			result.ConfigChanged = true
		}
		wasConsented := current.ConsentPresent
		if consentErr := recordRuntimeConnectionConsent(runtimeName, current.ConfigPath, executable, false); consentErr != nil {
			result.ResidualHook = installer.HookBinary(current.ConfigPath) != ""
			if result.ConfigChanged {
				removed, removeErr := installer.Uninstall(current.ConfigPath, executable)
				if removeErr == nil && removed {
					result.ResidualHook = false
				}
				if removeErr != nil {
					return connectionError("partial_connect",
						"hook was installed but consent could not be recorded, and compensating removal failed; the residual hook will not be auto-repaired")
				}
			}
			return connectionError("consent_failed", "connection was not recorded; any newly installed hook was removed")
		}
		result.ConsentChanged = !wasConsented
		return nil
	})
	if err != nil {
		result.Status, _ = RuntimeConnection(runtimeName, executable)
		return result, err
	}
	result.Detail = preview.DisplayName + " hooks connected; the provider was not launched. Verification remains user-driven."
	result.Status, _ = RuntimeConnection(runtimeName, executable)
	return result, nil
}

func DisconnectRuntime(runtimeName, executable, expectedStateDigest string) (RuntimeConnectionMutation, error) {
	executable = absoluteExecutable(executable)
	preview, err := PreviewRuntimeConnection(runtimeName, ConnectionOperationDisconnect, executable)
	if err != nil {
		return RuntimeConnectionMutation{}, err
	}
	if preview.StateDigest != expectedStateDigest {
		return RuntimeConnectionMutation{}, connectionError("preview_changed", "disconnect preview changed; review it again before disconnecting")
	}
	installer := hookInstallers[runtimeName]
	result := RuntimeConnectionMutation{Runtime: runtimeName, Operation: ConnectionOperationDisconnect}
	err = withRuntimeConfigLock(runtimeName, preview.ConfigPath, func() error {
		current, currentErr := PreviewRuntimeConnection(runtimeName, ConnectionOperationDisconnect, executable)
		if currentErr != nil {
			return currentErr
		}
		if current.StateDigest != expectedStateDigest {
			return connectionError("preview_changed", "disconnect preview changed; review it again before disconnecting")
		}
		if current.ConsentPresent {
			if forgetErr := forgetRuntimeConnectionConsent(runtimeName); forgetErr != nil {
				return connectionError("consent_failed", "consent could not be removed; provider configuration was left unchanged")
			}
			result.ConsentChanged = true
		}
		removed, removeErr := installer.Uninstall(current.ConfigPath, executable)
		if removeErr != nil {
			result.ResidualHook = installer.HookBinary(current.ConfigPath) != ""
			return connectionError("partial_disconnect",
				"provider consent was removed, but the owned hook could not be removed; automatic repair is disabled")
		}
		result.ConfigChanged = removed
		return nil
	})
	if err != nil {
		result.Status, _ = RuntimeConnection(runtimeName, executable)
		return result, err
	}
	result.Detail = "Crossing Guard's " + preview.DisplayName + " hooks and consent were removed; the provider and foreign hooks were left untouched."
	result.Status, _ = RuntimeConnection(runtimeName, executable)
	return result, nil
}

func runtimeConnectionDigest(preview RuntimeConnectionPreview, consent VendorConsent) string {
	hash := sha256.New()
	parts := []string{
		preview.Operation, preview.Runtime, filepath.Clean(preview.ConfigPath), preview.HookBinary,
		fmt.Sprintf("exists=%t", preview.snapshot.Exists), fmt.Sprintf("mode=%o", preview.snapshot.Mode),
		fmt.Sprintf("consent=%t", preview.ConsentPresent), consent.Config, consent.Binary,
		consent.ConsentedAt, fmt.Sprintf("grandfathered=%t", consent.Grandfathered),
	}
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(preview.snapshot.Data)
	return hex.EncodeToString(hash.Sum(nil))
}

func withRuntimeConfigLock(runtimeName, configPath string, change func() error) error {
	sum := sha256.Sum256([]byte(runtimeName + "\x00" + filepath.Clean(configPath)))
	lockPath := filepath.Join(dataDir(), "locks", "runtime-"+hex.EncodeToString(sum[:])+".lock")
	return filelock.With(lockPath, 0o600, change)
}
