package daemon

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
)

// Runtime status (settings-restructure plan §3.3): one row per registered
// runtime, saying how far Crossing Guard can see and stop it. The local facts
// come from guardcli's detection, the observations from stored live events —
// the same reads the team's device report is built from.

// runtimeObserved is what stored live events say about each runtime.
type runtimeObserved struct {
	live, canaries map[string]store.RuntimeObservation
	// ruleActive: the proof rule would deny the canary under the rulebook now.
	// ruleChecked: that question could be asked at all; false means unknown.
	ruleActive, ruleChecked bool
}

var errRuntimeObservationsUnavailable = errors.New("the event store is not open")

// runtimeObservations reads the newest live event and the newest proof-rule
// deny per runtime. It never caches: the device report always computes.
func runtimeObservations() (runtimeObserved, error) {
	if governor == nil || governor.ix == nil {
		return runtimeObserved{}, errRuntimeObservationsUnavailable
	}
	canaries, err := governor.ix.RuntimeCanaries(rulebook.CanaryRuleID)
	if err != nil {
		return runtimeObserved{}, err
	}
	live, err := governor.ix.RuntimeLastLive()
	if err != nil {
		return runtimeObserved{}, err
	}
	observed := runtimeObserved{live: live, canaries: canaries}
	if v, err := guardcli.CheckCommand("echo "+rulebook.CanaryMarker, governorDetectors()); err == nil {
		observed.ruleActive, observed.ruleChecked = canaryProvable(v), true
	}
	return observed, nil
}

// runtimeObservationCache holds the observations this route last read. The
// per-runtime read is a scan with no supporting index, so a console read reuses
// it for as long as the device report's own interval; a connection change or a
// watch that saw an event drops it.
var runtimeObservationCache struct {
	sync.Mutex
	observed runtimeObserved
	at       time.Time
}

func dropRuntimeObservationCache() {
	runtimeObservationCache.Lock()
	runtimeObservationCache.at = time.Time{}
	runtimeObservationCache.Unlock()
}

// runtimeObservationMaxAge is the team link's report interval: the configured
// one while a link document exists, the embedded default otherwise.
func runtimeObservationMaxAge() time.Duration {
	if team != nil {
		team.mu.Lock()
		interval := team.doc.ReportInterval.Duration
		team.mu.Unlock()
		if interval > 0 {
			return interval
		}
	}
	if document, err := teamlink.Default(); err == nil {
		return document.ReportInterval.Duration
	}
	return 0
}

// cachedRuntimeObservations never holds the cache's lock across the read: the
// scan can be slow, and a connection commit or a watch poll that drops the
// cache must not wait behind it. Two requests that both find it stale both
// read; the later one's answer stands.
func cachedRuntimeObservations(now time.Time) (runtimeObserved, time.Time, error) {
	maxAge := runtimeObservationMaxAge()
	runtimeObservationCache.Lock()
	observed, at := runtimeObservationCache.observed, runtimeObservationCache.at
	runtimeObservationCache.Unlock()
	if age := now.Sub(at); !at.IsZero() && age >= 0 && age < maxAge {
		return observed, at, nil
	}
	observed, err := runtimeObservations()
	if err != nil {
		return runtimeObserved{}, time.Time{}, err
	}
	runtimeObservationCache.Lock()
	runtimeObservationCache.observed, runtimeObservationCache.at = observed, now
	runtimeObservationCache.Unlock()
	return observed, now, nil
}

type runtimeStatusRow struct {
	Name              string          `json:"name"`
	DisplayName       string          `json:"display_name"`
	Installed         bool            `json:"installed"`
	PresenceReported  bool            `json:"presence_reported"`
	ConfigPath        string          `json:"config_path,omitempty"`
	HookConfigured    bool            `json:"hook_configured"`
	HookBinaryPresent bool            `json:"hook_binary_present"`
	HookCurrent       bool            `json:"hook_current"`
	CollectionOnly    bool            `json:"collection_only"`
	HookPhases        map[string]bool `json:"hook_phases,omitempty"`
	LastLiveAt        string          `json:"last_live_at,omitempty"`
	CanaryAt          string          `json:"canary_at,omitempty"`
	// Attention is the one thing about this runtime that needs the owner,
	// decided here; Problem is its sentence when the daemon has one.
	Attention         string `json:"attention,omitempty"`
	Problem           string `json:"problem,omitempty"`
	InterfaceRevision string `json:"interface_revision,omitempty"`
	GovernanceLane    string `json:"governance_lane,omitempty"`
	GovernanceNote    string `json:"governance_note,omitempty"`
}

type runtimeStatusResponse struct {
	Runtimes         []runtimeStatusRow `json:"runtimes"`
	CanaryRuleActive bool               `json:"canary_rule_active"`
	// CanaryRuleChecked: the rulebook could be asked; when false, active is unknown.
	CanaryRuleChecked bool `json:"canary_rule_checked"`
	// ObservedAt is when the observations were read; they may be that old.
	ObservedAt      string `json:"observed_at,omitempty"`
	LiveUnavailable bool   `json:"live_unavailable,omitempty"`
}

func registerRuntimeStatusRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runtime-status", handleRuntimeStatus)
}

func handleRuntimeStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, buildRuntimeStatus(time.Now()))
}

func buildRuntimeStatus(now time.Time) runtimeStatusResponse {
	response := runtimeStatusResponse{Runtimes: []runtimeStatusRow{}}
	observed, at, err := cachedRuntimeObservations(now)
	if err != nil {
		response.LiveUnavailable = true
	} else {
		response.CanaryRuleActive, response.CanaryRuleChecked = observed.ruleActive, observed.ruleChecked
		response.ObservedAt = at.UTC().Format(time.RFC3339)
	}
	guided := map[string]guardcli.RuntimeConnectionStatus{}
	if executable, err := runtimeIntegrationExecutable(); err == nil {
		for _, status := range guardcli.RuntimeConnections(executable) {
			guided[status.Descriptor.Runtime] = status
		}
	}
	capabilities := map[string]ChatCapability{}
	if list, err := chatCapabilities(); err == nil {
		for _, capability := range list {
			capabilities[capability.Runtime] = capability
		}
	}
	for _, runtime := range guardcli.DetectRuntimesMemoized() {
		row := runtimeStatusRow{Name: runtime.Name, DisplayName: runtime.Name, Installed: runtime.Installed,
			PresenceReported: runtime.PresenceReported, ConfigPath: runtime.Config, HookConfigured: runtime.Attached,
			HookBinaryPresent: runtime.BinaryPresent, HookCurrent: runtime.Current, CollectionOnly: runtime.CollectionOnly,
			HookPhases: runtime.HookPhases}
		connection, isGuided := guided[runtime.Name]
		capability, hasCapability := capabilities[runtime.Name]
		switch {
		case hasCapability && capability.DisplayName != "":
			row.DisplayName = capability.DisplayName
		case runtime.DisplayName != "":
			row.DisplayName = runtime.DisplayName
		}
		if hasCapability {
			row.InterfaceRevision, row.GovernanceLane, row.GovernanceNote = capability.InterfaceRevision, capability.GovernanceLane, capability.GovernanceNote
		}
		if o, ok := observed.live[runtime.Name]; ok {
			row.LastLiveAt = time.Unix(o.TS, 0).UTC().Format(time.RFC3339)
		}
		if o, ok := observed.canaries[runtime.Name]; ok {
			row.CanaryAt = time.Unix(o.TS, 0).UTC().Format(time.RFC3339)
		}
		row.Attention, row.Problem = runtimeAttention(row, connection, isGuided, response.LiveUnavailable)
		response.Runtimes = append(response.Runtimes, row)
	}
	return response
}

// runtimeAttention decides the one code a runtime's row carries. A guided
// connection's own judgement wins, so this read and the connection read never
// disagree about the same runtime.
func runtimeAttention(row runtimeStatusRow, connection guardcli.RuntimeConnectionStatus, isGuided, liveUnavailable bool) (string, string) {
	if isGuided && (connection.State == "needs_attention" || connection.State == "preview_blocked") {
		return connection.State, connection.Problem
	}
	switch {
	case row.HookConfigured && !row.HookBinaryPresent:
		return "hook_binary_missing", ""
	case row.HookConfigured && !row.HookCurrent:
		return "hook_outdated", ""
	case row.HookConfigured && row.LastLiveAt == "" && !liveUnavailable:
		return "never_fired", ""
	}
	return "", ""
}

// daemonVersionResponse is GET /api/version: the API contract a client built
// against, and the facts that identify this running daemon.
type daemonVersionResponse struct {
	Version      string `json:"version"`
	Note         string `json:"note"`
	BuildVersion string `json:"build_version"`
	StoreSchema  int    `json:"store_schema"`
	DataDir      string `json:"data_dir"`
	ListenAddr   string `json:"listen_addr"`
	// LogPath is set only when the service's own log file exists; a daemon
	// started from a terminal writes to that terminal and names none.
	LogPath string `json:"log_path,omitempty"`
}

func daemonVersion(dataDir, addr string) daemonVersionResponse {
	response := daemonVersionResponse{Version: apiVersion,
		Note:         "the canonical path prefix is /api/" + apiVersion + "/; /api/ is a compat alias",
		BuildVersion: guardcli.BuildVersion(), StoreSchema: store.SchemaVersion, DataDir: dataDir, ListenAddr: addr}
	if logPath := filepath.Join(dataDir, "daemon.log"); dataDir != "" {
		if info, err := os.Stat(logPath); err == nil && !info.IsDir() {
			response.LogPath = logPath
		}
	}
	return response
}
