# Session evidence API

The local console reads factual session evidence from:

```text
GET /api/govern/session?runtime=RUNTIME&id=SESSION_ID&section=SECTION
```

Send the local API token using the same authenticated mechanism as other console API
calls. `runtime` is an opaque registered runtime identity. Generic storage and response
code do not interpret a vendor name.

The 2026-08-30 console candidate can render these unchanged bounded sections in one full
evidence pane or two vertically split evidence panes. **Views** changes only browser
presentation; it does not add an endpoint, broaden a response, duplicate a cache, or
turn an evidence read into a live-workspace operation. The installed daemon may still
show the predecessor one-view-at-a-time rail until this candidate is promoted.

## Sections

| section | returned facts |
| --- | --- |
| `summary` | exact owned totals and explicit unavailable/partial boundaries; no row populations |
| `change` | bounded declaration/touch/checkout-change/claim projection used by Changes and Plan |
| `impact` | repository identity and measured downstream package/reference facts with explicit coverage |
| `verify` | repository identity and verification witnesses only |
| `reach` | observed runtime and decision-mode reach facts |
| `trace` | one chronological, filterable, snapshot-bound action page plus its exact declared resources |
| `action` | one session-owned action, delivery/input metadata, exact result observations, typed effects, and reconciliation |
| `file` | bounded actions, effects, and path reconciliation linked to one server-issued Change-row key |
| `code-changes` | bounded observed-checkout code-state comparison for one exact repository/checkout/checkpoint identity |
| `code-change-file` | one centered file comparison with changed declarations, exact structural-match changes, dependency facts, and separate action-attribution evidence |
| `statements` | bounded storage-sanitized, source-attributed user/assistant statement snippets; display evidence, never approval |
| `checks` | bounded observed test/build actions plus deterministic result-link and retained-byte metadata; not a verification witness |
| `edits` | the session's recorded edits, grouped by file in completion order: each effect that retained a body, described by kind (`replacement`, `content`, `diff`), byte counts, `replace_all`, tool, and state; live and transcript copies of one logical result collapse to the live copy; `limit` (capped by `edits_page_size`) and `offset` page; bodies are never inlined |
| `body` | one explicitly requested, session-owned, complete retained textual input/result/effect body (`body_kind` is `event_input`, `result`, `effect_content`, `effect_diff`, `effect_before`, or `effect_after`; the last two are a replacement's sides and are `unavailable` when that side was not retained) |
| `capture` | bounded compatibility facts used by the Governance Capture center |

Trace accepts `limit`, an opaque `cursor`, `query`, `decision`, and `origin`. Follow
`next_cursor` without inspecting or modifying it. Refresh without a cursor starts a new
snapshot, which may include newly captured or backfilled actions.

Action detail requires `event_id`. File detail requires the opaque `file_key` returned by
the selected Change row; a free-form path is not accepted. Body detail requires a
supported `body_kind` plus its action/result/effect selector. The server verifies that
every selector belongs to the requested session. Missing or cross-session evidence is
404; malformed selectors are 400.

`code-changes` accepts `repository_id`, `checkout_id`, optional `checkpoint_id`,
`code_offset`, and `code_limit` (maximum 25). A session with more than one checkout
requires the exact repository and checkout identities returned in
`available_checkouts`. The response selects the current checkpoint first and then asks
for its exact current-schema/current-analyzer generation. It never falls back to an
older analyzed checkpoint. `state` is `exact`, `baseline_unavailable`, `pending`,
`failed`, or `unavailable`; `pending` means analysis was already admitted by collection,
not that the HTTP read started work.

A boundary whose analysis facts retention removed (see `understanding_retention` in the
configuration reference) is never rendered as an empty comparison: a pruned baseline returns
state `baseline_unavailable`, a pruned current boundary (including one named by
`checkpoint_id`) and a pruned impact generation return `unavailable`, each with the reason
`analysis facts were removed by retention`. Boundary and generation objects carry
`facts_state` (`present` or `pruned`).

`code-change-file` additionally requires a normalized repository-relative `path` and
accepts `structural_offset` and `structural_limit` (maximum 100). It returns mechanical
facts only: declaration added/removed/modified/moved state, before/after source-span
lines and optional cyclomatic values, exact body-shape relationships added/removed,
dependent packages, referencing source files, and exact analyzer-qualified incoming
`symbol_calls_symbol` edges when the selected analyzer recorded that capability.
Function-call state follows the recorded per-analyzer coverage (`exact`, `partial`,
`unsupported`, `failed`, or `unavailable`); package dependents are never substituted for
function callers. Structural matches are not labeled duplicates, and no method-size
threshold, architecture role, quality score, or causal actor claim is produced.

`code-changes` also returns bounded aggregate populations for files and declaration
changes, added/removed/modified/moved declarations, comparable source-span and
cyclomatic deltas, exact dependent-package/referencing-file totals, incoming call rows,
and structural-match relationships. A bounded value list may be truncated while its
distinct total remains exact. Every aggregate carries its own state and reason.

`statements` accepts `statement_offset` and `statement_limit` (maximum 100). It reads the
existing storage-sanitized transcript projection and returns snippets, role/runtime
attribution, time, returned/total, truncation, and rebuildable-source state. Storage
sanitization removes invalid/control bytes and caps retained text; it is not a secret-
redaction claim. The section creates no durable statement copy and is not a governance
input. The response also carries the shared session-specific transcript-index `coverage`
envelope (`state`, `coverage_as_of`, counts, and closed limitations). An exact statement
total of zero is authoritative only when that coverage state is `current`.

`checks` accepts `check_offset` and `check_limit` (maximum 100). Rows are frozen from
observed test/build-tagged actions and batch-join current result observations. Result
state is `not_executed`, `held`, `pending`, `exact`, `indirect`, `ambiguous`,
`terminal_without_retained_body`, `unknown_due_to_bound`, or `missing`; the response
retains duration and raw/retained byte metadata without inlining result bodies. An
observed check is not promoted to a process-level verification witness.

Both sections are read-only. They load only stored immutable generations. A list page
fetches descriptors only for its selected paths; the one-file structural comparison is
bounded to one center against at most 10,000 stored units and never constructs an
all-pairs graph. `current_checkpoint_path_total` is the Git-observed path population for
the selected checkpoint, while `file_page.total` is the analyzed-unit delta (or current
analyzed units when the exact baseline is unavailable). Action/result/path
reconciliation remains a separate `attribution` population so a checkout delta is not
misstated as proof that the agent authored every byte.

The console composes these sections into three primary session views: **Changes**,
**Effects**, and **Plan**. Activity is a bounded disclosure under Changes; Verification
and Data/secrets are lazy disclosures under Effects; capture reach is Diagnostics; usage
is footer telemetry; Governance is a link to the Governance surface.

The console keeps these reads lazy. Opening Changes loads its ordinary session-evidence
mode; selecting the local Code state mode loads one bounded `code-changes` page, and
selecting Inspect on one row—or following an exact file identity from Effects—loads one
`code-change-file` response. A
`file_page.exact=false` value means the exact population is paged, not that collection
is partial. When `state=baseline_unavailable`, the console labels the population
current analyzed files and does not present those rows as session changes.

List responses include metadata and exact byte counts but do not inline retained
content. Bodies are fetched only through `section=body`, are limited to complete UTF-8
text already retained by collection policy, and remain subject to the existing retained
body ceiling.

The older request without `section` is retained temporarily for compatibility. It can
assemble the legacy all-sections report and may still be too large for a long session.
The installed console does not use that legacy form.
