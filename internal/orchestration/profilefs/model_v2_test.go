package profilefs

import (
	"bytes"
	"strings"
	"testing"
)

// validHelperV2Source is a format-v2 (agents redesign) profile: authored type,
// declared tag vocabulary, priority, an open stages selector map, a declared
// reply shape, and the v2 owner budget limit keys.
func validHelperV2Source() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: design-helper
version: "1.0.0"
name: Design helper
description: Reviews completed turns and replies from project design guidance.
role: follower
type: helper
priority: 25
may-tag:
  - needs-review
  - regex-hole
stages:
  task.completed: Review the completed turn against the design documents.
  task.failed: Summarize the failure honestly and suggest one recovery step.
reply-shape: One concise paragraph grounded in the cited design documents.
execution: managed-turn
trigger:
  event: task.completed
context:
  - kind: task.final-response
    required: true
  - kind: prior-claims
output:
  kind: draft-reply
authority-requests:
  - reply
requirements:
  capabilities:
    - managed-turn
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
  loop-budget: 3
  max-group-tokens: 50000
  max-agent-tokens: 20000
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Review the returned response and draft a grounded reply.
`)
}

func TestParseCompilesV2AgentFrontmatter(t *testing.T) {
	document, err := Parse("PROFILE.md", validHelperV2Source())
	if err != nil {
		t.Fatal(err)
	}
	profile := document.Profile
	if profile.Type != "helper" || profile.AgentType() != "helper" || profile.Priority != 25 {
		t.Fatalf("type/priority = %q %q %d", profile.Type, profile.AgentType(), profile.Priority)
	}
	if len(profile.MayTag) != 2 || profile.MayTag[0] != "needs-review" || profile.MayTag[1] != "regex-hole" {
		t.Fatalf("may_tag = %v", profile.MayTag)
	}
	if len(profile.Stages) != 2 || !strings.Contains(profile.Stages["task.completed"], "design documents") ||
		profile.Stages["task.failed"] == "" {
		t.Fatalf("stages = %v", profile.Stages)
	}
	if profile.ReplyShape == "" || profile.Limits.LoopBudget != 3 ||
		profile.Limits.MaxGroupTokens != 50000 || profile.Limits.MaxAgentTokens != 20000 {
		t.Fatalf("reply shape/limits = %q %+v", profile.ReplyShape, profile.Limits)
	}
}

func TestParseRejectsUnknownStageSelectorWithoutAnyEnum(t *testing.T) {
	source := bytes.Replace(validHelperV2Source(), []byte("task.failed:"), []byte("stage.plan:"), 1)
	if _, err := Parse("PROFILE.md", source); err == nil {
		t.Fatal("unpublished stage selector was accepted")
	}
}

func TestParseRejectsV2BoundsViolations(t *testing.T) {
	cases := map[string][2]string{
		"priority":      {"priority: 25", "priority: 5000"},
		"may-tag":       {"- regex-hole", "- " + strings.Repeat("x", 65)},
		"loop-budget":   {"loop-budget: 3", "loop-budget: -1"},
		"type":          {"type: helper", "type: overseer"},
		"passive-reply": {"type: helper", "type: follower"},
	}
	for name, replacement := range cases {
		source := bytes.Replace(validHelperV2Source(), []byte(replacement[0]), []byte(replacement[1]), 1)
		if _, err := Parse("PROFILE.md", source); err == nil {
			t.Fatalf("%s violation was accepted", name)
		}
	}
}

func TestAgentTypeLegacyRoleMappingIsDeterministic(t *testing.T) {
	cases := []struct {
		role      string
		authority []string
		want      string
	}{
		{"reviewer", []string{"advise"}, "reviewer"},
		{"follower", []string{"draft-reply"}, "follower"},
		{"follower", []string{"reply"}, "helper"},
		{"coordinator", []string{"launch-profile"}, "helper"},
		{"course-corrector", []string{"request-interrupt"}, "helper"},
		{"delegate", []string{"advise"}, "helper"},
	}
	for _, item := range cases {
		profile := CompiledProfile{Role: item.role, Authority: item.authority}
		if got := profile.AgentType(); got != item.want {
			t.Fatalf("AgentType(%s %v) = %q, want %q", item.role, item.authority, got, item.want)
		}
	}
}

// TestV1CanonicalBytesUnchangedByV2Fields pins the additive invariant: a v1
// profile's canonical JSON — and so its bundle digest and every stored
// revision's integrity check — gains no v2 keys.
func TestV1CanonicalBytesUnchangedByV2Fields(t *testing.T) {
	document, err := Parse("PROFILE.md", validReviewerSource("1.0.0", "Review the command.\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"type"`, `"priority"`, `"may_tag"`, `"stages"`,
		`"reply_shape"`, `"loop_budget"`, `"max_group_tokens"`, `"max_agent_tokens"`} {
		if bytes.Contains(document.Canonical, []byte(forbidden)) {
			t.Fatalf("v1 canonical JSON gained %s: %s", forbidden, document.Canonical)
		}
	}
}

func TestParseWildcardAndIndependentDeliveryGrant(t *testing.T) {
	source := strings.Replace(string(validHelperV2Source()), "  task.completed: Review", "  '*': Review", 1)
	doc, err := Parse("PROFILE.md", []byte(source))
	if err != nil || doc.Profile.Stages["*"] == "" {
		t.Fatalf("%+v %v", doc, err)
	}
	source = strings.ReplaceAll(source, "kind: draft-reply", "kind: intervention")
	source = strings.ReplaceAll(source, "  - reply", "  - send-message")
	if _, err := Parse("PROFILE.md", []byte(source)); err != nil {
		t.Fatal(err)
	}
	for _, passive := range []string{"follower", "reviewer"} {
		if _, err := Parse("PROFILE.md", []byte(strings.ReplaceAll(source, "type: helper", "type: "+passive))); err == nil {
			t.Fatal("passive delivery accepted")
		}
	}
}
