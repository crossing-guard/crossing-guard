# Framework and configuration

The framework owns mechanisms: supported event ingestion, evaluation, agent execution,
action grants, persistence and console evidence. Configuration supplies choices: complete
detector/rulebook documents, model routes, profiles, triggers, scopes and budgets.

Profiles describe an agent's purpose and instructions. Model routes describe inference
providers and models; credentials are not the agent's identity. Custom code can implement
additional analysis and behavior through supported interfaces. It still needs validated
inputs, scoped authority and an explicit failure result. Configuration cannot create an
unsupported provider capability.

## Explicit selection

The intended fresh-alpha baseline produces portable structural facts with an empty
unselected rulebook. Starters are explicit selections. Detector and rulebook documents
have separate owners: selecting one does not ensure the other produces all required facts.
The opt-in `security-observe` detector assembly contains the 30 portable floor detectors
plus four heuristic credential patterns and destination classification. It emits the
credential-material facts consumed by the separately selected security-observation rulebook.
Selecting detectors alone activates no rules and blocks no egress.

Preview before selecting. Durable selections pin complete documents and digests; existing
compatibility installations retain their behavior until explicitly changed. The console
exposes choices and results; observed runtime evidence establishes whether a path fired.

An extension must distinguish unavailable input, unsupported delivery, denied action,
unknown outcome and success. Do not blindly retry uncertain side effects. Recalled memory
and helper output inform decisions but never grant permission.


## Detector choices in this source candidate

Use `crossing-guard detectors preview security-observe` to inspect the complete document,
its digest and changed detector IDs. `detectors select security-observe` requires `--yes`
to apply. `detectors unselect --yes` archives the selection and returns to the recorded
installation baseline. That baseline differs for fresh and legacy installations.

`portable-floor` selects only the existing structural floor. `legacy` explicitly selects
the compatibility catalog; `baseline` and `starter` remain accepted historical aliases.
No separate safety detector document is needed: safety rules use command facts.

Security destination matching treats explicitly listed RFC1918, IPv4/IPv6 loopback and
IPv6 unique-local ranges as in-house. All other destinations are external unless you
customize and select a complete document with your network definitions. Credential patterns
can miss formats or obfuscated/split values and can match synthetic examples. These facts
support post-hoc observation; they do not prove a secret was transferred or stopped.

## Console selection

In Governance > Policy, use the detector review buttons for current, portable floor,
security observation or legacy compatibility. The rules section separately offers current,
safety starter, security observation, no rules and legacy compatibility. Each review shows
exact digests and changed IDs before you acknowledge and select a complete document.
Legacy compatibility is preserved for migration; it is not the recommended fresh baseline.

The security rulebook includes safety command guards plus report-only credential
observation. Select both security-observe documents to provide its credential facts and
observation rule. Selecting one does not select the other. Detector changes require a
daemon restart; the console distinguishes durable selection from the running assembly.

Unselect archives the selection and returns that artifact to its recorded installation
baseline. If configuration changed after your preview, the console rejects the stale
selection; use Reload Policy and review again before confirming a fresh preview.
The full installed-product journey remains a release gate.

## Codex launch options

The adapter explicitly selects the sandbox for new and resumed turns, including Read Only.
Sandbox and local-provider options precede the resume command. Prompt and session text
follow an argument terminator so text such as `--yolo` or `review` cannot become an option
or subcommand. Extra arguments accept only `--skip-git-repo-check` and one
`-c model_reasoning_effort=<level>` (or the equivalent `--config` form). Other values
are refused before launch. Remove unsupported saved values in Settings → extra args for
Codex to recover. The provider validates the reasoning-effort level at turn time.

This restricts adapter arguments; it does not make an authenticated arbitrary executable
override safe. Fixture HTTP checks verify admission and argument construction. Actual
vendor sandbox behavior and installed-client support require separate verification.

## Saved session views

Saved views live in `session-views.json` in the data directory; a fresh installation has
no owner-created views. The console manages this configuration through the experimental
session-views API. Writes carry the state token of the previously loaded file and refuse
stale changes or unreadable entries instead of overwriting them. Reload after a conflicting
edit and retry; repair an invalid entry before saving through the console.

See [organizing sessions](public/how-to/organize-sessions.md) for the user flow and
[loopback API](public/reference/loopback-api.md) for request details.
