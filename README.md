# Crossing Guard

An open source control layer for AI coding agents.

**Experimental source snapshot:** the framework and analysis components are available
for inspection and local builds. This is not a supported installed alpha or binary release.
See [the refreshed integration status](docs/refresh-status.md) for the current source pin and remaining release gates.

The public repository is [crossing-guard/crossing-guard](https://github.com/crossing-guard/crossing-guard). 

Crossing Guard connects supported coding workflows to a local control framework. Configure
rules, reuse project memory, and add reviewer and helper agents. Your code can extend
supported event and action paths. Coverage depends on the client, event and capability.
A saved configuration is not proof that its runtime path is active.

## Framework and configuration

The framework provides event handling, evaluation, agent execution, permission checks,
persistence and a local console. Configuration chooses the rules, detector documents,
agent instructions, model routes, triggers, budgets and grants. Custom code implements
additional behavior through supported extension points.

Read [framework and configuration](docs/framework-and-configuration.md) and
[agents and memory](docs/agents.md).

See [building the source candidate](docs/building-source.md) and
[building the analysis components](docs/building-components.md).

## Source-alpha verification

Before downloads are published, a clean source checkout must build and pass the quality
gate. The first target is a verified macOS alpha; release notes will state the exact
architecture, client versions and supported paths after testing.

The first-user journey is: inspect planned changes with `crossing-guard init --dry-run`,
consent to supported integrations, select configuration, deploy a scoped profile, verify
runtime firing and console evidence, exercise recovery, and uninstall. A build alone
is not proof of that journey. Other platforms remain unverified until tested separately.

## Project

The approved source license is [Apache-2.0](LICENSE). See the [third-party attribution inventory](THIRD_PARTY_NOTICES.md);
source-level native/generated attribution has been reviewed; final publication and artifact review remain open. See [contributing](CONTRIBUTING.md), [security](SECURITY.md),
and [support](SUPPORT.md). See [governance](GOVERNANCE.md), [conduct](CODE_OF_CONDUCT.md),
[release policy](docs/release-policy.md), and [data and recovery](docs/data-and-recovery.md).
Website: https://crossingguard.dev
