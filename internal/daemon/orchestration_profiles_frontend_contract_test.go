package daemon

import (
	"bytes"
	"testing"
)

func TestOrchestrationProfileFrontendIsInertExplicitAndSafe(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	agentsPage := read("js/orchestration/settings-agents.js")
	client := read("js/orchestration/profile-api.js")
	view := read("js/orchestration/settings-profiles.js")
	// Profile import lives on the Agents page (plan §9a, G-4): the importer
	// composes standalone — importing creates the agent's card; there is no
	// separate profile list surface.
	for _, required := range []string{"settings-profiles.js", "renderProfileImporter(main"} {
		if !bytes.Contains(agentsPage, []byte(required)) {
			t.Errorf("Agents page lost orchestration profile composition %q", required)
		}
	}
	for _, required := range []string{"/api/orchestration/profiles", "source_base64", "source_digest",
		"bundle_digest", "state_token", "confirmed: true", "bytesToBase64"} {
		if !bytes.Contains(client, []byte(required)) {
			t.Errorf("profile client lost exact selection contract %q", required)
		}
	}
	for _, required := range []string{"Reusable profiles", "Selecting one does not run it",
		"agent binding below", "file.arrayBuffer()", "Synthetic preview · no profile ran",
		"Select exact revision", "Preview current bytes again", "Selected · inert", "textContent",
		"Exact PROFILE.md source", "Normalized typed value", "Local profile storage", "await onSelected()",
		"response.preview, onImported)"} {
		if !bytes.Contains(view, []byte(required)) {
			t.Errorf("profile Settings flow lost %q", required)
		}
	}
	for _, forbidden := range []string{"localStorage", "innerHTML", "insertAdjacentHTML", "mdToHtml",
		"/api/runtime-tasks", "/api/approvals", "/api/orchestration/bindings", "EventSource", "WebSocket"} {
		if bytes.Contains(client, []byte(forbidden)) || bytes.Contains(view, []byte(forbidden)) {
			t.Errorf("profile frontend gained forbidden behavior/sink %q", forbidden)
		}
	}
}
