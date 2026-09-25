package guardcli

import (
	"os"
	"path/filepath"
	"strings"
)

type Runtime struct {
	Name           string
	Config         string
	Installed      bool
	Consented      bool
	Attached       bool
	Current        bool
	Manual         string
	HookBinary     string
	BinaryPresent  bool
	HookPhases     map[string]bool
	CollectionOnly bool
}

func DetectRuntimes() []Runtime {
	executable, err := os.Executable()
	if err == nil {
		executable, _ = filepath.Abs(executable)
	}
	runtimes := make([]Runtime, 0, len(hookInstallers))
	for _, name := range installerNames() {
		installer := hookInstallers[name]
		config := installer.ResolveConfig()
		runtime := Runtime{Name: name, Config: config, Installed: config != "", Consented: IsConsented(name)}
		if presence, ok := installer.(RuntimePresenceReporter); ok {
			runtime.Installed = presence.RuntimePresent()
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
