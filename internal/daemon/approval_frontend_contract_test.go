package daemon

import (
	"bytes"
	"testing"
)

func TestApprovalFrontendUsesOneProviderNeutralProjection(t *testing.T) {
	files := []string{
		"js/approval/approval-api.js",
		"js/approval/approval-projection-store.js",
		"js/session/event-stream-client.js",
		"js/approval/approval-attention-client.js",
		"js/approval/approval-card.js",
		"js/approval/approval-choice.js",
		"js/views/approvals.js",
	}
	for _, name := range files {
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range [][]byte{
			[]byte("runtime === 'claude'"), []byte("runtime === 'codex'"),
			[]byte("runtime === 'opencode'"), []byte("--permission-prompt-tool"),
			[]byte("PermissionV1.Event"), []byte("item/commandExecution/requestApproval"),
			// A runtime's own question tool and its reply field belong to that
			// runtime's adapter. If either name reaches the browser, the generic
			// choice vocabulary has stopped being generic.
			[]byte("AskUserQuestion"), []byte("updatedInput"), []byte("multiSelect"),
		} {
			if bytes.Contains(body, forbidden) {
				t.Fatalf("%s contains provider behavior %q", name, forbidden)
			}
		}
	}
	app, err := staticFS.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{
		[]byte("new EventStreamClient({"),
		[]byte("approvalStore: approvalProjectionStore"),
		[]byte("new ApprovalAttentionClient()"),
	} {
		if !bytes.Contains(app, required) {
			t.Fatalf("app bootstrap missing %q", required)
		}
	}
	view, _ := staticFS.ReadFile("static/js/views/approvals.js")
	for _, forbidden := range [][]byte{[]byte("new EventSource"), []byte("alert("), []byte("apReasonBusy")} {
		if bytes.Contains(view, forbidden) {
			t.Fatalf("approval view retained stale transport/UI behavior %q", forbidden)
		}
	}
}
