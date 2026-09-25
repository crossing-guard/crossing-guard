package daemon

import (
	"bytes"
	"testing"
)

func TestSessionStatusFrontendKeepsOneProviderNeutralProjection(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	status := read("js/task/session-status.js")
	for _, forbidden := range [][]byte{
		[]byte("runtime === 'claude'"), []byte("runtime === 'codex'"),
		[]byte("runtime === 'opencode'"), []byte("modified"),
		[]byte("new EventSource"), []byte("fetch("),
	} {
		if bytes.Contains(status, forbidden) {
			t.Fatalf("session status projection contains forbidden provider/transport heuristic %q", forbidden)
		}
	}
	// The browser is a RENDERER of the daemon's status frame (session-status
	// signal plan, 2026-09-01): it owns the reader's acknowledgement ledger —
	// two cursors, one per attention id space — and the words/shapes. It
	// never ranks facts or joins tasks to approvals; that logic moved to Go.
	for _, required := range [][]byte{
		[]byte("renderSessionStatus"), []byte("statusLabel"),
		[]byte("attention_source"), []byte("since_ms"),
		[]byte("const SOURCES = ['task', 'turn']"),
		[]byte("establishBaseline"), []byte("ENTRY_LIMIT = 512"),
		[]byte("session-attention-v2"),
	} {
		if !bytes.Contains(status, required) {
			t.Errorf("session status renderer lost %q", required)
		}
	}
	for _, forbidden := range [][]byte{
		[]byte("approvalMatchesSession"), []byte("taskHasVisibleResult"),
		[]byte("acknowledgedEventID"), []byte("taskEventsToTranscript"),
	} {
		if bytes.Contains(status, forbidden) {
			t.Errorf("session status renderer regained browser-side business logic %q", forbidden)
		}
	}

	view := read("js/views/sessions.js")
	for _, required := range [][]byte{
		[]byte("session-status.js"), []byte("session-status-slot"),
		[]byte("session-status-summary"), []byte("document.visibilityState === 'visible'"),
		[]byte("document.hasFocus()"), []byte("sessionAttentionStore.acknowledge"),
		[]byte("paintSessionSelection"), []byte("selectSessionRows"),
	} {
		if !bytes.Contains(view, required) {
			t.Errorf("session status view lost %q", required)
		}
	}
	if got := bytes.Count(view, []byte("taskProjectionStore.subscribe(refreshSessionStatuses)")); got != 1 {
		t.Errorf("task status projection subscriptions = %d, want one module-lifetime subscription", got)
	}
	if bytes.Contains(view, []byte("taskstatus")) {
		t.Error("session rail retained the superseded trailing task-status badge")
	}
	if bytes.Contains(view, []byte("classList.add('sel')")) {
		t.Error("session view bypasses the shared exact selection painter")
	}
	if got := bytes.Count(view, []byte("el('button', 'sess')")); got != 1 {
		t.Errorf("session row constructors = %d, want one shared buildSessionRow owner", got)
	}
	for _, required := range [][]byte{
		[]byte("createSessionRowShell"), []byte("buildSearchSessionRow(h)"),
	} {
		if !bytes.Contains(view, required) {
			t.Errorf("search results lost shared shell/compact presentation contract %q", required)
		}
	}

	css := read("css/app.css")
	for _, required := range [][]byte{
		[]byte("grid-template-columns:12px minmax(0,1fr)"),
		// Selection is inset from the rail edges and uses a muted theme-neutral
		// fill, distinct from hover (fit-and-finish plan Slice 3, SSH-RT-5/6).
		[]byte(".sess.sel { background: color-mix(in srgb, var(--text) 6%, var(--panel)); }"),
		[]byte(".sess.sel:hover { background: color-mix(in srgb, var(--text) 9%, var(--panel)); }"),
		[]byte("width:calc(100% - 12px); margin:1px 6px;"),
		// Observed native presence is a smaller quiet fill, not a bold hollow
		// ring (FF-RT-7): shape/size — not hue — separates it from attention fills.
		[]byte(".session-status-dot.native_open { width:7px; height:7px;"),
		[]byte(".sess-search .search-snippet"),
		[]byte(".session-status-dot.running"), []byte(".session-status-dot.approval"),
		[]byte(".session-status-dot.failed"), []byte(".session-status-dot.new_result"),
		[]byte("prefers-reduced-motion: reduce"),
	} {
		if !bytes.Contains(css, required) {
			t.Errorf("session status styling lost %q", required)
		}
	}
	if bytes.Contains(css, []byte(".sess:hover, .sess.sel")) {
		t.Error("session hover and durable selection must not share one indistinguishable rule")
	}
}
