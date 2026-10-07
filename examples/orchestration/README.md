# Configured helper capability examples

These profiles are inert examples. Import and preview PROFILE.md in Agents, select
the exact revision, and deploy to an exact session with the desired runtime and
grants. A profile does not activate itself. Runtime/model selection belongs to the
deployment; memory is read through the Crossing Guard recall tools (`search_memories`,
`get_memory`), which read through the daemon — a helper never opens the store.

`recall-helper/PROFILE.md` uses the current `task.completed` event and existing
reply action. It demonstrates recall at a managed task hand-back. It does **not**
claim a before-call pause. Mid-session delivery and natural-session observation are
separate work; the before-call pause remains an explicit non-goal.

Start with automatic action off to inspect results. A reply grant plus automatic
action permits the installed host to send its attributed reply at a supported
hand-back. Keep the deployment session-scoped and use a bounded loop budget;
disable it when the demonstration finishes. The recall tools must be registered for
the selected helper runtime (`crossing-guard init --recall`); a missing tool produces an
explicit diagnostic. The memory CLI is not a helper path: a read-only Codex sandbox
refuses both the store file and the daemon's loopback port.

The generic message capability adds two alternative profiles:

| Profile | Behavior configured by its prompt | Framework capability |
| --- | --- | --- |
| `recall-message/PROFILE.md` | Search/read existing memory; send useful prior decisions | `session.turn-started`, bounded `session.messages`, `send-message` grant |
| `document-message/PROFILE.md` | Read existing project documents; send relevant guidance | The same event, context, grant, and delivery port |
| `peer-overlap/PROFILE.md` | Ask the `crossing-guard` MCP's `active_sessions` tool which other open sessions share the repository; warn when their changed files or task overlap | `session.turn-started`, bounded `session.messages`, `prior-claims`, `send-message` grant |

`peer-overlap` needs the local `crossing-guard` MCP registered for the helper's runtime.
It passes the
source session's identity as `from_session`, so peers are reported from that session's
point of view and it is never listed as its own peer. A binding fires only for sessions
whose working directory is exactly its project root, so sessions in worktrees are reported
as peers but are not warned themselves, and only one helper binding acts per signal on a
root.

`recall-message` fires on `session.turn-started` (the installed deployment's choice,
adopted 2026-09-28) and `document-message` on `session.tool-completed` — pauses every
session has, whether the daemon launched it or you did. Both read `session.messages`, the session transcript
through the harvest owner on both paths. A profile never states, and never needs to
know, who launched the session. Deploy
with **watch natural** on to include your own terminal sessions; a natural runtime
that serves a kind is listed on the signal in Settings, an unserved one shows
"incompatible here" on preview.

These profiles declare **helper** with the legacy **follower** alias and the existing
**intervention** output contract. Message delivery needs no task-control capability. Grant `send-message`
and turn on automatic action for delivery. With automatic action off, a message is
only a visible proposal; this slice adds no manual resend button. A grant is not
permission for the helper to run its own messaging commands.

Delivery is addressed to the exact source **session** (its canonical identity), never
to a task, so a terminal session and a console task take the same path. Each runtime
adapter answers per session per call: Claude and OpenCode hand the message to the
session's own next boundary (Claude: the installed hook's next prompt-submit, tool-call
or tool-result event, printed as `hookSpecificOutput.additionalContext`; OpenCode: the
installed plugin's next tool call or tool result, appended as a context-only message);
Codex uses its `queue` verb by default, with the hook carrier selectable per
`orchestration.json` `delivery.runtime_options.codex.transport`. The claim card
distinguishes queued (accepted), delivered (handed to the session's boundary —
consumption is not separately confirmed), expired (no boundary before the TTL),
unavailable, and unknown. An idle session receives at its next boundary; nothing wakes
it. Unknown/pending outcomes are never retried automatically. Settings disclose every
source runtime's transport and boundary. Existing `reply` remains a separate terminal
hand-back and, off a session the daemon does not own, is recorded as
`attended_session` rather than performed.

For one prompt at every published event, configure `trigger.event: "*"`. For a
default plus event-specific prompts, use `stages` with a quoted `"*"` entry and
explicit event entries; explicit prompts override the default. This selects the
published catalog only, skipping kinds a `session.*` kind supersedes so one fact fires
once. Coverage is per runtime and published by the agents API as `served_by` evidence:
Claude serves every session kind from its hooks, OpenCode serves
`session.tool-completed` (turn-ended once its idle canary is recorded), Codex serves
none natively until its turn/result hooks are proven to fire; the managed path serves
all of them for every runtime. A kind no runtime serves simply produces no rows.

`session.messages` supplies the session transcript — user turns included — through the
harvest owner, bounded by the configured tail (`context.transcript_tail_events`) and
the profile's byte limit; the highest sequence supplied is pinned on the run
(`source_transcript_seq`) so a provider retry reads the same cutoff. `task.messages` remains the managed-only task-stream alternative (no user
input; 512-event scan ceiling). Context and retrieved memories remain
untrusted evidence. Importance, search strategy, repeated-reminder suppression,
and interrupt criteria belong to prompts; budgets and grants belong to deployment.

Memory needs no new store or special helper integration. Verify the existing CLI
or memory tool in the actual helper environment. Machine-specific paths belong in
the daemon process environment (`PATH` and `CG_MEMORY_DIR`), not these portable examples. The measured local
installation had two stores and a scratch PATH gap; the demo selected the intended
existing store and executable explicitly without migrating data or changing global
settings. The separate `request-interrupt` grant uses the existing task controller:
it terminates an exact managed task. Use the existing correction/resume control
afterward. It is not a tool-call pause-and-release protocol.

Context is read through an injected framework port. Task final-response and messages
use the admitted event cutoff; group notes, prior claims, and tags read current data
on each attempt and record their read time. Successful empty reads satisfy required
context; unavailable providers fail required selections. Named selectors and other
declared context kinds are currently unavailable on the managed path. Every run
records `context_coverage`; this does not claim full transcript or memory snapshots.

The existing chat capability API includes `message_delivery` with `supported`,
`boundary` and `detail` fields, supplied by the runtime adapter. These are transport
facts; installed binary availability and queue receipts are separate evidence.

## One helper session per source session

A deployed follower or helper runs in ONE vendor session per source session (the layer
design's "persistent linked session, 1:1"). The first signal starts it; every later signal
is a new turn that resumes it, so the helper keeps its own working memory across the
source session. A signal that arrives while a turn is still running is not queued: it
replaces the pairing's single pending slot and launches when the turn ends, with the
wait recorded on that run (`coalesced_signals`, `waited_ms`). `max-total` therefore
counts turns per source session (shipped default 48, editable per agent); `max-concurrency`
is bounded by the runtime's concurrent-turns capability, which is one on every shipped
runtime. The helper is never told it was resumed.

## Where a helper's model runs, and how long it may take

Every example except `peer-overlap` declares `requirements.destination.locality: local-only`.
That is the safe default, and it is enforced. `peer-overlap` declares
`explicit-local-or-remote`, so read its Destination line before you deploy it.

- **Local routes.** A deployment is accepted only on a route its runtime declares
  local. On Codex that means "Local · Ollama" or "Local · LM Studio". Claude and
  OpenCode declare no local route today.
- **Hosted routes.** To run a helper on a hosted model, deploy a copy of the profile
  that declares `explicit-local-or-remote`. Transcript text and memory records then
  go to that model's provider, and the Agents card says so on the "Destination" line.
- **Deployments saved earlier.** A deployment saved before the rule existed is refused
  visibly, once per source session. It launches nothing until its profile states the
  truth.
- **What "local" means.** "Local" is where the runtime sends inference. It does not
  cover the runtime's own telemetry or the MCP servers it loads.
- **Replies on a local model.** A reply or correction resumes the **source** session
  with the deployment's model. A local model exists only on its own runtime, so an
  auto-reply deployment on a local model must be scoped to that runtime's sessions.

`limits.timeout: 2m` is enforced. A helper turn still running two minutes after it
started is stopped and recorded as a timeout, and its helper session is kept.
`limits.max-tokens` is not applied to these turns: the runtime makes its own model
calls. The card lists it under "Not applied on this host".
