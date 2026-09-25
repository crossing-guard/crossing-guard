# Third-party notices

This source candidate preserves license, copyright, patent and notice texts from the
exact dependency versions resolved by its three Go module graphs. The list includes
transitive, test and development-tool dependencies; it is broader than any one executable.
Dependency source is not vendored here. [Module checksums and file hashes](third_party/modules.json)
identify the captured inputs. Original terms and copyright notices are preserved unchanged.

The project Apache-2.0 license does not replace these dependency terms. The translated
SQLite module explicitly retains the original licenses of translated code; its MIT-0
file alone is not the license of every generated component. The [source provenance
review](third_party/provenance.md) records the SQLite inputs, embedded notices and native
parser sources. Final binary contents still require release review.
This inventory does not claim toolchain/system-library or binary SBOM completeness.

| Module version | Resolved graphs | Preserved upstream texts |
| --- | --- | --- |
| `github.com/BurntSushi/toml@v1.4.1-0.20240526193622-a339e1f7089c` | root, php, javascript | [COPYING](third_party/licenses/github.com/BurntSushi/toml@v1.4.1-0.20240526193622-a339e1f7089c/COPYING), [cmd/toml-test-decoder/COPYING](third_party/licenses/github.com/BurntSushi/toml@v1.4.1-0.20240526193622-a339e1f7089c/cmd/toml-test-decoder/COPYING), [cmd/toml-test-encoder/COPYING](third_party/licenses/github.com/BurntSushi/toml@v1.4.1-0.20240526193622-a339e1f7089c/cmd/toml-test-encoder/COPYING), [cmd/tomlv/COPYING](third_party/licenses/github.com/BurntSushi/toml@v1.4.1-0.20240526193622-a339e1f7089c/cmd/tomlv/COPYING) |
| `github.com/davecgh/go-spew@v1.1.1` | php, javascript | [LICENSE](third_party/licenses/github.com/davecgh/go-spew@v1.1.1/LICENSE) |
| `github.com/dchest/siphash@v1.2.3` | root | [LICENSE](third_party/licenses/github.com/dchest/siphash@v1.2.3/LICENSE) |
| `github.com/google/go-cmp@v0.6.0` | root | [LICENSE](third_party/licenses/github.com/google/go-cmp@v0.6.0/LICENSE) |
| `github.com/google/uuid@v1.6.0` | root | [LICENSE](third_party/licenses/github.com/google/uuid@v1.6.0/LICENSE) |
| `github.com/kisielk/errcheck@v1.9.0` | root, php, javascript | [LICENSE](third_party/licenses/github.com/kisielk/errcheck@v1.9.0/LICENSE) |
| `github.com/mattn/go-pointer@v0.0.1` | php, javascript | [LICENSE](third_party/licenses/github.com/mattn/go-pointer@v0.0.1/LICENSE) |
| `github.com/ncruces/go-sqlite3-wasm/v3@v3.2.35303` | root, php, javascript | [LICENSE](third_party/licenses/github.com/ncruces/go-sqlite3-wasm/v3@v3.2.35303/LICENSE), [parser/LICENSE](third_party/licenses/github.com/ncruces/go-sqlite3-wasm/v3@v3.2.35303/parser/LICENSE) |
| `github.com/ncruces/go-sqlite3@v0.35.2` | root, php, javascript | [LICENSE](third_party/licenses/github.com/ncruces/go-sqlite3@v0.35.2/LICENSE) |
| `github.com/ncruces/julianday@v1.0.0` | root, php, javascript | [LICENSE](third_party/licenses/github.com/ncruces/julianday@v1.0.0/LICENSE) |
| `github.com/ncruces/sort@v1.0.0` | root | [LICENSE](third_party/licenses/github.com/ncruces/sort@v1.0.0/LICENSE) |
| `github.com/ncruces/wasm2go@v0.4.11` | root | [LICENSE](third_party/licenses/github.com/ncruces/wasm2go@v0.4.11/LICENSE), [STB-SPRINTF-NOTICE.txt](third_party/licenses/github.com/ncruces/wasm2go@v0.4.11/libc-gen/STB-SPRINTF-NOTICE.txt), [DOUG-LEA-MALLOC-NOTICE.txt](third_party/licenses/github.com/ncruces/wasm2go@v0.4.11/libc-gen/DOUG-LEA-MALLOC-NOTICE.txt) |
| `github.com/ncruces/wbt@v1.0.0` | root | [LICENSE](third_party/licenses/github.com/ncruces/wbt@v1.0.0/LICENSE) |
| `github.com/pmezard/go-difflib@v1.0.0` | php, javascript | [LICENSE](third_party/licenses/github.com/pmezard/go-difflib@v1.0.0/LICENSE) |
| `github.com/psanford/httpreadat@v0.1.0` | root | [LICENSE](third_party/licenses/github.com/psanford/httpreadat@v0.1.0/LICENSE) |
| `github.com/stretchr/testify@v1.10.0` | php, javascript | [LICENSE](third_party/licenses/github.com/stretchr/testify@v1.10.0/LICENSE) |
| `github.com/tree-sitter/go-tree-sitter@v0.25.0` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/go-tree-sitter@v0.25.0/LICENSE), [src/unicode/LICENSE](third_party/licenses/github.com/tree-sitter/go-tree-sitter@v0.25.0/src/unicode/LICENSE), [ENDIAN-NOTICE.txt](third_party/licenses/github.com/tree-sitter/go-tree-sitter@v0.25.0/src/portable/ENDIAN-NOTICE.txt) |
| `github.com/tree-sitter/tree-sitter-c@v0.23.4` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-c@v0.23.4/LICENSE) |
| `github.com/tree-sitter/tree-sitter-cpp@v0.23.4` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-cpp@v0.23.4/LICENSE) |
| `github.com/tree-sitter/tree-sitter-embedded-template@v0.23.2` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-embedded-template@v0.23.2/LICENSE) |
| `github.com/tree-sitter/tree-sitter-go@v0.23.4` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-go@v0.23.4/LICENSE) |
| `github.com/tree-sitter/tree-sitter-html@v0.23.2` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-html@v0.23.2/LICENSE) |
| `github.com/tree-sitter/tree-sitter-java@v0.23.5` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-java@v0.23.5/LICENSE) |
| `github.com/tree-sitter/tree-sitter-javascript@v0.23.1` | php | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-javascript@v0.23.1/LICENSE) |
| `github.com/tree-sitter/tree-sitter-javascript@v0.25.0` | javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-javascript@v0.25.0/LICENSE) |
| `github.com/tree-sitter/tree-sitter-json@v0.24.8` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-json@v0.24.8/LICENSE) |
| `github.com/tree-sitter/tree-sitter-php@v0.23.11` | javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-php@v0.23.11/LICENSE) |
| `github.com/tree-sitter/tree-sitter-php@v0.24.2` | php | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-php@v0.24.2/LICENSE) |
| `github.com/tree-sitter/tree-sitter-python@v0.23.6` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-python@v0.23.6/LICENSE) |
| `github.com/tree-sitter/tree-sitter-ruby@v0.23.1` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-ruby@v0.23.1/LICENSE) |
| `github.com/tree-sitter/tree-sitter-rust@v0.23.2` | php, javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-rust@v0.23.2/LICENSE) |
| `github.com/tree-sitter/tree-sitter-typescript@v0.23.2` | javascript | [LICENSE](third_party/licenses/github.com/tree-sitter/tree-sitter-typescript@v0.23.2/LICENSE) |
| `github.com/yuin/goldmark@v1.4.13` | root | [LICENSE](third_party/licenses/github.com/yuin/goldmark@v1.4.13/LICENSE) |
| `golang.org/x/crypto@v0.53.0` | root | [LICENSE](third_party/licenses/golang.org/x/crypto@v0.53.0/LICENSE), [PATENTS](third_party/licenses/golang.org/x/crypto@v0.53.0/PATENTS) |
| `golang.org/x/exp/typeparams@v0.0.0-20231108232855-2478ac86f678` | root, php, javascript | [LICENSE](third_party/licenses/golang.org/x/exp/typeparams@v0.0.0-20231108232855-2478ac86f678/LICENSE) |
| `golang.org/x/exp@v0.0.0-20231110203233-9a3e6036ecaa` | root | [LICENSE](third_party/licenses/golang.org/x/exp@v0.0.0-20231110203233-9a3e6036ecaa/LICENSE), [PATENTS](third_party/licenses/golang.org/x/exp@v0.0.0-20231110203233-9a3e6036ecaa/PATENTS) |
| `golang.org/x/image@v0.45.0` | root, php, javascript | [LICENSE](third_party/licenses/golang.org/x/image@v0.45.0/LICENSE), [PATENTS](third_party/licenses/golang.org/x/image@v0.45.0/PATENTS) |
| `golang.org/x/mod@v0.36.0` | root, php, javascript | [LICENSE](third_party/licenses/golang.org/x/mod@v0.36.0/LICENSE), [PATENTS](third_party/licenses/golang.org/x/mod@v0.36.0/PATENTS) |
| `golang.org/x/net@v0.54.0` | root | [LICENSE](third_party/licenses/golang.org/x/net@v0.54.0/LICENSE), [PATENTS](third_party/licenses/golang.org/x/net@v0.54.0/PATENTS) |
| `golang.org/x/sync@v0.21.0` | root, php, javascript | [LICENSE](third_party/licenses/golang.org/x/sync@v0.21.0/LICENSE), [PATENTS](third_party/licenses/golang.org/x/sync@v0.21.0/PATENTS) |
| `golang.org/x/sys@v0.47.0` | root, php, javascript | [LICENSE](third_party/licenses/golang.org/x/sys@v0.47.0/LICENSE), [PATENTS](third_party/licenses/golang.org/x/sys@v0.47.0/PATENTS) |
| `golang.org/x/telemetry@v0.0.0-20260508192327-42602be52be6` | root | [LICENSE](third_party/licenses/golang.org/x/telemetry@v0.0.0-20260508192327-42602be52be6/LICENSE), [PATENTS](third_party/licenses/golang.org/x/telemetry@v0.0.0-20260508192327-42602be52be6/PATENTS) |
| `golang.org/x/text@v0.41.0` | root | [LICENSE](third_party/licenses/golang.org/x/text@v0.41.0/LICENSE), [PATENTS](third_party/licenses/golang.org/x/text@v0.41.0/PATENTS) |
| `golang.org/x/tools/go/expect@v0.1.1-deprecated` | root, php, javascript | [LICENSE](third_party/licenses/golang.org/x/tools/go/expect@v0.1.1-deprecated/LICENSE) |
| `golang.org/x/tools@v0.45.0` | root, php, javascript | [LICENSE](third_party/licenses/golang.org/x/tools@v0.45.0/LICENSE), [PATENTS](third_party/licenses/golang.org/x/tools@v0.45.0/PATENTS) |
| `gopkg.in/check.v1@v0.0.0-20161208181325-20d25e280405` | root | [LICENSE](third_party/licenses/gopkg.in/check.v1@v0.0.0-20161208181325-20d25e280405/LICENSE) |
| `gopkg.in/yaml.v3@v3.0.1` | root, php, javascript | [LICENSE](third_party/licenses/gopkg.in/yaml.v3@v3.0.1/LICENSE), [NOTICE](third_party/licenses/gopkg.in/yaml.v3@v3.0.1/NOTICE) |
| `honnef.co/go/tools@v0.6.1` | root, php, javascript | [LICENSE](third_party/licenses/honnef.co/go/tools@v0.6.1/LICENSE), [LICENSE-THIRD-PARTY](third_party/licenses/honnef.co/go/tools@v0.6.1/LICENSE-THIRD-PARTY), [go/gcsizes/LICENSE](third_party/licenses/honnef.co/go/tools@v0.6.1/go/gcsizes/LICENSE), [go/ir/LICENSE](third_party/licenses/honnef.co/go/tools@v0.6.1/go/ir/LICENSE) |
| `lukechampine.com/adiantum@v1.1.1` | root | [LICENSE](third_party/licenses/lukechampine.com/adiantum@v1.1.1/LICENSE), [internal/xchacha/LICENSE](third_party/licenses/lukechampine.com/adiantum@v1.1.1/internal/xchacha/LICENSE) |
