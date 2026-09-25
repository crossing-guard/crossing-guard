package daemon

// Pins the Diff pane's wiring (workspace-panes implementation plan §4.3): the
// pane is registered as a pane source, its presentation defaults come from the
// console configuration, the turn boundary is dispatched from the live state
// handler, and the pane's stylesheet exists.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceDiffPaneWiring(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("static", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	app := read("js/app.js")
	for _, pin := range []string{"registerPaneSource(workspacePaneSource)", "configureWorkspaceDiff(config)", "consoleKeymap.show_files", "cg:diff-show-files"} {
		if !strings.Contains(app, pin) {
			t.Fatalf("app.js lost %q", pin)
		}
	}
	pane := read("js/views/workspace-diff.js")
	for _, pin := range []string{"id: 'workspace.diff'", "capability: 'workspace-read'", "section: 'edits'", "body_kind: kind", "/api/workspace-diff", "cg:session-turn-state", "cg:open-editor"} {
		if !strings.Contains(pane, pin) {
			t.Fatalf("workspace-diff.js lost %q", pin)
		}
	}
	// Presentation defaults are configuration (plan §8): the pane must not carry
	// the side-by-side width or a compiled preference.
	for _, forbidden := range []string{"900", "vscode://"} {
		if strings.Contains(pane, forbidden) {
			t.Fatalf("workspace-diff.js compiles in %q", forbidden)
		}
	}
	sessions := read("js/views/sessions.js")
	if !strings.Contains(sessions, "new CustomEvent('cg:session-turn-state'") || !strings.Contains(sessions, "wasRunning") {
		t.Fatal("sessions.js no longer dispatches the turn boundary from the live state handler")
	}
	css := read("css/app.css")
	for _, pin := range []string{".diff-toolbar", ".diff-file-header", ".diff-edit", ".diff-menu-item.disabled", ".diff-nowrap .diff-code"} {
		if !strings.Contains(css, pin) {
			t.Fatalf("app.css lost %q", pin)
		}
	}
	for _, kind := range []string{`"effect_before"`, `"effect_after"`} {
		if !strings.Contains(read("../govern_serve.go"), kind) {
			t.Fatalf("govern_serve.go no longer routes %s", kind)
		}
	}
}
