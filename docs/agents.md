# Agents and memory

A profile defines a review or assistance role. Bind it to supported events and a session
scope, choose its model route, set a budget and grant actions explicitly. Profiles are
inert until selected and deployed. Start with automatic action off to inspect proposals.

Reviewers evaluate work; helpers provide information or propose actions. Some profiles
use the legacy follower role name. Provider choice depends on model/runtime configuration;
delivery capabilities have their own compatibility limits.

The release examples must demonstrate profile preview, selection, deployment, attributed
results and disabling a binding in a real supported workflow. Valid syntax alone is not proof.

A message to a session is handed to that session's own next boundary. Claude Code and
OpenCode receive it through the installed hook or plugin at the next prompt, tool call or
tool result; Codex uses its queue by default. Acceptance differs from consumption, and an
idle session receives nothing until its next boundary. See the
[examples](../examples/orchestration/README.md) for the delivery states. Natural sessions expose lifecycle signals rather than the complete
managed-task message/tool stream. Wildcard triggers cannot create missing observation.

## Spontaneous Memory

An “aha” reminder retrieves an earlier insight outside the main conversation context.
Distinguish retrieval, delivery at a supported event/hand-back/turn boundary, and interruption
of ongoing work. The first two have bounded implementation evidence. They do not establish
universal mid-reasoning interruption. Existing interruption stops a managed process; a
correction requires an explicitly resumed turn, not release of a paused tool call.

The receiving agent must assess potentially stale memories. Context never overrides
permissions. Disable a binding when it is no longer needed.

See the [profile examples](../examples/orchestration/README.md).
