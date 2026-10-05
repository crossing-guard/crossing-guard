# Record and inspect change evidence

Crossing Guard can attach a portable, source-linked change envelope to a session. The
envelope is evidence, not policy configuration: it records what was claimed, what Git
observed, what governed actions touched, and what a verification process returned. It
does not decide which files are required, whether a change is risky, or whether a test
is sufficient.

## Evidence classes

| mark | fact | class |
| --- | --- | --- |
| `P` | path named in the latest declaration | claimed |
| `T` | file target present in the governed event log | observed action fact |
| `Δ` | path reported by the latest bounded Git snapshot | observed repository fact |
| `C` | path named in the latest implementation claim | claimed |

These sets are independent. `P` does not prove a file changed; `T` does not prove Git
changed it; `C` does not promote a claim to observation. Added/omitted scope is available
only when both a declaration and a complete Git snapshot exist.

## Record a journey

Use one stable session ID and the explicit repository root for every command:

```bash
crossing-guard change declare \
  --session SESSION_ID --repo /path/to/repo --source-file /path/to/plan.md \
  --runtime codex --title "Short factual title" --intent "Claimed intent" \
  --path path/from/repo/root --symbol path/from/repo/root::OptionalSymbol

crossing-guard change snapshot \
  --session SESSION_ID --repo /path/to/repo --base HEAD

crossing-guard change claim \
  --session SESSION_ID --repo /path/to/repo --source-file /path/to/implementation-note.md \
  --path path/from/repo/root

crossing-guard change verify-run \
  --session SESSION_ID --repo /path/to/repo --cwd /path/to/repo \
  --timeout 2m --boundary focused --name "Focused repository checks" \
  -- go test ./path/to/package

crossing-guard change show --session SESSION_ID --json
```

`declare`, `claim`, and `verify-claim` require a regular source file. Crossing Guard
stores a SHA-256 digest and a repository-relative reference, or a digest of an external
path plus its basename. It never stores the source body. Symlinks, unreadable/mutating
sources, paths that escape the repository, and sources over 16 MiB are refused.

`snapshot` resolves `--base` to an immutable commit, observes committed/index/worktree/
untracked layers twice, and refuses unstable or oversized output. New records use a
`git-tree-v2-sha256:` identity that also covers the exact tracked/non-ignored-untracked
manifest and bounded untracked content. Ignored files are not included and symlink targets
are not followed. Diff and source bodies are discarded; only typed file facts, the source
identity, and content digests remain.

`verify-run` executes the exact argv without a shell, requires a repository-contained
working directory and timeout, streams output to your terminal, and stores no output,
environment values, or raw argv. The executable identity, exit/signal/timeout, and times
are observed. The human-readable check name and boundary remain claimed metadata; exit
zero does not prove that label was adequate.

## Use a non-default data root

Place `--data` immediately after `change`, and start the daemon with the same root:

```bash
crossing-guard change --data /path/to/data declare ...
crossing-guard change --data /path/to/data show --session SESSION_ID --json
crossing-guard serve --data /path/to/data
```

Every mutation and `show` prints the resolved store path. This prevents an unrelated
daemon from being mistaken for the store that received the evidence.

## Inspect large envelopes

The CLI and session API expose the same projection and paging fields. `show --json`
accepts:

```text
--repository-offset N --repository-limit N
--change-offset N --change-limit N
--observed-offset N --observed-limit N --observed-scope RELATION
--session-root DIR
--added-offset N --omitted-offset N --verification-offset N
```

`--session-root` supplies a recorded session working directory for path-relationship
inspection; repeat it when a resumed session has several source segments. Crossing Guard
canonicalizes every supplied root and reports the working-directory relation only when
the non-empty roots agree. A working directory is not called a Git repository unless a
captured checkout identity establishes that fact.

Observed tool targets retain independent relationships to the selected captured
checkout, another captured checkout, the recorded working directory, and the host
temporary roots. A path outside all identified roots is labeled exactly that; it is not
assumed to be another repository, unrelated work, or disposable. Targets matching nested
checkout roots remain ambiguous. These are action facts, not proof that a file changed.

## Recover missing evidence

The console names the existing command that can add each missing evidence type. The
displayed commands are templates with placeholders; review and replace every placeholder
before running them.

| missing fact | recovery | what it establishes |
| --- | --- | --- |
| declaration | `crossing-guard change declare ...` | a sourced claim about intended paths and intent |
| checkout snapshot | `crossing-guard change snapshot ...` | a new bounded Git observation at the time the command runs |
| implementation claim | `crossing-guard change claim ...` | a sourced claim about implementation paths |
| verification witness | `crossing-guard change verify-run ...` | the observed process boundary, executable identity, termination, and exit result |
| current repository understanding | `crossing-guard understand scan --repo <repo> --base <ref>` | a new structural/reference generation for the repository snapshot |

Recovery adds a new attributed observation. A current snapshot or understanding scan
does not reconstruct historical session actions, and the console must not present it as
though it did.

Totals, returned counts, exactness, and next offsets make every omitted response row
visible. Negative offsets are treated as zero; offsets beyond the population are treated
as the population end. The console provides page controls and resets child row pages when
you move between repositories. If history exceeds 1,000 records or distinct file touches
exceed 10,000, the response and panel name the envelope as partial with exact totals rather
than presenting the bounded population as complete. A session known only from an envelope can
appear in the session rail with “Transcript unavailable”; that does not claim the
governor captured its tool calls. Downstream symbol, package, configuration, data,
documentation, policy, and user-journey effects remain “not analyzed” until the separate
understanding layer supplies them.
