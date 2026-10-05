# Portable Schemas

These JSON Schemas are the portable record model. They use JSON Schema 2020-12 and are
versioned independently from application releases. As of 2026-09-17 they are
**executable**: `schemas` is a Go package that embeds them and validates documents, and
`go test ./schemas/` is the conformance command this README asked for from the start.

## Wire contract — 1.0

The records that cross between a device and the team server (experimental team protocol). Each was
reconciled to what the client can actually produce; the 2026-07-10 drafts described
records the product never built.

- `common.schema.json`: shared ids, timestamps, scopes, provenance, sensitivity, plus
  `session` (one `ses_…` id per device + runtime + native id), `target` (repo-relative
  path, or `outside_repository` — an absolute path is invalid by pattern), and `chain`
- `event.schema.json`: one governed action — typed payload (verb, tool, target, frozen
  tags, tags digest, decision, reason, origin) and its per-session chain entry
- `memory.schema.json` (**1.1**): the shared dossier (ADR 0013), one revision per record.
  `scope` may be `repository` or `organization` only; a `user`-scoped record is invalid on
  the wire. `content_hash` is the wire hash of the redacted record; `base_content_hash`
  (1.1) is the wire hash of the revision the edit was made against, absent on a first
  appearance
- `handoff.schema.json` (**1.1**): a handoff between members — an immutable document the
  sender's device builds: `title`, `body_markdown` (the person's edited text), `remaining`
  (the sender's list), the sender's `session` (wire id, runtime, native id, and the
  catalog and resume ids), a remote-derived `repository_id` or null, a typed `recipient`
  (a server user id), `agents` (references, never bodies), declared governance state, and
  an optional `conversation` excerpt. `content_hash` is the wire hash of the document
  after the device's path and secret checks (`teamwire.HandoffWireHash`); anchors are
  `sender-local`. Push kind `handoff`
- `handoff-receipt.schema.json` (**1.0**): one transition of one handoff reported by one
  device — `received`, `started`, `opened`, `declined`, `closed`, `withdrawn`. Its id is
  deterministic over (handoff, transition, device, ticket, native session id), so a retry
  is byte-identical. `started` and `opened` carry the ticket and the recipient's session.
  Push kind `handoff_receipt`
- `tombstone.schema.json`: deletion propagation record. `record_type` is `memory` (the
  record's global id) or `session_content` (a wire session id; `required_projection_cleanup`
  `["cache"]`)
- `device-report.schema.json`: a bounded projection of `doctor --json`
- `bundle.schema.json` (**1.1**; a 1.0 document is still accepted): signed governance and
  agent-configuration bundle; an unsigned bundle is invalid. 1.1 adds the caps the schema
  can state — at most 8 `documents`, a `body` of at most 262,144 characters; the two it
  cannot (at most one `rulebook`; the body cap counted in bytes) are checked by
  `teamwire.CheckBundleCaps` on the server and on the device. Document kinds are
  `rulebook`, `profile` (the exact `PROFILE.md` bytes) and `detectors` (carried, not
  applied by this version)

`event` is encoded by `engine.EncodeWireEvent`; a handoff document is built by the daemon's
send (`internal/daemon/team_handoff_send.go`) and a receipt by the store
(`store.EnqueueHandoffReceiptTx`), each frozen in the outbox row that carries it.

## Draft schemas — 0.1

Not on the wire: `request-path`, `policy-decision`, `adapter-manifest`,
`model-data-handling`, `rule`, and `hook-compile-map`. These retain historical draft
namespace identifiers for compatibility; the identifiers do not name a supported hosted
service. They reference `common` by file name and are validated by the same engine.
Private design references and unshipped seed documents are not part of this source snapshot.
All schemas remain experimental public interfaces; a version label is not a compatibility
guarantee for the entire framework.

## The engine is fail-closed

`schemas/validate.go` implements the subset of 2020-12 this repository uses — `type`,
`properties`, `required`, `additionalProperties`, `items`, `minItems`, `maxItems`,
`uniqueItems`, `enum`, `const`, `pattern` (RE2), `minLength`, `maxLength`, `minimum`,
`maximum`, `minProperties`, `format` (`date-time`, `date`, `uri`), `oneOf`, `allOf`,
`if`/`then`, `$ref`, `$defs`. **Any other keyword is a load error**, so a schema can never
depend on semantics the engine would silently skip; to use a new keyword, implement it
there first. It has no dependencies. The schemas stay standard, so a full engine can
replace it later.

## Compatibility rules

- Unknown fields are rejected, to expose accidental format drift.
- Additive optional fields may be introduced in a new schema version.
- Required-field changes, enum removals, or semantic changes require a new version.
- Consumers must reject unsupported major schema versions visibly.
- Native vendor payloads are references or optional opaque attachments, not portable
  canonical fields.
- **Immutability begins at the first server deployment or public release, whichever
  comes first.** Until then nothing outside this repository consumes 1.0, and a change
  needs its fixtures updated and a line here. Changelog: 2026-09-17 — 1.0 defined. 2026-09-17 —
  `device-report`: `runtimes[].canary {observed_at, rule_active}` added and required (the fleet's
  third rung had no field); free-text `firing.evidence` removed; `maxItems` on both arrays;
  `runtimes[].name` and `daemon.version` constrained by pattern (team plan §5.15). 2026-09-29 —
  team item 4 (additive; the demo server accepts none of these until it runs item 4):
  `session`, `session-checkpoint-fact`, and `session-content` defined at 1.0 (the push
  kinds of the item 4 plan §4.3; content chunks capped at 256 KiB); `sandbox-hook` added to
  `provenance.kind` (§7.1's one-enum reservation — posture B's observations). Recorded
  late: `event.payload.rule` (item 2a) and `event.payload.layer` (item 3a). 2026-10-02 —
  team item 5: `memory` → **1.1** (`base_content_hash` added, optional; a server at 1.0
  refuses 1.1 by `schema_version`); tombstone fixtures added, valid (`session_content`)
  and invalid (no cleanup, no prior hash); `memory-bad-base` invalid fixture.

## Fixtures

`fixtures/valid` must validate; `fixtures/invalid` must be rejected. Each invalid fixture
has an adjacent `.expected.txt`; when its first line is `error-contains: <text>`, the
conformance test also requires that text in the validator's error, so a fixture cannot be
rejected for the wrong reason. A fixture is matched to the schema whose base name is the
longest prefix of its file name.
