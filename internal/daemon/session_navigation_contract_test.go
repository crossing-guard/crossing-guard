package daemon

import (
	"bytes"
	"testing"
)

func TestSessionNavigationStaticContract(t *testing.T) {
	app, err := staticFS.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := staticFS.ReadFile("static/js/views/sessions.js")
	if err != nil {
		t.Fatal(err)
	}

	for _, required := range [][]byte{
		[]byte("function parseSessionLocation"),
		[]byte("new URL(location.href)"),
		[]byte("url.searchParams.set('runtime'"),
		[]byte("url.searchParams.set('session'"),
		[]byte("history.pushState"),
		[]byte("window.addEventListener('popstate'"),
		[]byte("document.addEventListener('cg:session-selected'"),
		[]byte("restoreLocatedSession"),
		[]byte("S.sel = null"),
		[]byte("location.pathname + (bootGoto ? '' : location.search)"),
	} {
		if !bytes.Contains(app, required) {
			t.Fatalf("app navigation lost required contract %q", required)
		}
	}
	for _, forbidden := range [][]byte{
		[]byte("localStorage.setItem('session'"),
		[]byte("sessionStorage.setItem('session'"),
	} {
		if bytes.Contains(app, forbidden) || bytes.Contains(sessions, forbidden) {
			t.Fatalf("session payload/navigation duplicated in browser storage: %q", forbidden)
		}
	}

	for _, required := range [][]byte{
		[]byte("d.dataset.runtime = s.runtime"),
		[]byte("d.dataset.sessionId = s.id"),
		[]byte("cg:session-selected"),
		[]byte("aria-current"),
		[]byte("sessionOpenGeneration"),
		[]byte("generation === sessionOpenGeneration"),
	} {
		if !bytes.Contains(sessions, required) {
			t.Fatalf("session row/loader lost navigation contract %q", required)
		}
	}
	ui, err := staticFS.ReadFile("static/js/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{
		[]byte("isCurrent = () => true"),
		[]byte("if (!isCurrent()) return"),
		[]byte("withState(container, pending, workFn, render, isCurrent)"),
	} {
		if !bytes.Contains(ui, required) {
			t.Fatalf("shared async state lost current-generation contract %q", required)
		}
	}

	load := bytes.Index(sessions, []byte("api(`/api/session?runtime="))
	publish := bytes.Index(sessions, []byte("cg:session-selected"))
	if load < 0 || publish < 0 || publish < load {
		t.Fatal("session identity must be published only from the successful load path")
	}
}
