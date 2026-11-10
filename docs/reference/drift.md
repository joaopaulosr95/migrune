# Index drift

`migrune drift --dir <dir> --db <db>`, with `MIGRUNE_URI` set, compares
the indexes on the server with the indexes the applied migrations leave. It
is read-only: it reads the changelog, `listCollections` and `listIndexes`,
takes no lock and writes nothing, so a user with the `read` role is enough.
When to run it and what it can't see: [detecting index
drift](../running.md#detecting-index-drift).

## What is compared

**The expected side** is the migrations `Finished` in the
[namespace](../running.md#several-apps-on-one-database)'s changelog. Each is
replayed from the copy its latest `Finished` entry stores in its
[`migration`](changelog-and-lock.md#changelog-migrations_changelog) field, which
is the migration as it was applied. The replay is lint's
[index replay](lint-rules.md#index-replay): `createIndex`, `dropIndex` and
`collModIndex`, in id order. `collModCollection` steps aren't replayed, and
the replay's own findings are left to lint. Files in `--dir` with no `Finished`
entry are pending, so drift leaves them out and logs how many there are.

`--dir` is still read, for two checks lint makes online too. A `Finished`
migration whose file changed is a `checksum` error, and one whose file is
missing is an `applied-missing` error (see [lint rules](lint-rules.md#rules)).
Both are still replayed from their stored copy, so the other findings stay
right.

**The actual side** is `listIndexes` on every collection of the database,
except views, `migrations_changelog`, `migrations_lock` and `system.*`.

Indexes are matched as [`--expected-indexes`](lint-rules.md#how-indexes-are-matched)
matches them: by collection, ordered keys and options, never by name.
The `_id_` index is skipped. The one difference is `collation`, which
`listIndexes` expands with every default (`caseLevel`, `alternate`,
`version`, …):

- When the migration gives a collation, every field it gives must have the
  same value on the server. Fields only the server has count as defaults.
- When the migration gives none, the server's index must have none, or
  exactly its collection's default collation, which the index inherits.

Blind spot: an index recreated by hand with a non-default collation field the
migration never set, such as `caseLevel: true`, still matches.

## Findings

| Outcome                                                                         | Severity | Rule            |
| ------------------------------------------------------------------------------- | -------- | --------------- |
| An index the applied migrations leave is not on the server                      | error    | `index-missing` |
| The server's index has the same keys but other options                          | error    | `index-options` |
| An index on the server that no applied migration of the namespace creates       | warning  | `index-extra`   |
| The server's index matches an applied one by definition, under another name     | warning  | `index-renamed` |

`index-extra` is a warning because the index may be legitimate: another
namespace's migrations, the application or a person may have created it. Drop
it if nothing needs it, or create it in a migration.

## Output and exit codes

Findings are printed as [lint's](lint-rules.md#output), one per line, sorted
by subject:

```txt
<collection or file>: <severity>: <rule>: <message>
```

The subject is the collection for the index rules, and the file for
`checksum` and `applied-missing`. With `-o json`, each object has
`collection` instead of `file` for the index rules:

```json
[
  {
    "collection": "sessions",
    "severity": "error",
    "rule": "index-options",
    "message": "index expires_1 has keys {\"expiresAt\": 1} with options {\"expireAfterSeconds\": 60} on the server, but the applied migrations leave index expires_1, created by 2026-11-01-001-indexes, with options {\"expireAfterSeconds\": 3600}"
  }
]
```

drift exits with status 1 if there is at least one error. Warnings alone
exit 0. A missing `--dir`, `--uri` or `--db` exits 2 (see
[exit codes](env-and-exit-codes.md#exit-codes)).
