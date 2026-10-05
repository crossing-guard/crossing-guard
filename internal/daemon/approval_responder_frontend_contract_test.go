package daemon

import (
	"bytes"
	"os"
	"testing"
)

func TestApprovalResponderFrontendIsAttributionOnly(t *testing.T) {
	view, err := staticFS.ReadFile("static/js/views/approvals.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{
		[]byte("Answered through local console"),
		[]byte("Responder not recorded (legacy)"),
		[]byte("Nobody answered before the deadline"),
		[]byte("Audit only—request was already closed"),
		[]byte("line('response ID', response.id"),
		[]byte("line('submitted', response.submitted_at"),
		[]byte("line('accepted', response.accepted_at"),
		[]byte("line('responder', approvalResponderLabel(response))"),
	} {
		if !bytes.Contains(view, required) {
			t.Fatalf("approval responder view missing %q", required)
		}
	}
	for _, forbidden := range [][]byte{
		[]byte("innerHTML"), []byte("capability"), []byte("response_id:"),
		[]byte("responder:"), []byte("submitted_at:"),
	} {
		if bytes.Contains(view, forbidden) {
			t.Fatalf("approval responder view gained browser authority or unsafe rendering %q", forbidden)
		}
	}
}

func TestApprovalDecisionFrontendDTOStaysHumanCompatible(t *testing.T) {
	api, err := staticFS.ReadFile("static/js/approval/approval-api.js")
	if err != nil {
		t.Fatal(err)
	}
	// Request-bound decisions retain their exact legacy bodies. The only other
	// browser-owned field is the immutable request-bound grant option ID.
	for _, required := range [][]byte{
		[]byte("JSON.stringify({ id, decision, reason })"),
		[]byte("JSON.stringify({ id, decision, reason, selections })"),
		[]byte("grantID !== 'request'"),
		[]byte("grant_id: selectedGrant"),
	} {
		if !bytes.Contains(api, required) {
			t.Fatalf("interactive approval decision DTO changed: missing %q", required)
		}
	}
	for _, forbidden := range [][]byte{
		[]byte("capability"), []byte("responder"), []byte("response_id"), []byte("submitted_at"),
	} {
		if bytes.Contains(api, forbidden) {
			t.Fatalf("interactive decision API sends owner-controlled field %q", forbidden)
		}
	}
}

func TestApprovalOwnerDoesNotImportOrchestrationVocabulary(t *testing.T) {
	for _, name := range []string{"approvals.go", "approval_response.go", "approvals_config.go"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range [][]byte{
			[]byte("internal/orchestration"), []byte("orchestration profile"),
			[]byte("follower session"), []byte("coordinator session"),
			[]byte(`ApprovalResponder{Kind: "service"`),
		} {
			if bytes.Contains(body, forbidden) {
				t.Fatalf("%s mixes upper-layer vocabulary %q into approval ownership", name, forbidden)
			}
		}
	}
}

// TestApprovalOwnerHoldsNoRuntimeQuestionVocabulary keeps the generic side generic.
// The approval owner and the shared wire may carry choice prompts; neither may learn
// which runtime's tool produced them or how that runtime wants them answered.
func TestApprovalOwnerHoldsNoRuntimeQuestionVocabulary(t *testing.T) {
	for _, name := range []string{
		"approvals.go", "approval_response.go", "approvals_config.go",
		"../approvalchoice/choice.go",
	} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range [][]byte{
			[]byte("AskUserQuestion"), []byte("updatedInput"), []byte("multiSelect"),
			[]byte("claude"), []byte("codex"), []byte("opencode"),
		} {
			if bytes.Contains(body, forbidden) {
				t.Fatalf("%s names a runtime question surface %q", name, forbidden)
			}
		}
	}
}
