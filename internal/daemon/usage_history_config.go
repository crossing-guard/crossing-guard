package daemon

// Usage-history policy (token-usage-analytics plan §3.9): how often the usage
// recorder runs, how many bytes one pass may read, and the response budgets of
// the usage routes. Compiled defaults with an operator override, the
// session-activity.json shape, for THIS feature's values only (ADR 0026). No
// key deletes anything — only `crossing-guard prune --usage` does (plan D-7) —
// so an unusable file falls back to the defaults safely.

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

const usageHistoryFormatVersion = 1

// UsageHistoryConfig is the typed owner of the usage recorder's cadence and
// work bound and of the usage routes' response budgets.
type UsageHistoryConfig struct {
	FormatVersion int                        `json:"format_version"`
	Recorder      UsageHistoryRecorderConfig `json:"recorder"`
	Report        UsageHistoryReportConfig   `json:"report"`
}

// UsageHistoryRecorderConfig bounds the recorder: its interval, and the bytes
// one pass reads (which sets how long the first backfill takes).
type UsageHistoryRecorderConfig struct {
	IntervalSeconds int   `json:"interval_seconds"`
	ReadBudgetBytes int64 `json:"read_budget_bytes"`
}

// UsageHistoryReportConfig bounds the usage routes' responses and the agent
// classification read (session usage breakdown plan §5.5).
type UsageHistoryReportConfig struct {
	MaxGroups   int `json:"max_groups"`
	TopSessions int `json:"top_sessions"`
	// SessionSeriesPoints bounds a session's context series in the Usage pane.
	SessionSeriesPoints int `json:"session_series_points"`
	// SessionAgentTicks bounds each agent's call times in the Usage pane.
	SessionAgentTicks int `json:"session_agent_ticks"`
	// SessionMembersMax bounds a session's subagent and agent lists, largest
	// first; the rest are counted as omitted.
	SessionMembersMax int `json:"session_members_max"`
	// AgentSessionsMax bounds the read of which sessions are Crossing Guard
	// agents; a cut marks the agent split incomplete.
	AgentSessionsMax int `json:"agent_sessions_max"`
}

func defaultUsageHistoryConfig() UsageHistoryConfig {
	return UsageHistoryConfig{
		FormatVersion: usageHistoryFormatVersion,
		Recorder:      UsageHistoryRecorderConfig{IntervalSeconds: 30, ReadBudgetBytes: 256 << 20},
		Report: UsageHistoryReportConfig{MaxGroups: 24, TopSessions: 12, SessionSeriesPoints: 600,
			SessionAgentTicks: 500, SessionMembersMax: 200, AgentSessionsMax: 100000},
	}
}

func usageHistoryConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "usage-history.json")
}

func loadUsageHistoryConfig(dataDir string) (UsageHistoryConfig, string, error) {
	path := usageHistoryConfigPath(dataDir)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultUsageHistoryConfig(), "builtin-default", nil
		}
		return UsageHistoryConfig{}, "", fmt.Errorf("open usage history configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	config := defaultUsageHistoryConfig()
	if err := decoder.Decode(&config); err != nil {
		return UsageHistoryConfig{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return UsageHistoryConfig{}, "", fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := config.validate(); err != nil {
		return UsageHistoryConfig{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	return config, path, nil
}

func (c UsageHistoryConfig) validate() error {
	if c.FormatVersion != usageHistoryFormatVersion {
		return fmt.Errorf("unsupported format_version %d", c.FormatVersion)
	}
	positive := map[string]int64{
		"recorder.interval_seconds":    int64(c.Recorder.IntervalSeconds),
		"recorder.read_budget_bytes":   c.Recorder.ReadBudgetBytes,
		"report.max_groups":            int64(c.Report.MaxGroups),
		"report.top_sessions":          int64(c.Report.TopSessions),
		"report.session_series_points": int64(c.Report.SessionSeriesPoints),
		"report.session_agent_ticks":   int64(c.Report.SessionAgentTicks),
		"report.session_members_max":   int64(c.Report.SessionMembersMax),
		"report.agent_sessions_max":    int64(c.Report.AgentSessionsMax),
	}
	for name, value := range positive {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	// A longer interval would overflow time.Duration and run the recorder back
	// to back (code red-team C-9); a day is already far past any useful cadence.
	if c.Recorder.IntervalSeconds > maxUsageRecorderIntervalSeconds {
		return fmt.Errorf("recorder.interval_seconds must be at most %d", maxUsageRecorderIntervalSeconds)
	}
	// A point is about 30 bytes of JSON and a tick about 15, so these keep one
	// session's Usage pane response under about 600 KB and 300 KB per agent.
	for name, value := range map[string]int{"report.session_series_points": c.Report.SessionSeriesPoints,
		"report.session_agent_ticks": c.Report.SessionAgentTicks} {
		if value > maxUsageSessionSeriesEntries {
			return fmt.Errorf("%s must be at most %d", name, maxUsageSessionSeriesEntries)
		}
	}
	return nil
}

// maxUsageSessionSeriesEntries bounds a session's series and each agent's
// ticks (the response-size reason above).
const maxUsageSessionSeriesEntries = 20000

// maxUsageRecorderIntervalSeconds bounds the interval: one day.
const maxUsageRecorderIntervalSeconds = 24 * 60 * 60

// RecorderInterval is the usage recorder's cadence.
func (c UsageHistoryConfig) RecorderInterval() time.Duration {
	return time.Duration(c.Recorder.IntervalSeconds) * time.Second
}

var (
	usageHistoryConfigOnce  sync.Once
	usageHistoryConfigValue UsageHistoryConfig
)

func usageHistoryConfig() UsageHistoryConfig {
	usageHistoryConfigOnce.Do(func() {
		config, origin, err := loadUsageHistoryConfig(filepath.Dir(indexPath()))
		if err != nil {
			log.Printf("usage history configuration unusable, using defaults: %v", err)
			usageHistoryConfigValue = defaultUsageHistoryConfig()
			return
		}
		log.Printf("usage history configuration: origin=%s interval=%s budget=%d groups=%d top=%d",
			origin, config.RecorderInterval(), config.Recorder.ReadBudgetBytes, config.Report.MaxGroups,
			config.Report.TopSessions)
		usageHistoryConfigValue = config
	})
	return usageHistoryConfigValue
}
