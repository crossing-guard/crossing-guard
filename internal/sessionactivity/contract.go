// Package sessionactivity owns the provider-neutral session-presence
// projection and CARRIES the status decider's output (execution, attention)
// on the same Item so every consumer reads one frame. It computes none of
// that: the decider lives in the daemon, which sees every owner. This package
// still owns no task execution, approvals, control, provider discovery, HTTP,
// persistence, or presentation policy, and imports none of them.
package sessionactivity

import "time"

const SchemaVersion = 1

type Item struct {
	Runtime          string    `json:"runtime"`
	CatalogSessionID string    `json:"catalog_session_id"`
	NativeSessionID  string    `json:"native_session_id,omitempty"`
	Presence         string    `json:"presence"`  // open | changing | unknown
	Execution        string    `json:"execution"` // unknown until stronger provider evidence exists
	Evidence         string    `json:"evidence"`  // file_open | hook_liveness | native_protocol | native_incremental
	Freshness        string    `json:"freshness"` // live | recent | stale | unknown
	Authority        string    `json:"authority"` // observed | owned | none
	Controllable     bool      `json:"controllable"`
	ObservedAt       time.Time `json:"observed_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Detail           string    `json:"detail,omitempty"`
	// Attention is the decider's answer to "does this need the reader":
	// none | approval | new_result | new_failure | interrupted. AttentionID is
	// a monotonic id inside the space named by AttentionSource (task event id
	// or turn row id) — the browser's acknowledgement ledger compares within
	// one space only; the two are not one sequence.
	Attention       string `json:"attention,omitempty"`
	AttentionID     int64  `json:"attention_id,omitempty"`
	AttentionSource string `json:"attention_source,omitempty"` // task | turn
	// SinceMS is the daemon-clock instant of the fact that decided Execution,
	// so a renderer can always show how old the picture is.
	SinceMS int64 `json:"since_ms,omitempty"`
	// Progress refines a running Execution for the open session only:
	// thinking | writing | tool (with ProgressTool named). Absent everywhere
	// else, including every rail item — it is transient and never stored.
	Progress     string `json:"progress,omitempty"`
	ProgressTool string `json:"progress_tool,omitempty"`
	// Owner attention from helper agents (escalation-delivery plan §6.2),
	// carried beside the attention ladder, never inside it. AskID is the
	// ask's place in claim-settle order, its own id space (source "agent"):
	// acknowledging it acknowledges every ask settled up to it. AskText is the newest unresolved ask's
	// line, cut at the configured bound; AskAgent names the agent; AskCount
	// counts unresolved asks. DraftCount and DraftText describe proposed
	// replies that were not sent. AskState is "unknown" when the ask source
	// could not be read — the other fields are then absent, never zero-as-fact.
	AskID      int64  `json:"ask_id,omitempty"`
	AskText    string `json:"ask_text,omitempty"`
	AskAgent   string `json:"ask_agent,omitempty"`
	AskCount   int    `json:"ask_count,omitempty"`
	DraftCount int    `json:"draft_count,omitempty"`
	DraftText  string `json:"draft_text,omitempty"`
	AskState   string `json:"ask_state,omitempty"`
}

type Capability struct {
	Status string `json:"status"` // available | unavailable
	Detail string `json:"detail"`
}

type Snapshot struct {
	SchemaVersion int        `json:"schema_version"`
	Generation    int64      `json:"generation"`
	ObservedAt    time.Time  `json:"observed_at"`
	Capability    Capability `json:"capability"`
	Items         []Item     `json:"items"`
}
