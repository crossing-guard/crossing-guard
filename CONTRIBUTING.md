# Contributing

This repository holds the published source. GitHub web commits require sign-off. The
Source checks workflow runs the quality gate and checks that each pull request commit is
signed off by its author. Branch protection is not configured yet. A sign-off check does
not establish legal provenance.

Use focused branches and pull requests. Explain the user-visible problem, the affected
workflow, verification and limitations. Update documentation alongside behavior changes.
Never include credentials, natural user transcripts, private source or operational stores
as fixtures.

Contributions require a Signed-off-by line certifying the
[Developer Certificate of Origin 1.1](https://developercertificate.org/). Use your own
contribution identity and sign off only if you can make that certification. Contributions
are submitted under Apache-2.0; preserve required third-party notices and explain provenance.

The assembled repository's `bash scripts/check.sh` is the required quality gate. Missing
prerequisites or source files are failures. Exercise the affected journey through the
compiled CLI, real HTTP or browser, using isolated data. Record success and recovery.
A mock is not installed-product proof. Do not alter a running installation merely to test.

See [SECURITY.md](SECURITY.md) before submitting vulnerability details.
