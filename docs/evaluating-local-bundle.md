# Evaluating the local macOS bundle

**Historical artifact:** the existing local archive predates the integrated Codex safety
fixes. Do not use it to launch coding agents or treat it as the current source candidate.

This is an unpublished local evaluation artifact for macOS on Apple silicon. It is not
a public release, a notarized download, or evidence of supported provider integration.
The package records its exact source inventory, toolchain, executable hashes and linked
system libraries in `BUILD-INFO.json` and `SHA256SUMS`. It was built and exercised on
macOS 15.7.7. The native analyzer binaries record a minimum macOS version of 15.0; other
OS versions have not been tested.

## Contents and verification

The package includes the main `bin/crossing-guard` executable with built-in Go analysis,
plus PHP and JavaScript/TypeScript analyzers under `analyzers/<name>/bin/`. Their manifests
retain relative entrypoints. Merely extracting the package does not register analyzers,
install hooks or start a background service.

From the extracted package root, verify its files:

```sh
shasum -a 256 -c SHA256SUMS
./bin/crossing-guard --help
```

The current help command prints usage and exits 2. Unknown commands also exit 2.
Keep `LICENSE`, `THIRD_PARTY_NOTICES.md` and the complete `third_party/` directory with
the binaries. The latter contains dependency and Go toolchain notices. macOS system
libraries listed in build metadata are not redistributed in the archive.

## Installation preview

From a durable location, `./bin/crossing-guard init --dry-run` reports the planned runtime
attachments and current rulebook without installing hooks or services. Inspect that
preview before any real installation. A temporary-folder warning is expected when
trying the bundle from an extracted evaluation directory.

A fresh home reports an empty unselected rulebook. Runtime discovery can still find
globally installed applications. Claude presence requires an executable found by the
same lookup used by chat; settings alone do not establish presence. If its executable
is absent, the preview says "client not detected" and shows surviving configuration.
Other runtimes still have older discovery behavior; a complete passive inventory and
version evidence are not implemented. Detection does not prove that hooks will fire.

The extracted bundle passed local checks for analyzer installation, selection, scanning
Go/PHP/JavaScript/TypeScript/TSX fixtures, refusal of unconfirmed selection and removal of
selected packages, and deselection/removal. Tampered or missing bundle files and stale
analyzer source hashes were rejected. Executables built with ordinary 0755 permissions
install into private 0700 storage while retaining existing installed package identities.
These checks use synthetic source and do not establish a coding-agent integration.

Do not use full `init` or `uninstall` merely to test a temporary home on a machine with an
existing installation: macOS background services share a per-user service name. A separate
home directory alone does not isolate that service. The full fresh-install trial needs a
separate macOS user/VM or a deliberate, reviewed change to the existing installation.

## Remaining installed trial

The release trial must start from the exact packaged files in a durable location, use a
real supported coding-agent client, and verify consent, observed hook events, selected
configuration, console evidence, backup/export and uninstall/recovery. Analyzer protocol
checks on synthetic source do not replace this journey. No install or download link should
claim that journey has passed until its results are recorded for the exact artifact.
