# Getting started

Start a local MongoDB with the example in [`examples/`](../examples/), apply
its sample migrations, then write, plan and apply a migration of your own.
You need Docker with Compose v2 and a clone of this repository; migrune is
built from the repository's `Dockerfile` and runs in a container.

## 1. Start MongoDB

Run every command from `examples/`:

```sh
cd examples
docker compose up -d --wait mongo
```

This starts a single-member replica set (migrune needs transactions, which
MongoDB only has on replica sets) under the Compose project
`migrune-example`, published on `127.0.0.1:27117`. The `migrune` service in
[`docker-compose.yaml`](../examples/docker-compose.yaml) is configured through
environment variables, so the commands below need no flags:

```yaml
environment:
  MIGRUNE_URI: mongodb://mongo:27017/?replicaSet=rs0
  MIGRUNE_DB: example
  MIGRUNE_DIR: /migrations
```

## 2. Lint the sample migrations

[`examples/migrations/`](../examples/migrations/) holds one migration of each
kind: a **data** migration seeding currencies and plans, an **index**
migration creating `currency_1`, and a **merge** migration copying each
currency's symbol onto its plans.

```sh
docker compose run --rm migrune lint
```

The first run builds the image, which takes a minute. migrune then logs
(timestamps shortened):

```txt
time=… level=INFO msg=connecting hosts=mongo:27017 db=example
time=… level=INFO msg="lint passed" warnings=0
```

## 3. Plan

```sh
docker compose run --rm migrune plan
```

Logs go to stderr and the report to stdout; only the report is shown here and
below:

```txt
Plan: each status is what apply would do. Nothing was committed.
Database: example (new: it had no collections)
First apply: the changelog has no entries for this namespace; apply needs --init

MIGRATION                                 KIND   STATUS   DURATION  ERROR
2026-11-01-001-seed-currencies-and-plans  data   applied  85ms
  /steps/0 updateOne on currencies: matched 0, modified 0, upserted 1
  /steps/1 updateOne on currencies: matched 0, modified 0, upserted 1
  /steps/2 updateOne on plans: matched 0, modified 0, upserted 1
  /steps/3 updateOne on plans: matched 0, modified 0, upserted 1
2026-11-02-001-plans-currency-index       index  applied  13ms
  index to create: currency_1 on plans, keys {"currency":1}
2026-11-03-001-copy-currency-symbol       merge  applied  8ms
  merge: diffCountBefore 0, unmatchedCount 0, typeChangedCount 0, duplicateOnCount 0, nullOnCount 0, arrayOnCount 0, idMismatchCount 0

Total: 3 migrations: 3 applied, 0 errored, 0 interrupted, 0 skipped in 169ms
```

plan ran the data migration twice in a transaction and aborted it, read the
indexes without building any, and ran the merge pipeline without its `$merge`.
Each data step's line shows what its first run did: every seed would insert
one document. Nothing changed, which is why the merge sees
`diffCountBefore 0`: the plans aren't seeded yet. The report names the
target database, marked new because it has no collections yet, and says this
is the first apply: the changelog has no entries yet, so every migration is
pending. plan also logs it as a warning. Expected here, but against a
database that should already be migrated, it means a mistyped `--db` (or
`--namespace`).

## 4. Apply

apply refuses a first apply unless you confirm it:

```sh
docker compose run --rm migrune apply --no-digest
```

```txt
Error: the changelog has no entries for the default namespace on database example, so apply would run every migration; check --db and --namespace, and pass --init if this is the first apply
```

It exits 1 without writing anything. This is the first apply, so pass
`--init`:

```sh
docker compose run --rm migrune apply --no-digest --init
```

```txt
Database: example (new: it had no collections)
First apply: the changelog had no entries for this namespace

MIGRATION                                 KIND   STATUS   DURATION  ERROR
2026-11-01-001-seed-currencies-and-plans  data   applied  35ms
  /steps/0 updateOne on currencies: matched 0, modified 0, upserted 1
  /steps/1 updateOne on currencies: matched 0, modified 0, upserted 1
  /steps/2 updateOne on plans: matched 0, modified 0, upserted 1
  /steps/3 updateOne on plans: matched 0, modified 0, upserted 1
2026-11-02-001-plans-currency-index       index  applied  141ms
  index created: currency_1 on plans, keys {"currency":1}
2026-11-03-001-copy-currency-symbol       merge  applied  22ms
  merge: diffCountBefore 2, diffCountAfter 0, unmatchedCount 0, typeChangedCount 0, duplicateOnCount 0, nullOnCount 0, arrayOnCount 0, idMismatchCount 0
  differs at /steps/0 {"_id":"basic"}: currencySymbol: missing -> string
  differs at /steps/0 {"_id":"pro"}: currencySymbol: missing -> string

Total: 3 migrations: 3 applied, 0 errored, 0 interrupted, 0 skipped in 206ms
```

`--no-digest` is for local runs like this one: by default apply needs the
digest file plan writes with `--digest-out`, so it runs exactly what was
planned and approved. In CI, apply runs with the plan's digest (see
[running in CI](running.md#in-ci-with-an-approval-gate)). `--init` is for
this one run: once the changelog has entries, apply refuses it (see
[first deploy](running.md#first-deploy)).

This time the seeds were committed first, so the merge found two plans to
change, and nothing left to change after `$merge` (`diffCountAfter 0`). Each
sample names the field `$merge` sets and its BSON types, missing before and a
string after; `--sample-values` would show the values too, which plan and
apply leave out by default to keep data out of CI logs. Each migration now
has a `Finished` entry in `migrations_changelog`, so the next run leaves it
alone.

## 5. Write a migration

Create `migrations/2026-11-07-001-activate-plans.json`:

```json
{
  "id": "2026-11-07-001-activate-plans",
  "description": "Mark every plan without an active flag as active",
  "steps": [
    {
      "collection": "plans",
      "operation": "updateMany",
      "filter": { "active": { "$exists": false } },
      "update": { "$set": { "active": true } }
    }
  ]
}
```

The `id` repeats the file name.

Now add one that isn't idempotent, `migrations/2026-11-07-002-raise-prices.json`:

```json
{
  "id": "2026-11-07-002-raise-prices",
  "description": "Raise every price by one",
  "steps": [
    {
      "collection": "plans",
      "operation": "updateMany",
      "filter": {},
      "update": { "$inc": { "monthlyPrice": 1 } }
    }
  ]
}
```

```sh
docker compose run --rm migrune plan
```

```txt
Plan: each status is what apply would do. Nothing was committed.
Database: example

MIGRATION                      KIND  STATUS   DURATION  ERROR
2026-11-07-001-activate-plans  data  applied  21ms
  /steps/0 updateMany on plans: matched 2, modified 2, upserted 0
2026-11-07-002-raise-prices    data  errored  15ms      /steps/0 (updateMany on plans, second run): not idempotent: running the migration again modified 2 documents; a step must change nothing once applied (filter on the target state, use $set instead of $inc)
  /steps/0 updateMany on plans: matched 2, modified 2, upserted 0

Total: 2 migrations: 1 applied, 1 errored, 0 interrupted, 0 skipped in 208ms

Error: migration 2026-11-07-002-raise-prices failed: /steps/0 (updateMany on plans, second run): not idempotent: running the migration again modified 2 documents; a step must change nothing once applied (filter on the target state, use $set instead of $inc)
```

Only the two new migrations are pending. migrune runs every data migration
twice in one transaction and fails if the second run changes anything, so the
`$inc` fails in plan, before anyone approves it. plan exits 1 and writes
nothing. Delete that file:

```sh
rm migrations/2026-11-07-002-raise-prices.json
```

## 6. Apply your migration

```sh
docker compose run --rm migrune apply --no-digest
```

```txt
Database: example

MIGRATION                      KIND  STATUS   DURATION  ERROR
2026-11-07-001-activate-plans  data  applied  9ms
  /steps/0 updateMany on plans: matched 2, modified 2, upserted 0

Total: 1 migration: 1 applied, 0 errored, 0 interrupted, 0 skipped in 18ms
```

Look at the changelog with the `mongosh` in the MongoDB container:

```sh
docker compose exec mongo mongosh --quiet example --eval 'db.migrations_changelog.find({}, {_id: 0, migrationId: 1, status: 1}).sort({migrationId: 1})'
```

```txt
[
  {
    migrationId: '2026-11-01-001-seed-currencies-and-plans',
    status: 'Finished'
  },
  {
    migrationId: '2026-11-02-001-plans-currency-index',
    status: 'Finished'
  },
  {
    migrationId: '2026-11-03-001-copy-currency-symbol',
    status: 'Finished'
  },
  { migrationId: '2026-11-07-001-activate-plans', status: 'Finished' }
]
```

Each entry also stores the file's checksum, the runner, the step results and
the migration as applied. Editing an applied file now makes plan and apply
fail: write a new migration instead. Running apply again does nothing:

```sh
docker compose run --rm migrune apply --no-digest
```

```txt
Database: example

No pending migrations.

Total: 0 migrations: 0 applied, 0 errored, 0 interrupted, 0 skipped in 12ms
```

## 7. Clean up

```sh
docker compose down -v
rm migrations/2026-11-07-001-activate-plans.json
```

Next: [write your own migrations](writing-migrations.md) and
[run them in CI](running.md).
