# Authenticated loopback API inventory

> **Status: internal and experimental in 0.1.0-alpha.** This HTTP surface exists so the
> bundled console, installed hooks, and same-version CLI can communicate with the local
> daemon. It is not a supported remote API or stable third-party integration contract.

The current server registers 81 method/path patterns. This page inventories them so public
source does not hide its network surface. Individual request and response shapes remain
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
GET  /api/runtime-status
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

`GET /api/version` answers the API contract `version` a client built against, and the
facts that identify the running daemon: `build_version`, `store_schema`, `data_dir`,
`listen_addr`, and `log_path` when the service's own log file exists (a daemon started
from a terminal names none).

`GET /api/runtime-status` answers one row per registered runtime, connected or not:
`name`, `display_name`, `installed` (with `presence_reported` false when the runtime's installer cannot tell an
installed client from a surviving configuration file), `config_path`, `hook_configured`,
`hook_binary_present`, `hook_current`, `collection_only`, `hook_phases`, and what stored
live events say — `last_live_at` (the newest live event from that runtime) and
`canary_at` (the newest denial by the proof rule). `attention`, when present, is the one
thing about that runtime that needs the owner: a guided connection's own
`needs_attention` or `preview_blocked` (with its `problem` sentence), else
`hook_binary_missing`, `hook_outdated`, or `never_fired`. The observations are read at
most once per team-report interval and may be as old as `observed_at`; a connection
change or a verification watch that sees an event reads them again. `live_unavailable`
means the event store could not be read and the local facts stand alone. A runtime
with a chat capability also carries `interface_revision`, `governance_lane` and
`governance_note`. Beside the rows, `canary_rule_active` says the active rulebook would
deny the harmless canary, and is meaningful only when `canary_rule_checked` is true
(the rulebook could be asked).

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
GET  /api/team
POST /api/team/link
POST /api/team/unlink
GET  /api/team/layers
POST /api/team/layers/adopt
POST /api/team/layers/unadopt
POST /api/team/org-key/repin
POST /api/team/bundles/build
POST /api/team/content
GET  /api/team/content/sent
POST /api/team/memory/share
POST /api/team/memory/take-team-version
GET  /api/team/memory/deletions
GET  /api/govern/runtimes
GET  /api/govern/sessions
GET  /api/govern/session
GET  /api/govern/entity
```

`GET /api/govern/health` reports capture liveness. When the daemon started without its
governor (for example because the store was written by a newer version, its migration
failed, or another process held the store's write lock through start-up and one retry),
the daemon still serves. The response then has `configured: false` and a `problem`
string that says why. In that state:

- the observe, result, closure, session-entry and session-turn routes (the hook-facing
  POSTs) answer 503 with the bare body `governor not configured`, because the hook prints
  a failure body on its stderr;
- every other route that needs the governor answers 503 with the body
  `governor not configured: <problem>`. These are `GET /api/chain/verify`,
  `GET /api/govern/results`, `/entity`, `/session` and `/sessions`, and the team
  status, link, unlink, layers, adopt and unadopt routes;
- `POST /api/govern/decide` answers `allow` with `evaluated: false` (fail-open);
- `GET /api/govern/runtimes` answers `configured: false` with no runtimes;
- the `/api/workspace-diff` and `/api/workspace-files` routes answer 503 with the error
  code `review-unavailable`.

When the runtime task service did not start for the same kind of reason, the
`/api/runtime-tasks` routes answer 503 with `runtime task service unavailable: <reason>`.
The `/api/session-turn-settings` routes answer 503 with `task settings unavailable:
<reason>`, and the event stream's `tasks` feed sends one `unavailable` frame carrying the
same text. Without a recorded reason, these bodies are the bare text before the colon.

`GET /api/runtime-tasks` and `GET /api/session-activity` are the console's opening
snapshots. A 503 from either means that service is absent for the life of the daemon
process. It is never a transient failure. The console then opens `/api/events/stream`
without that snapshot, and the stream's `unavailable` frame for the feed says why.

`GET /api/chain/verify?session=<vendor/id>` recomputes one session's event chain (team
plan §5.12) and checks its tail against the anchor this daemon holds in RAM. The response
is `engine.ChainReport`: `status` (`verified` | `fork` | `gap` | `none` | `empty`), the
chained and legacy row counts, and two spans kept deliberately separate — `disk_span`
(rows verified from disk only, for example before a daemon restart) and `held_span` (rows
whose tail the daemon has held since boot). A session not seen since boot is verified for
internal consistency only and `detail` says so. `400` without `session`.

`GET /api/team` reports where this device stands with a team server (team plan §5.15):
`state` is one of `unlinked`, `pending`, `linked`, `inconsistent`, `drift`, `revoked`,
with `problem` saying why for the last three; the server, organization, device name and
key fingerprint; the pending enrollment's user code and approval address while one is
open; the last device report's time, outcome, and **the exact document sent**; and
`sends`, in words. `POST /api/team/link` with `{"server", "name"}` starts the
device-authorization enrollment and answers `202` with the code to approve. Its
refusals say which of three things happened: `409` the link's state refuses (the device
is linked, an enrollment is pending, or the state is inconsistent — a link is never
swapped silently), `400` the server URL is one the daemon will not sign to (malformed,
or plaintext beyond loopback), `502` the server could not be reached or refused the
enrollment start. `POST /api/team/unlink` revokes the key on the server when it can be reached,
removes the link files, and says whether the server acknowledged. The daemon is the only
writer of the link; the shipped `team-link-change` rule asks a person before an agent runs
these verbs, and the daemon chains every link-state change under the `daemon/team-link`
session.

While linked, `GET /api/team` also carries `outbox` — what is waiting to be sent
(`pending`, `by_kind`, `oldest_at`, and `over_high_water` against `high_water`, a flag,
never a cap), the kinds held back and why (`parked`: the server answered
`unsupported_kind`, re-tried at `next_probe_at`, or this build has no encoder for the
kind yet), and what the server refused (`dead_letter` by kind and code, `conflicts` by
kind) — and `content`: the sessions opted in (`opted_in`, keyed `<runtime>/<id>`), an
adopted organization bundle's `mandate` when one exists, the per-session `session_cap`,
and `retention` in words. `POST /api/team/content` with `{"runtime", "session",
"enabled"}` opts one session's captured tool inputs in or out: forward only (nothing
already captured is sent), `409` when the device is not linked, `400` for a malformed
body; the answer is the updated `content` object, and the change is chained under
`daemon/team-link` naming the session. With `"enabled": false, "delete_sent": true` it also
asks the team server to erase the content it already holds from this device for that
session (a deletion naming the session's wire id and the hashes of what was sent) and
turns sharing off for the session; `409` when the server holds nothing from this device
for it. `GET /api/team/content/sent?runtime=&session=` answers `{"session", "chunks"}`:
the content chunks the server this device is linked to NOW accepted from it that no later
deletion covers (content an earlier link's server accepted is not counted, and cannot be
deleted from here).

While linked, `GET /api/team` also carries `memory`: counts of this device's shared-memory
state — `shared`, `pulled` (records from teammates), `weak_repository` (repository records
identified only by a folder name, which stay on this device), `share_candidates` and
`import_candidates` (active records that could travel and are not yet shared),
`not_shareable` (queued rows refused on this device), `conflicts` and `discarded_edits`
(conflict copies), `shadowed` and `aliased` (name collisions), `held`, `deleted`,
`deleted_by_team` and `deletions_refused` (deletions the team did not take — refused, or
never delivered; what the team holds returns) — and `pull`: `last_at`, `outcome`, `error`,
`cursor`, `interval`, `unlandable` (team records this build could not read and skipped; a
different build pulls again from the start). `not_shareable` counts refusals within
`not_shareable_window`. Every count is for the current link: an acknowledgement made
under an earlier link is not counted.
`POST /api/team/memory/share` is the one action that shares records written before this
device shared them: `{}` shares every candidate except imported records and records
received from a team, `{"ids": [...]}` shares the named records (those two kinds only
this way). An imported record is never shared by being written. It answers `{"shared",
"memory"}`; `409` when not linked, `404` for an unknown id, `400` for a record that is
not active or whose scope cannot travel. `POST /api/team/memory/take-team-version` with
`{"id"}` gives up this device's edit of a team record — one the team refused, for example
— and brings the team's current revision back with the next pull; the local body is kept
as a conflict copy and nothing is sent. It answers `{"id"}`; `409` when the device is not
linked, the team does not hold the record, or a revision of it is still being sent; `404`
for an unknown id. `GET /api/team/memory/deletions` asks the team
server how far this device's recent memory deletions have reached: `{"deletions":
[{"global_id", "slug", "deleted_at", "acknowledged", "outstanding", "stale", "revoked",
"error"}], "retention"}`, where `retention` states in words what deletion does not cover
(revoked devices and backups may retain a copy).

`GET /api/team/layers`, `POST /api/team/layers/adopt` and `POST /api/team/layers/unadopt`
are described under "Shared bundles: adoption, re-pin, and building" below.

These are installed hook/console interfaces. A successful observe response proves only that
one accepted payload reached this daemon.

`GET /api/govern/session?section=edits` pages a session's recorded edits (the Diff pane's
"This session" scope): per file, each effect that retained a body, described by kind
(`replacement`, `content`, `diff`) and byte counts, in completion order. `limit` is capped
by `daemon.json` `edits_page_size`; `offset` pages. Bodies are released one at a time
through `section=body` with `body_kind=effect_before` or `effect_after` (a replacement's
sides), `effect_content`, or `effect_diff`. See `session-evidence-api.md`.

### Shared bundles: adoption, re-pin, and building

A bundle is a signed document the team server serves: at most one rulebook, shared agents
(`profile` documents), and kinds this version lists and does not apply (`detectors`). The
device pulls the catalog on its own cadence, fetches each bundle's signed bytes, and
verifies the signature against the organization key it pinned. **Every value these routes
record, show or compare is decoded from the signed bytes.** A catalog entry whose id,
revision, scope, expiry, failure mode or document list differs from its signed document is
refused, named in `reasons`, and nothing from it is recorded.

`GET /api/team/layers` answers:

- `available`: each verified, usable bundle — `id`, `organization_id`, `scope`
  (`organization` or `repository:<id>`), `revision`, `schema_version`, `key_id`,
  `expires_at`, `failure_mode`, an optional `content_policy`, `adopted`, and `status`:
  `offered` (nothing is adopted for the scope), `adopted`, `changed` (another revision is
  adopted and this one differs, so it asks), or `partial` (an adoption of it stopped
  part-way; Adopt completes it). `adopted_revision` is the revision in force when it is
  another one. `changes` lists each signed field that differs from the adopted bundle as
  `{field, label, from, to}` — failure mode, content policy, bundle format, signing key —
  one entry per field. `rules` previews the rulebook and `rule_changes`
  (`added`, `removed`, `changed`) is the rule diff against the adopted rulebook.
  `state_token` names the device state the offer was built against.
- each entry's `documents`: `kind`, `name`, `digest`, `media_type`, and `state`, one of
  `offered` (usable, not adopted yet), `adopted`, `cant_be_used`, `not_applied` (a
  `detectors` document: listed, never staged), and `id_in_use` (one of your own agents
  already uses the shared agent's id: that document is not adopted, your agent is never
  replaced, and the rest of the bundle adopts). A `profile` document also carries
  `profile_id`, `profile_name`, `text`, `change` (`new`, `unchanged`, `update`,
  `collision`), and, for an `update`, `text_diff` — lines of `{op, text}` with `op` one
  of `keep`, `add`, `remove`. `reason` holds the technical reason behind `cant_be_used`,
  `not_applied` and `id_in_use`.
- `unusable`: verified bundles this device cannot use — over a cap
  (`bundle_too_many_documents`, `bundle_too_many_rulebooks`, `bundle_body_too_large`), a
  document that does not parse or match its digest (`bundle_document_invalid`), or nothing
  this version applies (`bundle_nothing_usable`). Each has `id`, `organization_id`,
  `scope`, `revision`, `still_on_revision` (the adopted revision that stays in force),
  `code`, `document`, `reason`, and `documents`. The checks run in a fixed order: the caps
  before any parser, then every rulebook and every agent through the same parsers the
  server and the build tool use.
- `adopted`: each scope's adopted-bundle record, and `adopted_bundles`: every record,
  including those of an organization this device is no longer linked to. A record has
  `organization_id`, `organization_name`, `scope`, `bundle_id`, `revision`, `key_id`,
  `failure_mode`, `content_policy`, `expires_at`, `adopted_at`, `signed_digest`,
  `rulebook_digest` (empty for a bundle with no rulebook), `schema_version`, plus `linked`,
  `expired`, `blocking` (expired and `fail-closed`: governed tool calls in its scope are
  denied, each reason naming the organization and the date, until a refresh or Un-adopt),
  `agents` (the shared agents it lists), `agents_bundle_id`, and `rule_count` (how many
  rules its staged rulebook holds, read from this device's own copy, so it is known while
  unlinked; 0 for a bundle with no rulebook).
- `key_mismatches`: present when the server presents a bundle signed by a key other than
  the pinned one — `pinned_key_id`, `pinned_fingerprint`, `presented_key_id`,
  `presented_fingerprint`, `bundles`. Nothing signed by the presented key is offered or
  adopted until a person re-pins.
- `pinned_org_key` (`key_id`, `public_key`, `fingerprint`, `pinned_at`) and `reasons`
  (bundles the pull refused, and problems with a current adoption).

A pulled revision whose revision number is not greater than the adopted one for its
organization and scope is refused by name. A newer revision whose signed content equals
the adopted bundle's in every field except `id`, `revision`, `expires_at`, `created_at`
and the signature value is **refreshed with no prompt**: the record takes the new bundle
id, revision and expiry, and a `team.bundle.refreshed` event is chained. Any other
difference asks (`status: "changed"`), and until it is adopted the adopted bundle stays in
force — including its failure mode and its content policy. The one exception follows a
re-pin: a bundle whose only difference is its signing key id, where the record's key is
the one that re-pin replaced and the bundle's is the current pin, refreshes too.

`POST /api/team/layers/adopt` takes `{"scope", "digest": "<verified bundle id>",
"state_token": "<the offer's state_token>", "surface": "console" | "command"}`. All three of
`scope`, `digest` and `state_token` are required (`400` without one): read the offer from
`GET /api/team/layers` first, as the console and `crossing-guard layers --adopt` do.
It adopts from the bytes the device verified: the rulebook is staged, each agent is handed
to the agent store, and the adopted-bundle record is written last. The answer is
`{"adopted": <record>, "bundle_id", "revision", "offer": <the entry as it reads now>}`, so
the offer reads adopted in the same response. It refuses with `400` for a malformed body,
`404` when no verified bundle offers that scope and id (an unusable bundle is never
adoptable), and `409` when the device is not linked, when the revision is not newer than
the adopted one, or when the offer is stale — an agent or the adoption changed after the
offer was built, or `state_token` is not the offer's. A stale adoption changes nothing and
rebuilds the offer. Adopting turns nothing on: a shared agent runs only after a place is
turned on for it.

`POST /api/team/layers/unadopt` takes `{"scope", "organization_id": "<optional>",
"surface"}` and works whether or not the device is linked. It turns off the places that
adoption governs, removes the agents' adoption record (each agent the adoption wrote stays
listed as no longer shared), removes the adopted-bundle record, and answers
`{"unadopted", "organization_id", "bundle_id", "revision", "was_adopted", "places_off":
{"managed": [...], "review": bool}, "offers": [...]}`. Adopting again does not turn the
places back on. `organization_id` is needed only when two organizations' bundles hold the
same scope and the one meant is not the linked organization's. `400` for a malformed body
or a scope that is neither `organization` nor `repository:<id>`.

An agent whose current version an adoption wrote is read-only on the device: the profile
and draft routes refuse an edit with the problem code `adopted_read_only`, and Duplicate
makes your own agent under a new id.

`POST /api/team/org-key/repin` takes `{"fingerprint": "<the presented key's
fingerprint>", "surface": "console" | "command"}`. It replaces the pinned organization key
with the presented key of `key_mismatches` whose fingerprint the request names, chains
`team.org-key.repinned` (both keys and the surface), pulls, and answers
`{"pinned_org_key", "replaced_key_id", "replaced_fingerprint"}`. `400` when the fingerprint
or the surface is missing; `409` when the device is not linked, when no other key is
presented, or when the fingerprint is not a presented key's — the pin is then unchanged.
Until a device re-pins, what it adopted keeps working to its expiry. Unlinking removes the
pin; adoptions stay and run to their expiry.

`POST /api/team/bundles/build` builds the next revision of a scope's bundle on a linked
device. The body is `{"scope": "organization" | "repository:<id>" | "repository", "cwd",
"agents": [...], "remove_agents": [...], "rules": bool, "failure_mode", "content_policy":
"off" | "consent" | "mandated", "expires_in"}`; a bare `repository` scope is resolved from
`cwd`, which must be inside a checkout this device has identified. The daemon pulls, takes
the scope's published bundle, removes and adds the named agents (their current published
versions on this device), replaces the rulebook with the device's own when `rules` is
true, carries `failure_mode` and `content_policy` forward unless the request changes
them (a first bundle takes `team.json`'s `bundle` defaults), mints a new bundle id, sets
`revision` to the published one plus one and `schema_version` to `1.1`, and validates the
result with the caps and the shared parsers. It writes the **unsigned** document to
`<data directory>/bundles/<scope>-r<revision>.json` and answers `{"path", "signed_path",
"sign_command", "bundle_id", "organization_id", "scope", "revision",
"published_revision", "schema_version", "failure_mode", "content_policy", "expires_at",
"documents": [{"kind", "name", "digest", "change": "added" | "removed" | "changed" |
"unchanged", "profile_id", "text_diff"}], "rule_changes", "changes"}`. `sign_command`
already contains the path. The daemon never holds or reads the organization's private
key: signing is `crossing-guard bundle sign`, a command that asks no daemon. `400` for a
malformed body or value, `409` when the device is not linked, when the result would carry
no document or breaks a cap or does not parse, when a named agent is not published here,
or when the published revision cannot be read on this device; `502` when the team server
cannot be reached.

All five routes answer `503` when the daemon has no governor. The shipped
`team-link-change` rule asks a person before an agent runs `crossing-guard bundle`,
`org-key`, `layers`, or calls the adopt, un-adopt, re-pin or build routes.

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
`daemon.json` (`diff.max_files`, `diff.max_status_entries`, `diff.max_file_bytes`,
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
`truncated` and `dropped` state what `daemon.json` `files.max_entries_per_dir` or the
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
GET /api/session/related
GET /api/session-activity

**Reconciled 2026-09-26 (child thread identity):** `GET /api/session` carries `identities`, the ids that name the session, its own id first.
- Read a session's agent runs and tags under these ids.
- `thread_id` stays a fact: the thread the rollout belongs to. For a subagent it names the **parent**, so it is not one of the subagent's identities.
- A binding's `scope_session` is still compared with `id`, `meta_id` and `thread_id`, because a subagent's activity is recorded under its parent's thread.
- `usage.delegated` counts only the session's own descendants.
- `has_change_evidence` on `GET /api/sessions` rows goes to the rows a thread id names, never to a subagent that carries it.

**Reconciled 2026-09-01 (session-status signal):** each item now carries the daemon-computed status frame: `execution` (`queued|starting|running|terminal|idle|waiting|unknown`), `authority` (`owned|observed|none`), `attention` (`none|approval|new_result|new_failure|interrupted`), `attention_id` + `attention_source` (`task|turn` — two independent id spaces), `since_ms`. `POST /api/govern/session-turn/v1` accepts turn-boundary envelopes (`trn_…`).

**Agent asks on the frame (2026-09-29, escalation-delivery plan).** Beside the attention ladder, each item may carry a helper agent's line for the owner, every field omitted when empty: `ask_id` (the ask's place in claim-settle order, its own id space, source `agent`; acknowledging it acknowledges every ask settled up to it), `ask_text` (the newest unresolved ask, plain text cut at `session-stream.json` `attention.ask_line_max_chars`), `ask_agent` (the agent's name from its profile, its profile id when the profile cannot be read), `ask_count`, `draft_count` and `draft_text` (replies a helper proposed that were not sent), and `ask_state: "unknown"` when the ask source could not be read or the orchestration host is still opening (the other ask fields are then absent; the ladder is unaffected; a read failure is also reported once in `GET /api/orchestration/managed/settings` `problem`). An ask or draft resolves when a turn starts in the session that the daemon did not launch and that is not a sub-agent re-entry. `approval` (and an input request) outranks an ask; an unseen ask outranks the unread markers.

**Reconciled 2026-09-02 (composer-turn dedupe):** owned Claude task events of type `text`, `thinking`, `tool`, and `tool_result` carry `anchor`, the vendor's record uuid — the same value harvested events expose as `turn_anchor`. Streamed `delta`/`thinking_delta` chunks carry none.

**Reconciled 2026-09-01 (in-turn progress and session ownership):** the `state` frame of `GET /api/session/live` (the open session only, never the rail) may carry `progress` (`thinking|writing|tool`) and `progress_tool`, refining `execution=running`; its `events` carry a transient `thought_ms` on the block that followed a thought. `POST /api/runtime-tasks` accepts `allow_shared_session: true`; without it, a session another process is using right now is refused with **409** and a JSON body `{"code":"session_in_use","message":"another process is using this session right now; a reply sent from here will not reach it"}` — branch on the code, render the message.
GET /api/session-activity/stream
GET /api/sessions/peers
POST /api/session-message/send
GET /api/session-message/invocations
GET /api/session-message/health
GET /api/search
GET /api/usage
GET /api/usage/breakdown
GET /api/session/usage
GET /api/files
GET /api/refs/index
GET /api/refs/resolve
GET /api/refs/doc
GET /api/refs/backlinks
GET /api/codemap/descriptor
```

`GET /api/session/related?runtime=&id=` (documented 2026-09-26) returns
`{runtime, id, related: [...], coverage}`. Each row carries a `provenance`: `observed`
(stated by the runtime's own files) or `caused` (an agent run Crossing Guard admitted).
The two kinds of row are never merged.

- `kind`. Observed: `spawned`, `messaged`, `waited_on`, `read_context_of`. Caused:
  `reviewed-by`, `reviews`, `resumed-by` (an agent run whose task resumed this same
  session).
- `direction`: `parent`, `child`, `descendant` (a nested native child; `via` names the
  child that spawned it), `peer`, or `self`.
- `openable` is always present. It is false when the runtime rules the id out as a
  session: a native subagent's agent id is listed but not openable. True means "not ruled
  out"; `GET /api/session` can still answer 404 (for example, a session not yet scanned).
  Link a row only when this is true, and handle the 404.
- `description`: text the runtime recorded for a native child.
- `runs` / `replies`: a caused row stands for the runs read between the same two sessions
  in the same role, with the newest run's state. The read covers the newest 100 relations
  per identity, and `coverage` says when it hit that limit. A run whose counterpart
  session is not known yet stays its own row.
- `coverage` joins every note about what could not be read; an empty string means
  complete. A non-empty coverage never means "no related sessions".

See [Session evidence API](session-evidence-api.md) for the bounded session projection that
has a dedicated contributor reference. Local paths and retained content are sensitive.

`GET /api/search?q=...` reads only the local relational transcript-search projection.
Its response contains `source`, `hits`, and the shared transcript `coverage` envelope;
each hit includes its indexed match `kind` (`title`, `user`, `assistant`, or tool text)
plus exact catalog/resume navigation identity when known. A zero is authoritative only
when `coverage.state` is `current`. The request never falls back to scanning raw vendor
transcripts. `catching-up`, `stale`, `incomplete`, and `unavailable` remain successful
JSON responses with closed limitations so clients can qualify partial results.
`event_limit` is how many matching events were read before hits were grouped one per
session; a client that filters hits filters after that cap.

`GET /api/sessions/peers?runtime=&id=&cwd=&cwd_source=&scope=repository|all[&from_runtime=&from_id=][&place_only=1]`
answers "who else is working in this repository right now" for the recall tools. When `runtime`/`id` name a
catalog session (under the runtime's own identity rule, so a child rollout is never its
parent), every row of that session is excluded and `caller.identified` is true; otherwise
nothing is excluded and one `same_checkout` row may be the caller. `from_runtime`/`from_id`
move the place to a watched session and exclude it too; `caller.from_session_state` is
`not_found` when that session is not in the catalog. Candidates are every open session; each is labeled
`same_checkout`, `sibling_worktree` (same git common dir, different checkout — worktrees are
one repository), `other_repository` (`scope=all` only) or `unresolved` (with `reason`). The
`caller` block and every sibling carry `git`: `branch`, `base` (resolved as the Diff pane
resolves it) with `ahead`/`behind`, `upstream` (`state:"none"` = not pushed), `changed_files`
(`scope` `since_base` or `uncommitted`, capped, with `total`) and `pull_request`
(`unavailable`). Siblings carry `overlap_files`, computed on the uncapped lists. Every fact
group has `state: measured|unavailable`. When session presence is unavailable the response is
`state:"unavailable"`, never an empty list. Crossing Guard's own agent sessions (helper and follower sessions, found through the managed-run
and helper-session linkage for exactly the open candidates) are excluded unless `include_agents=1`, which lists them with `agent_of`;
`agent_sessions` (`excluded|included|unknown`) and `agent_sessions_excluded` say which.
`place_only=1` returns only the caller block
(repository root, checkout root, `memory_repository` label). Bounds come from `daemon.json`
`recall` (`peers_max` per relationship, git timeouts, concurrency, route deadline).
When the store cannot be read, sessions known only from recorded change evidence are not
candidates and `has_change_evidence` is false; `change_evidence_problem` then states the
store's reason (omitted otherwise). The `view=rail` answer of `GET /api/sessions` carries the
same field.

`GET /api/usage` and `GET /api/usage/breakdown` read the model calls the usage recorder
stored (token-usage-analytics plan §3.7). The recorder reads every Claude, Codex and
OpenCode source on the interval in `usage-history.json` and keeps the calls after the vendor
removes its files, so every figure is a sum of recorded calls:

- Both responses carry `coverage`: `state` (`current`, `catching-up`, `incomplete` when a
  source could not be read or the recorder could not write, `unavailable` only when the
  route could not read the store), `sources_discovered`, `sources_recorded`,
  `sources_pending`, `sources_failed`, `sessions_without_source` (sessions with recorded
  calls whose source the recorder's last listing no longer names), `recorder_error` (the
  recorder's last pass could not write; the figures are readable but not advancing) and
  `as_of`, and `agents`: whether the split into Crossing Guard agents' work is `current`,
  `incomplete` (the read of which sessions are agents was cut at `usage-history.json`
  `report.agent_sessions_max`) or `unavailable` (that read failed; the split is withheld,
  and agent calls are never counted as a session's own).
- Work splits three ways (session usage breakdown plan): `main` is a session's own calls,
  `subagent` its native subagents' calls (a Claude subagent file, or a child session that
  names it as its parent, at any depth), and `agent` the calls of Crossing Guard agent
  sessions working for it, with their own descendants.
- `GET /api/usage` returns `totals` and `by_runtime` (`calls`, `input_tokens` excluding
  cache, `cache_read`, `cache_create`, `output_tokens` including reasoning,
  `cache_hit_rate` (absent when no input was stated), `reasoning_tokens` when any call
  stated it, `reasoning_stated_calls`,
  `other[]`, `total`: every stated token), `work` and `work_by_runtime` (the same totals
  per kind), `by_type` (subagent and agent types: `type` is a subagent source's
  vendor-published role, or an agent's run profiles, or `no role stated`; `kind`,
  `runtime`, `members` (distinct subagents or agents), `roots` (the root sessions they
  worked for) and the totals; at most `report.max_groups`, the rest counted in
  `by_type_omitted`), `sessions` and `no_data` (root sessions with any call, and root
  sessions whose sources were read to the end and hold no call anywhere under them), UTC
  `days` and `days_by_runtime` (`input`, `output`, `cache_read`, `cache_create`, `calls`,
  `other[]`), `top_context` and `top_total` rows (`runtime`, `id`, `title`, `project`,
  `modified`, `total`, `context`, `hit_rate` (absent when undefined), `model`, `cost[]`,
  `source_present`, `main`, `subagent` and `agent` (`{calls, total, output_tokens}`),
  `subagents`, `agents`, `agent_profiles`), `cost[]` (one line per unit and basis) and
  `cost_coverage` (`with_cost`, `without_cost`, counted in calls). A row is a root
  session: its own calls, every descendant session's and every agent serving any of them,
  rolled up; `context` is the root's own latest call. A row whose `source_present` is false
  carries no title. With `coverage.agents` `unavailable`, `work`, `work_by_runtime`,
  `by_type`, `sessions`, `no_data` and the rows are withheld; the totals stand.
- `GET /api/usage/breakdown` takes `group` (repeatable: `runtime`, `model`, `effort`,
  `client`, `delegation`, `member` (who did the work: an agent's native session id, a
  subagent's agent id, a child session's canonical id, or the session's own id),
  `session` (the recorded session a call belongs to) and `agent_type`), `bucket` (`day`,
  or `week`: the UTC Monday on or before the call's UTC date), `from` and `to` (UTC dates,
  `from` inclusive, `to` exclusive, absent means open), `runtime` and `delegation`
  (`main`, `subagent` or `agent`). Grouping by `effort` or `client` always adds `runtime`.
  With `coverage.agents` `unavailable`, a request that groups or filters by `delegation`,
  `member` or `agent_type` returns no groups. It returns `coverage`, `window`, `group_by`, `bucket`,
  `groups[]` and `omitted_groups` (groups past `usage-history.json`
  `report.max_groups`, ranked by calls). Each group has `key`, `calls`, `sessions`,
  `input`, `cache_read`, `cache_write`, `output` and `reasoning` as `{sum, stated_calls}`,
  `reasoning_share` over the `reasoning_share_calls` that stated reasoning and output,
  `reasoning_per_call`, `context_per_call` over `context_calls`, `cache_hit_rate` (only
  when every call stated all three input classes), `parts[]` (`{of, id, label, count}`:
  labelled parts of one class, such as cache lifetimes of `cache-write`), `other[]`,
  `cost[]` (`{unit, basis, amount, calls}`) and `readers[]` (`{reader, calls}`). A ratio is
  absent, never zero, when its denominator is zero or unstated. An unknown dimension,
  bucket, filter or date is a 400.
- Ids, model names, effort values and client versions are opaque and returned as stated.
- `GET /api/session` `usage` is the session's recorded own calls with `reasoning_stated_calls`
  and `as_of`, `usage.delegated` (its native subagents' calls, folded the same way) and
  `usage.agents` (the calls of Crossing Guard agents working for it); both carry
  `children`, the number of subagents or agents counted. Each is absent when there is
  nothing to count, and `usage.agents` also when the agents cannot be read. `usage` is
  absent until the recorder has read the session.
- `GET /api/session/usage?runtime=&id=` is one session's usage, split three ways and read
  once, so it agrees with `GET /api/session` `usage` at the same `as_of`. It returns
  `coverage` (as above), `as_of` (the latest write over the session's own sources, its
  subtree's and its agents'), `main` (the session's own calls: `calls`, `input_tokens`,
  `cache_read`, `cache_create`, `output_tokens`, `reasoning_tokens` when stated,
  `reasoning_stated_calls`, `cache_hit_rate`, `other[]`, `total`, `first_at`, `last_at`,
  `peak_context`, `models[]`, `series[]` of `{at_ms, context}`, at most
  `report.session_series_points`, and `series_unavailable` when the series could not be
  read), `subagent` and `agent` (the same totals; `agent` is
  absent when `coverage.agents` is `unavailable`), `subagents[]` (each with `id` (the id
  Related sessions uses for that child), `runtime`, `role`, `parent` (the member it hangs
  from, when stated), `depth` (when the runtime states one) and the totals) and `agents[]`
  (each with `id` (the agent's native session id), `runtime`, `role`, `profiles[]`,
  `openable`, `call_times[]` (at most `report.session_agent_ticks`, every n-th past the
  bound; the rest counted in `call_times_omitted`) and the totals). Both lists are cut at
  `report.session_members_max`, largest total first; the rest are counted in
  `subagents_omitted` and `agents_omitted`. Relative to the session asked for: its own
  calls are `main` even when it is itself a subagent or an agent.

`POST /api/session-message/send?caller_runtime=&caller_id=` (session-message-cross-vendor plan
§4–§5) places **one attributed message into one exact watched session** on behalf of an agent
session — the delivery design's reserved `deliver_attended` authority. The body is
`{"runtime","session_id","message"}`; the caller identity rides the query and is
**attribution, not authentication**: the bearer token is the only authentication, and the
admission contract is the boundary. The reply is a typed receipt — `invocation_id`, `state`
(`pending|refused|accepted|delivered|expired|unavailable|unknown`), `tier`
(`queued-delivery` for a hook target, `socket-post` for a direct post), `boundary`, `detail` —
and every outcome is terminal on the ledger record. Admission, checked before any delivery
call: the deployment grant `orchestration.json` `delivery.deliver_attended` (off until
selected), the caller must be an open watched session, the target must be an open watched
session that is not the caller and is inside `delivery.send_scope` (`repository` default |
`all`), the per-target budget (`max_pending_per_session`, one budget across both transports)
and per-caller window (`max_sends_per_caller_window`), the loop bound (a send that would close
a cycle through accepted deliveries inside the delivery TTL is refused, naming the cycle), and
the duplicate rule (an identical caller+target+message digest inside the delivery TTL is
refused as a duplicate citing the first invocation; a repeat after expiry is a new send).
A refused send mints a `refused` record — the admission fact survives. The message travels
wrapped (`attributedInvocationMessage`: named source session, invocation id, "agent-provided
message, not operator authorization"), so no transport can present it as operator speech, and
the receiver's own inbound controls stay in force. Hook targets wait for the session's next
boundary (the carrier claims exactly like a helper send; `delivered` is reachable only through
that handoff); a direct post's terminal success is `accepted` — transport success is never
consumption. `GET /api/session-message/invocations?limit=` reads the ledger newest-first.
The send tool does not start or resume sessions. If a target is absent or is not
observed open, resume the exact target in its native runtime and request delivery
again after openness is observed. An `active_sessions` caller's `identified=true`
means its identity was found; `state=available` means the presence observation is
available. Neither field proves that the caller itself is currently open. Native
subagents have their own canonical presence identity even when their resume handle
names their parent's thread.
`GET /api/session-message/health` is the doctor's report-only probe: table presence,
invocation count, and the stuck-record count the sweeper would settle `unknown`; it writes
nothing.

`GET /api/usage` and `GET /api/usage/breakdown` read the model calls the usage recorder
stored (token-usage-analytics plan §3.7). The recorder reads every Claude, Codex and
OpenCode source on the interval in `usage-history.json` and keeps the calls after the vendor
removes its files, so every figure is a sum of recorded calls:

- Both responses carry `coverage`: `state` (`current`, `catching-up`, `incomplete` when a
  source could not be read or the recorder could not write, `unavailable` only when the
  route could not read the store), `sources_discovered`, `sources_recorded`,
  `sources_pending`, `sources_failed`, `sessions_without_source` (sessions with recorded
  calls whose source the recorder's last listing no longer names), `recorder_error` (the
  recorder's last pass could not write; the figures are readable but not advancing) and
  `as_of`, and `agents`: whether the split into Crossing Guard agents' work is `current`,
  `incomplete` (the read of which sessions are agents was cut at `usage-history.json`
  `report.agent_sessions_max`) or `unavailable` (that read failed; the split is withheld,
  and agent calls are never counted as a session's own).
- Work splits three ways (session usage breakdown plan): `main` is a session's own calls,
  `subagent` its native subagents' calls (a Claude subagent file, or a child session that
  names it as its parent, at any depth), and `agent` the calls of Crossing Guard agent
  sessions working for it, with their own descendants.
- `GET /api/usage` returns `totals` and `by_runtime` (`calls`, `input_tokens` excluding
  cache, `cache_read`, `cache_create`, `output_tokens` including reasoning,
  `cache_hit_rate` (absent when no input was stated), `reasoning_tokens` when any call
  stated it, `reasoning_stated_calls`,
  `other[]`, `total`: every stated token), `work` and `work_by_runtime` (the same totals
  per kind), `by_type` (subagent and agent types: `type` is a subagent source's
  vendor-published role, or an agent's run profiles, or `no role stated`; `kind`,
  `runtime`, `members` (distinct subagents or agents), `roots` (the root sessions they
  worked for) and the totals; at most `report.max_groups`, the rest counted in
  `by_type_omitted`), `sessions` and `no_data` (root sessions with any call, and root
  sessions whose sources were read to the end and hold no call anywhere under them), UTC
  `days` and `days_by_runtime` (`input`, `output`, `cache_read`, `cache_create`, `calls`,
  `other[]`), `top_context` and `top_total` rows (`runtime`, `id`, `title`, `project`,
  `modified`, `total`, `context`, `hit_rate` (absent when undefined), `model`, `cost[]`,
  `source_present`, `main`, `subagent` and `agent` (`{calls, total, output_tokens}`),
  `subagents`, `agents`, `agent_profiles`), `cost[]` (one line per unit and basis) and
  `cost_coverage` (`with_cost`, `without_cost`, counted in calls). A row is a root
  session: its own calls, every descendant session's and every agent serving any of them,
  rolled up; `context` is the root's own latest call. A row whose `source_present` is false
  carries no title. With `coverage.agents` `unavailable`, `work`, `work_by_runtime`,
  `by_type`, `sessions`, `no_data` and the rows are withheld; the totals stand.
- `GET /api/usage/breakdown` takes `group` (repeatable: `runtime`, `model`, `effort`,
  `client`, `delegation`, `member` (who did the work: an agent's native session id, a
  subagent's agent id, a child session's canonical id, or the session's own id),
  `session` (the recorded session a call belongs to) and `agent_type`), `bucket` (`day`,
  or `week`: the UTC Monday on or before the call's UTC date), `from` and `to` (UTC dates,
  `from` inclusive, `to` exclusive, absent means open), `runtime` and `delegation`
  (`main`, `subagent` or `agent`). Grouping by `effort` or `client` always adds `runtime`.
  With `coverage.agents` `unavailable`, a request that groups or filters by `delegation`,
  `member` or `agent_type` returns no groups. It returns `coverage`, `window`, `group_by`, `bucket`,
  `groups[]` and `omitted_groups` (groups past `usage-history.json`
  `report.max_groups`, ranked by calls). Each group has `key`, `calls`, `sessions`,
  `input`, `cache_read`, `cache_write`, `output` and `reasoning` as `{sum, stated_calls}`,
  `reasoning_share` over the `reasoning_share_calls` that stated reasoning and output,
  `reasoning_per_call`, `context_per_call` over `context_calls`, `cache_hit_rate` (only
  when every call stated all three input classes), `parts[]` (`{of, id, label, count}`:
  labelled parts of one class, such as cache lifetimes of `cache-write`), `other[]`,
  `cost[]` (`{unit, basis, amount, calls}`) and `readers[]` (`{reader, calls}`). A ratio is
  absent, never zero, when its denominator is zero or unstated. An unknown dimension,
  bucket, filter or date is a 400.
- Ids, model names, effort values and client versions are opaque and returned as stated.
- `GET /api/session` `usage` is the session's recorded own calls with `reasoning_stated_calls`
  and `as_of`, `usage.delegated` (its native subagents' calls, folded the same way) and
  `usage.agents` (the calls of Crossing Guard agents working for it); both carry
  `children`, the number of subagents or agents counted. Each is absent when there is
  nothing to count, and `usage.agents` also when the agents cannot be read. `usage` is
  absent until the recorder has read the session.
- `GET /api/session/usage?runtime=&id=` is one session's usage, split three ways and read
  once, so it agrees with `GET /api/session` `usage` at the same `as_of`. It returns
  `coverage` (as above), `as_of` (the latest write over the session's own sources, its
  subtree's and its agents'), `main` (the session's own calls: `calls`, `input_tokens`,
  `cache_read`, `cache_create`, `output_tokens`, `reasoning_tokens` when stated,
  `reasoning_stated_calls`, `cache_hit_rate`, `other[]`, `total`, `first_at`, `last_at`,
  `peak_context`, `models[]`, `series[]` of `{at_ms, context}`, at most
  `report.session_series_points`, and `series_unavailable` when the series could not be
  read), `subagent` and `agent` (the same totals; `agent` is
  absent when `coverage.agents` is `unavailable`), `subagents[]` (each with `id` (the id
  Related sessions uses for that child), `runtime`, `role`, `parent` (the member it hangs
  from, when stated), `depth` (when the runtime states one) and the totals) and `agents[]`
  (each with `id` (the agent's native session id), `runtime`, `role`, `profiles[]`,
  `openable`, `call_times[]` (at most `report.session_agent_ticks`, every n-th past the
  bound; the rest counted in `call_times_omitted`) and the totals). Both lists are cut at
  `report.session_members_max`, largest total first; the rest are counted in
  `subagents_omitted` and `agents_omitted`. Relative to the session asked for: its own
  calls are `main` even when it is itself a subagent or an agent.

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
(see [configuration](configuration.md#saved-session-views)). These routes are how the
console writes that file. `GET` returns `{views, rejected, origin, state_token}`;
`origin` is `none` until the owner saves a view. Every write presents the
`state_token` of the list it was made against and is refused with `409` if the file
has changed since, or while the file holds an entry that could not be read, so neither
a second browser nor the console can overwrite a hand edit. A view's query, grouping
and sort are validated when it is written. `PUT /api/session-views/{view}` replaces the
stored view: send every field you want kept, including `board` and `record_kind`
([saved session views](configuration.md#saved-session-views)), because a field left out is
deleted. The console sends back the whole view it loaded. A `POST` or `PUT` that succeeds answers with
the views document plus `view_notes`, the notes on the written view's query keyed by its
id (omitted when there is nothing to note); a note never rejects a write. An API writer
should read `view_notes` after every write: a query that selects nothing because a key
was written where a value belongs (`tag:flow` for `tag:flow=*`) is saved and noted, not
refused. Write-time notes are taken over every tag the daemon holds; the rail's
read-time notes are authoritative.

`GET /api/sessions` accepts, in addition to `view=rail` and `view=repository`:
`query` (the filter grammar below), `group_by` (`repository`, `runtime`, `none`, or
`tag-key:<key>`), `sort` (`newest`, `oldest`, `longest` — longest in the view first),
and `counts=1` (adds `view_counts`, sessions per saved view by id, and `view_notes`,
the notes on each saved view's query by id). `view=group` with
`group=<key>` pages one group of a filtered rail: `mode` (`all` or `open`, required),
`offset`, and `limit` (default 15, at most 50). With `owner_groups=1` on a `tag-key:`
grouping rows are placed by the owner's tags only, as a board's columns and a board view's
rail both read them: `limit` defaults to `session_organization.board_column_cards_max` and is
capped at the larger of that and 50; `total` says how many the group holds. `view=rail`
takes the same `owner_groups=1` to count its `tag-key:` groups by the owner's tags only (a
board's columns); other groupings ignore it on both.

`board_view=<view id>` on `view=rail` or `view=group` names a saved board view, and the
daemon places rows as that board does: by the owner's tags under the board's key, as
`owner_groups=1` does, and then, for a row that carries none, by the view's placement
rules (`board.placement` in [saved session views](configuration.md#saved-session-views)),
first match in list order; a row no rule matches is in the group with the empty key. The
rules are read from the views file, never from the request. `limit` behaves as under
`owner_groups=1`. The request's `group_by` must be the view's; a `board_view` that names no
view, a view that is not a board, or another grouping is `400`, as is a rule that can no
longer be bound (a glob past `glob_expansion_max`), named by its place and column. A board
that has rules answers `503` naming `session_organization.tag_index_rows_max` when the tags
were read only in part, since it would otherwise place sessions wrongly; a board without
rules still answers. A `view=rail` answer for a board with rules adds `placement_counts`
(per rule, in list order, how many of the view's sessions the rule placed) and
`placement_notes` (`[{rule, column, notes}]`, the notes on the rules' filters, when any).
Rows of a `view=group` answer placed by the owner's tags or by a rule carry `placed_by`
(`owner` or `rule`) and `in_column_since` (Unix seconds: when the tag was applied, or when
the row came to satisfy the rule); a row the owner placed whose rules name another column
carries `observed_group`, that column. A view write for a board answers `placement_notes`
beside `view_notes`.

A request carrying neither `query` nor
`group_by` is answered exactly as before. Rows gain `tags`, `facts` (what detectors
saw), `note`, `in_view_since` and `transcript_missing`, each omitted when empty. A view is
counted only when its membership holds still: a query using `status:`, `open:` or
search words has no count, and no view has one when a read behind the rail reached
`session_organization.tag_index_rows_max`. A malformed query is `400` with a sentence
naming the term. A filtered rail also carries `query_notes`: notes on its own query,
each `{term, suggest, problem}` — the term as typed, the term probably meant, and one
sentence to show beside the query. Today a note is given for a key-less `tag:` or
`mine:` term whose word is no tag's value but is a tag key (for `mine:`, one of the
owner's keys). Notes, like counts, are withheld when the tags could not be read in
full; they never change what a query matches. A filtered rail also carries `match_total`,
the number of sessions the query matched before grouping and `repository_limit`. It is
absent wherever a saved view's count would be: a query with `status:`, `open:` or search
words, or tags read in part. A query that matches nothing answers `0`. The console's
Settings › Session views page uses it to count a filter while it is being typed.

Under a `tag-key` grouping a group's key is the tag value in lower case, so `view=group`
reads `group=Review`, `group=review` and `group=REVIEW` as the same group, and the
response's `group` is that key (`review`). Repository and runtime groups are matched
exactly. A saved board view whose columns differ only by letter case is refused (`400`).

Filter grammar — terms are `field:value`, `-field:value` excludes, a value with
spaces is quoted, anything else is a transcript search word confined to what the terms
left: `tag:` (any source; `key=value`, `key:value`, `key=*` for any value under key,
`*` globs; a bare value matches values only, never a key), `mine:`
(the owner's tags only), `repo:`, `runtime:`, `branch:`, `title:`, `note:`,
`touched:<7d` / `touched:>14d` (last activity; `h`, `d`, `w`), `tagged:` (age of the
owner's tag), `calls:>5` / `lines:<20` (`<` or `>` and a whole number: the session's model
calls with usage records, the row's `turns`; and its transcript `lines`; both zero for a
session whose transcript is no longer found; refused in a flow's stage queries), `status:`
(the status decider's execution word; a session with no published status is `unknown`),
`open:yes|no`. Terms are all required; repeating a
single-valued field, or one tag key, means either.

### Memory, notes, and handoff

```text
GET  /api/memory
POST /api/memory
GET  /api/memory/record
GET  /api/memory/records
GET  /api/memory/conflicts
GET  /api/memory/search
GET  /api/memory/pending
POST /api/memory/promote
POST /api/memory/reject
POST /api/memory/propose
POST /api/memory/tags
GET  /api/memory/tags
GET  /api/memory/by-tag
GET  /api/memory/config
GET  /api/notes
POST /api/notes
GET  /api/handoff/generate
```

`GET /api/handoff/generate?runtime=&id=` answers the mechanical extract of a session as a
draft for the send sheet: `{"title", "markdown", "remaining", "session_ref"}`. File paths
under the session's checkout are repository-relative. `remaining` is a prefill read from
the list items of the session's last agent message; nothing asks the session. The route
that wrote a handoff into a checkout (`POST /api/handoff/publish`) is removed: a handoff
is never written into a repository. Sending one is under *Handoff between members* below.

`POST /api/memory` is the console's record write. Without an `id` it creates a new `pending`
record under a freshly minted id and can never modify an existing one (a clash answers 409).
With an `id` it edits that existing record (unknown id: 404; `claim`, the title, is required
on an edit too), re-drafting it `pending`; fields
the request does not carry — an empty `body`, an absent or `null` `tags`/`aliases` — are kept,
and `[]` clears a list. `tags` and `aliases` are the record's content labels (the owner-tag
grammar is `POST /api/memory/tags`): each list holds at most 64 unique values, tags up to 100
and aliases up to 200 characters, with no comma, control character, or surrounding space. A
refused value answers 400 with the reason; a store failure answers 503. `GET /api/memory`
lists active records only; read new records with `GET /api/memory/records?status=pending`.
`POST /api/memory/propose` applies the same label rules and the same never-overwrite create.
An edit may carry the `revision` it read; when the stored record has since moved (a
teammate's revision landed) the save answers 409 and nothing is written. An edit of a
record shared with or received from a team must carry it (409 without). Each record in
`GET /api/memory` carries `revision` and, for a record at repository or organization scope
or one received from a teammate, `team`: `global_id`, `scope_type`, `shared`, `can_travel`,
`identity_note` (why a repository record stays on this device), `origin` (`local` |
`pulled`), `author` (the member the team server authenticated for the last landed
revision), `server_revision`, `in_sync`, `rejected_here`, `collision` (`alias` |
`shadowed`) with `wire_slug`, `conflicts`, `imported`, and `refused` — the code the team
refused this record's latest revision with, when it did (the edit made here is not the
team's; `POST /api/team/memory/take-team-version` brings the team's back). A team record rejected on this
device stays listed with status `rejected`. `GET /api/memory/conflicts?id=<global id>`
returns `{"conflicts": [...]}` — conflict copies, newest first (all of them without `id`):
a local version that a teammate's revision or deletion displaced, with `reason`
(`stale_base` | `pulled_over_edit` | `deleted`), `title`, `body`, `by_author`,
`local_revision`, `created_at`. They are kept, never merged.

`GET /api/memory/search?q=&repository=&cwd=&category=&tag=&limit=&via=` is the memory store's keyword
search, and the only one: the recall server and the `memory search` command both call it. `q` or
`tag` is required; a tag alone returns every record carrying it, newest first. `repository`
compares labels without case. `cwd` (an absolute folder) filters as recall does: every user
and organization record, repository records identified by a remote only when the folder is
a checkout of that remote, and weak repository records by folder name. Rejected records are never hits. Hits carry frontmatter, `pending`
(an unreviewed proposal, disclosed rather than filtered), `score`, `why` and a bounded `excerpt`;
`total` and `truncated` state the cap (`recall.memory_search_limit_max`). Every call is appended
to the memory recall log as `mcp-search`, or `cli-search` with `via=cli`; any other `via` is a
400.

`GET /api/memory/record?id=&via=` returns one record in full: `id`, `title`, `category`,
`scope_type`, `repository`, `tags`, `aliases` (arrays, never null), `source`, `origin`,
`superseded_by`, `verified_at`, `verified_by`, `created`, `updated` (RFC 3339, UTC), `format`,
`body`, `pending`, `revision`, `status`, the scope and team facts (`global_id`, `scope_id`,
`repository_identity`, `share_state`, `sync_origin`, `team_author`, `server_revision`, `in_sync`,
`collision`, `wire_slug`, `identity_note`, `reject_reason`) and its cited `sources`. It is logged as `get`, or `cli-get` with `via=cli`.
`GET /api/memory/records?status=active|pending|rejected&cwd=` returns `{"records": [...]}`, every
record in that status in the same shape without `sources`, newest-updated first; with `cwd`
(the SessionStart hook sends its folder) it also returns `recall_scope` — `label`,
`repository_id` when the folder is a checkout with one origin remote, and `resolution`
(`git` | `folder` | `unresolved`) — resolved by the daemon so the hook runs no git; any other status
is a 400, and it writes no recall-log line. These three reads, the console's `GET /api/memory` and
`GET /api/memory/pending`, and `GET /api/memory/tags` and `GET /api/memory/by-tag` answer 503 with
the store's reason when the store cannot be read (for example a store written by a newer
Crossing Guard) — never an empty result or "not found" (a failed query after the store opened
answers 500 on `/tags` and `/by-tag`); `/record` answers 404 only for an id that does not exist. A store that has not been created yet, in a daemon that has never opened one,
has no records: those reads answer empty (and `/record` 404). A store file that disappears
after the daemon opened it answers 503 naming the missing path.

`POST /api/memory/propose` is the agent recall server's one write-shaped door. It requires the
owner's separate consent (`propose.enabled` in `memory.json`, default off), must name the
session it comes from (`session: "<runtime>/<session-id>"` becomes the record's citation),
is bounded per session (`propose.per_session_max`), and can only ever create a `pending`
record — a human promotes. `GET /api/memory/config` answers the propose consent and the
memory search budgets in one place; the recall server's propose tool reads it per call, so
a flip in the file is obeyed by the next proposal with no restart. Its `store.index_path` names
the index this daemon reads (resolved once at start from `--data`, else `$CG_INDEX`, else
`~/.crossing-guard/index.sqlite`); the `memory` read commands compare it with their own store
and refuse a mismatch.

`POST /api/memory/tags` applies and retracts owner tags on memory records with the same
`key:value` grammar and limits as session tags; `records` names record ids. `GET /api/memory/tags`
lists the distinct tags in use with their counts. `GET /api/memory/by-tag?key=&value=` lists the
records carrying one tag. Owner tags are organization content: never a rule input or agent
context.

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

`POST /api/policy/check` dry-runs a command through the static tier. It answers
`decision`, `rule` (when one fired), `evaluator` and `raw`, and adds
`state_rules_not_evaluated` (an integer, omitted when zero) when the active rulebook holds
rules that can stop a call (deny, ask or redact) over `session:`/`target:`/`agent:` facts: a
dry run has no session state, so those are decided at run time by the daemon's stateful
tier, not by the preview. Observe and warn state rules never block and are not counted.
It adds `rules_undecided` (an integer, omitted when zero) when a deny or ask rule could
not be decided because it reads the tool, a path or a destination: a command preview
names none, so the rule did not fire here and the live call may differ.

`POST /api/govern/decide` adds `undecided` (an integer, omitted when zero): the deny or
ask stateful rules that read `target:` state on a call that resolves no single target (a
shell command). They did not fire.

`POST /api/audit/run` and `GET /api/audit/session` add `rules_not_fully_audited` (an
integer), and `GET /api/audit/rules` lists the same
rules by id in `not_fully_audited`: armed rules that read a fact a past session does not
hold (the command, tool or target of one call, or a session fact the daemon writes live
only). Such a rule still reports through any branch the audit can decide.

`GET /api/policy/coverage` returns each rule's computed label (`engine[]`) for the user
rulebook as loaded at request time — the rules the stateful tier reloads on every decision
and the hook's standalone tier loads — classified with the governor's detectors.
`rulebook` gives its `path`, `origin` and `digest`. Team layers are per checkout and are
not labeled here (`note`); `crossing-guard coverage` in a checkout labels them. When the
daemon has its own compatibility policy file (`--policy`, its `$CG_POLICY`, or
`<data>/policy-engine.json`), `compatibility` reports its `path` and `origin` with
`labeled: false`: only the dev ledger reads it, and a hook loads whatever invocation file
its own environment resolves. When the rulebook cannot load, or no governor detector set
resolved, `engine` is `null` and `error` says why. The older `legacy` key is gone.

Each rule's `reach` names the tiers that load it on this host. A rule with a
`session:`, `target:` or `agent:` term is labeled at the stateful tier's reach when that
tier is armed, and otherwise at a static reach — `stateful tier not armed on this
platform`, or `the daemon's stateful tier is not running` when the governor is down. At a
static reach no tier evaluates the rule — the hook's tiers skip every rule that reads
state — so it cannot fire, whether its state term is positive or negated; it is labeled
INERT and the term's `note` says `state is never read at this reach`. Report-only
(`silent-log`) and `warn` state rules in the user rulebook act only in the audit and are
labeled at harvest reach.

A rule is labeled over each live tier that loads it. The hook's two tiers decide over the
same facts — this action's detector tags, the raw `command` and the exact `tool` — and
differ only in the rule sets they load; the daemon's stateful tier adds state. The two
hook tiers evaluate only rules with no state term, and the stateful tier only rules that
have one. A rule can fire only when **one** tier can fire its whole predicate, so
`all:[tool=Bash, session:x=y]` can fire in the stateful tier (and nowhere when that tier
is not armed), and a rule joining a detector tag to `command` can fire at either hook
tier. The per-rule `tiers` array lists each tier's own verdict (`tier`, `loads`,
`detection_label`, `can_fire`, `unverified`), and each term's `tiers` names the tiers in
which it has a producer. The engine tier loads this rulebook only when a hook's own
invocation file exists with no rules or is the rulebook itself; the daemon cannot see that
file, so that row's `loads` is `unknown`. Because both hook tiers carry the same facts,
the rule's label does not depend on it. At harvest reach a term the dry run cannot read
(the `command`, `tool` or `target:` of one call, a live-only session fact) is inert, with
the term `note` `not readable by this dry run`. A term's
producers are detectors plus declared state producers: the daemon's own session facts
(for example `session:work=uncommitted`, written at checkpoint settle) and the
model-claimed tags `agent:<binding>:<tag>` that saved bindings may write, read from this
daemon's store. Per rule, `can_fire` means the rule is **proven** able to fire, and
`unverified` means the daemon could not see a term's producers (its store could not be
read), so firing is neither claimed nor ruled out. INERT is reported
only when `can_fire` and `unverified` are both false. Per term, `undetectable` and
`unverified` follow the same split, `note` says why a term is unverified or unread, and `limits` lists known (not exhaustive) gaps of
the term's unknowable producers, such as a model claim's.

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
POST /api/v1/approvals/grant/revoke
GET  /api/v1/approvals
GET  /api/v1/approvals/stream
POST /api/v1/approvals/presence
GET  /api/v1/events/stream
GET  /api/v1/console/config
PUT  /api/v1/console/config/transcript
PUT  /api/v1/console/config/appearance
GET  /api/v1/console/view-profiles
PUT  /api/v1/console/view-profiles/{id}
DELETE /api/v1/console/view-profiles/{id}
GET  /api/v1/console/themes
PUT  /api/v1/console/themes/{id}
DELETE /api/v1/console/themes/{id}
GET  /api/v1/console/appearance
PUT  /api/v1/console/appearance/{id}
DELETE /api/v1/console/appearance/{id}
GET  /appearance.css
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
`daemon.json` (layout presets in the pane-layout grammar, the default preset, split
clamp, minimum region size, recently-closed length, keymap, editor URL scheme, Diff pane
defaults, edits page size) as `{origin, config, problems, state_token}`. `origin` is
`builtin-default` (no file), the file path, `last-good` when a hand edit broke the file and the
daemon kept the last value that parsed, or `invalid` when the file does not parse and nothing
has parsed since start (the defaults apply); `problems` says why, in the words doctor prints.
The daemon re-reads `daemon.json` when the console writes it and, at most every
`console_reload_check_ms` (250–60000, default 1000), when a stat shows a hand edit. The
Diff review budgets are read at start-up only. The browser compiles none of these values in.
`transcript` carries the view profile selection: `default_profile` and
`role_profiles` (orchestration role to module id). `appearance.module` selects the
appearance. `console_limits` bounds appearance values and `console_write_bytes_max` bounds
one write body. A selection that does not resolve degrades only its own section to the
built-in and is listed in `problems`.

`PUT /api/v1/console/config/transcript` takes `{state_token, default_profile?,
role_profiles?}` and `PUT /api/v1/console/config/appearance` takes `{state_token, module}`.
Each writes a minimal overlay into `daemon.json`: only its own keys, never the resolved
configuration. Keys the owner wrote by hand are kept with their values; the file is re-written
with sorted keys and two-space indentation. A role set to `null` drops that
role's entry so the built-in mapping applies again; `null` is never written, and a
built-in mapping can be re-pointed but not removed. `state_token` is the one the config
read returned (`""` when there is no file); a stale token is 409, and a selection that does
not resolve is 422. The response is the new config read.

`GET /api/v1/console/view-profiles` publishes the transcript view profile modules in use,
`{"profiles":[{format_version,id,name,description,rules[],origin,builtin,state_token}],"rejected":[{path,error}]}`.
Each rule is `{match:{kind?,min_chars?,max_chars?,tool?,fact?},display}` with display `show`,
`collapse`, or `hide`; the first matching rule decides a row and an unmatched row is shown.
`max_chars` and `fact` need `format_version` 2, which also refuses `collapse` on
`tool_call`, `tool_result` and `thinking` rows (already one line). `fact` is a detector fact
(`exec:run`) checked by syntax only. `origin` is `builtin` or the installed file; `builtin`
says a built-in of that id exists; `state_token` is the installed file's token (`""` when
none). The directory is read on every request.

`PUT /api/v1/console/view-profiles/{id}` takes `{state_token, profile}`; the route id must
equal `profile.id`. The module is validated exactly as the loader reads it before anything
is written, then replaces `<dataDir>/view-profiles/<id>.json` atomically at mode 0600.
`DELETE /api/v1/console/view-profiles/{id}?state_token=` removes the installed file, so a
built-in of that id applies again. Stale token 409; invalid module 422; absent file 404; a
module with no built-in that `daemon.json` selects is refused with 409 naming the key.
Both return the module list.

`GET /api/v1/console/themes` and `GET /api/v1/console/appearance` return the same catalog,
`{themes[], appearances[], rejected[], limits, selected}`. A theme is
`{format_version, id, name, scheme, tokens{}, labels{<runtime>:{bg,fg}}}` plus `origin`,
`builtin`, `state_token`, `inherited` (tokens it takes from the built-in theme of its
scheme) and `unknown_labels` (label keys that name no registered runtime; kept, never
rendered). An appearance is `{format_version, id, name, theme_dark, theme_light, follow_os,
pinned_scheme?, accent?{dark?,light?:{accent?,accent_ink?,accent_fg?}}, ui_font, mono_font,
text_size, transcript_width, chat_width}`; only the built-in carries `type_steps`. `PUT` and
`DELETE` on `/api/v1/console/themes/{id}` and `/api/v1/console/appearance/{id}` follow the
view-profile rules (`{state_token, theme}` / `{state_token, appearance}`); deleting a theme an
appearance uses, or the selected appearance, is 409. Font stacks accept generic family
keywords and names of letters, digits, spaces, dots, hyphens and underscores only.

`GET /appearance.css` renders the selected appearance as CSS custom properties (theme
tokens per scheme, fonts, type steps `--fs-1`…`--fs-15`, reading widths, runtime label
colours). It is outside `/api` because a stylesheet link cannot send the token, so it
answers only a Host that is this listener, refuses `Sec-Fetch-Site: cross-site`, and sends
`Cross-Origin-Resource-Policy: same-origin`. On any internal failure it returns an empty
sheet; `tokens.css` holds the built-in themes.

`GET /api/v1/session` rows carry `facts` on `tool_call` rows: the detector facts
(`key:value`) the row classifies as, which a view profile's `fact` matches. Live-stream
`events` deltas carry them too; `snapshot` rows carry no `facts`.
A held call may carry **choice prompts**: the questions the call is asking its approver.
`POST /api/v1/approvals/request` accepts `prompts`, an array of
`{id, text, header?, options[]{label, description?}, multi?, free_text?}`. Prompts that
exceed the operator's configured ceilings (`approvals.json`) are dropped rather than
refused: the approval is admitted and reports `prompts_completeness: "truncated"`, so a
requester can tell its session that nobody saw the options. Structurally unanswerable
prompts are refused with 400.

Runtime-tool requests may also carry bounded `action`, `targets[]`, and
`approval_reason` presentation facts. The daemon redacts them and supplies server-owned
`allow_label`, `grant_scope`, `grant_duration`, and `grant_options[]` fields in
pending/history projections. Legacy requests receive only the request-bound **Allow
once** default. A measured adapter capability may set `offer_exact_run_grant` to offer
`run_exact`; the OpenCode adapter is the current caller. The option means the same
permission name and exact ordered `targets[]` for the same runtime task and native session
until that message finishes or is stopped. The `deadline` remains the response deadline
and is not the grant lifetime.

An interactive `allow` decision may carry the request-bound `grant_id`. Missing IDs remain
request-scoped; a denial may not select a grant. Only the local interactive responder can
choose `run_exact`. The wait result then carries the selected `grant_id` and an opaque
`grant_token` directly to the blocked runtime adapter. The token is absent from approval
projections, event streams, history and audit records. A later runtime request can present
it only on the authenticated loopback route; a runtime, task, native-session, permission,
or exact-target mismatch returns 403 without creating a pending approval. Every match
creates a separate allowed history/audit record attributed to `remembered-run-grant`.
The adapter retries a stale token once without it so the user receives a normal prompt.
`POST /api/v1/approvals/grant/revoke` takes `{grant_token}` and idempotently returns 204;
the owning adapter calls it when the message protocol exits.

`POST /api/v1/approvals/decision` accepts `grant_id` and `selections` alongside `id`,
`decision` and `reason`. `selections` is an array of `{prompt_id, values[]}`. On an approval that carries prompts, an
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
GET  /api/orchestration/profiles/{id}/revision
POST /api/orchestration/profiles/preview
POST /api/orchestration/profiles/select
GET  /api/orchestration/profiles/{id}/draft
PUT  /api/orchestration/profiles/{id}/draft
DELETE /api/orchestration/profiles/{id}/draft
POST /api/orchestration/profiles/{id}/draft/publish
POST /api/orchestration/drafts
GET  /api/orchestration/roster
GET  /api/orchestration/roster/{profile}
GET  /api/orchestration/roster/{profile}/runs
GET  /api/orchestration/agents
PUT  /api/orchestration/agents/{binding}
POST /api/orchestration/agents/{binding}/disable
POST /api/orchestration/agents/batch
POST /api/orchestration/agents/binding-id
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
GET  /api/orchestration/tags/summary
GET  /api/orchestration/groups/{group}/notes
POST /api/orchestration/groups/{group}/notes
POST /api/orchestration/notes/{note}/retract
```

The optional orchestration layer (ADR 0028) is inactive until it is configured, and every
route here returns `503` when it is unavailable. Reading these endpoints never starts an
agent.

`GET /api/orchestration/tags` takes exactly one selector. `session_id` (repeatable, one
session's identity alternates) lists that session's active agent-claimed tags: one row per
agent key across the listed identities, its newest claim, newest first. `tag=` or
`agent_key=` is the reverse lookup: `{tags, sessions, truncated}`. `tags` holds one row per
session identity and agent tag, its newest claim (an agent re-claiming a tag every turn does
not repeat it), newest first, at most `recall.tag_lookup_limit` rows. `sessions` folds those
rows through the session catalog's identity alternates (one entry per session, `catalog:
false` when no catalog row matched). A `tag=` lookup ignores case. `GET
/api/orchestration/tags/summary` counts distinct sessions per active `(tag, agent_key)` among
the newest `recall.tag_lookup_limit` distinct (session identity, agent tag) rows; `truncated`
says older ones were not counted.

**Profiles are instructions; bindings are authority.** A profile is a `PROFILE.md` document
identified by content: `preview` accepts `source_base64` and returns a `source_digest`, a
`bundle_digest`, and a `state_token`; `select` requires all three plus `confirmed: true`,
and records the profile as selected but **inert** — selecting one never runs it. Authority a
profile *requests* (`reply`, `respond-approval`) is granted only by a binding, per
deployment. A profile that requests an authority it is never granted cannot use it.

**A binding fires for work in its own folder.** A binding's `project_root` matches a task's
or session's working directory when both name the same folder: equal spellings, or equal
folder keys. The daemon computes a key from the folder itself: on macOS the path the file
system reports for the open folder (symlinks, the `/tmp` → `/private/tmp` alias, firmlinks
and letter case resolved); elsewhere, or for a folder the daemon cannot open, the
symlink-resolved path, where letter case must match. A subdirectory or a parent never
matches, and a binding with no root matches nothing. `GET /api/orchestration/agents` returns
`project_root_key` on each agent, and `GET /api/session` returns `cwd_key` when any enabled
binding exists: opaque comparison values, not paths. A client showing which agents watch a
session applies the same rule (equal spellings, or both keys non-empty and equal). That rule
does not yet consider `watch_natural`, so an agent can show as watching a session whose
natural turns it will not fire on; the console has the same limitation.

**Writes are compare-and-swap.** `PUT` on a binding requires the `expected_state_token`
read from its current state and an explicit `confirmed`; a stale token is refused rather
than merged. Binding edits apply at the next admission and never to a turn already in
flight.

**The roster is the Agents page's read model.** `GET /roster` lists one row per agent
(profile id): its current version or draft, `agent_type`, its *places* (every managed binding
plus the review binding for that profile) with state, pinned version and model, stats over
the configured window (`tz_offset_minutes` sets the day boundaries; `stats_unavailable` when
the history cannot be read, never zeros), the newest run, and an
`attention` code (`revision_unavailable`, `profile_problem`, `provider_outage`,
`recent_failures`), plus the tripped provider routes in `outages` (runtime, model, class, detail,
`tripped_at`, `parked`) for the reroute. `problems` lists stored agents that failed
verification and, when the daemon refused `orchestration.json` at start, one
`orchestration_config_rejected` entry carrying the decoder's reason (built-in defaults are
then in force; the file is read once per start, so a fix clears the problem at the next
restart). `GET /roster/{profile}` adds the exact current source, every stored
version with the places pinning it, the draft, the editors' catalogs (signals, the context
kinds the managed reader supplies, and in `context_always` what every run reads unasked,
runtime capabilities), lifetime `totals` (or `totals_unavailable`) and which limits the daemon
enforces.
`GET /roster/{profile}/runs?place=&outcome=&before=&limit=` pages the agent's decisions newest
first; `before` is the `next` value of the previous page. Outcome classes are `acted`,
`quiet`, `failed`, `skipped`, `deferred`, `running` and `waiting`.
Each managed run in these pages, and in `GET /managed`'s `runs`, carries the claim
contract's owner-attention answer for a settled claim run: `attention_class` (`ask` — an
`ask_owner` line for the owner; `draft` — proposed reply text that was not carried to the
session, or a `draft_reply`; omitted otherwise) and `awaits_operator` (true while the claim
waits on the operator's confirmation: a `draft_reply` until the operator's `/send` records
`detail.delivery.state: started` on it, or a `launch_profile` / `request_interrupt` whose
receipt is `not_requested`). Clients read these flags instead of
keeping their own list of actions. `ask_owner` is offered to helper agents only.

**Every acting claim carries one delivery receipt (2026-09-29).** A run whose claim acts on
its session (`reply`, `launch_profile`, `request_interrupt`, `send_message`) carries
`detail.delivery` = `{state, tier, reason_class, detail}` from the moment the claim is
stored. `state` is `pending` until the daemon decides the outcome, then one of `started`
(the task was admitted), `accepted`, `delivered` or `expired` (the session-message states),
`not_requested` (the helper does not act automatically; the claim waits for the operator,
and a confirmed `/act` moves it to `started`) or `unavailable` with a structural
`reason_class`: `attended_session`, `non_terminal_signal`, `no_controllable_task`,
`grant:<refusal>`, `pending_approval`, `dry_run`, `ceiling_breach`, `binding_changed`,
`authority_not_granted`, `suppressed:<class>`, `task_admission`, `resume_error`,
`action_error` or `decision_error` (the daemon could not decide, for example an unreadable
flow grant; nothing was sent). A claim whose outcome was never recorded (the daemon stopped
between storing it and deciding it) settles at the next start, or on the delivery-expiry
sweep once older than `delivery.ttl_seconds`, as `state: unknown` with `reason_class:
interrupted` — or as `started` when the run it launched proves the send began. Read receipts
whose state is not `pending`; runs written before receipts existed carry none. A flow ceiling
breach is recorded on the completed run as `error_class: flow_ceiling_breach`; the roster's
breach lane stays on while that member's ceiling is breached and clears when the owner's
re-tag re-arms it.

`POST …/managed/runs/{run}/send` and `…/act` answer `409` with the reason when the action did
not start: a refused task admission, a replayed admission whose run never started, or an
interrupt that was not confirmed. When the task started but its record then failed, they
answer as admitted.

**Drafts are inert.** A draft is one agent's work in progress, stored beside the selected
versions. `PUT …/draft` takes the draft's `expected_state_token` (the roster detail's
`draft_token`) plus `expected_selection_token` (the detail's `selection_token`, checked when
the save starts the draft so edits made against a replaced version are refused with 409), and
either a structured `edit` (name, description, version, trigger event,
context, instructions, reply shape, per-signal stage prompts, authority requests, may-tag,
locality) or exact `source_base64`; an invalid draft is kept and returned with its
`problem`. `POST …/draft/publish` takes `expected_draft_token`, `expected_selection_token`
and `confirmed: true`, selects the draft's exact bytes and removes the draft; it refuses when
another version was published since the draft started, when the version number is taken, or
when the bounded history would drop a version a binding still runs or a helper may launch
(`pinned_revision`, naming it; 409), or when those bindings cannot be read (`pins_unavailable`,
503).
`POST /drafts` starts a new agent (`start: blank|duplicate`). `GET …/revision?source_digest=&
bundle_digest=` reads one stored version. None of these runs or binds anything.

**Batches are all-or-nothing.** `POST /agents/batch` carries `changes` of `op` `create`
(a full `place`), `update` or `enable` (a `set` naming only the sections it edits: `revision`,
`model`, `permissions`, `declared_tags`, `priority`, `budgets`, `fallback`, `scope`; every other
field keeps the binding's own value) or `disable` (state only). Each change carries its own
`expected_state_token`; with `validate_only: true` the daemon reports each change's validity
without writing; otherwise every change commits in one transaction or none does. A binding's
`state` survives batch edits. `PUT` writes a whole binding: an omitted `state` means `enabled`,
as it always has, so send `state: disabled` to save a turned-off binding without turning it on.
`POST /agents/binding-id` derives a free id for a new place (`<profile>--<repository>`) and
its creation token.

**Two lanes with different shapes.** Managed agent bindings (`/agents`, `/managed`) run a
follower or helper against a session hand-back through the same runtime-task path as any
other governed work, so each binding names its own runtime, model, and ordered fallback
routes. The review binding (`/reviews`) is a **singleton** whose inference path is a
literal-loopback Ollama endpoint; it is the only lane that may answer a held approval, and
its `effect` is either `report-only` or `delegated-first`.

**Destination and limits on managed bindings.**
- **Destination at save.** A profile's `requirements.destination.locality` is enforced.
  Under `local-only`, `PUT` refuses a binding when any of these is a route its runtime does
  not declare local:
  - the primary route;
  - any fallback route;
  - the route an allowlisted child would run on.

  "Local" is the runtime adapter's claim about where inference goes, not proof of egress.
  `explicit-local-or-remote` admits any route.
- **Refusals for existing bindings.** A binding saved before this rule records one
  `suppressed` run per source session, with `error_class: destination_locality` and a
  `recovery` text. It launches nothing.
- **Pinned-revision fields on each agent.** `GET /api/orchestration/agents` carries:
  - `profile_problem`: a value this host refuses for managed turns;
  - `destination_problem`: a route the destination forbids;
  - `destination`: `{required, routes: [{runtime, model, label, local, basis}]}`.
- **Fields the host does not apply.** Each managed profile option in
  `/api/orchestration/managed/settings` lists them in `not_applied: [{field, reason}]`.
- **Timeouts.** A helper turn that runs past its profile `limits.timeout` is stopped and
  recorded as `failed`, with `error_class: timeout`. The watcher's cadence is
  `orchestration.json` `helper_session.turn_deadline_check_ms`.
- **Cross-runtime resumes.** A `send` or `resume` that would resume another runtime's
  session with a model the binding's runtime runs locally answers `409` with the reason.

**Delegated answers.** A delegated response carries `allow`, `deny`, or `abstain` and
is attributed to a service principal. An allow for a question carries selections:
offered option labels, or a typed answer when the prompt permits free text, bounded
by the same configured `max_free_text_bytes` used for human answers. Invalid answers
leave the approval pending; abstain leaves it for the human. Existing routing grants,
budgets, deadlines and final approval validation still apply.

These interfaces are experimental and follow the alpha compatibility rules below.

### Model routes

```text
GET    /api/model-routes
GET    /api/model-routes/{id}
POST   /api/model-routes/preview
POST   /api/model-routes/select
DELETE /api/model-routes/{id}
```

A model route is a named, device-local choice of where a model call goes. It is the one
place a runtime, a model id or an inference endpoint is typed: a place (a managed binding,
each entry of its fallback chain, the reviewer) names a route by id. A route is stored
under `<data dir>/models/routes/`, is never sent anywhere, survives unlink, and has no
field for a credential.

There are two families. `runtime-model` (kind `runtime-model`) has `fields.runtime`,
`fields.model` and an optional `fields.thinking_effort` (`{"kind":"inherit"}` or
`{"kind":"level","value":…}`); managed places and their fallback entries take it.
`inference` (kind `local-ollama`) has `fields.endpoint`, which must be a literal loopback
URL (`http://127.0.0.1:<port>` or `http://[::1]:<port>`), and `fields.model`; the reviewer
takes it. A route id is `rte_` followed by a ULID. A name is unique on the device after
trimming and case folding.

- `GET /api/model-routes` returns `routes[]`, `problems[]` (a stored route that failed
  its integrity check, by `selection_key`), `absent_state_token` (the state token a
  create presents) and `places_unavailable` (the store could not be read, so `places` is
  empty and says nothing). Each route carries `route_id`, `name`, `family`, `kind`,
  `fields`, `local` with `locality` (`on this machine` or `leaves this machine`; for a
  `runtime-model` route this is the runtime adapter's own claim, never a proof of where
  traffic goes), `revision_digest`, `state_token`, `created_at`, `migrated_at` when the
  daemon created the route from a place's earlier typed settings, and `places[]`: every
  place or fallback entry that uses it (`place_id`, `lane`, `profile_id`, `repository`,
  `state`, `fallback`, `position`, `label`).
- `GET /api/model-routes/{id}` returns `route`, or `404` with code `route_not_found`.
- `POST /api/model-routes/preview` takes `{route_id?, name, family, fields}` and writes
  nothing. It returns the normalised `route`, the `preview_digest` and `state_token` a
  select must send back, `exists`, `changed`, `problems[]` (for an edit: the places the
  change would stop from running, with `code: route_edit_violates_places` and `places`;
  an unusable thinking effort), and `admission[]`: what the selected route rules answer
  for the route alone and on each place that uses it (`allowed`, `action`, `rule`,
  `message`, `fired`). The preview decides nothing; a bind and a run start do.
- `POST /api/model-routes/select` takes the same draft with `preview_digest`,
  `expected_state_token` and `confirmed: true`. With no `route_id` it creates a route.
  With one it writes a new revision of that route — a rename is a new revision of the
  same id — and then moves every place that uses the route to it in one store
  transaction: the place's stored runtime, model and effort (the reviewer's endpoint and
  model, with its request-path identity recomputed) follow, and each moved place gets a
  new `state_token`, so a sheet holding the old token is refused and reloads. The
  response has `route`, `changed`, `created` and `moved_places[]`. An edit that would
  make any place violate its profile's declared locality, or leave it with a mode its
  runtime does not have, is refused `409` naming the places, and nothing is written. If
  the daemon stops between writing the revision and moving the places, the next start
  moves them.
- `DELETE /api/model-routes/{id}` takes `{expected_state_token, confirmed: true}` and is
  refused `409` with code `route_in_use`, naming the places, while any place or fallback
  entry uses the route.

Errors are `{"error": {"code", "field"?, "message", "recovery"?, "places"?}}`. Codes:
`invalid_route` and `confirmation_required` (422), `route_not_found` (404),
`route_name_taken`, `state_conflict`, `route_in_use`, `integrity_conflict` and
`route_edit_violates_places` (409), `storage_error` (500).

**A binding write names a route.** `PUT /api/orchestration/agents/{binding}` takes
`route_id` and `mode`; each `routes[]` entry takes `route_id` and `mode`. In
`POST /api/orchestration/agents/batch` the `model` section is `{route_id, mode}`, a
`fallback` entry is `{route_id, mode}`, and a created `place` carries `route_id`.
`PUT /api/orchestration/reviews/binding` takes `route_id`. A write that still sends
`runtime`, `model`, `endpoint` or `thinking_effort` is refused `400`, naming the field
(the review route answers code `typed_model_field`); nothing is written. The daemon
resolves the route, checks that its family fits the place and that the profile's declared
locality admits it, runs route admission, and writes the reference and the resolved copy
together. Responses keep `runtime`, `model`, `thinking_effort` and the reviewer's
`endpoint` as that resolved copy, beside `route_id`, `route_revision_digest`,
`route_problem` and `adoption_key`. `POST /api/orchestration/managed/provider-reroute`
still names the failed `runtime` and `model`: that is the key of the outage it answers,
not a binding write.

**Route problems.** `route_problem` on a binding is empty, `route_missing` (the route can
no longer be read: a managed place starts no run and records one refused run with error
class `route_missing`; the reviewer stops reviewing and a person answers; a fallback
entry whose route is missing is passed over and listed in the run's
`provider_outage.skipped_routes`) or `migration_failed` (the daemon could not give a place
from an earlier version a route; the place keeps its stored settings and runs as
before). Nothing falls back to a runtime's default model.

**Route admission.** A rule whose predicate reads a `route:` fact is a route rule. It is
evaluated only when a route is bound to a place and when a run starts, over these facts:
`route:id`, `route:family`, `route:runtime`, `route:model`, `route:local` (`true` or
`false`), `route:destination-class` and `route:destination-host` (inference routes only),
`profile:locality` and `project:root`. With no route rule selected, every bind and run
proceeds. A `deny` or `ask` rule refuses the bind, or the run at start, with
`admission_refused` and the rule's id; an `observe` rule proceeds and the run's detail
lists it under `route_rules_observed`. A hook call is never evaluated against a route
rule. A rule that reads a `route:` fact beside any other kind of term (a command, a
tool, a detector fact, or `session:`, `target:` or `agent:` state) is refused when the
rule document is written. No route rule ships. If the rulebook cannot be read (the rule
document, or the records of the team bundles this device adopted), the daemon logs why
and: a bind is refused with `rulebook_unreadable`; a route preview lists a
`rulebook_unreadable` problem; a run of a place that was bound while the rulebook could
be read still starts, decided over the rules that could be read, as on the hook's
stateful tier.

**Roster.** Each place in `GET /api/orchestration/roster` and `/roster/{profile}` carries
`route_id`, `route_name`, `route_local`, `route_problem`, `held_reason` (why a place of a
shared agent starts no run while it stays on: `adoption_expired`,
`version_no_longer_shared` or `adoption_ended`) and, for the reviewer, `run_refusal` (the
code of the last review that was not started). An agent carries `origin` when a team
adoption wrote its current version. An agent surface should show a route's name and
whether it leaves this machine, and nothing else of the route.

For that, the roster names routes wherever a surface would otherwise need a model id. A
managed place carries `fallback_routes[]` beside `routes[]`: each fallback chain entry as
`{route_id, route_name, route_local, mode, missing}`, where `missing` is true for an entry
whose route can no longer be read. Each entry of `outages[]` in
`GET /api/orchestration/roster` carries `route_names[]`: the names of the model routes
that resolve to the tripped runtime and model (an empty list when none does). An agent
carries `collision` (`{organization_name}`) when it is the member's own agent and an
offered team agent uses the same id; that team agent is not adopted and this one is never
replaced. `attention` may also be `place_held`, `route_missing` or `run_refused`: a place
that is on and starts no run because its adoption holds it, its model route cannot be
read, or its last run start was refused.

### Skills, chat, and runtime tasks

`GET /api/skills` reports inventory, coverage and a sorted `providers` array of
`{runtime, can_probe}` entries from the registered skills adapters, including when
inventory is empty. Clients use this metadata for columns and explicit probe buttons;
filesystem coverage does not prove live loading. An unsupported runtime probe returns 400.

```text
GET  /api/skills
POST /api/skills/probe
GET  /api/chat/auth
POST /api/chat/auth/start
GET  /api/chat/capabilities
GET  /api/chat/models
POST /api/chat/models/refresh
POST /api/chat
POST /api/runtime-tasks
GET  /api/runtime-tasks
GET  /api/runtime-tasks/stream
POST /api/runtime-tasks/{task}/interrupt
```

These are experimental local runtime-control interfaces. Availability and capability must
be read from their runtime responses; source visibility is not a support claim.

`POST /api/runtime-tasks` requires an idempotency key of 8–200 characters, as
`idempotency_key` in its body or the `Idempotency-Key` header (a retry with the same key
returns the task already created). A request without one is answered **400**.

**Added 2026-09-24 (runtime model catalog and usage):**

- **`GET /api/chat/models?runtime=R`** returns the models runtime R reports it can run:
  `{runtime, state, observed_at, digest, scope, binary, interface_revision, reason_code,
  rejected, models}`.
  - `state` is `fresh`, `stale` (the last good list, shown while a refresh runs or after
    one failed), `unavailable` or `unsupported` (the runtime does not publish a list).
  - `reason_code` is one of `not-installed`, `exited-with-error`, `timed-out`,
    `output-too-large`, `unparseable`, `too-many-entries` and `work-dir-in-repository`.
    It never quotes runtime output.
  - Each entry in `models` is a chat model option with `source: "runtime"` and optional
    `group`, `group_label`, `limits` (`context_tokens`, `input_tokens`, `output_tokens`),
    `inputs` (task input kinds the adapter admits for that id), `price` (`unit`,
    `per_tokens`, `rates[]` of `{class, label?, amount, above_context_tokens?}`) and
    `price_partial`.
  - Ids are opaque: send them back unchanged as `model`. Absent facts are unknown.
- **`POST /api/chat/models/refresh?runtime=R`** reruns discovery unless one ran within
  `daemon.json` `models.min_refresh_seconds`.
- **Usage events.** A runtime task now publishes `usage` events (kind `usage.delta`)
  instead of `cost` and `usage` fields on `result`. Each carries:
  - `delta`: this report's amounts, with the well-known disjoint token classes `input`
    (excludes cache), `cache_read`, `cache_write`, `output` (includes reasoning) and
    `reasoning` (informational), plus `other[]` runtime-specific classes and `cost`
    `{amount, unit, basis}`;
  - `turn_total`: the task's summed classes and `cost[]`, one line per unit and basis;
  - `context`: `{used, window}`.

  `basis: "runtime"` means the runtime stated the figure; it is not an invoice.
- **`GET /api/usage` and `GET /api/session`.** Usage objects gain `reasoning_tokens`,
  `other` and `cost`. The report gains `cost[]` (one line per unit and basis) and
  `cost_coverage` (`with_cost`, `without_cost`). Report totals carry no cost.
- **Recorded usage (2026-09-25).** `GET /api/usage` is built from recorded calls (see
  *Sessions, search, files, and references*). `turns` became `calls` in totals, per
  runtime and per day; `cost_coverage` counts calls, not sessions; top rows gain
  `source_present`. `GET /api/session` `usage` is the session's recorded own calls, with
  `delegated` (its subagents' calls and every descendant session's calls, folded the same
  way), `reasoning_stated_calls` and `as_of`; `turns` there, and on session summaries,
  counts model calls. `usage` is absent until the recorder has read the session.
- **Session usage breakdown (2026-09-26), a breaking change.** The breakdown's
  `delegation` values are now `main`, `subagent` and `agent`; `delegated` is refused with
  a 400 that names them. `main` no longer includes the calls of Crossing Guard agent
  sessions, which are `agent`. `GET /api/session` `usage.delegated` holds native
  subagents' calls only, and `usage.agents` (new) holds agents' calls; both carry
  `children`. `GET /api/usage` rows are root sessions (see above) and gain the split;
  `sessions` and `no_data` count root sessions. New: `GET /api/session/usage`, the
  `member`, `session` and `agent_type` dimensions, `by_type`, `work`, `work_by_runtime` and
  `coverage.agents`.

## Compatibility rules for alpha

- The bundled UI, hook, daemon, and CLI should come from the same build.
- Persisted configuration and data require explicit migration or safe refusal, even though
  HTTP response shapes may change during alpha.
- A route becoming documented here does not make it supported for third-party clients.
- Before any stable API declaration, the project needs machine-checked route/schema drift,
  normalized errors, pagination/stream contracts, and at least one independent consumer.

### Thinking effort (2026-09-26 candidate)

The model catalog may include `thinking_effort` with `state`
(`supported`, `unsupported`, `unknown`), model-scoped `choices`
(`id`, `label`, optional `description`, adapter mapping digest), optional `default`,
and `can_start`/`can_resume`. Use the opaque choice ID, not its display label.
An omitted descriptor is unknown. Shared labels do not make values portable between
models. Refresh uses the existing model refresh endpoint.

`POST /api/runtime-tasks` and legacy `/api/chat` accept optional `thinking_effort`:
`{"kind":"level","value":"high"}` or `{"kind":"inherit"}`. Inheritance sends no
effort override. Omitting the field preserves old caller behavior, including supported
legacy extra args; it does not silently adopt saved GUI defaults. Conflicting typed
and legacy selections are refused. Unknown request properties and invalid selection
shapes are refused. Binary, endpoint, credential and local-provider overrides have no
verified effort catalog, so explicit levels are refused for those contexts.

- `GET /api/session-turn-settings?runtime=R&session_id=S&model=M` reads the canonical
  session/model selection, revision `token` and `updated_at`. An absent row is inherit,
  token `"0"`.
- `PUT /api/session-turn-settings` takes `runtime`, `session_id`, concrete `model`,
  expected `token`, and `thinking_effort`; it validates the model and atomically saves
  the next revision. Reset writes inherit and retains revision continuity.
- `POST /api/chat/effort-preview` accepts `runtime`, `model`, `extra_args`, optional
  `thinking_effort`; it parses legacy syntax without launching a process or saving.
  Returns a requested-settings record or null. It is not a capability validation.

These routes also have the existing `/api/v1/` aliases and authentication/origin guards.
Settings and preview bodies are limited to 64 KiB. Existing task/chat prompt body-size
behavior is preserved. A GUI turn sends the acknowledged selection and
`session_effort_token`; admission verifies native destination, canonical session,
model, value and revision in the creation transaction. Stale tokens return 409
`stale_session_default`. Unresolved catalog identities return 409 `identity_pending`.
Other effort refusals include `invalid_choice`, `legacy_conflict`, `unsupported` and
`unavailable_evidence`. Bodies carry `code`, `field: "thinking_effort"`, `message`.

New opted-in task records and queued events contain immutable `requested_settings`:
`model`, `thinking_effort`, `thinking_effort_label`, internal `source`
(`turn`, `session`, `legacy`, `binding`, `fallback`) and optional `catalog_digest`.
Idempotent retries return the original snapshot even if defaults or catalog change.
Snapshots survive event-window trimming for the task's existing retention lifetime.
They describe requested intent, never provider-confirmed applied effort. Older tasks
without a snapshot remain unknown. Session defaults have independent retention.

A managed place's thinking effort is its model route's (`fields.thinking_effort`, see
Model routes): binding writes no longer accept `thinking_effort`. It is validated against
the route's runtime and model when the route is previewed and when a place is bound to
it, and every launch revalidates against the selected model. Configuration refusals do
not trigger automatic provider-outage fallback. State tokens cover effort; no public
source/provenance field is accepted.

### Propose a memory

`POST /api/v1/memory/propose` is available when `recall.propose_enabled` is true.
The bounded JSON body accepts `title`, `body`, optional `category` (default `note`),
`tags`, `aliases`, `repository`, and `session` as `<registered-runtime>/<session-id>`.
A citation is required. Success returns `{ "id": "…", "status": "pending", "result": "…" }`;
a human must promote it before normal recall. Disabled proposals return 404, invalid
or unidentified proposals 400, and the configured per-session rate bound 429.

### Handoff between members

```text
GET  /api/team/members
GET  /api/team/handoffs
GET  /api/team/handoffs/{id}
POST /api/team/handoffs/send
POST /api/team/handoffs/{id}/decline
POST /api/team/handoffs/{id}/withdraw
POST /api/team/handoffs/{id}/close
```

These are console and CLI routes. None is device-facing, and no route or MCP tool lets an
agent session send or pick up a handoff by itself. Every refusal is
`{"error": "<sentence>", "code": "<data code>"}`.

`GET /api/team/members` answers the cached member directory of the linked organization:
`{"linked", "organization_id", "members": [{"user_id", "display_name", "self"}],
"refreshed_at", "problem"}`. A device knows a user id and a display name about a member
and nothing else. The cache is refreshed every `members_refresh` (`team.json`);
`?refresh=1` refreshes it first. Unlinked, `members` is empty.

`GET /api/team/handoffs` answers `{"linked", "organization_id", "received": [item],
"sent": [item], "transport"}`. An item is `{"id", "organization_id", "local",
"sent_here", "to_me", "from_another_device", "peer": {"user_id", "display_name"},
"title", "state", "state_at", "created_at", "refusal_code", "receipt_code", "has_text",
"ever_held", "session", "repository_id", "opened_by", "other_opened_count",
"link_ended", "offers"}` and never carries the text.

- `state` is the server's delivery state (`sent`, `received`, `started`, `opened`,
  `declined`, `closed`, `withdrawn`, `expired`) or one of the device's own: `queued` (sent
  here, not yet accepted by the server) and `refused` (the server refused it;
  `refusal_code` names why — `unknown_recipient`, `recipient_inactive`,
  `recipient_inbox_full`, `rate_limited`, `over_cap`, `conflict` — and the text is kept).
- `offers` lists what this device may ask for now: `declined` while the item is
  `received`; `closed` once it is `started` or `opened`; `withdrawn` on any item the
  user sent that has not ended.
- A handoff sent from this device is under `sent`, a self-send included (once). One sent
  from another of the user's devices is under `sent` with `from_another_device`, no
  title and `has_text: false`; it can be withdrawn here. A local handoff is under
  `received` with `local: true`.
- `to_me` with `ever_held: false` is a handoff that expired or was withdrawn before it
  reached this device: the sender and the time, no title, no text, nothing offered.
  `has_text: false` with `ever_held: true` is a copy a withdrawal erased.
- `receipt_code` is the code the server rejected this device's last receipt with, when
  the handoff had already ended another way (`handoff_withdrawn`, …) or a session had
  already started (`handoff_started`, after a decline); `state` is then what the server
  holds.
- Linked, the lists hold the linked organization's handoffs and the local ones. Unlinked,
  they hold everything on the device, each team item marked `link_ended` and offering
  nothing: it stays readable and nothing is sent for it.
- `transport` is `{"server_carries_handoffs", "pull_outcome", "pull_problem", "pull_at",
  "members_refreshed_at", "members_problem", "hash_conflicts", "unlandable"}`.
  `server_carries_handoffs: false` means the team server is older than handoffs: queued
  items stay queued and leave when it is upgraded. `hash_conflicts` counts pulled rows
  refused because their wire hash differed from the one held for the same id.
- `?leftovers=1` adds `checkout_leftovers`: the checkouts this device has seen that still
  hold a handoff file or marker block an earlier version wrote
  (`{"dir", "file", "blocks", "damaged"}`).

`GET /api/team/handoffs/{id}` answers `{"handoff": item, "document", "opens"}`.
`document` is the handoff document (`handoff.schema.json` 1.1) when this device holds it:
a device of the recipient, or the device that sent it. `opens` lists this device's opens
of it. `404` for an id this device does not list.

`POST /api/team/handoffs/send` takes `{"runtime", "session_id", "to", "local", "title",
"body_markdown", "remaining", "include_conversation", "preview", "id", "created_at",
"wire_hash"}`. `to` is a user id or a display name from the directory; `local: true`
writes a same-device handoff instead (no recipient, no server, works unlinked). `title`,
`body_markdown` and `remaining` are the person's final text.

- With `"preview": true` it writes nothing and answers **exactly the content that will
  leave**: `{"preview", "id", "created_at", "wire_hash", "recipient", "title",
  "remaining", "body_markdown", "conversation": {"turn_count", "bytes", "truncated",
  "turns"}, "session", "repository_id", "governance_state", "agents", "bytes",
  "checks": {"redactions", "redaction_count", "absolute_paths"}}` — after the checks: a
  path under the session's checkout is relative, any other home, temporary or system
  path is `[absolute path]`, and a match of a named secret pattern is
  `[redacted:<pattern>]`. `bytes` is the size of the document as pushed. `conversation`
  is present only with `include_conversation`, bounded by `handoff.conversation_turns`
  and `handoff.conversation_max_bytes` (`daemon.json`).
- Without `preview` the request repeats the preview's `id`, `created_at` and
  `wire_hash`. The daemon builds the document again and sends it only when it hashes to
  `wire_hash`; otherwise `409` `preview_mismatch` and nothing is sent. It answers the same
  shape with `state`: `queued`, or `received` for a local handoff.
- `400` `invalid_handoff` (an empty title or body, or a document the wire schema
  refuses — the answer names the field and the rule, never the text), `400`
  `preview_required`, `400` `ambiguous_recipient`, `404` `unknown_session`, `409`
  `not_linked`, `409` `recipient_inactive` (the directory does not list the recipient;
  nothing is written).

`POST …/{id}/decline`, `…/withdraw` and `…/close` (no body) change the state and queue
the receipt in one transaction, and answer the detail shape. `409` with the code when
the item does not offer it: `wrong_state`, `not_offered`, `link_ended`, or
`handoff_withdrawn` / `handoff_declined` / `handoff_closed` / `handoff_expired` when it
has already ended. A local handoff changes state and sends nothing.

### Opening a handoff, and memory recall

```text
GET  /api/team/handoffs/{id}/open-options
POST /api/team/handoffs/{id}/open
POST /api/team/handoffs/{id}/open/{ticket}/cancel
POST /api/team/handoffs/{id}/deliver-again
GET  /api/team/handoffs/claimed
GET  /api/team/handoffs/session
GET  /api/memory/attach
POST /api/memory/attach
```

A handoff is opened only from the console. There is no `handoff open` verb, and a session
a person starts in a terminal never picks one up. None of these routes launches anything:
the new session is started by the person's first prompt, sent to `POST /api/runtime-tasks`
with the ticket the open returned. Refusals are `{"error", "code"}` as above.

**The item.** Each handoff item (list and detail) also carries `open_offered`,
`open_withheld_code` and `opens`. `open_offered` is true on any item of the recipient's
that has not ended and whose text this device holds — on any of the recipient's devices,
whether it is `received`, `started` or `opened`, and on a row of an ended link (it opens
locally; nothing is sent). `open_withheld_code` says why not: a terminal code
(`handoff_withdrawn`, …), `not_offered` (the device that sent it), `no_text`, or
`wrong_state`. An entry of `opens` is one Open on this device:

```text
{"ticket_id", "runtime", "state", "checkout_root", "opened_at",
 "ended_code", "ended_detail",
 "session": {"runtime", "native_id"}, "started_at",
 "brief", "brief_delivered_at", "brief_due_again", "offers"}
```

- `state` is `waiting` (its composer is open), `claimed` (a session started for it) or
  `cancelled`. A ticket has no timer.
- `ended_code` says why an open ended without a session: `composer_closed`;
  `runtime_not_started` — the runtime never started a session, `ended_detail` is the
  launched task's own reason, and `offers` holds `retry` (the only case it does);
  `hook_did_not_run` — a session started and the Crossing Guard hook did not run in it
  (trust or re-attach the hook; no retry is offered); `link_ended`; or the handoff's
  terminal code.
- `session` is the session that started for the open, by runtime and native id. The
  catalog and resume ids are the session row's own; a client reads them there and never
  joins on one id.
- `brief` is the brief's state for that session: `waiting_for_next_prompt`, `delivered`,
  or `not_delivered` (the device gave up after `handoff.brief_wait`; nothing was reported
  as opened). `brief_due_again` says Deliver again was asked for and the session's next
  prompt carries it.
- `offers` holds `deliver-again` on every claimed open of a handoff that has not ended.

`GET …/{id}/open-options` is what the open sheet shows:
`{"handoff_id", "offered", "withheld_code", "checkout_roots", "runtimes", "firing_window"}`.
`checkout_roots` are the folders this device has resolved to the handoff's repository,
newest first; the person may choose another. `runtimes` lists the runtimes a handoff can
be opened in — a runtime whose hook cannot tell a nested call from the session's own at
the prompt event is not listed (OpenCode, in this release). Each entry is

```text
{"runtime", "ready", "missing", "session_entry_at", "prompt_at", "unclaimed_launch_at",
 "recall_tools", "memory_recall"}
```

`ready` is true only when this device has **observed**, from that runtime, inside
`handoff.firing_window`, a session-entry row posted by the hook and a prompt row, and the
prompt kind is among `delivery.carrier_kinds`. A settings file proves neither. `missing`
names exactly what is not there: `session_entry_not_observed`,
`prompt_event_not_observed`, `prompt_kind_not_a_carrier_kind`, or
`hook_did_not_run_in_launched_session` — a session Crossing Guard started for an Open
made no claim, and no row newer than that has been seen. `recall_tools` is the state of
the runtime's recall tools registration (`current`, `absent`, …, or `unavailable`); where
they are not registered the brief still arrives and `get_handoff` does not.
`memory_recall` is the shape `GET /api/memory/attach` returns for one runtime.

`POST …/{id}/open` takes `{"runtime", "checkout_root"}` and writes a ticket. It answers
`{"ticket": open, "handoff": item}`. `400` `checkout_root_required` or
`checkout_root_unusable`; `409` `runtime_not_offered`, `runtime_not_ready` (the sentence
lists what is missing), or the item's `open_withheld_code`.

The composer then sends the first prompt:
`POST /api/runtime-tasks` accepts `"handoff_ticket": "<ticket_id>"` beside its other
fields (binary, model, mode, extra arguments and the rest keep working). The session
starts in the ticket's folder unless the request names `cwd` or a workspace selection.
The daemon launches the turn as an ordinary console task and sets `CG_HANDOFF_TICKET` in
that one process's environment and no other. A ticketed send is refused with `409`
`{"code", "message"}`: `handoff_withdrawn` ("the sender withdrew this handoff") and the
other terminal codes, `ticket_cancelled`, `ticket_already_used` (one ticket, one launch;
a retry with the same idempotency key returns the task it already made),
`ticket_runtime_mismatch`, `ticket_not_found`, `ticket_needs_new_session` (a handoff opens
a new session; `session_id` and `catalog_session_id` must be empty).

`POST …/{id}/open/{ticket}/cancel` (no body) cancels a ticket whose composer was closed
without sending: `{"ticket": open, "cancelled"}`. `cancelled` is false, and nothing
changes, when the ticket already launched its session or is no longer waiting. `404`
`ticket_not_found`.

`POST …/{id}/deliver-again` takes `{"ticket"}` — which opened session; it may be omitted
when the handoff has exactly one — and arms the brief for that session again: its next
prompt carries it. It is offered on a `started` and on an `opened` item and never
produces a second `opened`. It answers `{"ticket": open, "armed"}`; `armed` is false when
the session already holds `delivery.max_pending_per_session` undelivered messages, and
the brief is armed at the next sweep. `409` with the terminal code on a handoff that has
ended (`handoff_withdrawn`, …), `409` `not_claimed` when no session started for it here,
`400` `ticket_required` when more than one did.

`GET /api/team/handoffs/claimed?caller_runtime=&caller_id=` backs the recall tool
`get_handoff`. It answers `{"handoff_id", "from": {"user_id", "display_name"}, "local",
"state", "document"}` — the full document and its excerpt — only when the caller's
runtime and native id are the session that claimed one of this device's tickets. `403`
`caller_unidentified` when either is missing; `403` `not_the_opened_session` for any
other caller; `409` `handoff_withdrawn` when the handoff was withdrawn before that
session was handed its brief; `409` `no_text` when this device holds none. This is an
addressing rule, not a confidentiality boundary: any process holding the loopback token
can name a caller, and on Claude Code a session cleared with `/clear` keeps the same MCP
server process and so the same id.

`GET /api/team/handoffs/session?runtime=&native_id=` answers whether a session on this
device was opened for a handoff: `{"opened": false}`, or `{"opened": true, "handoff_id",
"title", "from", "local", "state", "ticket": open}` — the facts behind "continues <title>
from <sender>".

`GET /api/memory/attach` answers `{"runtimes": [state]}` for each runtime with a memory
hook, where a state is

```text
{"runtime", "observed_injecting", "sessions_checked", "sessions_injected",
 "entry_present", "added_by_action", "config_path", "offers", "withheld_code", "withheld"}
```

`observed_injecting` is true when one of the runtime's last
`handoff.recall_check_sessions` sessions carries a memory index this device's hook
emitted — the doctor's proof, not a read of a settings file. `entry_present` says the
runtime's settings hold a memory hook entry and `added_by_action` that the action below
wrote it. `offers` holds `attach` when there is no entry and `detach` when the action
added the one that is there. It is empty, and `withheld_code` says why, when this daemon
may not edit that runtime's settings: `not_this_installation` — the home's lifecycle hook
for the runtime names another executable (a development build pointed at someone's home)
— or `no_lifecycle_hook`.

`POST /api/memory/attach` takes `{"runtime", "action": "attach" | "detach", "consent":
true}` and answers `{"action", "changed", "code", "state"}`. `attach` installs the
existing memory hook, through the existing adapter, for this daemon's own executable;
`detach` removes exactly the entry an attach wrote and nothing else. `changed` is false
when the settings file was left as it was, and `code` says why: `already_present` (an
entry was already there; it is not recorded as the action's), `not_added_by_action` (the
entry was added by hand or by something else, and is left alone), `entry_changed` (the
recorded entry no longer matches what was written), `not_attached`. `400`
`consent_required` without `consent: true` — nothing is written and the settings file is
byte-identical; `404` `unknown_runtime`; `409` `not_this_installation` or
`no_lifecycle_hook`.

The shipped `team-link-change` rule asks a person before an agent calls this route —
either action, under `/api/` or `/api/v1/` — or runs `crossing-guard attach`, which
writes the same entry. The rule matches the path, so a governed command that only reads
`GET /api/memory/attach` is asked about as well. It is a tripwire, not a wall: `consent`
is a field an agent holding the loopback token can send, and the rule is what puts a
person in front of it.

### Choosing a folder

```text
POST /api/folder/choose
```

The console never asks for a typed path when a handoff is opened: it lists the checkouts
this device knows and offers "Choose a folder…". A browser cannot read an absolute path
from its own picker, so this route makes the daemon open the operating system's folder
dialog, on the machine it runs on, and answers the folder the person chose.

`POST /api/folder/choose` takes `{"start"}` — the folder the dialog opens at, ignored
unless it is an existing absolute directory — and waits for the person. It answers
`{"chosen": true, "path": "<absolute folder>"}`, or `{"chosen": false}` when the dialog
was closed without a choice. The route runs a fixed system dialog and nothing else; it
reads no folder and writes nothing. Refusals are `{"error", "code"}`: `400`
`invalid_request`; `409` `picker_open` while a dialog this route opened is still on screen
(one at a time); `501` `picker_unavailable` on a platform where the daemon has no folder
dialog to open (anything but macOS in this release); `500` `picker_failed` when the
dialog could not be shown.
