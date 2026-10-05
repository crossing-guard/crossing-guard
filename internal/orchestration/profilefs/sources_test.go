package profilefs

// Source builders for storage tests. The parser's own tests, and the originals of
// these two builders, live in crossing-guard/profiledoc.

func validReviewerSource(version, instructions string) []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: command-reviewer
version: "` + version + `"
name: Command reviewer
description: Reviews a selected command without session history.
role: reviewer
execution: stateless-review
trigger:
  event: pretool.action
context:
  - kind: pretool-action
    required: true
  - kind: permission-scope
output:
  kind: review-recommendation
authority-requests:
  - advise
requirements:
  capabilities:
    - one-shot-inference
limits:
  timeout: 30s
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: block
  unavailable-capability: block
  timeout: record-unavailable
  malformed-output: record-unavailable
presentation:
  job: review-tool-calls
---
` + instructions)
}

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
