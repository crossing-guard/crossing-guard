package daemon

import (
	"bytes"
	"testing"
)

func TestSessionLoadingKeepsOneCommittedSelectionAndStableChrome(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	view := read("js/views/sessions.js")
	start := bytes.Index(view, []byte("async function openSession(s)"))
	end := bytes.Index(view, []byte("function hydrateSessionReferences(")) // the usage strip moved to usage-strip.js
	if start < 0 || end <= start {
		t.Fatal("could not isolate openSession")
	}
	open := view[start:end]
	for _, forbidden := range [][]byte{
		[]byte("withState("),
		[]byte("selectSessionRows(s)"),
		[]byte("await refs.ready()"),
		[]byte("main.innerHTML = ''"),
	} {
		if bytes.Contains(open, forbidden) {
			t.Errorf("session open retained destructive/premature behavior %q", forbidden)
		}
	}
	for _, required := range [][]byte{
		[]byte("beginSessionTransition"),
		[]byte("generation !== sessionOpenGeneration"),
		[]byte("refs.forProject"),
		[]byte("referenceScope.start()"),
		[]byte("referenceScope.activate()"),
		[]byte("renderSessionDetail(d, allNotes, referenceScope, referenceLoad, transcriptView)"),
		[]byte("endSessionTransition"),
		[]byte("showSessionOpenFailure"),
	} {
		if !bytes.Contains(open, required) {
			t.Errorf("session open lost stable transaction contract %q", required)
		}
	}
	if bytes.Index(open, []byte("S.sel = d")) > bytes.Index(open, []byte("referenceScope.activate()")) {
		t.Error("committed selection must be established before its reference root activates")
	}

	for _, required := range [][]byte{
		[]byte("setPendingSessionRows"),
		[]byte("aria-busy"),
		[]byte("session-load-overlay"),
		[]byte("session-load-error"),
		[]byte("hydrateSessionReferences"),
		[]byte(".ref-pending[data-ref-token]"),
		[]byte("TRANSCRIPT_WINDOW_EVENTS = 500"),
		[]byte("function appendTranscriptEvents"),
		[]byte("Load older · "),
		[]byte("scroller.scrollTop = beforeTop + delta"),
	} {
		if !bytes.Contains(view, required) {
			t.Errorf("session view lost pending/hydration contract %q", required)
		}
	}

	panel := read("js/pane-host.js")
	for _, required := range [][]byte{
		[]byte("beginPaneDeckTransition"),
		[]byte("endPaneDeckTransition"),
		[]byte("activeTransition?.token === token"),
	} {
		if !bytes.Contains(panel, required) {
			t.Errorf("panel lost generation-scoped transition %q", required)
		}
	}

	refs := read("js/refs.js")
	for _, required := range [][]byte{
		[]byte("export function forProject(root)"),
		[]byte("resolveFor(key, token, baseDir)"),
		[]byte("activate: () => setProject(key)"),
	} {
		if !bytes.Contains(refs, required) {
			t.Errorf("reference client lost root-bound scope %q", required)
		}
	}

	core := read("js/core.js")
	if !bytes.Contains(core, []byte("class=\"ref-pending\" data-ref-token=")) {
		t.Error("neutral indexing references cannot hydrate in place")
	}

	app := read("js/app.js")
	restoreStart := bytes.Index(app, []byte("async function restoreLocatedSession"))
	restoreEnd := bytes.Index(app[restoreStart:], []byte("document.addEventListener('cg:session-selected'"))
	if restoreStart < 0 || restoreEnd < 0 {
		t.Fatal("could not isolate deep-link restore")
	}
	restore := app[restoreStart : restoreStart+restoreEnd]
	if bytes.Contains(restore, []byte("S.sel = null")) || bytes.Contains(restore, []byte("hidePaneHost()")) {
		t.Error("Back/Forward restore clears committed selection before target success")
	}

	css := read("css/app.css")
	for _, required := range [][]byte{
		[]byte(".sess.pending"),
		[]byte(".session-load-overlay"),
		[]byte(".session-load-error"),
		[]byte(".panel-transition"),
		[]byte(".session-reference-status"),
		[]byte(".transcript-window-control"),
	} {
		if !bytes.Contains(css, required) {
			t.Errorf("stable loading styling lost %q", required)
		}
	}
}
