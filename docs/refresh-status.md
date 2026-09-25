# Local integration candidate — September 25, 2026

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
