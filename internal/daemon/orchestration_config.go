package daemon

// Orchestration policy (helper-session-attachment plan D8): how fast natural
// hook rows reach the agent matcher, how long a pending helper message waits
// for its session's next boundary, how much transcript a helper reads, and the
// per-runtime delivery options each adapter interprets for itself. These are
// product decisions, not mechanism, so they carry compiled defaults and are
// overridable from the data directory — the same shape as
// session_stream_config.go: one typed owner, an explicit origin, defaults when
// the file is absent, a visible error when it is malformed. Not a generic
// configuration service (ADR 0026).

import (
	"crossing-guard/internal/observation"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const orchestrationConfigFormatVersion = 1

// OrchestrationConfig owns the orchestration tunables.
type OrchestrationConfig struct {
	FormatVersion int                        `json:"format_version"`
	NaturalSignal NaturalSignalConfig        `json:"natural_signal"`
	Context       OrchestrationContextConfig `json:"context"`
	Delivery      DeliveryConfig             `json:"delivery"`
	HelperSession HelperSessionConfig        `json:"helper_session"`
}

// HelperSessionConfig paces the helper-session sweep (helper-persistent-
// session plan D4): how many wedged runs, lost launches, and pending groups
// one 30 s lifecycle pass reconciles and drains.
type HelperSessionConfig struct {
	SweepBatch int `json:"sweep_batch"`
}

// NaturalSignalConfig paces the hook-row → signal emitter.
type NaturalSignalConfig struct {
	// SweepSeconds is the emitter's own safety-sweep cadence.
	SweepSeconds int `json:"sweep_seconds"`
	// CoalesceMS is how long an ingest nudge is held so a burst of hook rows
	// produces one translation pass.
	CoalesceMS int `json:"coalesce_ms"`
	// TaskSettleMS is the launch window inside which a row for a session the
	// daemon may have just launched is left unrouted, so the task stream — not
	// the hook rows — owns that session's facts (red-team M6).
	TaskSettleMS int `json:"task_settle_ms"`
}

// OrchestrationContextConfig bounds helper context reads.
type OrchestrationContextConfig struct {
	// TranscriptTailEvents bounds how many of the newest transcript events the
	// session.messages provider supplies before the byte limit applies.
	TranscriptTailEvents int `json:"transcript_tail_events"`
}

// DeliveryConfig owns pending helper message policy.
type DeliveryConfig struct {
	// TTLSeconds is how long a pending message waits for a boundary.
	TTLSeconds int `json:"ttl_seconds"`
	// MaxPendingPerSession caps pending records per target session.
	MaxPendingPerSession int `json:"max_pending_per_session"`
	// CarrierKinds are OUR observation kinds whose receipts may carry a pending
	// message back to the session's hook (never a vendor event name).
	CarrierKinds []string `json:"carrier_kinds"`
	// AdapterTimeoutSeconds bounds one adapter delivery call.
	AdapterTimeoutSeconds int `json:"adapter_timeout_seconds"`
	// ClaimBytes bounds how many message bytes one boundary drains; records
	// past the budget stay pending for the next boundary instead of being
	// marked delivered and then cut by the vendor's cap. Keep it at or under
	// the smallest installer encoder cap.
	ClaimBytes int `json:"claim_bytes"`
	// ClaimMarginMS is how much of the hook's stated deadline must remain for
	// a boundary to claim pending messages at all: less than this and the
	// reply may arrive after the hook has given up, so the messages stay
	// pending for the next boundary (delivery-claim-on-reply plan D3).
	ClaimMarginMS int `json:"claim_margin_ms"`
	// RuntimeOptions is opaque per-runtime JSON keyed by registry name, handed
	// to the registered driver's optional chatDeliveryConfigurer port. The
	// generic loader never reads inside a blob.
	RuntimeOptions map[string]json.RawMessage `json:"runtime_options,omitempty"`
}

func defaultOrchestrationConfig() OrchestrationConfig {
	return OrchestrationConfig{
		FormatVersion: orchestrationConfigFormatVersion,
		NaturalSignal: NaturalSignalConfig{SweepSeconds: 30, CoalesceMS: 500, TaskSettleMS: 2000},
		Context:       OrchestrationContextConfig{TranscriptTailEvents: 200},
		Delivery: DeliveryConfig{TTLSeconds: 1800, MaxPendingPerSession: 3,
			CarrierKinds: []string{"turn.started", "tool.started", "tool.completed"}, AdapterTimeoutSeconds: 15,
			ClaimBytes: 7000, ClaimMarginMS: 300},
		HelperSession: HelperSessionConfig{SweepBatch: 50},
	}
}

func orchestrationConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "orchestration.json")
}

// loadOrchestrationConfig reads the operator's values, or returns the compiled
// defaults when no file exists. A malformed file is an error the operator can
// see, never a silent fallback that hides their intent.
func loadOrchestrationConfig(dataDir string) (OrchestrationConfig, string, error) {
	path := orchestrationConfigPath(dataDir)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultOrchestrationConfig(), "builtin-default", nil
		}
		return OrchestrationConfig{}, "", fmt.Errorf("open orchestration configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	config := defaultOrchestrationConfig()
	if err := decoder.Decode(&config); err != nil {
		return OrchestrationConfig{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return OrchestrationConfig{}, "", fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := config.validate(); err != nil {
		return OrchestrationConfig{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	return config, path, nil
}

func (c OrchestrationConfig) validate() error {
	if c.FormatVersion != orchestrationConfigFormatVersion {
		return fmt.Errorf("unsupported format_version %d", c.FormatVersion)
	}
	positive := map[string]int{
		"natural_signal.sweep_seconds":     c.NaturalSignal.SweepSeconds,
		"natural_signal.coalesce_ms":       c.NaturalSignal.CoalesceMS,
		"natural_signal.task_settle_ms":    c.NaturalSignal.TaskSettleMS,
		"context.transcript_tail_events":   c.Context.TranscriptTailEvents,
		"delivery.ttl_seconds":             c.Delivery.TTLSeconds,
		"delivery.max_pending_per_session": c.Delivery.MaxPendingPerSession,
		"delivery.adapter_timeout_seconds": c.Delivery.AdapterTimeoutSeconds,
		"delivery.claim_bytes":             c.Delivery.ClaimBytes,
		"delivery.claim_margin_ms":         c.Delivery.ClaimMarginMS,
		"helper_session.sweep_batch":       c.HelperSession.SweepBatch,
	}
	names := make([]string, 0, len(positive))
	for name := range positive {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if positive[name] <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.ClaimMargin() >= observation.HookDeliveryBudget {
		// A margin at or above the hook's whole budget would silently turn
		// off claiming for every hook that states a deadline (review F4).
		return fmt.Errorf("delivery.claim_margin_ms must be less than the hook delivery budget (%d ms)", observation.HookDeliveryBudget.Milliseconds())
	}
	if len(c.Delivery.CarrierKinds) == 0 {
		return errors.New("delivery.carrier_kinds must name at least one observation kind")
	}
	for _, kind := range c.Delivery.CarrierKinds {
		if kind == "" || kind == "turn.ended" {
			return fmt.Errorf("delivery.carrier_kinds may not include %q", kind)
		}
	}
	return nil
}

// IsCarrierKind reports whether a receipt for this observation kind may carry
// pending deliveries.
// ClaimMargin is the least remaining hook deadline a claim requires.
func (c OrchestrationConfig) ClaimMargin() time.Duration {
	return time.Duration(c.Delivery.ClaimMarginMS) * time.Millisecond
}

func (c OrchestrationConfig) IsCarrierKind(kind string) bool {
	for _, carrier := range c.Delivery.CarrierKinds {
		if carrier == kind {
			return true
		}
	}
	return false
}

// Sweep is the natural-signal emitter's safety-sweep cadence.
func (c OrchestrationConfig) Sweep() time.Duration {
	return time.Duration(c.NaturalSignal.SweepSeconds) * time.Second
}

// Coalesce is how long an ingest nudge is held before one emitter pass.
func (c OrchestrationConfig) Coalesce() time.Duration {
	return time.Duration(c.NaturalSignal.CoalesceMS) * time.Millisecond
}

// TaskSettle is the launch window a fresh hook row waits before it is routed
// as natural.
func (c OrchestrationConfig) TaskSettle() time.Duration {
	return time.Duration(c.NaturalSignal.TaskSettleMS) * time.Millisecond
}

// TTL is how long a pending helper message waits for a carrier boundary.
func (c OrchestrationConfig) TTL() time.Duration {
	return time.Duration(c.Delivery.TTLSeconds) * time.Second
}

// AdapterTimeout bounds one adapter delivery call.
func (c OrchestrationConfig) AdapterTimeout() time.Duration {
	return time.Duration(c.Delivery.AdapterTimeoutSeconds) * time.Second
}

// chatDeliveryConfigurer is an optional port on the registered runtime driver:
// the adapter validates and keeps its own delivery options. The generic loader
// dispatches by registry name and never reads a vendor key.
type chatDeliveryConfigurer interface {
	ConfigureDelivery(raw json.RawMessage) error
}

// applyOrchestrationRuntimeOptions hands each runtime_options blob to its
// registered driver. Every name is checked for a registered driver with an
// options port BEFORE any blob is applied, so a typo in one name applies
// nothing; a blob its own adapter rejects is reported by name and the blobs
// before it stay applied (adapters validate, they do not roll back).
func applyOrchestrationRuntimeOptions(config OrchestrationConfig) error {
	names := make([]string, 0, len(config.Delivery.RuntimeOptions))
	for name := range config.Delivery.RuntimeOptions {
		names = append(names, name)
	}
	sort.Strings(names)
	configurers := make([]chatDeliveryConfigurer, 0, len(names))
	for _, name := range names {
		driver, ok := chatDrivers[name]
		if !ok {
			return fmt.Errorf("delivery.runtime_options names unregistered runtime %q; no runtime options were applied", name)
		}
		configurer, ok := driver.(chatDeliveryConfigurer)
		if !ok {
			return fmt.Errorf("runtime %q publishes no delivery options; no runtime options were applied", name)
		}
		configurers = append(configurers, configurer)
	}
	for index, name := range names {
		if err := configurers[index].ConfigureDelivery(config.Delivery.RuntimeOptions[name]); err != nil {
			return fmt.Errorf("delivery.runtime_options.%s: %w (options for runtimes named before it were applied)", name, err)
		}
	}
	return nil
}

var (
	orchestrationConfigOnce  sync.Once
	orchestrationConfigValue OrchestrationConfig
	orchestrationConfigMu    sync.RWMutex
)

// orchestrationConfig resolves once per process. A configuration error is
// reported and the defaults are used, so a typo cannot take the agents host
// offline — the log line is the operator's signal.
func orchestrationConfig() OrchestrationConfig {
	orchestrationConfigOnce.Do(func() {
		config, origin, err := loadOrchestrationConfig(filepath.Dir(indexPath()))
		if err != nil {
			log.Printf("orchestration configuration unusable, using defaults: %v", err)
			config = defaultOrchestrationConfig()
			origin = "builtin-default-after-error"
		}
		log.Printf("orchestration configuration: origin=%s sweep=%s coalesce=%s settle=%s ttl=%s pending_cap=%d carriers=%v claim_margin=%s",
			origin, config.Sweep(), config.Coalesce(), config.TaskSettle(), config.TTL(),
			config.Delivery.MaxPendingPerSession, config.Delivery.CarrierKinds, config.ClaimMargin())
		if err := applyOrchestrationRuntimeOptions(config); err != nil {
			log.Printf("orchestration configuration: %v", err)
		}
		orchestrationConfigMu.Lock()
		orchestrationConfigValue = config
		orchestrationConfigMu.Unlock()
	})
	orchestrationConfigMu.RLock()
	defer orchestrationConfigMu.RUnlock()
	return orchestrationConfigValue
}

// swapOrchestrationConfig substitutes the resolved configuration for tests and
// returns the restore function.
func swapOrchestrationConfig(config OrchestrationConfig) func() {
	orchestrationConfigOnce.Do(func() {
		orchestrationConfigMu.Lock()
		orchestrationConfigValue = defaultOrchestrationConfig()
		orchestrationConfigMu.Unlock()
	})
	orchestrationConfigMu.Lock()
	previous := orchestrationConfigValue
	orchestrationConfigValue = config
	orchestrationConfigMu.Unlock()
	return func() {
		orchestrationConfigMu.Lock()
		orchestrationConfigValue = previous
		orchestrationConfigMu.Unlock()
	}
}
