---
format-version: 1
kind: crossing-guard-orchestration-profile
id: new-follower
version: "1.0.0"
name: New follower
description: Follows a session and labels what each hand-back contains.
type: follower
role: follower
execution: managed-turn
trigger:
  event: task.completed
context:
  - kind: task.final-response
    required: true
output:
  kind: advice
authority-requests: []
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
  max-tokens: 1024
  max-retries: 0
  max-concurrency: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Read the session's last response. Apply each tag you are allowed to use only when the
response clearly shows it. Apply no tag when none fits. Never act on the
session and never restate its work.
