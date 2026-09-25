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
	for _, pin := range []string{"if (!q) return S.sel ? renderRail() : renderSessionList();", "if (await handleBarInput(q)) return;",
		"organizedRailUrl(active, 500) || '/api/sessions?view=rail&repository_limit=500'", "mountHeaderTags(head, d);", "appendRowOrganization(d, s,"} {
		if !strings.Contains(sessions, pin) {
			t.Fatalf("sessions.js lost %q", pin)
		}
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
	for _, name := range []string{"tag-popover.js", "tag-manage.js", "view-list.js", "query-bar.js"} {
		source := readStatic(t, "js/session-organization/"+name)
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
	literalView := regexp.MustCompile(`createView\(\s*\{[^}]*name:\s*['"]`)
	literalTag := regexp.MustCompile(`(apply|retract):\s*\[\s*\{[^}]*value:\s*['"]`)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".test.mjs") {
			continue
		}
		source := readStatic(t, "js/session-organization/"+entry.Name())
		if literalView.MatchString(source) || literalTag.MatchString(source) {
			t.Fatalf("%s names a view or a tag in code; both are the owner's", entry.Name())
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
	if !strings.Contains(readStatic(t, "js/session-organization/rail-selection.js"), "offsetParent !== null") {
		t.Fatal("a shift-range may again select rows the owner cannot see (collapsed agent folds)")
	}
	if !strings.Contains(readStatic(t, "js/views/sessions.js"), "clearRailSelection();") {
		t.Fatal("a rail redraw no longer clears the multi-selection it invalidates")
	}
}
