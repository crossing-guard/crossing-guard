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
	agentsPage := read("js/orchestration/agents/agent-dialog.js")
	client := read("js/orchestration/profile-api.js")
	view := read("js/orchestration/settings-profiles.js")
	// Profile import lives in the Agents pages' Import dialog
	// (agents-settings-redesign plan §3.2): one importer, no separate
	// profile list surface.
	for _, required := range []string{"settings-profiles.js", "renderProfileImporter("} {
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
	// The importer's narration strings are gone (owner rule 2026-08-31: no
	// system self-narration); the exact-bytes flow they described stays pinned.
	for _, required := range []string{"file.arrayBuffer()", "selectProfile(sourceName, bytes, preview)",
		"Preview current bytes again", "textContent", "source_digest", "bundle_digest",
		"Normalized typed value", "await onSelected(result)", "response.preview, onImported)"} {
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
