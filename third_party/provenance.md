# Native and generated source provenance

Reviewed September 17, 2026 against the pinned dependencies in this candidate.
[Machine-readable evidence](provenance.json) records source hashes, input URLs and
current-host import graphs. [The broader module inventory](modules.json) includes
49 module versions across development, test and build graphs; it is not a list of
components linked into every executable.

## SQLite translation

The main application imports go-sqlite3 v0.35.2 and go-sqlite3-wasm/v3 v3.2.35303.
The translation module's README says original copyright and license terms continue to
apply to translated code; its MIT-0 license covers the remaining module work.

The pinned build/download.sh identifies SQLite 3.53.3 and an archive SHA3-256 of
`98f2b3f3c11be6a03ea32346937b032c2472ebbd7a716bed36ca2f5693e7ce8b`.
The retrieved archive matches that checksum. Build/main.c includes the amalgamation,
decimal, ieee754, regexp, series and uint extensions, module bindings and generated libc.
The build also includes mptest and speedtest1 wrappers. The seven extension/test inputs
were retrieved from the version-3.53.3 tag; their hashes are recorded separately because
the module download script does not checksum those downloads.

SQLite describes its deliverable code as public domain, distinguishing it from some
build tooling. [SQLite copyright statement](https://sqlite.org/copyright.html).
The reviewed core, five extension and mptest files contain copyright disclaimers;
exact excerpts are preserved under `upstream/sqlite-3.53.3/` and indexed in provenance.json.
The speedtest1 input is recorded with its source hash and the upstream project statement;
no separate inline license was inferred from its descriptive header.

The module pins wasm2go/libc-gen v0.4.11. Its allocator wrapper includes Doug Lea's
malloc source, whose actual header states MIT-0 and a September 2023 relicensing;
that header takes precedence over the generator README's older public-domain shorthand.
The [allocator notice](licenses/github.com/ncruces/wasm2go@v0.4.11/libc-gen/DOUG-LEA-MALLOC-NOTICE.txt)
is retained with its source-file hash and line range.

The generator also bundles STB sprintf with MIT and public-domain alternatives.
[Both alternatives are retained](licenses/github.com/ncruces/wasm2go@v0.4.11/libc-gen/STB-SPRINTF-NOTICE.txt).
This is generator attribution, not a linkage claim: the inspected stdio.c uses SQLite's
formatter when SQLITE3_H is defined, and SQLite's main.c includes the amalgamation first.

Optional translation subpackages such as parser, fts5, rtree, spellfix and vec1 are not
imported by the main application in the reviewed default graph. Their presence in the
module does not mean the application uses every optional extension.

## Native analyzers

The default darwin/arm64 import graphs resolve these external runtime components:

| Executable | External components |
| --- | --- |
| Main application | go-sqlite3 v0.35.2, go-sqlite3-wasm/v3 v3.2.35303, julianday v1.0.0, x/image v0.45.0, x/sys v0.47.0, yaml.v3 v3.0.1; no cgo packages in this graph |
| PHP analyzer | go-pointer v0.0.1, go-tree-sitter v0.25.0, tree-sitter-php v0.24.2; Go runtime/cgo |
| JavaScript/TypeScript analyzer | go-pointer v0.0.1, go-tree-sitter v0.25.0, tree-sitter-javascript v0.25.0, tree-sitter-typescript v0.23.2; Go runtime/cgo |

The analyzers also use this project's shared codemap/modulekit code. Other grammar
modules in go.mod's wider graph are not imported by these default executable builds.
PHP's binding package includes both PHP grammar variants; TypeScript includes TypeScript
and TSX. The Go bindings include the generated C parsers and scanners directly.

Module MIT notices are preserved. Tree-sitter's nested Unicode/ICU notice is preserved
separately, along with the inline public-domain statement in its portable endian header.
Source hashes include native C/header files for the selected runtime and grammars, so a
later dependency change cannot silently reuse this review.

## Evidence limits and release use

This is a source/build-input trace, not an independent reproduction of upstream generated
Go files. Upstream download/generator/build scripts were inspected, not executed. The
module-cache integrity check passed; the existing translated outputs remain tied to
pinned Go module checksums. Module pins and application behavior were not changed.

For each binary release, record the actual executable's module/build metadata and
platform libraries and supply its notices, including the Go toolchain's applicable
notices. Do not label these source graphs a complete cross-platform binary SBOM. The
current main audit binary's module metadata agrees with the six external modules above.
Project asset authorship and the installed-product trial are separate release checks.
