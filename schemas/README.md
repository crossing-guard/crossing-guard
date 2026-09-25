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
- `memory.schema.json`: the shared dossier (ADR 0013). `scope` may be `repository` or
  `organization` only; a `user`-scoped record is invalid on the wire
- `handoff.schema.json`: human-edited markdown plus declared governance state; anchors
  are `sender-local`
- `tombstone.schema.json`: deletion propagation record
- `device-report.schema.json`: a bounded projection of `doctor --json`
- `bundle.schema.json`: signed governance and agent-configuration bundle; an unsigned
  bundle is invalid

Only `event` has an encoder today (`engine.EncodeWireEvent`); the others gain theirs with
the features that produce them.

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
  needs its fixtures updated and a line here. Changelog: 2026-09-17 — 1.0 defined.

## Fixtures

`fixtures/valid` must validate; `fixtures/invalid` must be rejected. Each invalid fixture
has an adjacent `.expected.txt`; when its first line is `error-contains: <text>`, the
conformance test also requires that text in the validator's error, so a fixture cannot be
rejected for the wrong reason. A fixture is matched to the schema whose base name is the
longest prefix of its file name.
