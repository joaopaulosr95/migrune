# Writing migrations

A migration is a JSON file in your migrations directory. This page covers how
to write one and make it pass migrune's checks. The exact format
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

Steps are `updateOne`, `updateMany` and `deleteMany`, for backfills that fit
in a transaction, value renames, cleanups and seeds: these are data
migrations. There is no `insert*`, `deleteOne`, `replaceOne` or `bulkWrite`:
they aren't idempotent.

## Rules for every migration

- **Stay compatible with the running code.** Migrations run before the
  deploy, while the old code still serves traffic, and stay applied if the
  deploy rolls back. Only add or fill data the current code tolerates. To
  rename a field or change its type, add a new field in one release and stop
  using the old one in a later release (expand, then contract).
- **Be idempotent.** A migration must change nothing when run a second time.
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
```

## Data migrations

All the steps run in file order in one transaction. Each step has:

- `collection`;
- `operation`: `updateOne`, `updateMany` or `deleteMany`;
- `filter`: a query, required (`{}` matches everything);
- `update`, for updates only: an update document (`{"$set": …}`) or an update
  pipeline (an array of stages such as `$set`, `$unset`, `$replaceWith`);
- `options`, optional: `upsert`, `arrayFilters`, `collation`, `hint` and
  `let` for updates; `collation`, `hint` and `let` for `deleteMany`.

To keep it idempotent:

- **Filter on the target state.** `{"archived": {"$exists": false}}` matches
  nothing once the field is set, which also keeps a re-run cheap.
- **Use `$set`, not `$inc`, `$push` or `$mul`**, unless the filter excludes
  the documents already changed.
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

lint warns about a `deleteMany` or `updateMany` with the filter `{}`
(`empty-filter`).

## What doesn't belong in a migration

Migrations may seed **reference data** that is the same in every environment
(currencies, a price table). They are not for **fixtures**, such as demo
accounts or test users: keep those in a seed script your application owns.
See [design](design.md#why-fixtures-are-out-of-scope).
