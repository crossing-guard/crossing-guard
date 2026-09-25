package daemon

// Pins the Files pane's wiring (console-files-pane-plan §4): the pane is offered
// by the workspace pane source, reads only the two workspace-files routes and the
// working scope, jumps to the Diff pane through the host, and compiles in no cap
// or editor scheme.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceFilesPaneWiring(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("static", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	pane := read("js/views/workspace-files.js")
	for _, pin := range []string{"id: 'workspace.files'", "/api/workspace-files?", "/api/workspace-files/read?", "scope: 'working'", "activatePane('workspace.diff', { scope: 'working', path })", "cg:open-editor", "cg:session-turn-state"} {
		if !strings.Contains(pane, pin) {
			t.Fatalf("workspace-files.js lost %q", pin)
		}
	}
	for _, forbidden := range []string{"vscode://", "1048576", "2000"} {
		if strings.Contains(pane, forbidden) {
			t.Fatalf("workspace-files.js compiles in %q", forbidden)
		}
	}
	diff := read("js/views/workspace-diff.js")
	if !strings.Contains(diff, "panes: () => [workspaceDiffDescriptor, workspaceFilesDescriptor]") || !strings.Contains(diff, "pendingPath") {
		t.Fatal("workspace-diff.js does not offer the Files pane or keep a pending jump")
	}
	css := read("css/app.css")
	for _, pin := range []string{".files-tree", ".files-row", ".files-lines", ".files-note"} {
		if !strings.Contains(css, pin) {
			t.Fatalf("app.css lost %q", pin)
		}
	}
}
