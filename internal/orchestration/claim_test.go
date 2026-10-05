package orchestration

import (
	"strings"
	"testing"
)

func TestHelperClaimEnforcesCitationContainment(t *testing.T) {
	labels := []string{"source.task", "source.lifecycle", "source.final_message"}
	claim, err := DecodeAgentClaim([]byte(`{"action":"draft_reply","message":"Follow ADR 0028.","citations":["source.final_message"]}`), "helper", 1024, labels, nil, nil)
	if err != nil || claim.Action != "draft_reply" {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	if _, err := DecodeAgentClaim([]byte(`{"action":"reply","message":"Do it.","citations":["invented"]}`), "helper", 1024, labels, nil, nil); err == nil {
		t.Fatal("accepted invented citation")
	}
}

func TestHelperCannotEscapeChildProfileAllowlist(t *testing.T) {
	labels := []string{"source.task", "source.lifecycle"}
	good := []byte(`{"action":"launch_profile","message":"Design is ready for review.","citations":["source.task"],"stage_id":"design-review","child_profile_id":"design-reviewer"}`)
	if _, err := DecodeAgentClaim(good, "helper", 2048, labels, []string{"design-reviewer"}, nil); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"action":"launch_profile","message":"Launch it.","citations":["source.task"],"stage_id":"design-review","child_profile_id":"unlisted"}`)
	if _, err := DecodeAgentClaim(bad, "helper", 2048, labels, []string{"design-reviewer"}, nil); err == nil {
		t.Fatal("accepted unlisted child profile")
	}
}

func TestPassiveTypesRejectChildFieldsAndActingActions(t *testing.T) {
	labels := []string{"source.task"}
	for _, agentType := range []string{"reviewer", "follower"} {
		raw := []byte(`{"action":"no_action","message":"x","citations":[],"child_profile_id":"c"}`)
		if agentType == "reviewer" {
			raw = []byte(`{"action":"allow","message":"x","citations":[],"child_profile_id":"c"}`)
		}
		if _, err := DecodeAgentClaim(raw, agentType, 1024, labels, nil, nil); err == nil {
			t.Fatalf("%s accepted a child profile field", agentType)
		}
	}
	if _, err := DecodeAgentClaim([]byte(`{"action":"draft_reply","message":"x","citations":[]}`), "follower", 1024, labels, nil, nil); err == nil {
		t.Fatal("passive follower accepted an acting action")
	}
	if _, err := DecodeAgentClaim([]byte(`{"action":"reply","message":"x","citations":[]}`), "unknown-type", 1024, labels, nil, nil); err == nil {
		t.Fatal("unknown agent type accepted")
	}
}

func TestDeclaredTagValidationRefusesUndeclaredTags(t *testing.T) {
	labels := []string{"source.task"}
	raw := []byte(`{"action":"no_action","message":"x","citations":[],"tags":["needs-review"]}`)
	if _, err := DecodeAgentClaim(raw, "follower", 1024, labels, nil, []string{"needs-review"}); err != nil {
		t.Fatalf("declared tag refused: %v", err)
	}
	if _, err := DecodeAgentClaim(raw, "follower", 1024, labels, nil, nil); err == nil {
		t.Fatal("tag accepted with no declared vocabulary")
	}
	if _, err := DecodeAgentClaim(raw, "follower", 1024, labels, nil, []string{"other"}); err == nil {
		t.Fatal("undeclared tag accepted")
	}
}

func TestClaimSchemaDescriptionNamesTypeActions(t *testing.T) {
	helper := ClaimSchemaDescription("helper")
	if !strings.Contains(helper, "launch_profile") || !strings.Contains(helper, "child_profile_id") {
		t.Fatalf("helper description=%q", helper)
	}
	reviewer := ClaimSchemaDescription("reviewer")
	if !strings.Contains(reviewer, "allow, deny") || strings.Contains(reviewer, "child_profile_id") {
		t.Fatalf("reviewer description=%q", reviewer)
	}
}

func TestClaimAcceptsOneEnclosingFence(t *testing.T) {
	labels := []string{"source.final_message"}
	cases := []struct {
		name string
		raw  string
	}{
		{"bare json", `{"action":"reply","message":"Continue.","citations":["source.final_message"]}`},
		{"fenced json", "```json\n{\"action\":\"reply\",\"message\":\"Continue.\",\"citations\":[\"source.final_message\"]}\n```"},
		{"fenced no lang", "```\n{\"action\":\"reply\",\"message\":\"Continue.\",\"citations\":[\"source.final_message\"]}\n```"},
	}
	for _, tc := range cases {
		claim, err := DecodeAgentClaim([]byte(tc.raw), "helper", 1024, labels, nil, nil)
		if err != nil || claim.Action != "reply" || claim.Message != "Continue." {
			t.Fatalf("%s: claim=%+v err=%v", tc.name, claim, err)
		}
	}
}

func TestClaimRefusesFenceAbuse(t *testing.T) {
	labels := []string{"source.final_message"}
	good := `{"action":"reply","message":"Continue.","citations":["source.final_message"]}`
	cases := []struct {
		name string
		raw  string
	}{
		{"prose before fence", "Here you go:\n```json\n" + good + "\n```"},
		{"prose after fence", "```json\n" + good + "\n```\nDone."},
		{"two fenced documents", "```json\n" + good + "\n```\n```json\n" + good + "\n```"},
	}
	for _, tc := range cases {
		if _, err := DecodeAgentClaim([]byte(tc.raw), "helper", 4096, labels, nil, nil); err == nil {
			t.Fatalf("%s: accepted fence abuse", tc.name)
		}
	}
	// A fence inside a JSON string VALUE is the model's message content:
	// valid JSON that decodes; refusal, if any, belongs to the delivery
	// layer's fenced-block rule — not the decoder.
	contentValue := "{\"action\":\"reply\",\"message\":\"```Continue.\",\"citations\":[\"source.final_message\"]}"
	if _, err := DecodeAgentClaim([]byte(contentValue), "helper", 4096, labels, nil, nil); err != nil {
		t.Fatalf("fence inside a value must decode: %v", err)
	}
}
