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
| index | `createIndex`, `dropIndex`, `collModIndex` | Creating, changing and dropping indexes                          |
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

Keep it small. Large transactions hold old document versions in cache and
conflict with live writes, so migrune limits what one can touch: before the
transaction, in plan and in apply, it counts the documents each `updateMany`
and `deleteMany` step's filter matches (with the step's `collation`, `hint`
and `let`), and a step that matches more than 10,000 fails the migration
before anything is written:

```txt
/steps/1 (updateMany on items) matches more than 10,000 documents: one transaction would hold them all; set batchSize (1 to 10,000) to run it in batches
```

`updateOne` steps aren't counted. The count is best effort: it runs outside
the transaction, and before the migration's earlier steps, so documents
those steps or other writers make match can take a step past the limit
unseen. MongoDB also aborts a transaction that runs
longer than `transactionLifetimeLimitSeconds`, 60 seconds by default; the
error then contains `lower batchSize, or set it`. For more documents, set a
[`batchSize`](#batched-data-migrations); for data from another collection,
write a merge migration. Both errors are in
[troubleshooting](troubleshooting.md#a-data-migration-is-too-large-for-one-transaction).

plan runs the migration twice and aborts the transaction, so nothing is
committed. Its report lists, for each step, what the first run matched,
modified, upserted or deleted: check those numbers before you approve, since
a filter typo that matches every document still passes the double run. lint
warns about a `deleteMany` or `updateMany` with the filter `{}`
(`empty-filter`). If a data migration fails in apply, nothing of it was
committed, unless it has a `batchSize`.

### Batched data migrations

A backfill over more documents than one transaction may hold runs in
batches: set `batchSize`, from 1 to 10,000, on a migration with a single
`updateMany` or `deleteMany` step and no `upsert` or `collation` option.
Each batch selects its documents by `_id`, and through a collation, such as a
case-insensitive one, that would also match string `_id`s outside the batch.

```json
{
  "id": "2026-11-05-007-backfill-plan-tier",
  "description": "Default the plan tier to free on every account without one",
  "batchSize": 1000,
  "steps": [
    {
      "collection": "accounts",
      "operation": "updateMany",
      "filter": { "tier": { "$exists": false } },
      "update": { "$set": { "tier": "free" } }
    }
  ]
}
```

migrune reads the `_id`s the filter matches through one cursor, in `_id`
order, outside any transaction (with the step's `hint` and `let`), and runs the step on each batch of `batchSize` of them, in a
transaction of its own: the step
restricted to the batch's `_id`s, the same again, and the check that the
second run changed nothing. Each batch is fenced and committed on its own,
and the last one writes the `Finished` entry, with the counts summed over the
batches and how many there were. The 10,000-document limit doesn't apply: no
batch holds more than `batchSize` documents. Every document is visited once,
whatever the type of its `_id`, even when the update leaves it matching the
filter.

A migration with several dependent steps can't be batched: split it into one
file per step, in the order they must run. Each file is then batched, and
applied, on its own.

What you give up:

- **Atomicity.** A failure in a later batch leaves the earlier ones
  committed, and the changelog says `Errored` with how many: see
  [a batched migration failed part-way](troubleshooting.md#a-batched-migration-failed-part-way).
  Every batch must leave the data valid for the running code, as any
  migration must.
- **plan's full check.** plan counts the matching documents and runs only
  the first batch, so later batches can fail on documents plan never ran.
  Its report says how many it checked:
  `batched: checked 1,000 of 2,314,000 matching documents`.
- **Documents written during the run.** A document inserted, or changed to
  match the filter, while the run goes may not be visited, whatever its
  `_id`: the server may sort the matching `_id`s up front. Deploy the code
  that stops writing such documents first, or catch them with a later
  migration.
- **Very large `_id`s.** Each batch's step carries its `_id`s in an `$in`.
  At a `batchSize` of 10,000, large custom `_id`s, such as long strings or
  documents, can take the command near MongoDB's 16 MB limit; the batch then
  fails with the server's error. Lower `batchSize` if so.

## Index migrations

MongoDB can't build, change or drop indexes on existing collections in a
transaction, nor run `collMod`, so index steps run one by one, outside a
transaction, and the `Finished` entry is written after the last one. A
collection's validation changes with `collMod` too, so its step
(`collModCollection`) belongs to index migrations, and can share a file with
the index steps.

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
- `collModIndex` takes the index `name` and, in `set`, the options to change
  in place: see [changing an index in place](#changing-an-index-in-place).
- `collModCollection` takes, in `set`, the collection's validation options to
  change: see [changing a collection's validation](#changing-a-collections-validation).

Always name indexes: `dropIndex`, `collModIndex` and lint work by name, and
generated names change with the keys.

Re-running is safe: creating an index that already exists with the same name
and definition does nothing, and so does changing an index to the values it
already has; dropping a missing index (or on a missing collection) counts as
success, and a unique index build fails on its own if documents have
duplicates. If a step fails, the steps before it stay done; fix
the file and run again.

lint replays every index migration in id order, without a database, and fails
on a `createIndex` MongoDB would reject: a name reused for a different
definition (`index-name-reused`), or the same keys as another live index with
the same `collation`, `partialFilterExpression`, `unique`, `sparse` and
`wildcardProjection` (`index-conflict`). Every index option counts in a
definition, but the few MongoDB ignores: see
[how indexes are matched](reference/lint-rules.md#how-indexes-are-matched).
`collModIndex` changes `expireAfterSeconds`, `hidden` and `unique` in place. To change anything else, create the replacement under a
new name, then drop the old index, as above, or in a later migration once no
code names it (in a `hint`, for instance). Never drop an index and
recreate it under the same name or keys: if the build fails, such as a unique
index on duplicates, the collection is left without either, and lint rejects
it in one migration (`drop-then-create`). If your application declares its
indexes too, compare them with `--expected-indexes` (see
[lint rules](reference/lint-rules.md#expected-indexes)).

A TTL index (`expireAfterSeconds`) deletes documents: every one whose indexed
date is already older than the TTL goes as soon as it is built, and the rest
as they age. lint warns about every TTL index and every TTL a `collModIndex`
sets (`ttl-index`), and plan reports how many documents a new TTL index, or a
TTL a `collModIndex` adds or lowers, would delete at once (`wouldExpire`).

plan builds, changes and drops nothing: it reads the existing indexes and
lists, step by step, the index to create, to modify, to drop, already present
or already absent, and fails a step MongoDB would reject. It carries what
each pending index migration would do to the next, so a migration that
recreates an index an earlier pending one drops plans as apply would run it.
It compares indexes the way lint does, except that on MongoDB 7.0, which
takes a `createIndex` that differs from the index of its name only by
`hidden` as that index, plan reports it `present`; lint, without a database,
rejects it as 8.0 and later do. For a collation, `listIndexes` lists every
default (`caseLevel`, `alternate`, `version` and so on), and plan doesn't
guess them: a field the step and the index both set with different values is
another collation, so plan fails the step as MongoDB would reject it; when
every field the step sets matches but the index has more, plan says the
index differs, or warns on the index to create, that apply may fail. It can't predict a unique
index build failing on duplicates; only apply finds out.

### Changing an index in place

`collModIndex` changes options of an existing index with MongoDB's
`collMod`, without rebuilding it, so the collection never goes without the
index:

```json
{
  "collection": "events",
  "operation": "collModIndex",
  "name": "createdAt_1",
  "set": { "expireAfterSeconds": 2592000 }
}
```

`set` takes one or more of `expireAfterSeconds`, `hidden`, `prepareUnique`
and `unique`; `prepareUnique` goes in a step of its own, as MongoDB requires
(see the [reference](reference/migration-file.md#index-steps)). Each change
only rewrites the index's metadata, so it is fast. A step whose index already
has every value it sets does nothing (`present`); a step whose index doesn't
exist fails, in plan and apply, since there is nothing to change. plan
reports the index `to modify`, with each option's value before and after.

**Add or change a TTL.** `expireAfterSeconds` turns a plain index on a
single date field into a TTL index, or changes its TTL. As with a new TTL
index, every document already older than the TTL goes at the TTL monitor's
next pass, so plan reports `wouldExpire` when the step adds a TTL or lowers
it; raising it deletes nothing more. plan fails the step on a compound index
or `_id_`, which MongoDB refuses.

**Retire an index safely.** Hide it, watch, then drop it in a later
migration:

```json
{ "collection": "orders", "operation": "collModIndex", "name": "status_1", "set": { "hidden": true } }
```

MongoDB keeps a hidden index up to date but the query planner stops using
it, so you see how queries do without it while it can still come back. If
something regresses, unhide it at once with `"hidden": false` in a new
migration: it is back as soon as that runs, with no rebuild. Once nothing
regressed for long enough, drop it in a later release.

**Make an index unique.** MongoDB converts an existing index to unique only
in two `collMod` steps, with the duplicates removed in between. Write three
migrations:

1. `"set": { "prepareUnique": true }`. From then on MongoDB rejects every
   insert and update that would add a duplicate key to the index, so check
   the application can't write one first; plan warns about it.
2. A [data migration](#data-migrations) that deletes the duplicates or makes
   their keys distinct.
3. `"set": { "unique": true }`. MongoDB refuses it on an index without
   `prepareUnique` (lint's `unique-without-prepare`, and plan fails), and on
   duplicates. plan runs `collMod` with `dryRun` and fails the step on
   duplicates, with how many keys, how many documents share them, and the
   `_id`s of the documents sharing up to five of the keys (the keys
   themselves with `--sample-values`, see
   [values in errors](reference/report.md#values-in-errors)).

plan checks the data as it is, so ship the third migration in a later
release than the second: while the data migration is still pending, plan
finds the duplicates it would remove and fails. When the first migration is
pending too, plan can't check at all, as MongoDB checks only an index that is
already `prepareUnique`: it warns, and apply fails on duplicates.

**Turn a TTL off.** `collModIndex` can't remove a TTL. Set
`expireAfterSeconds` to 2147483647 (about 68 years), the largest value it
takes: the index stays a TTL index but deletes nothing in practice. If your
application declares its indexes, declare the same value there, or
`--expected-indexes` reports the index (`index-options`).

### Changing a collection's validation

`collModCollection` sets a collection's
[schema validation](https://www.mongodb.com/docs/manual/core/schema-validation/)
with MongoDB's `collMod`:

```json
{
  "collection": "users",
  "operation": "collModCollection",
  "set": {
    "validator": { "$jsonSchema": { "required": ["email"], "properties": { "email": { "bsonType": "string" } } } },
    "validationLevel": "moderate",
    "validationAction": "error"
  }
}
```

`set` takes one or more of `validator`, `validationLevel` and
`validationAction` (see the [reference](reference/migration-file.md#index-steps));
`"validator": {}` removes the validator. A step whose collection already has
every value it sets does nothing (`present`); validators are compared
field by field, in any order, numbers by value. A step whose collection
doesn't exist fails, in plan and apply, and so does one on a view or a
time-series collection, which MongoDB refuses. plan reports the validation
`to modify`, with each value before and after; a collection that never had
one shows the defaults, `{}`, `strict` and `error`.

MongoDB doesn't check the documents already there when the validator
changes: they all stay, matching or not. plan counts those that don't match
the new validator (`nonMatching`) whenever one applies after the step (not
`{}`, and a level other than `off`), and warns when there are any, as what
becomes of them depends on the level and action:

- `strict` and `error`: every later update to one of them that leaves it not
  matching fails, so writes the application thought routine start failing.
  Fix them in a [data migration](#data-migrations) first, or start with
  `moderate`.
- `moderate` and `error`: updates to documents that don't match go through;
  inserts, and updates to documents that match, must match.
- `warn`: MongoDB writes everything, and logs each write that doesn't match.

To bring in a validator on a collection with documents that don't match,
start with `moderate` (or `warn`), fix the documents in data migrations, then
switch to `strict` in a later release, once plan reports `nonMatching 0`. plan
counts on the data as it is, so a pending data migration's fixes don't show
until it is applied.

plan sees what earlier pending steps would do to the collection's
validation, and a collection an earlier pending `createIndex` would create,
but not one a pending data or merge migration would create: that step fails
in plan, and passes once the data migration is applied.

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
  (default `_id`), and both actions. `whenMatched` is `"merge"`. Choose
  `"discard"` to update only existing matches, or `"insert"` to create
  [missing catalog entries](#catalog-upserts) keyed by `_id`.
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

### Catalog upserts

Use `whenNotMatched: "insert"` to copy a catalog while preserving its BSON
IDs. For example, this pipeline copies two fields from `legacyCatalog`
into `catalog`:

```json
[
  { "$project": { "_id": 1, "label": 1 } },
  { "$merge": { "into": "catalog", "on": "_id", "whenMatched": "merge", "whenNotMatched": "insert" } }
]
```

Emit one row per stable `_id`. A missing, null, array, regex or undefined ID
fails preflight. Document-valued IDs must also be valid stored MongoDB IDs.
For example, catalog preflight checks the `$id` payload of an embedded DBRef.
It rejects `{"_id":{"r":{"$ref":"items","$id":{"$bad":1}}}}` on every
supported version, even on servers that accept it in a native `$merge`.
On a match, the emitted `_id` must preserve the stored BSON representation.
For example, `Decimal128("1.0")` matches `Decimal128("1.00")` by numeric
value, but the different encoding fails preflight.
Literal top-level output names must be nonempty and cannot contain a dot or
begin with `$`. A dotted input expression such as `"$details.label"` can still
supply an ordinary output field. When selecting one of several source rows,
order ties deterministically.

A missing ID receives the complete output document. An existing match keeps
its other top-level fields. Project only catalog-owned fields, since an update
between plan and apply can turn a planned insert into a matched update.
Objects and arrays follow the [field replacement rules](#shape-the-output).
Removing a source row leaves its target row in place. To refresh after
`Finished`, retain the original file and add a migration with a new ID.

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
| `unmatchedCount`   | Output documents a `discard` step would skip               | Above 0 logs a warning: the `on` fields are probably wrong |
| `typeChangedCount` | Matched documents where an output field changes the BSON type of an existing, non-null target field, or of a value nested in it, or drops a non-null nested field | Above 0 fails validation |
| `duplicateOnCount` | Output documents whose `on` fields equal another output document's and match a target document. An insert step reports duplicates of a missing ID as a separate error | Above 0 fails validation |
| `nullOnCount`      | Output documents with an `on` field that is null or missing: `$merge` would match them with a target document whose field is null or missing, if there is one | Above 0 fails validation |
| `arrayOnCount`     | Output documents with an `on` field that is an array, matched or not: `$merge` rejects them (error 51185), part way through | Above 0 fails validation |
| `idMismatchCount`  | Matched documents whose emitted `_id` would change the target's immutable ID. Insert steps require its exact BSON representation. For a discard step on other fields, omit `_id` from the projection | Above 0 fails validation |
| `collationOnlyMatchCount` | Only when the unique index on the `on` fields has a non-simple collation: output documents with no exact match, so in `unmatchedCount` too, that match a target through that collation (see [collations](#collations)) | Above 0 logs a warning saying what the server's version does with them |
| `diffCountAfter`   | Apply only: documents that still differ after `$merge`                          | Above 0 fails the migration: it didn't settle |

A field missing or null in the target takes any type and counts only as a
diff. The reverse, a non-null field the output sets to null, is a type
change. Plan stops after preflight. Apply validates the step before writing,
runs the full pipeline, and compares again. For an insert step, both missing
IDs and differing matches must reach zero. In a file containing an insert
step, the comparison after the last write and fresh comparisons of every
earlier step enforce each step's preflight rules again. An insert step also
enforces them immediately after its own write. A duplicate match or invalid
output can first appear during these checks, even with zero differences.
The error identifies the step and earlier writes survive. Each insert comparison
validates IDs, counts duplicates and measures differences from one pipeline
evaluation. After an insert comparison following a write, the runner checks
that the primary source and each named `$unionWith` source still exist.
Application writers can change the data between preflight, the
write and verification. See the
[per-target report](reference/report.md#catalog-insert-observations).

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

A failed preflight prevents that step's write. A failed comparison after the
write leaves the target changes in place. A step interrupted or rejected
during `$merge` can be half done: that is safe as long as the running code copes with old and new
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
what `$merge` does. Discard steps warn and continue as described below.
Insert steps require simple collation on the primary source and target.

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
