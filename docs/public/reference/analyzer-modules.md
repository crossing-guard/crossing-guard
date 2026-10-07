# Code analyzer modules

Crossing Guard code analyzers are executable modules selected outside a repository. The
core observes the exact Git source set, invokes each selected module once, validates its
facts, and writes accepted language-neutral units, edges, and coverage into the existing
understanding generation. A module never owns Git capture, SQLite, governance policy, or
the GUI.

The shipped Go analyzer remains a compatibility provider. Reference PHP and JavaScript
modules are separate native executables under `analyzers/php/` and
`analyzers/javascript/`; the main binary does not import their code or parser libraries.
Third parties can implement the documented NDJSON protocol without changing framework
source. The optional `analyzers/modulekit` Go helper is author-side framing only.

## Build and lifecycle

Build reference packages into a new empty output directory:

```sh
./scripts/build-analyzers.sh /path/to/output
```

Then inspect and explicitly select exact content-addressed packages:

```sh
crossing-guard analyzers install /path/to/output/php
crossing-guard analyzers list
crossing-guard analyzers inspect MODULE@sha256-v1:DIGEST
crossing-guard analyzers select --yes MODULE@sha256-v1:DIGEST
crossing-guard analyzers doctor
```

Selection authorizes a native executable to run with the local user's permissions. V1
does not sandbox filesystem or network access. Merely installing a package never selects
or executes it. Selection is user/data-directory scoped; repositories, file extensions,
`PATH`, and project configuration cannot activate executable modules. Restart the daemon
after selection changes so one immutable assembly serves scans and descriptor reads.

To reverse selection, deselect first. Exact unselected package removal is separately
confirmed:

```sh
crossing-guard analyzers deselect --yes MODULE
crossing-guard analyzers remove --yes MODULE@sha256-v1:DIGEST
```

## Package and stream contract

Every package contains `manifest.json` and one package-relative self-contained native
entrypoint. V1 rejects symlinks, special files, absolute/escaping paths, shebang scripts,
unknown fields, conflicting selected extensions, and package mutation.

The process reads strict newline-delimited JSON: one `scan_start`, zero or more exact
manifest `path` records with source hash/bytes/lines, then `scan_end`. It writes one
matching `analysis_start`, bounded `unit`, `edge`, `coverage`, and `unit_failure`
records, then one matching `analysis_end` with exact totals.

Facts remain provisional until clean process exit, complete protocol validation, and a
post-run package rehash. Any mismatch discards that module's facts. Accepted call
relations are `symbol_calls_symbol` and `file_references_symbol`; declaration ranges are
checked against core-observed source and core computes exact declaration source digests.

Coverage is per analyzer and portable family. `complete` means the offered set was fully
attempted with no error, unresolved, or ambiguous count. `partial`, `failed`, and
`unsupported` stay distinct. Dynamic behavior remains counted gaps, never zero.

The V1 structs and constants are in `codemap/module_protocol.go`. Output is mechanics,
not a quality verdict: analyzers do not label controllers/services, grade complexity,
declare duplicates, select thresholds, or make governance decisions.

The PHP reference module resolves exact same-namespace functions, same-class
`$this`/static calls, and reports dynamic dispatch as unresolved. The JavaScript module
resolves same-file lexical calls, same-class `this` calls, and exported named relative
imports (including aliases and extension/index resolution). Bare packages, non-exported
bindings, computed members, runtime loading, and other dynamic behavior remain explicit
unresolved coverage. Vue single-file components are unsupported in V1.
