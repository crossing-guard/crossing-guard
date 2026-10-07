package daemon

// The console's writes to daemon.json and to the appearance families
// (session-view-and-console-preferences plan §B3, §C4). A selection write edits
// only the keys it owns, as a minimal overlay over the built-in defaults: it
// never writes the resolved configuration, so every other default keeps coming
// from the binary. The owner's hand-written keys keep their values; the file
// is re-written with sorted keys and two-space indentation.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"crossing-guard/harvest"
	"crossing-guard/internal/atomicfile"
	"crossing-guard/internal/filelock"
)

var (
	errConsoleConfigStale   = errors.New("daemon.json changed since it was read; reload and try again")
	errConsoleConfigInvalid = errors.New("the console configuration would be invalid")
)

// transcriptSelectionRequest is the body of PUT /api/console/config/transcript.
// A role set to null drops that role's overlay entry, so the built-in mapping
// applies again; null is never written. A built-in role mapping can be
// re-pointed, never removed.
type transcriptSelectionRequest struct {
	StateToken     string             `json:"state_token"`
	DefaultProfile *string            `json:"default_profile,omitempty"`
	RoleProfiles   map[string]*string `json:"role_profiles,omitempty"`
}

// appearanceSelectionRequest is the body of PUT /api/console/config/appearance.
type appearanceSelectionRequest struct {
	StateToken string `json:"state_token"`
	Module     string `json:"module"`
}

type transcriptOverlay struct {
	DefaultProfile string            `json:"default_profile,omitempty"`
	RoleProfiles   map[string]string `json:"role_profiles,omitempty"`
}

// mutateConsoleConfig applies one section change to daemon.json under its
// lock. The file is re-read, the caller's token must match it, and the result
// must parse with the changed section resolving before it replaces the file.
func mutateConsoleConfig(dataDir, expectedToken, section string, change func(map[string]json.RawMessage) error) error {
	path := consoleConfigPath(dataDir)
	err := filelock.With(path+".lock", 0o600, func() error {
		raw, err := readConsoleConfigFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			raw = nil
		} else if err != nil {
			return err
		}
		current := ""
		if raw != nil {
			current = moduleToken(raw)
		}
		if current != expectedToken {
			return errConsoleConfigStale
		}
		document := map[string]json.RawMessage{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &document); err != nil {
				return fmt.Errorf("%w: daemon.json cannot be read (%v); fix it first", errConsoleConfigInvalid, err)
			}
		}
		if err := change(document); err != nil {
			return err
		}
		body, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return err
		}
		body = append(body, '\n')
		_, problems, err := parseConsoleConfig(body, dataDir)
		if err != nil {
			return fmt.Errorf("%w: %v", errConsoleConfigInvalid, err)
		}
		for _, problem := range problems {
			if strings.HasPrefix(problem, section+".") {
				return fmt.Errorf("%w: %s", errConsoleConfigInvalid, strings.SplitN(problem, ";", 2)[0])
			}
		}
		return atomicfile.Write(path, body, 0o600)
	})
	if err == nil {
		invalidateConsoleConfig()
	}
	return err
}

func applyTranscriptSelection(document map[string]json.RawMessage, request transcriptSelectionRequest) error {
	overlay := transcriptOverlay{}
	if raw, ok := document["transcript"]; ok {
		if err := json.Unmarshal(raw, &overlay); err != nil {
			return fmt.Errorf("%w: transcript: %v", errConsoleConfigInvalid, err)
		}
	}
	defaults := defaultConsoleConfig().Transcript
	if request.DefaultProfile != nil {
		overlay.DefaultProfile = *request.DefaultProfile
		if overlay.DefaultProfile == defaults.DefaultProfile {
			overlay.DefaultProfile = ""
		}
	}
	roles := make([]string, 0, len(request.RoleProfiles))
	for role := range request.RoleProfiles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if !transcriptRoles[role] {
			return fmt.Errorf("%w: transcript.role_profiles.%s: role must be reviewer, follower or helper", errConsoleConfigInvalid, role)
		}
		if overlay.RoleProfiles == nil {
			overlay.RoleProfiles = map[string]string{}
		}
		value := request.RoleProfiles[role]
		if value == nil || *value == defaults.RoleProfiles[role] {
			delete(overlay.RoleProfiles, role)
			continue
		}
		overlay.RoleProfiles[role] = *value
	}
	if overlay.DefaultProfile == "" && len(overlay.RoleProfiles) == 0 {
		delete(document, "transcript")
		return nil
	}
	encoded, err := json.Marshal(overlay)
	if err != nil {
		return err
	}
	document["transcript"] = encoded
	return nil
}

func handleConsoleTranscriptSelectionPut(w http.ResponseWriter, r *http.Request) {
	var request transcriptSelectionRequest
	if !decodeConsoleWriteBody(w, r, &request) {
		return
	}
	err := mutateConsoleConfig(consoleDataDir(), request.StateToken, "transcript", func(document map[string]json.RawMessage) error {
		return applyTranscriptSelection(document, request)
	})
	if respondModuleWrite(w, err) {
		writeJSON(w, consoleConfigSnapshot())
	}
}

func handleConsoleAppearanceSelectionPut(w http.ResponseWriter, r *http.Request) {
	var request appearanceSelectionRequest
	if !decodeConsoleWriteBody(w, r, &request) {
		return
	}
	err := mutateConsoleConfig(consoleDataDir(), request.StateToken, "appearance", func(document map[string]json.RawMessage) error {
		if request.Module == "" || request.Module == defaultConsoleConfig().Appearance.Module {
			delete(document, "appearance")
			return nil
		}
		encoded, err := json.Marshal(ConsoleAppearanceSelection{Module: request.Module})
		document["appearance"] = encoded
		return err
	})
	if respondModuleWrite(w, err) {
		writeJSON(w, consoleConfigSnapshot())
	}
}

// ResolvedTheme is one theme in use, with what it inherits and which of its
// label keys name no registered runtime (kept, never rendered).
type ResolvedTheme struct {
	ThemeModule
	Origin        string   `json:"origin"`
	Builtin       bool     `json:"builtin"`
	StateToken    string   `json:"state_token"`
	Inherited     []string `json:"inherited"`
	UnknownLabels []string `json:"unknown_labels"`
}

// ResolvedAppearance is one appearance in use.
type ResolvedAppearance struct {
	AppearanceModule
	Origin     string `json:"origin"`
	Builtin    bool   `json:"builtin"`
	StateToken string `json:"state_token"`
}

// appearanceCatalogResponse is the typed body of GET /api/console/themes and
// GET /api/console/appearance: both families, so an editor reads one document.
type appearanceCatalogResponse struct {
	Themes      []ResolvedTheme      `json:"themes"`
	Appearances []ResolvedAppearance `json:"appearances"`
	Rejected    []ModuleRejection    `json:"rejected"`
	Limits      ConsoleLimits        `json:"limits"`
	Selected    string               `json:"selected"`
	// TypeSteps are the built-in reference sizes, so an editor can preview a
	// text size even when the selected appearance is an installed copy.
	TypeSteps []float64 `json:"type_steps"`
}

type themeWriteRequest struct {
	StateToken string      `json:"state_token"`
	Theme      ThemeModule `json:"theme"`
}

type appearanceWriteRequest struct {
	StateToken string           `json:"state_token"`
	Appearance AppearanceModule `json:"appearance"`
}

func registerAppearanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/console/themes", handleAppearanceCatalog)
	mux.HandleFunc("PUT /api/console/themes/{id}", handleThemePut)
	mux.HandleFunc("DELETE /api/console/themes/{id}", handleThemeDelete)
	mux.HandleFunc("GET /api/console/appearance", handleAppearanceCatalog)
	mux.HandleFunc("PUT /api/console/appearance/{id}", handleAppearancePut)
	mux.HandleFunc("DELETE /api/console/appearance/{id}", handleAppearanceDelete)
}

func appearanceCatalog(dataDir string) appearanceCatalogResponse {
	config, _ := consoleConfig()
	registered := map[string]bool{}
	for _, runtime := range harvest.RuntimeNames() {
		registered[runtime] = true
	}
	themes, rejected := themeFamily().load(dataDir)
	out := appearanceCatalogResponse{Themes: []ResolvedTheme{}, Appearances: []ResolvedAppearance{},
		Limits: config.Limits, Selected: config.Appearance.Module}
	for _, theme := range themes {
		resolved := ResolvedTheme{ThemeModule: theme.Module, Origin: theme.Origin, Builtin: theme.Builtin,
			StateToken: theme.StateToken, Inherited: []string{}, UnknownLabels: []string{}}
		for _, name := range themeTokens {
			if theme.Module.Tokens[name] == "" {
				resolved.Inherited = append(resolved.Inherited, name)
			}
		}
		for _, runtime := range sortedKeys(theme.Module.Labels) {
			if !registered[runtime] {
				resolved.UnknownLabels = append(resolved.UnknownLabels, runtime)
			}
		}
		out.Themes = append(out.Themes, resolved)
	}
	appearances, appearanceRejected := appearanceFamily(dataDir, config.Limits, false).load(dataDir)
	for _, module := range appearances {
		out.Appearances = append(out.Appearances, ResolvedAppearance{AppearanceModule: module.Module,
			Origin: module.Origin, Builtin: module.Builtin, StateToken: module.StateToken})
	}
	out.Rejected = append(rejected, appearanceRejected...)
	builtinAppearances, _ := appearanceFamily(dataDir, config.Limits, false).loadBuiltins()
	out.TypeSteps = builtinAppearances[defaultConsoleConfig().Appearance.Module].TypeSteps
	if out.TypeSteps == nil {
		out.TypeSteps = []float64{}
	}
	return out
}

func handleAppearanceCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, appearanceCatalog(consoleDataDir()))
}

func handleThemePut(w http.ResponseWriter, r *http.Request) {
	var request themeWriteRequest
	if !decodeConsoleWriteBody(w, r, &request) {
		return
	}
	if key := themeReschemeBlockedBy(r.PathValue("id"), request.Theme.Scheme); key != "" {
		http.Error(w, fmt.Sprintf("%v by %s under the other scheme; choose another theme there first", errModuleInUse, key), http.StatusConflict)
		return
	}
	if respondModuleWrite(w, themeFamily().put(consoleDataDir(), r.PathValue("id"), request.StateToken, request.Theme)) {
		handleAppearanceCatalog(w, r)
	}
}

func handleThemeDelete(w http.ResponseWriter, r *http.Request) {
	err := themeFamily().remove(consoleDataDir(), r.PathValue("id"), r.URL.Query().Get("state_token"), themeSelectedBy)
	if respondModuleWrite(w, err) {
		handleAppearanceCatalog(w, r)
	}
}

func handleAppearancePut(w http.ResponseWriter, r *http.Request) {
	var request appearanceWriteRequest
	if !decodeConsoleWriteBody(w, r, &request) {
		return
	}
	config, _ := consoleConfig()
	family := appearanceFamily(consoleDataDir(), config.Limits, true)
	if respondModuleWrite(w, family.put(consoleDataDir(), r.PathValue("id"), request.StateToken, request.Appearance)) {
		handleAppearanceCatalog(w, r)
	}
}

func handleAppearanceDelete(w http.ResponseWriter, r *http.Request) {
	config, _ := consoleConfig()
	family := appearanceFamily(consoleDataDir(), config.Limits, false)
	err := family.remove(consoleDataDir(), r.PathValue("id"), r.URL.Query().Get("state_token"), func(id string) string {
		if config.Appearance.Module == id {
			return "appearance.module"
		}
		return ""
	})
	if respondModuleWrite(w, err) {
		handleAppearanceCatalog(w, r)
	}
}

// themeReschemeBlockedBy names an installed appearance that uses this theme for
// the scheme it is about to leave: re-scheming it would silently swap that
// appearance's theme for the built-in.
func themeReschemeBlockedBy(id, scheme string) string {
	config, _ := consoleConfig()
	appearances, _ := appearanceFamily(consoleDataDir(), config.Limits, false).load(consoleDataDir())
	for _, module := range appearances {
		if (module.Module.ThemeDark == id && scheme != "dark") || (module.Module.ThemeLight == id && scheme != "light") {
			return "appearance " + module.Module.ID
		}
	}
	return ""
}

// themeSelectedBy names an installed appearance that references the theme.
func themeSelectedBy(id string) string {
	config, _ := consoleConfig()
	appearances, _ := appearanceFamily(consoleDataDir(), config.Limits, false).load(consoleDataDir())
	for _, module := range appearances {
		if module.Module.ThemeDark == id || module.Module.ThemeLight == id {
			return "appearance " + module.Module.ID
		}
	}
	return ""
}
