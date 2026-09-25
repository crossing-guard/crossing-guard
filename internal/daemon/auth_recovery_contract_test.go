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
	for _, required := range []string{"storeTokenFromHash", "hashchange", "localStorage.setItem('cg_token'", "location.reload()", "#tab=${g.tab}&session=${g.session}"} {
		if !strings.Contains(app, required) {
			t.Errorf("app.js does not pin same-tab token recovery behavior %q", required)
		}
	}

	core := read("js/core.js")
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
