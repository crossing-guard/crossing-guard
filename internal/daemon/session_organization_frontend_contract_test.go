package daemon

// Pins how session organization meets the running console
// (session-organization implementation plan §0 C1, C7 and §5). Each pin is a
// failure that was found in review or in the live run, not a style preference.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readStatic(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("static", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// functionBody returns the source from a function's declaration to the next
// top-level function, which is enough to say what it does and does not call.
func functionBody(t *testing.T, source, declaration string) string {
	t.Helper()
	start := strings.Index(source, declaration)
	if start < 0 {
		t.Fatalf("%q is gone", declaration)
	}
	rest := source[start+len(declaration):]
	if end := regexp.MustCompile(`\n(async )?function |\n\$\('#search'\)`).FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	return rest
}

// Tagging, selecting a view and clearing the filter all happen while a session
// is open, often mid-turn. Only renderSessionsLanding may close the open
// session's live stream or replace the centre pane; renderRail must do neither.
func TestRailRedrawNeverTouchesTheOpenSession(t *testing.T) {
	sessions := readStatic(t, "js/views/sessions.js")
	rail := functionBody(t, sessions, "async function renderRail(options = {}) {")
	if strings.Contains(rail, "clearSelectedSessionBindings") {
		t.Fatal("renderRail closes the open session's live stream; that belongs to renderSessionsLanding")
	}
	for _, write := range regexp.MustCompile(`\$\('#main'\)\.innerHTML`).FindAllStringIndex(rail, -1) {
		line := rail[strings.LastIndex(rail[:write[0]], "\n")+1 : write[0]]
		if !strings.Contains(line, "options.landing") {
			t.Fatalf("renderRail writes the centre pane outside the landing case: %q", strings.TrimSpace(line))
		}
	}
	landing := functionBody(t, sessions, "function renderSessionsLanding() {")
	if !strings.Contains(landing, "clearSelectedSessionBindings()") || !strings.Contains(landing, "$('#main').innerHTML") {
		t.Fatal("renderSessionsLanding no longer owns the binding close and the centre-pane write")
	}
	tagged := functionBody(t, sessions, "function repaintTaggedRows(updated) {")
	if strings.Contains(tagged, "renderRail(") || strings.Contains(tagged, "renderSessionList(") || !strings.Contains(tagged, "repaintRowTags(") {
		t.Fatal("a tag change must patch the affected rows in place, not redraw the rail")
	}
	// The rail fetch falls back to the plain repository rail when no query is
	// active. Its third argument names the selected view's board, whose
	// placement belongs to that view and never to a filter-bar query (sessions
	// board plan, post-review PO-11; board-observed-columns plan §2.2).
	for _, pin := range []string{"if (!q) return S.sel ? renderRail() : renderSessionList();", "if (await handleBarInput(q)) return;",
		"organizedRailUrl(active, 500, boardView) || '/api/sessions?view=rail&repository_limit=500'",
		"const boardView = viewScoped && boardConfig(selectedView()) ? selectedView().id : '';",
		// A board view's rail pages place rows as its headings counted them
		// (board-rail-owner-paging plan, invariant 1).
		"organizedPageUrl(active, { key, mode: pref.mode, offset: pref.offset, limit: SESSION_PAGE_SIZE, selected, boardView })",
		"mountHeaderTags(head, d);", "appendRowOrganization(d, s,"} {
		if !strings.Contains(sessions, pin) {
			t.Fatalf("sessions.js lost %q", pin)
		}
	}
	// A board view draws the board in the centre only while no session is
	// open, and leaves the rail a hook: a session opening redraws it, so its
	// groups are listed (board-open-redraws-rail plan).
	boardBranch := `if (board && active && active.scope === 'view:' + view.id && !S.sel && boardFitted) {
      renderBoardCenter(view, board, data);
      // The board holds the centre only while no session does: opening one
      // hands the centre over, and the rail then lists the view's groups.
      revealRailSelection = () => { renderRail(); };
      return;
    }`
	if !strings.Contains(rail, boardBranch) {
		t.Fatal("renderRail: the board branch must require no open session, draw the board, arm the rail redraw, and return")
	}
	app := readStatic(t, "js/app.js")
	if !strings.Contains(app, "clearBar(); if (S.sel) renderRail(); else renderSessionList();") {
		t.Fatal("app.js: Escape in the filter bar must redraw only the rail while a session is open")
	}
	if strings.Contains(app, "tag_session") {
		t.Fatal("app.js binds the tag shortcut; its listener has no typing guard and would swallow the letter in the composer")
	}
}

// Left to bubble, Escape reaches the global handler that interrupts a running
// agent turn. Every popover this feature opens must stop it.
func TestOrganizationPopoversKeepEscapeToThemselves(t *testing.T) {
	// The Session views page handles no Escape of its own; its dialogs are the
	// Agents pages' (session-views-rebuild plan §2).
	for _, name := range []string{"js/session-organization/tag-popover.js", "js/session-organization/tag-manage.js",
		"js/session-organization/view-list.js", "js/session-organization/query-bar.js", "js/orchestration/agents/agent-dialog.js"} {
		source := readStatic(t, name)
		escapes := strings.Count(source, "'Escape'")
		if escapes == 0 || strings.Count(source, "event.stopPropagation()") < escapes {
			t.Fatalf("%s handles Escape %d time(s) without stopping propagation each time", name, escapes)
		}
	}
	shortcut := readStatic(t, "js/session-organization/tag-shortcut.js")
	if !strings.Contains(shortcut, "isTypingTarget(document.activeElement)") {
		t.Fatal("the tag shortcut lost its typing guard")
	}
}

// The browser ships the mechanism and no opinion: no view, no tag, and no
// native dialogs (they block embedded browsers and cannot be driven by tests).
func TestOrganizationBrowserCodeShipsNoViewsTagsOrDialogs(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("static", "js", "session-organization"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".test.mjs") {
			continue
		}
		// Every check reads code only: a word in a comment is never shipped
		// behaviour, and all checks agree on what counts as code.
		source := stripJSComments(readStatic(t, "js/session-organization/"+entry.Name()))
		if problem := organizationVocabularyInCode(source); problem != "" {
			t.Fatalf("%s %s; the owner's views, keys and values are his, never code's", entry.Name(), problem)
		}
		if strings.Contains(source, "window.prompt(") || strings.Contains(source, "window.alert(") {
			t.Fatalf("%s uses a native prompt or alert", entry.Name())
		}
		for _, runtime := range []string{"'claude'", "'codex'", "'opencode'"} {
			if strings.Contains(source, runtime) {
				t.Fatalf("%s names a runtime", entry.Name())
			}
		}
	}
	// Who said a tag is a colour and a hover; these words must never be built
	// into what the owner reads.
	chips := readStatic(t, "js/session-organization/tag-chips.js")
	for _, word := range []string{"'asserted", "'claimed", "provenance ·", "detector"} {
		if strings.Contains(chips, word) {
			t.Fatalf("tag-chips.js puts %s where the owner can read it", word)
		}
	}
	css := readStatic(t, "css/app.css")
	for _, pin := range []string{".chip.cl-model-claimed", ".viewlist", ".view .vc", ".sess .g", ".sess .n", ".sess.multi",
		".sess.missing .t", ".tag-toggle", ".tag-add", ".tag-pop", "#searchstatus", ".rail-bulk", ".session-tags"} {
		if !strings.Contains(css, pin) {
			t.Fatalf("app.css lost %q", pin)
		}
	}
	index := readStatic(t, "index.html")
	if !strings.Contains(index, `id="searchstatus"`) {
		t.Fatal("index.html lost the filter bar's status line")
	}
	// The count line changes on every pause in typing; as a live region it would
	// be read out again each time. Only a problem is announced (role=alert in JS).
	if strings.Contains(index, `id="searchstatus" role="status"`) {
		t.Fatal("the filter bar's status line is a live region again")
	}
	bar := readStatic(t, "js/session-organization/query-bar.js")
	for _, pin := range []string{"generation !== inputGeneration", "renderRail({ prefetched: rail })"} {
		if !strings.Contains(bar, pin) {
			t.Fatalf("query-bar.js lost %q: a slow answer to an older keystroke could undo a newer filter, or every keystroke fetches the rail twice", pin)
		}
	}
	// A view PUT replaces the stored view, so the Settings page commits the
	// whole view it holds, never a closed list of fields (settings-views-full-
	// view-save plan §2: a closed list deleted a board's columns).
	// The page's modules: only settings-views.js writes a view (Save and
	// Duplicate), and each write takes viewCommit of the whole view. None of
	// them opens a native dialog.
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "settings-view") || strings.HasSuffix(name, ".test.mjs") {
			continue
		}
		source := stripJSComments(readStatic(t, "js/session-organization/"+name))
		if strings.Contains(source, "window.confirm(") {
			t.Fatalf("%s opens a native dialog", name)
		}
		writes := strings.Count(source, "createView(") + strings.Count(source, "updateView(")
		whole := strings.Count(source, "createView(viewCommit(") + strings.Count(source, "updateView(viewCommit(")
		if name != "settings-views.js" && writes > 0 {
			t.Fatalf("%s writes a view: every write belongs to settings-views.js, through viewCommit", name)
		}
		if name == "settings-views.js" && (whole < 3 || writes != whole || closedViewCommit.MatchString(source)) {
			t.Fatalf("settings-views.js no longer saves through viewCommit (%d of %d writes): a field the form does not list (board, record_kind) would be deleted", whole, writes)
		}
	}
	if !strings.Contains(readStatic(t, "js/session-organization/rail-selection.js"), "offsetParent !== null") {
		t.Fatal("a shift-range may again select rows the owner cannot see (collapsed agent folds)")
	}
	if !strings.Contains(readStatic(t, "js/views/sessions.js"), "clearRailSelection();") {
		t.Fatal("a rail redraw no longer clears the multi-selection it invalidates")
	}
}

// jsQuoted is one quoted literal in any of the three quote styles, non-empty
// and without spaces: the shape a compiled key or value takes. Its inner group
// is the literal's text.
const jsQuoted = `(?:'([^'\s]+)'|"([^"\s]+)"|` + "`([^`\\s]+)`" + `)`

// jsTagKeyRead is a tag's key read by property or by bracket: x.key,
// x['key'], x["key"].
const jsTagKeyRead = `(?:\.key\b|\[\s*['"` + "`" + `]key['"` + "`" + `]\s*\])`

var (
	literalViewName = regexp.MustCompile(`createView\(\s*\{[^}]*name:\s*['"` + "`" + `]`)
	literalTagPart  = regexp.MustCompile(`(apply|retract):\s*\[\s*\{[^}]*(key|value):\s*['"` + "`" + `]`)
	// A key compared with a literal, either way round, or switched on — the
	// three shapes scripts/vendor-lint-js.mjs's BRANCH pattern covers.
	literalKeyTests = []*regexp.Regexp{
		regexp.MustCompile(jsTagKeyRead + `\s*(?:===|!==|==|!=)\s*` + jsQuoted),
		regexp.MustCompile(jsQuoted + `\s*(?:===|!==|==|!=)\s*[\w$.]*` + jsTagKeyRead),
		regexp.MustCompile(`switch\s*\(\s*[\w$.]*` + jsTagKeyRead + `\s*\)\s*\{[^}]*?\bcase\s*` + jsQuoted),
	}
	closedViewCommit = regexp.MustCompile(`\{\s*id:\s*view\.id,\s*name:`)
	tagKeyGrouping   = regexp.MustCompile(`['"` + "`" + `]tag-key:`)
	// A literal fallback: after || or ??, or as a ternary's else. The else is
	// told from an object key by the space before its colon (` : 'x'`, the
	// codebase's ternary style) — `{ key: 'x' }` has none.
	quotedFallback = regexp.MustCompile(`(?:\|\||\?\?)\s*` + jsQuoted + `|\?[^;]*\s:\s*` + jsQuoted)
	// wipFactCoordinate is the one named exception (board-view-query-
	// correctness plan invariant 4, RT-12): the daemon-published WIP fact is a
	// framework coordinate, not owner vocabulary. Moving its display into the
	// daemon (owner decision D-4) retires this exception.
	wipFactCoordinate = "tag.key === 'work' && tag.value === 'uncommitted'"
)

// keyboardKeys are KeyboardEvent.key values: a comparison with one of these
// is a key press, whatever the receiver is named, never a tag key. A single
// character (the board's 'm') is a key press too.
var keyboardKeys = map[string]bool{"Escape": true, "Enter": true, "Tab": true, "Backspace": true, "Delete": true,
	"ArrowUp": true, "ArrowDown": true, "ArrowLeft": true, "ArrowRight": true, "Home": true, "End": true,
	"PageUp": true, "PageDown": true}

// organizationVocabularyInCode is a shape heuristic, not a proof: it names
// the first place the code visibly compiles in the owner's vocabulary — a
// view by name, a literal key or value in a move's apply/retract, a tag key
// compared or switched against a literal, a literal fallback where the
// grouping key is read from tag-key: (the 'flow' of the 2026-09-29
// incident), or the removed key cache. A key assembled at run time, or a
// literal routed through a variable, passes it; the behaviour tests
// (organization.test.mjs, session_board_test.go) are the real guard.
func organizationVocabularyInCode(code string) string {
	switch {
	case literalViewName.MatchString(code):
		return "creates a view by name"
	case literalTagPart.MatchString(code):
		return "applies or retracts a literal tag key or value"
	case strings.Contains(code, "boardGroupKeyCache"):
		return "keeps a board group-key cache"
	}
	checked := strings.ReplaceAll(code, wipFactCoordinate, "")
	for _, test := range literalKeyTests {
		for _, match := range test.FindAllStringSubmatch(checked, -1) {
			if literal := quotedText(match); !keyboardKeys[literal] && len([]rune(literal)) > 1 {
				return "compares a tag key with a literal: " + match[0]
			}
		}
	}
	for _, at := range tagKeyGrouping.FindAllStringIndex(code, -1) {
		start := strings.LastIndex(code[:at[0]], ";") + 1
		end := len(code)
		if next := strings.Index(code[at[1]:], ";"); next >= 0 {
			end = at[1] + next
		}
		if fallback := quotedFallback.FindString(code[start:end]); fallback != "" {
			return "falls back to a literal where it reads the grouping key: " + strings.TrimSpace(fallback)
		}
	}
	return ""
}

// quotedText is the text of the last quoted literal a jsQuoted match captured.
func quotedText(match []string) string {
	for index := len(match) - 1; index > 0; index-- {
		if match[index] != "" {
			return match[index]
		}
	}
	return ""
}

// stripJSComments ports scripts/vendor-lint-js.mjs stripComments (JS kind) to
// Go: comments go, strings stay, so a word in a comment is never code and
// "//" inside a string is never a comment. Unlike that port it also copies an
// escaped character outside a string whole, so a regex literal such as
// /https?:\/\// is not read as a line comment. A regex literal holding a
// quote can still read as the start of a string; there is none in these files,
// and the effect is a count that is too high, never a hidden literal. The two
// ports should stay in step.
func stripJSComments(source string) string {
	var out strings.Builder
	for i := 0; i < len(source); {
		c := source[i]
		var next byte
		if i+1 < len(source) {
			next = source[i+1]
		}
		switch {
		case c == '\\' && i+1 < len(source):
			out.WriteString(source[i : i+2])
			i += 2
		case c == '/' && next == '*':
			end := strings.Index(source[i+2:], "*/")
			if end < 0 {
				return out.String()
			}
			i += 2 + end + 2
			out.WriteByte(' ')
		case c == '/' && next == '/':
			end := strings.IndexByte(source[i:], '\n')
			if end < 0 {
				return out.String()
			}
			i += end
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(source) && source[j] != c {
				if source[j] == '\\' {
					j++
				} else if c != '`' && source[j] == '\n' {
					break
				}
				j++
			}
			j = min(j+1, len(source))
			out.WriteString(source[i:j])
			i = j
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// TestStripJSCommentsKeepsCodeAfterARegexLiteral pins the port's one
// divergence: an escaped slash in a regex literal is not a comment.
func TestStripJSCommentsKeepsCodeAfterARegexLiteral(t *testing.T) {
	code := stripJSComments("const re = /https?:\\/\\//; const k = 'flow'; // gone\n/* gone */ x")
	if !strings.Contains(code, "const k = 'flow';") || strings.Contains(code, "gone") {
		t.Fatalf("stripped = %q", code)
	}
}

// TestVocabularyHeuristicShapes pins each rejected shape and the false
// positives it must not raise.
func TestVocabularyHeuristicShapes(t *testing.T) {
	for code, rejected := range map[string]bool{
		"if (tag.key === 'flow') x();":                                      true,
		"if ('flow' === tag.key) x();":                                      true,
		"if (tag['key'] == \"flow\") x();":                                  true,
		"switch (tag.key) { case `flow`: x(); }":                            true,
		"const k = (g.startsWith('tag-key:') ? g.slice(8) : '') || 'flow';": true,
		"k = g.startsWith('tag-key:') ? g.slice(8) : `flow`;":               true,
		"if (e.key === 'Escape' || ev.key === 'm') x();":                    false,
		"const c = g.startsWith('tag-key:') ? { key: 'x' } : null;":         false,
		"const choice = t.key ? 'tag-key:' + t.key : '';":                   false,
		"if (tag.key === 'work' && tag.value === 'uncommitted') x();":       false,
	} {
		if got := organizationVocabularyInCode(stripJSComments(code)) != ""; got != rejected {
			t.Errorf("%s: rejected=%v, want %v", code, got, rejected)
		}
	}
}
