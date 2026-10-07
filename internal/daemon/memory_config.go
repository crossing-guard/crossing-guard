package daemon

// memory.json — the memory owner's configuration (daemon-memory-import plan D4;
// config-ownership plan Fix A). Tunables live here, never in code. Missing
// file = compiled defaults with origin builtin-default; a malformed file is
// logged and the defaults are used, the console_config.go pattern, and
// doctor shows the origin so the fallback is visible.
//
// RT-C1 (config-ownership red-team): the propose CONSENT is LIVE. The cache
// re-reads on mtime change like consoleConfig(), so an owner flipping
// propose.enabled mid-run is obeyed by the next route call — a restart-locked
// consent would be a silent semantic change. The import throttle rides the
// same re-read (one file, one cache); both are cheap reads of a ≤16 KiB file.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	memoryConfigFormatVersion = 1
	memoryConfigReadLimit     = 16 << 10
)

type MemoryConfig struct {
	FormatVersion int `json:"format_version"`
	// ImportMinIntervalSeconds is the least time between two daemon imports of
	// Claude auto-memory; a trigger inside the window marks the import pending
	// and the next lifecycle sweep (compiled 30 s cadence) runs it.
	ImportMinIntervalSeconds int `json:"import_min_interval_seconds"`
	// ImportOnSessionEnd runs an import (subject to the interval) when a
	// session's end row lands — the moment a vendor's auto-memory is settled.
	ImportOnSessionEnd bool `json:"import_on_session_end"`
	// Propose is the agent proposal door's policy (config-ownership plan
	// Fix A): the owner's separate consent — default off; the read tools'
	// registration never turns it on (memory-first-class-records plan §5.2).
	Propose MemoryProposeConfig `json:"propose"`
	// Synthesis is the daemon session-synthesis step's policy (synthesis v1
	// plan): default OFF; when on, an ended session with a memory-shaped
	// event drafts at most one pending lesson candidate. The gate is read at
	// the session-end moment, so a flip applies to the NEXT ended session.
	Synthesis MemorySynthesisConfig `json:"synthesis"`
}

// MemorySynthesisConfig is whether the daemon drafts lesson candidates from
// ended sessions, and how many per UTC day.
type MemorySynthesisConfig struct {
	Enabled   bool `json:"enabled"`
	MaxPerDay int  `json:"max_per_day"`
}

// MemoryProposeConfig is whether agent sessions may propose memories, and
// how fast one session may.
type MemoryProposeConfig struct {
	Enabled       bool `json:"enabled"`
	PerSessionMax int  `json:"per_session_max"`
}

func defaultMemoryConfig() MemoryConfig {
	return MemoryConfig{FormatVersion: memoryConfigFormatVersion, ImportMinIntervalSeconds: 300, ImportOnSessionEnd: true,
		Propose:   MemoryProposeConfig{Enabled: false, PerSessionMax: 5},
		Synthesis: MemorySynthesisConfig{Enabled: false, MaxPerDay: 20}}
}

func memoryConfigPath(dataDir string) string { return filepath.Join(dataDir, "memory.json") }

func loadMemoryConfig(dataDir string) (MemoryConfig, string, error) {
	path := memoryConfigPath(dataDir)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultMemoryConfig(), "builtin-default", nil
		}
		return MemoryConfig{}, "", fmt.Errorf("open memory configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, memoryConfigReadLimit))
	decoder.DisallowUnknownFields()
	config := defaultMemoryConfig()
	if err := decoder.Decode(&config); err != nil {
		return MemoryConfig{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return MemoryConfig{}, "", fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := config.validate(); err != nil {
		return MemoryConfig{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	return config, path, nil
}

func (c MemoryConfig) validate() error {
	if c.FormatVersion != memoryConfigFormatVersion {
		return fmt.Errorf("unsupported format_version %d", c.FormatVersion)
	}
	if c.ImportMinIntervalSeconds <= 0 {
		return errors.New("import_min_interval_seconds must be positive")
	}
	if c.Propose.Enabled && c.Propose.PerSessionMax <= 0 {
		return errors.New("propose.per_session_max must be positive when propose.enabled is on")
	}
	if c.Synthesis.Enabled && c.Synthesis.MaxPerDay <= 0 {
		return errors.New("synthesis.max_per_day must be positive when synthesis.enabled is on")
	}
	return nil
}

// memoryConfigState is the LIVE cache (RT-C1): the value, its origin and the
// file stamp it came from, re-read when the stamp changes — consoleConfig's
// pattern, not the old sync.Once.
type memoryConfigState struct {
	mu      sync.Mutex
	loaded  bool
	value   MemoryConfig
	origin  string
	stamp   time.Time // the file's mtime, the re-read trigger
	checked time.Time
	swapped bool // a test override is in force; file reads wait for the restore
}

var memoryState memoryConfigState

// memoryConfig returns the memory owner's configuration in force and its
// origin, re-reading memory.json when it changed on disk. The propose route
// calls this PER REQUEST so a consent flip is live; the importer may hold a
// resolved value across a run where its own throttle semantics need it.
func memoryConfig() (MemoryConfig, string) {
	dataDir := filepath.Dir(indexPath())
	memoryState.mu.Lock()
	defer memoryState.mu.Unlock()
	if memoryState.swapped {
		return memoryState.value, memoryState.origin
	}
	if info, err := os.Stat(memoryConfigPath(dataDir)); memoryState.loaded && err == nil &&
		memoryState.stamp.Equal(info.ModTime()) {
		return memoryState.value, memoryState.origin
	}
	memoryState.checked = time.Now()
	config, origin, err := loadMemoryConfig(dataDir)
	if err != nil {
		log.Printf("memory configuration unusable, using defaults: %v", err)
		config, origin = defaultMemoryConfig(), "builtin-default"
	} else if !memoryState.loaded {
		log.Printf("memory configuration: origin=%s import_min_interval=%ds import_on_session_end=%v propose_enabled=%v",
			origin, config.ImportMinIntervalSeconds, config.ImportOnSessionEnd, config.Propose.Enabled)
	}
	if info, err := os.Stat(memoryConfigPath(dataDir)); err == nil {
		memoryState.stamp = info.ModTime()
	}
	memoryState.value, memoryState.origin, memoryState.loaded = config, origin, true
	return config, origin
}

// swapMemoryConfig replaces the resolved configuration for a test and returns
// the restore function. The stamp is cleared so the swap wins over an
// unchanged file until the file actually changes.
func swapMemoryConfig(config MemoryConfig) func() {
	memoryState.mu.Lock()
	defer memoryState.mu.Unlock()
	previous, previousOrigin, previousLoaded := memoryState.value, memoryState.origin, memoryState.loaded
	previousStamp := memoryState.stamp
	memoryState.value, memoryState.origin, memoryState.loaded = config, "test", true
	memoryState.stamp = time.Time{}
	memoryState.swapped = true
	return func() {
		memoryState.mu.Lock()
		defer memoryState.mu.Unlock()
		memoryState.value, memoryState.origin, memoryState.loaded = previous, previousOrigin, previousLoaded
		memoryState.stamp = previousStamp
		memoryState.swapped = false
	}
}
