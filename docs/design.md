# Design

migrune is built so that a migration either happens completely and is
recorded, or doesn't happen and stays pending, whatever goes wrong: a bad
migration, a crash, two pipelines at once, a cancelled job. This page explains
how, where that stops, and why migrune works the way it does.

## Guarantees

**Data and changelog commit together.** A data migration runs in one
transaction, and its `Finished` changelog entry is written in that same
transaction, so the changelog can't claim a migration that didn't happen or
miss one that did. This is why migrune needs a replica set. Index builds and
`$merge` can't run in a transaction, so those kinds write their entry right
after their last step; if a run dies in between, the next run repeats steps
that are idempotent by construction.

**Idempotency is checked, not trusted.** Every data migration runs twice in
its transaction and fails if the second run changes anything, which catches
any non-idempotent update, pipeline updates included, without a list of
allowed operators. Merges can't run twice in a transaction, so migrune counts
the documents `$merge` would change before and after it, and requires 0 after.
The count is type-strict, and a merge that would change an existing field's
BSON type fails before writing, because a type changed in place breaks the
code still reading the old type. Since the changelog already runs each
migration once per database, this is a safety net for crash re-runs, retired
or renamed migrations and `--mutable`.

**Applied migrations are immutable.** The changelog stores each file's
SHA-256, and plan and apply fail if a `Finished` migration's file changes or
disappears; otherwise environments that applied different versions would
silently disagree, and a revert would look like a rollback. Retiring a
migration is an explicit act: delete its changelog entries.

**Reads come from the primary.** migrune reads from the primary with read
concern `majority`, whatever the URI's `readPreference` and
`readConcernLevel` (see [credentials](running.md#credentials)). A
secondary can lag behind the writes migrune just made: a merge reading one
would compare against data from before an earlier migration, and could be
recorded `Finished` without changing anything. A read that isn't
majority-committed could see a write that a failover then rolls back.
Transactions keep read concern `snapshot`; `listIndexes` and
`listCollections` accept no read concern and read the primary's current
state.

**One runner at a time.** plan and apply take a lock, one document per
[namespace](running.md#several-apps-on-one-database), taken with an atomic
`findOneAndUpdate` and owned through a random token,
new on every acquisition, so runners sharing a `--runner-id` still exclude
each other. The holder sends a heartbeat every 10 seconds, and a dead
holder's lock expires after `max(30s, --lock-ttl)`, computed with the
server's clock, so clock skew between machines doesn't matter. A paused or
cut-off holder may wake up after its lock was taken over, so every
transaction also writes to the lock document, matching only while its runner
owns it (a **fence**): a zombie's transaction aborts. Index and merge steps
can't be fenced, so the runner stops before sending one once its last
successful heartbeat is too old to be sure the lock is still its own. The
90-second default is above MongoDB's 60-second transaction lifetime, so a
live runner's transaction never outlives its lock. Details in
[changelog and lock](reference/changelog-and-lock.md).

**What is approved is what runs.** plan runs everything for real and throws
the result away (data migrations run twice and abort, indexes are diffed
against `listIndexes`, merges run without `$merge`), so it catches
non-idempotent data migrations, type changes, duplicate merge keys and index
conflicts against the actual data. It can't prove a merge converges: that
shows only once apply has written (`diffCountAfter`). It then
writes a digest of the target database and replica set names, the
deployment (the UUID of its `migrations_changelog` collection), the pending
ids and checksums. apply requires it (`--digest-in`, unless `--no-digest`
for a local run) and runs nothing if its own digest differs, so a plan made
on staging can't approve production, even when their names are the same.
A snapshot clone of a database keeps the collection's UUID, so a plan
against the clone matches the original; that is harmless, as the clone holds
the same changelog.

**Failures stop the run; interruptions are clean.** A transaction that
fails with a transient error (a write conflict, an election) runs again, up
to 3 more times, and a commit whose outcome is unknown is settled by looking
for its changelog entry. apply stops at the first failure, records it as
`Errored` and exits 1, so a deploy that depends on it doesn't run. Because
every migration must work with the running code, stopping half way through a
release is safe. SIGINT or SIGTERM aborts the open transaction (a commit
already sent finishes), writes no changelog entry, releases the lock and still
prints the report; SIGKILL leaves what a crash would, which the transaction
and lock expiry already handle.

### What migrune doesn't guarantee

- **Compatibility with your code.** Every migration must work with the code
  running before and after the deploy; reviews and expand/contract ensure
  that, not migrune.
- **Atomicity of index and merge migrations.** A failure leaves earlier steps
  done and a merge possibly half written; the next run completes them, as
  long as the merge converges.
- **Merges that don't converge.** A merge that computes a field from its own
  value writes a new result on every run. plan can't see that; apply detects
  it after writing and fails.
- **Correct values.** The checks prove a data migration is repeatable and a
  merge keeps types, not that it computes the right values. The plan's report and samples
  are there for a human to read.
- **Unique index builds.** plan can't predict a unique index build failing
  on duplicates; apply finds out. Converting an existing index to unique
  with `collModIndex` is checked: see
  [making an index unique](writing-migrations.md#changing-an-index-in-place).
- **Bounded run times.** migrune sets no time limit on index builds (and
  no `commitQuorum`, so the server's default applies), merges or plan's
  reads; a run on a large collection lasts as long as the server takes.
  The CI job timeout is the bound: see
  [large collections](running.md#large-collections).
- **Who approved a run.** `approvedBy` (`--approved-by-name`,
  `--approved-by-email`) is an unverified label: migrune records what the
  apply job passes. The plan digest is an integrity check, not an
  authorization: it proves apply runs what plan saw, as long as the digest
  file apply reads is the plan job's own, kept in an artifact store the
  pipeline trusts. Who may start apply is your CI's gate (see
  [running in CI](running.md#in-ci-with-an-approval-gate)).
- **Work on the server after the runner loses it.** An index build or merge
  already sent keeps running on the server if the runner loses its network
  mid-step, possibly after the lock expired and another runner took it. The
  fence stops the runner's own later writes, not what the server is already
  doing.
- **Changes migrune can't express.** Dropping or renaming collections,
  creating collections with options or views, and inserting documents happen
  out of band and aren't in the changelog. Record them in a migration's
  description or in your deploy log.
- **Sharded clusters.** They are refused: connected to a mongos, migrune
  exits 1 with an error (see [Requirements](../README.md#requirements)).

## Why migrations are JSON

Most MongoDB migration tools run code. migrune runs declarative Extended JSON
steps, each naming a collection, an operation and its native arguments.

- **The runner can check idempotency.** A step is data, so migrune can run it
  twice and inspect what MongoDB reports. A script can do anything between two
  writes, so only its author knows if it is safe to repeat.
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
itself; the server does that during plan.

## Why there are no rollbacks

Migrations are forward-only. To undo one, write a new migration that does the
opposite; it goes through lint, plan, approval and the changelog like any
other.

Down migrations promise more than they deliver. They can't restore what a
`$unset`, a `deleteMany` or a lossy conversion destroyed; they are rarely
tested and run only in emergencies, against data their author never saw; and
they also revert whatever the application wrote since.

They aren't needed either. Migrations run before the deploy and must be
compatible with the running code, so if the deploy is rolled back the old code
runs on migrated data it was required to handle. Changes that would break it
are split across releases: add the new field, move the code, drop the old
field later.

A `git revert` of a migration file changes no data, so migrune stops on a
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
production, so plan, approval and audit add only friction. Keep them in a seed
script your application owns, run against a database migrune has migrated.

## Alternatives considered

The facts about golang-migrate and migrate-mongo come from their
documentation; Flyway was tried with its MongoDB support (version 13.12.1,
Community edition, JSON migrations) against a single-node replica set. Tools
change: check current versions before deciding.

|                                 | golang-migrate (MongoDB driver)                      | migrate-mongo                                    | Flyway (MongoDB, JSON)                                                    | migrune                                                                   |
| ------------------------------- | ---------------------------------------------------- | ------------------------------------------------ | ------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| Migration format                | JSON arrays of `runCommand` documents, up/down pairs | JavaScript modules with `up` and `down`          | One command document per file                                             | Declarative Extended JSON: data, index and merge kinds, with a JSON Schema |
| What counts as pending          | Anything above one stored version number            | Any file missing from the changelog              | Versions missing from its history collection                              | Any id without a `Finished` changelog entry                                |
| A file merged late, out of order | Silently skipped                                    | Runs                                             | Error by default; runs with `outOfOrder`                                  | Runs, in id order                                                          |
| Idempotency                     | Not checked                                          | The author's responsibility                      | Not checked                                                               | Data migrations run twice in a transaction; merges must reach a diff of 0  |
| Edits to applied files          | Not detected                                         | Ignored, or re-run with a file hash option       | Rejected (checksum)                                                       | Rejected (checksum); `--mutable` re-runs them locally                      |
| Transactions                    | Opt-in                                               | Written by the author                            | On by default for JSON, off per file                                      | Always for data migrations; index and merge kinds run outside by design    |
| Record written with the data    | No: a version number and a dirty flag                | No: written after `up` resolves                  | Yes, for transactional files                                              | Yes: the `Finished` entry is in the migration's transaction                |
| Lock                            | Advisory lock collection                             | Lock collection, optional TTL                    | None; two parallel runs end in a write conflict                           | Global lock with heartbeat, server-time expiry and an in-transaction fence |
| On failure                      | Version marked dirty; a human forces it              | Stops                                            | Transactional: rolled back. Otherwise a failed row blocks until `repair` | Stops, records `Errored`; the file can be fixed in place                   |
| Dry run                         | None                                                 | None                                             | None for MongoDB (`info` lists what's pending)                            | `plan` runs everything and aborts; `apply` is tied to the plan's digest    |
| Indexes                         | Any command, unchecked                               | Any code, unchecked                              | `createIndexes` with transactions off                                     | Own kind; lint replays them and checks expected indexes                    |
| Cross-collection logic          | `aggregate` through `runCommand`, unchecked          | Any code                                         | `$merge` failed with an error although the data was written               | Merge kind: pipeline ending in `$merge`, `$out` banned, diff counted       |
| Audit history                   | Current version only                                 | File name, date, optional hash                   | Version, checksum, date, duration, success                                | Outcome, step results, runner, approver and the migration as applied       |
| Lint without a database         | JSON parsing                                         | None (it's code)                                 | None                                                                      | Schema, Extended JSON, pipeline rules, prefixes, index replay              |
| Rollbacks                       | Down files                                           | `down`                                           | Undo migrations in paid editions                                          | Forward-only: compensating migrations                                      |

- **golang-migrate** suits teams already using it for SQL. For MongoDB, its
  single version number breaks when releases pick migrations out of order, and
  a failure needs a human to force the version.
- **migrate-mongo** is the cheapest option if you accept code migrations,
  trust authors with idempotency and do without plan or lint; the changelog
  written in the migration's transaction can't be added as a convention.
- **Flyway** has transactional JSON migrations, checksums and out-of-order
  runs, but no lock, dry run or idempotency check, and its `$merge` support
  didn't work when tried.
- **migrune** has a plan to approve, verified idempotency and offline lint,
  at the cost of being a smaller, younger, MongoDB-only tool.

Other approaches: a **workflow engine** adds a service every deploy depends
on and still needs a database-side fence; **letting the ODM create indexes**
at startup never drops, races between replicas and often logs conflicts
instead of failing (migrune's `--expected-indexes` compares with what the
application declares); **hand-run scripts** record nothing, don't prevent
concurrent runs and aren't checked for re-runs.
