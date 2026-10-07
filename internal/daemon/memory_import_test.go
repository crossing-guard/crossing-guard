package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"crossing-guard/memory"
	"crossing-guard/store"
)

func writeClaudeTopicFile(t *testing.T, home, project, name, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", project, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".md")
	content := "---\nname: " + name + "\ndescription: " + name + " description\ntype: project\n---\n\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// memory.json: missing = defaults, malformed = an error the caller sees,
// values validated.
func TestMemoryConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	config, origin, err := loadMemoryConfig(dir)
	if err != nil || origin != "builtin-default" || config.ImportMinIntervalSeconds != 300 || !config.ImportOnSessionEnd {
		t.Fatalf("defaults: %+v %s %v", config, origin, err)
	}
	if err := os.WriteFile(memoryConfigPath(dir), []byte(`{"format_version":1,"import_min_interval_seconds":30,"import_on_session_end":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	config, origin, err = loadMemoryConfig(dir)
	if err != nil || origin != memoryConfigPath(dir) || config.ImportMinIntervalSeconds != 30 || config.ImportOnSessionEnd {
		t.Fatalf("file: %+v %s %v", config, origin, err)
	}
	for _, bad := range []string{`{"format_version":2}`, `{"format_version":1,"import_min_interval_seconds":0}`, `{"format_version":1,"unknown":1}`, `{"format_version":1} trailing`} {
		if err := os.WriteFile(memoryConfigPath(dir), []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadMemoryConfig(dir); err == nil {
			t.Fatalf("malformed accepted: %s", bad)
		}
	}
}

// Scheduling: disabled daemons never import; a first trigger runs at once;
// triggers inside the interval coalesce into one pending run; the sweep drains
// the pending run once the interval has passed and otherwise runs nothing.
func TestMemoryImportThrottlesCoalescesAndDrainsOnSweep(t *testing.T) {
	defer swapMemoryConfig(MemoryConfig{FormatVersion: 1, ImportMinIntervalSeconds: 3600, ImportOnSessionEnd: true})()
	var runs atomic.Int32
	importer := &memoryImporter{run: func(reason string) MemoryImportState { runs.Add(1); return MemoryImportState{Reason: reason} }}
	importer.runIfDue("sweep")
	if runs.Load() != 0 {
		t.Fatal("a daemon that does not own the installation must never import")
	}
	importer.enabled = true
	importer.runIfDue("sweep")
	if runs.Load() != 1 || importer.pending {
		t.Fatalf("first sweep imports once: runs=%d pending=%v", runs.Load(), importer.pending)
	}
	importer.runIfDue("session-end")
	importer.runIfDue("session-end")
	if runs.Load() != 1 || !importer.pending {
		t.Fatalf("inside the interval triggers coalesce: runs=%d pending=%v", runs.Load(), importer.pending)
	}
	importer.runIfDue("sweep")
	if runs.Load() != 1 {
		t.Fatal("the sweep must not drain inside the interval")
	}
	importer.lastRun = time.Now().Add(-2 * time.Hour)
	importer.runIfDue("sweep")
	if runs.Load() != 2 || importer.pending {
		t.Fatalf("the sweep drains the pending run after the interval: runs=%d pending=%v", runs.Load(), importer.pending)
	}
	importer.lastRun = time.Now().Add(-2 * time.Hour)
	importer.runIfDue("sweep")
	if runs.Load() != 2 {
		t.Fatal("with nothing pending the sweep imports nothing on its own cadence")
	}
	importer.runIfDue("session-end")
	if runs.Load() != 3 {
		t.Fatal("a session end after the interval imports at once")
	}
}

// The session-end switch is honoured before anything is scheduled.
func TestMemoryImportSessionEndSwitch(t *testing.T) {
	defer swapMemoryConfig(MemoryConfig{FormatVersion: 1, ImportMinIntervalSeconds: 1, ImportOnSessionEnd: false})()
	previous := memoryImport
	var runs atomic.Int32
	memoryImport = &memoryImporter{enabled: true, run: func(reason string) MemoryImportState { runs.Add(1); return MemoryImportState{} }}
	defer func() { memoryImport = previous }()
	requestMemoryImport("session-end")
	time.Sleep(50 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatal("import_on_session_end=false must not schedule an import")
	}
	requestMemoryImport("boot")
	deadline := time.Now().Add(2 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runs.Load() != 1 {
		t.Fatal("other reasons still import")
	}
}

// The real run: a Claude auto-memory topic file lands in the product store,
// only the written records are indexed through the governor's own handle,
// the state file records the run, and a second run imports nothing.
func TestMemoryImportWritesStoreIndexesWrittenRecordsAndRecordsState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	storeDir := filepath.Join(t.TempDir(), "memory")
	t.Setenv("CG_MEMORY_DIR", storeDir)
	dataDir := t.TempDir()
	previousIndexPath := resolvedIndexPath
	setIndexPath(dataDir)
	defer func() { resolvedIndexPath = previousIndexPath }()
	ix, err := store.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	previousGovernor := governor
	governor = NewGovernor(ix, nil)
	defer func() { governor = previousGovernor }()
	writeClaudeTopicFile(t, home, "-Users-x-Documents-Sites-demo-repo", "demo-fact", "We decided the demo repo ships on Fridays.")

	// First run with a governor: written AND indexed (indexed == imported by
	// construction — the write owner maintains entity/classification/FTS in
	// the same transaction; the old no-governor gap cannot exist).
	state := runMemoryImport("test")
	if state.Error != "" || state.Imported != 1 || state.Indexed != 1 || state.Errors != 0 || state.Store != storeDir {
		t.Fatalf("first run: %+v", state)
	}
	records := memory.Load(storeDir)
	if len(records) != 1 || records[0].Repository != "demo-repo" || !strings.Contains(records[0].ID, "demo-fact") {
		t.Fatalf("store: %+v", records)
	}
	saved, found, err := ReadMemoryImportState(dataDir)
	if err != nil || !found || saved.Imported != 1 || saved.Reason != "test" || saved.LastAt == "" {
		t.Fatalf("state file: %+v %v %v", saved, found, err)
	}
	again := runMemoryImport("test")
	if again.Imported != 0 || again.Skipped != 1 || again.Indexed != 0 {
		t.Fatalf("second run must import nothing: %+v", again)
	}
	// With no governor the records are STILL written and indexed: the store
	// write owner needs no governor (the daemon's governor is for events, not
	// memory records — first-class-records plan §3.3).
	governor = nil
	writeClaudeTopicFile(t, home, "-Users-x-Documents-Sites-demo-repo", "second-fact", "Another fact.")
	third := runMemoryImport("test")
	if third.Imported != 1 || third.Indexed != 1 || third.Errors != 0 {
		t.Fatalf("no governor: %+v", third)
	}
}
