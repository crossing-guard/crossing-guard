package memcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"crossing-guard/internal/vendorconfig"
	"crossing-guard/internal/vendorpaths"
)

func init() { registerAdapter(claudeAdapter{}) }

// claudeAdapter integrates memory hooks into a Claude settings.json.
type claudeAdapter struct{}

func (claudeAdapter) Name() string       { return "claude" }
func (claudeAdapter) ConfigFlag() string { return "settings" }
func (claudeAdapter) DefaultConfigPath() string {
	return filepath.Join(home(), filepath.FromSlash(vendorpaths.ClaudeSettingsRelative))
}

// Attach merges SessionStart/Stop hooks into a Claude settings.json, preserving
// everything already there.
func (claudeAdapter) Attach(settingsPath, selfPath string) error {
	snapshot, err := vendorconfig.Read(settingsPath)
	if err != nil {
		return err
	}
	cfg := map[string]any{}
	if snapshot.Exists {
		if err := json.Unmarshal(snapshot.Data, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	}
	rawHooks, hooksPresent := cfg["hooks"]
	hooks, hooksOK := rawHooks.(map[string]any)
	if hooksPresent && !hooksOK {
		return fmt.Errorf("%s: hooks must be a JSON object; refusing to replace it", settingsPath)
	}
	if !hooksPresent {
		hooks = map[string]any{}
	}
	changed := false
	added, err := ensureHookEntry(hooks, "SessionStart", "startup|resume|clear|compact", selfPath+" memory index")
	if err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	if added {
		changed = true
	}
	// Stop is no longer written here. guardcli is the ONE writer of vendor
	// lifecycle hooks (2026-09-01, amends E22); memory sync on turn end is
	// dropped — the lifecycle hooks already deliver the source hints the
	// index needs, and two features writing one hook event was a defect.
	if !changed {
		fmt.Println("already installed:", settingsPath)
		return nil
	}
	cfg["hooks"] = hooks
	out, _ := json.MarshalIndent(cfg, "", "  ")
	backup, err := vendorconfig.Replace(settingsPath, snapshot, append(out, '\n'))
	if err != nil {
		return err
	}
	fmt.Println("installed SessionStart hook →", settingsPath)
	if backup != "" {
		fmt.Println("backup (immediate prior) →", backup)
	}
	fmt.Println("verify: start a fresh claude session, then run: crossing-guard doctor")
	return nil
}

// MemoryHookBinary reads settings.json for our SessionStart memory hook.
func (claudeAdapter) MemoryHookBinary(settingsPath string) string {
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	for _, entry := range cfg.Hooks["SessionStart"] {
		for _, h := range entry.Hooks {
			if bin := memoryHookBinaryFromCommand(h.Command); bin != "" {
				return bin
			}
		}
	}
	return ""
}

// InjectedText recognizes Claude's injected-context line: an `attachment` row
// with attachment.hookEvent=SessionStart (observed shape 2026-07-16).
func (claudeAdapter) InjectedText(line []byte) (string, bool) {
	var row struct {
		Type       string `json:"type"`
		Attachment struct {
			HookEvent string `json:"hookEvent"`
			Content   string `json:"content"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &row) != nil {
		return "", false
	}
	if row.Type == "attachment" && row.Attachment.HookEvent == "SessionStart" {
		return row.Attachment.Content, true
	}
	return "", false
}
