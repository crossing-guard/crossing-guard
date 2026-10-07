package guardcli

import (
	"os"
	"path/filepath"
	"strings"
)

type Runtime struct {
	Name string
	// DisplayName is the runtime's name for people: its installer's own, else its
	// connection descriptor's; empty when neither names it.
	DisplayName string
	Config      string
	Installed   bool
	// PresenceReported says Installed came from the runtime's own presence
	// check. Without it Installed only means a configuration location was found.
	PresenceReported bool
	Consented        bool
	Attached         bool
	Current          bool
	Manual           string
	HookBinary       string
	BinaryPresent    bool
	HookPhases       map[string]bool
	CollectionOnly   bool
}

// DetectRuntimes reports every registered runtime, resolving each configuration
// path afresh: the command-line callers run once per process.
func DetectRuntimes() []Runtime {
	return detectRuntimes(func(name string) string { return hookInstallers[name].ResolveConfig() })
}

// DetectRuntimesMemoized is DetectRuntimes for a long-lived process: it shares
// RuntimeAttachStates' memo of resolved configuration paths, so a read served on
// every console load does not spawn a runtime binary each time.
func DetectRuntimesMemoized() []Runtime { return detectRuntimes(memoizedConfig) }

func detectRuntimes(resolve func(name string) string) []Runtime {
	executable, err := os.Executable()
	if err == nil {
		executable, _ = filepath.Abs(executable)
	}
	runtimes := make([]Runtime, 0, len(hookInstallers))
	for _, name := range installerNames() {
		installer := hookInstallers[name]
		config := resolve(name)
		runtime := Runtime{Name: name, Config: config, Installed: config != "", Consented: IsConsented(name)}
		if presence, ok := installer.(RuntimePresenceReporter); ok {
			runtime.Installed, runtime.PresenceReported = presence.RuntimePresent(), true
		}
		if namer, ok := installer.(RuntimeDisplayNamer); ok {
			runtime.DisplayName = namer.DisplayName()
		} else if provider, ok := installer.(runtimeConnectionProvider); ok {
			runtime.DisplayName = provider.RuntimeConnectionDescriptor().DisplayName
		}
		if capability, ok := installer.(CollectionOnlyInstaller); ok {
			runtime.CollectionOnly = capability.CollectionOnly(config)
		}
		if config != "" {
			runtime.HookBinary = installer.HookBinary(config)
			runtime.Attached = runtime.HookBinary != ""
			runtime.BinaryPresent = runtime.Attached && fileExists(runtime.HookBinary)
			runtime.Current = installer.IsCurrent(config, executable)
			runtime.Manual = installer.ManualStep()
			if reporter, ok := installer.(HookPhaseReporter); ok {
				runtime.HookPhases = reporter.HookPhaseStatus(config, executable)
			}
		}
		runtimes = append(runtimes, runtime)
	}
	return runtimes
}

// flagValue is the tolerant hot-path argv reader; it must never terminate a hook.
func flagValue(args []string, name string) string {
	for index, argument := range args {
		if argument == name && index+1 < len(args) {
			return args[index+1]
		}
		if value, ok := strings.CutPrefix(argument, name+"="); ok {
			return value
		}
	}
	return ""
}

// installFlagValue shares flagValue's two accepted syntaxes while reporting presence.
func installFlagValue(args []string, name string) (string, bool) {
	for index, argument := range args {
		if argument == name && index+1 < len(args) {
			return args[index+1], true
		}
		if value, ok := strings.CutPrefix(argument, name+"="); ok {
			return value, true
		}
	}
	return "", false
}
