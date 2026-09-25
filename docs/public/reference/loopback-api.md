# Authenticated loopback API inventory

> **Status: internal and experimental in 0.1.0-alpha.** This HTTP surface exists so the
> bundled console, installed hooks, and same-version CLI can communicate with the local
> daemon. It is not a supported remote API or stable third-party integration contract.

This is an incomplete inventory of the implemented loopback routes. It is included for
source inspection; passing the current documentation gate does not imply full coverage. Individual request and response shapes remain
source-level experimental contracts unless another public reference page explicitly owns
them.

## Boundary and authentication

- The daemon is intended to listen on loopback. Do not expose it through a proxy or public
  network interface.
- Every `/api/*` request requires the local token using `Authorization: Bearer <token>`,
  `X-CG-Token`, or the console's query-token bootstrap channel.
- Browser requests with an `Origin` header must match the configured local daemon origin.
- The persistent token file is owner-only and sensitive. Use `crossing-guard console --open`
  instead of copying tokens into shell history or documentation.
- `/api/v1/...` is the canonical current prefix and `/api/...` is a compatibility alias.
  The `v1` label identifies the current wire generation; it does not promise stability
  across alpha releases.
- JSON errors are not normalized across all handlers yet. Clients must handle non-2xx
  status and plain-text error bodies.

## Route inventory

### Version and runtime integration

```text
GET  /api/version
GET  /api/runtime-integrations
POST /api/runtime-integrations/{runtime}/preview-connect
POST /api/runtime-integrations/{runtime}/connect
POST /api/runtime-integrations/{runtime}/preview-disconnect
POST /api/runtime-integrations/{runtime}/disconnect
POST /api/runtime-integrations/{runtime}/watch
GET  /api/runtime-integrations/{runtime}/watch/{token}
POST /api/runtime-integrations/{runtime}/watch/{token}/confirm-visible
```

Connect/disconnect mutations require a matching, expiring, single-use preview token and an
explicit confirmation. Watch tokens prove bounded surface evidence; they are not bearer
authentication replacements.

### Governance ingestion and queries

```text
POST /api/govern/observe
POST /api/govern/observe/v1
POST /api/govern/result/v1
POST /api/govern/closure/v1
POST /api/govern/session-entry/v1
POST /api/govern/decide
GET  /api/govern/results
GET  /api/govern/health
GET  /api/chain/verify
GET  /api/govern/runtimes
GET  /api/govern/sessions
GET  /api/govern/session
GET  /api/govern/entity
```

`GET /api/chain/verify?session=<vendor/id>` recomputes one session's event chain (team
plan §5.12) and checks its tail against the anchor this daemon holds in RAM. The response
is `engine.ChainReport`: `status` (`verified` | `fork` | `gap` | `none` | `empty`), the
chained and legacy row counts, and two spans kept deliberately separate — `disk_span`
(rows verified from disk only, for example before a daemon restart) and `held_span` (rows
whose tail the daemon has held since boot). A session not seen since boot is verified for
internal consistency only and `detail` says so. `400` without `session`.

These are installed hook/console interfaces. A successful observe response proves only that
one accepted payload reached this daemon.

`GET /api/govern/session?section=edits` pages a session's recorded edits (the Diff pane's
"This session" scope): per file, each effect that retained a body, described by kind
(`replacement`, `content`, `diff`) and byte counts, in completion order. `limit` is capped
by `console.json` `edits_page_size`; `offset` pages. Bodies are released one at a time
through `section=body` with `body_kind=effect_before` or `effect_after` (a replacement's
sides), `effect_content`, or `effect_diff`. See `session-evidence-api.md`.

### Workspace

```text
GET  /api/workspaces
POST /api/workspace-selections
GET  /api/workspace-diff/checkout
GET  /api/workspace-diff
GET  /api/workspace-diff/file
GET  /api/workspace-diff/refs
GET  /api/workspace-files
GET  /api/workspace-files/read
```

`/api/workspaces` and `/api/workspace-selections` (candidate checkouts and bindings, the
gate for mutation and managed worktrees) exist only when `workspace.json` is present and
valid in the daemon data directory; otherwise they answer `503` with `workspace_unavailable`
naming the file. The four live diff routes need no file: reading git in a folder a
session recorded requires no allowlist, because the daemon already holds that folder's
transcripts and edit bodies (owner decision O-G, 2026-09-05). Their budgets come from
`console.json` (`diff.max_files`, `diff.max_status_entries`, `diff.max_file_bytes`,
`diff.context_lines`, `diff.max_refs`, `diff.git_timeout_seconds`, `diff.base_ref`) with
builtin defaults. They take the session identity the console carries (`id`, with `runtime`
accepted and ignored for parity) and never a path. `/checkout` resolves the session's one
recorded folder (a bound workspace selection supersedes it) and returns `checkout` (root,
branch, head, observed_at, dirty counts) or a typed `problem` (`no-recorded-folder`,
`ambiguous-folder` with `roots`, `not-a-repository` with `folder`, `git-timeout` naming
the configured seconds).
`/api/workspace-diff?scope=&base=` lists
the changed files of one scope (`working` HEAD→worktree plus untracked, `staged`
HEAD→index, `unstaged` index→worktree, `branch` merge-base→HEAD, `since-base`
merge-base→worktree) as typed rows (`kind` is `text`, `binary`, `mode`, `typechange`,
`rename`, `conflict`, `submodule`, or `special` for a path that is not a regular file;
counts; per-file content-sensitive `freshness`; a row whose worktree file exceeds
`diff.max_file_bytes` carries `truncated`) with `truncated` when
`diff.max_status_entries` or `diff.max_files` applied; scope failures (`git-failed`,
`git-timeout`, `base-ref-missing`, `unborn-head`) are `200` with `problem`. `/file?path=`
renders one file's unified patch bounded by `diff.max_file_bytes` (`truncated`
when cut) with its freshness as observed now; typed rows return no patch; a path the scope
does not change is `file-not-in-scope`. `/refs` lists branches and recent commits bounded
by `diff.max_refs`. A malformed scope, an option-shaped base, or a path that is not a
relative repository path is `400`. Nothing here writes to a repository.

The Files pane's routes read the same session folder with no file needed. `GET
/api/workspace-files?id&dir=` lists one directory's immediate children from git's
population (tracked plus non-ignored untracked; `dir` empty or `.` is the root and also
carries `checkout`): `listing.entries` of `{name, path, kind, untracked, size, mtime}`
where `kind` is `dir`, `file`, `symlink`, `submodule`, `missing` (tracked, no file),
`nested-repository`, or `special`, directories first; a directory that lists nothing
carries `listing.state` (`not-in-checkout`, `ignored`, `empty`, `nested-repository`);
`truncated` and `dropped` state what `console.json` `files.max_entries_per_dir` or the
listing cap cut, and `unreadable_names` counts names that are not valid UTF-8; listing a
path that is a file is `not-a-directory`. A wholly untracked directory is listed without
git's directory collapse, so that one walk is bounded by the listing cap alone. `GET
/api/workspace-files/read?id&path=` serves one listed file bounded by
`files.max_read_bytes` (truncation is judged from the bytes read, not a prior stat):
`file.{path, kind, size, mtime, text, bytes, truncated, target}` with `kind` `text`,
`binary` (no text), `symlink` (its `target`), or `special`; a path git does not list is
`file-not-in-tree`; a listed path with no file is `not-a-file`; a read the platform or
the filesystem refuses is `unreadable` or `unsupported-platform`. A `dir` or `path` that is
not a relative repository path, or that has a `.git` component in any letter case, is
`400`. Every git call runs with literal pathspecs and every open refuses to follow a
symlink.

### Ledger and audit

```text
POST /api/ledger/observe
GET  /api/ledger/verify
GET  /api/ledger/status
GET  /api/audit/rules
POST /api/audit/run
GET  /api/audit/session
```

Ledger verification is tamper-evident within the running-daemon anchor boundary. Audit is
post-hoc and cannot be described as a past live block.

### Sessions, search, files, and references

```text
GET /api/sessions
GET /api/session
GET /api/session/event
GET /api/session-activity

**Reconciled 2026-09-01 (session-status signal):** each item now carries the daemon-computed status frame: `execution` (`queued|starting|running|terminal|idle|waiting|unknown`), `authority` (`owned|observed|none`), `attention` (`none|approval|new_result|new_failure|interrupted`), `attention_id` + `attention_source` (`task|turn` — two independent id spaces), `since_ms`. `POST /api/govern/session-turn/v1` accepts turn-boundary envelopes (`trn_…`).

**Reconciled 2026-09-02 (composer-turn dedupe):** owned Claude task events of type `text`, `thinking`, `tool`, and `tool_result` carry `anchor`, the vendor's record uuid — the same value harvested events expose as `turn_anchor`. Streamed `delta`/`thinking_delta` chunks carry none.

**Reconciled 2026-09-01 (in-turn progress and session ownership):** the `state` frame of `GET /api/session/live` (the open session only, never the rail) may carry `progress` (`thinking|writing|tool`) and `progress_tool`, refining `execution=running`; its `events` carry a transient `thought_ms` on the block that followed a thought. `POST /api/runtime-tasks` accepts `allow_shared_session: true`; without it, a session another process is using right now is refused with **409** and a JSON body `{"code":"session_in_use","message":"another process is using this session right now; a reply sent from here will not reach it"}` — branch on the code, render the message.
GET /api/session-activity/stream
GET /api/search
GET /api/usage
GET /api/files
GET /api/refs/index
GET /api/refs/resolve
GET /api/refs/doc
GET /api/refs/backlinks
GET /api/codemap/descriptor
```

See [Session evidence API](session-evidence-api.md) for the bounded session projection that
has a dedicated contributor reference. Local paths and retained content are sensitive.

`GET /api/search?q=...` reads only the local relational transcript-search projection.
Its response contains `source`, `hits`, and the shared transcript `coverage` envelope;
each hit includes its indexed match `kind` (`title`, `user`, `assistant`, or tool text)
plus exact catalog/resume navigation identity when known. A zero is authoritative only
when `coverage.state` is `current`. The request never falls back to scanning raw vendor
transcripts. `catching-up`, `stale`, `incomplete`, and `unavailable` remain successful
JSON responses with closed limitations so clients can qualify partial results.

`GET /api/session-activity` returns the current bounded, provider-neutral native-session
observation snapshot. The stream is server-sent events and carries replacement snapshots,
not per-row deltas. `presence=open` currently means only that an expected live vendor
process holds that exact Claude or Codex session file open; `execution=unknown` is not a
claim that a model turn is running or waiting. OpenCode's shared session database cannot
provide exact per-session file-open evidence and remains explicitly unsupported. Every
item includes freshness/expiry and evidence qualification. As with the other loopback
routes, `/api/v1/session-activity...` is the canonical versioned alias.

### Session tags, notes, and saved views

```text
GET    /api/session-tags
POST   /api/session-tags
POST   /api/session-tags/rename
POST   /api/session-tags/purge
GET    /api/session-tags/vocabulary
PUT    /api/session-notes
GET    /api/session-views
POST   /api/session-views
PUT    /api/session-views/order
PUT    /api/session-views/{view}
DELETE /api/session-views/{view}
```

Tags and the one-line note are text the owner puts on a session to organize his own
work. They are stored apart from detector facts and agent-claimed tags, are never an
input to a rule, and are never shown to an agent. `POST /api/session-tags` takes
`{"sessions":[{"runtime","id"}…], "apply":[{"key"?,"value"}…], "retract":[…]}` for 1 to
`session_organization.bulk_selection_max` sessions and is all-or-nothing; the daemon
resolves each session itself and stores under the runtime's canonical id. A tag's key and
value are each at most 64 bytes, compare without regard to case (the first spelling is
kept), and may not contain `=`, `*`, `"`, a control character, or more than the one
colon that separates key from value; the key `tag` is reserved. `rename` changes a tag
on every session at once and keeps how long each has carried it; `purge` deletes a tag
everywhere, history included. `PUT /api/session-notes` replaces the session's note
(at most 500 bytes); empty text clears it. `GET /api/session-tags/vocabulary` returns the
owner's tags most recently used first, and every distinct tag present on any session
from any source. Nothing is ever suggested that does not already exist in the store.

Saved views are the owner's configuration: `session-views.json` in the data directory
(see [configuration](../../framework-and-configuration.md#saved-session-views)). These routes are how the
console writes that file. `GET` returns `{views, rejected, origin, state_token}`;
`origin` is `none` until the owner saves a view. Every write presents the
`state_token` of the list it was made against and is refused with `409` if the file
has changed since, or while the file holds an entry that could not be read, so neither
a second browser nor the console can overwrite a hand edit. A view's query, grouping
and sort are validated when it is written.

`GET /api/sessions` accepts, in addition to `view=rail` and `view=repository`:
`query` (the filter grammar below), `group_by` (`repository`, `runtime`, `none`, or
`tag-key:<key>`), `sort` (`newest`, `oldest`, `longest` — longest in the view first),
and `counts=1` (adds `view_counts`, sessions per saved view by id). `view=group` with
`group=<key>` pages one group of a filtered rail. A request carrying neither `query`
nor `group_by` is answered exactly as before. Rows gain `tags`, `facts` (what detectors
saw), `note`, `in_view_since` and `transcript_missing`, each omitted when empty. A view is
counted only when its membership holds still: a query using `status:`, `open:` or
search words has no count, and no view has one when a read behind the rail reached
`session_organization.tag_index_rows_max`. A malformed query is `400` with a sentence
naming the term.

Filter grammar — terms are `field:value`, `-field:value` excludes, a value with
spaces is quoted, anything else is a transcript search word confined to what the terms
left: `tag:` (any source; `key=value`, `key:value`, a bare value, `*` globs), `mine:`
(the owner's tags only), `repo:`, `runtime:`, `branch:`, `title:`, `note:`,
`touched:<7d` / `touched:>14d` (last activity; `h`, `d`, `w`), `tagged:` (age of the
owner's tag), `status:` (the status decider's execution word; a session with no
published status is `unknown`), `open:yes|no`. Terms are all required; repeating a
single-valued field, or one tag key, means either.

### Memory, notes, and handoff

```text
GET  /api/memory
POST /api/memory
GET  /api/memory/record
GET  /api/memory/pending
POST /api/memory/promote
POST /api/memory/reject
GET  /api/notes
POST /api/notes
GET  /api/handoff/generate
POST /api/handoff/publish
```

“Publish” in the handoff path means publish into the local product-owned handoff state; it
must not be interpreted as uploading to a public service.

### Policy and configuration

```text
GET  /api/policy/rules
PUT  /api/policy/rules
POST /api/policy/rules/selection/preview
POST /api/policy/rules/selection
POST /api/policy/rules/selection/rollback
POST /api/policy/rules/selection/unselect
GET  /api/policy/detectors
POST /api/policy/detectors/selection/preview
POST /api/policy/detectors/selection
POST /api/policy/detectors/selection/unselect
POST /api/policy/check
GET  /api/policy/decisions
GET  /api/policy/coverage
```

Selection mutations bind the acknowledged digest to the reviewed active state token.
Detector and rulebook owners remain separate; callers must report partial completion rather
than pretending a two-owner change is atomic.

The following compatibility routes are retired and return a retirement response. New code
must not use them:

```text
GET /api/policy
PUT /api/policy
```

### Approvals

```text
POST /api/v1/approvals/request
POST /api/v1/approvals/decision
GET  /api/v1/approvals
GET  /api/v1/approvals/stream
POST /api/v1/approvals/presence
GET  /api/v1/events/stream
GET  /api/v1/console/config
GET  /api/v1/console/view-profiles
```

`GET /api/v1/events/stream` is the console's one long-lived connection: it multiplexes
the approvals stream, the runtime-task stream, the session-activity stream, and one
optional live session (`runtime` + `id`) over a single server-sent-events response.
Every frame is an envelope `{"feed","event","cursor","payload"}` whose `payload` is the
byte-identical body the corresponding single-feed route sends; the SSE `event` name is
the feed. Per-feed cursors travel in the request (`tasks=<event id>`,
`activity=<generation>`); a `reset` frame for one feed ends the response so the client
reconnects with its cursors — and the response ends only after every feed has sent its
opening frames (the approvals snapshot, the task replay's first frame or its reset, the
activity generation when the cursor is behind, the live session's snapshot), so a client
never has to reconnect to learn another feed's opening state. The first frame (`feed: "stream"`, `event: "hello"`)
publishes the keepalive and backoff pacing. A `client_id` makes the open connection the
client's presence lease, so presence is posted only on visibility/focus changes.

`GET /api/v1/console/config` publishes the console's presentation policy from
`console.json` (layout presets in the pane-layout grammar, the default preset, split
clamp, minimum region size, recently-closed length, keymap, editor URL scheme, Diff pane
defaults, edits page size) with its origin (`builtin-default` or the file path). The
browser reads it once at boot and compiles none of these values in.
`transcript` carries the view profile selection: `default_profile` and
`role_profiles` (orchestration role to module id).

`GET /api/v1/console/view-profiles` publishes the transcript view profile modules in use,
`{"profiles":[{format_version,id,name,description,rules[],origin}],"rejected":[{path,error}]}`.
Each rule is `{match:{kind?,min_chars?,tool?},display}` with display `show`, `collapse`, or
`hide`; the first matching rule decides a row and an unmatched row is shown. `origin` is
`builtin` or the installed file. The directory is read on every request.
A held call may carry **choice prompts**: the questions the call is asking its approver.
`POST /api/v1/approvals/request` accepts `prompts`, an array of
`{id, text, header?, options[]{label, description?}, multi?, free_text?}`. Prompts that
exceed the operator's configured ceilings (`approvals.json`) are dropped rather than
refused: the approval is admitted and reports `prompts_completeness: "truncated"`, so a
requester can tell its session that nobody saw the options. Structurally unanswerable
prompts are refused with 400.

`POST /api/v1/approvals/decision` accepts `selections` alongside `id`, `decision` and
`reason`: an array of `{prompt_id, values[]}`. On an approval that carries prompts, an
`allow` **requires** exactly one selection per prompt, each value an offered option label
(or, where `free_text` is set, one typed answer); anything else is refused with 422 and
nothing is recorded as operative. A `deny` may not carry selections. The wait result of a
held request returns `selections` and `prompts_completeness` so the requester can deliver
the answer in its own runtime's shape. Answers are recorded on the approval response
beside the responder principal, separately from the claimed reason.

The stream is server-sent events and starts with an authoritative pending/history snapshot
before incremental approval events. For a verified policy-hook hold, expiry denies the
action; a late decision is advisory and does not change that result. Provider-native
callback behavior remains provider- and budget-specific. Presence is a short-lived browser-
attention lease: it can suppress a duplicate OS notification but cannot permit, deny, hide,
or extend an approval. The unversioned `/api/...` paths remain compatibility aliases; new
clients use the versioned contract. See [Approvals inbox](../how-to/approvals.md).

### Orchestration agents

```text
GET  /api/orchestration/profiles
GET  /api/orchestration/profiles/{id}
POST /api/orchestration/profiles/preview
POST /api/orchestration/profiles/select
GET  /api/orchestration/agents
PUT  /api/orchestration/agents/{binding}
POST /api/orchestration/agents/{binding}/disable
GET  /api/orchestration/managed
GET  /api/orchestration/managed/settings
POST /api/orchestration/managed/runs/{run}/act
POST /api/orchestration/managed/runs/{run}/send
POST /api/orchestration/managed/runs/{run}/resume
POST /api/orchestration/managed/provider-reroute/preview
POST /api/orchestration/managed/provider-reroute
GET  /api/orchestration/reviews/settings
GET  /api/orchestration/reviews/session
PUT  /api/orchestration/reviews/binding
POST /api/orchestration/reviews/binding/disable
GET  /api/orchestration/tags
GET  /api/orchestration/groups/{group}/notes
POST /api/orchestration/groups/{group}/notes
POST /api/orchestration/notes/{note}/retract
```

The optional orchestration layer (ADR 0028) is inactive until it is configured, and every
route here returns `503` when it is unavailable. Reading these endpoints never starts an
agent.

**Profiles are instructions; bindings are authority.** A profile is a `PROFILE.md` document
identified by content: `preview` accepts `source_base64` and returns a `source_digest`, a
`bundle_digest`, and a `state_token`; `select` requires all three plus `confirmed: true`,
and records the profile as selected but **inert** — selecting one never runs it. Authority a
profile *requests* (`reply`, `respond-approval`) is granted only by a binding, per
deployment. A profile that requests an authority it is never granted cannot use it.

**Writes are compare-and-swap.** `PUT` on a binding requires the `expected_state_token`
read from its current state and an explicit `confirmed`; a stale token is refused rather
than merged. Binding edits apply at the next admission and never to a turn already in
flight.

**Two lanes with different shapes.** Managed agent bindings (`/agents`, `/managed`) run a
follower or helper against a session hand-back through the same runtime-task path as any
other governed work, so each binding names its own runtime, model, and ordered fallback
routes. The review binding (`/reviews`) is a **singleton** whose inference path is a
literal-loopback Ollama endpoint; it is the only lane that may answer a held approval, and
its `effect` is either `report-only` or `delegated-first`.

**Delegated answers.** A delegated response carries `allow`, `deny`, or `abstain` and
is attributed to a service principal. An allow for a question carries selections:
offered option labels, or a typed answer when the prompt permits free text, bounded
by the same configured `max_free_text_bytes` used for human answers. Invalid answers
leave the approval pending; abstain leaves it for the human. Existing routing grants,
budgets, deadlines and final approval validation still apply.

These interfaces are experimental and follow the alpha compatibility rules below.

### Skills, chat, and runtime tasks

```text
GET  /api/skills
POST /api/skills/probe
GET  /api/chat/auth
POST /api/chat/auth/start
GET  /api/chat/capabilities
POST /api/chat
POST /api/runtime-tasks
GET  /api/runtime-tasks
GET  /api/runtime-tasks/stream
POST /api/runtime-tasks/{task}/interrupt
```

These are experimental local runtime-control interfaces. Availability and capability must
be read from their runtime responses; source visibility is not a support claim.

## Compatibility rules for alpha

- The bundled UI, hook, daemon, and CLI should come from the same build.
- Persisted configuration and data require explicit migration or safe refusal, even though
  HTTP response shapes may change during alpha.
- A route becoming documented here does not make it supported for third-party clients.
- Before any stable API declaration, the project needs machine-checked route/schema drift,
  normalized errors, pagination/stream contracts, and at least one independent consumer.
