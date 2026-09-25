# Building the analysis components

This source candidate contains the shared code-analysis packages, built-in Go analyzer,
and PHP and JavaScript/TypeScript analyzers alongside the main application.
It is unpublished and has no verified application installation or downloadable release yet.

Use Go 1.26 or later and a working C compiler for the tree-sitter analyzers. From the
repository root, check the shared code with:

```sh
go test -mod=readonly -race ./codemap/... ./analyzers/modulekit
```

From `analyzers/php` or `analyzers/javascript`, run:

```sh
go test -mod=readonly -race ./...
go build -mod=readonly -o bin/analyzer .
```

The executable speaks a line-delimited JSON protocol on standard input/output; it is
intended to be called by the analyzer host, not used as a standalone interactive CLI.
Its manifest describes its eventual installed entrypoint; the build above is a local
verification output. Running it with empty input reports an incomplete request and exits
nonzero. No registration with the main application occurs here.

Verification on the assembly machine covers compiled protocol processing of synthetic
source, malformed-request rejection and changed-source failure reporting. It does not
establish installed application, provider integration or cross-platform support.

The full `scripts/check.sh` checks the application, console and analyzer modules together.
Do not treat the component tests as the complete product release gate.
See [third-party notices](../THIRD_PARTY_NOTICES.md) for pinned dependency attribution.
