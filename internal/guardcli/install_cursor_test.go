package guardcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cursorSeed(stale string) []byte {
	config := map[string]any{
		"version": float64(1),
		"foreign": "keep",
		"hooks": map[string]any{
			"preToolUse": []any{
				map[string]any{"command": stale},
				map[string]any{"command": "/foreign/pre", "matcher": "Shell"},
			},
			"postToolUse":   []any{map[string]any{"command": "/foreign/post"}},
			"workspaceOpen": []any{map[string]any{"command": "/foreign/workspace"}},
		},
	}
	raw, _ := json.MarshalIndent(config, "", "  ")
	return append(raw, '\n')
}

func readCursorConfigForTest(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestCursorInstallPreservesForeignHooksRepairsDuplicatesAndUninstalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	executable := "/opt/current/crossing-guard"
	seed := cursorSeed(`"/old/crossing-guard" hook --runtime cursor`)
	if err := os.WriteFile(path, seed, 0o640); err != nil {
		t.Fatal(err)
	}
	installer := cursorInstaller{}
	if err := installer.Install(path, executable); err != nil {
		t.Fatal(err)
	}
	if !installer.IsCurrent(path, executable) {
		t.Fatal("installed Cursor hooks are not current")
	}
	assertExpectedHookPhases(t, installer.HookPhaseStatus(path, executable),
		"SessionStart", "PreToolUse", "PostToolUse", "PostToolUseFailure", "SessionEnd")
	config := readCursorConfigForTest(t, path)
	if config["foreign"] != "keep" || config["version"] != float64(1) {
		t.Fatalf("top-level Cursor config changed unexpectedly: %+v", config)
	}
	serialized, _ := json.Marshal(config)
	for _, foreign := range []string{"/foreign/pre", "/foreign/post", "/foreign/workspace"} {
		if !strings.Contains(string(serialized), foreign) {
			t.Fatalf("foreign hook %q was lost: %s", foreign, serialized)
		}
	}
	if strings.Contains(string(serialized), "/old/crossing-guard") {
		t.Fatalf("stale owned hook survived: %s", serialized)
	}
	if got, _ := os.ReadFile(path + ".crossing-guard.bak"); string(got) != string(seed) {
		t.Fatalf("backup mismatch:\n%s", got)
	}

	backup := path + ".crossing-guard.bak"
	if err := os.WriteFile(backup, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installer.Install(path, executable); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(backup); string(got) != "sentinel" {
		t.Fatalf("idempotent install rewrote backup: %q", got)
	}

	removed, err := installer.Uninstall(path, executable)
	if err != nil || !removed {
		t.Fatalf("uninstall removed=%t err=%v", removed, err)
	}
	config = readCursorConfigForTest(t, path)
	serialized, _ = json.Marshal(config)
	if strings.Contains(string(serialized), "crossing-guard") {
		t.Fatalf("owned Cursor hook survived uninstall: %s", serialized)
	}
	for _, foreign := range []string{"/foreign/pre", "/foreign/post", "/foreign/workspace"} {
		if !strings.Contains(string(serialized), foreign) {
			t.Fatalf("uninstall lost foreign hook %q: %s", foreign, serialized)
		}
	}
	removed, err = installer.Uninstall(path, executable)
	if err != nil || removed {
		t.Fatalf("second uninstall removed=%t err=%v", removed, err)
	}
}

func TestCursorInstallRejectsMalformedAndUnknownShapesWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "malformed", raw: `{broken`},
		{name: "unknown version", raw: `{"version":2,"foreign":"keep"}`},
		{name: "string version", raw: `{"version":"1","foreign":"keep"}`},
		{name: "hooks scalar", raw: `{"version":1,"hooks":"foreign"}`},
		{name: "event scalar", raw: `{"version":1,"hooks":{"preToolUse":"foreign"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			if err := os.WriteFile(path, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := (cursorInstaller{}).Install(path, "/opt/crossing-guard"); err == nil {
				t.Fatal("invalid Cursor config was accepted")
			}
			if got, _ := os.ReadFile(path); string(got) != test.raw {
				t.Fatalf("invalid config changed: %q", got)
			}
			if _, err := os.Stat(path + ".crossing-guard.bak"); !os.IsNotExist(err) {
				t.Fatalf("invalid config created backup: %v", err)
			}
		})
	}
}

func TestCursorDuplicateOwnedHooksAreNotCurrentAndAreRepaired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	executable := "/opt/current/crossing-guard"
	want := hookCommand(executable, cursorVendor)
	config := map[string]any{"version": float64(1), "hooks": map[string]any{}}
	for _, event := range cursorLifecycleHookEvents {
		config["hooks"].(map[string]any)[event] = []any{
			map[string]any{"command": want}, map[string]any{"command": want},
		}
	}
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	installer := cursorInstaller{}
	if installer.IsCurrent(path, executable) {
		t.Fatal("duplicate Cursor hooks reported current")
	}
	if err := installer.Install(path, executable); err != nil {
		t.Fatal(err)
	}
	if !installer.IsCurrent(path, executable) {
		t.Fatal("duplicate Cursor hooks were not repaired")
	}
}

func TestCursorOwnedHookOnUnsupportedEventIsRepairedAndRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	executable := "/opt/current/crossing-guard"
	want := hookCommand(executable, cursorVendor)
	config := map[string]any{"version": float64(1), "hooks": map[string]any{}}
	for _, event := range cursorLifecycleHookEvents {
		config["hooks"].(map[string]any)[event] = []any{map[string]any{"command": want}}
	}
	config["hooks"].(map[string]any)["workspaceOpen"] = []any{
		map[string]any{"command": want},
		map[string]any{"command": "/foreign/workspace"},
	}
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	installer := cursorInstaller{}
	if installer.IsCurrent(path, executable) {
		t.Fatal("extra owned Cursor hook on an unsupported event reported current")
	}
	if err := installer.Install(path, executable); err != nil {
		t.Fatal(err)
	}
	if !installer.IsCurrent(path, executable) {
		t.Fatal("extra owned Cursor hook was not repaired")
	}
	got, _ := json.Marshal(readCursorConfigForTest(t, path))
	if strings.Count(string(got), "crossing-guard") != len(cursorLifecycleHookEvents) {
		t.Fatalf("unexpected owned hook count after repair: %s", got)
	}
	if !strings.Contains(string(got), "/foreign/workspace") {
		t.Fatalf("repair removed foreign unsupported-event handler: %s", got)
	}
}

func TestCursorCurrentHooksWithoutSchemaVersionAreRepaired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	executable := "/opt/current/crossing-guard"
	want := hookCommand(executable, cursorVendor)
	config := map[string]any{"foreign": "keep", "hooks": map[string]any{}}
	for _, event := range cursorLifecycleHookEvents {
		config["hooks"].(map[string]any)[event] = []any{map[string]any{"command": want}}
	}
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	installer := cursorInstaller{}
	if installer.IsCurrent(path, executable) {
		t.Fatal("Cursor hooks without required schema version reported current")
	}
	if err := installer.Install(path, executable); err != nil {
		t.Fatal(err)
	}
	got := readCursorConfigForTest(t, path)
	if !cursorVersionIsCurrent(got) || got["foreign"] != "keep" {
		t.Fatalf("Cursor version repair = %+v", got)
	}
}

func TestCursorDetectionSeparatesClientPresenceFromStaleConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldLookPath, oldStat := cursorClientLookPath, cursorClientStat
	t.Cleanup(func() { cursorClientLookPath, cursorClientStat = oldLookPath, oldStat })
	cursorClientLookPath = func(string) (string, error) { return "", errors.New("absent") }
	cursorClientStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	installer := cursorInstaller{}
	if got := installer.ResolveConfig(); got != "" {
		t.Fatalf("absent Cursor fabricated config %q", got)
	}
	path := filepath.Join(home, filepath.FromSlash(".cursor/hooks.json"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := installer.ResolveConfig(); got != path {
		t.Fatalf("stale config must remain discoverable for uninstall: %q", got)
	}
	if installer.RuntimePresent() {
		t.Fatal("stale config reported Cursor client installed")
	}

	cursorClientLookPath = func(binary string) (string, error) {
		if binary == "cursor-agent" {
			return "/opt/cursor-agent", nil
		}
		return "", errors.New("absent")
	}
	if !installer.RuntimePresent() || installer.ResolveConfig() != path {
		t.Fatal("measured Cursor executable was not detected")
	}
	var found *Runtime
	for _, detected := range DetectRuntimes() {
		if detected.Name == cursorVendor {
			copy := detected
			found = &copy
		}
	}
	if found == nil || !found.Installed || found.Config != path {
		t.Fatalf("Cursor runtime detection = %+v", found)
	}
}
