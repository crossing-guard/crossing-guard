package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPaneHostOwnsLayoutWithoutFeatureBranches pins the pane host's contract: one
// layout owner, one host, no feature or API knowledge in either, and the strip as
// the persistent way back to any pane.
func TestPaneHostOwnsLayoutWithoutFeatureBranches(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	if _, err := staticFS.ReadFile("static/js/pane-deck.js"); err == nil {
		t.Fatal("pane-deck.js must be gone: the host is the one layout owner")
	}
	hostFiles := []string{"js/pane-host.js", "js/pane-region.js", "js/pane-splitter.js", "js/pane-strip.js", "js/pane-menu.js"}
	for _, name := range hostFiles {
		body := read(name)
		for _, forbidden := range [][]byte{
			[]byte("/api/"), []byte("sessionEvidence"), []byte("session.change"),
			[]byte("terminal"), []byte("worktree"), []byte("checkout"), []byte("git "),
			[]byte("./infopanel.js"), []byte("pane-deck.js"),
		} {
			if bytes.Contains(bytes.ToLower(body), bytes.ToLower(forbidden)) {
				t.Errorf("%s contains feature branch/owner %q", name, forbidden)
			}
		}
	}
	host := read("js/pane-host.js")
	for _, required := range [][]byte{
		[]byte("registerPaneSource"), []byte("duplicate pane source"), []byte("duplicate pane id"),
		[]byte("descriptor.catalogMatch"), []byte("descriptor.create"), []byte("controller?.dispose"),
		[]byte("visible ? 'resume' : 'suspend'"), []byte("callController(entry, 'activate'"),
		[]byte("export function setWorkspaceHidden"), []byte("export function activatePane"),
		[]byte("beginPaneDeckTransition"), []byte("endPaneDeckTransition"), []byte("configurePaneHost"),
	} {
		if !bytes.Contains(host, required) {
			t.Errorf("pane host lost contract %q", required)
		}
	}
	layout := read("js/pane-layout.js")
	for _, required := range [][]byte{
		[]byte("PANEL_PREFS_VERSION = 4"), []byte("cg_panel_prefs"), []byte("migratePanelPreferences"),
		[]byte("module_disabled"), []byte("recently_closed"), []byte("export function normalize"),
		[]byte("export function split"), []byte("export function closePane"), []byte("export function movePane"),
	} {
		if !bytes.Contains(layout, required) {
			t.Errorf("pane layout lost contract %q", required)
		}
	}
	for _, forbidden := range [][]byte{[]byte("document."), []byte("/api/"), []byte("infopanel"), []byte("sessionEvidence")} {
		if bytes.Contains(layout, forbidden) {
			t.Errorf("pure layout module contains concrete owner %q", forbidden)
		}
	}
	adapter := read("js/infopanel.js")
	for _, required := range [][]byte{
		[]byte("evidencePaneSource"), []byte("createEvidenceController"), []byte("pane?.group"),
		[]byte("capability: metadata.capability || 'evidence'"), []byte("paneModule?.placement"),
		[]byte("enabledModules"), []byte("dispose: () =>"), []byte("host.replaceChildren()"), []byte("retry.textContent = 'Retry'"),
	} {
		if !bytes.Contains(adapter, required) {
			t.Errorf("evidence adapter lost contract %q", required)
		}
	}
	if bytes.Contains(adapter, []byte("./pane-host.js")) || bytes.Contains(host, []byte("./infopanel.js")) {
		t.Fatal("pane host and evidence adapter introduced a cyclic import")
	}
	app := read("js/app.js")
	if bytes.Count(app, []byte("registerPaneSource(evidencePaneSource)")) != 1 {
		t.Fatal("application must register the evidence pane source exactly once")
	}
	for _, required := range [][]byte{[]byte("/api/console/config"), []byte("configurePaneHost("), []byte("toggleWorkspaceHidden()")} {
		if !bytes.Contains(app, required) {
			t.Errorf("application lost the console-config or keymap seam %q", required)
		}
	}
	if bytes.Contains(app, []byte("const EDITOR_SCHEME = 'vscode://file'")) {
		t.Fatal("the editor scheme is published configuration, not a compiled constant")
	}
	index := read("index.html")
	for _, required := range [][]byte{[]byte(`id="pane-strip"`), []byte(`aria-controls="refpanel"`), []byte(`id="refpanel"`), []byte(`id="refdivider"`)} {
		if !bytes.Contains(index, required) {
			t.Errorf("pane host markup lost %q", required)
		}
	}
	for _, name := range []string{"js/views/session-change.js", "js/views/session-impact.js"} {
		if !bytes.Contains(read(name), []byte("group: 'evidence'")) {
			t.Errorf("%s must join the Evidence pane as a tab", name)
		}
	}
	if bytes.Contains(read("js/views/session-plan.js"), []byte("group:")) {
		t.Fatal("Plan is its own pane kind (O-A), not an Evidence tab")
	}
	agents := read("js/orchestration/session-agents-panel.js")
	if !bytes.Contains(agents, []byte("paneModule: { placement: 'deck' }")) || bytes.Contains(agents, []byte("required: true")) {
		t.Fatal("Agents must remain optional while keeping host-level placement")
	}
}

// A deck module renders above the regions. It may be any height (the Agents
// panel grows with a session), so the module zone must shrink and scroll while
// the regions keep the configured minimum height. On an installed session with a
// busy Agents panel the regions were squeezed to 8px before this was pinned.
func TestPaneHostModulesCannotSqueezeTheRegions(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("static", "css", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(css)
	modules := cssRule(t, body, ".pane-host-modules")
	if !strings.Contains(modules, "overflow:auto") || strings.Contains(modules, "flex:none") {
		t.Fatalf(".pane-host-modules must shrink and scroll: %s", modules)
	}
	area := cssRule(t, body, ".pane-host-area")
	if !strings.Contains(area, "min-height:var(--pane-min-h") {
		t.Fatalf(".pane-host-area must reserve the configured minimum: %s", area)
	}
	host, err := os.ReadFile(filepath.Join("static", "js", "pane-host.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(host), "--pane-min-h") {
		t.Fatal("pane-host.js no longer publishes the configured minimum region height")
	}
}

// cssRule returns the declaration line of the first rule with this selector.
func cssRule(t *testing.T, body, selector string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), selector+" {") {
			return line
		}
	}
	t.Fatalf("no rule for %s", selector)
	return ""
}
