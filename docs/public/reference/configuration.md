# Configuration reference

Crossing Guard keeps framework mechanism and authored configuration separate. Framework
code captures actions, validates documents, computes provenance/digests, evaluates one
predicate language, and records evidence. Rule predicates, consequences, detector
instances, taxonomies, thresholds, and workflow opinions are configuration.

Change envelopes are derived evidence/runtime data, not configuration. The `change`
commands and Change map are framework tools; the source files, declared/claimed paths,
check labels, and any policy that later acts on those facts belong to the user or a
selected configuration pack. See [Record and inspect change evidence](../how-to/record-change-evidence.md).

Understanding generations are also derived evidence/runtime data, not configuration.
Analyzer ports, source identity, typed fact vocabulary, coverage states, budgets, and the
Impact view are framework tools. Repository role mappings and any thresholds/rules that
act on those facts are configuration.

## Console presentation

`daemon.json` in the daemon's data directory (`~/.crossing-guard` for the installed
daemon — its `--data`; a custom `--data` moves it with the data) owns the console's
presentation policy: layout
presets (in the pane-layout grammar: a region with pane tabs, or a split with a
direction, a ratio, and two children), the default preset, the split clamp, the
minimum region size, the workspace column's initial width, the recently-closed length, the keymap, the editor URL scheme,
the Diff pane's presentation defaults and git budgets (`diff.*`: side-by-side minimum width, word wrap, changed-word marks,
folder grouping, whitespace hiding, `fetch_concurrency` bodies or patches fetched at once, `stale_after_seconds` before a
re-shown git scope reloads; `max_files`, `max_status_entries`, `max_file_bytes`, `context_lines`, `max_refs`,
`git_timeout_seconds`, and `base_ref` for the live git scopes, which need no `workspace.json`; `files.max_entries_per_dir`
and `files.max_read_bytes` for the Files pane, which shares `diff.git_timeout_seconds`),
the Diff pane defaults, the edits page size, and `models.*`, the bounds of runtime model
discovery (`refresh_after_seconds` before a read starts a background refresh,
`min_refresh_seconds` between explicit refreshes, `discovery_timeout_seconds` and
`discovery_output_max_bytes` for one discovery run, `max_entries` for one list, refused
rather than truncated, and `max_id_bytes`/`max_label_bytes` for one entry), and `usage.*`,
how the Usage pane first presents a session (`timeline_gap_minutes`, default 20: an idle
gap its timeline collapses to one mark; `wide_columns_px`, default 720: the pane width from
which its table shows every column instead of expanding rows; `default_measure`, default
`all`, one of `all`, `input`, `output`, `reasoning`, `calls`; `default_grouping`, default
`tree`, or `type`). It lists no model: which models exist comes from each runtime's own
configuration. Discovery runs
from `<data-directory>/model-discovery/`, which must not sit inside a repository.
Unknown fields are refused; a missing file yields the compiled defaults. A malformed file
is logged and the last value that parsed stays in force (origin `last-good`), or the
defaults when nothing has parsed yet (origin `invalid`);
`crossing-guard doctor` and `GET /api/console/config` say so in the same words. The console
writes its own selections here (Settings › Transcript modes and Appearance, and a view's
"Make default") as a minimal overlay: only the keys it owns, never the resolved document.
Hand-written keys keep their values; the file is re-written with sorted keys and two-space
indentation. A Claude session whose transcript declares no working directory now shows its
raw folder name (for example `-Users-me-proj`) as its project, instead of a guessed path;
the `import` project filter and usage rows match that name. The daemon re-reads the file when the console writes it and,
at most every `console_reload_check_ms` (default 1000, 250–60000), when a stat shows a
hand edit; the Diff review budgets apply at start-up only. See
[console.example.json](config/console.example.json).
Pane ids that a surface does not offer are dropped when a preset is applied, so a
preset may name panes that are not installed yet.

### Transcript view profiles

A view profile is a module that decides how each transcript row is drawn: one JSON file
of rules, `{"match": {"kind", "min_chars", "max_chars", "tool", "fact"}, "display": "show" | "collapse" | "hide"}`.
`max_chars` and `fact` need `"format_version": 2`. `fact` matches a tool call the detector
library classifies with that `key:value` (`exec:run`, `search:code`); detectors do not yet
name OpenCode's lowercase tools, so a `fact` rule does not match OpenCode tool rows. Format
2 refuses `collapse` on `tool_call`, `tool_result` and `thinking` rows, which are already one
line.
The first matching rule decides a row; a row no rule matches is shown. `collapse` draws
one line (label, first line, size) that opens to the whole row; `hide` leaves a count that
draws the hidden rows in place. Kinds are `user`, `assistant`, `thinking`, `tool_call`,
`tool_result`, `summary`, `system`, `context` (text a hook added to the conversation), and
`other`; a tool result always goes wherever its call went.

Four modules are built in: `full` (context folded), `conversation` (tool calls,
thinking and context hidden, counted), `agent-review` (inputs of 2,000 characters or more
and context folded) and `work-log` (the agent's prose folded, thinking hidden, inputs of 400
characters or more folded). Settings › Transcript modes edits them without writing JSON:
saving a built-in installs your copy under its id, and "Revert to built-in" deletes it. A file at `view-profiles/<id>.json` in the data
directory replaces the built-in of that id whole, or adds a module under a new id. The file
name must match `id`; unknown fields are refused, and a file that fails is skipped (the
built-in of that id stays in use) and listed by `crossing-guard doctor`. Modules are read
on every session open, so no restart is needed.

`daemon.json` `transcript` selects which module a session opens with:
`default_profile` (default `conversation`) and `role_profiles`, mapping an orchestration role
(`reviewer`, `follower`, `helper`) to a module id (default: helper and follower use
`agent-review`). A file's `role_profiles` entries are added to the defaults', and every
named id must resolve to a module; one that does not makes only the `transcript` section
fall back to the built-in. The view switch above the transcript applies to that view only;
its "Make default" saves the choice for that kind of session. An explicit `full`
selection remains in force after an upgrade; an absent selection now opens in
Conversation. Conversation groups hidden activity as muted disclosure text. Open a
group to see its individual entries, then open a command to read its arguments and
result. Clipped call and result text are reread separately when available.

### Appearance

Colours, fonts, text size and reading widths are modules too. `themes/<id>.json` is one
colour set for one scheme, `{"format_version": 1, "id", "name", "scheme": "dark" | "light",
"tokens": {…}, "labels": {"<runtime>": {"bg", "fg"}}}`; a token an installed theme omits
comes from the built-in theme of its scheme. Built-in themes are `dark`, `light` and
`high-contrast`. `appearance/<id>.json` picks a theme per scheme, whether to follow the
system or pin one scheme (`follow_os`, `pinned_scheme`), an optional accent per scheme,
`ui_font`, `mono_font`, `text_size`, `transcript_width` and `chat_width`. `daemon.json`
`appearance.module` (default `default`) selects one. Settings › Appearance and the account
menu's theme button write these; nothing about appearance is kept in the browser any more.
Font stacks take generic family keywords and system font names only: the console loads no
fonts from the network. `daemon.json` `console_limits` bounds `text_size` (default 11–20)
and the widths (560–1600), and may not exclude a built-in appearance's values. The daemon
serves the selection as `/appearance.css` before the console's own stylesheet; the built-in
themes are also in `tokens.css`, so the console renders if that sheet fails.

### Session tags and filters

`daemon.json` `session_organization` holds budgets only — it holds no tag and no view:
`row_tags_max` (tag chips on one rail row), `recent_tag_toggles` (how many of your most
recently used tags the session header offers as one-click toggles), `bulk_selection_max`,
`views_max`, `views_visible` (views listed before "Show all"), `handoffs_ended_visible`
(default 3: the ended handoffs, most recently ended first, the rail's Handoffs group lists
before "Show ended"; a handoff that has not ended is always listed), `views_file_bytes_max`,
`count_refresh_ms` (how often the rail's view counts and its Handoffs group are read
again, at most), `query_bytes_max`, `query_terms_max`, `glob_expansion_max` (a glob
matching more tags than this is refused by name rather than truncated),
`text_hit_limit`, `board_column_cards_max` (default 200, at most 1000: the cards one board
column shows; a column holding more says how many it does not show), `board_columns_max`
(default 12, at most 50: the columns one board view may declare), `board_placement_rules_max`
(default 24, at most 100: the placement rules one board view may have; lowering either below
what a saved board uses makes that view an entry that cannot be read), `vocabulary_max`, `tag_request_bytes_max`, `tag_index_rows_max` (the
row bound of each read behind the rail's tags; agent-claimed tags count one row per
session identity and agent tag, however often it was re-claimed; a read that reaches it
removes view counts rather than showing low ones), `kept_sessions_max` (how many tagged
sessions outlive their transcript file, most recently tagged first) and
`kept_text_documents_max`. `keymap.tag_session` (default `t`, ignored while typing in a
field) opens the tag editor; an empty value switches it off.

### Saved session views

`session-views.json` in the data directory is **your** list of saved views. Crossing
Guard ships none: there is no built-in view, no example file, and an absent file is an
empty list. The console writes the file when you save, rename, reorder or delete a
view — from the rail's Save view or from the **Settings › Session views** page, which manages
the whole list — so you never have to; and because it is a plain file you can edit it,
copy it to another machine, or keep it under version control.

```json
{"format_version": 1,
 "views": [{"id": "follow-ups", "name": "Follow-ups, any repo",
            "query": "mine:follow-up", "group_by": "repository", "sort": "longest"}]}
```

`id` is any text without spaces or slashes; `query` is the filter grammar of the
[loopback API](loopback-api.md#session-tags-notes-and-saved-views); `group_by` and `sort`
are optional. `record_kind` is `sessions` (the default when absent) or `memory`, for a
view that lists memories instead of sessions. `board` turns a view into a board of
columns: `{"columns": ["building", "review", "done"], "empty_columns": true}` lists the
column order (values of the grouping tag; values not listed follow after) and whether
empty declared columns still show. A column holds the tag value of the same name whatever
its letter case: a column `Review` holds sessions tagged `review`, and is shown as you
spelled it. Two columns that differ only by letter case are one column and are refused; written by
hand, they make the view an entry that cannot be read (below).
A board view must group by `tag-key:<key>`; the key and every column name must be text a
tag can hold (no `:`, `=`, `*` or `"`, and the key cannot be `tag`), because a move writes
the column as your tag under that key. The key need not have been used before.

`board.placement` lists placement rules, each `{"column": "<a declared column>",
"query": "<filter>"}`. A session that carries none of your tags under the board's key is
shown in the column of the first rule, in list order, whose filter it matches; a session
no rule matches is in no column. A rule writes nothing. A rule's filter must have at least
one term and cannot use `status:`, `open:`, search words, or a tag term naming the board's
own key. The file is read on every rail load, so an edit shows without a restart.
The console's **Settings › Session views** page manages this file for you — create, edit,
duplicate, reorder and delete — while right-click on a rail view line stays the quick path.
A save from that page writes the whole view, so a field the page does not show is kept.
An entry that cannot be read is named in the rail beside its problem and the other views
keep working; while such an entry exists the console refuses to save, so it can never
overwrite an edit you have half made. Unknown fields are refused.

### Recall tools

`daemon.json` `recall` bounds the recall tools agent sessions call
([how-to](../how-to/recall-tools.md)) and the routes behind them. Every key must be positive:
`request_timeout_ms` (one route call from the recall server), `max_result_bytes` (one tool
result; whole list items are dropped to fit and the result says `truncated_to_fit`),
`memory_body_max_bytes`, `memory_excerpt_bytes`, `memory_search_limit_max` (also the default
`limit`), `tag_lookup_limit` (one row per session identity and agent tag, however often it
was re-claimed), `peers_max` (per relationship, so other repositories never crowd
out this one), `peer_git_timeout_ms` and `peer_git_concurrency` (each checkout's git reads),
`peer_route_deadline_ms` (the whole peers route; it must be below `request_timeout_ms`, so the
route answers with `unresolved: deadline` rows before the client gives up),
`peer_changed_files_max`, `scope_cache_seconds` (how long a folder's resolved recall
scope — its repository label and, for a checkout with one origin remote, that
repository's id — is reused, so SessionStart hooks in one checkout cost one resolution),
and `scope_cache_max` (how many folders' scopes are held; past it the cache starts over). The base branch peers are compared against is the Diff pane's
`diff.base_ref`. Rolling back to a binary older than this section: its strict loader refuses a
`daemon.json` that contains `recall`, so it falls back to the built-in defaults for every
console setting until the section is removed.

### Understanding facts retention

Every code-analysis scan stores a complete copy of a checkout's units and relationships. The
daemon removes the copies nothing reads any more, by itself, and keeps the scan's own record
(when it ran, what it measured, how much). The `understanding_retention` section of
`daemon.json` paces it:

| key | default | meaning |
| --- | --- | --- |
| `enabled` | `true` | `false` stops pruning at the running pass's next write, without a restart |
| `keep_days` | `14` | how long after a session's newest checkpoint its baseline and current analysis facts are kept (1–36500; to keep everything, set `enabled` to `false`) |
| `intermediate_grace_hours` | `24` | every scan this young is kept, whatever reads it (24–876000) |
| `start_delay_seconds` | `120` | wait between the daemon starting and the first pass (0–604800); separate from the interval, so a daemon restarted often still gets its passes |
| `pass_interval_seconds` | `1800` | time between passes (1–604800) |
| `pause_ms` | `2000` | pause after every write transaction inside a pass (0–600000) |
| `delete_rows_per_transaction` | `10000` | the most unit and edge rows one transaction removes, so no delete holds the store's write lock for long (1–1000000) |
| `delete_descriptors_per_transaction` | `500` | the most unreferenced unit descriptors one transaction removes; descriptors are kilobytes each (1–1000000) |

A scan keeps its facts while it is the baseline or the current boundary of a session seen
within `keep_days`, the newest scan of its checkout, or younger than the grace. Everything
else is pruned: scans taken between a session's baseline and its current state after the
grace, and both boundaries of a session quiet for longer than `keep_days`. A pruned scan is
never read as "measured nothing": the session's code changes say
`analysis facts were removed by retention`, and a generation carries `facts_state`
(`present` or `pruned`). If a checkout returns to a tree whose scan was pruned, it is scanned
again as a new generation. A session that resumes after more than `keep_days` of quiet keeps
working but has no baseline comparison for that checkout again, because the tree its baseline
described no longer exists to re-measure.

`keep_days` and `intermediate_grace_hours` may not be shorter than the 24-hour scan recovery
window. An invalid section turns retention **off**, names the problem in
`GET /api/console/config` and `doctor`, and leaves the rest of the file in force. A
`daemon.json` that does not load at all also turns retention off: deleting is never switched
on by a fallback value. The section is read again before every write of a running pass, so
turning retention off or changing a horizon ends that pass at once. `GET /api/govern/health`
reports `understanding_retention_enabled`, `understanding_retention_last_pass`,
`understanding_facts_pruned`, `understanding_prune_candidates` (the backlog the last pass
left; meaningful only when retention is enabled and a last pass is recorded),
`understanding_prune_unfinished` (pruned scans whose rows are still being deleted),
`understanding_prune_skipped_busy` and `understanding_remeasured`. Pruned pages are reused
by later scans, so the store file stops growing; it shrinks when you stop the daemon and
run `crossing-guard compact`.
Governance events and recorded usage are never pruned automatically; `crossing-guard prune`
remains the only way to remove those. Rolling back to a binary older than this section:
remove the section first (the older strict loader refuses the file otherwise) and follow the
schema-43 note in `store/index.go`.

## Workspace

`workspace.json` in the daemon data directory (example: `config/workspace.example.json`)
enables the mutation side of the workspace domain: candidate checkouts, bound selections,
and managed worktrees. The file is strict: a missing or invalid file disables only that
capability with the reason, and there are no fallback budgets. `allowed_roots` is a
restriction, never an addition.

The console's Diff pane does **not** need this file. Its git scopes read the folder a
session recorded, with budgets from `daemon.json` `diff.*` (owner decision O-G,
2026-09-05): a fresh install shows every scope with no configuration. `diff.base_ref` names
the ref the branch scopes compare against; empty resolves the repository's default branch
at call time, from `origin/HEAD`, then `main`, then `master`.

## Team link (`team.json`)

`team.json` beside the store is written by the daemon when a device links and removed
when it unlinks; it is decoded strictly over the embedded defaults, so a file written by
an earlier version keeps working and gains the newer keys' defaults. Its tunables (each
must be positive):

| key | default | what it bounds |
| --- | --- | --- |
| `report_interval` | `300s` | the device report's cadence |
| `pull_interval` | `60s` | the bundle catalog pull and the shared-memory pull |
| `poll_interval`, `request_timeout`, `enrollment_timeout` | `5s`, `15s`, `10m` | enrollment polling, one signed request, how long a pending enrollment waits |
| `push_interval`, `push_batch`, `push_max_attempts`, `push_max_bytes` | `30s`, `50`, `20`, `1048576` | the outbox drain: cadence, rows per push, retries of a row the server fails to store, bytes per push |
| `parked_kind_retry` | `1h` | how often a record kind an older server refused is tried again |
| `outbox_high_water` | `10000` | the backlog size Settings → Team flags (never a cap) |
| `content_session_cap` | `8388608` | opted-in content bytes one session may send |
| `pull_apply_batch` | `1` | pulled memory rows landed per local transaction |
| `memory_pull_pages` | `20` | pages one memory pull tick lands; a backlog drains over several ticks |
| `memory_pull_start_delay` | `2s` | how long after the link's jobs start the first memory pull runs |
| `identity_upgrade_roots` | `500` | checkout roots considered when a repository's identity is resolved (the guarded upgrade, and the auto-memory import); also the governed sessions one bundle pull resolves to a repository, so a repository-scoped bundle finds its checkouts |
| `identity_resolve_timeout` | `2s` | one repository resolution (git) |
| `deletions_verify_max` | `10` | deletions whose reach one Settings → Team read asks the server about |
| `conflicts_page_max` | `200` | conflict copies one listing returns |
| `not_shareable_window` | `168h` | how long a queued record refused on this device stays in Settings → Team's count |
| `handoff.pull_pages` | `5` | pages one handoff pull tick lands (the handoff pull runs at `pull_interval`, first after `memory_pull_start_delay`) |
| `members_refresh` | `15m` | how often the member directory (user id and display name) is re-read |

Rolling back to a build older than a key: its strict decoder refuses a `team.json` that
contains it. Unlink before rolling back, or remove the newer keys from the file (the
daemon then reports the link as changed outside it; unlink and link again).

### What the team server bounds (for reference)

These are not device settings. They are in the team server's own configuration (its
`server.json`, decoded over the server's embedded `internal/config/config.default.json`)
and are listed here because a device shows their refusals by name:

| server key | default | what a device sees |
| --- | --- | --- |
| `handoff.body_max_bytes` | `200000` | a handoff whose text is larger is refused `over_cap` |
| `handoff.per_member_per_hour` | `60` | more sends from one member in an hour are refused `rate_limited` |
| `handoff.per_recipient_per_hour` | `20` | more sends from one sender to one recipient in an hour are refused `rate_limited` |
| `handoff.unopened_per_recipient` | `50` | a recipient holding this many handoffs no session has opened: `recipient_inbox_full` |
| `retention.handoff_unopened` | `720h` | a handoff no session opened becomes `expired` and its title and text are erased |
| `retention.handoff_body` | `720h` | how long the server keeps a handoff's text after the handoff ended |
| `devices.push_max_bytes` | `1048576` | the size of one push, and of an uploaded signed bundle |

## Model routes (`models/routes.json`)

Named model routes are stored under `<data dir>/models/routes/` and are created in
Settings → Models or with `crossing-guard routes create`; nobody writes those files by
hand. The route owner has two tunables. Their shipped defaults are an embedded document;
to change one, write `<data dir>/models/routes.json` in the same shape. The file is
optional and is decoded strictly: an unknown key, or a value outside its range, is an
error the route API reports, and the bound does not silently change.

```json
{ "format_version": "crossing-guard-model-routes-config-v1", "history_max": 50, "name_max_runes": 80 }
```

| key | default | range | what it bounds |
| --- | --- | --- | --- |
| `history_max` | `50` | 1–500 | earlier revisions one route's record keeps |
| `name_max_runes` | `80` | 1–200 | the length of a route's name |

Route rules (rules that read a `route:` fact) are part of the rulebook like any other
rule and are described in the [loopback API reference](loopback-api.md#model-routes).
None ships.

### Shared bundle defaults (`team.json` `bundle`)

`crossing-guard bundle build` and the Agents page's Share with team… use these when
neither a flag nor the scope's currently published bundle sets the value:

| key | default | what it sets |
| --- | --- | --- |
| `bundle.default_expiry` | `720h` | how long a built bundle is valid when `--expires-in` is not given (must be positive) |
| `bundle.default_failure_mode` | `fail-open` | the failure mode of a scope's **first** bundle (`fail-open` or `fail-closed`); a later revision carries the published one forward unless `--failure-mode` changes it |

A bundle's caps are part of the wire contract, not configuration: at most 8 documents,
at most one of them a rulebook, and at most 262,144 bytes per document body. The server
refuses a bundle over them at upload and a device refuses one before it parses anything.

The built file is written to `<data directory>/bundles/<scope>-r<revision>.json`, and
`bundle sign` writes `…-r<revision>.signed.json` beside it. The organization's private
key is never in the data directory: `org-key init` refuses to write it there or inside a
git checkout.

What a device adopted is recorded in two files it owns. `layers.json` (format version 2)
holds one adopted-bundle record per organization and scope — the bundle id, revision, key
id, failure mode, content policy, expiry, the signed digest and the rulebook digest — and
the pinned organization key. A file at format version 1 is read and rewritten at version
2 the next time something is adopted or pinned. A build older than this one does not
refuse a version 2 file: it reads the pinned key from it and finds no adoption in it, so
after a rollback the device behaves as if it had adopted nothing — no team rule applies —
until you adopt again on that build.

**Upgrading with a bundle already adopted.** A device that adopted a team bundle on a
build older than this one comes up with that adoption carried over from the version 1
file: its rules keep applying until their expiry, with the same failure mode. Two things
differ until the member adopts again, and both are deliberate:

- **It asks once.** The version 1 record held no organization, revision or signed
  digest, so the next bundle the server offers for that scope cannot be matched to what
  was adopted. It is shown as an offer to review and adopt, not refreshed silently — even
  when it is the same bundle the device already had. Adopting it once writes the full
  record; later revisions that change nothing signed then refresh without a prompt.
- **It requires no session content.** A bundle's content mandate is read from the
  `content_policy` in the adopted-bundle record, and a version 1 record has none. Between
  the upgrade and that one re-adoption, a bundle that mandated session content does not
  mandate it on this device: nothing a session-level choice did not already share is
  uploaded on the bundle's say-so. Re-adopting restores the mandate, and the adopt screen
  shows it before you confirm.

Nothing needs editing by hand, and the file is not rewritten until that adoption (or a
key pin) writes it at version 2. A team that depends on the mandate should ask members to
open Settings → Team after upgrading and adopt the offered bundle.

`orchestration/profiles/adoptions/` holds
one record per organization and scope listing the agents that adoption shares. An agent's
selection record gains a `released` field only when an adoption let it go; a build older
than this one refuses a selection that carries it.
## Handoff (`daemon.json`)

The `handoff` section of `daemon.json` bounds handoff between members on this device.
It is in `daemon.json`, not `team.json`, because a local handoff works on a device that
is not linked. Its defaults come from a document embedded in the binary
(`internal/daemon/daemon.default.json`); a `daemon.json` without the section, or with
some of its keys, takes the rest from there.

| key | default | what it bounds |
| --- | --- | --- |
| `handoff.inject_max_bytes` | `4900` | the brief a session is handed at its first prompt |
| `handoff.firing_window` | `720h` | how recently a runtime's hooks must have been observed for Open to be offered for it |
| `handoff.brief_wait` | `168h` | how long an armed brief waits for its session before the device gives up on it |
| `handoff.conversation_turns` | `20` | the turns a ticked conversation excerpt carries (at most 200, the wire schema's bound) |
| `handoff.conversation_max_bytes` | `65536` | the excerpt's text in total (at most 65536) |
| `handoff.recall_check_sessions` | `5` | how many of a runtime's most recent sessions are read to say whether its memory hook has been observed injecting |
| `handoff.follow_retry` | `1s` | the pause before the daemon follows the console's task events again after that stream closed (it is how an opened session's first turn is told apart from one that never started) |

Every value must be positive. A section that fails this falls back to the defaults as a
whole, is named in `GET /api/console/config`'s `problems` and by `doctor`, and leaves the
rest of the file in force.

`handoff.inject_max_bytes` is also checked, when the file loads, against the two other
owners a brief crosses, so that a brief always arrives whole and no runtime is left to
cut it: it must be no more than the smallest hook context cap (7,000 bytes for Codex,
9,000 for Claude Code — each runtime's own, not settings) and no more than
`delivery.claim_bytes` less the bytes that join a brief to the other messages a session
at `delivery.max_pending_per_session` is handed with it (`orchestration.json`; 6,996 with
the defaults). It must also hold the brief's own first line and its closing lines (the
tool that returns the rest, and the line about the repository's shared memory at its
widest count). A value outside
those bounds is refused by name — `handoff.inject_max_bytes is 9000 and must be at most
6996 (…)` — and the built-in handoff bounds stay in force. `orchestration.json` is read
once per start, so the same check is repeated each time a brief is built.

`handoff.firing_window` and `handoff.brief_wait` work with two settings they do not own:
a brief is one pending delivery, so it lives `delivery.ttl_seconds` (1,800 s) at a time
and is armed again when that runs out, until `handoff.brief_wait` has passed since it was
last asked for; and Open is offered in a runtime only when the prompt kind
(`turn.started`) is among `delivery.carrier_kinds`.

## Memory import and proposals

`memory.json` in the daemon data directory (example: `config/memory.example.json`) owns
the memory system's policy. The import half paces the daemon's own import of vendor
auto-memory (today: Claude Code's per-project topic files) into the memory store:
`import_min_interval_seconds` is the least time between two imports; a session end inside
that window marks one pending run that the next lifecycle sweep performs.
`import_on_session_end` turns the session-end trigger off; the sweep still runs the first
import after boot and drains anything pending.

The `propose` half is the agent proposal door's consent: `propose.enabled` (default off —
the recall tools' registration never turns it on) and `propose.per_session_max` (how many
proposals one session may make per hour). The door only ever creates `pending` records a
human promotes. **The consent is live**: the daemon re-reads the file when it changes, so
flipping `propose.enabled` is obeyed by the next proposal — no restart.

The `synthesis` half is the daemon's session-synthesis step (synthesis v1): when
`synthesis.enabled` is on (default off), an ended session with repeated failure-shaped events
(two or more tags with key `error` or `data-class=error`) drafts at most ONE pending
lesson candidate built from recorded facts only, redacted, citing the session, for the
human promote gate. No shipped detector emits those tags, so with the shipped detector
library synthesis does not trigger; a detector overlay that emits them enables it.
`synthesis.max_per_day` (default 20) bounds drafts per UTC day. The
gate is read at the session-end moment, so a flip applies to the NEXT ended session;
errors are skipped and counted (doctor shows the counters), never retried. The queue is
`memory list --status pending` and the console's pending inbox.

A missing file yields the compiled defaults; a malformed file is logged and the defaults
are used; `doctor` prints the last import (time, reason, counts) from
`memory-import-state.json` beside the file. The file is hand-authored — the product never
writes it; the example file is the template. The store itself is `$CG_MEMORY_DIR` or
`~/.crossing-guard/memory` (the write-through mirror; the records live in the store the
daemon reads); a daemon started with `--no-hook-install` never imports into it.
`memory search`, `memory get`, `memory list` and the SessionStart `memory index` read through
the running daemon and never open the store themselves. They ask the daemon which index it
reads and refuse, naming both paths, when it is not the one the other `memory` commands write (`$CG_INDEX` /
`$CPMEM_INDEX`, else `~/.crossing-guard/index.sqlite`), and with the daemon down they say so
instead of reading the file.

`memory index --runtime NAME` selects the registered memory adapter's output
format explicitly. Existing hook writers keep their canonical commands;
new adapters can opt into explicit selection in their own commands. Explicit
selection does not read stdin; an empty, unknown or unsupported runtime fails
before reading records or creating an emission nonce. Without the option,
manual output stays plaintext and existing native hooks retain their bounded
legacy-payload detection. This option does not migrate existing attachments
or change their removal contract. Configuration or JSON output alone does not prove
context delivery: `doctor` verifies a logged emission nonce in the runtime's
actual injected-context position.

`daemon.json` was `console.json` until 2026-09-27. A data directory still holding the old
name is adopted once — copied to `daemon.json` on the next daemon start when the console
is idle, with one log line — and the old file is left in place, ignored from then on. An
old file carrying the pre-move `recall.propose_*` keys loads with a problems note naming
the move; its other settings are untouched.

## Session status

Two typed configuration owners, both under the data directory, both with
compiled defaults used when the file is absent and a visible error when it
is malformed. Neither is a general configuration service.

`session-stream.json` — the live session stream and the status decider:

| key | default | meaning |
| --- | --- | --- |
| `poll_seconds` | 2 | how often an open stream re-reads its session (floor: the harvest reuse window) |
| `quiet_seconds` | 300 | silence after a turn start, tool call, or question past which a session reads "no update for N" |
| `coalesce_ms` | 250 | how long a burst of facts about one session is held before one refold |
| `lookback_rows` | 32 | how many recent rows of each kind the decider reads |
| `owned_end_attribution_seconds` | 5 | a session end this close to a console-started task's completion is read as that process ending, not as the person's session ending |
| `progress_window_records` | 64 | how many of the newest transcript events the turn-progress words ("thinking…", "writing…", "running <tool>") are read from, for the open session only |
| `min_thought_seconds` | 1 | the shortest thought that renders as "thought for N"; shorter ones are omitted |
| `snapshot_events` / `keepalive_seconds` / `max_streams` / `governance_window` | 400 / 15 / 16 / 60 | stream bounds |
| `client_backoff_cap_seconds` / `client_age_tick_seconds` | 10 / 15 | published to the browser |
| `attention.ask_line_max_chars` | 280 | the longest agent line (an ask or an unsent reply) a session's status carries; longer lines are cut and marked |
| `attention.ask_horizon_seconds` | 604800 (7 days) | how far back an unresolved agent ask or unsent reply keeps its session on the rail |
| `attention.ask_notify` | true | one OS notification per agent ask, when no console window is visible and focused; `CG_NOTIFY=off` in the daemon's environment wins |
| `attention.subagent_reentry_ms` | 2000 | a turn start this soon after a background sub-agent ended is the runtime re-entering its result, not the person answering, so it does not resolve an ask |

An agent ask or unsent reply stays on a session until the person starts a turn in it (one the
daemon did not launch) or reads it in the console. Reading it is remembered per browser: another
device shows it again until the person's next turn in that session.

`session-activity.json` — the presence sampler and turn-row retention:

| key | default | meaning |
| --- | --- | --- |
| `sampler_interval_seconds` / `sampler_timeout_seconds` / `sampler_ttl_seconds` | 30 / 2 / 45 | presence sampling |
| `liveness_live_seconds` / `liveness_recent_seconds` / `liveness_window_seconds` / `liveness_horizon_seconds` | 120 / 900 / 3600 / 604800 | hook-liveness grading (must increase) |
| `max_rail_sessions` | 200 | the most sessions the hook-liveness and recent-turn lanes will report open at once |
| `rail_keepalive_seconds` | 15 | activity stream keepalive |
| `turn_retention_days` | 30 | how long turn-boundary rows are kept |

`usage-history.json` — the usage recorder, which reads every runtime's usage sources into
the store and keeps the calls after the vendor removes its files. No key deletes anything:
only `crossing-guard prune --usage` removes recorded calls, so an unusable file falls back
to these defaults safely.

| key | default | meaning |
| --- | --- | --- |
| `recorder.interval_seconds` | 30 | how often the recorder reads sources; at most 86400 (a day) |
| `recorder.read_budget_bytes` | 268435456 | the most bytes one pass reads; the first pass after install reads every source once, across as many passes as this takes |
| `report.max_groups` | 24 | the most groups `GET /api/usage/breakdown` returns, largest first |
| `report.top_sessions` | 12 | rows in each top-sessions table of `GET /api/usage` |
| `report.session_series_points` | 600 | the most points in one session's context series (`GET /api/session/usage`); at most 20000, about 600 KB |
| `report.session_agent_ticks` | 500 | the most call times listed per agent in one session's usage; at most 20000 |
| `report.session_members_max` | 200 | the most subagents, and the most agents, one session's usage lists, largest first |
| `report.agent_sessions_max` | 100000 | the most agent sessions read to split usage into agents' work; past it the split reads `incomplete` |

## Task inputs

Image and file admission is disabled until the daemon data directory contains a valid
`task-inputs.json`. Start from
[`task-inputs.example.json`](config/task-inputs.example.json), review its positive
text-extension and image-media-type allowlists and its byte/pixel/expiry ceilings, then
copy it to `<data-directory>/task-inputs.json` before restarting the daemon. Missing,
malformed, unknown-field, or future-version configuration disables only attachment
admission; text-only tasks remain available.

The server treats the browser filename and media declaration as untrusted. It bounds
the upload, sniffs the bytes, requires extension/type agreement, rejects non-UTF-8 or
NUL-bearing text, and fully decodes images before staging. Static PNG, JPEG, GIF, and
WebP inputs are converted to a metadata-free PNG provider copy. Animated images,
embedded ICC profiles, malformed/truncated images, unknown binary files, and limit
excesses are rejected before a coding runtime starts. Original and prepared bytes live
only under the daemon's owner-only expiring task-input directory; task events contain
safe names/types/sizes, never the scope token or local provider path.

Provider delivery remains exact and visible: Codex receives prepared images through
its proved repeatable image argument and bounded text in the prompt; Claude receives
only daemon-owned opaque staged paths; OpenCode receives ordered file parts through its private loopback server API.
For a runtime that publishes a model list (`GET /api/chat/models`), an image is admitted
only when the request names a model (a typed id included) and that runtime's list says
the model accepts images; an unlisted model, or a list that could not be read, is
refused before launch. Runtimes without a list (Claude, and Codex, which advertises no
image input for a named model) keep their own adapter rules. OpenCode additionally
requires the no-tools `vision-proof` mode. OpenCode learns which models accept images from
its own configuration: declare a local model under
`provider.<id>.models.<model>` with `modalities.input` including `"image"` (and a
`limit.context`, which then also shows in the composer). A combination that has not been
stated fails before launch instead of silently dropping an attachment.

The `vision-proof` mode is admitted only when the OpenCode binary that will run lists an
agent named `vision-proof` as a primary agent (or one with no mode) in **global**
OpenCode configuration (`~/.config/opencode`, `OPENCODE_CONFIG`, or
`OPENCODE_CONFIG_DIR`); otherwise the request is refused before launch. OpenCode would
otherwise fall back to its default agent, which may use every tool. Turns in this mode
ignore project configuration (`opencode.json`, `.opencode/`, project instructions), so
the agent and the vision model must both be declared globally, and they do not accept
extra arguments. Configure the agent with every tool disabled; Crossing Guard checks that
it exists, not what it permits.

OpenCode extra arguments (Settings → Runtimes → extra args, or a request's `extra_args`) must be
empty. The interactive server transport has no verified safe mapping for raw `run`
options, so any nonempty value is refused before the task is created. OpenCode's configured
permissions still apply: requests with an `ask` decision wait in **Awaiting Approval**,
and an Allow grants only that exact request once.

Codex extra arguments (Settings → Runtimes → extra args, or a request's `extra_args`) accept only a
bare `--skip-git-repo-check` (Codex otherwise refuses to run in a directory that is not
a trusted Git repository) and one reasoning-effort override written
`-c model_reasoning_effort=<level>`, `--config model_reasoning_effort=<level>` or
`--config=model_reasoning_effort=<level>`, where the level is a short lowercase word that
Codex's provider checks (for example `low`, `medium`, `high`). Each can appear at most
once. Anything else is refused before the task is created, including
`--dangerously-bypass-approvals-and-sandbox` and its hidden alias `--yolo`, any other
`-c`/`--config` key (such as `sandbox_mode` or `approval_policy`), `--sandbox`,
`--profile`, `--add-dir`, `--cd`, `--ignore-user-config` and options Crossing Guard sets
itself. The session id and prompt are always passed after a `--`, so a prompt such as
`--yolo` or `review` is sent as text. So the selected mode's sandbox is not overridden
from Settings. This guards against a saved setting, not against a caller who holds the
API token and also picks the Codex binary path.

**Where the Codex executable is found.** A binary path set in Settings is used for chat
turns only. Otherwise Crossing Guard looks on the service's `PATH`, then in the known
install directories (`/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin`, `~/.bun/bin`,
`~/.volta/bin`, `~/go/bin`, and the nvm, asdf and pnpm Node directories), then inside the
ChatGPT app on macOS: the app's declared entrypoint
`Contents/Resources/codex-cli/bin/codex`, then the pre-2026-09-30 location
`Contents/Resources/codex`. A copy in a searched directory therefore overrides the
bundled one. If the executable cannot be found, the error lists every location tried;
placing or linking it in one of the searched directories fixes every feature (turns,
sign-in status, the model catalog, the skills probe and session messages). Linux and
Windows are unverified. On Windows only a `codex` found on `PATH` resolves: a configured
path and the install directories are not recognised there.

The selected Codex mode is always sent to Codex as its sandbox, on new and resumed
turns, Read Only included. A `sandbox_mode` in Codex's own config, or a project Codex has
marked trusted (which otherwise runs workspace-write), does not widen it. To let Codex
edit files, pick Workspace Write.

## Speech

Dictation in the console composer is disabled until the daemon data directory contains a
valid `speech.json`. Start from
[`speech.example.json`](config/speech.example.json), set the local model path and its
SHA-256, review the limits, copy it to `<data-directory>/speech.json`, and restart the
daemon. A missing, malformed, unknown-field, or future-version file disables dictation
only; typing and attachments are unaffected. Settings → Dictation shows what the file says,
whether it loaded, and the exact problem when it did not.

The `transcription.backend` value selects one registered adapter. `whispercpp` runs the
installed `whisper-cli` inside a deny-by-default `sandbox-exec` profile with no network
access, CPU only, and hints only in its prompt; dictated text never enters a process
argument. `openai-batch` uploads the finished recording to the configured OpenAI origin
after you let go, using the key named by `api_key_env`; it never reads any coding
runtime's login. A backend that needs a disclosure shows its operator-authored text before
the first byte leaves the machine, and the daemon refuses to open the stream until that
disclosure is accepted for the dictation at hand.

Hints (the project directory name, the current branch when the task is bound to a
workspace selection, and a keyword list) are configured per backend. They default off for
a disclosure backend, and when enabled there the disclosure text must name each one or the
file fails validation. Every duration, energy floor, confidence floor, cadence, window,
cap, and key binding in the file is policy; the daemon and the browser hold no fallback
values. Raw audio lives only under the daemon's private `speech-clips` directory for the
life of one dictation and is deleted on every exit path, including at the next boot.

Transcript search is derived local runtime data, not configuration. When the daemon is
running, one Governor-independent worker reconciles at startup and after each 30-second
interval, admitting at most 100 newest changed canonical sessions per sweep. It retries
typed repository contention after two seconds. `crossing-guard harvest` and
`crossing-guard harvest --rebuild` are explicit projection-only recovery commands; they
never delete the unified store. No vendor `Stop` hook or orchestration profile configures
this scheduler.

Executable analyzer selection is likewise not repository configuration. It is explicit,
user/data-directory-scoped lifecycle state; repositories cannot activate modules. See
[Code analyzer modules](analyzer-modules.md).

## Independent report-only review

Reusable orchestration profiles are user configuration, but selecting one is inert.
The current report-review subset requires a second, explicit activation in
**Settings → Agents**, on the reviewer agent's **Where it runs** tab. That local binding
pins:

- one exact selected profile source and compiled digest;
- a literal loopback Ollama endpoint (`127.0.0.1` or `[::1]`), with no proxy or redirect;
- one explicit installed model name;
- a timeout no greater than the profile ceiling; and
- an optional exact runtime filter.

Saving checks the configuration but does not contact Ollama, download a model, or prove
availability. Settings says **availability unverified** until invocation history shows
a result. Updating a selected `PROFILE.md` does not roll the binding forward; Settings
shows **Update available** and requires an explicit review/save. Disabling prevents new
admission and retains prior history.

Each admitted review consumes the existing bounded pre-tool observation and runs after
that observation commits. The hook makes no second review request and does not wait for
the model. Stored history contains pinned identities, lifecycle/latency, bounded
validated recommendation and supplied-fact citations—not the assembled prompt, hidden
reasoning, credential, or a second full tool-input copy. A recommendation is always
report only and cannot change governance, approval, hook, or tool outcomes.

The framework also records versioned declaration names and adapter-produced body-shape
digests. For one exact stored file, it can show other files with an identical full
declaration name or identical non-empty digest:

```text
crossing-guard understand --data DIR show --generation ID --compare PATH --json
```

These are mechanical candidates, not duplicate/slop findings. The command and session
Impact view show the exact analyzer, coverage state, match basis, totals, paging, and
supporting dependency intersections. Returned-of-total caps remain visible, and native
expandable controls expose the full shape digest, node count, and left/right declaration
facts without relying on hover text. They apply no score, threshold, ignored-path list,
quality label, recommendation, or consequence. Any such interpretation remains selected
configuration and is not active merely because a repository was scanned.

Durable scans do not discover a project convention file. A convention document affects
only the invocation that explicitly names it:

```text
crossing-guard understand scan --repo /path/to/repo --base HEAD \
  --conventions /path/to/conventions.json
```

The file reference and digest are stored with the generation. Missing, symlinked,
mutating, oversized, or malformed input fails without falling back. Session Impact uses
only an exact no-convention generation; an explicit convention generation is inspectable
but is not silently treated as the session's default truth.

## Action observation data and local sensitivity

Action observation is derived runtime evidence, not policy configuration. New v1 hook
observations keep one canonical action and attach delivery identity, exact declared
resource fields, and bounded structured tool input in the local SQLite store. Complete
input is retained through 1 MiB; larger input keeps only exact byte count and digest.
Raw input is not full-text indexed and is not returned by an API in this collection
slice.

Pending delivery uses owner-only local files under
`~/.crossing-guard/observation-spool`. The directory is limited to 10,000 regular files
or 512 MiB and never deletes older evidence merely because it aged. A compatible daemon
drains it automatically after startup and removes a file only after an acknowledgement
for the same observation identity.

The live store, spool, and `crossing-guard export` snapshots may therefore contain code,
commands, URLs, MCP arguments, and secrets supplied to tools. Treat all three as
sensitive local artifacts. No observation payload is uploaded by this mechanism.

### Result payload retention

After-tool collection has a separate, explicit local retention choice in
`~/.crossing-guard/collection.json`:

```json
{
  "format_version": 1,
  "result_payload_mode": "code-effects"
}
```

The supported modes are:

- `metadata-only`: keep result status, byte counts, digests, and typed file-effect
  metadata, but no result, diff, replacement, or content bodies;
- `code-effects` (the default when the file is absent): keep bounded diff/content and
  proposed-replacement bodies for typed code effects, but not generic tool output; and
- `complete-bounded`: additionally keep bounded generic result output.

All retained result and effect bodies share a 1 MiB envelope bound. Automatic Git
checkpoints separately keep at most 1 MiB per path, 8 MiB per checkpoint, and 128 paths;
their byte counts and SHA-256 digests still describe the complete observed stream when a
body is bounded. The daemon re-applies the selected mode before SQLite persistence, so a
hook cannot bypass the receiver's retention setting. Present-but-malformed, unknown, or
future-version configuration fails visibly instead of silently reverting to a more
permissive mode. Health and the session collection projection report the selected mode
and configuration origin; ordinary session APIs return metadata and digests, not the raw
bodies.

## First-alpha catalog decision

Fresh mechanism-first installations activate the 30-detector portable floor and an empty
unselected rulebook. The first public catalog is deliberately small:

- `safety-starter`: six command/canary rules (including the team-link tripwire); it needs no optional detector document;
- `security-observe`: the safety rules plus one report-only credential/external-
  destination observation, paired with a complete detector assembly containing the
  portable floor and the required credential/destination heuristics;
- legacy 70-detector and nine-rule documents: compatibility artifacts for existing
  installations, not recommended starters.

Conversational labels and plan/red-team workflow rules are not part of the first public
catalog. No catalog item is selected by install, init, project discovery, or demo. The two
named rulebooks are selectable in the current source candidate. The paired
`security-observe` detector assembly is not yet implemented, so selecting that rulebook
alone does not establish the credential/destination facts it needs or prove that egress
was observed. Use an explicitly reviewed complete detector document for those facts until
the paired assembly is implemented and verified.

## Rulebook lifecycle

One `engine.Policy` rulebook is active at a time. `crossing-guard rules status` reports:

- **available starter** — the shipped catalog's content digest; availability is not consent;
- **selected** — whether a durable or invocation choice exists;
- **active** — whether the exact returned bytes passed migration, decoding, and predicate
  compilation;
- **origin and selection** — the layer and activation mechanism;
- **digest and path** — inspectable identity for the exact active bytes;
- **selector and selected-at** — attribution for durable user choice;
- **selected source and source reference** — the reviewed candidate kind plus its exact
  path/catalog reference, or the prior selected digest when Policy edits advance it;
- **compatibility** — legacy activation that preserves old behavior but is not consent;
- **displaced selection** — a durable choice hidden temporarily by `$CG_RULES`.

### Commands

```text
crossing-guard rules status
crossing-guard rules select current [--yes]
crossing-guard rules select safety-starter [--yes]
crossing-guard rules select security-observe [--yes]
crossing-guard rules select none [--yes]
crossing-guard rules select PATH [--yes]
crossing-guard rules unselect [--yes]
```

The current top-level help synopsis still abbreviates this source list as
`current|starter|none|PATH`; the `rules` implementation accepts the named catalog sources
shown above. Publication remains blocked until compiled help and this reference agree.

Each selection review is bound to the current state token and proposed content digest.
Concurrent or stale writers fail with reload/review guidance. Selected documents are
immutable SHA-256 snapshots; Policy saves create a new snapshot and atomically advance the
selection record. Unselect archives the record and returns to the installation cohort's
baseline while preserving snapshots.

The durable configuration record is
`~/.crossing-guard/policy/rulebook/selection.json`; selected bytes live under
`~/.crossing-guard/policy/rulebook/documents/sha256-<digest>.json`. These are owned by
the rulebook lifecycle mechanism. The JSON inside the snapshots and the selection choice
remain configuration, not framework policy.

`none` is an explicit selected rulebook with an empty `rules` array. A mechanism-first
installation already has an unselected empty baseline; these states have the same current
consequence but different selection facts. Capture and structural
framework behavior remain available, but no configured ask/deny/observe consequence and no
canary rule is active.

### Compatibility and precedence

Current migration precedence is:

1. `$CG_RULES`: invocation-scoped file, shown as `invocation-path`;
2. durable user selection: content-addressed document, shown as `explicit-user`;
3. cohort baseline: an older compatibility installation retains executable/user file
   discovery and the embedded starter fallback; a mechanism-first installation uses an
   empty unselected rulebook that cannot ask, deny, or observe.

A missing invocation file cannot silently expose a displaced durable selection. A
malformed record, missing/tampered snapshot, invalid selected policy, or digest mismatch
fails loudly rather than activating different policy. Remove `$CG_RULES` before Policy UI
edits or rollback. The CLI may create a durable selection while an override is active, but
it states that the choice remains displaced until the override is removed.

Older binaries do not understand the selection/cohort record and may reactivate implicit
defaults. Downgrade is unsupported; unselect preserves archives but cannot make an older
binary implement the mechanism-first contract.

## Rule document

```json
{
  "rules": [
    {
      "id": "protect-shared-history",
      "intent": "Do not rewrite shared history without review.",
      "action": "ask",
      "if": { "tag": "command", "matches": "git\\s+push\\b.*--force" }
    }
  ]
}
```

Rules use one predicate language:

- leaf: `{"tag":"key","value":"exact"}` or `{"tag":"key","matches":"RE2"}`;
- composition: `all`, `any`, or `not`;
- action: `allow`, `observe`, `ask`, `redact`, or `deny`;
- an explicit engine `mode` remains supported for tag-oriented policies.

Standalone PreToolUse evaluation mechanically supplies two policy facts:

- `command` — exact raw command text, including an empty value for non-command tools;
- `tool` — exact bare tool identity when the parsed invocation names one.

This permits a narrow non-shell rule without content inference:

```json
{
  "id": "deny-artifact-publication",
  "action": "deny",
  "message": "Artifact publication is disabled; write the deliverable locally",
  "if": { "tag": "tool", "value": "Artifact" }
}
```

`tool` is evaluated from the same normalized identity stored in `event.tool`; it is not
stored again as a frozen tag. An exact rule covers only parsed PreToolUse calls using
that identity. It does not cover renamed tools, browser uploads, direct network clients,
disabled/bypassed hooks, or provider routes outside the hook. A hard deny is
non-overridable and its response does not suggest a manual bypass.

Documents validate before activation. Unknown actions and invalid regular expressions are
loud errors. Mechanical selection review lists added/removed rule IDs and changed actions/
predicate digests, but arbitrary regex and state-predicate semantics are **not enumerable**.

### Live-path predicate boundary

The selected rulebook has two current live tiers:

- local static evaluation supplies only the exact `command` and bare `tool` facts;
- daemon-backed stateful evaluation handles rules that reference at least one
  `session:*`, `target:*` or `agent:<binding>:<tag>` fact and can combine those stored
  facts with current-action detector tags.

Stored facts come from detectors, from facts the daemon writes itself (today only
`session:work=uncommitted`, written at checkpoint settle), and from tags a saved binding
may claim (`agent:<binding>:<tag>`, value `model-claimed`, limited to the binding's
declared tags from its profile's `may-tag` vocabulary). `crossing-guard coverage` counts
all three as producers. A claim is model judgment, so a claim-backed rule is labeled
precautionary, never complete. Where the stateful tier is not armed (see the platform
record in `doctor`), or does not load the rule set (the hook-side invocation file; adopted team layers are loaded),
`coverage` labels those rules at a static reach instead, where no stored fact reaches them.

Both live tiers read the same layered policy: the selected rulebook, then the adopted
repository layer for the tool call's checkout (once the daemon has resolved that
checkout), then any adopted organization layer. A state-referencing rule in an adopted
team layer is decided by the daemon the same way as one in the selected rulebook. The
hook's local tier sets every state-referencing rule aside, so a rule that negates a state
fact (`not: session:…`) is decided by the daemon alone. A team layer that cannot be read contributes
no rules to either tier. State rules in the older invocation-policy file (`CG_POLICY`)
are not evaluated by the daemon, because that file belongs to the hook's environment.
`check` evaluates the selected rulebook only, not adopted team layers.

A selected rule containing only an unprefixed detector tag such as `fs`, `data-class`, or
`destination-class` is not currently a supported live-hook consequence path. The older
invocation-policy mechanism can evaluate event-local detector tags, but it is a
compatibility surface rather than the first-alpha selected-rulebook journey.
`crossing-guard coverage` labels such a rule `INERT` unless the invocation file your
environment resolves makes the hook's engine tier load the rulebook (no rules, or the
rulebook itself); `/api/policy/coverage` labels it `UNVERIFIED`. Coverage labels each live
tier on its own tags. A rule mixing `tool` or `command` with a `session:` fact is decided
by the stateful tier, which has all three, and is `INERT` where that tier is not armed. The stateful
consult deliberately fails open when the daemon, platform gate, or required state is
unavailable. Use `check` for command/tool rules, verify stateful rules through a real firing
hook plus daemon, and never infer live enforcement from an audit match.

## Detector lifecycle

The detector owner currently exposes two embedded assemblies:

- a 30-detector structural baseline containing deterministic tool/action facts; and
- an available 70-detector starter that additionally contains architecture/path roles,
  sensitivity and personal-data patterns, destination trust, risk, agent/user behavior,
  and workflow phase configuration. This 70-detector artifact is retained for migration
  compatibility and is not the recommended first-alpha starter.

A mechanism-first installation activates only the structural baseline. An older
compatibility installation retains the complete starter plus its historical merge-by-ID
overlay as compatibility, not consent.
The old daemon root overlay and policy overlay were used by different surfaces; when their
effective digests differ, status and Policy show both rather than silently choosing one.

```text
crossing-guard detectors status
crossing-guard detectors list
crossing-guard detectors preview (current|starter|baseline|PATH)
crossing-guard detectors select (current|starter|baseline|PATH) [--yes]
crossing-guard detectors unselect [--yes]
crossing-guard detectors init [PATH]
crossing-guard detectors lint PATH
```

Selection pins a complete validated detector snapshot under
`~/.crossing-guard/detectors/documents/`; the durable record is
`~/.crossing-guard/detectors/selection.json`. `init` writes an inert complete draft and
does not select it. That draft is seeded from the legacy 70-detector compatibility starter,
not a recommended first-alpha pack; review and remove its opinionated entries before use.
`$CG_DETECTORS` and daemon `--detectors` remain invocation overlays
over the full starter for compatibility and visibly displace any durable selection.
A detector tag key starting with `session:`, `target:` or `agent:` is reserved for the
stateful tier's own state and is rejected wherever a document loads: `lint`, `preview`,
`select`, the invocation overlays and a durable selection.
Missing, tampered or invalid selected bytes make detector-dependent paths unavailable.
There is no silent fallback to another detector set. `status` names the error; rules over
detector tags stop deciding at the hook, and command/tool rules still apply.
Recovery depends on the source: `detectors unselect --yes` for a durable selection (a new
`select` is refused while the active document is invalid); fix or unset
`$CG_DETECTORS`/`--detectors`; fix or remove the legacy `policy/detectors.json` overlay.
Daemon selection requires a restart because ledger/audit/governor cache
their exact assembly at process start; Policy displays configured and runtime digests.

The create-once `<data>/installation.json` is framework state recording whether the data
root belongs to the older compatibility cohort or the mechanism-first cohort. It contains
no selected configuration and never implies runtime attachment or policy consent.
`init --dry-run` does not create it.

Runtime attachment is framework state and separate consent. Attaching Claude, Codex, or
the collection-only OpenCode candidate does not select a rulebook or detector policy.
OpenCode's plugin invokes `collect-hook`, not the governed `hook` command, so its
before-tool phase records an attempt without allowing, asking, or denying it.
