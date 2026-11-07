# Writing migrations

A migration is a JSON file in your migrations directory. This page covers the
two kinds and how to make each one pass migrune's checks. The exact format
is in the [migration file reference](reference/migration-file.md), and every
lint rule is in [lint rules](reference/lint-rules.md).

## The file

Name the file `YYYY-MM-DD-NNN-slug.json`, with today's date (UTC) and the
next free `NNN` for that day; the slug is lower-case letters and digits
separated by dashes. Copy the name, without `.json`, into `id`:

```json
{
  "id": "2026-11-05-001-add-archived-flag",
  "description": "Default archived to false on existing projects",
  "steps": [
    {
      "collection": "projects",
      "operation": "updateMany",
      "filter": { "archived": { "$exists": false } },
      "update": { "$set": { "archived": false } }
    }
  ]
}
```

Migrations run in id order. A file's date only orders it: a file dated after
today runs like any other. Values are [MongoDB Extended JSON](https://www.mongodb.com/docs/manual/reference/mongodb-extended-json/),
so types survive: `{"$oid": "…"}`, `{"$date": "2026-11-05T00:00:00Z"}`,
`{"$numberDecimal": "9.90"}`, `{"$numberLong": "42"}`.

The steps' `operation` decides the kind, and a file never mixes kinds:

| Kind  | Operations                              | Use it for                                                          |
| ----- | --------------------------------------- | ------------------------------------------------------------------- |
| data  | `updateOne`, `updateMany`, `deleteMany` | Backfills that fit in a transaction, value renames, cleanups, seeds |
| index | `createIndex`, `dropIndex`              | Creating and dropping indexes                                       |

There is no `insert*`, `deleteOne`, `replaceOne` or `bulkWrite`: they aren't
idempotent.

## Rules for every migration

- **Stay compatible with the running code.** Migrations run before the
  deploy, while the old code still serves traffic, and stay applied if the
  deploy rolls back. Only add or fill data the current code tolerates. To
  rename a field or change its type, add a new field in one release and stop
  using the old one in a later release (expand, then contract).
- **Be idempotent.** A migration must change nothing when run a second time.
  migrune checks this for data migrations; index steps are idempotent in
  MongoDB itself.
- **Never edit an applied migration.** Once a migration is `Finished` in a
  database, plan and apply fail on an edited file. Write a new migration. A
  migration with only `Errored` entries can be fixed in place.
- **Never delete or rename an applied migration's file.** Removing it doesn't
  undo it, and plan and apply stop until it is back (see
  [troubleshooting](troubleshooting.md#applied-but-missing)).
- **Keep the file's bytes as committed.** The checksum covers every byte,
  so a checkout that converts line endings, or a formatter that reflows the
  file or adds a final newline, makes an applied migration look edited in
  every database. Save migrations as UTF-8 without a byte order mark, with
  LF line endings (lint's `line-endings` rule checks both). Tell git not to
  convert them, in `.gitattributes`:

  ```txt
  migrations/*.json -text
  ```

  and leave the directory out of formatters, for example in
  `.prettierignore`, and of editor settings that rewrite files on save.

Check every migration before you commit it:

```sh
migrune lint --dir migrations
MIGRUNE_URI='mongodb://…' migrune plan --dir migrations --db app
```

## Data migrations

All the steps run in file order in one transaction, twice. The migration is
accepted only if the second run modified, upserted and deleted nothing. Each
step has:

- `collection`;
- `operation`: `updateOne`, `updateMany` or `deleteMany`;
- `filter`: a query, required (`{}` matches everything);
- `update`, for updates only: an update document (`{"$set": …}`) or an update
  pipeline (an array of stages such as `$set`, `$unset`, `$replaceWith`);
- `options`, optional: `upsert`, `arrayFilters`, `collation`, `hint` and
  `let` for updates; `collation`, `hint` and `let` for `deleteMany`. Any other
  option fails the migration.

To pass the double run:

- **Filter on the target state.** `{"archived": {"$exists": false}}` matches
  nothing once the field is set, which also keeps a re-run cheap.
- **Use `$set`, not `$inc`, `$push` or `$mul`**, unless the filter excludes
  the documents already changed. A failure reads
  `not idempotent: running the migration again modified N documents`.
- **Seed reference data with an upsert**: filter on a natural key, put the
  values in `$setOnInsert` and set `"options": {"upsert": true}`. To change
  seeded values later, write a new migration; the `$setOnInsert` one won't
  touch existing documents.

  ```json
  {
    "collection": "currencies",
    "operation": "updateOne",
    "filter": { "_id": "EUR" },
    "update": { "$setOnInsert": { "symbol": "€" } },
    "options": { "upsert": true }
  }
  ```

- **Use `deleteMany` with a precise filter.** There is no `deleteOne`: with a
  filter matching several documents, it deletes a different one on each run.

Keep it small. MongoDB aborts transactions after 60 seconds by default
(`transactionLifetimeLimitSeconds`), and large transactions hold old document
versions in cache and conflict with live writes. For many documents, split
the work across several migrations, each filtering on a part of the
documents.

plan runs the migration twice and aborts the transaction, so nothing is
committed. Its report lists, for each step, what the first run matched,
modified, upserted or deleted: check those numbers before you approve, since
a filter typo that matches every document still passes the double run. lint
warns about a `deleteMany` or `updateMany` with the filter `{}`
(`empty-filter`). If a data migration fails in apply, nothing of it was
committed.

## Index migrations

MongoDB can't build indexes on existing collections, or drop them, in a
transaction, so index steps run one by one, outside a transaction, and the
`Finished` entry is written after the last one.

```json
{
  "id": "2026-11-05-003-account-name-ci-index",
  "description": "Replace name_1 with the case-insensitive name_1_ci",
  "steps": [
    {
      "collection": "accounts",
      "operation": "createIndex",
      "keys": { "name": 1 },
      "options": { "name": "name_1_ci", "unique": true, "collation": { "locale": "en", "strength": 2 } }
    },
    { "collection": "accounts", "operation": "dropIndex", "name": "name_1" }
  ]
}
```

- `createIndex` takes `keys`, in order (`{"a": 1, "b": -1}` differs from
  `{"b": -1, "a": 1}`), and `options`, which must include `name`. Any other
  [index option](https://www.mongodb.com/docs/manual/reference/method/db.collection.createIndex/#options)
  goes in `options` too: `unique`, `sparse`, `partialFilterExpression`,
  `expireAfterSeconds`, `collation`, `weights`, `hidden` and so on.
- `dropIndex` takes only the index `name`.

Always name indexes: `dropIndex` and lint work by name, and generated names
change with the keys.

Re-running is safe: creating an index that already exists with the same name
and definition does nothing, dropping a missing index (or on a missing
collection) counts as success, and a unique index build fails on its own if
documents have duplicates. If a step fails, the steps before it stay done; fix
the file and run again.

lint replays every index migration in id order, without a database, and fails
on a `createIndex` MongoDB would reject: a name reused for a different
definition (`index-name-reused`), or the same keys as another live index with
the same `collation`, `partialFilterExpression`, `unique`, `sparse` and
`wildcardProjection` (`index-conflict`). Every index option counts in a
definition, but the few MongoDB ignores: see
[how indexes are matched](reference/lint-rules.md#how-indexes-are-matched).
To change an index's options, create the replacement under a new name, then
drop the old index, as above, or in a later migration once no code names it
(in a `hint`, for instance). Never drop an index and
recreate it under the same name or keys: if the build fails, such as a unique
index on duplicates, the collection is left without either, and lint rejects
it in one migration (`drop-then-create`). If your application declares its
indexes too, compare them with `--expected-indexes` (see
[lint rules](reference/lint-rules.md#expected-indexes)).

A TTL index (`expireAfterSeconds`) deletes documents: every one whose indexed
date is already older than the TTL goes as soon as it is built, and the rest
as they age. lint warns about every TTL index (`ttl-index`), and plan reports
how many documents a new one would delete at once (`wouldExpire`).

plan builds and drops nothing: it reads the existing indexes and lists, step
by step, the index to create, to drop, already present or already absent, and
fails a step MongoDB would reject. It carries what each pending index
migration would do to the next, so a migration that recreates an index an
earlier pending one drops plans as apply would run it. It compares indexes the way lint does, except that on MongoDB 7.0, which
takes a `createIndex` that differs from the index of its name only by
`hidden` as that index, plan reports it `present`; lint, without a database,
rejects it as 8.0 and later do. For a collation, `listIndexes` lists every
default (`caseLevel`, `alternate`, `version` and so on), and plan doesn't
guess them: a field the step and the index both set with different values is
another collation, so plan fails the step as MongoDB would reject it; when
every field the step sets matches but the index has more, plan says the
index differs, or warns on the index to create, that apply may fail. It can't predict a unique
index build failing on duplicates; only apply finds out.

## What doesn't belong in a migration

Migrations may seed **reference data** that is the same in every environment
(currencies, a price table). They are not for **fixtures**, such as demo
accounts or test users: keep those in a seed script your application owns.
See [design](design.md#why-fixtures-are-out-of-scope).
