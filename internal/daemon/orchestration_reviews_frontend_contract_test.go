package daemon

import (
	"bytes"
	"testing"
)

func TestReviewFrontendSpeaksUnifiedClaimWireAndBrowserHasNoDirectAuthority(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	client := read("js/orchestration/review-api.js")
	agentsPage := read("js/orchestration/settings-agents.js")
	session := read("js/orchestration/session-reviews.js")
	sessionsView := read("js/views/sessions.js")
	css := read("css/app.css")
	for _, required := range []string{"/api/orchestration/reviews/settings", "/api/orchestration/reviews/binding",
		"expected_state_token", "confirmed: true", "/api/orchestration/reviews/session"} {
		if !bytes.Contains(client, []byte(required)) {
			t.Errorf("review client lost %q", required)
		}
	}
	for _, required := range []string{"Reviewer · independent tool-call review", "Review and enable",
		"Answer existing asks first", "approval_subdeadline_ms", "Disable new reviews",
		"availability unverified", "Data destination", "Update available"} {
		if !bytes.Contains(agentsPage, []byte(required)) {
			t.Errorf("Agents page reviewer binding lost %q", required)
		}
	}
	// The claim renderer reads the unified agent-claim wire: `action`, never a
	// second `decision` format.
	for _, required := range []string{"reviewCard",
		"review.action", "Observed governance decision", "Actual tool outcome",
		"not evidence that the tool did not run"} {
		if !bytes.Contains(session, []byte(required)) {
			t.Errorf("review card renderer lost %q", required)
		}
	}
	if bytes.Contains(session, []byte("review.decision")) || bytes.Contains(agentsPage, []byte("review.decision")) {
		t.Error("review frontend still reads the retired claim `decision` field")
	}
	// Review output does NOT render in the conversation pane. It is the
	// reviewer's work, not the session's content, and as stacked cards under
	// the transcript it buried the composer and read as model prose the reader
	// never asked for (owner, 2026-09-01: "that isn't the right place ... it
	// smells like AI telling slop"). Its home is the Agents surface, which
	// still renders the same cards.
	for _, forbidden := range []string{"renderSessionReviews", "session-reviews.js"} {
		if bytes.Contains(sessionsView, []byte(forbidden)) {
			t.Errorf("the session pane renders review output again: %q", forbidden)
		}
	}
	if !bytes.Contains(agentsPage, []byte("reviewCard")) {
		t.Error("the Agents surface must still render review cards — removing them from the session pane must not lose them")
	}
	for _, required := range []string{".orchestration-review-card"} {
		if !bytes.Contains(css, []byte(required)) {
			t.Errorf("review styling lost %q", required)
		}
	}
	for _, forbidden := range []string{"localStorage", "innerHTML", "insertAdjacentHTML", "mdToHtml",
		"permissionDecision", "/api/approvals", "/api/runtime-tasks", "EventSource", "WebSocket"} {
		if bytes.Contains(client, []byte(forbidden)) || bytes.Contains(session, []byte(forbidden)) {
			t.Errorf("review frontend gained forbidden direct authority/sink %q", forbidden)
		}
	}
}
