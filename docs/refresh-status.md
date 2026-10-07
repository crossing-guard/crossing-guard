# Source refresh — October 5, 2026

This repository now holds a filtered export of upstream revision
`5607e50889b7408560b264a98070c6f3eea907a8`. It replaces the September 25 snapshot
(`a65d4055`, published as `c26a2cc8`). It is an experimental public source snapshot,
not a supported installed alpha or binary download.

## What changed in how this tree is made

The September snapshot was assembled by hand and had drifted from upstream: several
fixes existed only here. Those fixes were ported upstream first, so this tree is now a
direct export of the upstream source folders plus the publication documents in this
repository. Sample data in tests and help text was replaced upstream with neutral
samples before the export.

## What is new since September

- Database schema 46 (was 32).
- Recall tools over a local MCP server (`crossing-guard mcp`, `init --recall`).
- A team server client: `crossing-guard link`, a sync outbox, and memory by scope. A
  linked device sends records to the team server its user names. The server is a
  separate project and is not in this repository. An unlinked device sends nothing to
  a team server.
- Usage history from local session files, session views and boards, model routes,
  provider-outage handling for managed agents, an Antigravity transcript reader and
  Antigravity CLI chat through the shared provider contract.
- The safety-starter rulebook has six rules (adds `team-link-change`).
- The agent executable resolver (`ResolveRuntimeBinary`) has one home, in
  `internal/guardcli`. Other lookups of unrelated tools are unchanged.

## Verified for this refresh

- **Local quality gate: PASS**, October 5, on the export of `2c118232`. The pin then
  moved twice, to `5607e508`. The first move changed only test fixture values and
  documents. The second added Antigravity CLI chat and a session-presence fix (41
  files outside design documents). Every lint stage and the tests of the changed
  packages were rerun and pass; the hosted run covers the whole gate again. The local run covered: formatting, vet,
  errcheck, staticcheck, browser syntax and unit suites, the shape, module-graph,
  API-contract, vendor and name lints, and race tests across the main module and both
  analyzer modules.
- **Build:** the application and both analyzers build with `-mod=readonly`.
- **Upgrade trial:** a store created and seeded by the published September build
  (schema 32: one rulebook selection, four observed tool calls, one session state and
  checkpoint) was opened by this build. It migrated to schema 46 with every seeded row
  kept, `export` wrote a schema-46 snapshot, and the September build then refused the
  upgraded store instead of opening it. The pre-upgrade copy stayed readable at 32.
- **Scratch daemon:** with a fresh home, `--no-hook-install` and an unused loopback
  port: `doctor`, `init --dry-run`, `check`, `coverage`; HTTP 200 with the token and 401
  without it or with a wrong one; in a browser, Sessions, Governance (showing the
  seeded session and its four events), the not-connected screen for a wrong key, and
  recovery with the right one.

These are isolated checks on disposable data. They do not establish a natural
installed-client journey.

## Release gates still open

- A natural installed coding-agent journey (install, recovery, uninstall) on a clean
  macOS account, on the exact release artifact.
- Hosted verification of this refresh.
- A secret scan with a dedicated scanner; none was run for this refresh. A scan for
  private names, business sample data and captured session ids was run and is clean. GitHub push
  protection remains enabled.
- Executable notice closure and package metadata for a distribution archive.
- Draft schemas remain drafts; inclusion does not make one a wire contract.
- `crossing-guard init` prints "nothing leaves this machine" beside the data
  directory. That is true of an unlinked device and is not yet reworded for a linked one.

No provider account, installed hook or live service was changed by this refresh.

---

# Earlier record

## Local integration candidate — September 25, 2026

This separate source snapshot integrates upstream revision
`a65d4055969c19d6831e091f96527a5c89e2b1c7` with the reviewed local release fixes.
This is an experimental public source snapshot, not a supported installed alpha or binary download.

The source includes the newer helper-session, memory, event-chain, session-organization
and OpenCode changes. Local detector selection belongs to internal/detectorselection;
portable rule documents belong to ruledoc. Existing cohort defaults, rollback, analyzer
permissions and shared executable resolution are retained. Database schema is 32.

Build checks are recorded separately; successful isolated fixtures do not establish
provider integration or migration safety for an existing installation. Do not replace an
installed service or open retained user data as a test of this snapshot.

Release gates still open:

- Verify the ported Codex sandbox/argument fixes against an actual installed client;
  merged upstream PRs 55 and 57 are now included in this local candidate.
- Review newly admitted schema/data/docs for public naming and provenance. Draft schema
  identifiers are retained unchanged; inclusion does not promote every schema to a wire
  contract or certify source publication clearance.
- Refresh final executable notice closure and package metadata; local compiled outputs
  are not a signed/notarized distribution archive.
- Verify a natural installed coding-agent journey and recovery on its exact release artifact.
  A disposable schema28 → 32 task-data upgrade/export/recovery trial has passed.
- Complete hosted verification and link an installed release only after its journey passes.

No provider account, installed hooks or live service is changed by this assembly.

Local verification on September 25: full quality gate PASS (root and analyzer race
tests, static checks, frontend checks and existing lint ceilings). The main executable
and all analyzer executables build. Thirty-four compiled CLI fixture cases and scratch
HTTP/Chrome success/refusal/recovery journeys pass. Independent source assembly matches
all 889 files. These results do not close the installed-user release gates above.

September 25 safety update: ported merged PR55/57 from cb9a5c0, retaining the shared
executable resolver and candidate release fixes. Focused Codex tests and eight real HTTP
requests using a synthetic runtime and the full quality gate pass. The
earlier local inspection archive predates this port and is not the current candidate.

The public GitHub repository holds this experimental source snapshot. Web commit sign-off
and private vulnerability reporting are enabled. See [release policy](release-policy.md)
and [data/recovery scope](data-and-recovery.md).

### September 27 public CI repair

The first hosted source check for published revision `c26a2cc8` failed four doctor tests
and two natural-session tests. The doctor fixture depended on a developer-installed
client; the session tests assumed immediate completion of asynchronous work. This repair
supplies an inert fixture executable and waits through the existing bounded helpers,
with held-child fixtures making the pending-signal path deterministic. The first full
local gate then caught another early assertion, which now waits for the recorded
attended-session capability outcome. Production source, timeout bounds and gate checks
are unchanged. All seven affected tests pass ten focused race repetitions; the complete
local gate passes, including root and analyzer race tests, static checks and frontend
checks. Hosted verification of the correction remains pending.

All three executables build. Compiled CLI, real loopback HTTP and browser success,
refusal and recovery were exercised with fresh data and no provider credentials or
installed hooks. The browser correctly shows empty integration state. No supported
binary release, natural installed-client proof or migration claim follows from this run;
the release gates above remain open.
