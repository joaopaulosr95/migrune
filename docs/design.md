# Design

migrune is built so that a migration either happens completely and is
recorded, or doesn't happen and stays pending, whatever goes wrong: a bad
migration, a crash, two pipelines at once, a cancelled job. This page explains
how, where that stops, and why migrune works the way it does.

## Guarantees

**Data and changelog commit together.** A migration's `Finished` changelog
entry is written in the same transaction as its data changes, so the changelog can't claim a migration that didn't happen or
miss one that did. This is why migrune needs a replica set.

**Applied migrations are immutable.** The changelog stores each file's
SHA-256, and lint, with a connection, fails if a `Finished` migration's file
changes or disappears; otherwise environments that applied different
versions would silently disagree, and a revert would look like a rollback. Retiring a
migration is an explicit act: delete its changelog entries.

**One runner at a time.** A run takes a global lock, one document
taken with an atomic `findOneAndUpdate` and owned through a random token,
new on every acquisition, so runners sharing a runner id still exclude
each other. The holder sends a heartbeat every 10 seconds, and a dead
holder's lock expires after the longer of 30 seconds and the lock TTL,
computed with the server's clock, so clock skew between machines doesn't
matter. A paused or cut-off holder may wake up after its lock was taken over, so every
transaction also writes to the lock document, matching only while its runner
owns it (a **fence**): a zombie's transaction aborts. The
90-second default is above MongoDB's 60-second transaction lifetime, so a
live runner's transaction never outlives its lock. Details in
[changelog and lock](reference/changelog-and-lock.md).

**Failures are recorded; interruptions are clean.** A transaction that
fails with a transient error (a write conflict, an election) runs again, up
to 3 more times, and a commit whose outcome is unknown is settled by looking
for its changelog entry. A migration that fails is recorded as `Errored`.
SIGINT or SIGTERM aborts the open transaction (a commit already sent
finishes), writes no changelog entry and releases the lock; SIGKILL leaves
what a crash would, which the transaction and lock expiry already handle.

### What migrune doesn't guarantee

- **Compatibility with your code.** Every migration must work with the code
  running before and after the deploy; reviews and expand/contract ensure
  that, not migrune.
- **Correct values.** lint checks a migration's shape, not that it computes
  the right values.
- **Changes migrune can't express.** Dropping or renaming collections,
  creating collections with options or views, and inserting documents happen
  out of band and aren't in the changelog. Record them in a migration's
  description or in your deploy log.
- **Sharded clusters.** They aren't supported: the guarantees above are
  only tested on replica sets (see [Requirements](../README.md#requirements)).

## Why migrations are JSON

Most MongoDB migration tools run code. migrune runs declarative Extended JSON
steps, each naming a collection, an operation and its native arguments.

- **Lint works offline.** A file can be validated without running it, in a
  pull request with no database and no credentials.
- **Reviewers approve data, not code.** A reviewer sees exactly the filter,
  update and pipeline that will run with write access to production, with no
  hidden helper.
- **No language dependency.** One binary runs migrations for a project in
  any language.
- **The query language is enough** for the vast majority of changes:
  backfills, value renames, denormalisation across collections, seeds.
  Extended JSON keeps types exact.
- **The audit is self-contained.** The changelog stores the migration as it
  ran, readable after the file is gone.

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
opposite; it goes through lint and the changelog like any other.

Down migrations promise more than they deliver. They can't restore what a
`$unset`, a `deleteMany` or a lossy conversion destroyed; they are rarely
tested and run only in emergencies, against data their author never saw; and
they also revert whatever the application wrote since.

They aren't needed either. Migrations run before the deploy and must be
compatible with the running code, so if the deploy is rolled back the old code
runs on migrated data it was required to handle. Changes that would break it
are split across releases: add the new field, move the code, drop the old
field later.

A `git revert` of a migration file changes no data, so lint fails on a
`Finished` migration whose file is gone instead of ignoring it (see
[troubleshooting](troubleshooting.md#applied-but-missing)). A compensating
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
