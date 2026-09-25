# Release policy

This is an experimental source snapshot. No supported binary release or installation claim follows
from a successful local build. Source publication and binary distribution are separate.

A source snapshot must pass a clean build and the complete repository quality gate, have
reviewed provenance and private-data exposure, resolve documentation links, and describe
its limitations. Apache-2.0 and DCO contribution requirements apply. Maintainers choose
the release commit and tag after those checks; a tag alone is not a stability promise.

Before a binary or installation recommendation, record the exact OS, architecture, client
versions, install/consent flow, observed hooks, console evidence and recovery/uninstall
results. Publish checksums, dependency notices, build metadata and known limitations with
the artifact. Experimental APIs, schemas and package interfaces may change incompatibly.

For a faulty release, maintainers should identify affected versions, publish a warning and
replacement or recovery guidance, and withdraw download recommendations as needed. Keep
historical evidence available without presenting the affected artifact as safe. No automatic
upgrade, maintenance period or response deadline is promised.

The current local inspection archive predates the latest Codex safety fixes. It is not an
installation recommendation. See [current verification](refresh-status.md).
