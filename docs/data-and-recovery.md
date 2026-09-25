# Data, export and recovery

The default data root is `~/.crossing-guard`; the local SQLite store is `index.sqlite`.
Memory files and other configuration can live outside that database. Data may contain
prompts, tool activity, source paths, results and recalled information. Review it before
sharing. Do not attach whole databases or transcripts to public issues.

`crossing-guard export PATH` creates a consistent SQLite snapshot. It refuses to overwrite
an existing destination. This is a database backup, not a complete backup of memory files,
provider settings, credentials or the installation. Keep those separately as appropriate.
The exported snapshot contains the same sensitive records as the source database.

The September 25 disposable trial created a schema-28 store with the older binary, retained
one synthetic task and its four events across upgrade to schema 32, exported the result,
and reopened a pre-upgrade backup with the older binary. Existing-backup overwrite and
newer-schema access by an older binary were refused. This does not cover every historical
dataset or establish recovery of a live installation.

An older executable is not an in-place downgrade tool. Recovery requires stopping writers
and choosing a compatible pre-upgrade snapshot. Rehearse on a separate copy before making
an intentional installed-service change. A disposable HOME alone does not isolate macOS
background-service names. Do not run init/uninstall merely to test recovery alongside an
existing installation. See [local evaluation limits](evaluating-local-bundle.md).
