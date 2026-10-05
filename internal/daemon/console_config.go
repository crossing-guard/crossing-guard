package daemon

// Console presentation defaults (workspace-panes implementation plan §8, R8).
//
// The browser must not compile in a layout preset, a split clamp, a keymap, or
// an editor URL scheme: those are operator policy. They live in one strict
// file, daemon.json, published through one typed route. A missing file means
// the compiled defaults with origin "builtin-default".
//
// The console writes its own selections into this file (session-view-and-
// console-preferences plan §B3, §B4), so it is live: the daemon re-reads it when
// the console writes it, and a stat check catches hand edits. A file that
// fails is logged and the last good value stays in force with origin
// "last-good"; a selection that no longer resolves degrades only its own
// section to the built-in. A typo can never take the console offline.

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/teamlink"
	"crossing-guard/internal/workspace"
	"crossing-guard/teamwire"
)

// daemonDefaultDocument holds the defaults of the daemon.json sections that are loaded
// from an embedded document rather than compiled (team rest-of-release plan §8.4): the
// handoff keys, and only those.
//
//go:embed daemon.default.json
var daemonDefaultDocument []byte

// reviewBudgets hands the published diff budgets to the review service.
func reviewBudgets(c ConsoleConfig) workspace.ReviewBudgets {
	return workspace.ReviewBudgets{BaseRef: c.Diff.BaseRef, MaxFiles: c.Diff.MaxFiles, MaxStatusEntries: c.Diff.MaxStatusEntries,
		MaxFileBytes: c.Diff.MaxFileBytes, ContextLines: c.Diff.ContextLines, MaxRefs: c.Diff.MaxRefs, GitTimeoutSeconds: c.Diff.GitTimeoutSeconds,
		MaxEntriesPerDir: c.Files.MaxEntriesPerDir, MaxReadBytes: c.Files.MaxReadBytes}
}

const consoleFormatVersion = 1

// ConsoleConfig is the published shape. Most fields are policy the browser
// reads; some sections are daemon-enforced bounds of console features (Diff's
// budgets via reviewBudgets, Models' discovery bounds).
type ConsoleConfig struct {
	FormatVersion int `json:"format_version"`
	// DefaultPreset names the layout a surface gets before the reader changes it.
	DefaultPreset string `json:"default_preset"`
	// Presets are layout trees in the pane-layout v4 grammar
	// ({type:"split",dir,ratio,a,b} | {type:"region",tabs:[paneID…]}); pane ids
	// that the surface does not offer are dropped when the preset is applied.
	Presets map[string]json.RawMessage `json:"presets"`
	// SplitClamp bounds a divider's ratio as [minimum, maximum].
	SplitClamp [2]float64 `json:"split_clamp"`
	// MinRegionPx is the size below which only the focused region renders.
	MinRegionPx ConsoleSize `json:"min_region_px"`
	// WorkspaceWidthPx is the right column's width before the reader resizes it.
	WorkspaceWidthPx int `json:"workspace_width_px"`
	// RecentlyClosed bounds the reopen list per surface.
	RecentlyClosed int `json:"recently_closed"`
	// Keymap holds the browser shortcuts by action name, in KeyboardEvent terms
	// ("Meta+." is the command key plus period).
	Keymap map[string]string `json:"keymap"`
	// EditorScheme opens a file in the reader's editor (scheme://file/path:line).
	EditorScheme string `json:"editor_scheme"`
	// NativeOpenLinks shows the control that opens a session in its vendor's
	// desktop app. False hides it everywhere (a machine without those apps).
	NativeOpenLinks bool `json:"native_open_links"`
	// Diff holds the Diff pane's presentation defaults.
	Diff ConsoleDiffDefaults `json:"diff"`
	// EditsPageSize bounds one page of a session's recorded edits.
	EditsPageSize int `json:"edits_page_size"`
	// Files holds the Files pane's budgets (console-files-pane-plan §8).
	Files ConsoleFilesDefaults `json:"files"`
	// Transcript selects the view profile module a session opens with.
	Transcript ConsoleTranscriptDefaults `json:"transcript"`
	// SessionOrganization holds the budgets of session tags, filters and saved
	// views. It holds no tag and no view: those are the owner's, never defaults.
	SessionOrganization ConsoleSessionOrganization `json:"session_organization"`
	// Appearance selects the appearance module the console renders with.
	Appearance ConsoleAppearanceSelection `json:"appearance"`
	// Limits bound what an appearance module may set. They are installation
	// policy, so they live here and never in a module file.
	Limits ConsoleLimits `json:"console_limits"`
	// WriteBytesMax bounds one console config write request body.
	WriteBytesMax int64 `json:"console_write_bytes_max"`
	// ReloadCheckMS is how often, at most, the daemon stats daemon.json for a
	// hand edit. The console's own writes apply at once regardless.
	ReloadCheckMS int `json:"console_reload_check_ms"`
	// Models bounds runtime model discovery (runtime-model-catalog-and-usage
	// plan §2.1). It lists no model: model facts come from each runtime.
	Models ConsoleModelsDefaults `json:"models"`
	// Usage holds the Usage pane's presentation defaults (session usage
	// breakdown plan §5.5).
	Usage ConsoleUsageDefaults `json:"usage"`
	// Recall bounds the recall tools agent sessions call (recall-mcp-v1-plan §3.8).
	Recall ConsoleRecallDefaults `json:"recall"`
	// UnderstandingRetention paces the automatic removal of analysis facts no
	// reader resolves any more (understanding-facts-retention plan §4.4).
	UnderstandingRetention ConsoleUnderstandingRetention `json:"understanding_retention"`
	// Handoff bounds handoff between members on this device (team rest-of-release
	// plan §8.4). It lives here and not in team.json because a local handoff works
	// on a device that is not linked.
	Handoff ConsoleHandoffDefaults `json:"handoff"`
}

// ConsoleHandoffDefaults is the handoff section. Its defaults come from the embedded
// daemon.default.json, never from a compiled literal.
type ConsoleHandoffDefaults struct {
	// InjectMaxBytes bounds the brief a session is handed at its first prompt.
	InjectMaxBytes int `json:"inject_max_bytes"`
	// FiringWindow is how recently a runtime's hooks must have been observed at
	// both events for Open to be offered for it.
	FiringWindow teamlink.Duration `json:"firing_window"`
	// BriefWait is how long an armed brief waits for its session's next carrying
	// event before the device gives up on it.
	BriefWait teamlink.Duration `json:"brief_wait"`
	// ConversationTurns is how many of the session's last turns a ticked excerpt
	// carries; ConversationMaxBytes bounds the excerpt's text in total.
	ConversationTurns    int `json:"conversation_turns"`
	ConversationMaxBytes int `json:"conversation_max_bytes"`
	// RecallCheckSessions is how many of a runtime's most recent sessions are read
	// to say whether its memory hook has been observed injecting.
	RecallCheckSessions int `json:"recall_check_sessions"`
	// FollowRetry is the pause before the handoff owner opens its subscription to the
	// task events again after it closed or could not be opened.
	FollowRetry teamlink.Duration `json:"follow_retry"`
}

// handoffConversationTurnsCeiling is handoff.schema.json's maxItems for an excerpt's
// turns, and handoffConversationBytesCeiling its maxLength for one turn's text: a
// configured bound above either would build a document the wire schema refuses.
const (
	handoffConversationTurnsCeiling = 200
	handoffConversationBytesCeiling = 65536
)

// defaultHandoffConfig reads the handoff section's defaults from the embedded
// document. The document is part of the binary, so a failure here is a build defect;
// TestDaemonDefaultDocumentLoads holds it.
func defaultHandoffConfig() (ConsoleHandoffDefaults, error) {
	var document struct {
		Handoff ConsoleHandoffDefaults `json:"handoff"`
	}
	if err := decodeModuleStrict(daemonDefaultDocument, &document); err != nil {
		return ConsoleHandoffDefaults{}, fmt.Errorf("embedded daemon.default.json: %w", err)
	}
	if err := validateHandoffConfig(document.Handoff); err != nil {
		return ConsoleHandoffDefaults{}, fmt.Errorf("embedded daemon.default.json: %w", err)
	}
	return document.Handoff, nil
}

// validateHandoffConfig checks the handoff section on its own terms: every bound is
// positive and the excerpt bounds fit the wire schema. inject_max_bytes is checked
// against the hook encoders' caps and orchestration.json's delivery.claim_bytes by
// validateHandoffInjectBudget (team_handoff_brief.go), which crosses owners this
// function does not read.
func validateHandoffConfig(h ConsoleHandoffDefaults) error {
	for name, value := range map[string]int64{"inject_max_bytes": int64(h.InjectMaxBytes), "firing_window": int64(h.FiringWindow.Duration),
		"brief_wait": int64(h.BriefWait.Duration), "conversation_turns": int64(h.ConversationTurns),
		"conversation_max_bytes": int64(h.ConversationMaxBytes), "recall_check_sessions": int64(h.RecallCheckSessions),
		"follow_retry": int64(h.FollowRetry.Duration)} {
		if value <= 0 {
			return fmt.Errorf("handoff.%s must be positive", name)
		}
	}
	if h.ConversationTurns > handoffConversationTurnsCeiling {
		return fmt.Errorf("handoff.conversation_turns must be at most %d (the %s wire schema's bound)", handoffConversationTurnsCeiling, teamwire.KindHandoff)
	}
	if h.ConversationMaxBytes > handoffConversationBytesCeiling {
		return fmt.Errorf("handoff.conversation_max_bytes must be at most %d (the %s wire schema's bound)", handoffConversationBytesCeiling, teamwire.KindHandoff)
	}
	return nil
}

// ConsoleUnderstandingRetention is the understanding_retention section. A
// complete generation keeps its facts while it is the baseline or the current
// boundary of a session seen within KeepDays, the newest of its checkout, or
// younger than IntermediateGraceHours; everything else is pruned.
type ConsoleUnderstandingRetention struct {
	Enabled bool `json:"enabled"`
	// KeepDays is how long after a session's newest checkpoint its baseline and
	// current analysis facts are kept.
	KeepDays int `json:"keep_days"`
	// IntermediateGraceHours keeps every generation this young, whatever reads it.
	IntermediateGraceHours int `json:"intermediate_grace_hours"`
	// StartDelaySeconds is the wait between the daemon starting and the first pass.
	// It is separate from the interval so a daemon that is restarted more often than
	// the interval still gets its passes.
	StartDelaySeconds int `json:"start_delay_seconds"`
	// PassIntervalSeconds is the time between candidate queries; PauseMS is the
	// sleep after every write transaction inside one pass.
	PassIntervalSeconds int `json:"pass_interval_seconds"`
	PauseMS             int `json:"pause_ms"`
	// DeleteRowsPerTransaction bounds how many unit and edge rows one transaction
	// removes, so no delete holds the store's write lock for long.
	DeleteRowsPerTransaction int `json:"delete_rows_per_transaction"`
	// DeleteDescriptorsPerTransaction bounds how many unreferenced unit descriptors
	// one transaction removes. Descriptors are kilobytes each, so the bound is far
	// lower than the row bound.
	DeleteDescriptorsPerTransaction int `json:"delete_descriptors_per_transaction"`
}

// Upper bounds of the understanding_retention section. They are safety limits, not
// tuning: the values become time.Duration, and an unbounded one wraps negative —
// a keep_days of 999999 meant as "forever" would put the horizon in the future
// and prune every live session's baseline.
const (
	understandingRetentionKeepDaysMax        = 36500
	understandingRetentionIntervalSecondsMax = 7 * 24 * 3600
	understandingRetentionPauseMSMax         = 10 * 60 * 1000
	understandingRetentionDeleteRowsMax      = 1000000
)

// validate holds the horizon and the grace at or above the scan recovery
// window: a shorter one would prune facts the recovery pass re-measures at
// once, a delete and re-scan loop that reclaims nothing.
func (r ConsoleUnderstandingRetention) validate() error {
	floorHours := int(understandingRecoveryWindow / time.Hour)
	switch {
	case r.KeepDays*24 < floorHours:
		return fmt.Errorf("understanding_retention.keep_days must cover the %d-hour scan recovery window", floorHours)
	case r.IntermediateGraceHours < floorHours:
		return fmt.Errorf("understanding_retention.intermediate_grace_hours must be at least the %d-hour scan recovery window", floorHours)
	case r.KeepDays > understandingRetentionKeepDaysMax:
		return fmt.Errorf("understanding_retention.keep_days must be at most %d; set enabled to false to keep everything", understandingRetentionKeepDaysMax)
	case r.IntermediateGraceHours > understandingRetentionKeepDaysMax*24:
		return fmt.Errorf("understanding_retention.intermediate_grace_hours must be at most %d", understandingRetentionKeepDaysMax*24)
	case r.StartDelaySeconds < 0 || r.StartDelaySeconds > understandingRetentionIntervalSecondsMax:
		return fmt.Errorf("understanding_retention.start_delay_seconds must be between 0 and %d", understandingRetentionIntervalSecondsMax)
	case r.PassIntervalSeconds < 1 || r.PassIntervalSeconds > understandingRetentionIntervalSecondsMax:
		return fmt.Errorf("understanding_retention.pass_interval_seconds must be between 1 and %d", understandingRetentionIntervalSecondsMax)
	case r.PauseMS < 0 || r.PauseMS > understandingRetentionPauseMSMax:
		return fmt.Errorf("understanding_retention.pause_ms must be between 0 and %d", understandingRetentionPauseMSMax)
	case r.DeleteRowsPerTransaction < 1 || r.DeleteRowsPerTransaction > understandingRetentionDeleteRowsMax:
		return fmt.Errorf("understanding_retention.delete_rows_per_transaction must be between 1 and %d", understandingRetentionDeleteRowsMax)
	case r.DeleteDescriptorsPerTransaction < 1 || r.DeleteDescriptorsPerTransaction > understandingRetentionDeleteRowsMax:
		return fmt.Errorf("understanding_retention.delete_descriptors_per_transaction must be between 1 and %d", understandingRetentionDeleteRowsMax)
	}
	return nil
}

// ConsoleRecallDefaults bounds the recall MCP server and the routes behind it.
type ConsoleRecallDefaults struct {
	// RequestTimeoutMS bounds one route call from the recall server;
	// MaxResultBytes bounds one tool result, trimmed by whole list items.
	RequestTimeoutMS int `json:"request_timeout_ms"`
	MaxResultBytes   int `json:"max_result_bytes"`
	// MemoryBodyMaxBytes cuts one memory body; MemoryExcerptBytes one search
	// excerpt; MemorySearchLimitMax bounds one memory search.
	MemoryBodyMaxBytes   int `json:"memory_body_max_bytes"`
	MemoryExcerptBytes   int `json:"memory_excerpt_bytes"`
	MemorySearchLimitMax int `json:"memory_search_limit_max"`
	// TagLookupLimit bounds one tag lookup or summary read, which holds one row
	// per live (session identity, agent, tag) however often it was re-claimed.
	TagLookupLimit int `json:"tag_lookup_limit"`
	// PeersMax bounds each relationship's peers; the git budgets bound the
	// checkouts read behind them, PeerRouteDeadlineMS the whole route (it
	// must stay below RequestTimeoutMS so the route answers first).
	PeersMax            int `json:"peers_max"`
	PeerGitTimeoutMS    int `json:"peer_git_timeout_ms"`
	PeerGitConcurrency  int `json:"peer_git_concurrency"`
	PeerRouteDeadlineMS int `json:"peer_route_deadline_ms"`
	PeerChangedFilesMax int `json:"peer_changed_files_max"`
	// ScopeCacheSeconds is how long a folder's resolved recall scope (its repository
	// label and remote-derived id) is reused, so SessionStart hooks in one checkout
	// cost one resolution (team item 5 decision 12).
	ScopeCacheSeconds int `json:"scope_cache_seconds"`
	// ScopeCacheMax bounds how many folders' scopes are held; past it the cache starts over.
	ScopeCacheMax int `json:"scope_cache_max"`
	// ProposeEnabled/ProposePerSessionMax are DEPRECATED (config-ownership
	// plan Fix A / RT-C4): the propose door's policy moved to memory.json's
	// `propose` section. They stay decodable for one release so an old file
	// never triggers the strict loader's refusal and silently reverts every
	// OTHER tuned setting; presence is reported in problems[] ("moved to
	// memory.json; ignored here") and nothing reads them. Removing them is a
	// later release's recorded breaking change.
	ProposeEnabled       bool `json:"propose_enabled"`
	ProposePerSessionMax int  `json:"propose_per_session_max"`
}

func (r ConsoleRecallDefaults) validate() error {
	for name, value := range map[string]int{"request_timeout_ms": r.RequestTimeoutMS, "max_result_bytes": r.MaxResultBytes,
		"memory_body_max_bytes": r.MemoryBodyMaxBytes, "memory_excerpt_bytes": r.MemoryExcerptBytes,
		"memory_search_limit_max": r.MemorySearchLimitMax, "tag_lookup_limit": r.TagLookupLimit,
		"peers_max": r.PeersMax, "peer_git_timeout_ms": r.PeerGitTimeoutMS, "peer_git_concurrency": r.PeerGitConcurrency,
		"peer_route_deadline_ms": r.PeerRouteDeadlineMS, "peer_changed_files_max": r.PeerChangedFilesMax,
		"scope_cache_seconds": r.ScopeCacheSeconds, "scope_cache_max": r.ScopeCacheMax} {
		if value <= 0 {
			return fmt.Errorf("recall.%s must be positive", name)
		}
	}
	if r.PeerRouteDeadlineMS >= r.RequestTimeoutMS {
		return errors.New("recall.peer_route_deadline_ms must be below recall.request_timeout_ms")
	}
	return nil
}

// ConsoleUsageDefaults is how the Usage pane first presents a session.
type ConsoleUsageDefaults struct {
	// TimelineGapMinutes is the idle gap the timeline collapses to one mark.
	TimelineGapMinutes int `json:"timeline_gap_minutes"`
	// WideColumnsPx is the pane width from which the table shows every column
	// instead of expanding rows.
	WideColumnsPx int `json:"wide_columns_px"`
	// DefaultMeasure is the measure the pane opens with; DefaultGrouping is
	// its table's grouping.
	DefaultMeasure  string `json:"default_measure"`
	DefaultGrouping string `json:"default_grouping"`
}

// usageMeasures and usageGroupings are the values the Usage pane knows.
var (
	usageMeasures  = map[string]bool{"all": true, "input": true, "output": true, "reasoning": true, "calls": true}
	usageGroupings = map[string]bool{"tree": true, "type": true}
)

// ConsoleModelsDefaults bounds the model lists runtimes report.
type ConsoleModelsDefaults struct {
	// RefreshAfterSeconds is the list age at which a read starts a background
	// rediscovery; MinRefreshSeconds rate-limits an explicit refresh.
	RefreshAfterSeconds int `json:"refresh_after_seconds"`
	MinRefreshSeconds   int `json:"min_refresh_seconds"`
	// DiscoveryTimeoutSeconds and DiscoveryOutputMaxBytes bound one discovery run.
	DiscoveryTimeoutSeconds int   `json:"discovery_timeout_seconds"`
	DiscoveryOutputMaxBytes int64 `json:"discovery_output_max_bytes"`
	// MaxEntries bounds one list; a longer list is refused, never truncated.
	MaxEntries int `json:"max_entries"`
	// MaxIDBytes and MaxLabelBytes bound one entry's id and labels.
	MaxIDBytes    int `json:"max_id_bytes"`
	MaxLabelBytes int `json:"max_label_bytes"`
}

// ConsoleAppearanceSelection names the appearance module in use.
type ConsoleAppearanceSelection struct {
	Module string `json:"module"`
}

// ConsoleLimits are the validation bounds for appearance modules.
type ConsoleLimits struct {
	TextSizeMin      float64 `json:"text_size_min"`
	TextSizeMax      float64 `json:"text_size_max"`
	WidthMin         int     `json:"width_min"`
	WidthMax         int     `json:"width_max"`
	FontItemCharsMax int     `json:"font_item_chars_max"`
	FontItemsMax     int     `json:"font_items_max"`
}

// Compiled bounds on the owner's bounds (plan §8). They exist so one typo
// cannot make every value invalid or unbounded: the write floor must hold a
// maximum-size module file or every write fails; the ceilings stop one request
// buffering an unbounded body; the reload floor keeps a stat off the hot path
// and the ceiling keeps a hand edit from needing a restart.
const (
	consoleWriteBytesFloor   = moduleReadLimit
	consoleWriteBytesCeiling = 16 * moduleReadLimit
	consoleReloadFloorMS     = 250
	consoleReloadCeilingMS   = 60000
	consoleTextSizeFloor     = 8
	consoleTextSizeCeiling   = 32
	consoleWidthFloor        = 320
	consoleWidthCeiling      = 3200
	consoleFontItemChars     = 128
	consoleFontItems         = 16
	// One board column read may not be unbounded however the budget is set
	// (board-column-overflow plan §2.1).
	consoleBoardColumnCardsCeiling = 1000
	// A board is a screen and its rules run on every read of it
	// (board-observed-columns plan §2.1).
	consoleBoardColumnsCeiling        = 50
	consoleBoardPlacementRulesCeiling = 100
)

// ConsoleSessionOrganization bounds what tagging, filtering and saved views
// may cost (session-organization implementation plan §4.3). Every feature
// works with these defaults; none is a step the owner must take first.
type ConsoleSessionOrganization struct {
	// RowTagsMax bounds the tag chips on one rail row.
	RowTagsMax int `json:"row_tags_max"`
	// RecentTagToggles is how many of the owner's most recently used tags the
	// open session's header offers as one-click toggles.
	RecentTagToggles int `json:"recent_tag_toggles"`
	// BulkSelectionMax bounds one tag request's sessions, TagsPerRequestMax its
	// tags: the two multiply inside one write transaction.
	BulkSelectionMax  int `json:"bulk_selection_max"`
	TagsPerRequestMax int `json:"tags_per_request_max"`
	// ViewsMax bounds the saved views file; ViewsVisible is how many the rail
	// lists before "Show all".
	ViewsMax     int `json:"views_max"`
	ViewsVisible int `json:"views_visible"`
	// HandoffsEndedVisible is how many ended handoffs, the most recently ended
	// first, the rail's Handoffs group lists before "Show ended". A handoff that
	// has not ended is always listed.
	HandoffsEndedVisible int `json:"handoffs_ended_visible"`
	// ViewsFileBytesMax bounds one read of session-views.json.
	ViewsFileBytesMax int64 `json:"views_file_bytes_max"`
	// CountRefreshMS coalesces the rail's view-count refreshes, and is how often
	// the rail's Handoffs group is read again.
	CountRefreshMS int `json:"count_refresh_ms"`
	// QueryBytesMax, QueryTermsMax and GlobExpansionMax bound one filter.
	QueryBytesMax    int `json:"query_bytes_max"`
	QueryTermsMax    int `json:"query_terms_max"`
	GlobExpansionMax int `json:"glob_expansion_max"`
	// TextHitLimit bounds a search confined to a filter's sessions.
	TextHitLimit int `json:"text_hit_limit"`
	// BoardColumnCardsMax bounds the cards one board column reads; a column
	// holding more says how many it does not show.
	BoardColumnCardsMax int `json:"board_column_cards_max"`
	// BoardColumnsMax bounds one board view's declared columns and
	// BoardPlacementRulesMax its placement rules.
	BoardColumnsMax        int `json:"board_columns_max"`
	BoardPlacementRulesMax int `json:"board_placement_rules_max"`
	// VocabularyMax bounds the tag suggestions one request returns.
	VocabularyMax int `json:"vocabulary_max"`
	// TagRequestBytesMax bounds one tag, note or view request body.
	TagRequestBytesMax int64 `json:"tag_request_bytes_max"`
	// TagIndexRowsMax bounds each whole-table read behind the rail's tags (the
	// agent-tag read holds one row per live (session identity, agent, tag)). A
	// read that reaches it makes view counts unavailable rather than wrong.
	TagIndexRowsMax int `json:"tag_index_rows_max"`
	// KeptSessionsMax bounds how many tagged sessions outlive their transcript
	// file; KeptTextDocumentsMax bounds what is shown of one.
	KeptSessionsMax      int `json:"kept_sessions_max"`
	KeptTextDocumentsMax int `json:"kept_text_documents_max"`
}

type ConsoleFilesDefaults struct {
	// MaxEntriesPerDir bounds one directory listing's response.
	MaxEntriesPerDir int `json:"max_entries_per_dir"`
	// MaxReadBytes bounds one file read; a whole file's text, unlike a patch.
	MaxReadBytes int64 `json:"max_read_bytes"`
}

// ConsoleTranscriptDefaults is selection only (transcript-view-profiles plan):
// the modules themselves are view-profiles/*.json, never this file.
type ConsoleTranscriptDefaults struct {
	// DefaultProfile is the module id for a session no role mapping names.
	DefaultProfile string `json:"default_profile"`
	// RoleProfiles maps an orchestration role (reviewer, follower, helper) to a
	// module id. A file's entries are added to the defaults', so an entry
	// changes a role's module and never removes another role's.
	RoleProfiles map[string]string `json:"role_profiles"`
}

// transcriptRoles are the orchestration roles a session can carry.
var transcriptRoles = map[string]bool{"reviewer": true, "follower": true, "helper": true}

type ConsoleSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type ConsoleDiffDefaults struct {
	SideBySideMinWidth int  `json:"side_by_side_min_width"`
	WordWrap           bool `json:"word_wrap"`
	HighlightWords     bool `json:"highlight_words"`
	GroupByFolder      bool `json:"group_by_folder"`
	HideWhitespace     bool `json:"hide_whitespace"`
	// FetchConcurrency is how many bodies or patches the pane fetches at once.
	FetchConcurrency int `json:"fetch_concurrency"`
	// StaleAfterSeconds is the age past which a re-shown git scope reloads.
	StaleAfterSeconds int `json:"stale_after_seconds"`
	// The git observation budgets (owner decision O-G): read-only git scopes need
	// no workspace file, so their policy lives here beside the pane's.
	// BaseRef is the ref the branch scopes compare against; empty resolves the
	// repository's default branch at call time.
	BaseRef string `json:"base_ref"`
	// MaxFiles bounds one listing; MaxStatusEntries bounds the per-file reads
	// behind it; MaxFileBytes bounds one rendered patch.
	MaxFiles         int   `json:"max_files"`
	MaxStatusEntries int   `json:"max_status_entries"`
	MaxFileBytes     int64 `json:"max_file_bytes"`
	// ContextLines is the -U value of a rendered patch.
	ContextLines int `json:"context_lines"`
	// MaxRefs bounds the compare picker's branches and commits, each.
	MaxRefs int `json:"max_refs"`
	// GitTimeoutSeconds is the deadline of one observation.
	GitTimeoutSeconds int `json:"git_timeout_seconds"`
}

// consoleConfigResponse is the typed body of GET /api/console/config.
// Problems lists why the file, or one of its sections, is not in force; doctor
// prints the same lines.
type consoleConfigResponse struct {
	Origin     string        `json:"origin"`
	Config     ConsoleConfig `json:"config"`
	Problems   []string      `json:"problems"`
	StateToken string        `json:"state_token"`
}

func defaultConsoleConfig() ConsoleConfig {
	// The embedded document cannot fail to load in a built binary (a test holds it);
	// a zero section would fail validation loudly rather than run with made-up bounds.
	handoff, _ := defaultHandoffConfig()
	region := func(tabs ...string) string {
		encoded, _ := json.Marshal(map[string]any{"type": "region", "tabs": tabs})
		return string(encoded)
	}
	split := func(dir string, ratio float64, a, b string) string {
		return fmt.Sprintf(`{"type":"split","dir":%q,"ratio":%g,"a":%s,"b":%s}`, dir, ratio, a, b)
	}
	return ConsoleConfig{
		FormatVersion: consoleFormatVersion,
		DefaultPreset: "review",
		Presets: map[string]json.RawMessage{
			"review": json.RawMessage(split("col", 0.62,
				region("workspace.diff", "workspace.files"),
				region("workspace.terminal", "session.plan", "session.tasks"))),
			"build": json.RawMessage(split("col", 0.55,
				split("row", 0.5, region("workspace.files"), region("workspace.diff")),
				region("workspace.terminal"))),
			"focus": json.RawMessage(region("workspace.diff", "workspace.terminal", "workspace.files", "session.plan")),
		},
		SplitClamp:       [2]float64{0.15, 0.85},
		MinRegionPx:      ConsoleSize{Width: 240, Height: 160},
		WorkspaceWidthPx: 560,
		RecentlyClosed:   8,
		Keymap: map[string]string{
			"hide_workspace": "Meta+.",
			"show_files":     "Meta+Shift+Y",
			"open_in_editor": "Meta+Shift+O",
			// Unmodified on purpose: it is bound where typing is excluded. An
			// empty value in daemon.json switches it off.
			"tag_session": "t",
		},
		EditorScheme: "vscode://file", NativeOpenLinks: true,
		Diff: ConsoleDiffDefaults{SideBySideMinWidth: 900, WordWrap: true, HighlightWords: true,
			GroupByFolder: true, HideWhitespace: false, FetchConcurrency: 4, StaleAfterSeconds: 30,
			MaxFiles: 500, MaxStatusEntries: 2000, MaxFileBytes: 2 << 20, ContextLines: 3, MaxRefs: 50, GitTimeoutSeconds: 20},
		EditsPageSize: 200,
		Files:         ConsoleFilesDefaults{MaxEntriesPerDir: 2000, MaxReadBytes: 1 << 20},
		Transcript: ConsoleTranscriptDefaults{DefaultProfile: "conversation",
			RoleProfiles: map[string]string{"helper": "agent-review", "follower": "agent-review"}},
		SessionOrganization: ConsoleSessionOrganization{RowTagsMax: 6, RecentTagToggles: 4, BulkSelectionMax: 200, TagsPerRequestMax: 20,
			ViewsMax: 60, ViewsVisible: 12, HandoffsEndedVisible: 3, ViewsFileBytesMax: 256 << 10, CountRefreshMS: 4000,
			QueryBytesMax: 600, QueryTermsMax: 24, GlobExpansionMax: 200, TextHitLimit: 200, BoardColumnCardsMax: 200, BoardColumnsMax: 12, BoardPlacementRulesMax: 24, VocabularyMax: 400,
			TagRequestBytesMax: 256 << 10, TagIndexRowsMax: 250000, KeptSessionsMax: 2000, KeptTextDocumentsMax: 4000},
		Appearance: ConsoleAppearanceSelection{Module: "default"},
		Limits: ConsoleLimits{TextSizeMin: 11, TextSizeMax: 20, WidthMin: 560, WidthMax: 1600,
			FontItemCharsMax: 64, FontItemsMax: 8},
		WriteBytesMax: 64 << 10,
		ReloadCheckMS: 1000,
		Usage: ConsoleUsageDefaults{TimelineGapMinutes: 20, WideColumnsPx: 720, DefaultMeasure: "all",
			DefaultGrouping: "tree"},
		Models: ConsoleModelsDefaults{RefreshAfterSeconds: 300, MinRefreshSeconds: 10, DiscoveryTimeoutSeconds: 20,
			DiscoveryOutputMaxBytes: 8 << 20, MaxEntries: 1000, MaxIDBytes: 256, MaxLabelBytes: 200},
		Recall: ConsoleRecallDefaults{RequestTimeoutMS: 15000, MaxResultBytes: 60000, MemoryBodyMaxBytes: 16000,
			MemoryExcerptBytes: 400, MemorySearchLimitMax: 50, TagLookupLimit: 200, PeersMax: 20, PeerGitTimeoutMS: 3000,
			PeerGitConcurrency: 4, PeerRouteDeadlineMS: 10000, PeerChangedFilesMax: 200, ScopeCacheSeconds: 60, ScopeCacheMax: 512,
			ProposeEnabled: false, ProposePerSessionMax: 0},
		UnderstandingRetention: ConsoleUnderstandingRetention{Enabled: true, KeepDays: 14,
			IntermediateGraceHours: 24, StartDelaySeconds: 120, PassIntervalSeconds: 1800, PauseMS: 2000,
			DeleteRowsPerTransaction: 10000, DeleteDescriptorsPerTransaction: 500},
		Handoff: handoff,
	}
}

// consoleConfigPath is the daemon tunables file's name. It was console.json;
// config-ownership plan Fix B renamed it to daemon.json (the file outgrew
// the name years ago). A data dir that still holds the old name is adopted
// once — see adoptLegacyConsoleConfig.
func consoleConfigPath(dataDir string) string { return filepath.Join(dataDir, "daemon.json") }

// legacyConsoleConfigPath is the pre-rename name, read ONCE for adoption.
func legacyConsoleConfigPath(dataDir string) string { return filepath.Join(dataDir, "console.json") }

// adoptLegacyConsoleConfig copies an old-name file to the new name, once, and
// only when idle (RT-C3: a browser holding a state token read from the old
// file must not have its next CAS write re-keyed silently). The old file is
// left untouched — ignored after adoption, never deleted (the reversible
// choice). Called from the config refresh path with the state lock held.
func adoptLegacyConsoleConfig(dataDir string) {
	if _, err := os.Stat(consoleConfigPath(dataDir)); err == nil {
		return // the new name exists; the old one, if any, is already ignored
	}
	raw, err := os.ReadFile(legacyConsoleConfigPath(dataDir))
	if err != nil {
		return // nothing to adopt
	}
	tmp := consoleConfigPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("daemon configuration adoption: could not write the adopted file: %v", err)
		return
	}
	if err := os.Rename(tmp, consoleConfigPath(dataDir)); err != nil {
		_ = os.Remove(tmp)
		log.Printf("daemon configuration adoption: could not rename: %v", err)
		return
	}
	log.Printf("daemon configuration: adopted %s as %s (the old file is now ignored, left in place)",
		legacyConsoleConfigPath(dataDir), consoleConfigPath(dataDir))
}

// consoleConfigReadLimit bounds the file read; the document is a few hundred bytes.
const consoleConfigReadLimit = 64 << 10

// loadConsoleConfig reads daemon.json. A file-level failure is an error and
// the caller keeps the last good value; a selection that does not resolve
// degrades only its own section to the built-in and comes back as a problem.
func loadConsoleConfig(dataDir string) (ConsoleConfig, string, []string, error) {
	path := consoleConfigPath(dataDir)
	raw, err := readConsoleConfigFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultConsoleConfig(), "builtin-default", nil, nil
		}
		return ConsoleConfig{}, "", nil, fmt.Errorf("open console configuration: %w", err)
	}
	config, problems, err := parseConsoleConfig(raw, dataDir)
	if err != nil {
		return ConsoleConfig{}, "", nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, path, problems, nil
}

func readConsoleConfigFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, consoleConfigReadLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > consoleConfigReadLimit {
		return nil, fmt.Errorf("the file is larger than %d bytes", consoleConfigReadLimit)
	}
	return raw, nil
}

// parseConsoleConfig decodes one daemon.json body over the defaults. Entries
// of a map (keymap, role_profiles) are added to the defaults' entries.
func parseConsoleConfig(raw []byte, dataDir string) (ConsoleConfig, []string, error) {
	config := defaultConsoleConfig()
	if err := decodeModuleStrict(raw, &config); err != nil {
		return ConsoleConfig{}, nil, err
	}
	if err := config.validate(); err != nil {
		return ConsoleConfig{}, nil, fmt.Errorf("validate: %w", err)
	}
	if err := validateAppearanceLimits(config.Limits); err != nil {
		return ConsoleConfig{}, nil, fmt.Errorf("validate: %w", err)
	}
	defaults := defaultConsoleConfig()
	problems := []string{}
	if err := validateTranscriptSelection(config.Transcript, dataDir); err != nil {
		problems = append(problems, err.Error()+"; the built-in transcript selection is in use")
		config.Transcript = defaults.Transcript
	}
	if err := validateAppearanceSelection(config.Appearance, config.Limits, dataDir); err != nil {
		problems = append(problems, err.Error()+"; the built-in appearance is in use")
		config.Appearance = defaults.Appearance
	}
	// A bad retention section turns retention OFF and leaves the rest of the file
	// in force: deleting is not a behaviour a defaulted value may switch on.
	if err := config.UnderstandingRetention.validate(); err != nil {
		problems = append(problems, err.Error()+"; understanding retention is off")
		config.UnderstandingRetention = defaults.UnderstandingRetention
		config.UnderstandingRetention.Enabled = false
	}
	// A bad handoff section falls back to the embedded defaults and leaves the rest
	// of the file in force: a typo must not stop a handoff from being read.
	if err := validateHandoffConfig(config.Handoff); err != nil {
		problems = append(problems, err.Error()+"; the built-in handoff bounds are in use")
		config.Handoff = defaults.Handoff
	} else if err := validateHandoffInjectBudget(config.Handoff.InjectMaxBytes, orchestrationConfig().Delivery); err != nil {
		// The brief's ceiling crosses two other owners — the hook encoders' caps and
		// the delivery claim budget — so a value that does not fit them is refused
		// here, by name, and never loaded.
		problems = append(problems, err.Error()+"; the built-in handoff bounds are in use")
		config.Handoff = defaults.Handoff
	}
	// RT-C4: the moved propose keys are named, not silently ignored — the
	// owner holding an old file is told where the policy lives now.
	if config.Recall.ProposeEnabled || config.Recall.ProposePerSessionMax != 0 {
		problems = append(problems, "recall.propose_enabled and recall.propose_per_session_max moved to memory.json's propose section; ignored here")
	}
	return config, problems, nil
}

func (o ConsoleSessionOrganization) validate() error {
	for name, value := range map[string]int64{"row_tags_max": int64(o.RowTagsMax), "recent_tag_toggles": int64(o.RecentTagToggles),
		"bulk_selection_max": int64(o.BulkSelectionMax), "tags_per_request_max": int64(o.TagsPerRequestMax), "views_max": int64(o.ViewsMax), "views_visible": int64(o.ViewsVisible),
		"handoffs_ended_visible": int64(o.HandoffsEndedVisible),
		"views_file_bytes_max":   o.ViewsFileBytesMax, "count_refresh_ms": int64(o.CountRefreshMS),
		"query_bytes_max": int64(o.QueryBytesMax), "query_terms_max": int64(o.QueryTermsMax),
		"glob_expansion_max": int64(o.GlobExpansionMax), "text_hit_limit": int64(o.TextHitLimit), "board_column_cards_max": int64(o.BoardColumnCardsMax),
		"board_columns_max": int64(o.BoardColumnsMax), "board_placement_rules_max": int64(o.BoardPlacementRulesMax),
		"vocabulary_max": int64(o.VocabularyMax), "tag_request_bytes_max": o.TagRequestBytesMax,
		"tag_index_rows_max": int64(o.TagIndexRowsMax), "kept_sessions_max": int64(o.KeptSessionsMax),
		"kept_text_documents_max": int64(o.KeptTextDocumentsMax)} {
		if value <= 0 {
			return fmt.Errorf("session_organization.%s must be positive", name)
		}
	}
	if o.BoardColumnCardsMax > consoleBoardColumnCardsCeiling {
		return fmt.Errorf("session_organization.board_column_cards_max must be at most %d", consoleBoardColumnCardsCeiling)
	}
	if o.BoardColumnsMax > consoleBoardColumnsCeiling {
		return fmt.Errorf("session_organization.board_columns_max must be at most %d", consoleBoardColumnsCeiling)
	}
	if o.BoardPlacementRulesMax > consoleBoardPlacementRulesCeiling {
		return fmt.Errorf("session_organization.board_placement_rules_max must be at most %d", consoleBoardPlacementRulesCeiling)
	}
	return nil
}

func (c ConsoleConfig) validate() error {
	if c.FormatVersion != consoleFormatVersion {
		return fmt.Errorf("unsupported format_version %d", c.FormatVersion)
	}
	if _, ok := c.Presets[c.DefaultPreset]; !ok {
		return fmt.Errorf("default_preset %q is not a preset", c.DefaultPreset)
	}
	for name, preset := range c.Presets {
		if err := validateLayoutNode(preset, 0); err != nil {
			return fmt.Errorf("preset %q: %w", name, err)
		}
	}
	if !(0 < c.SplitClamp[0] && c.SplitClamp[0] < c.SplitClamp[1] && c.SplitClamp[1] < 1) {
		return errors.New("split_clamp must satisfy 0 < min < max < 1")
	}
	for name, value := range map[string]int{"min_region_px.width": c.MinRegionPx.Width,
		"min_region_px.height": c.MinRegionPx.Height, "recently_closed": c.RecentlyClosed, "workspace_width_px": c.WorkspaceWidthPx,
		"diff.side_by_side_min_width": c.Diff.SideBySideMinWidth, "diff.fetch_concurrency": c.Diff.FetchConcurrency,
		"diff.stale_after_seconds": c.Diff.StaleAfterSeconds, "diff.max_files": c.Diff.MaxFiles,
		"diff.max_status_entries": c.Diff.MaxStatusEntries, "diff.max_refs": c.Diff.MaxRefs,
		"diff.git_timeout_seconds": c.Diff.GitTimeoutSeconds, "edits_page_size": c.EditsPageSize,
		"models.refresh_after_seconds": c.Models.RefreshAfterSeconds, "models.min_refresh_seconds": c.Models.MinRefreshSeconds,
		"models.discovery_timeout_seconds": c.Models.DiscoveryTimeoutSeconds, "models.max_entries": c.Models.MaxEntries,
		"models.max_id_bytes": c.Models.MaxIDBytes, "models.max_label_bytes": c.Models.MaxLabelBytes,
		"usage.timeline_gap_minutes": c.Usage.TimelineGapMinutes, "usage.wide_columns_px": c.Usage.WideColumnsPx} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.Diff.MaxFileBytes <= 0 {
		return errors.New("diff.max_file_bytes must be positive")
	}
	if c.Models.DiscoveryOutputMaxBytes <= 0 {
		return errors.New("models.discovery_output_max_bytes must be positive")
	}
	if err := c.SessionOrganization.validate(); err != nil {
		return err
	}
	if !usageMeasures[c.Usage.DefaultMeasure] {
		return fmt.Errorf("usage.default_measure %q is not one of all, input, output, reasoning, calls", c.Usage.DefaultMeasure)
	}
	if !usageGroupings[c.Usage.DefaultGrouping] {
		return fmt.Errorf("usage.default_grouping %q is not tree or type", c.Usage.DefaultGrouping)
	}
	if err := c.Recall.validate(); err != nil {
		return err
	}
	if c.Files.MaxEntriesPerDir <= 0 || c.Files.MaxReadBytes <= 0 {
		return errors.New("files.max_entries_per_dir and files.max_read_bytes must be positive")
	}
	if c.Diff.ContextLines < 0 {
		return errors.New("diff.context_lines must be zero or positive")
	}
	if err := changeenv.ValidBaseRef(c.Diff.BaseRef); err != nil {
		return fmt.Errorf("diff.base_ref: %w", err)
	}
	for _, action := range []string{"hide_workspace", "show_files", "open_in_editor"} {
		if c.Keymap[action] == "" {
			return fmt.Errorf("keymap.%s must be set", action)
		}
	}
	if c.EditorScheme == "" {
		return errors.New("editor_scheme must be set")
	}
	if c.Transcript.DefaultProfile == "" {
		return errors.New("transcript.default_profile must be set")
	}
	if c.Appearance.Module == "" {
		return errors.New("appearance.module must be set")
	}
	if c.WriteBytesMax < consoleWriteBytesFloor || c.WriteBytesMax > consoleWriteBytesCeiling {
		return fmt.Errorf("console_write_bytes_max must be between %d and %d", consoleWriteBytesFloor, consoleWriteBytesCeiling)
	}
	if c.ReloadCheckMS < consoleReloadFloorMS || c.ReloadCheckMS > consoleReloadCeilingMS {
		return fmt.Errorf("console_reload_check_ms must be between %d and %d", consoleReloadFloorMS, consoleReloadCeilingMS)
	}
	if err := c.Limits.validate(); err != nil {
		return err
	}
	roles := make([]string, 0, len(c.Transcript.RoleProfiles))
	for role := range c.Transcript.RoleProfiles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if !transcriptRoles[role] {
			return fmt.Errorf("transcript.role_profiles.%s: role must be reviewer, follower or helper", role)
		}
	}
	return nil
}

// layoutNodeDepthLimit bounds preset nesting; a deeper tree is a mistake, not a layout.
const layoutNodeDepthLimit = 8

// validateLayoutNode checks the preset grammar without knowing any pane catalog:
// a preset may name panes a surface does not offer, and the browser drops those.
func validateLayoutNode(raw json.RawMessage, depth int) error {
	if depth > layoutNodeDepthLimit {
		return errors.New("layout nests too deeply")
	}
	var node struct {
		Type  string          `json:"type"`
		Dir   string          `json:"dir"`
		Ratio float64         `json:"ratio"`
		A     json.RawMessage `json:"a"`
		B     json.RawMessage `json:"b"`
		Tabs  []string        `json:"tabs"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return err
	}
	switch node.Type {
	case "region":
		for _, tab := range node.Tabs {
			if tab == "" {
				return errors.New("region tab ids must be non-empty")
			}
		}
		return nil
	case "split":
		if node.Dir != "row" && node.Dir != "col" {
			return fmt.Errorf("split dir %q must be row or col", node.Dir)
		}
		if !(0 < node.Ratio && node.Ratio < 1) {
			return errors.New("split ratio must be between 0 and 1")
		}
		if node.A == nil || node.B == nil {
			return errors.New("split needs both a and b")
		}
		if err := validateLayoutNode(node.A, depth+1); err != nil {
			return err
		}
		return validateLayoutNode(node.B, depth+1)
	default:
		return fmt.Errorf("layout node type %q must be region or split", node.Type)
	}
}

// consoleConfigState is the resolved daemon.json, re-read when the console
// writes it (invalidate) or when a stat, at most every ReloadCheckMS, shows a
// hand edit. Readers share a lock; one reload runs at a time.
type consoleConfigState struct {
	mu     sync.RWMutex
	loaded bool
	// dirty forces the next read to re-read the file without forgetting the
	// value in force, so a failed write can never discard the last good value.
	dirty    bool
	dataDir  string
	value    ConsoleConfig
	origin   string
	problems []string
	token    string
	stamp    consoleFileStamp
	checked  time.Time
}

type consoleFileStamp struct {
	exists  bool
	size    int64
	modTime time.Time
}

var consoleState consoleConfigState

// consoleConfig returns the console configuration in force and its origin.
func consoleConfig() (ConsoleConfig, string) {
	snapshot := consoleConfigSnapshot()
	return snapshot.Config, snapshot.Origin
}

func consoleConfigSnapshot() consoleConfigResponse {
	dataDir := consoleDataDir()
	consoleState.mu.RLock()
	fresh := consoleState.loaded && !consoleState.dirty && consoleState.dataDir == dataDir &&
		time.Since(consoleState.checked) < time.Duration(consoleState.value.ReloadCheckMS)*time.Millisecond
	if fresh {
		snapshot := consoleState.snapshotLocked()
		consoleState.mu.RUnlock()
		return snapshot
	}
	consoleState.mu.RUnlock()
	consoleState.mu.Lock()
	defer consoleState.mu.Unlock()
	consoleState.refreshLocked(dataDir)
	return consoleState.snapshotLocked()
}

func (s *consoleConfigState) snapshotLocked() consoleConfigResponse {
	return consoleConfigResponse{Origin: s.origin, Config: s.value, Problems: append([]string{}, s.problems...), StateToken: s.token}
}

// refreshLocked re-reads console.json when it changed on disk, the console
// wrote it (dirty), the data directory changed, or nothing is loaded yet. The
// token, the value and the stamp all come from one read of one descriptor, so
// they always describe the same bytes.
func (s *consoleConfigState) refreshLocked(dataDir string) {
	path := consoleConfigPath(dataDir)
	s.checked = time.Now()
	// RT-C3: adoption waits for idle — a pending console write (dirty) holds
	// the old bytes' token and must not be silently re-keyed by a rename.
	if !s.dirty {
		adoptLegacyConsoleConfig(dataDir)
	}
	sameDir := s.loaded && s.dataDir == dataDir
	if sameDir && !s.dirty && statConsoleConfig(path) == s.stamp {
		return
	}
	s.dirty, s.dataDir = false, dataDir
	raw, stamp, readErr := readConsoleConfigOnce(path)
	s.stamp = stamp
	s.token = ""
	var (
		config   ConsoleConfig
		problems []string
		err      = readErr
		origin   = path
	)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		config, origin, err = defaultConsoleConfig(), "builtin-default", nil
	case readErr == nil:
		s.token = moduleToken(raw)
		config, problems, err = parseConsoleConfig(raw, dataDir)
		if err != nil {
			err = fmt.Errorf("%s: %w", path, err)
		}
	}
	switch {
	case err == nil:
		s.value, s.origin, s.problems = config, origin, problems
	case sameDir:
		log.Printf("console configuration unusable, keeping the last good value: %v", err)
		s.origin, s.problems = "last-good", []string{err.Error()}
	default:
		// The file exists and does not parse, and nothing has parsed since start:
		// the defaults apply, and the origin says the file is the reason.
		log.Printf("console configuration unusable, using defaults: %v", err)
		s.value, s.origin, s.problems = defaultConsoleConfig(), "invalid", []string{err.Error()}
	}
	s.loaded = true
	for _, problem := range s.problems {
		log.Printf("console configuration: %s", problem)
	}
	profiles, rejected := LoadViewProfiles(dataDir)
	log.Printf("console configuration: origin=%s default_preset=%s view_profiles=%d", s.origin, s.value.DefaultPreset, len(profiles))
	for _, rejection := range rejected {
		log.Printf("view profile skipped: %s: %s", rejection.Path, rejection.Error)
	}
}

// readConsoleConfigOnce reads the file and stats the same descriptor.
func readConsoleConfigOnce(path string) ([]byte, consoleFileStamp, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, consoleFileStamp{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, consoleFileStamp{}, err
	}
	stamp := consoleFileStamp{exists: true, size: info.Size(), modTime: info.ModTime()}
	raw, err := io.ReadAll(io.LimitReader(file, consoleConfigReadLimit+1))
	if err != nil {
		return nil, stamp, err
	}
	if len(raw) > consoleConfigReadLimit {
		return nil, stamp, fmt.Errorf("the file is larger than %d bytes", consoleConfigReadLimit)
	}
	return raw, stamp, nil
}

// invalidateConsoleConfig makes the next read re-read console.json, keeping
// the value in force until that read succeeds. The console's own writes to
// console.json or to a module call it, so a selection re-resolves at once.
func invalidateConsoleConfig() {
	consoleState.mu.Lock()
	defer consoleState.mu.Unlock()
	consoleState.dirty = true
}

func statConsoleConfig(path string) consoleFileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return consoleFileStamp{}
	}
	return consoleFileStamp{exists: true, size: info.Size(), modTime: info.ModTime()}
}

func registerConsoleConfigRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/console/config", handleConsoleConfig)
	mux.HandleFunc("PUT /api/console/config/transcript", handleConsoleTranscriptSelectionPut)
	mux.HandleFunc("PUT /api/console/config/appearance", handleConsoleAppearanceSelectionPut)
	mux.HandleFunc("GET /api/console/view-profiles", handleConsoleViewProfiles)
	mux.HandleFunc("PUT /api/console/view-profiles/{id}", handleConsoleViewProfilePut)
	mux.HandleFunc("DELETE /api/console/view-profiles/{id}", handleConsoleViewProfileDelete)
	registerAppearanceRoutes(mux)
}

func handleConsoleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, consoleConfigSnapshot())
}
