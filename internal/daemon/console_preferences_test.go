package daemon

// Tests for the console preference modules and their writers
// (session-view-and-console-preferences plan §12).

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
)

func tokenOf(t *testing.T, path string) string {
	t.Helper()
	token, err := installedToken(path)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func withConsoleDataDir(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	previous := resolvedIndexPath
	setIndexPath(dataDir)
	invalidateConsoleConfig()
	t.Cleanup(func() { resolvedIndexPath = previous; invalidateConsoleConfig() })
	return dataDir
}

func TestViewProfileFormatTwoRules(t *testing.T) {
	good := `{"format_version":2,"id":"x","name":"x","rules":[{"match":{"fact":"exec:run","max_chars":900},"display":"hide"}]}`
	if _, err := decodeViewProfile([]byte(good), false); err != nil {
		t.Fatalf("v2 fact and max_chars refused: %v", err)
	}
	for body, want := range map[string]string{
		`{"format_version":1,"id":"x","name":"x","rules":[{"match":{"fact":"exec:run"},"display":"hide"}]}`:           "format_version 2",
		`{"format_version":2,"id":"x","name":"x","rules":[{"match":{"kind":"tool_call"},"display":"collapse"}]}`:      "no effect",
		`{"format_version":2,"id":"x","name":"x","rules":[{"match":{"kind":"thinking"},"display":"collapse"}]}`:       "no effect",
		`{"format_version":2,"id":"x","name":"x","rules":[{"match":{"fact":"exec run"},"display":"hide"}]}`:           "key:value",
		`{"format_version":2,"id":"x","name":"x","rules":[{"match":{"tool":"  "},"display":"hide"}]}`:                 "blank",
		`{"format_version":2,"id":"x","name":"x","rules":[{"match":{"min_chars":9,"max_chars":3},"display":"hide"}]}`: "exceed",
	} {
		if _, err := decodeViewProfile([]byte(body), false); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: want %q, got %v", body, want, err)
		}
	}
}

func TestModuleWriterGuards(t *testing.T) {
	dataDir := t.TempDir()
	profile := ViewProfile{FormatVersion: 2, ID: "quiet", Name: "Quiet", Rules: []ViewRule{{Match: ViewMatch{Kind: "thinking"}, Display: "hide"}}}
	if err := viewProfileFamily.put(dataDir, "quiet", "", profile); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := viewProfileFamily.file(dataDir, "quiet")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("installed file mode: %v %v", info, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(viewProfileFamily.dir(dataDir), ".atomic-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	if err := viewProfileFamily.put(dataDir, "quiet", "", profile); !errors.Is(err, errModuleStale) {
		t.Fatalf("a stale token must be refused: %v", err)
	}
	if err := viewProfileFamily.put(dataDir, "other", "", profile); !errors.Is(err, errModuleInvalid) {
		t.Fatalf("a body id unlike the route id must be refused: %v", err)
	}
	if err := viewProfileFamily.put(dataDir, "../escape", "", profile); !errors.Is(err, errModuleInvalid) {
		t.Fatalf("a path-shaped id must be refused before any join: %v", err)
	}
	bad := profile
	bad.Rules = []ViewRule{{Match: ViewMatch{Kind: "tool_call"}, Display: "collapse"}}
	if err := viewProfileFamily.put(dataDir, "quiet", tokenOf(t, path), bad); !errors.Is(err, errModuleInvalid) {
		t.Fatalf("an invalid module must be refused before writing: %v", err)
	}
	if err := viewProfileFamily.remove(dataDir, "absent", "", func(string) string { return "" }); !errors.Is(err, errModuleAbsent) {
		t.Fatalf("deleting an absent module is a not-found: %v", err)
	}
	selected := func(id string) string { return "transcript.default_profile" }
	if err := viewProfileFamily.remove(dataDir, "quiet", tokenOf(t, path), selected); !errors.Is(err, errModuleInUse) {
		t.Fatalf("a selected module with no built-in must not be deleted: %v", err)
	}
	if err := viewProfileFamily.remove(dataDir, "quiet", tokenOf(t, path), func(string) string { return "" }); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// A built-in id may be deleted even when selected: the built-in applies again.
	override := ViewProfile{FormatVersion: 1, ID: "full", Name: "Mine", Rules: []ViewRule{}}
	if err := viewProfileFamily.put(dataDir, "full", "", override); err != nil {
		t.Fatal(err)
	}
	if err := viewProfileFamily.remove(dataDir, "full", tokenOf(t, viewProfileFamily.file(dataDir, "full")), selected); err != nil {
		t.Fatalf("reverting a built-in must be allowed: %v", err)
	}
}

func TestModuleRoutesStatusCodes(t *testing.T) {
	withConsoleDataDir(t)
	put := func(id, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/api/console/view-profiles/"+id, strings.NewReader(body))
		request.SetPathValue("id", id)
		recorder := httptest.NewRecorder()
		handleConsoleViewProfilePut(recorder, request)
		return recorder
	}
	if code := put("quiet", `{"state_token":"","profile":{"format_version":2,"id":"quiet","name":"Quiet","rules":[]}}`).Code; code != http.StatusOK {
		t.Fatalf("create: %d", code)
	}
	if code := put("quiet", `{"state_token":"","profile":{"format_version":2,"id":"quiet","name":"Quiet","rules":[]}}`).Code; code != http.StatusConflict {
		t.Fatalf("stale: %d", code)
	}
	if code := put("bad", `{"state_token":"","profile":{"format_version":2,"id":"bad","name":""}}`).Code; code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid: %d", code)
	}
	if code := put("bad", `{"state_token":"","profile":{},"extra":1}`).Code; code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/console/view-profiles/nothing", nil)
	request.SetPathValue("id", "nothing")
	recorder := httptest.NewRecorder()
	handleConsoleViewProfileDelete(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("absent delete: %d", recorder.Code)
	}
}

func putTranscriptSelection(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handleConsoleTranscriptSelectionPut(recorder, httptest.NewRequest(http.MethodPut, "/api/console/config/transcript", strings.NewReader(body)))
	return recorder
}

func TestTranscriptSelectionOverlay(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	hand := `{"format_version":1,"recently_closed":5}` + "\n"
	if err := os.WriteFile(consoleConfigPath(dataDir), []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	token := moduleToken([]byte(hand))
	recorder := putTranscriptSelection(t, `{"state_token":"`+token+`","role_profiles":{"helper":"conversation"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-point: %d %s", recorder.Code, recorder.Body.String())
	}
	raw, _ := os.ReadFile(consoleConfigPath(dataDir))
	var written map[string]json.RawMessage
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	var overlay transcriptOverlay
	_ = json.Unmarshal(written["transcript"], &overlay)
	if string(written["recently_closed"]) != "5" || overlay.DefaultProfile != "" || len(overlay.RoleProfiles) != 1 || overlay.RoleProfiles["helper"] != "conversation" {
		t.Fatalf("the overlay must keep hand keys and write only its own: %s", raw)
	}
	if _, present := written["diff"]; present {
		t.Fatalf("the resolved config must never be written: %s", raw)
	}
	// The write applies at once, with no restart.
	config, _ := consoleConfig()
	if config.Transcript.RoleProfiles["helper"] != "conversation" || config.Transcript.RoleProfiles["follower"] != "agent-review" {
		t.Fatalf("selection not live: %+v", config.Transcript)
	}
	// null drops the overlay entry, so the built-in mapping applies; null is never written.
	recorder = putTranscriptSelection(t, `{"state_token":"`+moduleToken(raw)+`","role_profiles":{"helper":null}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", recorder.Code, recorder.Body.String())
	}
	raw, _ = os.ReadFile(consoleConfigPath(dataDir))
	if bytes.Contains(raw, []byte("null")) || bytes.Contains(raw, []byte("transcript")) {
		t.Fatalf("null must drop the key, never be written: %s", raw)
	}
	if config, _ := consoleConfig(); config.Transcript.RoleProfiles["helper"] != "agent-review" {
		t.Fatalf("built-in mapping not restored: %+v", config.Transcript)
	}
	// Full is now an explicit owner choice, while a missing overlay opens Conversation.
	if config, _ := consoleConfig(); config.Transcript.DefaultProfile != "conversation" {
		t.Fatalf("missing overlay did not use the new built-in: %+v", config.Transcript)
	}
	recorder = putTranscriptSelection(t, `{"state_token":"`+moduleToken(raw)+`","default_profile":"full"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("explicit Full selection: %d %s", recorder.Code, recorder.Body.String())
	}
	raw, _ = os.ReadFile(consoleConfigPath(dataDir))
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(written["transcript"], &overlay); err != nil || overlay.DefaultProfile != "full" {
		t.Fatalf("Full owner choice was not persisted: %s", raw)
	}
	if config, _ := consoleConfig(); config.Transcript.DefaultProfile != "full" || config.Transcript.RoleProfiles["follower"] != "agent-review" {
		t.Fatalf("explicit Full or role default lost: %+v", config.Transcript)
	}
	if code := putTranscriptSelection(t, `{"state_token":"stale","default_profile":"conversation"}`).Code; code != http.StatusConflict {
		t.Fatalf("stale token: %d", code)
	}
	if code := putTranscriptSelection(t, `{"state_token":"`+moduleToken(raw)+`","default_profile":"missing"}`).Code; code != http.StatusUnprocessableEntity {
		t.Fatalf("an unresolved selection must be refused: %d", code)
	}
}

func TestConsoleConfigReloadAndLastGood(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	path := consoleConfigPath(dataDir)
	if err := os.WriteFile(path, []byte(`{"format_version":1,"recently_closed":5,"console_reload_check_ms":250}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if config, origin := consoleConfig(); config.RecentlyClosed != 5 || origin != path {
		t.Fatalf("first load: %d %s", config.RecentlyClosed, origin)
	}
	// A hand edit that breaks the file keeps the last good value.
	if err := os.WriteFile(path, []byte(`{"format_version":1,"recently_closed":`), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, later, later)
	time.Sleep(300 * time.Millisecond)
	snapshot := consoleConfigSnapshot()
	if snapshot.Origin != "last-good" || snapshot.Config.RecentlyClosed != 5 || len(snapshot.Problems) != 1 {
		t.Fatalf("a broken hand edit must keep the last good value: %+v %v", snapshot.Origin, snapshot.Problems)
	}
	// A valid hand edit is picked up by the stat check.
	if err := os.WriteFile(path, []byte(`{"format_version":1,"recently_closed":7,"console_reload_check_ms":250}`), 0o600); err != nil {
		t.Fatal(err)
	}
	later = later.Add(2 * time.Second)
	_ = os.Chtimes(path, later, later)
	time.Sleep(300 * time.Millisecond)
	if config, origin := consoleConfig(); config.RecentlyClosed != 7 || origin != path {
		t.Fatalf("hand edit not picked up: %d %s", config.RecentlyClosed, origin)
	}
}

func TestConsoleConfigBoundsAndLimits(t *testing.T) {
	dataDir := t.TempDir()
	for body, want := range map[string]string{
		`{"format_version":1,"console_reload_check_ms":10}`: "console_reload_check_ms",
		`{"format_version":1,"console_write_bytes_max":10}`: "console_write_bytes_max",
		`{"format_version":1,"console_limits":{"text_size_min":4,"text_size_max":20,"width_min":560,"width_max":1600,"font_item_chars_max":64,"font_items_max":8}}`: "text sizes",
		// Narrowing the limits until the built-in appearance falls outside is refused.
		`{"format_version":1,"console_limits":{"text_size_min":15,"text_size_max":20,"width_min":560,"width_max":1600,"font_item_chars_max":64,"font_items_max":8}}`: "exclude built-in appearance",
	} {
		if _, _, err := parseConsoleConfig([]byte(body), dataDir); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: want %q, got %v", body, want, err)
		}
	}
}

func TestThemeModules(t *testing.T) {
	dataDir := t.TempDir()
	builtins, rejected := themeFamily().loadBuiltins()
	if len(rejected) != 0 || builtins["dark"].Scheme != "dark" || builtins["light"].Scheme != "light" || builtins["high-contrast"].Scheme != "dark" {
		t.Fatalf("built-in themes: %+v %+v", builtins, rejected)
	}
	partial := ThemeModule{FormatVersion: 1, ID: "mine", Name: "Mine", Scheme: "dark", Tokens: map[string]string{"accent": "#ff8800"},
		Labels: map[string]ThemeLabel{"st-draft": {Bg: "#000000", Fg: "#ffffff"}}}
	if err := themeFamily().put(dataDir, "mine", "", partial); err != nil {
		t.Fatalf("a partial theme inherits the rest: %v", err)
	}
	for body, want := range map[string]string{
		`{"format_version":1,"id":"x","name":"x","scheme":"dark","tokens":{"glow":"#ffffff"}}`: "not a theme token",
		`{"format_version":1,"id":"x","name":"x","scheme":"dark","tokens":{"bg":"red"}}`:       "#rgb",
		`{"format_version":1,"id":"x","name":"x","scheme":"sepia","tokens":{}}`:                "scheme",
	} {
		if _, err := decodeTheme([]byte(body), false); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: want %q, got %v", body, want, err)
		}
	}
	catalog := appearanceCatalog(dataDir)
	var mine ResolvedTheme
	for _, theme := range catalog.Themes {
		if theme.ID == "mine" {
			mine = theme
		}
	}
	if len(mine.Inherited) != len(themeTokens)-1 || len(mine.UnknownLabels) != 1 || mine.UnknownLabels[0] != "st-draft" {
		t.Fatalf("inheritance and unknown labels must be reported: %+v", mine)
	}
}

func TestAppearanceModules(t *testing.T) {
	dataDir := t.TempDir()
	limits := defaultConsoleConfig().Limits
	family := appearanceFamily(dataDir, limits, true)
	builtin, _ := family.loadBuiltins()
	base := builtin["default"]
	base.TypeSteps = nil
	base.ID, base.Name, base.TextSize = "mine", "Mine", 16
	if err := family.put(dataDir, "mine", "", base); err != nil {
		t.Fatalf("a valid appearance: %v", err)
	}
	for mutate, want := range map[string]func(*AppearanceModule){
		"text_size must be between":  func(a *AppearanceModule) { a.TextSize = 40 },
		"type_steps is set only":     func(a *AppearanceModule) { a.TypeSteps = []float64{1} },
		"is not a dark theme":        func(a *AppearanceModule) { a.ThemeDark = "light" },
		"pinned_scheme must be dark": func(a *AppearanceModule) { a.FollowOS = false },
		"font name":                  func(a *AppearanceModule) { a.UIFont = `Inter; } body { color: red` },
		"a font stack holds":         func(a *AppearanceModule) { a.MonoFont = "" },
	} {
		candidate := base
		want(&candidate)
		candidate.ID = "probe"
		if err := family.put(dataDir, "probe", "", candidate); err == nil || !strings.Contains(err.Error(), mutate) {
			t.Fatalf("want %q, got %v", mutate, err)
		}
	}
	// limits are installation policy, never a module field.
	if _, err := family.decode([]byte(`{"format_version":1,"id":"x","name":"x","limits":{}}`), false); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("limits in a module must be refused: %v", err)
	}
	stack, err := normalizeFontStack(`-apple-system, 'Segoe UI', "Inter", sans-serif`, limits)
	if err != nil || stack != `-apple-system, "Segoe UI", "Inter", sans-serif` {
		t.Fatalf("font stack re-serialisation: %q %v", stack, err)
	}
}

func appearanceCSS(t *testing.T, host, fetchSite string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/appearance.css", nil)
	request.Host = host
	if fetchSite != "" {
		request.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	recorder := httptest.NewRecorder()
	appearanceCSSHandler("127.0.0.1:7777")(recorder, request)
	return recorder
}

func TestAppearanceSheet(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	recorder := appearanceCSS(t, "localhost:7777", "same-origin")
	sheet := recorder.Body.String()
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" ||
		recorder.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("headers: %d %v", recorder.Code, recorder.Header())
	}
	// The default appearance reproduces today's sizes exactly.
	for _, want := range []string{"--fs-1: 7px;", "--fs-5: 10.5px;", "--fs-15: 20px;", "--measure: 880px;", "--chat-measure: 860px;",
		"@media (prefers-color-scheme: light) {", ":root:root:root {", "--bg: #16161a;", "--bg: #f4f4f2;"} {
		if !strings.Contains(sheet, want) {
			t.Fatalf("default sheet misses %q:\n%s", want, sheet)
		}
	}
	for host, site := range map[string]string{"evil.example:7777": "", "127.0.0.1:7777": "cross-site", "127.0.0.1:8888": ""} {
		if code := appearanceCSS(t, host, site).Code; code != http.StatusForbidden {
			t.Fatalf("host %s site %q: %d", host, site, code)
		}
	}
	// A pinned built-in scheme emits its whole token set unconditionally, so
	// the OS light block cannot show through.
	limits := defaultConsoleConfig().Limits
	family := appearanceFamily(dataDir, limits, true)
	builtin, _ := family.loadBuiltins()
	pinned := builtin["default"]
	pinned.TypeSteps, pinned.ID, pinned.Name, pinned.FollowOS, pinned.PinnedScheme = nil, "pinned", "Pinned", false, "dark"
	pinned.TextSize = 16
	if err := family.put(dataDir, "pinned", "", pinned); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consoleConfigPath(dataDir), []byte(`{"format_version":1,"appearance":{"module":"pinned"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateConsoleConfig()
	sheet = appearanceCSS(t, "127.0.0.1:7777", "").Body.String()
	if strings.Contains(sheet, "@media") || !strings.Contains(sheet, "color-scheme: dark;") || !strings.Contains(sheet, "--chip-claude-bg: #3d2c24;") {
		t.Fatalf("pinned sheet:\n%s", sheet)
	}
	if !strings.Contains(sheet, "--fs-11: 16px;") || !strings.Contains(sheet, "--fs-1: "+strconv.FormatFloat(math.Round(7*16/14.0*100)/100, 'f', -1, 64)+"px;") {
		t.Fatalf("text size must scale every step:\n%s", sheet)
	}
}

// A selected appearance whose installed file is broken on disk degrades to the
// built-in at request time; the console never renders nothing.
func TestAppearanceSheetDegrades(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	if err := os.MkdirAll(filepath.Join(dataDir, "appearance"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "appearance", "default.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	sheet := appearanceCSS(t, "127.0.0.1:7777", "").Body.String()
	if !strings.Contains(sheet, "--fs-11: 14px;") {
		t.Fatalf("a broken installed appearance must fall back to the built-in:\n%s", sheet)
	}
}

func relativeLuminance(hex string) float64 {
	value, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	channel := func(c uint64) float64 {
		v := float64(c) / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(value>>16) + 0.7152*channel((value>>8)&255) + 0.0722*channel(value&255)
}

func contrastRatio(a, b string) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Built-in themes meet WCAG AA (4.5:1) for body text and the accent pair.
func TestBuiltinThemesMeetContrast(t *testing.T) {
	builtins, _ := themeFamily().loadBuiltins()
	for id, theme := range builtins {
		for _, pair := range [][2]string{{"text", "bg"}, {"text", "panel"}, {"accent-ink", "accent"}} {
			if ratio := contrastRatio(theme.Tokens[pair[0]], theme.Tokens[pair[1]]); ratio < 4.5 {
				t.Fatalf("theme %s: %s on %s is %.2f:1", id, pair[0], pair[1], ratio)
			}
		}
	}
}

// GET /api/session rows carry facts on tool_call rows only; audit's session
// tags are exactly the union of the per-row classification it extracted.
func TestTranscriptFacts(t *testing.T) {
	detectors, _ := engine.DefaultDetectors()
	previous := auditDetectors
	if err := initAudit(detectors); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { auditDetectors = previous })
	events := []harvest.CanonicalEvent{
		{Seq: 1, Kind: "user", Text: "run the tests"},
		{Seq: 2, Kind: "tool_call", Name: "Bash", Text: `{"command":"go test ./..."}`},
		{Seq: 3, Kind: "tool_result", Text: "ok"},
		{Seq: 4, Kind: "tool_call", Name: "Grep", Text: `{"pattern":"x"}`},
	}
	rows := withTranscriptFacts(events)
	if len(rows) != 4 || len(rows[0].Facts) != 0 || len(rows[2].Facts) != 0 {
		t.Fatalf("only tool_call rows carry facts: %+v", rows)
	}
	has := func(facts []string, want string) bool {
		for _, fact := range facts {
			if fact == want {
				return true
			}
		}
		return false
	}
	if !has(rows[1].Facts, "exec:run") || !has(rows[3].Facts, "search:code") {
		t.Fatalf("facts: %v / %v", rows[1].Facts, rows[3].Facts)
	}
	seen := map[string]bool{}
	for _, fact := range rows[1].Facts {
		if seen[fact] {
			t.Fatalf("duplicate fact %q", fact)
		}
		seen[fact] = true
	}
	// The audit extraction must not change what audit sees. The golden below
	// was captured from sessionTags at ce3a5c4, before classifyTranscriptEvent
	// existed, over the fixture in auditGoldenEvents.
	got := []string{}
	for _, tag := range sessionTags(&SessionDetail{SessionDetail: harvest.SessionDetail{Events: auditGoldenEvents}}) {
		got = append(got, tag.Key+"|"+tag.Value+"|"+tag.Detector)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(auditGolden, "\n") {
		t.Fatalf("audit classification changed:\ngot  %q\nwant %q", got, auditGolden)
	}
}

var auditGoldenEvents = []harvest.CanonicalEvent{
	{Seq: 1, Kind: "user", Text: "please run the tests and push"},
	{Seq: 2, Kind: "tool_call", Name: "Bash", Text: `{"command":"go test ./... && git push origin main"}`},
	{Seq: 3, Kind: "tool_result", Text: "ok"},
	{Seq: 4, Kind: "tool_call", Name: "Grep", Text: `{"pattern":"TODO","path":"internal"}`},
	{Seq: 5, Kind: "tool_call", Name: "Write", Text: `{"file_path":"/tmp/x/README.md","content":"hello"}`},
	{Seq: 6, Kind: "tool_call", Name: "WebFetch", Text: `{"url":"https://example.com/doc"}`},
	{Seq: 7, Kind: "tool_call", Name: "Bash", Text: `{"command":"npm install left-pad"}`},
	{Seq: 8, Kind: "assistant", Text: "Done. I pushed the change."},
}

var auditGolden = []string{
	"area|docs|area.docs",
	"destination-class|external|net.dest",
	"exec|run|exec.run",
	"fs|write|fs.write",
	"net|fetch|net.fetch",
	"pkg|install|pkg.install",
	"search|code|search.code",
	"session:area|docs|area.docs",
	"session:destination-class|external|net.dest",
	"session:exec|run|exec.run",
	"session:fs|write|fs.write",
	"session:net|fetch|net.fetch",
	"session:pkg|install|pkg.install",
	"session:search|code|search.code",
	"session:test|run|test.run",
	"session:vcs|push|vcs.push",
	"test|run|test.run",
	"vcs|push|vcs.push",
}

// C1: a failed console write keeps the last good value; a broken file with no
// earlier value says so in its origin.
func TestConsoleConfigLastGoodSurvivesAFailedWrite(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	path := consoleConfigPath(dataDir)
	if err := os.WriteFile(path, []byte(`{"format_version":1,"recently_closed":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if config, _ := consoleConfig(); config.RecentlyClosed != 5 {
		t.Fatal("first load")
	}
	broken := []byte(`{"format_version":1,"recently_closed":0}`)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateConsoleConfig()
	if snapshot := consoleConfigSnapshot(); snapshot.Origin != "last-good" || snapshot.Config.RecentlyClosed != 5 {
		t.Fatalf("hand edit: %s %d", snapshot.Origin, snapshot.Config.RecentlyClosed)
	}
	// A selection write against the broken file fails and must not discard the value.
	if code := putTranscriptSelection(t, `{"state_token":"`+moduleToken(broken)+`","default_profile":"conversation"}`).Code; code != http.StatusUnprocessableEntity {
		t.Fatalf("write against a broken file: %d", code)
	}
	if snapshot := consoleConfigSnapshot(); snapshot.Origin != "last-good" || snapshot.Config.RecentlyClosed != 5 {
		t.Fatalf("a failed write discarded the last good value: %s %d", snapshot.Origin, snapshot.Config.RecentlyClosed)
	}
	fresh := withConsoleDataDir(t)
	if err := os.WriteFile(consoleConfigPath(fresh), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if snapshot := consoleConfigSnapshot(); snapshot.Origin != "invalid" || len(snapshot.Problems) != 1 {
		t.Fatalf("a broken file with nothing earlier: %s %v", snapshot.Origin, snapshot.Problems)
	}
}

// C2: writing the module a selection names re-resolves the selection at once.
func TestModuleWriteReResolvesSelection(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	if err := os.WriteFile(consoleConfigPath(dataDir), []byte(`{"format_version":1,"transcript":{"default_profile":"later"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if snapshot := consoleConfigSnapshot(); snapshot.Config.Transcript.DefaultProfile != "conversation" || len(snapshot.Problems) != 1 {
		t.Fatalf("missing module must degrade: %+v %v", snapshot.Config.Transcript, snapshot.Problems)
	}
	if err := viewProfileFamily.put(dataDir, "later", "", ViewProfile{FormatVersion: 2, ID: "later", Name: "Later", Rules: []ViewRule{}}); err != nil {
		t.Fatal(err)
	}
	if snapshot := consoleConfigSnapshot(); snapshot.Config.Transcript.DefaultProfile != "later" || len(snapshot.Problems) != 0 {
		t.Fatalf("selection did not re-resolve: %+v %v", snapshot.Config.Transcript, snapshot.Problems)
	}
}

// C6, C7: a theme cannot be re-schemed out from under an appearance, and an
// unreadable module file is an error, not "absent".
func TestThemeReschemeAndUnreadableModule(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	night := ThemeModule{FormatVersion: 1, ID: "night", Name: "Night", Scheme: "dark", Tokens: map[string]string{}}
	if err := themeFamily().put(dataDir, "night", "", night); err != nil {
		t.Fatal(err)
	}
	family := appearanceFamily(dataDir, defaultConsoleConfig().Limits, true)
	builtin, _ := family.loadBuiltins()
	mine := builtin["default"]
	mine.TypeSteps, mine.ID, mine.Name, mine.ThemeDark = nil, "mine", "Mine", "night"
	if err := family.put(dataDir, "mine", "", mine); err != nil {
		t.Fatal(err)
	}
	if key := themeReschemeBlockedBy("night", "light"); key != "appearance mine" {
		t.Fatalf("re-scheme must be blocked: %q", key)
	}
	if key := themeReschemeBlockedBy("night", "dark"); key != "" {
		t.Fatalf("same scheme is fine: %q", key)
	}
	dir := filepath.Join(dataDir, "view-profiles", "odd.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := viewProfileFamily.put(dataDir, "odd", "", ViewProfile{FormatVersion: 2, ID: "odd", Name: "Odd", Rules: []ViewRule{}}); err == nil || errors.Is(err, errModuleStale) {
		t.Fatalf("an unreadable target must be an error: %v", err)
	}
}

func TestAppearanceSheetCases(t *testing.T) {
	dataDir := withConsoleDataDir(t)
	paper := ThemeModule{FormatVersion: 1, ID: "paper", Name: "Paper", Scheme: "light",
		Tokens: map[string]string{"bg": "#fffdf5"},
		Labels: map[string]ThemeLabel{"claude": {Bg: "#112233", Fg: "#ddeeff"}, "st-draft": {Bg: "#000000", Fg: "#ffffff"}}}
	if err := themeFamily().put(dataDir, "paper", "", paper); err != nil {
		t.Fatal(err)
	}
	family := appearanceFamily(dataDir, defaultConsoleConfig().Limits, true)
	builtin, _ := family.loadBuiltins()
	mine := builtin["default"]
	mine.TypeSteps, mine.ThemeLight = nil, "paper"
	if err := family.put(dataDir, "default", "", mine); err != nil {
		t.Fatal(err)
	}
	sheet := appearanceCSS(t, "127.0.0.1:7777", "").Body.String()
	light := sheet[strings.Index(sheet, "@media (prefers-color-scheme: light)"):]
	for _, want := range []string{"--bg: #fffdf5;", "--panel: #ffffff;", "--chip-claude-bg: #112233;"} {
		if !strings.Contains(light, want) {
			t.Fatalf("light block misses %q:\n%s", want, light)
		}
	}
	if strings.Contains(sheet, "st-draft") {
		t.Fatalf("an unregistered label key must never be emitted:\n%s", sheet)
	}
	if !strings.Contains(sheet, "var(--chip-claude-bg, var(--chip-neutral-bg))") {
		t.Fatalf("runtime chip rules fall back to neutral:\n%s", sheet)
	}
}

// The sheet answers an empty 200 on an internal failure: tokens.css holds the
// built-in themes, so the console still renders.
func TestAppearanceSheetFailureIsEmpty(t *testing.T) {
	withConsoleDataDir(t)
	previous := renderAppearanceSheet
	renderAppearanceSheet = func(string) (string, error) { return "", errors.New("broken") }
	t.Cleanup(func() { renderAppearanceSheet = previous })
	recorder := appearanceCSS(t, "127.0.0.1:7777", "")
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("failure: %d %q", recorder.Code, recorder.Body.String())
	}
}

// GET /api/session rows carry facts; stream snapshot rows do not.
func TestFactsRideRouteRowsNotSnapshots(t *testing.T) {
	detectors, _ := engine.DefaultDetectors()
	previous := auditDetectors
	if err := initAudit(detectors); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { auditDetectors = previous })
	detail := &SessionDetail{SessionDetail: harvest.SessionDetail{Events: auditGoldenEvents}}
	body, _ := json.Marshal(decorateSessionAgents(detail))
	if !strings.Contains(string(body), `"facts":["exec:run"`) {
		t.Fatalf("route rows must carry facts: %s", body)
	}
	for _, row := range annotateThoughts(auditGoldenEvents, 0) {
		if len(row.Facts) != 0 {
			t.Fatalf("snapshot rows carry no facts: %+v", row)
		}
	}
}

func TestConsoleWriteBodyIsBounded(t *testing.T) {
	withConsoleDataDir(t)
	huge := `{"state_token":"","profile":{"format_version":2,"id":"big","name":"` + strings.Repeat("x", 70<<10) + `","rules":[]}}`
	request := httptest.NewRequest(http.MethodPut, "/api/console/view-profiles/big", strings.NewReader(huge))
	request.SetPathValue("id", "big")
	recorder := httptest.NewRecorder()
	handleConsoleViewProfilePut(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an oversized body must be refused: %d", recorder.Code)
	}
}

// Only a declared working directory is a path (A5): an unresolved project label is
// never taken for the checkout the extract's paths are made relative to. The extract
// no longer suggests a directory at all — a handoff is not written into a checkout.
func TestHandoffTakesOnlyADeclaredDirectoryForACheckout(t *testing.T) {
	detail := &SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: harvest.SessionSummary{Runtime: "claude", ID: "x", Project: "-Users-me-proj"}}}
	if facts := handoffCheckout(detail); facts.root != "" || facts.repositoryID != nil {
		t.Fatalf("an unresolved project must not become a directory: %+v", facts)
	}
}

func TestAppearanceHealthListsBuiltins(t *testing.T) {
	health := LoadAppearanceHealth(t.TempDir())
	if len(health.Themes) != 3 || len(health.Appearances) != 1 || len(health.Rejected) != 0 {
		t.Fatalf("doctor appearance health: %+v", health)
	}
}
