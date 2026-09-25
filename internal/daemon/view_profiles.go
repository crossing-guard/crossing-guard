package daemon

// Transcript view profiles (transcript-view-profiles plan). A view profile is a
// module: one self-contained JSON document whose rules decide whether each
// transcript row is shown, collapsed to one line, or hidden behind a count.
// Built-in modules are embedded in that same format; a module installed at
// <dataDir>/view-profiles/<id>.json replaces the built-in of its id whole, and a
// new id adds a module. A module never names who uses it: which module a session
// opens with is selection, and lives in console.json.

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	viewProfileFormatVersion = 1
	// viewProfileReadLimit bounds one module; a rule list is a few hundred bytes.
	viewProfileReadLimit = 64 << 10
	viewProfileMaxRules  = 64
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
	// MinChars holds for rows whose full text is at least this long.
	MinChars int `json:"min_chars,omitempty"`
	// Tool holds for tool rows of this tool name.
	Tool string `json:"tool,omitempty"`
}

// ResolvedViewProfile is a module in use and where it came from: "builtin" or
// the installed file's path.
type ResolvedViewProfile struct {
	ViewProfile
	Origin string `json:"origin"`
}

// ViewProfileRejection is a module file that failed and was skipped.
type ViewProfileRejection struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// viewProfilesResponse is the typed body of GET /api/console/view-profiles.
type viewProfilesResponse struct {
	Profiles []ResolvedViewProfile  `json:"profiles"`
	Rejected []ViewProfileRejection `json:"rejected"`
}

var (
	viewRowKinds = map[string]bool{"user": true, "assistant": true, "thinking": true, "tool_call": true,
		"tool_result": true, "summary": true, "system": true, "context": true, "other": true}
	viewDisplays  = map[string]bool{"show": true, "collapse": true, "hide": true}
	viewProfileID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

func viewProfilesDir(dataDir string) string { return filepath.Join(dataDir, "view-profiles") }

// LoadViewProfiles resolves every module: the built-ins, then each installed
// file, which replaces the built-in of its id or adds a module. A file that
// fails is reported and skipped, so a built-in of that id stays in use.
func LoadViewProfiles(dataDir string) ([]ResolvedViewProfile, []ViewProfileRejection) {
	byID := map[string]ResolvedViewProfile{}
	rejected := []ViewProfileRejection{}
	builtins, _ := fs.Glob(builtinViewProfiles, "view-profiles/*.json")
	for _, name := range builtins {
		profile, err := readViewProfile(builtinViewProfiles.Open, name)
		if err != nil {
			rejected = append(rejected, ViewProfileRejection{Path: "builtin:" + path.Base(name), Error: err.Error()})
			continue
		}
		byID[profile.ID] = ResolvedViewProfile{ViewProfile: profile, Origin: "builtin"}
	}
	installed, _ := filepath.Glob(filepath.Join(viewProfilesDir(dataDir), "*.json"))
	for _, file := range installed {
		profile, err := readViewProfile(func(name string) (fs.File, error) { return os.Open(name) }, file)
		if err != nil {
			rejected = append(rejected, ViewProfileRejection{Path: file, Error: err.Error()})
			continue
		}
		byID[profile.ID] = ResolvedViewProfile{ViewProfile: profile, Origin: file}
	}
	profiles := make([]ResolvedViewProfile, 0, len(byID))
	for _, profile := range byID {
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return profiles, rejected
}

// readViewProfile decodes one module strictly and requires its id to be its
// file name, so the file that replaces a module is always the one named for it.
func readViewProfile(open func(string) (fs.File, error), name string) (ViewProfile, error) {
	file, err := open(name)
	if err != nil {
		return ViewProfile{}, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, viewProfileReadLimit))
	decoder.DisallowUnknownFields()
	var profile ViewProfile
	if err := decoder.Decode(&profile); err != nil {
		return ViewProfile{}, fmt.Errorf("decode: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ViewProfile{}, errors.New("decode: trailing JSON data")
	}
	if err := profile.validate(); err != nil {
		return ViewProfile{}, err
	}
	if stem := strings.TrimSuffix(path.Base(filepath.ToSlash(name)), ".json"); stem != profile.ID {
		return ViewProfile{}, fmt.Errorf("id %q must match the file name %q", profile.ID, stem)
	}
	if profile.Rules == nil {
		profile.Rules = []ViewRule{}
	}
	return profile, nil
}

func (p ViewProfile) validate() error {
	if p.FormatVersion != viewProfileFormatVersion {
		return fmt.Errorf("unsupported format_version %d", p.FormatVersion)
	}
	if !viewProfileID.MatchString(p.ID) {
		return fmt.Errorf("id %q must be lowercase letters, digits and hyphens", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name must be set")
	}
	if len(p.Rules) > viewProfileMaxRules {
		return fmt.Errorf("at most %d rules", viewProfileMaxRules)
	}
	for i, rule := range p.Rules {
		if !viewDisplays[rule.Display] {
			return fmt.Errorf("rules[%d].display %q must be show, collapse or hide", i, rule.Display)
		}
		if rule.Match.Kind != "" && !viewRowKinds[rule.Match.Kind] {
			return fmt.Errorf("rules[%d].match.kind %q is not a transcript row kind", i, rule.Match.Kind)
		}
		if rule.Match.MinChars < 0 {
			return fmt.Errorf("rules[%d].match.min_chars must be zero or positive", i)
		}
	}
	return nil
}

// validateTranscriptSelection requires every module console.json selects to
// resolve, so a typo in an id is a refused file rather than a silent full view.
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

// Modules are read on every request: dropping a file into view-profiles/ takes
// effect at the browser's next load, with no daemon restart.
func handleConsoleViewProfiles(w http.ResponseWriter, _ *http.Request) {
	profiles, rejected := LoadViewProfiles(filepath.Dir(indexPath()))
	writeJSON(w, viewProfilesResponse{Profiles: profiles, Rejected: rejected})
}
