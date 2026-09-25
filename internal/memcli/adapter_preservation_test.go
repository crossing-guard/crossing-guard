package memcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeMemoryAttachPreservesForeignConfigAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	seed := []byte(`{"model":"opus","hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"/foreign/guard"}]}]}}` + "\n")
	if err := os.WriteFile(path, seed, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (claudeAdapter{}).Attach(path, "/opt/crossing-guard"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"model\": \"opus\"", "/foreign/guard", "/opt/crossing-guard memory index"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("memory attach lost %q:\n%s", want, raw)
		}
	}
	// guardcli is the ONE writer of vendor lifecycle hooks (2026-09-01, E22
	// amended): memory attach must not write a Stop hook any more.
	if strings.Contains(string(raw), "sync --quiet") {
		t.Fatalf("memory attach still writes a Stop hook:\n%s", raw)
	}
	if got, _ := os.ReadFile(path + ".crossing-guard.bak"); string(got) != string(seed) {
		t.Fatalf("backup mismatch:\n%s", got)
	}
}

func TestCodexMemoryAttachReplacesOnlyOwnedBlockAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := []byte(`model = "gpt"

# >>> crossing-guard memory (cpmem attach codex) >>>
[[hooks.SessionStart]]
[[hooks.SessionStart.hooks]]
type = "command"
command = "/old/crossing-guard memory index"
# <<< crossing-guard memory <<<

[foreign]
keep = true
`)
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (codexAdapter{}).Attach(path, "/opt/current/crossing-guard"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{`model = "gpt"`, "[foreign]", "keep = true", "/opt/current/crossing-guard memory index"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("memory attach lost %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "sync --quiet") || strings.Contains(string(raw), "[[hooks.Stop]]") {
		t.Fatalf("memory attach still writes a Codex Stop hook:\n%s", raw)
	}
	if strings.Contains(string(raw), "/old/crossing-guard") {
		t.Fatalf("old owned block survived:\n%s", raw)
	}
	if got, _ := os.ReadFile(path + ".crossing-guard.bak"); string(got) != string(seed) {
		t.Fatalf("backup mismatch:\n%s", got)
	}
}

func TestClaudeMemoryAttachRefusesWrongHookContainerShapes(t *testing.T) {
	for _, raw := range []string{
		`{"foreign":"keep","hooks":"vendor-owned"}`,
		`{"foreign":"keep","hooks":{"SessionStart":"vendor-owned"}}`,
	} {
		path := filepath.Join(t.TempDir(), "settings.json")
		seed := []byte(raw)
		if err := os.WriteFile(path, seed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := (claudeAdapter{}).Attach(path, "/opt/crossing-guard"); err == nil {
			t.Fatalf("wrong-shaped hook container must fail: %s", raw)
		}
		if got, _ := os.ReadFile(path); string(got) != string(seed) {
			t.Fatalf("wrong-shaped config changed: %q", got)
		}
		if _, err := os.Stat(path + ".crossing-guard.bak"); !os.IsNotExist(err) {
			t.Fatalf("wrong-shaped config created backup: %v", err)
		}
	}
}
