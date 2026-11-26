# Plan and apply report

Every `migrune plan` and `migrune apply` run ends with a report on stdout:
one row per migration considered, plus totals. Logs go to stderr, so the
report can be captured on its own:

```sh
migrune apply --dir migrations --db app --digest-in plan.digest -o json > report.json
```

The report is also written when the run fails or is interrupted; the
[exit code](env-and-exit-codes.md#exit-codes) still tells the outcome.

No report is written when the run stops before it holds the migration lock:
invalid flags, invalid migration files, an unreachable server, an apply
refused for [`--init`](#first-apply), or a lock held by another runner.
When plan or apply stop on a [lint](lint-rules.md#in-plan-and-apply)
error, stdout has lint's error findings in place of the report, in lint's
format: with `-o json`, lint's [JSON array](lint-rules.md#output) of
findings, not a report object.

`--output` (`-o`) picks the format: `table` (the default) or `json`.

## Which migrations are considered

- **Pending** migrations: those with no `Finished` entry in the changelog, in
  id order. Only the entries of the run's
  [namespace](../running.md#several-apps-on-one-database) count, here and
  below.
- With `apply --mutable`, also the `Finished` migrations whose file changed.
- Without `--mutable`, a `Finished` migration whose file changed fails the run
  before anything runs. It is reported as `errored`, and every pending
  migration as `skipped`.
- A `Finished` migration whose file is no longer in `--dir` ("applied but
  missing", e.g. after a revert) fails the run before anything runs, with or
  without `--mutable`: every pending migration is `skipped`, the missing ones
  are listed in `appliedButMissing`, plan writes no digest and nothing is
  written to the changelog. See
  [troubleshooting](../troubleshooting.md#applied-but-missing).
- When `apply --digest-in` doesn't match the pending set (see
  [Plan digest](#plan-digest)), nothing runs and every row is `skipped`.

Migrations already applied with an unchanged file are left out.

### First apply

A namespace's first apply is the one that finds no changelog entry, of any
status, for its [namespace](../running.md#several-apps-on-one-database) in
the target database: every migration is pending. A mistyped `--db` or
`--namespace` looks the same, so apply refuses it unless `--init` is set, and
refuses `--init` once the namespace has entries. Both refusals come before
apply takes the lock or writes anything, with no report (see
[troubleshooting](../troubleshooting.md#apply-needs---init)).

plan, finding no entry, logs a warning, `first apply for namespace <name> on
database <db>` (`the default namespace` in place of `namespace <name>`), sets
`firstApply` to `true` and prints a `First apply:` line in the table; its
[digest](#plan-digest) gets a `first-apply` line, so the plan matches only an
apply that is still a first apply. apply with `--init` sets `firstApply` and
prints the line too, and logs `first apply for namespace <name> on database
<db> (--init)`, followed by the other namespaces the changelog has entries
for, if any. Check `--db` and `--namespace` before approving such a plan.

When the target database also has no collections at all, apart from
migrune's own, `migrations_lock` and an empty `migrations_changelog`, which
an earlier plan leaves behind, plan and apply set
`newDatabase` to `true` and say `(new: it had no collections)` after the
database name in the table.

## Statuses

| Status        | apply                                                                                  | plan                                 |
| ------------- | -------------------------------------------------------------------------------------- | ------------------------------------ |
| `applied`     | committed, with its `Finished` changelog entry                                         | passed: apply would apply it         |
| `errored`     | failed and recorded as `Errored`, unless the lock was lost; the run stopped            | failed: apply would fail on it       |
| `interrupted` | in progress when a signal arrived; aborted, it stays pending                           | in progress when a signal arrived    |
| `skipped`     | not reached: an earlier migration errored, the run was interrupted, or a check failed | the same                             |

plan runs every migration and aborts it: it never commits data or writes a
changelog entry, so each row tells what apply would do to the database as it
is now.

Data rows (`kind` `data`) list their steps in `steps`, with what MongoDB
reported for each in the first of the two runs: `matched`, `modified` and
`upserted` for an update, `deleted` for `deleteMany`. In plan, that is what
apply would do to the data as it is now; in apply, what it did. They are
there whenever the first run completed, so an `errored` row that failed the
idempotency check shows them too. The same counts go to the logs on stderr,
one line per step. Read them before approving a plan: a `deleteMany` whose
filter matches far more documents than expected shows up here.

A [batched](../writing-migrations.md#batched-data-migrations) data row also
has its `batchSize`. In plan, which runs only the first batch, its step
counts are that batch's, `matched` is how many documents the filter matches
in all, and `checked` how many of them the first batch holds. In apply, its
step counts are summed over the batches that committed, and `batches` is how
many did: all of them when the row is `applied`.

Index rows (`kind` `index`) list their steps in `indexes`: in plan, what each
step would do to the indexes, or to a collection's validation, as they are
now, changed by the pending index migrations before it; in apply, what it
did. A plan step predicted to fail, such as a
name already used by an index with other keys or options, a `collModIndex`
of a missing index, a `collModCollection` of a missing collection, or either
that the database user lacks the
[`collMod` action](../running.md#credentials) for, is the last item,
with the reason in `conflict`, and makes the row `errored`. The same lines
also go to the logs on stderr, one per step.

Merge rows (`kind` `merge`) carry the counts of the comparison migrune runs
before and, in apply, after `$merge`, summed over the steps that ran (see
[JSON](#json)), plus up to five `samples` per step, those with a type change
first, also logged on stderr. What each count means, the type-strict rules, nested
types and their depth are in
[how the comparison works](../writing-migrations.md#how-the-comparison-works).

A sample shows the document's `on` fields as they are, and for each field
that differs its name, its BSON types before and after, and the path of a
nested type change, but no values: plan runs before anyone approves it, and
its report and logs often end up in CI logs and artifacts. `--sample-values`
(`MIGRUNE_SAMPLE_VALUES=true`) adds the values, in the report and the logs
alike, each cut to 200 characters. Use it where those logs are as protected
as the data, such as locally.

The `on` fields are shown whatever `--sample-values` says, since without
them a sample can't be traced to its document. When `on` is a natural key,
such as an email address, those values reach the CI logs: merge on `_id`
or another opaque key where that matters.

## Values in errors

A row's `error`, the run's `error`, the logs and the `message` of an
`Errored` [changelog entry](changelog-and-lock.md#changelog-migrations_changelog) can
quote a server error. A server's message can quote document values (the key
of a duplicate key error, the value `$convert` failed on, the document an
update path ran into, a validation failure's details), so plan and apply,
without `--sample-values`, show none of the server's text. Each server
error shows only:

- its code name and code, such as `(DuplicateKey, code 11000)` (a write
  error's code name is shown for the codes migrations commonly meet, since
  the server sends only the code);
- for a write error whose details give it, such as a document validation
  failure (code 121), the failing document's `_id`.

migrune's own text around it stays: the migration, the step, any hint
migrune adds, and a batched migration's `committed N of M batches`. The text then ends with
`server message hidden; rerun plan with --sample-values to see it`. Errors
that don't come from the server, such as a network error, a timeout or one
of migrune's own checks, are shown whole: they quote no document values.
So are the errors of connecting (reaching the server, authenticating,
checking its version and topology), which come before any migration runs.

This covers plan and apply only. `drift` and `lint --uri` have no
`--sample-values`: they show server errors as the server and the driver word
them.

`_id`s are always shown, here and in a failed unique conversion's
duplicates, since they are what finds the documents. When `_id` is a natural
key, such as an email address, it reaches the CI logs too.

For example, an update that breaks a unique index:

```txt
/steps/0 (updateOne on users): write exception: write errors: [(DuplicateKey, code 11000)]; server message hidden; rerun plan with --sample-values to see it
```

And one that breaks the collection's validator:

```txt
/steps/0 (updateOne on users): write exception: write errors: [(DocumentValidationFailure, code 121) {"failingDocumentId":1}]; server message hidden; rerun plan with --sample-values to see it
```

plan's check of a unique conversion (a `collModIndex` setting `unique`)
names, for up to five duplicate keys, the `_id`s of up to five of the
documents sharing each, not the keys. `_id`s are shown, as `on` fields are
in merge samples, so the documents can be found.

With `--sample-values`, errors read as the server and the driver word them,
the server's message and the values it quotes included, and the unique
conversion check shows the keys instead of the `_id`s.

## JSON

```json
{
  "version": "0.1.0",
  "command": "apply",
  "database": "app",
  "startedAt": "2026-11-08T10:30:00.125Z",
  "runner": "ci-42",
  "approvedBy": { "name": "Ada Lovelace", "email": "ada@example.com" },
  "newDatabase": false,
  "firstApply": false,
  "digest": "f5db59303fc5cbe2a1554003b52fff9a9837b972373428d889dc3c0726d274bc",
  "migrations": [
    {
      "id": "2026-11-01-001-set-flag",
      "kind": "data",
      "status": "applied",
      "durationMs": 12,
      "steps": [
        { "index": 0, "operation": "updateMany", "collection": "items", "matched": 2, "modified": 2, "upserted": 0 }
      ]
    },
    {
      "id": "2026-11-01-002-inc-counter",
      "kind": "data",
      "status": "errored",
      "durationMs": 1500,
      "error": "/steps/0 (updateMany on items, second run): not idempotent: ...",
      "steps": [
        { "index": 0, "operation": "updateMany", "collection": "items", "matched": 3, "modified": 3, "upserted": 0 }
      ]
    },
    {
      "id": "2026-11-01-003-seed-settings",
      "kind": "data",
      "status": "skipped",
      "durationMs": 0
    }
  ],
  "appliedButMissing": [],
  "totals": {
    "migrations": 3,
    "applied": 1,
    "errored": 1,
    "interrupted": 0,
    "skipped": 1,
    "durationMs": 1530
  },
  "error": "migration 2026-11-01-002-inc-counter failed: ..."
}
```

| Field                     | Type             | Description                                                                                                  |
| ------------------------- | ---------------- | ------------------------------------------------------------------------------------------------------------ |
| `version`                 | string           | The migrune version that wrote the report, as `migrune --version` prints it.                               |
| `command`                 | string           | `plan` or `apply`.                                                                                           |
| `database`                | string           | The target database, `--db`.                                                                                 |
| `namespace`               | string, optional | The run's `--namespace`. Absent in the default namespace.                                                    |
| `startedAt`               | string           | When the run started, RFC 3339 in UTC to the millisecond, e.g. `2026-11-08T10:30:00.125Z`.                   |
| `runner`                  | string           | The runner id the run held the lock under, as in the logs and the changelog.                                 |
| `approvedBy`              | object, optional | apply only: `name` and `email`, as given by `--approved-by-name` and `--approved-by-email`, and recorded in the changelog. Absent in plan and when neither is set. |
| `newDatabase`             | boolean          | `true` when the target database had no collections, apart from `migrations_lock` and an empty `migrations_changelog`, when the run started. See [first apply](#first-apply). |
| `firstApply`              | boolean          | `true` when the changelog had no entry, of any status, for the run's namespace when the run started: apply needs `--init`. See [first apply](#first-apply). |
| `digest`                  | string, optional | The run's [plan digest](#plan-digest): in plan, what `--digest-out` writes; in apply, what `--digest-in` must match. Absent when the run failed before reading the changelog. |
| `migrations`              | array            | One row per migration considered, in id order. Empty when nothing is pending.                                |
| `migrations[].id`         | string           | The migration id: the file name without `.json`.                                                             |
| `migrations[].kind`       | string           | `data`, `index` or `merge`.                                                                                  |
| `migrations[].status`     | string           | `applied`, `errored`, `interrupted` or `skipped`.                                                            |
| `migrations[].durationMs` | integer          | How long the migration ran, in milliseconds. 0 when skipped.                                                 |
| `migrations[].error`      | string, optional | Why the migration errored or was interrupted, without the server's message unless `--sample-values` is set (see [values in errors](#values-in-errors)). Absent otherwise. |
| `migrations[].steps`      | array, optional  | Data migrations only: one item per step, in file order, with the first run's counts (see below; for a batched migration, see [statuses](#statuses)). Absent for other kinds, for `skipped` rows, and when the first run failed. |
| `migrations[].batchSize`  | integer, optional | Batched data migrations only: the file's `batchSize`. Absent for other migrations and for `skipped` rows. |
| `migrations[].matched`    | integer, optional | Batched data migrations, plan only: the documents the step's filter matches, counted before the first batch. |
| `migrations[].checked`    | integer, optional | Batched data migrations, plan only: the documents of the first batch, the only one plan runs. |
| `migrations[].batches`    | integer, optional | Batched data migrations, apply only: the batches that committed, each in a transaction of its own; all of them when the row is `applied`. |
| `migrations[].indexes`    | array, optional  | Index migrations only: one item per step that ran, in file order (see below). Absent for other kinds and for `skipped` rows. |
| `migrations[].diffCountBefore`  | integer, optional | Merge migrations only: matched output documents that differ from their target, counted before the write. Absent for other kinds and for `skipped` rows. |
| `migrations[].diffCountAfter`   | integer, optional | Merge migrations, apply only: the documents that still differed after `$merge` ran. 0 once the merge settled. Absent in plan. |
| `migrations[].unmatchedCount`   | integer, optional | Merge migrations only: output documents with no match in discard steps. Absent on insert-only rows. |
| `migrations[].typeChangedCount` | integer, optional | Merge migrations only: the documents where `$merge` would change the BSON type of an existing, non-null field, or of a value nested in it, or drop a non-null nested field. |
| `migrations[].duplicateOnCount` | integer, optional | Merge migrations only: the output documents whose `on` fields equal another output document's and match a target document. |
| `migrations[].nullOnCount`      | integer, optional | Merge migrations only: the output documents with a null or missing `on` field. |
| `migrations[].arrayOnCount`     | integer, optional | Merge migrations only: the output documents with an array as an `on` field, which `$merge` rejects. |
| `migrations[].idMismatchCount`  | integer, optional | Matched output documents whose `_id` differs from the target's. For insert steps this includes an equal lookup key with a different BSON representation. For discard steps this check applies when the `on` fields omit `_id`. |
| `migrations[].collationOnlyMatchCount` | integer, optional | Merge migrations only, when the unique index `$merge` matches the `on` fields through has a non-simple collation, such as a case-insensitive one: the output documents with no exact match in the target, so also in `unmatchedCount`, that match one through that collation. What MongoDB does with them depends on its version: see [collations](../writing-migrations.md#collations). Absent otherwise. |
| `migrations[].insertSteps` | array, optional | [Per-target observations](#catalog-insert-observations) for reached `whenNotMatched: "insert"` steps. |
| `migrations[].samples`    | array, optional  | Merge migrations only: up to five documents per step that differed before the merge (see below). Absent when none differed. |
| `appliedButMissing`       | array            | Migrations with a `Finished` changelog entry and no file, e.g. after a revert, in id order. Not empty means the run failed before running anything. |
| `appliedButMissing[].id`  | string           | The migration id.                                                                                            |
| `appliedButMissing[].description` | string   | The migration's description, as stored by its latest `Finished` changelog entry.                             |
| `appliedButMissing[].appliedAt`   | string, optional | When that entry was written (its `createdAt`), RFC 3339 in UTC, e.g. `2026-01-04T10:12:00Z`. Absent when the entry has no `createdAt`. |
| `appliedButMissing[].runner`      | string, optional | The runner id that applied it. Absent when the entry has no `runner`.                                |
| `totals.migrations`       | integer          | The number of rows.                                                                                          |
| `totals.applied`          | integer          | Rows with status `applied`.                                                                                  |
| `totals.errored`          | integer          | Rows with status `errored`.                                                                                  |
| `totals.interrupted`      | integer          | Rows with status `interrupted`.                                                                              |
| `totals.skipped`          | integer          | Rows with status `skipped`.                                                                                  |
| `totals.durationMs`       | integer          | How long the whole run took, in milliseconds.                                                                |
| `error`                   | string, optional | Why the run failed, without the server's message unless `--sample-values` is set (see [values in errors](#values-in-errors)). Absent when it succeeded. |

The merge validation counts (`typeChangedCount`, `duplicateOnCount`,
`nullOnCount`, `arrayOnCount` and `idMismatchCount`) sum the reached steps'
preflight observations. A positive count prevents that step's write. Earlier
steps may already have written. A violation first seen after a write appears
in `migrations[].error` and leaves the preflight counts unchanged.

### Data steps

Each item of `migrations[].steps` is one step of a data migration, as the
first of its two runs left it:

| Field        | Type              | Description                                                         |
| ------------ | ----------------- | ------------------------------------------------------------------- |
| `index`      | integer           | The step's index in file order.                                     |
| `operation`  | string            | `updateOne`, `updateMany` or `deleteMany`.                          |
| `collection` | string            | The step's collection.                                              |
| `matched`    | integer, optional | Updates only: the documents the filter matched.                     |
| `modified`   | integer, optional | Updates only: the documents changed.                                |
| `upserted`   | integer, optional | Updates only: the documents inserted by an upsert, 0 or 1.          |
| `deleted`    | integer, optional | `deleteMany` only: the documents deleted.                           |

### Index steps

Each item of `migrations[].indexes` is one step of an index migration. A
`collModCollection` step changes no index: its item has no `name`, `keys` or
`options`.

| Field        | Type             | Description                                                                                                                   |
| ------------ | ---------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `action`     | string           | `create`, `drop`, `modify` (a `collModIndex` or `collModCollection` that changes at least one option), `present` (a `createIndex` whose index already exists with that definition, or a `collModIndex` or `collModCollection` that finds every value it sets already set) or `absent` (a `dropIndex` whose index doesn't exist). In plan, what the step would do; in apply, what it did. plan also says `differs`: a `createIndex` whose index exists with a collation plan can't compare with the step's, because `listIndexes` shows it with every default filled in; see `warning`. |
| `collection` | string           | The step's collection.                                                                                                        |
| `name`       | string, optional | The index name. Absent for a `collModCollection`.                                                                             |
| `keys`       | object, optional | The key pattern, as relaxed Extended JSON, in key order: the step's for a `createIndex`, the existing index's for `drop` and for a `collModIndex`. Absent for `absent`, and for a `collModIndex` of a missing index. |
| `options`    | object, optional | The index options other than its name, the same way, for a `createIndex` or `drop`. Absent when there are none, and for a `collModIndex`. |
| `set`        | object, optional | `collModIndex` and `collModCollection` only: the step's `set`, the values after it, as relaxed Extended JSON.                 |
| `before`     | object, optional | `collModIndex` only, when the index exists: its value of each option in `set` before the step. `expireAfterSeconds` is `null` when the index has no TTL; the booleans are `false` when absent. `unique` is `true` on `_id_`, and `prepareUnique` on a unique index, since setting them changes nothing. For a `collModCollection` whose collection exists, its value of each field of `set`: `validator` is `{}` when it has none; `validationLevel` and `validationAction` are MongoDB's defaults, `strict` and `error`, when unset. |
| `conflict`   | string, optional | plan only: why MongoDB would reject the step. Only on the last item of an `errored` row.                                      |
| `wouldExpire`| integer, optional | plan only, on a `create` of a TTL index (`expireAfterSeconds`, one key), or a `modify` that adds a TTL or lowers it: the documents the index would delete as soon as it is built or changed, those whose indexed date is older than now, by migrune's clock, less `expireAfterSeconds`, among those its `partialFilterExpression` selects. |
| `warning`    | string, optional | plan only: why apply may fail although plan found no conflict, on a `differs` step, on a `create` next to an index with the same keys and options and a collation plan can't compare, or on a `modify` setting `unique` that plan can't check for duplicates because an earlier pending step sets `prepareUnique`. On a `modify` setting `prepareUnique`, that MongoDB rejects new duplicate keys from then on. On a `collModCollection` with `nonMatching` above 0, what becomes of those documents under the level and action set. |
| `nonMatching`  | integer, optional | plan only, on a `collModCollection` that changes the validation and leaves a validator in force (not `{}`, `validationLevel` not `off`): the documents of the collection that don't match that validator. See [changing a collection's validation](../writing-migrations.md#changing-a-collections-validation). |

```json
{
  "id": "2026-11-02-003-name-ci",
  "kind": "index",
  "status": "applied",
  "durationMs": 8,
  "indexes": [
    { "action": "present", "collection": "items", "name": "name_1", "keys": { "name": 1 } },
    {
      "action": "create",
      "collection": "items",
      "name": "name_1_ci",
      "keys": { "name": 1 },
      "options": { "unique": true, "collation": { "locale": "en", "strength": 2 } }
    },
    { "action": "drop", "collection": "items", "name": "name_1", "keys": { "name": 1 } },
    { "action": "absent", "collection": "items", "name": "stale_1" }
  ]
}
```

### Catalog insert observations

Each `insertSteps` item identifies a reached insert step by its zero-based
`step` and target collection `into`. `targetExistsBefore` is the observed
existence of that target. `pendingBefore` and `pendingAfter` count missing
target IDs. `diffCountBefore` and `diffCountAfter` count differing matches
for that same target. A measured zero or `false` appears explicitly. An
unavailable observation is absent, including after-counts in plan or after
a failed write. After a final recheck the row totals use each step's latest
completed after-count once. The table lists the counts per insert target.

For example, a plan with one missing ID in an empty target includes:

```json
{ "insertSteps": [{ "step": 0, "into": "catalog", "targetExistsBefore": true, "pendingBefore": 1, "diffCountBefore": 0 }] }
```

### Merge samples

Each item of `migrations[].samples` is an output document that differs from
its target document:

| Field                 | Type              | Description                                                                                         |
| --------------------- | ----------------- | --------------------------------------------------------------------------------------------------- |
| `step`                | integer           | The step's index in file order.                                                                     |
| `key`                 | object            | The output document's `on` fields, as relaxed Extended JSON, with or without `--sample-values`.     |
| `fields`              | array             | The output fields that differ, in output order.                                                     |
| `fields[].field`      | string            | The field name.                                                                                     |
| `fields[].beforeType` | string            | The target value's BSON type (`$type` name), `missing` when the target lacks the field; with `path`, the type of the target's value at `path`. |
| `fields[].afterType`  | string            | The output value's BSON type, which `$merge` writes; with `path`, the type of the output's value at `path`. The same two types mean the value changed. |
| `fields[].path`       | string, optional  | A nested type change: the dotted path, from the field, of the shallowest nested value whose type changes, with array elements by index, e.g. `price.amount` or `lines.1.qty`. A nested field the output drops is a change to type `missing`, e.g. `address.street` with `afterType` `missing`. Absent otherwise. |
| `fields[].typeChanged`| boolean, optional | `true` when the field exists in the target, not null, with another BSON type, or has a nested type change or drop (`path`). Absent otherwise. |
| `fields[].before`     | any, optional     | `--sample-values` only: the target's value, as relaxed Extended JSON. Absent when the target lacks the field. |
| `fields[].after`      | any, optional     | `--sample-values` only: the output's value.                                                         |
| `fields[].truncated`  | boolean, optional | `true` when `before` or `after` was longer than 200 characters of Extended JSON: it is then a JSON string of its first 200 characters followed by `…`. Absent otherwise. |

```json
{
  "id": "2026-11-03-004-qty-to-double",
  "kind": "merge",
  "status": "errored",
  "durationMs": 20,
  "error": "/steps/0 (aggregate on counters): typeChangedCount is 1: ...",
  "diffCountBefore": 2,
  "unmatchedCount": 0,
  "typeChangedCount": 1,
  "duplicateOnCount": 0,
  "nullOnCount": 0,
  "arrayOnCount": 0,
  "idMismatchCount": 0,
  "samples": [
    {
      "step": 0,
      "key": { "_id": 1 },
      "fields": [{ "field": "qty", "beforeType": "int", "afterType": "double", "typeChanged": true }]
    },
    {
      "step": 0,
      "key": { "_id": 2 },
      "fields": [{ "field": "qty", "beforeType": "missing", "afterType": "null" }]
    }
  ]
}
```

A nested type change, where the field's value is equal under `$eq`:

```json
{
  "step": 0,
  "key": { "_id": 1 },
  "fields": [{ "field": "price", "beforeType": "long", "afterType": "double", "path": "price.amount", "typeChanged": true }]
}
```

The first sample again, with `--sample-values`:

```json
{
  "step": 0,
  "key": { "_id": 1 },
  "fields": [
    { "field": "qty", "beforeType": "int", "afterType": "double", "typeChanged": true, "before": 1, "after": 1.0 }
  ]
}
```

This page describes the shape of the migrune version it ships with.

## Table

```txt
Database: app

MIGRATION                     KIND  STATUS   DURATION  ERROR
2026-11-01-001-set-flag       data  applied  12ms
  /steps/0 updateMany on items: matched 2, modified 2, upserted 0
2026-11-01-002-inc-counter    data  errored  1.5s      /steps/0 (updateMany on items, second run): not idempotent
  /steps/0 updateMany on items: matched 3, modified 3, upserted 0
2026-11-01-003-seed-settings  data  skipped  0s

Total: 3 migrations: 1 applied, 1 errored, 0 interrupted, 1 skipped in 1.53s

Error: migration 2026-11-01-002-inc-counter failed: ...
```

The table starts with the target database, followed by `(new: it had no
collections)` for a [new database](#first-apply), then, in a named
namespace, a `Namespace: <name>` line, then, on a [first apply](#first-apply),
`First apply: the changelog has no entries for this namespace; apply needs
--init` (plan) or `First apply: the changelog had no entries for this
namespace` (apply). With no row, the table prints `No
pending migrations.` in place of the header and rows; the JSON has
`"migrations": []`.

When the run failed, the table ends with its `error`, the same text as in the
JSON. A run that failed several checks at once has one line per check, the
lines after the first indented.

A data row is followed by one indented line per step: its index, operation
and collection, and the first run's counts. A batched row then has a
`batched:` line: in plan, `batched: checked 1,000 of 2,314,000 matching
documents`; in apply, `batched: committed 3 batches of up to 1,000
documents`.

An index row is followed by one indented line per step, with what the step
would do (plan: `to create`, `to modify`, `to drop`) or did (apply:
`created`, `modified`, `dropped`), or `already present` / `already absent`.
A `collModIndex` line gives its `set` and `before`. A `collModCollection`
line starts with `validation` in place of `index` and the name, says
`already set` rather than `already present`, and gives its `set`, `before`
and `nonMatching`. A step plan predicts to
fail says `would fail`, followed by the reason. A TTL index to create, or a
TTL to add or lower, ends with its `wouldExpire` count. A `differs` step says
`already present, collation not comparable`, and a step with a `warning`
ends with it, after `warning:`:

```txt
Plan: each status is what apply would do. Nothing was committed.
Database: app

MIGRATION                   KIND   STATUS   DURATION  ERROR
2026-11-02-003-name-ci      index  applied  8ms
  index already present: name_1 on items, keys {"name":1}
  index to create: name_1_ci on items, keys {"name":1}, options {"unique":true,"collation":{"locale":"en","strength":2}}
  index to drop: name_1 on items, keys {"name":1}
  index already absent: stale_1 on items
2026-11-02-004-conflict     index  errored  3ms       /steps/1 (createIndex on items): index name_1 already exists with a different definition ...
  index to create: createdAt_-1 on items, keys {"createdAt":-1}
  index to create, would fail: name_1 on items, keys {"name":-1}: index name_1 already exists with a different definition ...
2026-11-02-005-email-index  index  skipped  0s
```

`collModIndex` steps read like this:

```txt
2026-11-06-001-expire-events  index  applied  6ms
  index to modify: createdAt_1 on events, keys {"createdAt":1}, set {"expireAfterSeconds":2592000}, before {"expireAfterSeconds":null}, wouldExpire 12
2026-11-06-002-hide-legacy    index  applied  2ms
  index already present: legacy_1 on items, keys {"legacy":1}, set {"hidden":true}, before {"hidden":true}
2026-11-06-004-unique-email   index  errored  9ms       /steps/0 (collModIndex on users): index email_1 has 2 duplicate keys, ...
  index to modify, would fail: email_1 on users, keys {"email":1}, set {"unique":true}, before {"unique":false}: index email_1 has 2 duplicate keys, shared by 5 documents, such as the documents with the _ids [1,2,3], [4,5]: ...
```

`collModCollection` steps read like this:

```txt
2026-11-05-001-require-user-name  index  applied  5ms
  validation to modify on users, set {"validator":{"$jsonSchema":{"required":["name"]}},"validationLevel":"strict"}, before {"validator":{},"validationLevel":"strict"}, nonMatching 3: warning: 3 documents of users don't match the new validator: ...
2026-11-05-002-moderate-accounts  index  applied  1ms
  validation already set on accounts, set {"validationLevel":"moderate"}, before {"validationLevel":"moderate"}
```

A merge row is followed by its counts, then one line per sample: the step,
the `on` fields, and each differing field as `beforeType -> afterType`:

```txt
Database: app

MIGRATION                     KIND   STATUS   DURATION  ERROR
2026-11-03-001-copy-label     merge  applied  45ms
  merge: diffCountBefore 3, diffCountAfter 0, unmatchedCount 0, typeChangedCount 0, duplicateOnCount 0, nullOnCount 0, arrayOnCount 0, idMismatchCount 0
  differs at /steps/0 {"_id":1}: label: missing -> string
2026-11-03-004-qty-to-double  merge  errored  20ms      /steps/0 (aggregate on counters): typeChangedCount is 1: ...
  merge: diffCountBefore 2, unmatchedCount 0, typeChangedCount 1, duplicateOnCount 0, nullOnCount 0, arrayOnCount 0, idMismatchCount 0
  differs at /steps/0 {"_id":1}: qty: int -> double, type changed
  differs at /steps/0 {"_id":2}: qty: missing -> null
```

A nested type change names the path and its types, and a nested field the
output drops has type `missing`:

```txt
  differs at /steps/0 {"_id":1}: price: type changed at price.amount (long -> double)
  differs at /steps/0 {"_id":1}: address: type changed at address.street (string -> missing)
```

With `--sample-values`, each field reads `before -> after` instead, `absent`
for a field the target lacks, with the BSON types when they differ, and a
value longer than 200 characters cut and followed by `…`:

```txt
  differs at /steps/0 {"_id":1}: label: absent -> "a"
  differs at /steps/0 {"_id":1}: qty: 1 (int) -> 1.0 (double), type changed
  differs at /steps/0 {"_id":1}: price: {"amount":5} -> {"amount":5.0}, type changed at price.amount (long -> double)
```

A plan's table starts with a line saying nothing was committed, before the
database. When
migrations are applied but missing, the table lists them after the rows, and
every row is `skipped`:

```txt
Plan: each status is what apply would do. Nothing was committed.
Database: app

MIGRATION                     KIND  STATUS   DURATION  ERROR
2026-11-01-001-set-flag       data  skipped  0s
2026-11-01-003-seed-settings  data  skipped  0s

Applied but missing (Finished in the changelog, no file):
  2026-10-01-001-gone: Finished 2026-01-04T10:12:00Z by ci-7: "Backfill the flag"; the file is not in --dir. The full migration is stored in migrations_changelog (migrationId: "2026-10-01-001-gone")
  2026-10-02-001-gone: Finished 2026-01-05T12:12:01Z by laptop-81-x7: "Rename \"old\" to \"new\""; the file is not in --dir. The full migration is stored in migrations_changelog (migrationId: "2026-10-02-001-gone")

Total: 2 migrations: 0 applied, 0 errored, 0 interrupted, 2 skipped in 40ms

Error: applied migrations must keep their file, but the file of 2026-10-01-001-gone, 2026-10-02-001-gone is missing from the migrations directory; ... delete its changelog entries by hand
```

Each missing migration shows the description, applied time (UTC, to the
second) and runner from its latest `Finished` changelog entry, or `unknown`
for a time or runner only an entry written by hand can lack; descriptions
are quoted and escaped so each stays on one line. The JSON shape:

```json
"appliedButMissing": [
  {
    "id": "2026-10-01-001-gone",
    "description": "Backfill the flag",
    "appliedAt": "2026-01-04T10:12:00Z",
    "runner": "ci-7"
  }
]
```

## Plan digest

`plan --digest-out` writes the plan digest to a file, and `apply --digest-in`
runs nothing unless its own digest is the same. The digest is the SHA-256, in
lowercase hex, of this input, each line ending in a newline:

```txt
migrune-digest-v1
database <name>
replicaSet <name>
deployment <uuid>
namespace <name>
first-apply
migration <id> <checksum>
```

- `migrune-digest-v1` versions the input: a later change to what the digest
  covers gets a new version. apply refuses a file of another version as a
  usage error (exit 2): re-run plan with the migrune that applies.
- `database` is `--db`.
- `replicaSet` is the replica set name the server's `hello` reports. migrune
  refuses a mongos, so there is always one.
- `deployment` is the UUID of the target database's `migrations_changelog`
  collection, from `listCollections`, in lowercase hex with hyphens. plan
  and apply create the collection when it's missing (see
  [changelog](changelog-and-lock.md#changelog-migrations_changelog)), so a
  first plan has one too. The server gives a collection a new UUID when it creates it, so two
  deployments whose database and replica set names are the same have
  different digests. plan and apply read it the same way, before taking the
  lock. A snapshot clone keeps the UUID, so a plan against a clone matches
  the original: harmless, since both hold the same changelog.
- `namespace` is `--namespace`, only when it is set: a default-namespace
  digest has no such line.
- `first-apply` is there only when the changelog has no entry for the
  namespace ([first apply](#first-apply)), read before the lock like
  `deployment`. A plan made for a first apply doesn't match once the
  namespace has entries, and the other way round.
- One `migration` line per pending migration, sorted by id, with the
  SHA-256 of its file.

So apply against another deployment, database, replica set or namespace,
with a migration added, edited or applied since, or after the namespace's
first apply happened elsewhere, fails before running
anything, and its error names each part that differs, the deployment first.

The file holds the digest on its first line, followed by the input:

```txt
df1e3c5a831e9ec5ad4a8258bf79647d2243754b830c3132c6cb21b1d3a1613d
migrune-digest-v1
database app
replicaSet rs0
deployment 0f8fad5b-d9cb-469f-a165-70867728950e
migration 2026-11-01-001-a aaaa
```

apply refuses a file whose digest isn't the SHA-256 of the rest, such as one
edited or cut short, as a usage error (exit 2). To check a file by hand:

```sh
tail -n +2 plan.digest | shasum -a 256
```
