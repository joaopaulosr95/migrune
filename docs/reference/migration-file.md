# Migration file

A migration is one JSON file in the migrations directory. This page
describes the format; [writing migrations](../writing-migrations.md) explains
how to write one.

## Which files are read

- Every `*.json` file directly in the directory. Subdirectories and other files
  are ignored. migrune warns about a subdirectory, and about a
  file whose extension is close to `.json`, such as `.JSON`, `.json5` or
  `.jsonc`: it may be a migration that would never run.
- Each must be a regular file of at most 1 MiB (1,048,576 bytes). A
  symbolic link or a larger file fails to load: the checksum then always
  covers the file in the directory.
- The file name, without `.json`, is the migration **id**:
  `YYYY-MM-DD-NNN-slug`. The date must be a real date, `NNN` is three
  digits, and the slug is lower-case letters and digits in words separated
  by single dashes. A file without a valid date prefix fails to load.
- Migrations run in id order, so the date and `NNN` order them.
- A file's date only orders it: a file dated after today runs like any
  other.
- The **checksum** of a migration is the SHA-256, in lowercase hex, of the
  file's bytes. Any byte counts, whitespace and line endings included, and
  a file with a byte order mark fails to load. Keep
  git and formatters from rewriting migration files: see
  [writing migrations](../writing-migrations.md#rules-for-every-migration).

## Top-level fields

| Field         | Type   | Required | Description                                                         |
| ------------- | ------ | -------- | ------------------------------------------------------------------- |
| `id`          | string | yes      | The file name without `.json`. A different value is an error.       |
| `description` | string | yes      | What the migration does, not empty.                                 |
| `steps`       | array  | yes      | The operations, at least one, run in order.                         |

No other field is allowed, and no object may repeat a key: JSON parsers
disagree on which value wins, so the file fails to load, naming the key's
JSON pointer. Values anywhere in the file are
[MongoDB Extended JSON](https://www.mongodb.com/docs/manual/reference/mongodb-extended-json/),
canonical or relaxed: `{"$oid": "..."}`, `{"$date": "..."}`,
`{"$numberLong": "..."}`, `{"$numberDecimal": "..."}` and so on. A malformed
value, such as an `$oid` that isn't 24 hex digits, is an error.

## Kind

Every step's `operation` is `updateOne`, `updateMany` or `deleteMany`, which
makes the migration's **kind** `data`: all its steps run in one transaction.

There is no `insert*`, `deleteOne`, `replaceOne` or `bulkWrite`: inserts and
`deleteOne` aren't idempotent.

## Reserved collections

A migration must not name migrune's own collections, `migrations_changelog`
and `migrations_lock`, or any collection whose name starts with `system.`:
a step could otherwise forge or delete changelog entries, break the lock, or
copy them elsewhere. This applies wherever a collection is named:

- a step's `collection`: the schema refuses it;
- the `from` of `$lookup` and `$graphLookup`, the collection of `$unionWith`
  (its string form or its `coll`), and the target of any `$out`, at any
  depth of a pipeline, `$facet` and nested pipelines included: the
  file fails to load.

## Data steps

| Field        | Type            | Required                     | Description                                                     |
| ------------ | --------------- | ---------------------------- | --------------------------------------------------------------- |
| `collection` | string          | yes                          | The collection, not empty.                                      |
| `operation`  | string          | yes                          | `updateOne`, `updateMany` or `deleteMany`.                      |
| `filter`     | object          | yes                          | The query. `{}` matches every document.                         |
| `update`     | object or array | for updates; not for deletes | An update document, or an update pipeline with at least one stage. |
| `options`    | object          | no                           | See below.                                                      |

Options migrune passes to the driver:

| Operation                   | Options                                                   |
| --------------------------- | --------------------------------------------------------- |
| `updateOne`, `updateMany`   | `upsert`, `arrayFilters`, `collation`, `hint`, `let`      |
| `deleteMany`                | `collation`, `hint`, `let`                                |

The schema rejects `$merge` and `$out` as stages of an update pipeline, as MongoDB does.

## JSON Schema

migrune validates every file against a [JSON Schema](https://json-schema.org/draft/2020-12/schema)
(draft 2020-12), embedded in the binary from
[`internal/migration/schema.json`](../../internal/migration/schema.json).
Point your editor at it for completion and inline errors. It owns
everything it can express: the fields of each step, the allowed operations,
and no `$out` or `$merge` stage in an update
pipeline. The checks a schema can't express, such as Extended JSON values,
the file name's date and the reserved collections a pipeline reads or
writes, are made in code whenever the files are loaded.

[`examples/migrations/`](../../examples/migrations/) has an example migration.
