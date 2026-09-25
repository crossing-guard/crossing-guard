package daemon

// Session-stream policy. These are product decisions (how often we look, how
// long silence means "quiet", how much we hold), not mechanism, so they are
// declared here with compiled defaults and overridable from the data
// directory. The shape follows internal/collectionconfig: a typed owner for
// one feature's values, an explicit origin, defaults when the file is absent,
// and a visible error when it is malformed. It is deliberately NOT a generic
// configuration service (ADR 0026), and it does not absorb constants owned by
// other features (the scan-reuse window belongs to the harvest coalescer, the
// approval presence lease to the approval router).

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

const sessionStreamFormatVersion = 1

// SessionStreamConfig owns the tunables of the one session-scoped live stream.
type SessionStreamConfig struct {
	FormatVersion int `json:"format_version"`
	// PollSeconds is how often an open stream re-reads its session. The
	// harvest burst coalescer (1.5s) is the effective floor: polling faster
	// cannot produce fresher data, it only burns work.
	PollSeconds int `json:"poll_seconds"`
	// QuietSeconds bounds every in-flight state: a turn that started, a tool
	// that ran, a question that was asked — with nothing observed after it
	// for this long — stops reading as working and is reported as silence
	// with its duration. Tuned for hook boundaries, not transcript polling:
	// a long tool-less reply is the everyday case, not the exception. The
	// same bound retires a stale in-turn progress word (turn_progress.go):
	// one silence, one number.
	QuietSeconds int `json:"quiet_seconds"`
	// CoalesceMS is how long a burst of facts about one session is held
	// before the status is refolded once.
	CoalesceMS int `json:"coalesce_ms"`
	// LookbackRows bounds how many recent rows of each kind the fold reads.
	LookbackRows int `json:"lookback_rows"`
	// OwnedEndAttributionSeconds is how close to an owned task's completion a
	// session-end row must land to be read as THAT process ending rather than
	// the human's own session. There is no process identity on the hook row,
	// so proximity is the only signal; keep it tight. A human closing their
	// terminal inside this window after a console turn is the false negative.
	OwnedEndAttributionSeconds int `json:"owned_end_attribution_seconds"`
	// ProgressWindowRecords bounds how many of the newest transcript events
	// the turn-progress rules read for the open session.
	ProgressWindowRecords int `json:"progress_window_records"`
	// MinThoughtSeconds is the shortest thought that renders as "thought for
	// N"; shorter ones are omitted rather than shown as 0s.
	MinThoughtSeconds int `json:"min_thought_seconds"`
	// settle_seconds was policy for a derived "waiting" that no longer exists.
	// It is read and ignored with a warning so an operator file that still
	// carries it keeps loading (the file format did not change).
	DeprecatedSettleSeconds int `json:"settle_seconds,omitempty"`
	// SnapshotEvents bounds the newest-window snapshot sent on open.
	SnapshotEvents int `json:"snapshot_events"`
	// KeepaliveSeconds is the SSE comment cadence.
	KeepaliveSeconds int `json:"keepalive_seconds"`
	// MaxStreams bounds concurrent session streams daemon-wide.
	MaxStreams int `json:"max_streams"`
	// GovernanceWindow bounds governed-action deltas per flush.
	GovernanceWindow int `json:"governance_window"`
	// ClientBackoffCapSeconds and ClientAgeTickSeconds are published to the
	// browser so reconnect pacing and the age indicator are not compiled into
	// the client either.
	ClientBackoffCapSeconds int `json:"client_backoff_cap_seconds"`
	ClientAgeTickSeconds    int `json:"client_age_tick_seconds"`
}

// sessionStreamPollFloor is mechanism, not policy: below the harvest burst
// coalescer's reuse window a faster poll cannot observe anything new.
const sessionStreamPollFloor = 2 * time.Second

func defaultSessionStreamConfig() SessionStreamConfig {
	return SessionStreamConfig{
		FormatVersion:              sessionStreamFormatVersion,
		PollSeconds:                2,
		QuietSeconds:               300,
		CoalesceMS:                 250,
		LookbackRows:               32,
		OwnedEndAttributionSeconds: 5,
		ProgressWindowRecords:      64,
		MinThoughtSeconds:          1,
		SnapshotEvents:             400,
		KeepaliveSeconds:           15,
		MaxStreams:                 16,
		GovernanceWindow:           60,
		ClientBackoffCapSeconds:    10,
		ClientAgeTickSeconds:       15,
	}
}

func sessionStreamConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "session-stream.json")
}

// loadSessionStreamConfig reads the operator's values, or returns the compiled
// defaults when no file exists. A malformed file is an error the operator can
// see, never a silent fallback that hides their intent.
func loadSessionStreamConfig(dataDir string) (SessionStreamConfig, string, error) {
	path := sessionStreamConfigPath(dataDir)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultSessionStreamConfig(), "builtin-default", nil
		}
		return SessionStreamConfig{}, "", fmt.Errorf("open session stream configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	config := defaultSessionStreamConfig()
	if err := decoder.Decode(&config); err != nil {
		return SessionStreamConfig{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return SessionStreamConfig{}, "", fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := config.validate(); err != nil {
		return SessionStreamConfig{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	if config.DeprecatedSettleSeconds != 0 {
		log.Printf("session stream configuration: settle_seconds in %s is no longer used and was ignored", path)
		config.DeprecatedSettleSeconds = 0
	}
	return config, path, nil
}

func (c SessionStreamConfig) validate() error {
	if c.FormatVersion != sessionStreamFormatVersion {
		return fmt.Errorf("unsupported format_version %d", c.FormatVersion)
	}
	positive := map[string]int{
		"poll_seconds":                  c.PollSeconds,
		"quiet_seconds":                 c.QuietSeconds,
		"coalesce_ms":                   c.CoalesceMS,
		"lookback_rows":                 c.LookbackRows,
		"owned_end_attribution_seconds": c.OwnedEndAttributionSeconds,
		"progress_window_records":       c.ProgressWindowRecords,
		"min_thought_seconds":           c.MinThoughtSeconds,
		"snapshot_events":               c.SnapshotEvents,
		"keepalive_seconds":             c.KeepaliveSeconds,
		"max_streams":                   c.MaxStreams,
		"governance_window":             c.GovernanceWindow,
		"client_backoff_cap_seconds":    c.ClientBackoffCapSeconds,
		"client_age_tick_seconds":       c.ClientAgeTickSeconds,
	}
	for name, value := range positive {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	return nil
}

// Poll returns the effective poll interval, never faster than the floor.
func (c SessionStreamConfig) Poll() time.Duration {
	interval := time.Duration(c.PollSeconds) * time.Second
	if interval < sessionStreamPollFloor {
		return sessionStreamPollFloor
	}
	return interval
}

// Quiet is the silence bound past which an in-flight state reads as unknown.
func (c SessionStreamConfig) Quiet() time.Duration {
	return time.Duration(c.QuietSeconds) * time.Second
}

// Keepalive is the SSE comment cadence of the session stream.
func (c SessionStreamConfig) Keepalive() time.Duration {
	return time.Duration(c.KeepaliveSeconds) * time.Second
}

// MinThought is the shortest thought duration that renders.
func (c SessionStreamConfig) MinThought() time.Duration {
	return time.Duration(c.MinThoughtSeconds) * time.Second
}

// OwnedEndAttribution is the window inside which a session end is read as an
// owned task's own process ending.
func (c SessionStreamConfig) OwnedEndAttribution() time.Duration {
	return time.Duration(c.OwnedEndAttributionSeconds) * time.Second
}

var (
	sessionStreamConfigOnce  sync.Once
	sessionStreamConfigValue SessionStreamConfig
)

// sessionStreamConfig resolves once per process. A configuration error is
// reported and the defaults are used, so a typo cannot take the console's live
// view offline — the log line is the operator's signal.
func sessionStreamConfig() SessionStreamConfig {
	sessionStreamConfigOnce.Do(func() {
		config, origin, err := loadSessionStreamConfig(filepath.Dir(indexPath()))
		if err != nil {
			log.Printf("session stream configuration unusable, using defaults: %v", err)
			sessionStreamConfigValue = defaultSessionStreamConfig()
			return
		}
		// Say where the values came from: an operator who edited the file must
		// be able to confirm the daemon actually read it.
		log.Printf("session stream configuration: origin=%s poll=%s quiet=%s coalesce=%dms",
			origin, config.Poll(), config.Quiet(), config.CoalesceMS)
		sessionStreamConfigValue = config
	})
	return sessionStreamConfigValue
}
