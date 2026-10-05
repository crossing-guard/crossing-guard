---
format-version: 1
kind: crossing-guard-orchestration-profile
id: new-helper
version: "1.0.0"
name: New helper
description: Reads the session and sends a short message when it has something useful to add.
type: helper
role: follower
execution: managed-turn
trigger:
  event: session.turn-started
context:
  - kind: session.messages
    required: true
    max-bytes: 16384
output:
  kind: intervention
authority-requests:
  - send-message
reply-shape: One short message the session can act on, or nothing.
requirements:
  capabilities:
    - managed-turn
  destination:
    locality: explicit-local-or-remote
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
  max-input-bytes: 65536
  max-output-bytes: 4096
  max-tokens: 2048
  max-retries: 0
  max-concurrency: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Read the session's recent messages. When you have something the session has not
already considered and that would change what it does next, return send_message
with one short message that says what it is and why it matters now. Otherwise
return no_action with one sentence saying why there is nothing to add.
