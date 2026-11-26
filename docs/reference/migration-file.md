# Migration file

A migration is one JSON file in the migrations directory (`--dir`). This page
describes the format; [writing migrations](../writing-migrations.md) explains
how to write each kind, and [lint rules](lint-rules.md) lists every check.

## Which files are read

- Every `*.json` file directly in `--dir`. Subdirectories and other files
  are ignored. lint, plan and apply warn about a subdirectory, and about a
  file whose extension is close to `.json`, such as `.JSON`, `.json5` or
  `.jsonc` (`ignored-entry`): it may be a migration that would never run.
- Each must be a regular file of at most 1 MiB (1,048,576 bytes). A
  symbolic link or a larger file fails to load (`regular-file`,
  `file-size`): the checksum then always covers the file in the directory.
- The file name, without `.json`, is the migration **id**:
  `YYYY-MM-DD-NNN-slug`. The date must be a real date, `NNN` is three
  digits, and the slug is lower-case letters and digits in words separated
  by single dashes. A file without a valid date prefix fails to load.
- Migrations run in id order, so the date and `NNN` order them.
- A file's date only orders it: a file dated after today runs like any
  other.
- The `YYYY-MM-DD-NNN` prefix must be unique in the directory
  (`duplicate-prefix`).
- The **checksum** of a migration is the SHA-256, in lowercase hex, of the
  file's bytes. Any byte counts, whitespace and line endings included, so
  lint requires LF line endings and no UTF-8 byte order mark
  (`line-endings`), and a file with a byte order mark fails to load. Keep
  git and formatters from rewriting migration files: see
  [writing migrations](../writing-migrations.md#rules-for-every-migration).

## Top-level fields

| Field         | Type   | Required | Description                                                         |
| ------------- | ------ | -------- | ------------------------------------------------------------------- |
| `id`          | string | yes      | The file name without `.json`. A different value is an error.       |
| `description` | string | yes      | What the migration does, not empty. Stored in the changelog.        |
| `steps`       | array  | yes      | The operations, at least one, run in order.                         |
| `batchSize`   | integer | no      | Data migrations only, from 1 to 10,000: run the step in [batches](#batched-data-migrations) of this many documents. |

No other field is allowed, and no object may repeat a key: JSON parsers
disagree on which value wins, so the file fails to load, naming the key's
JSON pointer (`duplicate-key`). Values anywhere in the file are
[MongoDB Extended JSON](https://www.mongodb.com/docs/manual/reference/mongodb-extended-json/),
canonical or relaxed: `{"$oid": "..."}`, `{"$date": "..."}`,
`{"$numberLong": "..."}`, `{"$numberDecimal": "..."}` and so on. A malformed
value, such as an `$oid` that isn't 24 hex digits, is an error.

## Kinds

Every step's `operation` decides the migration's **kind**, and a file never
mixes kinds:

| Kind    | Operations                             | Runs                                                                |
| ------- | -------------------------------------- | ------------------------------------------------------------------- |
| `data`  | `updateOne`, `updateMany`, `deleteMany` | All steps in one transaction, twice; the second run must change nothing. With `batchSize`, one transaction per batch. |
| `index` | `createIndex`, `dropIndex`, `collModIndex`, `collModCollection` | Step by step, outside a transaction.                    |
| `merge` | `aggregate`                            | Step by step, outside a transaction; the pipeline ends in `$merge`. |

There is no `insert*`, `deleteOne`, `replaceOne` or `bulkWrite`: inserts and
`deleteOne` aren't idempotent.

## Reserved collections

A migration must not name migrune's own collections, `migrations_changelog`
and `migrations_lock`, or any collection whose name starts with `system.`:
a step could otherwise forge or delete changelog entries, break the lock, or
copy them elsewhere. This applies wherever a collection is named:

- a step's `collection`, and `$merge.into`: the schema refuses them;
- the `from` of `$lookup` and `$graphLookup`, the collection of `$unionWith`
  (its string form or its `coll`), and the target of any `$merge` or `$out`,
  at any depth of a pipeline, `$facet` and nested pipelines included: the
  file fails to load, in lint (`reserved-collection`), plan and apply alike.

## Data steps

| Field        | Type            | Required                     | Description                                                     |
| ------------ | --------------- | ---------------------------- | --------------------------------------------------------------- |
| `collection` | string          | yes                          | The collection, not empty.                                      |
| `operation`  | string          | yes                          | `updateOne`, `updateMany` or `deleteMany`.                      |
| `filter`     | object          | yes                          | The query. `{}` matches every document.                         |
| `update`     | object or array | for updates; not for deletes | An update document, or an update pipeline with at least one stage. |
| `options`    | object          | no                           | See below.                                                      |

Options migrune passes to the driver; any other option fails the migration
when it runs:

| Operation                   | Options                                                   |
| --------------------------- | --------------------------------------------------------- |
| `updateOne`, `updateMany`   | `upsert`, `arrayFilters`, `collation`, `hint`, `let`      |
| `deleteMany`                | `collation`, `hint`, `let`                                |

The schema rejects `$merge` and `$out` as stages of an update pipeline, as MongoDB does.

### Batched data migrations

A data migration with `batchSize` must have exactly one step, an
`updateMany` or a `deleteMany`, without the `upsert` or `collation` option. A file that
breaks one of these rules fails to load, in lint (`schema`), plan and apply
alike, with a message naming the rule, such as:

```txt
/steps: batchSize allows exactly one step, got 2: put each step in a migration of its own
```

`batchSize` on an index or merge migration is refused the same way
(`batchSize is only allowed in data migrations`). How a batched migration
runs, and what it gives up, is in
[batched data migrations](../writing-migrations.md#batched-data-migrations).

## Index steps

| Field        | Type   | Required             | Description                                                                     |
| ------------ | ------ | -------------------- | ------------------------------------------------------------------------------- |
| `collection` | string | yes                  | The collection, not empty.                                                      |
| `operation`  | string | yes                  | `createIndex`, `dropIndex`, `collModIndex` or `collModCollection`.              |
| `keys`       | object | `createIndex` only   | The index keys, in order, at least one.                                         |
| `options`    | object | `createIndex` only   | The index options. `name` is required; every other option is passed to MongoDB as given. |
| `name`       | string | `dropIndex`, `collModIndex` | The name of the index to drop or change.                                 |
| `set`        | object | `collModIndex`, `collModCollection` | The options to change on the index, or the collection's validation options, at least one; see below. |

`collModIndex` runs MongoDB's `collMod` with `index: {name, ...set}`. `set`
takes these fields and no other:

| Field                | Value                                                                              |
| -------------------- | ---------------------------------------------------------------------------------- |
| `expireAfterSeconds` | A JSON integer from 0 to 2147483647: adds a TTL to the index, or changes it.      |
| `hidden`             | `true` hides the index from the query planner; `false` unhides it.                 |
| `prepareUnique`      | `true` only: MongoDB rejects new duplicate keys. Alone in its `set`, as MongoDB requires. |
| `unique`             | `true` only: makes an index that is already `prepareUnique` unique.                |

There is no way to remove a TTL, make an index non-unique or clear
`prepareUnique`. How to use each option is in
[changing an index in place](../writing-migrations.md#changing-an-index-in-place).

`collModCollection` runs MongoDB's `collMod` on the step's collection with
the fields of `set`, which takes these and no other:

| Field              | Value                                                                                 |
| ------------------ | ------------------------------------------------------------------------------------- |
| `validator`        | An object: a `$jsonSchema` document or query operators. `{}` removes the validator.  |
| `validationLevel`  | `off`, `strict` or `moderate`.                                                       |
| `validationAction` | `error` or `warn`.                                                                   |

It takes no `name`, `keys` or `options`. How to use it is in
[changing a collection's validation](../writing-migrations.md#changing-a-collections-validation).

## Merge steps

| Field        | Type   | Required | Description                                                     |
| ------------ | ------ | -------- | --------------------------------------------------------------- |
| `collection` | string | yes      | The collection the pipeline runs on.                            |
| `operation`  | string | yes      | `aggregate`.                                                    |
| `pipeline`   | array  | yes      | The aggregation pipeline, ending in exactly one `$merge` stage. |

The `$merge` stage:

| Field            | Required | Value                                                       |
| ---------------- | -------- | ----------------------------------------------------------- |
| `into`           | yes      | The target collection's name, in the same database, not a [reserved](#reserved-collections) one. |
| `on`             | no       | A field name, or an array of distinct field names, none empty. Default `_id`. |
| `whenMatched`    | yes      | `"merge"`, nothing else.                                    |
| `whenNotMatched` | yes      | `"discard"` skips unmatched documents. `"insert"` creates them and accepts only `on: "_id"`, `on: ["_id"]` or omitted `on`. |

With `insert`, the primary source and each named `$unionWith` source must exist
as ordinary collections distinct from the target. A `$lookup` may read the target.
The primary source and an existing target must use simple collation. An existing
target must be an ordinary, uncapped collection with a unique `_id` identity.
Plan reports the target name and whether it exists. Apply may create it on the
first insert. See [catalog upserts](../writing-migrations.md#catalog-upserts)
for the output requirements and an example.

`$merge` must be the last stage. `$out` is not allowed anywhere, and neither
`$out` nor `$merge` may appear in a nested pipeline (`$lookup.pipeline`,
`$unionWith.pipeline`, `$facet`). The schema checks there is exactly one
`$merge`, its options, and no `$out` stage; lint checks the rest
(`merge-last`, `nested-out-merge`).

## JSON Schema

migrune validates every file against a [JSON Schema](https://json-schema.org/draft/2020-12/schema)
(draft 2020-12), embedded in the binary from
[`internal/migration/schema.json`](../../internal/migration/schema.json).
Point your editor at it for completion and inline errors. It owns
everything it can express: the fields of each step, the allowed operations,
one kind of migration per file, the `$merge` options and no `$out` stage, so
each problem is reported once, as a `schema` finding in lint. The checks a
schema can't express, such as the position of `$merge`, nested pipelines, Extended
JSON values, the file name and the prefix, are [lint](lint-rules.md) code;
the reserved collections a pipeline reads or writes are also checked when
plan and apply load the files. So are the rules of
[`batchSize`](#batched-data-migrations) beyond its range and kind (one step,
`updateMany` or `deleteMany`, no `upsert` or `collation`), reported as `schema` findings.

[`examples/migrations/`](../../examples/migrations/) has one migration of each
kind.
