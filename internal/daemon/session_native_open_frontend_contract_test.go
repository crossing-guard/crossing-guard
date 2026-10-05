package daemon

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The desktop-app control has one browser owner, every surface that shows it
// imports that owner, and no browser file spells a desktop route: routes are
// vendor adapter facts (native-session-open-links plan §2.3, §5).
func TestNativeOpenFrontendContract(t *testing.T) {
	read := func(name string) string {
		raw, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	for name, want := range map[string][]string{
		"static/js/views/sessions.js":         {`from "../session/native-open.js"`, "nativeOpenControl(s)", "openNative(d)"},
		"static/js/views/session-evidence.js": {`from '../session/native-open.js'`, "nativeOpenControl(ctx.selection)"},
		"static/js/app.js":                    {`from './session/native-open.js'`, "setNativeOpenEnabled(config.native_open_links)"},
	} {
		source := read(name)
		for _, needle := range want {
			if !strings.Contains(source, needle) {
				t.Fatalf("%s must use the one native-open owner: missing %q", name, needle)
			}
		}
	}
	// A board card is a draggable button: the board places the control beside
	// it through its own module and never builds one inside a card.
	board := read("static/js/session-organization/board.js")
	for _, needle := range []string{`from './board-card-open.js'`, "mountCardOpen(host)", "rememberCardLink(card, session)", "cardOpenItem(card)"} {
		if !strings.Contains(board, needle) {
			t.Fatalf("board.js must use the board's native-open module: missing %q", needle)
		}
	}
	if strings.Contains(board, "nativeOpenControl") {
		t.Fatal("board.js must not build the control itself; a control inside a card is a button in a button")
	}
	if !strings.Contains(read("static/js/session-organization/board-card-open.js"), "board.appendChild(control)") {
		t.Fatal("the board control must be appended to the board, never to a card")
	}
	owner := read("static/js/session/native-open.js")
	if !strings.Contains(owner, "'session-native modeltag'") {
		t.Fatal("the control's class list changed; re-check it against the delegated click handlers")
	}
	// Neither delegated handler may come to match the control.
	app := read("static/js/app.js")
	for _, selector := range []string{"closest?.('a.session-link')", "closest?.('a.ref-ok')"} {
		if !strings.Contains(app, selector) {
			t.Fatalf("delegated handler %q changed; re-check that a.session-native stays outside it", selector)
		}
	}
	route := regexp.MustCompile(`[a-z]://(resume|threads)\b`)
	err := fs.WalkDir(staticFS, "static/js", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		if route.MatchString(read(path)) {
			t.Errorf("%s spells a desktop-app route; routes belong to the vendor adapters", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
