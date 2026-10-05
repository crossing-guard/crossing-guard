package daemon

// Console appearance (session-view-and-console-preferences plan §C). Two module
// families on the config-module loader:
//
//   - themes/<id>.json: a colour token set for one scheme (dark or light), plus
//     runtime label colours keyed by runtime id. A token an installed theme
//     omits inherits from the built-in theme of its scheme.
//   - appearance/<id>.json: which theme to use for each scheme, whether to
//     follow the OS or pin one scheme, the accent, the fonts, the text size and
//     the reading widths.
//
// console.json appearance.module selects one appearance. GET /appearance.css
// renders it as CSS custom properties before app.css loads. tokens.css keeps
// the complete built-in themes, so the console renders without the sheet.

import (
	"embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"crossing-guard/harvest"
)

const appearanceFormatVersion = 1

//go:embed themes/*.json
var builtinThemes embed.FS

//go:embed appearance/*.json
var builtinAppearances embed.FS

// themeTokens are the colour tokens a theme sets, in the order the sheet
// emits them. Vendor label colours are not tokens: they are labels, keyed by
// runtime (ADR 0024).
var themeTokens = []string{
	"bg", "panel", "panel2", "surface", "border", "text", "dim",
	"accent", "accent-ink", "accent-fg", "accent2", "ok", "warn", "bad", "purple",
	"user-bubble-bg", "user-bubble-border",
	"chip-neutral-bg", "chip-neutral-fg", "chip-good-bg", "chip-good-fg", "chip-warn-bg", "chip-warn-fg",
	"chip-bad-bg", "chip-bad-fg", "chip-info-bg", "chip-info-fg", "chip-purple-bg", "chip-purple-fg",
	"chip-amber-bg", "chip-amber-fg",
	// Whose work a usage figure is: the session's own, its subagents', its
	// agents' (session usage breakdown plan §5.4).
	"work-main", "work-subagent", "work-agent",
}

// typeStepCount is how many --fs-N steps app.css names. Until type
// consolidation (owner decision O-8) only the built-in appearance sets them.
const typeStepCount = 15

var (
	themeColour   = regexp.MustCompile(`^#(?:[0-9a-f]{3}|[0-9a-f]{6})$`)
	runtimeLabel  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	fontName      = regexp.MustCompile(`^[A-Za-z0-9 ._-]+$`)
	themeTokenSet = func() map[string]bool {
		set := map[string]bool{}
		for _, token := range themeTokens {
			set[token] = true
		}
		return set
	}()
	// fontKeywords render unquoted; every other font name renders quoted.
	fontKeywords = map[string]bool{"serif": true, "sans-serif": true, "monospace": true, "cursive": true,
		"fantasy": true, "system-ui": true, "ui-serif": true, "ui-sans-serif": true, "ui-monospace": true,
		"ui-rounded": true, "math": true, "emoji": true, "-apple-system": true, "BlinkMacSystemFont": true}
)

// ThemeModule is one theme as written.
type ThemeModule struct {
	FormatVersion int                   `json:"format_version"`
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	Scheme        string                `json:"scheme"`
	Tokens        map[string]string     `json:"tokens"`
	Labels        map[string]ThemeLabel `json:"labels,omitempty"`
}

// ThemeLabel is one runtime's chip colours.
type ThemeLabel struct {
	Bg string `json:"bg"`
	Fg string `json:"fg"`
}

// AppearanceModule is one appearance as written.
type AppearanceModule struct {
	FormatVersion int    `json:"format_version"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	ThemeDark     string `json:"theme_dark"`
	ThemeLight    string `json:"theme_light"`
	// FollowOS renders the scheme the OS asks for; otherwise PinnedScheme.
	FollowOS     bool   `json:"follow_os"`
	PinnedScheme string `json:"pinned_scheme,omitempty"`
	// Accent overrides the accent set per scheme; unset members inherit.
	Accent          *AppearanceAccent `json:"accent,omitempty"`
	UIFont          string            `json:"ui_font"`
	MonoFont        string            `json:"mono_font"`
	TextSize        float64           `json:"text_size"`
	TranscriptWidth int               `json:"transcript_width"`
	ChatWidth       int               `json:"chat_width"`
	// TypeSteps are the reference sizes at text_size 14, smallest first. Only
	// the built-in sets them until type consolidation (O-8).
	TypeSteps []float64 `json:"type_steps,omitempty"`
}

type AppearanceAccent struct {
	Dark  *AccentSet `json:"dark,omitempty"`
	Light *AccentSet `json:"light,omitempty"`
}

type AccentSet struct {
	Accent    string `json:"accent,omitempty"`
	AccentInk string `json:"accent_ink,omitempty"`
	AccentFg  string `json:"accent_fg,omitempty"`
}

// themeFamily reads themes. A built-in must set every token; an installed
// theme may omit tokens, which inherit from the built-in of its scheme.
func themeFamily() moduleFamily[ThemeModule] {
	return moduleFamily[ThemeModule]{subdir: "themes", builtins: builtinThemes, glob: "themes/*.json",
		decode: decodeTheme, id: func(t ThemeModule) string { return t.ID }}
}

func decodeTheme(data []byte, builtin bool) (ThemeModule, error) {
	var theme ThemeModule
	if err := decodeModuleStrict(data, &theme); err != nil {
		return ThemeModule{}, err
	}
	if err := validateModuleHead(theme.FormatVersion, theme.ID, theme.Name); err != nil {
		return ThemeModule{}, err
	}
	if theme.Scheme != "dark" && theme.Scheme != "light" {
		return ThemeModule{}, fmt.Errorf("scheme %q must be dark or light", theme.Scheme)
	}
	for _, name := range sortedKeys(theme.Tokens) {
		if !themeTokenSet[name] {
			return ThemeModule{}, fmt.Errorf("tokens.%s is not a theme token", name)
		}
		if !themeColour.MatchString(theme.Tokens[name]) {
			return ThemeModule{}, fmt.Errorf("tokens.%s %q must be #rgb or #rrggbb in lowercase", name, theme.Tokens[name])
		}
	}
	if builtin {
		for _, name := range themeTokens {
			if theme.Tokens[name] == "" {
				return ThemeModule{}, fmt.Errorf("built-in theme misses tokens.%s", name)
			}
		}
	}
	for _, runtime := range sortedKeys(theme.Labels) {
		label := theme.Labels[runtime]
		if !runtimeLabel.MatchString(runtime) {
			return ThemeModule{}, fmt.Errorf("labels.%s is not a runtime id", runtime)
		}
		if !themeColour.MatchString(label.Bg) || !themeColour.MatchString(label.Fg) {
			return ThemeModule{}, fmt.Errorf("labels.%s colours must be #rgb or #rrggbb in lowercase", runtime)
		}
	}
	return theme, nil
}

// appearanceFamily reads appearances against the installation's limits. With
// strictRefs, a theme reference that does not resolve to a theme of the right
// scheme is refused (a write); without it, it degrades at render time (a load).
func appearanceFamily(dataDir string, limits ConsoleLimits, strictRefs bool) moduleFamily[AppearanceModule] {
	return moduleFamily[AppearanceModule]{subdir: "appearance", builtins: builtinAppearances, glob: "appearance/*.json",
		id: func(a AppearanceModule) string { return a.ID },
		decode: func(data []byte, builtin bool) (AppearanceModule, error) {
			var module AppearanceModule
			if err := decodeModuleStrict(data, &module); err != nil {
				return AppearanceModule{}, err
			}
			if err := module.validate(limits, builtin); err != nil {
				return AppearanceModule{}, err
			}
			if strictRefs && !builtin {
				if err := validateThemeRefs(dataDir, module); err != nil {
					return AppearanceModule{}, err
				}
			}
			return module, nil
		}}
}

func (a AppearanceModule) validate(limits ConsoleLimits, builtin bool) error {
	if err := validateModuleHead(a.FormatVersion, a.ID, a.Name); err != nil {
		return err
	}
	if !moduleIDPattern.MatchString(a.ThemeDark) || !moduleIDPattern.MatchString(a.ThemeLight) {
		return errors.New("theme_dark and theme_light must name themes")
	}
	if !a.FollowOS && a.PinnedScheme != "dark" && a.PinnedScheme != "light" {
		return errors.New("pinned_scheme must be dark or light when follow_os is false")
	}
	if a.FollowOS && a.PinnedScheme != "" {
		return errors.New("pinned_scheme applies only when follow_os is false")
	}
	if a.Accent != nil {
		for scheme, set := range map[string]*AccentSet{"dark": a.Accent.Dark, "light": a.Accent.Light} {
			if set == nil {
				continue
			}
			for name, value := range map[string]string{"accent": set.Accent, "accent_ink": set.AccentInk, "accent_fg": set.AccentFg} {
				if value != "" && !themeColour.MatchString(value) {
					return fmt.Errorf("accent.%s.%s %q must be #rgb or #rrggbb in lowercase", scheme, name, value)
				}
			}
		}
	}
	for field, stack := range map[string]string{"ui_font": a.UIFont, "mono_font": a.MonoFont} {
		if _, err := normalizeFontStack(stack, limits); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	if !builtin {
		if a.TextSize < limits.TextSizeMin || a.TextSize > limits.TextSizeMax {
			return fmt.Errorf("text_size must be between %g and %g", limits.TextSizeMin, limits.TextSizeMax)
		}
		for field, width := range map[string]int{"transcript_width": a.TranscriptWidth, "chat_width": a.ChatWidth} {
			if width < limits.WidthMin || width > limits.WidthMax {
				return fmt.Errorf("%s must be between %d and %d", field, limits.WidthMin, limits.WidthMax)
			}
		}
		if len(a.TypeSteps) > 0 {
			return errors.New("type_steps is set only by the built-in appearance")
		}
		return nil
	}
	if len(a.TypeSteps) != typeStepCount {
		return fmt.Errorf("the built-in type_steps must hold exactly %d sizes", typeStepCount)
	}
	for i, step := range a.TypeSteps {
		if step <= 0 || (i > 0 && step <= a.TypeSteps[i-1]) {
			return errors.New("type_steps must be positive and strictly ascending")
		}
	}
	return nil
}

func validateModuleHead(format int, id, name string) error {
	if format != appearanceFormatVersion {
		return fmt.Errorf("unsupported format_version %d", format)
	}
	if !moduleIDPattern.MatchString(id) {
		return fmt.Errorf("id %q must be lowercase letters, digits and hyphens", id)
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("name must be set")
	}
	return nil
}

// normalizeFontStack re-serialises a font stack from an allow-list grammar:
// generic keywords bare, every other name quoted. Nothing the owner typed
// reaches the stylesheet except those names.
func normalizeFontStack(stack string, limits ConsoleLimits) (string, error) {
	items := strings.Split(stack, ",")
	if strings.TrimSpace(stack) == "" || len(items) > limits.FontItemsMax {
		return "", fmt.Errorf("a font stack holds 1 to %d names", limits.FontItemsMax)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		name := strings.TrimSpace(item)
		if len(name) >= 2 && (name[0] == '"' || name[0] == '\'') && name[len(name)-1] == name[0] {
			name = strings.TrimSpace(name[1 : len(name)-1])
		}
		if name == "" || len(name) > limits.FontItemCharsMax || !fontName.MatchString(name) {
			return "", fmt.Errorf("font name %q must be 1 to %d letters, digits, spaces, dots, hyphens or underscores", name, limits.FontItemCharsMax)
		}
		if fontKeywords[name] {
			out = append(out, name)
		} else {
			out = append(out, `"`+name+`"`)
		}
	}
	return strings.Join(out, ", "), nil
}

func (l ConsoleLimits) validate() error {
	if !(consoleTextSizeFloor <= l.TextSizeMin && l.TextSizeMin < l.TextSizeMax && l.TextSizeMax <= consoleTextSizeCeiling) {
		return fmt.Errorf("console_limits text sizes must satisfy %d <= min < max <= %d", consoleTextSizeFloor, consoleTextSizeCeiling)
	}
	if !(consoleWidthFloor <= l.WidthMin && l.WidthMin < l.WidthMax && l.WidthMax <= consoleWidthCeiling) {
		return fmt.Errorf("console_limits widths must satisfy %d <= min < max <= %d", consoleWidthFloor, consoleWidthCeiling)
	}
	if l.FontItemCharsMax < 1 || l.FontItemCharsMax > consoleFontItemChars || l.FontItemsMax < 1 || l.FontItemsMax > consoleFontItems {
		return fmt.Errorf("console_limits font bounds must be within 1..%d characters and 1..%d names", consoleFontItemChars, consoleFontItems)
	}
	return nil
}

// validateAppearanceLimits refuses limits that would make a built-in
// appearance invalid: the owner may narrow the bounds, but never so far that
// the product's own default falls outside them.
func validateAppearanceLimits(limits ConsoleLimits) error {
	builtins, rejected := appearanceFamily("", limits, false).loadBuiltins()
	if len(rejected) > 0 {
		return fmt.Errorf("console_limits exclude a built-in appearance: %s", rejected[0].Error)
	}
	for _, id := range sortedKeys(builtins) {
		module := builtins[id]
		module.TypeSteps = nil
		if err := module.validate(limits, false); err != nil {
			return fmt.Errorf("console_limits exclude built-in appearance %q: %v", id, err)
		}
	}
	return nil
}

// validateAppearanceSelection requires console.json appearance.module to name
// a loadable appearance.
func validateAppearanceSelection(selection ConsoleAppearanceSelection, limits ConsoleLimits, dataDir string) error {
	if _, ok := appearanceFamily(dataDir, limits, false).resolve(dataDir, selection.Module); !ok {
		return fmt.Errorf("appearance.module %q is not an appearance", selection.Module)
	}
	return nil
}

func validateThemeRefs(dataDir string, module AppearanceModule) error {
	themes, _ := themeFamily().load(dataDir)
	schemes := map[string]string{}
	for _, theme := range themes {
		schemes[theme.Module.ID] = theme.Module.Scheme
	}
	if schemes[module.ThemeDark] != "dark" {
		return fmt.Errorf("theme_dark %q is not a dark theme", module.ThemeDark)
	}
	if schemes[module.ThemeLight] != "light" {
		return fmt.Errorf("theme_light %q is not a light theme", module.ThemeLight)
	}
	return nil
}

// resolvedTheme returns a theme's complete token set: its own tokens over the
// built-in theme of its scheme. A missing theme resolves to that built-in.
func resolvedTheme(themes []resolvedModule[ThemeModule], builtins map[string]ThemeModule, id, scheme string) ThemeModule {
	base := builtins[scheme]
	for _, candidate := range themes {
		if candidate.Module.ID != id || candidate.Module.Scheme != scheme {
			continue
		}
		merged := candidate.Module
		merged.Tokens = map[string]string{}
		for name, value := range base.Tokens {
			merged.Tokens[name] = value
		}
		for name, value := range candidate.Module.Tokens {
			merged.Tokens[name] = value
		}
		labels := map[string]ThemeLabel{}
		for runtime, label := range base.Labels {
			labels[runtime] = label
		}
		for runtime, label := range candidate.Module.Labels {
			labels[runtime] = label
		}
		merged.Labels = labels
		return merged
	}
	return base
}

// appearanceSheet renders the selected appearance. Every value in it comes
// from a validated module: colours match the colour pattern, fonts are
// re-serialised from the allow-list grammar, numbers are formatted here, and
// runtime ids are both registered and pattern-checked.
func appearanceSheet(dataDir string) (string, error) {
	config, _ := consoleConfig()
	family := appearanceFamily(dataDir, config.Limits, false)
	module, ok := family.resolve(dataDir, config.Appearance.Module)
	if !ok {
		noteSheetFallback(config.Appearance.Module)
		if module, ok = family.resolve(dataDir, defaultConsoleConfig().Appearance.Module); !ok {
			return "", errors.New("no built-in appearance")
		}
	} else {
		noteSheetFallback("")
	}
	builtinAppearance, _ := family.loadBuiltins()
	steps := builtinAppearance[defaultConsoleConfig().Appearance.Module].TypeSteps
	if len(steps) != typeStepCount {
		return "", errors.New("the built-in appearance has no type steps")
	}
	themes, _ := themeFamily().load(dataDir)
	builtinThemeSet, _ := themeFamily().loadBuiltins()
	builtinByScheme := map[string]ThemeModule{}
	for _, theme := range builtinThemeSet {
		if _, taken := builtinByScheme[theme.Scheme]; !taken || theme.ID == theme.Scheme {
			builtinByScheme[theme.Scheme] = theme
		}
	}
	dark := applyAccent(resolvedTheme(themes, builtinByScheme, module.ThemeDark, "dark"), module.Accent, "dark")
	light := applyAccent(resolvedTheme(themes, builtinByScheme, module.ThemeLight, "light"), module.Accent, "light")
	uiFont, err := normalizeFontStack(module.UIFont, config.Limits)
	if err != nil {
		return "", err
	}
	monoFont, err := normalizeFontStack(module.MonoFont, config.Limits)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("/* appearance: " + module.ID + " */\n:root:root:root {\n")
	fmt.Fprintf(&b, "  --ui-font: %s;\n  --mono: %s;\n", uiFont, monoFont)
	fmt.Fprintf(&b, "  --measure: %dpx;\n  --chat-measure: %dpx;\n", module.TranscriptWidth, module.ChatWidth)
	for i, step := range steps {
		fmt.Fprintf(&b, "  --fs-%d: %spx;\n", i+1, strconv.FormatFloat(roundHundredth(step*module.TextSize/14), 'f', -1, 64))
	}
	b.WriteString("}\n")
	if module.FollowOS {
		writeSchemeBlock(&b, "@media (prefers-color-scheme: dark) {\n", dark)
		writeSchemeBlock(&b, "@media (prefers-color-scheme: light) {\n", light)
	} else if module.PinnedScheme == "light" {
		writeSchemeBlock(&b, "", light)
	} else {
		writeSchemeBlock(&b, "", dark)
	}
	for _, runtime := range harvest.RuntimeNames() {
		if !runtimeLabel.MatchString(runtime) {
			continue
		}
		if _, ok := dark.Labels[runtime]; !ok {
			if _, ok := light.Labels[runtime]; !ok {
				continue
			}
		}
		// The neutral chip colours are the fallback, so a label one scheme's
		// theme omits still renders as a chip under that scheme.
		fmt.Fprintf(&b, ".chip.%s { background: var(--chip-%s-bg, var(--chip-neutral-bg)); color: var(--chip-%s-fg, var(--chip-neutral-fg)); }\n", runtime, runtime, runtime)
	}
	return b.String(), nil
}

// writeSchemeBlock emits one scheme's complete token set, including its
// registered runtime labels, under a selector that beats tokens.css's light
// blocks (specificity 0,2,0). wrap is "" for a pinned scheme.
func writeSchemeBlock(b *strings.Builder, wrap string, theme ThemeModule) {
	indent := ""
	if wrap != "" {
		b.WriteString(wrap)
		indent = "  "
	}
	fmt.Fprintf(b, "%s:root:root:root {\n%s  color-scheme: %s;\n", indent, indent, theme.Scheme)
	for _, name := range themeTokens {
		fmt.Fprintf(b, "%s  --%s: %s;\n", indent, name, theme.Tokens[name])
	}
	registered := map[string]bool{}
	for _, runtime := range harvest.RuntimeNames() {
		registered[runtime] = true
	}
	for _, runtime := range sortedKeys(theme.Labels) {
		if !registered[runtime] || !runtimeLabel.MatchString(runtime) {
			continue
		}
		label := theme.Labels[runtime]
		fmt.Fprintf(b, "%s  --chip-%s-bg: %s;\n%s  --chip-%s-fg: %s;\n", indent, runtime, label.Bg, indent, runtime, label.Fg)
	}
	fmt.Fprintf(b, "%s}\n", indent)
	if wrap != "" {
		b.WriteString("}\n")
	}
}

func applyAccent(theme ThemeModule, accent *AppearanceAccent, scheme string) ThemeModule {
	if accent == nil {
		return theme
	}
	set := accent.Dark
	if scheme == "light" {
		set = accent.Light
	}
	if set == nil {
		return theme
	}
	tokens := map[string]string{}
	for name, value := range theme.Tokens {
		tokens[name] = value
	}
	for name, value := range map[string]string{"accent": set.Accent, "accent-ink": set.AccentInk, "accent-fg": set.AccentFg} {
		if value != "" {
			tokens[name] = value
		}
	}
	theme.Tokens = tokens
	return theme
}

var (
	sheetFallbackMu   sync.Mutex
	sheetFallbackLast string
)

// noteSheetFallback logs once per change when the selected appearance has gone
// missing on disk and the sheet renders the built-in instead.
func noteSheetFallback(missing string) {
	sheetFallbackMu.Lock()
	defer sheetFallbackMu.Unlock()
	if missing == sheetFallbackLast {
		return
	}
	sheetFallbackLast = missing
	if missing != "" {
		log.Printf("appearance %q is selected but cannot be loaded; the built-in appearance renders", missing)
	}
}

func roundHundredth(value float64) float64 {
	return float64(int64(value*100+0.5)) / 100
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// renderAppearanceSheet is the sheet renderer the handler calls; a test swaps
// it to prove the failure path.
var renderAppearanceSheet = appearanceSheet

// appearanceCSSHandler serves the generated sheet. It is outside /api (a
// stylesheet link cannot send the token), so it guards itself: the Host must
// be this listener, a cross-site fetch is refused, and CORP stops another
// origin's page from applying (and so reading) it. On any internal failure it
// answers an empty sheet: tokens.css already holds the built-in themes.
func appearanceCSSHandler(addr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameHost(r.Host, addr) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		sheet, err := renderAppearanceSheet(consoleDataDir())
		if err != nil {
			log.Printf("appearance sheet unavailable, the built-in themes apply: %v", err)
			sheet = ""
		}
		_, _ = w.Write([]byte(sheet))
	}
}

// AppearanceHealth lists the theme and appearance modules in use and the
// module files that failed, for doctor. It reads the data directory directly,
// using the limits of console.json there.
type AppearanceHealth struct {
	Themes      []string          `json:"themes"`
	Appearances []string          `json:"appearances"`
	Rejected    []ModuleRejection `json:"rejected"`
}

func LoadAppearanceHealth(dataDir string) AppearanceHealth {
	limits := defaultConsoleConfig().Limits
	if config, _, _, err := loadConsoleConfig(dataDir); err == nil {
		limits = config.Limits
	}
	health := AppearanceHealth{Themes: []string{}, Appearances: []string{}}
	themes, rejected := themeFamily().load(dataDir)
	for _, theme := range themes {
		health.Themes = append(health.Themes, theme.Module.ID+" "+theme.Module.Scheme+" "+theme.Origin)
	}
	appearances, appearanceRejected := appearanceFamily(dataDir, limits, false).load(dataDir)
	for _, module := range appearances {
		health.Appearances = append(health.Appearances, module.Module.ID+" "+module.Origin)
	}
	health.Rejected = append(rejected, appearanceRejected...)
	return health
}
