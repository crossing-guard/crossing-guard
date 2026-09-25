package daemon

// Console presentation defaults (workspace-panes implementation plan §8, R8).
//
// The browser must not compile in a layout preset, a split clamp, a keymap, or
// an editor URL scheme: those are operator policy. They live in one strict
// file, console.json, published through one typed route, and the browser reads
// them once at boot. A missing file means the compiled defaults with origin
// "builtin-default"; a malformed file is a logged error and the defaults, so a
// typo cannot take the console offline.

import (
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

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/workspace"
)

// reviewBudgets hands the published diff budgets to the review service.
func reviewBudgets(c ConsoleConfig) workspace.ReviewBudgets {
	return workspace.ReviewBudgets{BaseRef: c.Diff.BaseRef, MaxFiles: c.Diff.MaxFiles, MaxStatusEntries: c.Diff.MaxStatusEntries,
		MaxFileBytes: c.Diff.MaxFileBytes, ContextLines: c.Diff.ContextLines, MaxRefs: c.Diff.MaxRefs, GitTimeoutSeconds: c.Diff.GitTimeoutSeconds,
		MaxEntriesPerDir: c.Files.MaxEntriesPerDir, MaxReadBytes: c.Files.MaxReadBytes}
}

const consoleFormatVersion = 1

// ConsoleConfig is the published shape; every field is policy the browser reads.
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
}

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
	// ViewsFileBytesMax bounds one read of session-views.json.
	ViewsFileBytesMax int64 `json:"views_file_bytes_max"`
	// CountRefreshMS coalesces the rail's view-count refreshes.
	CountRefreshMS int `json:"count_refresh_ms"`
	// QueryBytesMax, QueryTermsMax and GlobExpansionMax bound one filter.
	QueryBytesMax    int `json:"query_bytes_max"`
	QueryTermsMax    int `json:"query_terms_max"`
	GlobExpansionMax int `json:"glob_expansion_max"`
	// TextHitLimit bounds a search confined to a filter's sessions.
	TextHitLimit int `json:"text_hit_limit"`
	// VocabularyMax bounds the tag suggestions one request returns.
	VocabularyMax int `json:"vocabulary_max"`
	// TagRequestBytesMax bounds one tag, note or view request body.
	TagRequestBytesMax int64 `json:"tag_request_bytes_max"`
	// TagIndexRowsMax bounds each whole-table read behind the rail's tags. A
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
type consoleConfigResponse struct {
	Origin string        `json:"origin"`
	Config ConsoleConfig `json:"config"`
}

func defaultConsoleConfig() ConsoleConfig {
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
			// empty value in console.json switches it off.
			"tag_session": "t",
		},
		EditorScheme: "vscode://file",
		Diff: ConsoleDiffDefaults{SideBySideMinWidth: 900, WordWrap: true, HighlightWords: true,
			GroupByFolder: true, HideWhitespace: false, FetchConcurrency: 4, StaleAfterSeconds: 30,
			MaxFiles: 500, MaxStatusEntries: 2000, MaxFileBytes: 2 << 20, ContextLines: 3, MaxRefs: 50, GitTimeoutSeconds: 20},
		EditsPageSize: 200,
		Files:         ConsoleFilesDefaults{MaxEntriesPerDir: 2000, MaxReadBytes: 1 << 20},
		Transcript: ConsoleTranscriptDefaults{DefaultProfile: "full",
			RoleProfiles: map[string]string{"helper": "agent-review", "follower": "agent-review"}},
		SessionOrganization: ConsoleSessionOrganization{RowTagsMax: 6, RecentTagToggles: 4, BulkSelectionMax: 200, TagsPerRequestMax: 20,
			ViewsMax: 60, ViewsVisible: 12, ViewsFileBytesMax: 256 << 10, CountRefreshMS: 4000,
			QueryBytesMax: 600, QueryTermsMax: 24, GlobExpansionMax: 200, TextHitLimit: 200, VocabularyMax: 400,
			TagRequestBytesMax: 256 << 10, TagIndexRowsMax: 250000, KeptSessionsMax: 2000, KeptTextDocumentsMax: 4000},
	}
}

func consoleConfigPath(dataDir string) string { return filepath.Join(dataDir, "console.json") }

// consoleConfigReadLimit bounds the file read; the document is a few hundred bytes.
const consoleConfigReadLimit = 64 << 10

func loadConsoleConfig(dataDir string) (ConsoleConfig, string, error) {
	path := consoleConfigPath(dataDir)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultConsoleConfig(), "builtin-default", nil
		}
		return ConsoleConfig{}, "", fmt.Errorf("open console configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, consoleConfigReadLimit))
	decoder.DisallowUnknownFields()
	config := defaultConsoleConfig()
	if err := decoder.Decode(&config); err != nil {
		return ConsoleConfig{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ConsoleConfig{}, "", fmt.Errorf("decode %s: trailing JSON data", path)
	}
	if err := config.validate(); err != nil {
		return ConsoleConfig{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	if err := validateTranscriptSelection(config.Transcript, dataDir); err != nil {
		return ConsoleConfig{}, "", fmt.Errorf("validate %s: %w", path, err)
	}
	return config, path, nil
}

func (o ConsoleSessionOrganization) validate() error {
	for name, value := range map[string]int64{"row_tags_max": int64(o.RowTagsMax), "recent_tag_toggles": int64(o.RecentTagToggles),
		"bulk_selection_max": int64(o.BulkSelectionMax), "tags_per_request_max": int64(o.TagsPerRequestMax), "views_max": int64(o.ViewsMax), "views_visible": int64(o.ViewsVisible),
		"views_file_bytes_max": o.ViewsFileBytesMax, "count_refresh_ms": int64(o.CountRefreshMS),
		"query_bytes_max": int64(o.QueryBytesMax), "query_terms_max": int64(o.QueryTermsMax),
		"glob_expansion_max": int64(o.GlobExpansionMax), "text_hit_limit": int64(o.TextHitLimit),
		"vocabulary_max": int64(o.VocabularyMax), "tag_request_bytes_max": o.TagRequestBytesMax,
		"tag_index_rows_max": int64(o.TagIndexRowsMax), "kept_sessions_max": int64(o.KeptSessionsMax),
		"kept_text_documents_max": int64(o.KeptTextDocumentsMax)} {
		if value <= 0 {
			return fmt.Errorf("session_organization.%s must be positive", name)
		}
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
		"diff.git_timeout_seconds": c.Diff.GitTimeoutSeconds, "edits_page_size": c.EditsPageSize} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.Diff.MaxFileBytes <= 0 {
		return errors.New("diff.max_file_bytes must be positive")
	}
	if err := c.SessionOrganization.validate(); err != nil {
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

var (
	consoleConfigOnce   sync.Once
	consoleConfigValue  ConsoleConfig
	consoleConfigOrigin string
)

// consoleConfig resolves once per process, like the other typed config owners.
func consoleConfig() (ConsoleConfig, string) {
	consoleConfigOnce.Do(func() {
		dataDir := filepath.Dir(indexPath())
		config, origin, err := loadConsoleConfig(dataDir)
		if err != nil {
			log.Printf("console configuration unusable, using defaults: %v", err)
			config, origin = defaultConsoleConfig(), "builtin-default"
		}
		profiles, rejected := LoadViewProfiles(dataDir)
		log.Printf("console configuration: origin=%s default_preset=%s view_profiles=%d", origin, config.DefaultPreset, len(profiles))
		for _, rejection := range rejected {
			log.Printf("view profile skipped: %s: %s", rejection.Path, rejection.Error)
		}
		consoleConfigValue, consoleConfigOrigin = config, origin
	})
	return consoleConfigValue, consoleConfigOrigin
}

func registerConsoleConfigRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/console/config", handleConsoleConfig)
	mux.HandleFunc("GET /api/console/view-profiles", handleConsoleViewProfiles)
}

func handleConsoleConfig(w http.ResponseWriter, _ *http.Request) {
	config, origin := consoleConfig()
	writeJSON(w, consoleConfigResponse{Origin: origin, Config: config})
}
