# Building the source candidate

This is an experimental source snapshot. The code is available, but installation,
provider integration and release support are not verified for this candidate.

Use Go 1.26 or later, Node.js, Python 3, Bash, ripgrep, Perl, Git and a C compiler. The Go module
files pin the development analyzers. From the source root:

```sh
bash scripts/check.sh
mkdir -p bin
go build -mod=readonly -o bin/crossing-guard ./cmd/crossing-guard
./bin/crossing-guard --help
```

The current CLI prints usage and exits 2 for `--help`; this is existing behavior.
An unknown command also prints usage and exits 2. Building or requesting help does not
install the application. Do not run installation or service commands as part of this
build-only walkthrough.

The source candidate preserves legacy embedded configuration bytes for compatibility.
Their metadata and workflow-specific behavior are not recommendations for fresh users.
Named console configuration controls and the source-level native/generated provenance
review have been verified in the local candidate. The fresh installation/agent/console/
recovery/uninstall journey and public release remain open.
No universal agent-interruption, supported-platform or stable API promise follows from
successful compilation. The API reference is explicitly experimental and incomplete.

Historical September 13 verification: the assembled candidate passed the complete source quality
gate after correcting process diagnostic draining. Repeated synthetic-provider tests
and real loopback HTTP checks verified quota classification and attributed fallback.
These checks do not establish a natural installed-provider or console journey.

For the separately assembled local binary evaluation package, see
[evaluating the local bundle](evaluating-local-bundle.md). It includes all analyzer
components and attribution, but does not establish installed provider support.

September 25: the safety-fixed candidate passed the full quality gate. Eight HTTP cases
verified Codex launch arguments using a synthetic runtime; a disposable schema28 → 32
upgrade/export/recovery trial also passed. Neither establishes installed-client support.

September 27 CI repair: doctor fixtures now supply their own inert client executable,
and natural-session tests await asynchronous completion and capability outcomes. Exact
signal identity, counts, anchors and suppression reasons remain asserted; no production
code, timeouts or quality checks changed. Focused regressions pass ten race-instrumented
repetitions. The complete local quality gate passes, including root and analyzer race
tests, static checks and frontend checks. Hosted verification of the repair is pending.

The published source plus these test-only changes builds the application and both external
analyzers. A fresh, isolated local daemon passed compiled doctor/init-dry-run checks,
authenticated HTTP, browser navigation, invalid-token refusal and browser recovery.
The console contained no sessions and no provider credentials or hooks were attached.
This is local source inspection evidence; the installed-client release gates remain open.

October 5 refresh: the tree is now a direct export of upstream `5607e508`. The complete
local gate passes on it, and the gate allows each test binary 31 minutes because the
daemon race suite alone runs for several minutes. See [the refresh status](refresh-status.md)
for the upgrade trial and the isolated daemon and browser checks.
