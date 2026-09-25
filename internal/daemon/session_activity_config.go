package daemon

// Session-activity policy: how often the presence sampler looks, how long an
// observation stays fresh, how old a hook signal may be before a session reads
// recent/stale/absent, and how long turn-boundary rows are kept. These are
// product decisions, not mechanism, so they live here with compiled defaults
// and an operator override — the collectionconfig shape, for THIS feature's
// values only. It is not a generic configuration service (ADR 0026).

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

const sessionActivityFormatVersion = 1

type SessionActivityConfig struct {
	FormatVersion int `json:"format_version"`
	// The presence sampler's cadence, per-pass deadline, and how long one
	// observation is trusted before it expires.
	SamplerIntervalSeconds int `json:"sampler_interval_seconds"`
	SamplerTimeoutSeconds  int `json:"sampler_timeout_seconds"`
	SamplerTTLSeconds      int `json:"sampler_ttl_seconds"`
	// Hook-liveness grading by governance-event age: live within Live,
	// recent within Recent, stale beyond, and absent past Window. Horizon
	// bounds the newest-lifecycle-row scan.
	LivenessLiveSeconds    int `json:"liveness_live_seconds"`
	LivenessRecentSeconds  int `json:"liveness_recent_seconds"`
	LivenessWindowSeconds  int `json:"liveness_window_seconds"`
	LivenessHorizonSeconds int `json:"liveness_horizon_seconds"`
	// RailKeepaliveSeconds is the activity stream's SSE comment cadence.
	RailKeepaliveSeconds int `json:"rail_keepalive_seconds"`
	// TurnRetentionDays bounds the turn-boundary table.
	TurnRetentionDays int `json:"turn_retention_days"`
	// MaxRailSessions bounds how many sessions each presence lane may surface
	// per pass.
	MaxRailSessions int `json:"max_rail_sessions"`
}

func defaultSessionActivityConfig() SessionActivityConfig {
	return SessionActivityConfig{
		FormatVersion:          sessionActivityFormatVersion,
		SamplerIntervalSeconds: 30,
		SamplerTimeoutSeconds:  2,
		SamplerTTLSeconds:      45,
		LivenessLiveSeconds:    120,
		LivenessRecentSeconds:  900,
		LivenessWindowSeconds:  3600,
		LivenessHorizonSeconds: 7 * 24 * 3600,
		RailKeepaliveSeconds:   15,
		TurnRetentionDays:      30,
		MaxRailSessions:        200,
	}
}

func sessionActivityConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "session-activity.json")
}

func loadSessionActivityConfig(dataDir string) (SessionActivityConfig, string, error) {
	path := sessionActivityConfigPath(dataDir)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultSessionActivityConfig(), "builtin-default", nil
		}
		return SessionActivityConfig{}, "", fmt.Errorf("open session activity configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	config := defaultSessionActivityConfig()
	if err := decoder.Decode(&config); err != nil {
		return SessionActivityConfig{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return SessionActivityConfig{}, "", fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := config.validate(); err != nil {
		return SessionActivityConfig{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	return config, path, nil
}

func (c SessionActivityConfig) validate() error {
	if c.FormatVersion != sessionActivityFormatVersion {
		return fmt.Errorf("unsupported format_version %d", c.FormatVersion)
	}
	positive := map[string]int{
		"sampler_interval_seconds": c.SamplerIntervalSeconds,
		"sampler_timeout_seconds":  c.SamplerTimeoutSeconds,
		"sampler_ttl_seconds":      c.SamplerTTLSeconds,
		"liveness_live_seconds":    c.LivenessLiveSeconds,
		"liveness_recent_seconds":  c.LivenessRecentSeconds,
		"liveness_window_seconds":  c.LivenessWindowSeconds,
		"liveness_horizon_seconds": c.LivenessHorizonSeconds,
		"rail_keepalive_seconds":   c.RailKeepaliveSeconds,
		"turn_retention_days":      c.TurnRetentionDays,
		"max_rail_sessions":        c.MaxRailSessions,
	}
	for name, value := range positive {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	// The liveness grades are read as an ordered ladder; an inverted ladder
	// would make a grade unreachable rather than merely odd.
	if !(c.LivenessLiveSeconds < c.LivenessRecentSeconds && c.LivenessRecentSeconds < c.LivenessWindowSeconds &&
		c.LivenessWindowSeconds < c.LivenessHorizonSeconds) {
		return errors.New("liveness windows must increase: live < recent < window < horizon")
	}
	return nil
}

// SamplerInterval is the presence sampler's cadence.
func (c SessionActivityConfig) SamplerInterval() time.Duration {
	return time.Duration(c.SamplerIntervalSeconds) * time.Second
}

// SamplerTimeout bounds one sampler pass.
func (c SessionActivityConfig) SamplerTimeout() time.Duration {
	return time.Duration(c.SamplerTimeoutSeconds) * time.Second
}

// SamplerTTL is how long one presence observation is trusted.
func (c SessionActivityConfig) SamplerTTL() time.Duration {
	return time.Duration(c.SamplerTTLSeconds) * time.Second
}

// LivenessLive is the hook-liveness age that still reads live.
func (c SessionActivityConfig) LivenessLive() time.Duration {
	return time.Duration(c.LivenessLiveSeconds) * time.Second
}

// LivenessRecent is the hook-liveness age that still reads recent.
func (c SessionActivityConfig) LivenessRecent() time.Duration {
	return time.Duration(c.LivenessRecentSeconds) * time.Second
}

// LivenessWindow is the hook-liveness age past which a session is absent.
func (c SessionActivityConfig) LivenessWindow() time.Duration {
	return time.Duration(c.LivenessWindowSeconds) * time.Second
}

// LivenessHorizon bounds the newest-lifecycle-row scan.
func (c SessionActivityConfig) LivenessHorizon() time.Duration {
	return time.Duration(c.LivenessHorizonSeconds) * time.Second
}

// RailKeepalive is the activity stream's SSE comment cadence.
func (c SessionActivityConfig) RailKeepalive() time.Duration {
	return time.Duration(c.RailKeepaliveSeconds) * time.Second
}

// TurnRetention is how long turn-boundary rows are kept.
func (c SessionActivityConfig) TurnRetention() time.Duration {
	return time.Duration(c.TurnRetentionDays) * 24 * time.Hour
}

// SessionActivityConfig is the typed owner of presence and turn-retention
// policy; see the package comment at the top of this file.

var (
	sessionActivityConfigOnce  sync.Once
	sessionActivityConfigValue SessionActivityConfig
)

func sessionActivityConfig() SessionActivityConfig {
	sessionActivityConfigOnce.Do(func() {
		config, origin, err := loadSessionActivityConfig(filepath.Dir(indexPath()))
		if err != nil {
			log.Printf("session activity configuration unusable, using defaults: %v", err)
			sessionActivityConfigValue = defaultSessionActivityConfig()
			return
		}
		log.Printf("session activity configuration: origin=%s sampler=%s liveness=%s/%s/%s retention=%dd",
			origin, config.SamplerInterval(), config.LivenessLive(), config.LivenessRecent(),
			config.LivenessWindow(), config.TurnRetentionDays)
		sessionActivityConfigValue = config
	})
	return sessionActivityConfigValue
}
