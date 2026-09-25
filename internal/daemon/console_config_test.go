package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleConfigDefaultsValidateAndPublish(t *testing.T) {
	config, origin, err := loadConsoleConfig(t.TempDir())
	if err != nil || origin != "builtin-default" {
		t.Fatalf("missing file must yield the defaults: origin=%q err=%v", origin, err)
	}
	if err := config.validate(); err != nil {
		t.Fatalf("compiled defaults must validate: %v", err)
	}
	recorder := httptest.NewRecorder()
	handleConsoleConfig(recorder, httptest.NewRequest("GET", "/api/console/config", nil))
	var body consoleConfigResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Config.DefaultPreset == "" || body.Config.Keymap["hide_workspace"] == "" || body.Config.EditorScheme == "" {
		t.Fatalf("published config lost its policy: %s", recorder.Body.String())
	}
	if _, ok := body.Config.Presets[body.Config.DefaultPreset]; !ok {
		t.Fatalf("default preset %q is not published", body.Config.DefaultPreset)
	}
}

func TestConsoleConfigRefusesUnknownFieldsAndBadPresets(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "console.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"format_version":1,"unknown_key":true}`)
	if _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field must be refused: %v", err)
	}
	write(`{"format_version":1,"default_preset":"nope"}`)
	if _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "default_preset") {
		t.Fatalf("a default preset that does not exist must be refused: %v", err)
	}
	write(`{"format_version":1,"presets":{"review":{"type":"split","dir":"diagonal","ratio":0.5,"a":{"type":"region","tabs":[]},"b":{"type":"region","tabs":[]}}}}`)
	if _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "dir") {
		t.Fatalf("a bad split direction must be refused: %v", err)
	}
	write(`{"format_version":1,"split_clamp":[0.9,0.1]}`)
	if _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "split_clamp") {
		t.Fatalf("an inverted clamp must be refused: %v", err)
	}
	write(`{"format_version":1,"recently_closed":3,"keymap":{"hide_workspace":"Meta+.","show_files":"Meta+Shift+Y","open_in_editor":"Meta+Shift+O"}}`)
	config, origin, err := loadConsoleConfig(dir)
	if err != nil || origin != filepath.Join(dir, "console.json") || config.RecentlyClosed != 3 || config.DefaultPreset != "review" {
		t.Fatalf("a valid partial file must overlay the defaults: origin=%q config=%+v err=%v", origin, config, err)
	}
}

func TestConsoleConfigRejectsNonPositiveFilesBudgets(t *testing.T) {
	config := defaultConsoleConfig()
	config.Files.MaxReadBytes = 0
	if err := config.validate(); err == nil || !strings.Contains(err.Error(), "files.") {
		t.Fatalf("zero read budget accepted: %v", err)
	}
	config = defaultConsoleConfig()
	config.Files.MaxEntriesPerDir = -1
	if err := config.validate(); err == nil {
		t.Fatal("negative entries budget accepted")
	}
}
