package guardcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// fakeSelf is an executable named like ours, so ownership reads it as ours.
func fakeSelf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crossing-guard")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestClaudeRecallRegistersAllowsAndRemovesOnlyItsOwn(t *testing.T) {
	home := withTempHome(t)
	self := fakeSelf(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{"permissions":{"allow":["Bash(ls:*)"]},"model":"opus"}`)
	writeFile(t, filepath.Join(home, ".claude.json"), `{"numStartups":3,"mcpServers":{"other":{"type":"stdio","command":"x"}},"note":"<a&b>"}`)
	installer := claudeInstaller{}
	if state, err := installer.RecallStatus(settings, self); err != nil || state != RecallAbsent {
		t.Fatalf("before: %s %v", state, err)
	}
	if err := RecordConsent("claude", settings, self, false); err != nil {
		t.Fatal(err)
	}
	if err := RegisterRecallFor("claude", settings, self); err != nil {
		t.Fatal(err)
	}
	if state, _ := installer.RecallStatus(settings, self); state != RecallCurrent {
		t.Fatalf("after register: %s", state)
	}
	var user map[string]any
	_ = json.Unmarshal([]byte(readFile(t, filepath.Join(home, ".claude.json"))), &user)
	servers := user["mcpServers"].(map[string]any)
	entry := servers["crossing-guard"].(map[string]any)
	if servers["other"] == nil || user["numStartups"] != float64(3) || entry["command"] != self {
		t.Fatalf("user config = %v", user)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, ".claude.json")), "<a&b>") {
		t.Fatal("the rewrite escaped the user's <, > or &")
	}
	if text := readFile(t, settings); !strings.Contains(text, `"mcp__crossing-guard"`) || !strings.Contains(text, `"Bash(ls:*)"`) {
		t.Fatalf("settings = %s", text)
	}
	// A later hook re-consent (init repair, GUI connect) keeps the recall yes.
	if err := RecordConsent("claude", settings, self, false); err != nil {
		t.Fatal(err)
	}
	if LoadConsent().Vendors["claude"].Recall == nil {
		t.Fatal("re-recording the hook consent erased the recall consent")
	}
	removed, err := installer.UnregisterRecall(settings, self)
	if err != nil || !removed {
		t.Fatalf("unregister: %v %v", removed, err)
	}
	if text := readFile(t, filepath.Join(home, ".claude.json")); strings.Contains(text, "crossing-guard") || !strings.Contains(text, `"other"`) {
		t.Fatalf("user config after removal = %s", text)
	}
	if text := readFile(t, settings); strings.Contains(text, "mcp__crossing-guard") || !strings.Contains(text, "Bash(ls:*)") {
		t.Fatalf("settings after removal = %s", text)
	}

	// Another live program's entry under our name is foreign: never replaced or removed.
	other := filepath.Join(t.TempDir(), "some-other-tool")
	writeFile(t, other, "x")
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"crossing-guard":{"type":"stdio","command":"`+other+`","args":["serve"]}}}`)
	if state, _ := installer.RecallStatus(settings, self); state != RecallForeign {
		t.Fatalf("foreign entry read as %s", state)
	}
	if err := RegisterRecallFor("claude", settings, self); err == nil {
		t.Fatal("a foreign entry must not be overwritten")
	}
	if removed, _ := installer.UnregisterRecall(settings, self); removed {
		t.Fatal("a foreign entry must not be removed")
	}
}

func TestRecallNeedsHookConsent(t *testing.T) {
	home := withTempHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{}`)
	if err := RegisterRecallFor("claude", settings, fakeSelf(t)); err == nil {
		t.Fatal("recall registration without a hook consent must be refused")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Fatal("a refused registration wrote the user config")
	}
}

func TestCodexRecallBlockParsesAndRefusesForeignShapes(t *testing.T) {
	home := withTempHome(t)
	self := fakeSelf(t)
	codexHome := filepath.Join(home, ".codex")
	config := filepath.Join(codexHome, "config.toml")
	writeFile(t, config, "model = \"gpt\"\n\n[mcp_servers.other]\ncommand = \"x\"\n")
	installer := codexInstaller{}
	if err := installer.RegisterRecall(codexHome, self); err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if _, err := toml.Decode(readFile(t, config), &parsed); err != nil {
		t.Fatalf("the written config does not parse: %v", err)
	}
	server := parsed["mcp_servers"].(map[string]any)["crossing-guard"].(map[string]any)
	if server["command"] != self || server["default_tools_approval_mode"] != "approve" || parsed["model"] != "gpt" {
		t.Fatalf("parsed = %v", parsed)
	}
	if state, _ := installer.RecallStatus(codexHome, self); state != RecallCurrent {
		t.Fatalf("status %s", state)
	}
	if removed, err := installer.UnregisterRecall(codexHome, self); err != nil || !removed ||
		readFile(t, config) != "model = \"gpt\"\n\n[mcp_servers.other]\ncommand = \"x\"\n" {
		t.Fatalf("remove: %v %v %q", removed, err, readFile(t, config))
	}

	for name, body := range map[string]string{
		"inline table": "mcp_servers = { other = { command = \"x\" } }\n",
		"dotted keys":  "mcp_servers.crossing-guard.command = \"x\"\n",
		"header":       "[mcp_servers.crossing-guard]\ncommand = \"x\"\n",
	} {
		writeFile(t, config, body)
		if err := installer.RegisterRecall(codexHome, self); err == nil {
			t.Errorf("%s: a registration that cannot be written cleanly must be refused", name)
		}
		if readFile(t, config) != body {
			t.Errorf("%s: a refused registration changed the file", name)
		}
	}
}

func TestOpenCodeRecallRefusesCommentedJSON(t *testing.T) {
	home := withTempHome(t)
	self := fakeSelf(t)
	dir := filepath.Join(home, ".config", "opencode")
	path := filepath.Join(dir, "opencode.json")
	writeFile(t, path, "{\n  // a comment\n  \"model\": \"x\"\n}\n")
	installer := openCodeInstaller{}
	if err := installer.RegisterRecall(dir, self); err == nil {
		t.Fatal("a commented opencode.json must be refused, not rewritten")
	}
	writeFile(t, path, `{"model":"x","mcp":{"other":{"type":"local","command":["y"]}}}`)
	if err := installer.RegisterRecall(dir, self); err != nil {
		t.Fatal(err)
	}
	if state, _ := installer.RecallStatus(dir, self); state != RecallCurrent {
		t.Fatalf("status %s: %s", state, readFile(t, path))
	}
	if removed, err := installer.UnregisterRecall(dir, self); err != nil || !removed {
		t.Fatalf("remove %v %v", removed, err)
	}
	if text := readFile(t, path); strings.Contains(text, "crossing-guard") || !strings.Contains(text, `"other"`) {
		t.Fatalf("after removal: %s", text)
	}
}

func TestEnsureRecallRepairsOnlyWhereRecallWasConsented(t *testing.T) {
	home := withTempHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{}`)
	codexHome := filepath.Join(home, ".codex")
	writeFile(t, filepath.Join(codexHome, "config.toml"), "")
	if err := RecordConsent("claude", settings, "/x", false); err != nil {
		t.Fatal(err)
	}
	if err := RecordConsent("codex", codexHome, "/x", false); err != nil {
		t.Fatal(err)
	}
	if err := RecordRecallConsent("claude", filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	statuses := EnsureRecall()
	if len(statuses) != 1 || statuses[0].Vendor != "claude" || statuses[0].Action != "installed" {
		t.Fatalf("statuses %+v", statuses)
	}
	if strings.Contains(readFile(t, filepath.Join(codexHome, "config.toml")), "crossing-guard") {
		t.Fatal("boot registered recall for a runtime whose recall was never consented")
	}
}

// Codex writes config.toml itself; anything found between our markers that we
// did not write must survive every repair and removal.
func TestCodexRecallLeavesForeignContentInsideItsMarkersAlone(t *testing.T) {
	home := withTempHome(t)
	self := fakeSelf(t)
	codexHome := filepath.Join(home, ".codex")
	config := filepath.Join(codexHome, "config.toml")
	installer := codexInstaller{}
	writeFile(t, config, "model = \"x\"\n")
	if err := installer.RegisterRecall(codexHome, self); err != nil {
		t.Fatal(err)
	}
	spliced := strings.Replace(readFile(t, config), codexRecallEnd,
		"\n[projects.\"/work\"]\ntrust_level = \"trusted\"\n"+codexRecallEnd, 1)
	writeFile(t, config, spliced)
	if _, err := installer.RecallStatus(codexHome, self); err == nil {
		t.Fatal("foreign content inside the markers must be an error, not a repairable state")
	}
	if err := installer.RegisterRecall(codexHome, fakeSelf(t)); err == nil || readFile(t, config) != spliced {
		t.Fatal("a repair must not replace a span holding foreign settings")
	}
	if removed, err := installer.UnregisterRecall(codexHome, self); err == nil || removed || readFile(t, config) != spliced {
		t.Fatal("a removal must not delete a span holding foreign settings")
	}
}

// Uninstall forgets the recall yes even when no hook was left to remove, so a
// later boot never re-registers what the user removed.
func TestUninstallForgetsRecallConsentWithoutAHook(t *testing.T) {
	home := withTempHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{}`)
	self := fakeSelf(t)
	if err := RecordConsent("claude", settings, self, false); err != nil {
		t.Fatal(err)
	}
	if err := RegisterRecallFor("claude", settings, self); err != nil {
		t.Fatal(err)
	}
	statuses := uninstallRecall(self)
	if len(statuses) != 1 || statuses[0].Action != "removed" {
		t.Fatalf("statuses %+v", statuses)
	}
	if consent := LoadConsent().Vendors["claude"]; consent.Recall != nil {
		t.Fatal("the recall yes survived uninstall")
	}
	if got := EnsureRecall(); len(got) != 0 {
		t.Fatalf("boot re-registered after uninstall: %+v", got)
	}
	if strings.Contains(readFile(t, filepath.Join(home, ".claude.json")), "crossing-guard") {
		t.Fatal("the entry came back")
	}
}

// An entry left by a binary that no longer exists is ours to repair and remove.
func TestRecallEntryOfADeletedBinaryIsStaleAndRemovable(t *testing.T) {
	home := withTempHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{}`)
	gone := filepath.Join(t.TempDir(), "crossing-guard") // never created
	writeFile(t, filepath.Join(home, ".claude.json"),
		`{"mcpServers":{"crossing-guard":{"type":"stdio","command":"`+gone+`","args":["mcp","--runtime","claude"]}}}`)
	installer := claudeInstaller{}
	self := fakeSelf(t)
	if state, _ := installer.RecallStatus(settings, self); state != RecallStale {
		t.Fatalf("state %s", state)
	}
	if removed, err := installer.UnregisterRecall(settings, self); err != nil || !removed {
		t.Fatalf("remove %v %v", removed, err)
	}
	if text := readFile(t, filepath.Join(home, ".claude.json")); strings.Contains(text, "mcpServers") {
		t.Fatalf("an emptied mcpServers must go with the entry: %s", text)
	}
}

// A hook yes recorded for another config never carries the recall yes to it.
func TestRecallConsentDoesNotFollowAMovedConfig(t *testing.T) {
	home := withTempHome(t)
	first := filepath.Join(home, ".codex")
	if err := RecordConsent("codex", first, "/x", false); err != nil {
		t.Fatal(err)
	}
	if err := RecordRecallConsent("codex", filepath.Join(first, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := RecordConsent("codex", first, "/y", false); err != nil {
		t.Fatal(err)
	}
	if LoadConsent().Vendors["codex"].Recall == nil {
		t.Fatal("re-consenting the same config dropped recall")
	}
	if err := RecordConsent("codex", filepath.Join(home, "other-codex-home"), "/y", false); err != nil {
		t.Fatal(err)
	}
	if LoadConsent().Vendors["codex"].Recall != nil {
		t.Fatal("recall followed the hook consent to a config the user never agreed to")
	}
}
