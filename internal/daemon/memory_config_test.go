package daemon

// memory_config_test.go — the memory owner's config (config-ownership plan
// Fix A): the propose section validates, the shipped example loads, the
// consent is live (RT-C1), old console keys decode as deprecated with a note
// (RT-C4), and the rename adopts idle-only (RT-C3).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryConfigProposeValidation(t *testing.T) {
	if err := (MemoryConfig{FormatVersion: 1, ImportMinIntervalSeconds: 300,
		Propose: MemoryProposeConfig{Enabled: true, PerSessionMax: 0}}).validate(); err == nil {
		t.Fatal("enabled with a zero bound must be refused")
	}
	if err := (MemoryConfig{FormatVersion: 1, ImportMinIntervalSeconds: 300,
		Propose: MemoryProposeConfig{Enabled: false, PerSessionMax: 0}}).validate(); err != nil {
		t.Fatalf("disabled with a zero bound is the default shape: %v", err)
	}
}

func TestShippedMemoryExampleLoads(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile("../../config/memory.example.json")
	if err != nil {
		t.Fatalf("the shipped example is missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	config, origin, err := loadMemoryConfig(dir)
	if err != nil {
		t.Fatalf("the shipped example must load cleanly: %v", err)
	}
	if origin == "builtin-default" {
		t.Fatal("the example was not loaded")
	}
	if config.Propose.Enabled || config.Propose.PerSessionMax != 5 {
		t.Fatalf("example propose section mismatch: %+v", config.Propose)
	}
}

// RT-C4: a daemon.json still carrying the pre-move recall.propose_* keys
// loads, its other settings survive, and the move is named in problems.
func TestDeprecatedProposeKeysAreNotedNotFatal(t *testing.T) {
	dir := t.TempDir()
	body := `{"format_version":1,"default_preset":"review","recall":{"request_timeout_ms":15000,"max_result_bytes":60000,"propose_enabled":true,"propose_per_session_max":9}}`
	if err := os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config, _, problems, err := loadConsoleConfig(dir)
	if err != nil {
		t.Fatalf("the strict loader must not refuse the moved keys: %v", err)
	}
	if config.Recall.RequestTimeoutMS != 15000 {
		t.Fatalf("other recall settings must survive: %+v", config.Recall)
	}
	var noted bool
	for _, problem := range problems {
		if strings.Contains(problem, "moved to memory.json") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the move must be named in problems: %v", problems)
	}
}

// RT-C3: a data dir holding only console.json adopts daemon.json on the next
// refresh; the old file is left in place; a second refresh reads the new name.
func TestConfigAdoptionFromLegacyName(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"format_version":1,"default_preset":"review","keymap":{"hide_workspace":"ctrl+h"}}`
	if err := os.WriteFile(filepath.Join(dir, "console.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	config, origin := consoleConfigForDir(t, dir)
	if origin == "builtin-default" {
		t.Fatalf("the legacy file must be adopted, origin=%s", origin)
	}
	if _, err := os.Stat(filepath.Join(dir, "daemon.json")); err != nil {
		t.Fatalf("daemon.json must exist after adoption: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "console.json")); err != nil || string(raw) != legacy {
		t.Fatalf("the old file must be left untouched, err=%v", err)
	}
	if config.Keymap["hide_workspace"] != "ctrl+h" {
		t.Fatalf("adopted settings must carry over: %+v", config.Keymap)
	}
	// The state token now refers to the new file; the old one is ignored.
	_, origin = consoleConfigForDir(t, dir)
	if origin != filepath.Join(dir, "daemon.json") {
		t.Fatalf("the second read must use the new name, origin=%s", origin)
	}
}

// consoleConfigForDir drives the config state at one data dir for a test,
// isolated from the package's global state. The state's fields are reset in
// place (a consoleConfigState copy would move its mutex — staticcheck's
// lock-copy rule; the fields below are the whole mutable surface).
func consoleConfigForDir(t *testing.T, dir string) (ConsoleConfig, string) {
	t.Helper()
	consoleState.mu.Lock()
	prevLoaded, prevDirty, prevDataDir := consoleState.loaded, consoleState.dirty, consoleState.dataDir
	consoleState.loaded, consoleState.dirty, consoleState.dataDir = false, false, dir
	consoleState.mu.Unlock()
	t.Cleanup(func() {
		consoleState.mu.Lock()
		consoleState.loaded, consoleState.dirty, consoleState.dataDir = prevLoaded, prevDirty, prevDataDir
		consoleState.mu.Unlock()
	})
	setIndexPath(dir)
	t.Cleanup(func() { setIndexPath("") })
	config, origin := consoleConfig()
	return config, origin
}
