package guardcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func plantedHookConfig(stale string) []byte {
	cfg := map[string]any{
		"foreign": "keep",
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "Bash",
				"hooks": []any{
					map[string]any{"type": "command", "command": stale},
					map[string]any{"type": "command", "command": "/foreign/tool guard"},
				},
			}},
			"PostToolUse": []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": "/foreign/post"},
			}}},
		},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	return append(raw, '\n')
}

func assertPreservedAndCurrent(t *testing.T, path, wantCommand string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["foreign"] != "keep" {
		t.Fatal("foreign top-level field was lost")
	}
	serialized := string(raw)
	for _, want := range []string{"/foreign/tool guard", "/foreign/post"} {
		if !strings.Contains(serialized, want) {
			t.Fatalf("config lost %q:\n%s", want, serialized)
		}
	}
	owned, current := preToolUseHookCounts(cfg, wantCommand, func(map[string]any) bool { return true })
	if owned != 1 || current != 1 {
		t.Fatalf("owned/current hook counts = %d/%d, want 1/1:\n%s", owned, current, serialized)
	}
	if strings.Contains(serialized, "/old/crossing-guard") {
		t.Fatalf("stale owned handler survived:\n%s", serialized)
	}
}

func assertExpectedHookPhases(t *testing.T, status map[string]bool, expected ...string) {
	t.Helper()
	if len(status) != len(expected) {
		t.Fatalf("hook phase status keys=%v want exactly %v", status, expected)
	}
	for _, phase := range expected {
		current, present := status[phase]
		if !present || !current {
			t.Fatalf("hook phase %s present=%t current=%t status=%v", phase, present, current, status)
		}
	}
}

func duplicateHookConfig(command string) []byte {
	cfg := map[string]any{
		"foreign": "keep",
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{"matcher": "*", "hooks": []any{
					map[string]any{"type": "command", "command": command},
					map[string]any{"type": "command", "command": "/foreign/tool guard"},
				}},
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command}}},
			},
			"PostToolUse": []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": "/foreign/post"},
			}}},
		},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	return append(raw, '\n')
}

func TestInstallRepairsDuplicateOwnedHooks(t *testing.T) {
	exe := "/opt/current/crossing-guard"
	t.Run("claude", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, duplicateHookConfig(hookCommand(exe, claudeVendor)), 0o600); err != nil {
			t.Fatal(err)
		}
		if (claudeInstaller{}).IsCurrent(path, exe) {
			t.Fatal("duplicate Claude handlers reported current")
		}
		if err := (claudeInstaller{}).Install(path, exe); err != nil {
			t.Fatal(err)
		}
		assertPreservedAndCurrent(t, path, hookCommand(exe, claudeVendor))
	})
	t.Run("codex", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "hooks.json")
		if err := os.WriteFile(path, duplicateHookConfig(hookCommand(exe, codexVendor)), 0o600); err != nil {
			t.Fatal(err)
		}
		if (codexInstaller{}).IsCurrent(home, exe) {
			t.Fatal("duplicate Codex handlers reported current")
		}
		if err := (codexInstaller{}).Install(home, exe); err != nil {
			t.Fatal(err)
		}
		assertPreservedAndCurrent(t, path, hookCommand(exe, codexVendor))
	})
}

func TestLifecycleHookStatusDoesNotHidePartialInstallation(t *testing.T) {
	exe := "/opt/current/crossing-guard"
	t.Run("claude", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		cfg := map[string]any{"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": hookCommand(exe, claudeVendor)},
			}}},
		}}
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		installer := claudeInstaller{}
		status := installer.HookPhaseStatus(path, exe)
		if status["SessionStart"] || !status["PreToolUse"] || status["PostToolUse"] || status["SessionEnd"] || installer.IsCurrent(path, exe) {
			t.Fatalf("partial lifecycle status = %+v current=%t", status, installer.IsCurrent(path, exe))
		}
		if err := installer.Install(path, exe); err != nil {
			t.Fatal(err)
		}
		assertExpectedHookPhases(t, installer.HookPhaseStatus(path, exe), claudeLifecycleHookEvents...)
	})
	t.Run("codex", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "hooks.json")
		cfg := map[string]any{"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": hookCommand(exe, codexVendor)},
			}}},
		}}
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		installer := codexInstaller{}
		status := installer.HookPhaseStatus(home, exe)
		if status["SessionStart"] || !status["PreToolUse"] || status["PostToolUse"] || status["SessionEnd"] || installer.IsCurrent(home, exe) {
			t.Fatalf("partial lifecycle status = %+v current=%t", status, installer.IsCurrent(home, exe))
		}
		if err := installer.Install(home, exe); err != nil {
			t.Fatal(err)
		}
		assertExpectedHookPhases(t, installer.HookPhaseStatus(home, exe), codexLifecycleHookEvents...)
	})
}

func TestClaudeInstallPreservesForeignHooksAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	seed := plantedHookConfig(`"/old/crossing-guard" hook --runtime claude`)
	if err := os.WriteFile(path, seed, 0o640); err != nil {
		t.Fatal(err)
	}
	exe := "/opt/current/crossing-guard"
	if err := (claudeInstaller{}).Install(path, exe); err != nil {
		t.Fatal(err)
	}
	assertPreservedAndCurrent(t, path, hookCommand(exe, claudeVendor))
	backup := path + ".crossing-guard.bak"
	if got, _ := os.ReadFile(backup); string(got) != string(seed) {
		t.Fatalf("backup mismatch:\n%s", got)
	}
	if err := os.WriteFile(backup, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (claudeInstaller{}).Install(path, exe); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(backup); string(got) != "sentinel" {
		t.Fatalf("idempotent install rewrote backup: %q", got)
	}
}

func TestCodexInstallPreservesForeignHooksAndBacksUp(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "hooks.json")
	seed := plantedHookConfig(`"/old/crossing-guard" hook --runtime codex`)
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	exe := "/opt/current/crossing-guard"
	if err := (codexInstaller{}).Install(home, exe); err != nil {
		t.Fatal(err)
	}
	assertPreservedAndCurrent(t, path, hookCommand(exe, codexVendor))
	if got, _ := os.ReadFile(path + ".crossing-guard.bak"); string(got) != string(seed) {
		t.Fatalf("backup mismatch:\n%s", got)
	}
}

func TestCodexInstallRefusesMalformedPresentConfig(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "hooks.json")
	broken := []byte("{broken")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (codexInstaller{}).Install(home, "/opt/crossing-guard"); err == nil {
		t.Fatal("malformed config must fail")
	}
	if got, _ := os.ReadFile(path); string(got) != string(broken) {
		t.Fatalf("malformed config changed: %q", got)
	}
	if _, err := os.Stat(path + ".crossing-guard.bak"); !os.IsNotExist(err) {
		t.Fatalf("malformed config created backup: %v", err)
	}
}

func TestGuardInstallRefusesWrongHookContainerShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "hooks is scalar", raw: `{"foreign":"keep","hooks":"vendor-owned"}`},
		{name: "PreToolUse is scalar", raw: `{"foreign":"keep","hooks":{"PreToolUse":"vendor-owned"}}`},
	} {
		for _, vendor := range []string{"claude", "codex"} {
			t.Run(vendor+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "settings.json")
				seed := []byte(tc.raw)
				if vendor == "codex" {
					path = filepath.Join(dir, "hooks.json")
				}
				if err := os.WriteFile(path, seed, 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				if vendor == "claude" {
					err = (claudeInstaller{}).Install(path, "/opt/crossing-guard")
				} else {
					err = (codexInstaller{}).Install(dir, "/opt/crossing-guard")
				}
				if err == nil {
					t.Fatal("wrong-shaped hook container must fail")
				}
				if got, _ := os.ReadFile(path); string(got) != string(seed) {
					t.Fatalf("wrong-shaped config changed: %q", got)
				}
				if _, err := os.Stat(path + ".crossing-guard.bak"); !os.IsNotExist(err) {
					t.Fatalf("wrong-shaped config created backup: %v", err)
				}
			})
		}
	}
}
