package daemon

import (
	"bytes"
	"testing"
)

func TestNativeSessionActivityFrontendRemainsProviderNeutral(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	store := read("js/session/session-activity-store.js")
	client := read("js/session/event-stream-client.js")
	view := read("js/views/sessions.js")
	status := read("js/task/session-status.js")
	app := read("js/app.js")

	for name, body := range map[string][]byte{"store": store, "client": client, "status": status} {
		for _, forbidden := range [][]byte{
			[]byte("runtime === 'claude'"), []byte("runtime === 'codex'"), []byte("runtime === 'opencode'"),
			[]byte("new WebSocket"),
		} {
			if bytes.Contains(body, forbidden) {
				t.Errorf("%s contains provider/transport branch %q", name, forbidden)
			}
		}
	}
	for _, required := range [][]byte{
		[]byte("presence"), []byte("native_open"), []byte("native_stale"),
		[]byte("attention_source"), []byte("since_ms"),
	} {
		if !bytes.Contains(status, required) {
			t.Errorf("status renderer lost a frame field or presence shape %q", required)
		}
	}
	if bytes.Count(view, []byte("sessionActivityStore.subscribe(refreshSessionStatuses)")) != 1 {
		t.Error("Sessions must have exactly one module-lifetime activity subscription")
	}
	for _, required := range [][]byte{
		[]byte("activityStore: sessionActivityStore"),
		[]byte("eventStreamClient.start()"),
		[]byte("eventStreamClient.stop()"),
	} {
		if !bytes.Contains(app, required) {
			t.Errorf("app lifecycle lost one activity client %q", required)
		}
	}
	css := read("css/app.css")
	for _, required := range [][]byte{[]byte(".session-status-dot.native_open"), []byte(".session-status-dot.native_stale")} {
		if !bytes.Contains(css, required) {
			t.Errorf("activity status styling lost %q", required)
		}
	}
}
