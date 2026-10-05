package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// §8.4: the handoff keys' defaults come from the embedded daemon.default.json; an
// existing daemon.json without the section still loads; a bad section falls back to
// the defaults with a named problem and leaves the rest of the file in force.
func TestDaemonDefaultDocumentLoads(t *testing.T) {
	h, err := defaultHandoffConfig()
	if err != nil {
		t.Fatal(err)
	}
	if h.InjectMaxBytes != 4900 || h.FiringWindow.Duration != 720*time.Hour || h.BriefWait.Duration != 168*time.Hour ||
		h.ConversationTurns != 20 || h.ConversationMaxBytes != 65536 {
		t.Fatalf("embedded defaults: %+v", h)
	}
	if got := defaultConsoleConfig().Handoff; got != h {
		t.Fatalf("the defaults in force are the embedded document's: %+v", got)
	}
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"format_version":1,"recently_closed":3}`)
	config, _, problems, err := loadConsoleConfig(dir)
	if err != nil || len(problems) != 0 || config.Handoff != h || config.RecentlyClosed != 3 {
		t.Fatalf("a daemon.json without the section still loads: %+v problems=%v err=%v", config.Handoff, problems, err)
	}
	write(`{"format_version":1,"handoff":{"conversation_turns":5,"brief_wait":"24h"}}`)
	config, _, problems, err = loadConsoleConfig(dir)
	if err != nil || len(problems) != 0 || config.Handoff.ConversationTurns != 5 || config.Handoff.BriefWait.Duration != 24*time.Hour ||
		config.Handoff.InjectMaxBytes != 4900 {
		t.Fatalf("a partial section overlays the defaults: %+v problems=%v err=%v", config.Handoff, problems, err)
	}
	for _, bad := range []string{`{"conversation_turns":0}`, `{"conversation_turns":201}`, `{"conversation_max_bytes":70000}`,
		`{"inject_max_bytes":-1}`, `{"firing_window":"0s"}`} {
		write(`{"format_version":1,"recently_closed":3,"handoff":` + bad + `}`)
		config, _, problems, err = loadConsoleConfig(dir)
		if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "handoff.") || config.Handoff != h || config.RecentlyClosed != 3 {
			t.Fatalf("%s: the section falls back, named, and the rest stays in force: %+v problems=%v err=%v", bad, config.Handoff, problems, err)
		}
	}
	write(`{"format_version":1,"handoff":{"surprise":1}}`)
	if _, _, _, err := loadConsoleConfig(dir); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("an unknown handoff key is refused like any other: %v", err)
	}
}
