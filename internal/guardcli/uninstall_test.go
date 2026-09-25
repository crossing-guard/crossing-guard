package guardcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUninstallRemovesOnlyOurHook pins bar 2 / P-INST-3: uninstall must remove OUR
// PreToolUse hook and leave everything else — other hooks, other top-level keys —
// intact. A guard that can only be installed is a one-way door (D11).
func TestUninstallRemovesOnlyOurHook(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	exe := "/opt/crossing-guard/crossing-guard"

	// A settings file with OUR hook plus a foreign hook and a foreign top-level key.
	seed := map[string]any{
		"model": "sonnet", // foreign top-level key must survive
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{"matcher": "*", "hooks": []any{
					map[string]any{"type": "command", "command": hookCommand(exe, claudeVendor)},
				}},
				map[string]any{"matcher": "Bash", "hooks": []any{
					map[string]any{"type": "command", "command": "/other/tool run"}, // foreign hook must survive
				}},
			},
			"PostToolUse": []any{"keep-me"}, // foreign event must survive
		},
	}
	b, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(settings, b, 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := removeOurPreToolUseHook(settings, exe)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("our hook was present but uninstall reported nothing removed")
	}
	if backup, err := os.ReadFile(settings + ".crossing-guard.bak"); err != nil {
		t.Fatalf("uninstall backup missing: %v", err)
	} else if string(backup) != string(b) {
		t.Fatalf("uninstall backup does not match immediate prior config:\n%s", backup)
	}

	var got map[string]any
	rb, _ := os.ReadFile(settings)
	if err := json.Unmarshal(rb, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "sonnet" {
		t.Error("uninstall dropped a foreign top-level key")
	}
	hooks := got["hooks"].(map[string]any)
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Error("uninstall dropped a foreign hook event")
	}
	pre, _ := hooks["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Fatalf("PreToolUse should keep exactly the foreign entry, got %d", len(pre))
	}
	entry := pre[0].(map[string]any)
	inner := entry["hooks"].([]any)
	cmd := inner[0].(map[string]any)["command"].(string)
	if cmd != "/other/tool run" {
		t.Errorf("wrong hook survived: %q", cmd)
	}

	// Idempotent: a second uninstall finds nothing of ours and reports so.
	removed2, err := removeOurPreToolUseHook(settings, exe)
	if err != nil {
		t.Fatal(err)
	}
	if removed2 {
		t.Error("second uninstall claimed to remove our hook again")
	}
}

// TestUninstallDropsEmptyPreToolUse: when OUR hook was the only PreToolUse entry, the
// key is removed entirely, so the file returns to not registering any PreToolUse hook.
func TestUninstallDropsEmptyPreToolUse(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	exe := "/opt/crossing-guard/crossing-guard"

	seed := map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": "*", "hooks": []any{
			map[string]any{"type": "command", "command": hookCommand(exe, claudeVendor)},
		}}},
	}}
	b, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(settings, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := removeOurPreToolUseHook(settings, exe); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	rb, _ := os.ReadFile(settings)
	_ = json.Unmarshal(rb, &got)
	if hooks, ok := got["hooks"].(map[string]any); ok {
		if _, still := hooks["PreToolUse"]; still {
			t.Error("empty PreToolUse key should have been removed")
		}
	}
}

// TestUninstallReinstallRoundTrip: install → uninstall → the config no longer reports
// our hook as current, and re-install makes it current again. The full bar-2 loop.
func TestUninstallReinstallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	exe := "/opt/crossing-guard/crossing-guard"
	inst := claudeInstaller{}

	if err := inst.Install(settings, exe); err != nil {
		t.Fatal(err)
	}
	if !inst.IsCurrent(settings, exe) {
		t.Fatal("install did not produce a current hook")
	}
	if _, err := inst.Uninstall(settings, exe); err != nil {
		t.Fatal(err)
	}
	if inst.IsCurrent(settings, exe) {
		t.Fatal("hook still current after uninstall")
	}
	if err := inst.Install(settings, exe); err != nil {
		t.Fatal(err)
	}
	if !inst.IsCurrent(settings, exe) {
		t.Fatal("reinstall did not restore a current hook")
	}
}

func TestLifecycleUninstallRemovesOnlyOwnedHandlersAcrossEveryPhase(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	exe := "/opt/crossing-guard/crossing-guard"
	installer := claudeInstaller{}
	if err := installer.Install(settings, exe); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	raw, _ := os.ReadFile(settings)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	hooks := cfg["hooks"].(map[string]any)
	for _, event := range claudeLifecycleHookEvents {
		entries := hooks[event].([]any)
		entry := entries[0].(map[string]any)
		inner := entry["hooks"].([]any)
		inner = append(inner, map[string]any{"type": "command", "command": "/foreign/collector"})
		entry["hooks"] = inner
	}
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := installer.Uninstall(settings, exe)
	if err != nil || !removed {
		t.Fatalf("removed=%t err=%v", removed, err)
	}
	raw, _ = os.ReadFile(settings)
	if strings.Contains(string(raw), hookCommand(exe, claudeVendor)) {
		t.Fatalf("owned lifecycle hook survived: %s", raw)
	}
	if strings.Count(string(raw), "/foreign/collector") != len(claudeLifecycleHookEvents) {
		t.Fatalf("foreign lifecycle hooks were not preserved: %s", raw)
	}
}
