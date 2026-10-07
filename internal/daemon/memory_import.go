package daemon

// Daemon-owned memory import (daemon-memory-import plan). Claude auto-memory
// topic files land in the product store from the daemon's own loop: on every
// session end row (closure), and on the lifecycle sweep as the safety net —
// never from a vendor hook someone attached by hand. One import at a time;
// triggers inside the configured interval coalesce into one pending run that
// the next sweep drains. Records the run wrote are indexed into the governed
// entity/FTS model through the governor's own store handle under its write
// lock — never a second store writer. A daemon that does not own the
// installation (--no-hook-install: scratch daemons) never writes the store.

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"crossing-guard/internal/memcli"
	"crossing-guard/memory"
)

// MemoryImportState is the daemon's record of its last import, written to
// <dataDir>/memory-import-state.json after every run (outside the memory
// directory, whose own versioning would otherwise sweep it up) and read by
// doctor.
type MemoryImportState struct {
	LastAt     string `json:"last_at"`
	Reason     string `json:"reason"`
	Store      string `json:"store"`
	Projects   int    `json:"projects"`
	Files      int    `json:"files"`
	Imported   int    `json:"imported"`
	Skipped    int    `json:"skipped"`
	Tombstoned int    `json:"tombstoned"`
	Indexed    int    `json:"indexed"`
	Errors     int    `json:"errors"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type memoryImporter struct {
	mu      sync.Mutex
	enabled bool
	running bool
	pending bool
	lastRun time.Time
	// run is the import itself; a test swaps it to observe scheduling.
	run func(reason string) MemoryImportState
}

var memoryImport = &memoryImporter{run: runMemoryImport}

// enableDaemonMemoryImport is the ownership switch: only a daemon that owns
// the installation (service + hooks) imports into the installed store.
func enableDaemonMemoryImport(enabled bool) {
	memoryImport.mu.Lock()
	memoryImport.enabled = enabled
	memoryImport.mu.Unlock()
}

// requestMemoryImport is the session-end trigger (plan D1b): honours the
// import_on_session_end switch and never blocks the caller.
func requestMemoryImport(reason string) {
	config, _ := memoryConfig()
	if reason == "session-end" && !config.ImportOnSessionEnd {
		return
	}
	go memoryImport.runIfDue(reason)
}

// runMemoryImportIfDue is the sweep trigger (plan D1c) and the pending drain.
func runMemoryImportIfDue(reason string) { memoryImport.runIfDue(reason) }

func (m *memoryImporter) runIfDue(reason string) {
	config, _ := memoryConfig()
	interval := time.Duration(config.ImportMinIntervalSeconds) * time.Second
	m.mu.Lock()
	if !m.enabled {
		m.mu.Unlock()
		return
	}
	if m.running {
		m.pending = true
		m.mu.Unlock()
		return
	}
	if !m.lastRun.IsZero() && time.Since(m.lastRun) < interval {
		m.pending = true
		m.mu.Unlock()
		return
	}
	if reason == "sweep" && !m.pending && !m.lastRun.IsZero() {
		// The sweep only drains a pending trigger or runs the first import;
		// it does not import on its own cadence.
		m.mu.Unlock()
		return
	}
	m.running, m.pending = true, false
	m.mu.Unlock()
	state := m.run(reason)
	m.mu.Lock()
	m.running, m.lastRun = false, time.Now()
	m.mu.Unlock()
	if state.Imported > 0 || state.Errors > 0 || state.Error != "" {
		log.Printf("memory import (%s): %d imported, %d skipped, %d tombstoned, %d indexed, %d errors%s",
			state.Reason, state.Imported, state.Skipped, state.Tombstoned, state.Indexed, state.Errors, errorSuffix(state.Error))
	}
}

func errorSuffix(text string) string {
	if text == "" {
		return ""
	}
	return " — " + text
}

// runMemoryImport performs one import into the product STORE through the one
// write owner (first-class-records plan §3.3: record + entity + classification +
// FTS + outbox in one transaction; the separate indexWrittenMemory step and its
// no-governor drift window are gone). The store root is memory.DefaultDir()
// for the MIRROR; the records land in index.sqlite, the location every read
// surface reads.
func runMemoryImport(reason string) MemoryImportState {
	started := time.Now()
	dir := memory.DefaultDir()
	state := MemoryImportState{LastAt: started.UTC().Format(time.RFC3339), Reason: reason, Store: dir}
	stats, written, err := memcli.ImportAutoMemory(dir, "", false)
	state.Projects, state.Files, state.Imported, state.Skipped, state.Tombstoned = stats.Projects, stats.Files, stats.Imported, stats.Skipped, stats.Tombstoned
	if err != nil {
		state.Error = err.Error()
	}
	// Every written record now lands through the write owner, which already
	// maintains the entity/classification/FTS rows transactionally; the
	// imported count is the indexed count by construction.
	state.Indexed = len(written)
	if len(written) > 0 {
		state.Errors = 0
	}
	state.DurationMS = time.Since(started).Milliseconds()
	if err := writeMemoryImportState(filepath.Dir(indexPath()), state); err != nil {
		log.Printf("memory import state could not be written: %v", err)
	}
	return state
}

func memoryImportStatePath(dataDir string) string {
	return filepath.Join(dataDir, "memory-import-state.json")
}

func writeMemoryImportState(dataDir string, state MemoryImportState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := memoryImportStatePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, memoryImportStatePath(dataDir))
}

// ReadMemoryImportState reads the daemon's last import record for doctor.
// found=false means the daemon has not imported since the file's introduction.
func ReadMemoryImportState(dataDir string) (MemoryImportState, bool, error) {
	body, err := os.ReadFile(memoryImportStatePath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return MemoryImportState{}, false, nil
		}
		return MemoryImportState{}, false, err
	}
	var state MemoryImportState
	if err := json.Unmarshal(body, &state); err != nil {
		return MemoryImportState{}, false, err
	}
	return state, true, nil
}
