package daemon

// Transcript view profiles (transcript-view-profiles plan; format v2 and the
// console writer in session-view-and-console-preferences plan §B). A view
// profile is a module: one self-contained JSON document whose rules decide
// whether each transcript row is shown, collapsed to one line, or hidden behind
// a count. Built-in modules are embedded in that same format; a module installed
// at <dataDir>/view-profiles/<id>.json replaces the built-in of its id whole,
// and a new id adds a module. A module never names who uses it: which module a
// session opens with is selection, and lives in console.json.

import (
	"embed"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// Format 1 is the original rule vocabulary; format 2 adds fact and
	// max_chars and refuses a collapse that cannot take effect.
	viewProfileFormatV1 = 1
	viewProfileFormatV2 = 2
	viewProfileMaxRules = 64
)

//go:embed view-profiles/*.json
var builtinViewProfiles embed.FS

// ViewProfile is one module as written.
type ViewProfile struct {
	FormatVersion int    `json:"format_version"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	// Rules are tried in order; the first whose match holds decides the row,
	// and a row no rule matches is shown.
	Rules []ViewRule `json:"rules"`
}

type ViewRule struct {
	Match ViewMatch `json:"match"`
	// Display is show, collapse (one line that opens) or hide (counted, never silent).
	Display string `json:"display"`
}

// ViewMatch holds when every field it sets holds; an empty match holds for every row.
type ViewMatch struct {
	// Kind is a transcript row kind (harvest.CanonicalEvent.Kind).
	Kind string `json:"kind,omitempty"`
	// MinChars and MaxChars bound the row's full text length.
	MinChars int `json:"min_chars,omitempty"`
	MaxChars int `json:"max_chars,omitempty"`
	// Tool holds for tool rows of this exact tool name.
	Tool string `json:"tool,omitempty"`
	// Fact holds for tool rows the detector library classifies with this
	// key:value fact (exec:run, fs:edit, …). Checked by syntax only: the
	// detector library is layered and owner-overridable, so a fact no current
	// detector emits is a rule that matches nothing, not an invalid module.
	Fact string `json:"fact,omitempty"`
}

// ResolvedViewProfile is a module in use and where it came from: "builtin" or
// the installed file's path. Builtin says a built-in of this id exists, so
// deleting the installed file reverts rather than removes; StateToken is the
// installed file's token ("" when none is installed).
type ResolvedViewProfile struct {
	ViewProfile
	Origin     string `json:"origin"`
	Builtin    bool   `json:"builtin"`
	StateToken string `json:"state_token"`
}

// viewProfilesResponse is the typed body of GET /api/console/view-profiles.
type viewProfilesResponse struct {
	Profiles []ResolvedViewProfile  `json:"profiles"`
	Rejected []ViewProfileRejection `json:"rejected"`
}

// viewProfileWriteRequest is the body of PUT /api/console/view-profiles/{id}.
type viewProfileWriteRequest struct {
	StateToken string      `json:"state_token"`
	Profile    ViewProfile `json:"profile"`
}

var (
	viewRowKinds = map[string]bool{"user": true, "assistant": true, "thinking": true, "tool_call": true,
		"tool_result": true, "summary": true, "system": true, "context": true, "other": true}
	viewDisplays = map[string]bool{"show": true, "collapse": true, "hide": true}
	// viewChipKinds already render as one line that opens, so a v2 collapse
	// rule on them would silently do nothing.
	viewChipKinds = map[string]bool{"tool_call": true, "tool_result": true, "thinking": true}
	viewFact      = regexp.MustCompile(`^[a-z][a-z0-9_-]*:[a-z0-9][a-z0-9_.-]*$`)
)

var viewProfileFamily = moduleFamily[ViewProfile]{
	subdir:   "view-profiles",
	builtins: builtinViewProfiles,
	glob:     "view-profiles/*.json",
	decode:   decodeViewProfile,
	id:       func(p ViewProfile) string { return p.ID },
}

func decodeViewProfile(data []byte, _ bool) (ViewProfile, error) {
	var profile ViewProfile
	if err := decodeModuleStrict(data, &profile); err != nil {
		return ViewProfile{}, err
	}
	if err := profile.validate(); err != nil {
		return ViewProfile{}, err
	}
	if profile.Rules == nil {
		profile.Rules = []ViewRule{}
	}
	return profile, nil
}

// LoadViewProfiles resolves every module: the built-ins, then each installed
// file, which replaces the built-in of its id or adds a module. A file that
// fails is reported and skipped, so a built-in of that id stays in use.
func LoadViewProfiles(dataDir string) ([]ResolvedViewProfile, []ViewProfileRejection) {
	modules, rejected := viewProfileFamily.load(dataDir)
	profiles := make([]ResolvedViewProfile, 0, len(modules))
	for _, module := range modules {
		profiles = append(profiles, ResolvedViewProfile{ViewProfile: module.Module, Origin: module.Origin,
			Builtin: module.Builtin, StateToken: module.StateToken})
	}
	return profiles, rejected
}

func (p ViewProfile) validate() error {
	if p.FormatVersion != viewProfileFormatV1 && p.FormatVersion != viewProfileFormatV2 {
		return fmt.Errorf("unsupported format_version %d", p.FormatVersion)
	}
	if !moduleIDPattern.MatchString(p.ID) {
		return fmt.Errorf("id %q must be lowercase letters, digits and hyphens", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name must be set")
	}
	if len(p.Rules) > viewProfileMaxRules {
		return fmt.Errorf("at most %d rules", viewProfileMaxRules)
	}
	for i, rule := range p.Rules {
		if err := rule.validate(p.FormatVersion); err != nil {
			return fmt.Errorf("rules[%d].%w", i, err)
		}
	}
	return nil
}

func (r ViewRule) validate(format int) error {
	match := r.Match
	if !viewDisplays[r.Display] {
		return fmt.Errorf("display %q must be show, collapse or hide", r.Display)
	}
	if match.Kind != "" && !viewRowKinds[match.Kind] {
		return fmt.Errorf("match.kind %q is not a transcript row kind", match.Kind)
	}
	if match.MinChars < 0 || match.MaxChars < 0 {
		return errors.New("match.min_chars and match.max_chars must be zero or positive")
	}
	if format == viewProfileFormatV1 {
		if match.Fact != "" || match.MaxChars != 0 {
			return errors.New("match.fact and match.max_chars need format_version 2")
		}
		return nil
	}
	if match.MaxChars > 0 && match.MinChars > match.MaxChars {
		return errors.New("match.min_chars must not exceed match.max_chars")
	}
	if match.Tool != "" && strings.TrimSpace(match.Tool) == "" {
		return errors.New("match.tool must not be blank")
	}
	if match.Fact != "" && !viewFact.MatchString(match.Fact) {
		return fmt.Errorf("match.fact %q must be key:value, like exec:run", match.Fact)
	}
	if r.Display == "collapse" && viewChipKinds[match.Kind] {
		return fmt.Errorf("display collapse has no effect on %s rows, which are already one line", match.Kind)
	}
	return nil
}

// validateTranscriptSelection requires every module console.json selects to
// resolve, so a typo in an id is a refused section rather than a silent full view.
func validateTranscriptSelection(selection ConsoleTranscriptDefaults, dataDir string) error {
	profiles, _ := LoadViewProfiles(dataDir)
	known := map[string]bool{}
	for _, profile := range profiles {
		known[profile.ID] = true
	}
	if !known[selection.DefaultProfile] {
		return fmt.Errorf("transcript.default_profile %q is not a view profile", selection.DefaultProfile)
	}
	roles := make([]string, 0, len(selection.RoleProfiles))
	for role := range selection.RoleProfiles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if !known[selection.RoleProfiles[role]] {
			return fmt.Errorf("transcript.role_profiles.%s %q is not a view profile", role, selection.RoleProfiles[role])
		}
	}
	return nil
}

// transcriptSelectedBy names the console.json key that selects a view profile.
func transcriptSelectedBy(id string) string {
	config, _ := consoleConfig()
	if config.Transcript.DefaultProfile == id {
		return "transcript.default_profile"
	}
	roles := make([]string, 0, len(config.Transcript.RoleProfiles))
	for role := range config.Transcript.RoleProfiles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if config.Transcript.RoleProfiles[role] == id {
			return "transcript.role_profiles." + role
		}
	}
	return ""
}

func consoleDataDir() string { return filepath.Dir(indexPath()) }

// Modules are read on every request: dropping a file into view-profiles/ takes
// effect at the browser's next load, with no daemon restart.
func handleConsoleViewProfiles(w http.ResponseWriter, _ *http.Request) {
	profiles, rejected := LoadViewProfiles(consoleDataDir())
	writeJSON(w, viewProfilesResponse{Profiles: profiles, Rejected: rejected})
}

func handleConsoleViewProfilePut(w http.ResponseWriter, r *http.Request) {
	var request viewProfileWriteRequest
	if !decodeConsoleWriteBody(w, r, &request) {
		return
	}
	err := viewProfileFamily.put(consoleDataDir(), r.PathValue("id"), request.StateToken, request.Profile)
	if respondModuleWrite(w, err) {
		handleConsoleViewProfiles(w, r)
	}
}

func handleConsoleViewProfileDelete(w http.ResponseWriter, r *http.Request) {
	err := viewProfileFamily.remove(consoleDataDir(), r.PathValue("id"), r.URL.Query().Get("state_token"), transcriptSelectedBy)
	if respondModuleWrite(w, err) {
		handleConsoleViewProfiles(w, r)
	}
}
