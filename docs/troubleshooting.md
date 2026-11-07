# Troubleshooting

The errors below are shown as migrune prints them by default; with
`--log-format json`, the same text is the `msg` of a
[JSON log record](reference/env-and-exit-codes.md#exit-codes). The queries
run in `mongosh` against the target database.

## The server can't be reached

Every command that connects waits for a server it can use. After 30
seconds, the driver's default `serverSelectionTimeoutMS`, it fails with
exit 1, before doing anything and without a report:

```txt
time=… level=INFO msg=connecting db=app
Error: check the changelog format version: server selection error: context deadline exceeded, current topology: { Type: ReplicaSetNoPrimary, Servers: [{ Addr: db1.example.net:27017, Type: Unknown, Last error: dial tcp 10.0.0.12:27017: connect: connection refused }, ] }
```

Each server's `Last error` says why: a refused or timed-out connection (a
firewall, an IP allow list, the wrong port), a TLS handshake error, or no
such host.

The driver connects to the members the replica set advertises, not only the
hosts in the URI: a `Type: ReplicaSetNoPrimary` with every member
unreachable, while the host in the URI answers, means the runner can't reach
the names the members advertise (`rs.conf()` lists them), such as a
container's name outside its network. Connect from where those names
resolve, or, for a one-member replica set, add `directConnection=true` to the
URI.

To fail faster, set `serverSelectionTimeoutMS` in the URI, for example
`serverSelectionTimeoutMS=10000`. It bounds every operation's wait for a
server, not only the first: keep it longer than an election takes, so a run
waits through a failover instead of failing.

## The server isn't a replica set

On a standalone `mongod`, the first data migration fails, since it needs a
transaction:

```txt
Error: migration 2026-11-01-001-seed-account failed: fence migration lock: (IllegalOperation) Transaction numbers are only allowed on a replica set member or mongos
```

migrune needs transactions, so a replica set, even of one member (see
[Requirements](../README.md#requirements)). For local development, start
`mongod` with `--replSet rs0` and run `rs.initiate()` once, as
[`examples/docker-compose.yaml`](../examples/docker-compose.yaml) does.

## Authentication fails, or a privilege is missing

A user name or password the server refuses fails the connection, with exit 1:

```txt
Error: check the changelog format version: connection() error occurred during connection handshake: auth error: sasl conversation error: unable to authenticate using mechanism "SCRAM-SHA-256": (AuthenticationFailed) Authentication failed.
```

Check the credentials, that a password with reserved characters (`@`, `:`,
`/`, `%`) is percent-encoded in the URI, and `authSource`: users live in the
database named in the URI's path, or `admin` without one, whatever `--db`
says. Add `authSource=admin` when the user was created there. For
`MONGODB-AWS`, see [AWS IAM](running.md#aws-iam).

A user the server accepts but that lacks a privilege fails on the first
command it isn't allowed to run. For plan and apply, that is usually taking
the lock, with exit 1 and no report:

```txt
Error: acquire migration lock: (Unauthorized) not authorized on app to execute command { findAndModify: "migrations_lock", … }
```

plan and apply need `readWrite` on the target database. See
[credentials](running.md#credentials) for the roles each command needs.

## A migration errored

apply stops at the first migration that fails: it records it as `Errored` in
the changelog, reports it as `errored` and the ones after it as `skipped`, and
exits 1. The migrations before it stay applied.

An `Errored` migration is still pending (only a `Finished` entry marks a
migration as applied) and its file isn't locked, so you **fix it in place,
under the same id**, and run again.

1. **Find out what failed.** The report's `error` column and the logs name the
   step and the reason:

   ```txt
   2026-11-05-001-set-trial-days  data  errored  13ms  /steps/0 (updateMany on plans, second run): not idempotent: running the migration again modified 2 documents; …
   ```

   The `Errored` changelog entry stores the same messages in `errors`
   (`{step, message}`), with the migration as it ran:

   ```js
   db.migrations_changelog.find({migrationId: "<id>", status: "Errored"}).sort({createdAt: -1}).limit(1)
   ```

2. **Check what was left behind.**
   - Data: nothing; the transaction aborted.
   - Index: the steps before the failed one stay done. Running them again
     does nothing.
   - Merge: the steps before the failed one stay done. A step that failed on
     `typeChangedCount`, `duplicateOnCount`, `nullOnCount`, `arrayOnCount` or
     `idMismatchCount` wrote nothing. One that failed
     while `$merge` ran may have written part of its documents; once fixed,
     the next run compares again and completes it. One that failed on
     `diffCountAfter` wrote everything and doesn't converge: running it again
     writes again, so fix the pipeline (see
     [shape the output](writing-migrations.md#shape-the-output)) and check
     the data it wrote.

3. **Fix the file and run again**, keeping its name and `id`. For example,
   make a step idempotent by filtering on the target state:

   ```diff
   -      "filter": {},
   -      "update": { "$inc": { "trialDays": 14 } }
   +      "filter": { "trialDays": { "$exists": false } },
   +      "update": { "$set": { "trialDays": 14 } }
   ```

   Then lint, plan and apply as usual. In CI that means a new commit and a new
   pipeline, so the plan covers the fixed file. The old `Errored` entry
   stays as a record; the new `Finished` entry marks the migration applied.

If the migration shouldn't run at all, delete its file instead: a migration
with only `Errored` entries isn't applied.

## A run was interrupted

On the first SIGINT or SIGTERM (Ctrl+C, a cancelled job, a pod eviction),
migrune stops gracefully and exits 130 or 143:

- no new migration starts;
- the migration in progress is reported as `interrupted` and the rest as
  `skipped`. A data migration's transaction is aborted; an index build is
  left to finish on the server; a merge's aggregate is cancelled;
- no changelog entry is written, so the migration stays pending; but a
  data migration whose commit was already sent lets it finish, for up to 30
  seconds, and is reported as `applied` if it committed;
- the lock is released if this runner still owns it, within 5 seconds;
- the report is still printed.

A second signal exits at once with the first signal's code; every step above is safe to
cut short. SIGKILL can't be caught: the server aborts the open transaction on
its own, and the lock expires.

Nothing needs fixing: run plan and apply again. The next run's identical
`createIndex` waits for a build still in progress or does nothing, and a
half-done merge is finished.

## The lock is held

A run that can't take the migration lock fails with exit 3 before doing
anything, and prints no report:

```txt
Error: migration lock is held by runner "ci-42-1-AB12CD34" (applying 2026-11-05-001-set-trial-days), last heartbeat 2026-11-05T10:12:00Z
```

Another run is in progress: wait for it, or let migrune wait with
`--lock-wait` (for example `--lock-wait 10m`), which retries until the lock is
free or the duration has passed. A CI job can also retry on exit code 3. If
the holder died without releasing the lock (SIGKILL, a lost machine), the
lock expires on its own once its `expiresAt` has passed: the longer of 30
seconds and the holder's `--lock-ttl` (90 seconds by default) after its last
heartbeat. A runner that runs again after a crash waits for that too, even
with the same `--runner-id`. See
[the lock](reference/changelog-and-lock.md#lock-migrations_lock).

Don't delete the lock document by hand unless you are sure its holder is
gone. Even then, a runner that lost its lock can't commit a data migration
(the fence aborts its transaction), but it could still be running index or
merge steps.

## The lock was lost

A run that no longer owns the migration lock stops at once, and exits 1. The
migration in progress, if any, is reported as `errored` with the error
`migration lock lost`, the ones after it as `skipped`, and the logs say why:

```txt
time=… level=ERROR msg="migration lock lost: another runner took it over" runner=ci-42-1-AB12CD34
Error: migration 2026-11-05-001-set-trial-days failed: migration lock lost
```

The lock passes to another runner only once it has expired, so this runner
went without a successful heartbeat for longer than the expiry: the
connection to the primary was cut, the server was overloaded, or the process
was paused. When the runner notices it first, before another one takes over,
the run stops the same way, after a failed heartbeat or before its next index
or merge step, with `migration lock lost:` followed by the heartbeat's error,
or by `no heartbeat succeeded within the lock expiry, less a 10s margin` (see
[heartbeat](reference/changelog-and-lock.md#heartbeat)). Deleting the lock
document by hand has the same effect.

No transaction commits without the lock (the
[fence](reference/changelog-and-lock.md#fence)): a data migration's changes
are aborted, no changelog entry is written, and the migration stays pending.
An index migration's or a merge migration's steps done before the loss stay
done, and one already sent may still finish on the server; the next run
handles them as it does after an [interrupted run](#a-run-was-interrupted).

Find the runner that holds the lock now, if any, and let it finish:

```js
db.migrations_lock.find()
```

Then plan and apply again. If heartbeats keep failing, look at the
network between the runner and the primary, and the logs' `migration lock
heartbeat failed, retrying` warnings before the loss; raising `--lock-ttl`
gives a slow heartbeat more time.

## The changelog or lock has a newer format

Every command that connects fails with exit 1 before doing anything, and
prints no report:

```txt
Error: the changelog has format v2, newer than this migrune understands (v1): upgrade migrune
Error: the migration lock has format v2, newer than this migrune understands (v1): upgrade migrune
```

A newer migrune has written to this database, and this one can't read what
it wrote. It happens when a pipeline runs an older migrune than another one
does, or after rolling a CI image back. Run the migrune version the other
pipelines use, or a later one; going back to an older one isn't supported. See
[format versions](reference/changelog-and-lock.md#format-versions).

Don't edit or delete the newer entries or lock to get past it.

## An applied migration was edited

plan and apply fail before running anything, report the migration as
`errored` with both checksums, and report every pending migration as
`skipped`:

```txt
Error: applied migrations are immutable, but the file of <id> changed since it was applied; revert the edit and write a new migration (--mutable re-runs edited migrations, for local use only)
```

Revert the edit (any byte counts, whitespace included) and put the change in
a new migration. Locally, see [`--mutable`](running.md#locally-with---mutable).

## Applied but missing

A migration is applied but missing when the changelog has a `Finished` entry
for it and its file isn't in `--dir`. plan and apply exit 1 before running
anything, report every pending migration as `skipped` and list the missing
ones. With a connection, lint reports `applied-missing` too.

```txt
Applied but missing (Finished in the changelog, no file):
  2026-11-02-001-plans-currency-index: Finished 2026-11-08T00:11:27Z by ci-42-1-AB12CD34: "Index plans by currency"; the file is not in --dir. The full migration is stored in migrations_changelog (migrationId: "2026-11-02-001-plans-currency-index")

Error: applied migrations must keep their file, but the file of 2026-11-02-001-plans-currency-index is missing from the migrations directory; restore it (removing a file doesn't undo its migration), or, to retire a migration on purpose, delete its changelog entries by hand
```

Removing a file never undoes a migration; migrune stops so nobody believes
it did. The usual causes: the file was reverted or deleted; it was renamed
(one missing migration and one new one); or `--dir` points at the wrong
directory, or the checkout is older than the database.

### Fix 1: restore the file

Bring the file back byte for byte: a different checksum fails as an edited
migration. Restore it from the commit before the one that removed it, and
compare checksums:

```sh
git log --diff-filter=D --format=%H -1 -- migrations/<id>.json
git checkout <commit>^ -- migrations/<id>.json
sha256sum migrations/<id>.json
```

```js
db.migrations_changelog.find({migrationId: "<id>", status: "Finished"}, {checksum: 1}).sort({createdAt: -1}).limit(1)
```

If the file was renamed, rename it back. The latest `Finished` entry stores
the migration as applied (drop the projection above to see it), which tells
you what it did, but a file rebuilt from it has another checksum: restore from
version control.

### Fix 2: retire the migration

If the file should stay gone, delete its changelog entries by hand, in every
environment where it was applied, with a user allowed to write to
`migrations_changelog`. The entries are your audit trail, so save them first:

```sh
mongoexport --uri "$MIGRUNE_URI" --db app --collection migrations_changelog \
  --query '{"migrationId": "<id>"}' --out <id>.changelog.json
```

```js
db.migrations_changelog.deleteMany({migrationId: "<id>"})
```

This changes no data: whatever the migration did stays done. To undo it,
write a new migration that does the opposite.

To rename a migration on purpose, retire the old id this way. The file under
its new name is then pending and runs again, which is harmless when it is
idempotent, but documents written since the first run may match its filters
again, so read the plan before applying.

## A merge warns about a collation

plan and apply log a warning for each merge step that reads a collection with
a non-simple default collation, such as a case-insensitive one; when the
target's unique index on the `on` fields has it too, the same warning lists
what the comparison can't check through it. The run
continues, but the counts may not show everything `$merge` writes. See
[collations](writing-migrations.md#collations) for the limits and what to do.

## An applied migration did the wrong thing

Don't edit it: write a new migration that corrects the data. See
[why there are no rollbacks](design.md#why-there-are-no-rollbacks).
