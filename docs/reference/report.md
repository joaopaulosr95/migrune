# Plan and apply report

Every `migrune plan` and `migrune apply` run ends with a report on stdout:
one row per migration considered, plus totals. Logs go to stderr, so the
report can be captured on its own:

```sh
migrune apply --dir migrations --db app -o json > report.json
```

The report is also written when the run fails or is interrupted; the
[exit code](env-and-exit-codes.md#exit-codes) still tells the outcome.

No report is written when the run stops before it holds the migration lock:
invalid flags, invalid migration files, an unreachable server, or a lock held
by another runner.

`--output` (`-o`) picks the format: `table` (the default) or `json`.

## Which migrations are considered

- **Pending** migrations: those with no `Finished` entry in the changelog, in
  id order.
- With `apply --mutable`, also the `Finished` migrations whose file changed.
- Without `--mutable`, a `Finished` migration whose file changed fails the run
  before anything runs. It is reported as `errored`, and every pending
  migration as `skipped`.
- A `Finished` migration whose file is no longer in `--dir` ("applied but
  missing", e.g. after a revert) fails the run before anything runs, with or
  without `--mutable`: every pending migration is `skipped`, the missing ones
  are listed in `appliedButMissing` and nothing is written to the changelog.
  See [troubleshooting](../troubleshooting.md#applied-but-missing).

Migrations already applied with an unchanged file are left out.

### New databases

When the target database has no collections, apart from `migrations_lock`,
which an earlier plan leaves behind, plan and apply log a warning, set
`newDatabase` to `true` and say `(new: it had no collections)` after the
database name in the table.
Migrations against a new database are expected on a first deploy; anywhere
else, check `--db` for a typo before approving.

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

Index rows (`kind` `index`) list their steps in `indexes`: in plan, what each
step would do to the indexes as they are now, changed by the pending index
migrations before it; in apply, what it did. A plan step predicted to fail, such as a
name already used by an index with other keys or options, is the last item,
with the reason in `conflict`, and makes the row `errored`. The same lines
also go to the logs on stderr, one per step.

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
| `startedAt`               | string           | When the run started, RFC 3339 in UTC to the millisecond, e.g. `2026-11-08T10:30:00.125Z`.                   |
| `runner`                  | string           | The runner id the run held the lock under, as in the logs and the changelog.                                 |
| `approvedBy`              | object, optional | apply only: `name` and `email`, as given by `--approved-by-name` and `--approved-by-email`, and recorded in the changelog. Absent in plan and when neither is set. |
| `newDatabase`             | boolean          | `true` when the target database had no collections, apart from `migrations_lock`, when the run started. See [new databases](#new-databases). |
| `migrations`              | array            | One row per migration considered, in id order. Empty when nothing is pending.                                |
| `migrations[].id`         | string           | The migration id: the file name without `.json`.                                                             |
| `migrations[].kind`       | string           | `data` or `index`.                                                                                           |
| `migrations[].status`     | string           | `applied`, `errored`, `interrupted` or `skipped`.                                                            |
| `migrations[].durationMs` | integer          | How long the migration ran, in milliseconds. 0 when skipped.                                                 |
| `migrations[].error`      | string, optional | Why the migration errored or was interrupted. Absent otherwise.                                              |
| `migrations[].steps`      | array, optional  | Data migrations only: one item per step, in file order, with the first run's counts (see below). Absent for other kinds, for `skipped` rows, and when the first run failed. |
| `migrations[].indexes`    | array, optional  | Index migrations only: one item per step that ran, in file order (see below). Absent for other kinds and for `skipped` rows. |
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
| `error`                   | string, optional | Why the run failed. Absent when it succeeded.                                                                |

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

Each item of `migrations[].indexes` is one step of an index migration:

| Field        | Type             | Description                                                                                                                   |
| ------------ | ---------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `action`     | string           | `create`, `drop`, `present` (a `createIndex` whose index already exists with that definition) or `absent` (a `dropIndex` whose index doesn't exist). In plan, what the step would do; in apply, what it did. plan also says `differs`: a `createIndex` whose index exists with a collation plan can't compare with the step's, because `listIndexes` shows it with every default filled in; see `warning`. |
| `collection` | string           | The step's collection.                                                                                                        |
| `name`       | string           | The index name.                                                                                                               |
| `keys`       | object, optional | The key pattern, as relaxed Extended JSON, in key order: the step's for `create` and `present`, the existing index's for `drop`. Absent for `absent`. |
| `options`    | object, optional | The index options other than its name, the same way. Absent when there are none.                                              |
| `conflict`   | string, optional | plan only: why MongoDB would reject the step. Only on the last item of an `errored` row.                                      |
| `wouldExpire`| integer, optional | plan only, on a `create` of a TTL index (`expireAfterSeconds`, one key): the documents it would delete as soon as it is built, those whose indexed date is older than now, by migrune's clock, less `expireAfterSeconds`, among those its `partialFilterExpression` selects. |
| `warning`    | string, optional | plan only: why apply may fail although plan found no conflict, on a `differs` step, or on a `create` next to an index with the same keys and options and a collation plan can't compare. |

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
collections)` for a [new database](#new-databases). With no row, the table
prints `No pending migrations.` in place of the header and rows; the JSON has
`"migrations": []`.

When the run failed, the table ends with its `error`, the same text as in the
JSON. A run that failed several checks at once has one line per check, the
lines after the first indented.

A data row is followed by one indented line per step: its index, operation
and collection, and the first run's counts.

An index row is followed by one indented line per step, with what the step
would do (plan: `to create`, `to drop`) or did (apply: `created`, `dropped`),
or `already present` / `already absent`. A step plan predicts to fail says
`would fail`, followed by the reason. A TTL index to create ends with its
`wouldExpire` count. A `differs` step says `already present,
collation not comparable`, and a step with a `warning` ends with it, after
`warning:`:

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
