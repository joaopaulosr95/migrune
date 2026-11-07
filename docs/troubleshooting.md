# Troubleshooting

The errors below are shown as migrune prints them by default; with
`--log-format json`, the same text is the `msg` of a
[JSON log record](reference/env-and-exit-codes.md#exit-codes). The queries
run in `mongosh` against the target database.

## The server can't be reached

Every command that connects waits for a server it can use. After 30
seconds, the driver's default `serverSelectionTimeoutMS`, it fails with
exit 1, before doing anything:

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
server, not only the first: keep it longer than an election takes, so a
command waits through a failover instead of failing.

## Authentication fails, or a privilege is missing

A user name or password the server refuses fails the connection, with exit 1:

```txt
Error: check the changelog format version: connection() error occurred during connection handshake: auth error: sasl conversation error: unable to authenticate using mechanism "SCRAM-SHA-256": (AuthenticationFailed) Authentication failed.
```

Check the credentials, that a password with reserved characters (`@`, `:`,
`/`, `%`) is percent-encoded in the URI, and `authSource`: users live in the
database named in the URI's path, or `admin` without one, whatever `--db`
says. Add `authSource=admin` when the user was created there.

A user the server accepts but that lacks a privilege fails on the first
command it isn't allowed to run, with exit 1. lint with a connection needs
`read` on the target database.

## The changelog or lock has a newer format

Every command that connects fails with exit 1 before doing anything:

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

With a connection, lint reports a `checksum` error with both checksums:

```txt
migrations/<id>.json: error: checksum: the file changed after the migration was applied: the changelog has checksum <changelog checksum>, the file <file checksum>. Applied migrations are immutable: revert the edit and write a new migration
```

Revert the edit (any byte counts, whitespace included) and put the change in
a new migration.

## Applied but missing

A migration is applied but missing when the changelog has a `Finished` entry
for it and its file isn't in `--dir`. With a connection, lint reports it as
an `applied-missing` error, with what the changelog stored:

```txt
migrations/2026-11-01-001-seed-currencies-and-plans.json: error: applied-missing: Finished 2026-11-08T00:11:27Z by ci-42-1-AB12CD34: "Seed the reference currencies and the two subscription plans"; the file is not in --dir. The full migration is stored in migrations_changelog (migrationId: "2026-11-01-001-seed-currencies-and-plans"). Restore the file: removing a file doesn't undo its migration; to retire a migration on purpose, delete its changelog entries by hand
```

Removing a file never undoes a migration; migrune reports it so nobody
believes it did. The usual causes: the file was reverted or deleted; it was renamed
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
its new name is then a new migration and runs again, which is harmless when
it is idempotent, but documents written since the first run may match its
filters again.

## An applied migration did the wrong thing

Don't edit it: write a new migration that corrects the data. See
[why there are no rollbacks](design.md#why-there-are-no-rollbacks).
