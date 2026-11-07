# Design

This page explains where migrune's checks stop, and why migrune works the
way it does.

## What migrune doesn't guarantee

- **Compatibility with your code.** Every migration must work with the code
  running before and after the deploy; reviews and expand/contract ensure
  that, not migrune.
- **Correct values.** migrune checks a migration's shape, not that it
  computes the right values.
- **Changes migrune can't express.** Dropping or renaming collections,
  creating collections with options or views, and inserting documents happen
  out of band. Record them in a migration's description or in your deploy
  log.
- **Sharded clusters.** They aren't supported (see
  [Requirements](../README.md#requirements)).

## Why migrations are JSON

Most MongoDB migration tools run code. migrune runs declarative Extended JSON
steps, each naming a collection, an operation and its native arguments.

- **Files are checked offline.** A file can be validated without running
  it, in a pull request with no database and no credentials.
- **Reviewers approve data, not code.** A reviewer sees exactly the filter,
  update and pipeline that will run with write access to production, with no
  hidden helper.
- **No language dependency.** One binary runs migrations for a project in
  any language.
- **The query language is enough** for the vast majority of changes:
  backfills, value renames, denormalisation across collections, seeds.
  Extended JSON keeps types exact.

The cost: no application logic (hashing a password, calling an API), which is
deliberate, since such jobs are rarely repeatable or reviewable; no hand-written
conditionals, whose checks come from the database or migrune instead; and
verbose pipelines, which are also exactly what runs.

The file format is a standard JSON Schema (draft 2020-12), not MongoDB's
`$jsonSchema`, which needs a running server and can't express a step's shape
(no `const`, `if`/`then` or `$defs`). Neither validates the query language
itself; the server does that when the migration runs.

## Why there are no rollbacks

Migrations are forward-only. To undo one, write a new migration that does the
opposite; it is checked and reviewed like any other.

Down migrations promise more than they deliver. They can't restore what a
`$unset`, a `deleteMany` or a lossy conversion destroyed; they are rarely
tested and run only in emergencies, against data their author never saw; and
they also revert whatever the application wrote since.

They aren't needed either. Migrations run before the deploy and must be
compatible with the running code, so if the deploy is rolled back the old code
runs on migrated data it was required to handle. Changes that would break it
are split across releases: add the new field, move the code, drop the old
field later.

A `git revert` of a migration file changes no data. A compensating
migration is better: written for the data as it is now, reviewed like the
original, and recorded next to it.

## Why fixtures are out of scope

Migrations may seed **reference data**, the same in every environment and
assumed by the code: currencies, a price table, canonical categories. A seed
is an ordinary data migration, an upsert on a natural key with `$setOnInsert`:
idempotent, and it never overwrites a value an environment changed. `insert*`
is rejected because inserting twice isn't idempotent.

**Fixtures** (demo accounts, test users, sample projects) don't belong: they
differ per environment, change with every feature while applied migrations
are immutable, need application logic and secrets, and never reach
production, so review and audit add only friction. Keep them in a seed
script your application owns, run against a database migrune has migrated.
