package daemon

// The approvals inbox's operator-owned tunables. Ceilings and windows are
// configuration, never compiled decisions: an operator who needs a longer runtime
// deadline or a larger question edits this document rather than rebuilding.
//
// Ceilings here are enforced DAEMON-SIDE ONLY. The runtime bridge is a separate
// short-lived process launched from the daemon's own executable path with a data-dir
// rendezvous; it never reads this file. It enforces only its own hard message bounds,
// and learns what the inbox actually carried from the wait result's completeness.

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

	"crossing-guard/internal/approvalchoice"
)

const approvalsFormatVersion = 1

// ApprovalsConfig is the typed document. Every field has a shipped default and a
// bound; an out-of-bounds value is a load error, not a silent clamp.
type ApprovalsConfig struct {
	FormatVersion int `json:"format_version"`
	// Choice-prompt ceilings. Prompts beyond any of these are dropped and the
	// approval is marked truncated — a question never becomes a denial.
	MaxPromptsPerApproval int `json:"max_prompts_per_approval"`
	MaxOptionsPerPrompt   int `json:"max_options_per_prompt"`
	MaxPromptTextBytes    int `json:"max_prompt_text_bytes"`
	MaxFreeTextBytes      int `json:"max_free_text_bytes"`
	MaxPromptsWireBytes   int `json:"max_prompts_wire_bytes"`
	// RuntimeToolTimeoutSeconds is the fallback hold budget for a runtime
	// callback that names none of its own.
	RuntimeToolTimeoutSeconds int `json:"runtime_tool_timeout_seconds"`
	// MaxSummaryBytes bounds the provider-owned display summary and command.
	MaxSummaryBytes int `json:"max_summary_bytes"`
	// HistoryCap bounds the in-memory resolved-approval ring the console reads.
	// Older decisions live in the durable decisions log, not here.
	HistoryCap int `json:"history_cap"`
}

func defaultApprovalsConfig() ApprovalsConfig {
	return ApprovalsConfig{
		FormatVersion:             approvalsFormatVersion,
		MaxPromptsPerApproval:     4,
		MaxOptionsPerPrompt:       4,
		MaxPromptTextBytes:        2048,
		MaxFreeTextBytes:          2048,
		MaxPromptsWireBytes:       16384,
		RuntimeToolTimeoutSeconds: 300,
		MaxSummaryBytes:           8192,
		HistoryCap:                100,
	}
}

// choiceLimits projects the document onto the shared validator's input, so the
// wire owner's rules and the operator's ceilings cannot drift apart.
func (c ApprovalsConfig) choiceLimits() approvalchoice.ChoiceLimits {
	return approvalchoice.ChoiceLimits{
		MaxPrompts:       c.MaxPromptsPerApproval,
		MaxOptions:       c.MaxOptionsPerPrompt,
		MaxTextBytes:     c.MaxPromptTextBytes,
		MaxFreeTextBytes: c.MaxFreeTextBytes,
		MaxWireBytes:     c.MaxPromptsWireBytes,
	}
}

func (c ApprovalsConfig) runtimeToolTimeout() time.Duration {
	return time.Duration(c.RuntimeToolTimeoutSeconds) * time.Second
}

func approvalsConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "approvals.json")
}

// loadApprovalsConfig reads the document if it exists. A missing document is the
// shipped default and not an error; a malformed or out-of-bounds document is an
// error, because silently running on defaults would hide an operator's intent.
func loadApprovalsConfig(dataDir string) (ApprovalsConfig, string, error) {
	path := approvalsConfigPath(dataDir)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultApprovalsConfig(), "builtin-default", nil
		}
		return ApprovalsConfig{}, "", fmt.Errorf("open approvals configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	config := defaultApprovalsConfig()
	if err := decoder.Decode(&config); err != nil {
		return ApprovalsConfig{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ApprovalsConfig{}, "", fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := config.validate(); err != nil {
		return ApprovalsConfig{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	return config, path, nil
}

func (c ApprovalsConfig) validate() error {
	if c.FormatVersion != approvalsFormatVersion {
		return fmt.Errorf("approvals config format_version must be %d", approvalsFormatVersion)
	}
	for _, bound := range []struct {
		field    string
		value    int
		min, max int
	}{
		{"max_prompts_per_approval", c.MaxPromptsPerApproval, 1, 8},
		{"max_options_per_prompt", c.MaxOptionsPerPrompt, 2, 8},
		{"max_prompt_text_bytes", c.MaxPromptTextBytes, 64, 8192},
		{"max_free_text_bytes", c.MaxFreeTextBytes, 0, 8192},
		{"max_prompts_wire_bytes", c.MaxPromptsWireBytes, 1024, 32768},
		{"runtime_tool_timeout_seconds", c.RuntimeToolTimeoutSeconds, 30, 600},
		{"max_summary_bytes", c.MaxSummaryBytes, 1024, 32768},
		{"history_cap", c.HistoryCap, 10, 1000},
	} {
		if bound.value < bound.min || bound.value > bound.max {
			return fmt.Errorf("approvals config %s must be between %d and %d",
				bound.field, bound.min, bound.max)
		}
	}
	return nil
}

var (
	approvalsConfigOnce  sync.Once
	approvalsConfigValue = defaultApprovalsConfig()
)

// activeApprovalsConfig resolves the document once per process against the same data
// root the index lives under, mirroring how the session-activity document is wired.
// A load failure keeps the shipped defaults and is reported once on stderr, because
// the inbox must keep holding decisions even when its tunables are unreadable.
func activeApprovalsConfig() ApprovalsConfig {
	approvalsConfigOnce.Do(func() {
		config, origin, err := loadApprovalsConfig(filepath.Dir(indexPath()))
		if err != nil {
			log.Printf("approvals configuration unusable, using defaults: %v", err)
			return
		}
		log.Printf("approvals configuration: origin=%s prompts=%d/%d hold=%s history=%d",
			origin, config.MaxPromptsPerApproval, config.MaxOptionsPerPrompt,
			config.runtimeToolTimeout(), config.HistoryCap)
		approvalsConfigValue = config
	})
	return approvalsConfigValue
}
