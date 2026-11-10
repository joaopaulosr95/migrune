# Compatibility

migrune follows [Semantic Versioning](https://semver.org). This page says
what that covers: what a minor or patch release keeps working, and what only
a major release may change. Before 1.0, breaking changes bumped the minor
version instead.

Only the latest release is supported: fixes, security fixes included, ship in
the next release, and nothing is backported.

## Stable

A minor or patch release doesn't break these. Adding to them (a new command,
flag, optional field or exit code meaning) is a minor release.

- **Commands and flags**: `lint`, `plan`, `apply` and `drift`, their flags
  and the `MIGRUNE_*` environment variables
  ([CLI reference](cli/migrune.md), [environment variables](env-and-exit-codes.md)).
- **Exit codes** ([exit codes](env-and-exit-codes.md#exit-codes)).
- **The migration file schema**: a file that is valid in one release stays
  valid, and does the same thing, in every later release of the same major
  ([migration file](migration-file.md)).
- **The changelog and lock documents**, and their format versions
  ([format versions](changelog-and-lock.md#format-versions)).
- **The plan digest file** ([plan digest](report.md#plan-digest)): a plan and
  an apply from two releases of the same major agree on it.
- **JSON output**: the report (`-o json`) and the lint and drift findings
  (`-o json`). Fields are only added, never renamed, removed or changed in
  meaning ([report](report.md), [lint rules](lint-rules.md), [drift](drift.md)).
- **The images**: the tags (`X.Y.Z`, `X.Y`, `X`, `latest`, and their
  `-debug` variants), a distroless image running as a non-root user, and
  `migrune` as the entrypoint.

## Not stable

These can change in any release: the table output's wording and layout, log
messages and their attributes, warning text, and the text of error messages.
Parse the JSON output and the exit code, not the text.

## Lint rules

A new rule that only warns is a minor release. A new rule that reports
errors, or a warning that becomes an error, is a major release, since it can
fail a CI job that passed before.

## Removals

A major release may remove or change a flag, field or behaviour without
warning about it in an earlier release first. Its release notes list every
such change.

## MongoDB versions

Support follows MongoDB's lifecycle (see [Requirements](../../README.md#requirements)).
Dropping a MongoDB major that reached its end of life is a minor release. Its
release notes name the last migrune release that supports it.

## Downgrades

Downgrading migrune across a changelog or lock format change isn't
supported: older releases refuse a database a newer format was written to
([format versions](changelog-and-lock.md#format-versions)). A new format
version comes only with a major release.

## Go module

`go install` uses the module path, so a major release from 2.0 on changes it
to `github.com/joaopaulosr95/migrune/v2` (and so on). migrune has no Go API:
its packages are all `internal`.
