package guardcli

import (
	"sync"
)

// RuntimeAttachState is the first rung of a runtime's standing: whether OUR hook is in
// that runtime's configuration, read from the configuration file, not from a process.
type RuntimeAttachState struct {
	Name          string
	Attached      bool   // the runtime's configuration names a hook binary of ours
	HookBinary    string // the path it names; "" when not attached
	BinaryPresent bool   // that path exists
}

// resolvedConfigs memoizes each installer's RESOLVED configuration path for the
// process. One installer (OpenCode) resolves its path by running the runtime's own
// binary; a periodic job must not pay a process spawn per tick, and a runtime's
// configuration directory does not move while the daemon runs. An unresolved path
// ("": the runtime is not installed yet) is never memoized, so a runtime installed
// after the daemon boots is seen by the next report; the residual is one spawn per
// uninstalled runtime per call until it is.
var resolvedConfigs sync.Map

// memoizedConfig is a registered runtime's resolved configuration path, resolved
// once per process when it exists.
func memoizedConfig(name string) string {
	if config, ok := resolvedConfigs.Load(name); ok {
		path, _ := config.(string)
		return path
	}
	path := hookInstallers[name].ResolveConfig()
	if path != "" {
		resolvedConfigs.Store(name, path)
	}
	return path
}

// ForgetResolvedConfigs drops the memo, for a process whose home directory
// changes under it (a test that points HOME at a fresh directory).
func ForgetResolvedConfigs() {
	resolvedConfigs.Range(func(key, _ any) bool {
		resolvedConfigs.Delete(key)
		return true
	})
}

// RuntimeAttachStates reports every registered runtime's attach state. It spawns only
// when a runtime's configuration path is not yet known; it says nothing about firing,
// which only stored events can.
func RuntimeAttachStates() []RuntimeAttachState {
	out := make([]RuntimeAttachState, 0, len(hookInstallers))
	for _, name := range installerNames() {
		installer := hookInstallers[name]
		state := RuntimeAttachState{Name: name}
		if path := memoizedConfig(name); path != "" {
			state.HookBinary = installer.HookBinary(path)
			state.Attached = state.HookBinary != ""
			state.BinaryPresent = state.Attached && fileExists(state.HookBinary)
		}
		out = append(out, state)
	}
	return out
}
