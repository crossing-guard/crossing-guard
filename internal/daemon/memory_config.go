package daemon

// memory.json — the daemon-owned memory import's configuration (daemon-memory-import
// plan D4). Tunables live here, never in code. Missing file = compiled defaults with
// origin builtin-default; a malformed file is logged and the defaults are used, the
// console_config.go pattern, and doctor shows the origin so the fallback is visible.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
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
}

func defaultMemoryConfig() MemoryConfig {
	return MemoryConfig{FormatVersion: memoryConfigFormatVersion, ImportMinIntervalSeconds: 300, ImportOnSessionEnd: true}
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
	return nil
}

var (
	memoryConfigOnce   sync.Once
	memoryConfigValue  MemoryConfig
	memoryConfigOrigin string
)

// memoryConfig resolves once per process, like the other typed config owners.
func memoryConfig() (MemoryConfig, string) {
	memoryConfigOnce.Do(func() {
		config, origin, err := loadMemoryConfig(filepath.Dir(indexPath()))
		if err != nil {
			log.Printf("memory configuration unusable, using defaults: %v", err)
			config, origin = defaultMemoryConfig(), "builtin-default"
		}
		log.Printf("memory configuration: origin=%s import_min_interval=%ds import_on_session_end=%v",
			origin, config.ImportMinIntervalSeconds, config.ImportOnSessionEnd)
		memoryConfigValue, memoryConfigOrigin = config, origin
	})
	return memoryConfigValue, memoryConfigOrigin
}

// swapMemoryConfig replaces the resolved configuration for a test and returns
// the restore function.
func swapMemoryConfig(config MemoryConfig) func() {
	memoryConfigOnce.Do(func() {})
	previous, previousOrigin := memoryConfigValue, memoryConfigOrigin
	memoryConfigValue, memoryConfigOrigin = config, "test"
	return func() { memoryConfigValue, memoryConfigOrigin = previous, previousOrigin }
}
