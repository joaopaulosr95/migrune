# Changelog and lock documents

migrune keeps its state in two collections of the target database (`--db`),
next to your data:

- `migrations_changelog`: one entry per migration outcome, the audit trail.
- `migrations_lock`: at most one document per
  [namespace](../running.md#several-apps-on-one-database), the lock that keeps
  two runs in that namespace from working at once.

Both are written with write concern `majority` and read with read concern
`majority`; data migrations run in transactions with read concern
`snapshot`. Every timestamp is the server's clock (`$$NOW`), never the
runner's.

## Format versions

Every changelog entry and every lock document carries its format version in
`v`. This migrune writes and reads changelog entries v1 and lock documents
v1. A document without `v`, such as one written by hand, reads as v1.

Every command that connects (plan, apply, drift, status, unlock, and lint
with `--uri`) first looks for a changelog entry, in any namespace, or a lock document with
a `v` higher than the one it knows, before it reads or writes anything else.
If it finds one, a newer migrune has written to this database, and the run
fails with exit code 1, writing nothing:

```txt
Error: the changelog has format v2, newer than this migrune understands (v1): upgrade migrune
```

So **downgrading migrune across a format change isn't supported**: once a
newer migrune has written a newer format, older ones refuse the database.
Upgrade to a migrune that knows the format (see
[troubleshooting](../troubleshooting.md#the-changelog-or-lock-has-a-newer-format)).
A new format version comes only with a major release.

Neither lookup uses an index: the lock collection holds one document per
namespace, and the changelog grows by one entry per migration outcome, so
scanning it costs about as much as the changelog read plan and apply make
anyway.

## Changelog: `migrations_changelog`

apply writes an entry when a migration finishes or fails. plan, lint, drift,
status and unlock never write an entry.

plan and apply create the collection, empty, when it doesn't exist, after
apply's [`--init`](report.md#first-apply) check and before they
take the lock: its UUID is the [plan digest](report.md#plan-digest)'s
deployment, so it must exist from the first plan on. A refused apply, for
`--init` or a [digest](report.md#plan-digest) mismatch, writes no changelog
entry and runs no migration. A refused `--init` writes nothing at all; a
digest mismatch writes only what plan would: the lock, taken and released,
and the empty collection if it was missing.

- **`Finished`**: written when the migration succeeds. For a data migration,
  inside its transaction, together with its data changes, so the entry and
  the data commit or abort together; for a batched one, inside its last
  batch's transaction. For index and merge migrations, right
  after the last step; if the run dies between the two, the next run repeats
  the steps, which is harmless.
- **`Errored`**: written after the migration failed, in a transaction of
  its own, fenced like a data migration's (see [fence](#fence)): a runner
  that lost its lock writes no `Errored` entry, and its report row says the
  lock was lost.
- An interrupted run (SIGINT, SIGTERM) or a run that lost its lock writes no
  entry. Checks that fail before any migration runs (an edited or missing
  applied migration, a digest mismatch) write none either.

Every read of the changelog (plan, apply, lint, drift, status) takes only the
entries of the run's namespace: `{namespace: "<name>"}`, or `{namespace: null}` for the
default namespace, which also matches entries without the field, such as
those written before namespaces existed.

A migration is **pending** while it has no `Finished` entry in its namespace:
`Errored` entries don't count. When a migration has several `Finished` entries
(it was re-run with `--mutable`, or retired and applied again), the latest
one, by `createdAt`, holds the checksum that file edits are compared with.

Example `Finished` entry for a data migration:

```js
{
  _id: ObjectId('6aefbe2f08c49a79b06c466b'),
  v: 1,
  migrationId: '2026-11-01-001-seed-currencies-and-plans',
  checksum: '5f0c…',
  createdAt: ISODate('2026-11-08T00:11:27.010Z'),
  runner: 'ci-42-1-AB12CD34',
  approvedBy: { name: 'Ada Lovelace', email: 'ada@example.com' },
  status: 'Finished',
  migration: {
    id: '2026-11-01-001-seed-currencies-and-plans',
    description: 'Seed the reference currencies and the two subscription plans',
    steps: [ … ]
  },
  results: [
    { collection: 'currencies', operation: 'updateOne', matchedCount: 0, modifiedCount: 0, upsertedCount: 1 },
    …
  ]
}
```

| Field              | BSON type | Description                                                                                         |
| ------------------ | --------- | --------------------------------------------------------------------------------------------------- |
| `_id`              | ObjectId  | Generated for each entry, before its transaction starts.                                            |
| `v`                | int       | The entry format version: `1`. See [format versions](#format-versions).                              |
| `namespace`        | string    | The run's `--namespace`. Absent in the default namespace.                                           |
| `migrationId`      | string    | The migration id: the file name without `.json`.                                                    |
| `checksum`         | string    | SHA-256, lowercase hex, of the migration file's bytes when it ran.                                  |
| `createdAt`        | date      | When the entry was written, server time.                                                            |
| `runner`           | string    | The runner id that held the lock (`--runner-id`).                                                   |
| `approvedBy`       | document  | `{name, email}` from `--approved-by-name` and `--approved-by-email`. Empty strings when not given.  |
| `status`           | string    | `Finished` or `Errored`.                                                                            |
| `migration`        | document  | The migration as it ran: `id`, `description`, `batchSize` when the file has one, and `steps`, parsed from the file. Not byte-identical to the file. |
| `results`          | array     | `Finished` only. One item per step, in file order; see below.                                       |
| `batches`          | int       | `Finished` entries of [batched](../writing-migrations.md#batched-data-migrations) data migrations only: how many batches ran, each in a transaction of its own; 1 when the filter matched nothing. |
| `errors`           | array     | `Errored` only. `{step, message}` items; `step` is the failed step's index in file order, absent when the error isn't tied to a step; `message` is the report's error text, without the server's message unless `--sample-values` is set (see [values in errors](report.md#values-in-errors)). |

Each item of `results` has `collection` and `operation`, plus what MongoDB
returned for that kind of step:

| Kind  | Fields                                                                                 |
| ----- | -------------------------------------------------------------------------------------- |
| data  | `updateOne`, `updateMany`: `matchedCount`, `modifiedCount`, `upsertedCount`. `deleteMany`: `deletedCount`. From the first of the two runs; for a batched migration, summed over its batches. |
| index | `indexName` (not for `collModCollection`), and for `collModIndex` and `collModCollection` their `set`, as in the file. |
| merge | `diffCountBefore`, `diffCountAfter`, `unmatchedCount`, `typeChangedCount`, `duplicateOnCount`, `nullOnCount`, `arrayOnCount`, `idMismatchCount`, and `collationOnlyMatchCount` when the unique index on the `on` fields has a non-simple collation. See [how the comparison works](../writing-migrations.md#how-the-comparison-works). |

A batched migration's summed counts can undercount: when the commit outcome
of a batch other than the last is unknown, apply runs the batch again, and
if the first commit had in fact landed, the sums take the rerun's counts,
which find the batch's documents already changed: 0 modified or deleted.

To read the latest `Finished` entry of a migration, steps included (in a
named namespace, add `namespace: "<name>"` to the filter):

```js
db.migrations_changelog.find({migrationId: "<id>", status: "Finished"}).sort({createdAt: -1}).limit(1)
```

apply creates one index on this collection once its digest matched, before
it runs any migration (the read before it scans without it), named
`namespace_status_createdAt`, with keys `{namespace: 1, status: 1,
createdAt: 1, _id: 1}`: the namespace and status of the read, then its sort.
When an index of that name and those keys already exists, apply goes on; when
the name is taken by an index with other keys, apply fails. plan and lint
don't create it.

Don't edit entries; deleting a migration's entries is how you retire it on
purpose (see [troubleshooting](../troubleshooting.md#fix-2-retire-the-migration)).

## Lock: `migrations_lock`

One lock per namespace in each database: a document with `_id:
"migrations"` for the default namespace, `"migrations:<name>"` for the
namespace `<name>`. A namespace name can't hold `:`, so two namespaces never
share a lock, and runs in different namespaces don't wait for each other.
plan and apply take their namespace's lock before reading the changelog and
release it when they finish, fail or are interrupted. lint, drift and status
don't take it.

```js
{
  _id: 'migrations',
  v: 1,
  token: '9f86d081884c7d659a2feaa0c55ad015',
  owner: 'ci-42-1-AB12CD34',
  current: '2026-11-03-001-copy-currency-symbol',
  acquiredAt: ISODate('2026-11-08T00:10:02.114Z'),
  lastHeartbeat: ISODate('2026-11-08T00:11:27.300Z'),
  expiresAt: ISODate('2026-11-08T00:12:57.300Z')
}
```

| Field           | BSON type    | Description                                                                                       |
| --------------- | ------------ | ------------------------------------------------------------------------------------------------- |
| `_id`           | string       | `migrations`, or `migrations:<name>` in the namespace `<name>`.                                   |
| `v`             | int          | The lock format version: `1`, set on every write. See [format versions](#format-versions).        |
| `token`         | string       | A random 128-bit value, in hex, new on every acquisition. It alone says who owns the lock.        |
| `owner`         | string       | The holder's runner id (`--runner-id`): a label for people, shown in "lock is held" errors. Two runners may share it. |
| `current`       | string, null | The migration the holder is running, or null between migrations.                                 |
| `acquiredAt`    | date         | When the holder took the lock, server time.                                                       |
| `lastHeartbeat` | date         | The holder's last heartbeat, server time.                                                         |
| `expiresAt`     | date         | When another runner may take the lock: server time of the holder's last write plus its expiry.   |

The **expiry** is the longer of 3 heartbeat intervals (30 seconds) and
`--lock-ttl` (90 seconds by default, at least 61), so with the defaults a
lock expires 90 seconds after the holder's last write.

### Acquire

A runner takes the lock with one atomic `findOneAndUpdate` with upsert, under
a new token. It gets the lock when there is no lock document, or when the
lock has **expired** (`expiresAt < $$NOW`); the document is then rewritten
with its token, its runner id, and `expiresAt` set to `$$NOW` plus its
expiry. A runner that crashed and runs again has a new token, so it waits for
its old lock to expire like any other runner. A lock document with a newer
`v` is never taken, even expired: the run fails as described in
[format versions](#format-versions).

Otherwise the run fails with exit code 3, before anything else and without a
report, naming the holder:

```txt
Error: migration lock is held by runner "ci-42-1-AB12CD34" (applying 2026-11-03-001-copy-currency-symbol), last heartbeat 2026-11-08T00:11:27Z
```

With `--lock-wait <duration>`, the runner keeps trying, with a backoff from 1
to 10 seconds, until the duration has passed, then fails the same way, with
exit code 3. A signal while waiting exits at once with 128 + the signal
number.

There is no TTL index: the document is overwritten on takeover and deleted on
release.

### Heartbeat

While it holds the lock, the runner refreshes `lastHeartbeat` and `expiresAt`
every 10 seconds, during index builds and merges too, and right before each
transaction. A timed heartbeat waits at most 10 seconds for the server. If
the lock document no longer has the runner's token, the lock is lost: the run
stops at once.

The runner also keeps a **local lease**: the time it sent its last heartbeat
that succeeded, plus the expiry, minus a 10-second margin. The expiry counts
for at most 80 seconds here, the [unlock](#unlock) threshold with MongoDB's
default transaction lifetime, so the lease is 70 seconds at most, whatever
`--lock-ttl` is. When the lease has passed, the server may hand the lock to
another runner soon, or unlock may remove it, so the run stops, as if the
lock were lost. The lease is measured on the runner's monotonic
clock, so a step of its system clock, such as NTP setting it back, doesn't
extend it. The runner checks the lease right before every
index command (`createIndexes`, `dropIndexes`, `collMod`) and every `$merge` aggregate,
which aren't fenced, and after each heartbeat that fails.

`--lock-ttl` must stay above the server's transaction lifetime limit
(`transactionLifetimeLimitSeconds`, 60 seconds by default), so the lock can't
expire while one of the holder's transactions is open; migrune rejects a
value under 61 seconds. Raise it if your server allows longer transactions.

### Fence

Every transaction (a data migration's, the one an index or merge migration's
`Finished` entry is written in, and the one each `Errored` entry is written
in) first updates the lock document, matching `{_id: <the namespace's lock
id>, token: <this runner's token>}`. If another runner took the lock over,
that update matches nothing, or conflicts with the takeover, and the
transaction aborts. So a runner that lost its lock (a "zombie", paused or cut
off for longer than the expiry) can never commit. Index and merge steps aren't
fenced; the local lease keeps a runner from sending one once its lock may have
expired, and they are idempotent, so one that still lands gives the same
result.

### Transactions

Transactions use read concern `snapshot` and write concern `majority`.

- **Retries.** A transaction that fails with MongoDB's
  `TransientTransactionError` label (a write conflict with your
  application's writes, an election) runs again from the start, steps
  included, up to 3 more times, after 0.2, 0.4 and 0.8 seconds, while the run
  is live. Each attempt has a new session, a fresh heartbeat and a new fence.
  A fence that matches nothing never runs again: the lock is lost. A fence
  that conflicts with a takeover is a transient error, and the next
  attempt's fence matches nothing.
- **Commit.** The commit isn't cut short by SIGINT or SIGTERM: it runs on its
  own 30-second budget, and the report shows what happened. A commit that
  fails with `UnknownTransactionCommitResult` (a failover or a timeout while
  committing) is sent again, up to 3 times. If its outcome is still unknown,
  migrune ends the session, so the transaction can't commit later, and
  looks up the entry it wrote by its `_id`, with read concern
  `linearizable`, so a commit that landed can't be missed: there, the
  migration is applied;
  missing, the transaction didn't commit and the migration failed (a data
  migration then gets an `Errored` entry).

### Release

On finish, failure or interruption, the runner deletes the lock document if
it still has its token, with its own short timeout (5 seconds), so the next
run doesn't wait for the expiry. If another runner owns it by then, it is
left alone. When the release fails after a successful run, migrune logs a
warning and still exits 0: the lock expires on its own.

### Unlock

`migrune unlock --token <token>` removes one namespace's lock only when its
token matches and its last committed heartbeat is older than the **silence
threshold**:

```txt
transactionLifetimeLimitSeconds + 2 × heartbeat interval
```

The heartbeat interval is 10 seconds, so the threshold is 80 seconds with
MongoDB's default 60-second transaction lifetime. This guard is independent
of `expiresAt` and `--lock-ttl`: a stopped holder's lock can be removed
before its expiry.

Heartbeats pause while the holder has a transaction open. Its
[fence](#fence) updates `lastHeartbeat` at the transaction's start, but that
write becomes visible only when the transaction commits. A live holder can
therefore have no committed heartbeat for as long as the server allows a
transaction to live. The threshold covers that lifetime and two heartbeat
intervals. status uses the same threshold for its heartbeat label and
`unlockAfter`.

Both commands read `transactionLifetimeLimitSeconds` with
`{getParameter: 1, transactionLifetimeLimitSeconds: 1}` on `admin`. If that
read fails for any reason, including missing privileges, they assume 60
seconds and log it, at warn in unlock and at info in status, where it only
labels the heartbeat: **if the server's limit was raised, unlock may remove
the lock of a runner inside a transaction**. Resolve the failed read
or verify that the limit is still the default before using that fallback.

The removal is one atomic `findOneAndDelete` on `migrations_lock`, with this
filter (the threshold is in milliseconds):

```js
{
  _id: <the namespace's lock id>,
  token: <the supplied token>,
  $expr: {
    $lt: ["$lastHeartbeat", {$subtract: ["$$NOW", <threshold>]}]
  }
}
```

The server's clock decides whether removal is allowed. A new acquisition
has a different token and cannot be removed with the inspected token. The
delete can wait behind an open transaction; after it commits, MongoDB
re-evaluates the filter against the updated lock. The delete's timeout is
the transaction lifetime read above (or its fallback) plus 10 seconds. After
5 seconds, unlock logs at info that it is waiting for an open transaction
on the lock to end.

unlock writes nothing else. If nothing was deleted, it reads the locks to
explain why; see [refusals and recovery](../troubleshooting.md#the-lock-is-held).
