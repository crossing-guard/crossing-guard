package guardcli

import (
	"sort"
	"testing"
)

func TestRuntimeAttachStatesCoverEveryInstallerWithoutAHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir()) // the one installer that would otherwise spawn
	resolvedConfigs.Range(func(k, _ any) bool { resolvedConfigs.Delete(k); return true })
	states := RuntimeAttachStates()
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, s.Name)
		if s.Attached || s.BinaryPresent || s.HookBinary != "" {
			t.Errorf("%s: an empty home has no hook attached: %+v", s.Name, s)
		}
	}
	want := installerNames()
	sort.Strings(want)
	sort.Strings(names)
	if len(names) != len(want) {
		t.Fatalf("every registered runtime reports: %v vs %v", names, want)
	}
}
