package daemon

import (
	"strings"
	"testing"
)

func TestConsoleAuthRecoveryContract(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	app := read("js/app.js")
	for _, required := range []string{"storeTokenFromHash", "hashchange", "location.reload()", "#tab=${g.tab}&session=${g.session}"} {
		if !strings.Contains(app, required) {
			t.Errorf("app.js does not pin same-tab token recovery behavior %q", required)
		}
	}

	core := read("js/core.js")
	// The token is stored by core.js as it is evaluated, before any module that
	// imports it makes its first read: stored later (in app.js's body) a page opened
	// from its token link sent its first reads with no token (rest-of-release R-1).
	for _, required := range []string{"const storeTokenFromHash = hash =>", "storeTokenFromHash(location.hash)", "localStorage.setItem('cg_token'"} {
		if !strings.Contains(core, required) {
			t.Errorf("core.js does not store the token from the link before the first read: %q", required)
		}
	}
	if !strings.Contains(core, "err.status = r.status") {
		t.Error("core API errors do not preserve HTTP status")
	}

	ui := read("js/ui.js")
	for _, required := range []string{"err?.status === 401", "This browser tab is not connected", "crossing-guard console --open", "Try again", "location.reload()"} {
		if !strings.Contains(ui, required) {
			t.Errorf("ui.js does not pin auth recovery behavior %q", required)
		}
	}
	for _, forbidden := range []string{"Authorization: Bearer", "X-CG-Token", "api-token"} {
		if strings.Contains(ui, forbidden) {
			t.Errorf("base auth recovery UI exposes API mechanics %q", forbidden)
		}
	}
}
