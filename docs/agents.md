# Agents and memory

A profile defines a review or assistance role. Bind it to supported events and a session
scope, choose its model route, set a budget and grant actions explicitly. Profiles are
inert until selected and deployed. Start with automatic action off to inspect proposals.

Reviewers evaluate work; helpers provide information or propose actions. Some profiles
use the legacy follower role name. Provider choice depends on model/runtime configuration;
delivery capabilities have their own compatibility limits.

The release examples must demonstrate profile preview, selection, deployment, attributed
results and disabling a binding in a real supported workflow. Valid syntax alone is not proof.

Current implementation evidence supports memory recall at task hand-back and Codex message
queuing for a subsequent turn boundary. Acceptance differs from consumption; an idle source
may need its next normal user turn. Claude and OpenCode have no equivalent message transport
in this candidate. Natural sessions expose lifecycle signals rather than the complete
managed-task message/tool stream. Wildcard triggers cannot create missing observation.

## Spontaneous Memory

An “aha” reminder retrieves an earlier insight outside the main conversation context.
Distinguish retrieval, delivery at a supported event/hand-back/turn boundary, and interruption
of ongoing work. The first two have bounded implementation evidence. They do not establish
universal mid-reasoning interruption. Existing interruption stops a managed process; a
correction requires an explicitly resumed turn, not release of a paused tool call.

The receiving agent must assess potentially stale memories. Context never overrides
permissions. Disable a binding when it is no longer needed.

See the [three profile examples](../examples/orchestration/README.md).
