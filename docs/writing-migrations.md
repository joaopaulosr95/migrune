# Writing migrations

A migration is a JSON file in your migrations directory. This page covers the
three kinds and how to make each one pass migrune's checks. The exact format
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
| merge | `aggregate` ending in `$merge`          | Logic that reads other collections, backfills too big for a transaction |

There is no `insert*`, `deleteOne`, `replaceOne` or `bulkWrite`: they aren't
idempotent.

## Rules for every migration

- **Stay compatible with the running code.** Migrations run before the
  deploy, while the old code still serves traffic, and stay applied if the
  deploy rolls back. Only add or fill data the current code tolerates. To
  rename a field or change its type, add a new field in one release and stop
  using the old one in a later release (expand, then contract).
- **Be idempotent.** A migration must change nothing when run a second time.
  migrune checks this for data and merge migrations; index steps are
  idempotent in MongoDB itself.
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
versions in cache and conflict with live writes. For many documents, or data
from another collection, write a merge migration.

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

## Merge migrations

A merge migration runs an aggregation pipeline that ends in `$merge`. `$merge`
can't run in a transaction, so migrune checks it by counting: the documents
`$merge` would change before it runs, and the documents that still differ
after, which must be 0. Each count runs the pipeline in full: on a large
collection, see [large collections](running.md#large-collections) for what
plan and apply cost.

```json
{
  "id": "2026-11-05-004-denormalize-project-owner-name",
  "description": "Copy the owner's name onto projects that don't have it",
  "steps": [
    {
      "collection": "projects",
      "operation": "aggregate",
      "pipeline": [
        { "$match": { "ownerName": { "$exists": false } } },
        { "$lookup": { "from": "users", "localField": "ownerId", "foreignField": "_id", "as": "owner" } },
        { "$project": { "ownerName": { "$first": "$owner.name" } } },
        { "$merge": { "into": "projects", "on": "_id", "whenMatched": "merge", "whenNotMatched": "discard" } }
      ]
    }
  ]
}
```

Each step has `collection` (the collection the pipeline reads),
`"operation": "aggregate"` and `pipeline`. The schema and lint enforce:

- `$merge` is the only one (schema) and the last stage (`merge-last`).
- `$merge` takes `into` (a collection in the same database), optionally `on`
  (a field or an array of fields; default `_id`), and exactly
  `"whenMatched": "merge"` and `"whenNotMatched": "discard"` (schema).
  Merge migrations update existing documents; they never insert or replace
  them.
- No `$out` stage (schema): it replaces the whole collection and loses
  concurrent writes. No `$out` or `$merge` in `$lookup.pipeline`,
  `$unionWith.pipeline` or `$facet`, at any depth (`nested-out-merge`).
- No `$rand`, `$$NOW`, `$$CLUSTER_TIME`, `$function` or `$accumulator`
  anywhere in the pipeline (`volatile-merge`): their output changes from one
  run to the next, or can't be checked.

With an `on` other than `_id`, MongoDB requires a unique index on those
fields in the target, or `$merge` fails with error 51183. plan and apply check
it before writing anything, and fail the step without one, naming the fields
and the collation it needs. An index qualifies when it is:

- unique, and not partial (sparse or hidden is fine);
- on exactly the `on` fields, in any order and direction: `on: ["sku",
  "region"]` takes `{"region": 1, "sku": 1}`, not `{"sku": 1}` or
  `{"sku": 1, "region": 1, "at": 1}`;
- of the collation the pipeline runs with: the default collation of the
  collection the step reads, or the simple collation when it has none, since
  migrune sets none. A case-insensitive index doesn't qualify for a
  pipeline reading a collection without a default collation.

Create the index in an earlier index migration: plan sees the indexes the
pending index migrations before the merge would create. When such an index
gives a collation that plan can't compare with the collection's, which lists
every default, plan logs a warning that apply may fail. A file can hold
several aggregate steps, run in order.

### Shape the output

`whenMatched: "merge"` sets the output document's top-level fields on the
matched target and leaves its other fields alone. So output only the `on`
fields and the fields to write (`$project` keeps `_id` unless you remove it).
A nested document or array in the output replaces the target's whole value,
so a nested document must carry every non-null field of the target's: one it
drops fails the migration (see below).
Filtering out documents already done (`$match` on the target state) isn't
required, since unchanged documents count as no diff, but keeps re-runs cheap.

A merge must converge: run again on its own result, it must give the same
output. So compute the output from fields the merge doesn't write: copying
`name` into `label` converges, but `{"$add": ["$n", 1]}` written back to `n`
gives a new value on every run. migrune can't prove a merge converges before
writing: plan sees the same output either way, and only apply finds out,
after `$merge` wrote, when documents still differ (`diffCountAfter`). lint
rejects the operators it knows change on every run (`volatile-merge`), and
the comparison fails when two output documents share an `on` value that
matches a target document (`duplicateOnCount`), since `$merge` would then
write the same target twice and keep whichever came last.

### How the comparison works

For each step, migrune runs the pipeline without `$merge` and compares every
output document with the target document whose `on` fields equal its own, as
`whenMatched: "merge"` would write it: only the output's top-level fields
count, fields absent from the output are ignored, and the comparison is
type-strict (int `1`, long `1` and double `1.0` differ). Strings compare byte
by byte, with the simple collation, even when the collection has a default
collation: under a case-insensitive one, a change of case still counts. That
has limits: see [collations](#collations).

| Count              | What it counts                                                                 | Effect                                       |
| ------------------ | ------------------------------------------------------------------------------ | -------------------------------------------- |
| `diffCountBefore`  | Matched documents with at least one output field missing from the target, or of another value or type | Reported, with up to five samples per step, type changes first: field names and types, and values only with `--sample-values` |
| `unmatchedCount`   | Output documents with no target document; `$merge` discards them               | Above 0 logs a warning: the `on` fields are probably wrong |
| `typeChangedCount` | Matched documents where an output field changes the BSON type of an existing, non-null target field, or of a value nested in it, or drops a non-null nested field | Above 0 fails the migration, in plan and apply, before anything is written |
| `duplicateOnCount` | Output documents whose `on` fields equal another output document's and match a target document. Duplicates with no target are left out: `$merge` discards them | Above 0 fails the migration, in plan and apply, before anything is written |
| `nullOnCount`      | Output documents with an `on` field that is null or missing: `$merge` would match them with a target document whose field is null or missing, if there is one | Above 0 fails the migration, in plan and apply, before anything is written |
| `arrayOnCount`     | Output documents with an `on` field that is an array, matched or not: `$merge` rejects them (error 51185), part way through | Above 0 fails the migration, in plan and apply, before anything is written |
| `idMismatchCount`  | When the `on` fields don't include `_id`: matched documents whose output `_id` differs from the target's. `$merge` can't change an `_id`, and would fail part way through. Leave `_id` out of the output with `{"$project": {"_id": 0, …}}` | Above 0 fails the migration, in plan and apply, before anything is written |
| `collationOnlyMatchCount` | Only when the unique index on the `on` fields has a non-simple collation: output documents with no exact match, so in `unmatchedCount` too, that match a target through that collation (see [collations](#collations)) | Above 0 logs a warning saying what the server's version does with them |
| `diffCountAfter`   | Apply only: documents that still differ after `$merge`                          | Above 0 fails the migration: it didn't settle |

A field missing or null in the target takes any type and counts only as a
diff. The reverse, a non-null field the output sets to null, is a type
change. plan stops after the first comparison and writes nothing; apply then
runs the full pipeline and compares again. The counts are summed over the
steps that ran.

Nested values are compared with MongoDB's `$eq` plus their BSON size, since
aggregation can't compare bytes, and `$eq` compares numbers by value whatever
their type. On top of that, migrune checks nested types:

- The check runs on each matched output field that has the same top-level
  type as the target's and is a document or an array. Values pair up by field
  name in documents and by index in arrays. A document is checked whether or
  not its value changed; an array only when it is equal to the target's under
  `$eq`, since once elements are added, removed or moved the same index no
  longer holds the same element.
- It compares the BSON type of every nested value with the target's, down to
  5 levels. Level 0 is the field itself, always compared; level 1 is its
  direct children; level `n` the children of level `n - 1`. So
  `{a: {b: {c: {d: {e: {f: long 5}}}}}}` against the same with `f` a double
  is checked (`f` is at level 5), but one level deeper isn't.
- A nested value of another type is a type change, as at the top level: a
  nested int turned into a long, or a long into a double of the same value,
  fails the migration. A nested target value that is null or missing takes any
  type.
- A nested field the target has, not null, and the output document lacks is
  a type change too, to type `missing`: `$merge` would drop it, since it
  replaces the whole top-level value. `{address: {city: "Lisbon"}}` written
  over `{address: {city: "Lisbon", street: "Rua Augusta"}}` fails with
  `type changed at address.street (string -> missing)`. A nested field that is
  null in the target may be dropped.
- Below the depth, values fall back to `$eq` plus BSON size: a nested int and
  a nested long or double of the same value differ (a diff, not a type
  change), while a nested long and double of the same value compare equal, so
  `$merge` can rewrite one as the other unnoticed.
- A document that changes in value is still checked: `{note: "x", amount: int
  10}` to `{amount: long 10}` is a type change at `amount`. An array that
  changes in value isn't: `[{qty: long 2, sku: "a"}]` to `[{qty: double 2.0}]`
  is only a diff, and so is anything nested in it.

So don't change a field's type in place, even nested: write the new type to a
new field, move the code to it, and drop the old field later. Watch numbers
computed by the pipeline: `$multiply` or `$divide` can turn an int into a
double.

plan sees the database as it is now, not what pending data migrations before
the merge would write, since plan aborts them:

```txt
2026-11-05-004-denormalize-project-owner-name  merge  applied  45ms
  merge: diffCountBefore 3, unmatchedCount 0, typeChangedCount 0, duplicateOnCount 0, nullOnCount 0, arrayOnCount 0, idMismatchCount 0
  differs at /steps/0 {"_id":{"$oid":"…"}}: ownerName: missing -> string
```

A merge that fails on `typeChangedCount`, `duplicateOnCount`, `nullOnCount`,
`arrayOnCount` or `idMismatchCount` writes nothing for that step. One that was interrupted, or failed while `$merge` ran, can be
half done: that is safe as long as the running code copes with old and new
data, which every migration must ensure anyway, and running it again
completes it, if it converges. One that failed on `diffCountAfter` doesn't
converge: it already wrote, and running it again would write again. Fix the
pipeline so it computes its output from fields it doesn't write, check what
it wrote, and repair the data with a new migration if needed.

### Collations

An aggregate runs with a single collation. migrune sets none, so `$merge`
runs the pipeline with the default collation of the collection the step
reads. The comparison runs the same pipeline with the simple collation, so
that strings compare byte by byte, and can't also run it with the
collection's. When the collection or the target's unique index has a
non-simple collation, such as a case-insensitive one, the counts can miss
what `$merge` does. plan and apply then log one warning for each step,
covering both cases below, and carry on.

**The collection the step reads has a non-simple default collation.** Every
expression that compares strings, in any stage, compares under it when
`$merge` runs: `$match`, `$group`, `$sort` and `$lookup`, but also `$eq`,
`$in` or `$cond` in an `$addFields`. So the output plan previews and the
before and after counts are computed from can differ from the output
`$merge` writes. migrune warns whatever the pipeline's stages are. A
collection that doesn't exist, has no default collation or has
`{"locale": "simple"}` doesn't warn.

**The unique index on the `on` fields has a non-simple collation.** It has
the same collation as the collection the step reads, or `$merge` fails (see
[merge migrations](#merge-migrations)), and `$merge` matches through it. The
step's warning then also lists these limits:

- `duplicateOnCount` groups `on` values under the simple collation, so it
  misses output documents such as `"ABC"` and `"abc"` that both match a
  target `"abc"` through the index's collation.
- An output value that matches a target only through the collation, say
  `"ABC"` against `"abc"`, counts as unmatched in the comparison, so its other
  fields get no diff and no type check. From MongoDB 8.3, `$merge` matches it
  in apply and writes it, rewriting the target's `"abc"` to `"ABC"`. MongoDB
  7.0 to 8.2 discard it, as the comparison predicts: before 8.3, `$merge`
  didn't pass the collation on to the updates it runs
  ([SERVER-115813](https://jira.mongodb.org/browse/SERVER-115813), with no
  backport listed).
- To show how many there are, plan and apply also run the pipeline with the
  index's collation, look each output document up as `$merge` would, and
  count those that match only through the collation in
  `collationOnlyMatchCount`. Above 0, they log another warning naming the
  server's version and what it does with them: merge and rewrite them (8.3
  and later) or discard them (7.0 to 8.2).
- `collationOnlyMatchCount` leaves out an `on` value that is an embedded
  document whose strings differ only by the collation.

Output `on` values exactly as the target stores them, or give the collection
and the index the simple collation. Comparing byte by byte while the pipeline
runs with the collection's collation would lift these limits; migrune
doesn't do that yet.

## What doesn't belong in a migration

Migrations may seed **reference data** that is the same in every environment
(currencies, a price table). They are not for **fixtures**, such as demo
accounts or test users: keep those in a seed script your application owns.
See [design](design.md#why-fixtures-are-out-of-scope).
