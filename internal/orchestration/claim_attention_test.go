package orchestration

import (
	"strings"
	"testing"
)

// AC-8: the generated follower contract text is byte-identical to the text
// before ask_owner existed, and the helper text differs only by the one
// ask_owner entry and its meaning.
func TestClaimContractTextChangesOnlyByAskOwner(t *testing.T) {
	const followerBefore = "Required fields: action, message, citations. citations is an array of at most 8 supplied fact labels. action must be one of: advise_user, no_action. Optional: verdict (one short line), findings (array, each {severity: info|warn|error, statement, refs}), tags (only declared tags). A ref is {kind: file|doc|event|session, path, line, session, anchor}; kind event requires session."
	const helperBefore = "Required fields: action, message, citations. citations is an array of at most 8 supplied fact labels. action must be one of: advise_user, draft_reply, launch_profile, no_action, reply, request_interrupt, send_message. Optional: verdict (one short line), findings (array, each {severity: info|warn|error, statement, refs}), tags (only declared tags). A ref is {kind: file|doc|event|session, path, line, session, anchor}; kind event requires session. For launch_profile also return child_profile_id (stage_id is optional)."
	if got := ClaimSchemaDescription("follower"); got != followerBefore {
		t.Fatalf("follower contract text changed:\n%s", got)
	}
	helper := ClaimSchemaDescription("helper")
	meaning := " ask_owner: " + actionGrants["ask_owner"].meaning + "."
	if !strings.Contains(helper, meaning) {
		t.Fatalf("helper text lacks the ask_owner meaning:\n%s", helper)
	}
	stripped := strings.Replace(strings.Replace(helper, "ask_owner, ", "", 1), meaning, "", 1)
	if stripped != helperBefore {
		t.Fatalf("helper text differs beyond ask_owner:\n%s", helper)
	}
}

// ask_owner is offered to helpers only in v1 (P2-8).
func TestAskOwnerIsHelperOnly(t *testing.T) {
	claim := []byte(`{"action":"ask_owner","message":"Decide.","citations":[]}`)
	if _, err := DecodeAgentClaim(claim, "helper", 4096, nil, nil, nil); err != nil {
		t.Fatalf("helper ask_owner refused: %v", err)
	}
	if _, err := DecodeAgentClaim(claim, "follower", 4096, nil, nil, nil); err == nil {
		t.Fatal("follower ask_owner accepted")
	}
	if grant, known := RequiredActionGrant("ask_owner"); !known || grant != "" {
		t.Fatalf("ask_owner must be a known non-acting action: %q %v", grant, known)
	}
}
