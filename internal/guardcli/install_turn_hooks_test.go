package guardcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every turn-boundary event's command carries OUR kind; the binary dispatches
// on it and never has to know the vendor's name (decision 1, 2026-09-01).
func TestClaudeInstallBakesObserveKindsAndMatchers(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	exe := "/opt/crossing-guard/crossing-guard"
	if err := (claudeInstaller{}).Install(settings, exe); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(settings)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	want := map[string]string{"UserPromptSubmit": "turn.started", "Stop": "turn.ended",
		"Notification": "input.requested", "SubagentStop": "subagent.ended", "PreCompact": "context.compacted"}
	for event, kind := range want {
		entries, _ := hooks[event].([]any)
		if len(entries) != 1 {
			t.Fatalf("%s: want one owned entry, got %d", event, len(entries))
		}
		entry := entries[0].(map[string]any)
		command := entry["hooks"].([]any)[0].(map[string]any)["command"].(string)
		if !strings.HasSuffix(command, "--observe "+kind) {
			t.Fatalf("%s command must carry our kind: %q", event, command)
		}
		if event == "Notification" && entry["matcher"] != claudeNotificationMatcher {
			t.Fatalf("Notification must be limited to real asks, got matcher %v", entry["matcher"])
		}
	}
	for _, event := range []string{"SessionStart", "PreToolUse", "PostToolUse", "PostToolUseFailure", "SessionEnd"} {
		command := hooks[event].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
		if strings.Contains(command, "--observe") {
			t.Fatalf("%s is not a turn boundary and must not carry --observe: %q", event, command)
		}
	}
	if !(claudeInstaller{}).IsCurrent(settings, exe) {
		t.Fatal("a fresh install must read as current")
	}
	// An older install that lacks the Notification matcher is NOT current:
	// it would fire on the idle nudge every minute.
	entries := hooks["Notification"].([]any)
	delete(entries[0].(map[string]any), "matcher")
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if (claudeInstaller{}).IsCurrent(settings, exe) {
		t.Fatal("a Notification entry without its matcher must read as not current")
	}
}

func TestCodexInstallBakesObserveKinds(t *testing.T) {
	home := t.TempDir()
	exe := "/opt/crossing-guard/crossing-guard"
	if err := (codexInstaller{}).Install(home, exe); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	hooks, _ := cfg["hooks"].(map[string]any)
	for _, row := range codexHookTable {
		command := hooks[row.Event].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
		if row.Observe != "" && !strings.HasSuffix(command, "--observe "+row.Observe) {
			t.Fatalf("%s: %q", row.Event, command)
		}
		if row.Observe == "" && strings.Contains(command, "--observe") {
			t.Fatalf("%s must not carry --observe: %q", row.Event, command)
		}
	}
}
