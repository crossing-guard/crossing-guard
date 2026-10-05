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
	config, origin, _, err := loadConsoleConfig(t.TempDir())
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
		if err := os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"format_version":1,"unknown_key":true}`)
	if _, _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field must be refused: %v", err)
	}
	write(`{"format_version":1,"default_preset":"nope"}`)
	if _, _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "default_preset") {
		t.Fatalf("a default preset that does not exist must be refused: %v", err)
	}
	write(`{"format_version":1,"presets":{"review":{"type":"split","dir":"diagonal","ratio":0.5,"a":{"type":"region","tabs":[]},"b":{"type":"region","tabs":[]}}}}`)
	if _, _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "dir") {
		t.Fatalf("a bad split direction must be refused: %v", err)
	}
	write(`{"format_version":1,"split_clamp":[0.9,0.1]}`)
	if _, _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "split_clamp") {
		t.Fatalf("an inverted clamp must be refused: %v", err)
	}
	write(`{"format_version":1,"recently_closed":3,"keymap":{"hide_workspace":"Meta+.","show_files":"Meta+Shift+Y","open_in_editor":"Meta+Shift+O"}}`)
	config, origin, _, err := loadConsoleConfig(dir)
	if err != nil || origin != filepath.Join(dir, "daemon.json") || config.RecentlyClosed != 3 || config.DefaultPreset != "review" {
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

// The board column bound is configuration with a compiled ceiling, so one
// typo cannot make a column read unbounded (board-column-overflow plan §2.1).
func TestConsoleConfigBoundsTheBoardColumnBudget(t *testing.T) {
	if got := defaultConsoleConfig().SessionOrganization.BoardColumnCardsMax; got != 200 {
		t.Fatalf("board_column_cards_max default = %d, want 200", got)
	}
	for _, value := range []int{0, consoleBoardColumnCardsCeiling + 1} {
		config := defaultConsoleConfig()
		config.SessionOrganization.BoardColumnCardsMax = value
		if err := config.validate(); err == nil || !strings.Contains(err.Error(), "board_column_cards_max") {
			t.Fatalf("board_column_cards_max %d accepted: %v", value, err)
		}
	}
}

// The Handoffs group of the rail lists a few ended handoffs before "Show ended"
// (journey R-3). How many is configuration: published with the rail's other
// budgets, read over the default from a file that sets it, and refused at zero.
func TestConsoleConfigPublishesTheHandoffsEndedBudget(t *testing.T) {
	if got := defaultConsoleConfig().SessionOrganization.HandoffsEndedVisible; got != 3 {
		t.Fatalf("handoffs_ended_visible default = %d, want 3", got)
	}
	published, err := json.Marshal(defaultConsoleConfig().SessionOrganization)
	if err != nil || !strings.Contains(string(published), `"handoffs_ended_visible":3`) {
		t.Fatalf("handoffs_ended_visible is not published: %s (%v)", published, err)
	}
	config, _, err := parseConsoleConfig([]byte(`{"session_organization":{"handoffs_ended_visible":5}}`), t.TempDir())
	if err != nil || config.SessionOrganization.HandoffsEndedVisible != 5 || config.SessionOrganization.ViewsVisible != 12 {
		t.Fatalf("a file that sets only handoffs_ended_visible: %+v (%v)", config.SessionOrganization, err)
	}
	config = defaultConsoleConfig()
	config.SessionOrganization.HandoffsEndedVisible = 0
	if err := config.validate(); err == nil || !strings.Contains(err.Error(), "handoffs_ended_visible") {
		t.Fatalf("handoffs_ended_visible 0 accepted: %v", err)
	}
}

// The Usage pane's presentation defaults are configuration (session usage
// breakdown plan §5.5): published, and a bad value refused.
func TestConsoleConfigUsageDefaults(t *testing.T) {
	config := defaultConsoleConfig()
	if config.Usage.TimelineGapMinutes != 20 || config.Usage.WideColumnsPx != 720 ||
		config.Usage.DefaultMeasure != "all" || config.Usage.DefaultGrouping != "tree" {
		t.Fatalf("usage defaults: %+v", config.Usage)
	}
	for _, mutate := range []func(*ConsoleConfig){
		func(c *ConsoleConfig) { c.Usage.TimelineGapMinutes = 0 },
		func(c *ConsoleConfig) { c.Usage.WideColumnsPx = -1 },
		func(c *ConsoleConfig) { c.Usage.DefaultMeasure = "cost" },
		func(c *ConsoleConfig) { c.Usage.DefaultGrouping = "flat" },
	} {
		bad := defaultConsoleConfig()
		mutate(&bad)
		if err := bad.validate(); err == nil || !strings.Contains(err.Error(), "usage.") {
			t.Fatalf("refused: %+v err=%v", bad.Usage, err)
		}
	}
}
