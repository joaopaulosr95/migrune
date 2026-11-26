# Running migrations

migrune has six commands:

- `lint` checks the files offline; with a connection, it also checks that no
  applied migration was edited or deleted, read-only.
- `plan` takes the migration lock, runs every pending migration without
  committing anything and prints what apply would do.
- `apply` takes the lock and applies the pending migrations in id order,
  stopping at the first failure.
- `drift` compares the indexes on the server with the indexes the applied
  migrations leave, read-only.
- `status` shows one namespace's migrations and lock, read-only.
- `unlock` removes a dead holder's lock, matching the token status showed.

```sh
export MIGRUNE_URI='mongodb://localhost:27017/?replicaSet=rs0'
migrune lint  --dir migrations --db app
migrune plan  --dir migrations --db app --digest-out plan.digest
migrune apply --dir migrations --db app --digest-in plan.digest
```

Every flag is also an environment variable (`--db` is `MIGRUNE_DB`); see
[environment variables](reference/env-and-exit-codes.md). The report goes to
stdout and logs to stderr, so redirect stdout to keep the report; `-o json`
gives a [machine-readable report](reference/report.md).

## Several apps on one database

By default, a migrations directory owns its database: the changelog holds
every migration applied to it, so a migration applied from another directory
shows up as [applied but missing](troubleshooting.md#applied-but-missing) and
plan and apply fail. When several apps, each with its own migrations directory,
migrate one database, give each app a namespace with `--namespace`
(`MIGRUNE_NAMESPACE`), the same on lint, plan, apply, drift, status and unlock:

```sh
migrune plan  --dir migrations --db app --namespace billing --digest-out plan.digest
migrune apply --dir migrations --db app --namespace billing --digest-in plan.digest
```

A namespace is 1 to 64 lowercase letters, digits, `_` or `-`. Without the
flag, a run uses the default namespace. A namespace scopes:

- **the changelog reads**: plan, apply and lint only consider their
  namespace's entries, so another app's migrations are neither pending nor
  applied but missing. Every namespace's entries stay in the one
  `migrations_changelog`, each with its
  [`namespace`](reference/changelog-and-lock.md#changelog-migrations_changelog);
- **the lock**: one per namespace, so apps on different namespaces migrate at
  the same time;
- **the [plan digest](reference/report.md#plan-digest)**: a plan made for one
  namespace doesn't approve another's apply.

A new namespace's first apply needs `--init`, like any
[first deploy](#first-deploy).

It doesn't scope collections or indexes: every namespace migrates the same
database, and plan's index preview sees every index `listIndexes` returns,
whichever namespace created it. drift replays only its namespace's migrations,
so another namespace's indexes are `index-extra` warnings there.

### Each namespace owns its collections

Two namespaces that touch the same collection can run at the same time, since
their locks differ: their data migrations interleave, their index builds
collide, and a plan approved in one can go stale when the other applies in
between, which its digest doesn't see. Apps that must share a collection share
a namespace: one migrations directory and one lock, so their migrations run
one at a time, in one order.

plan and apply warn about each pending migration that writes a collection
another namespace's changelog entries wrote, naming those namespaces, and
carry on. A step writes its `collection`, except a merge step, which writes
its `$merge` target.

Sharing a collection between apps is a bad practice in general, with or
without migrune. It is the shared-database integration pattern: the
collection's schema becomes an undeclared contract between the apps, so a
migration in one can break the other; changing it needs coordinated deploys;
and nobody clearly owns the data, its indexes or its validation. Prefer one
app that owns the collection and that the others reach through an API or
events. If sharing is unavoidable, share the namespace too.

### Moving onto namespaces

An app already deployed stays in the default namespace: its entries have no
`namespace` field, which is what the default namespace reads. Give the apps
that join the database their own namespaces. To name the deployed app's
namespace too, stop its deploys, rename its entries once, with a user allowed
to write to `migrations_changelog`, and deploy it with `--namespace <name>`
from then on:

```js
db.migrations_changelog.updateMany({namespace: {$exists: false}}, {$set: {namespace: "<name>"}})
```

Every runner on a database with namespaces needs a migrune version that has
them: an older one reads every namespace's entries as its own and fails with
applied but missing before doing anything.

## In CI, with an approval gate

Run two jobs per environment: **plan**, which runs on its own and publishes
the plan, and **apply**, which waits for someone to approve it and applies
exactly what was planned. Make the deploy job depend on apply, so a failed
migration blocks the deploy. Ready-to-adapt files:
[GitHub Actions](../examples/ci/github-actions.yaml) and
[GitLab CI](../examples/ci/gitlab-ci.yml).

1. **plan** runs `migrune plan --digest-out plan.digest`. Once every
   migration passed, it writes the **plan digest**, a hash of the target
   database and replica set names, the deployment (the UUID of its
   `migrations_changelog` collection), whether it is the namespace's
   [first deploy](#first-deploy), the pending migration ids and their file
   checksums. Keep the report and the digest as job artifacts.
2. A person reads the report and approves the apply job.
3. **apply** runs `migrune apply --digest-in plan.digest`. It computes the
   digest of its own run first; if a migration was added, edited or applied
   in between, or apply targets another deployment, database or replica set
   than plan did, the digests differ, apply runs nothing and exits 1, naming
   what differs: plan again (see [the digest](reference/report.md#plan-digest)).

### First deploy

A namespace's first apply, against a database whose changelog has no entries
for it, runs every migration. So does an apply with a mistyped `--db` or
`--namespace`, so apply refuses it unless `--init` (`MIGRUNE_INIT=true`) is
set (see [first apply](reference/report.md#first-apply)). The first deploy
of each namespace in each environment is:

1. **plan**, as usual. It warns `first apply for namespace <name> on
   database <db>`, its report says `First apply:` and its digest records it.
2. **Review**: every migration is pending; check the database and namespace
   are the ones you meant.
3. **apply `--init`**, with the plan's digest. Set `--init` for this run
   only, for instance as a manual input of the apply job: apply refuses
   `--init` once the namespace has entries, so a pipeline that keeps it
   fails on its next deploy instead of silently losing the check.

Every later apply runs without `--init`. A plan made for the first apply
doesn't match an apply after the namespace got entries, and the other way
round.

Both jobs need the same migrune image digest and migration commit. apply
refuses to run without `--digest-in` (exit 2); `--no-digest` (`MIGRUNE_NO_DIGEST`)
lifts that, for local runs only. The digest proves apply runs what plan saw,
not that anyone approved it: the approval is your CI's, who may start the
apply job and where its digest file comes from (see
[design](design.md#what-migrune-doesnt-guarantee)).

plan isn't read-only: it takes the lock (a write to `migrations_lock`),
creates the empty `migrations_changelog` collection if it's missing (see
[changelog](reference/changelog-and-lock.md#changelog-migrations_changelog)), and
opens transactions it aborts. Give it the same credentials as apply, and run
it only in pipelines you trust with them, such as tag or default-branch
pipelines, never in pull request pipelines that run the branch's own CI file.

The plan report and logs are meant to be published, so they leave document
values out: merge samples show field names and BSON types, and server errors
show their code name and code but not the server's message, which can quote
values (see [values in errors](reference/report.md#values-in-errors)). Two things are
shown anyway, so a document can be found: `_id`s in errors, and the `on`
fields of [merge samples](reference/report.md#merge-samples), which are
values when `on` is a natural key such as an email address. Leave
`--sample-values` off in CI unless its logs and artifacts are as protected
as the database.

plan and apply run lint's offline rules themselves: an error finding (such as
`server-js`) in a file they could run prints like `migrune lint` does and
fails the command with the same exit status, and warnings are logged. Errors in
already-applied migrations are only logged (see
[lint in plan and apply](reference/lint-rules.md#in-plan-and-apply)). Keep lint as a
pull-request check, and in the release or tag pipeline as the first step of
the plan job, as both examples do, so authors see findings before a deploy.
lint needs no database; add `MIGRUNE_URI` and `--db`, with a read-only user
in pull requests, to also catch edited or deleted applied migrations, and
`--expected-indexes <file>` if your application declares indexes (see
[lint rules](reference/lint-rules.md#expected-indexes)).

### Record who approved

Every `Finished` changelog entry records `approvedBy` from
`--approved-by-name` and `--approved-by-email`. It is a label the apply job
sets: migrune doesn't verify it. migrune reads no CI variable itself; map your provider's onto `MIGRUNE_APPROVED_BY_NAME` and
`MIGRUNE_APPROVED_BY_EMAIL`:

| Provider       | Name                                    | Email                                         |
| -------------- | --------------------------------------- | --------------------------------------------- |
| GitLab CI      | `GITLAB_USER_LOGIN`                     | `GITLAB_USER_EMAIL`                           |
| GitHub Actions | the reviewer's login, from the run's [approvals](https://docs.github.com/en/rest/actions/workflow-runs#get-the-review-history-for-a-workflow-run) | `<id>+<login>@users.noreply.github.com`       |

In GitLab, `GITLAB_USER_*` in a manual job is the user who started it, so the
manual apply job is the approval: protect the environment to choose who may
start it, and add deployment approvals for a second pair of eyes. Record the
login, which is unique, rather than `GITLAB_USER_NAME`, a display name anyone
can set to anything. GitHub has no variable for the person who approved an
environment, so the example reads the run's approvals with `gh api`, and
fails when there is none: required reviewers on the environment are the
gate, and a missing approval is never recorded as someone else's.

Whoever can create a release tag runs the plan and apply jobs as written at
that tag, with the production credentials. Protect the tags the pipeline runs
on (`v*` in the examples): a tag ruleset in GitHub, protected tags in GitLab,
which also gate GitLab's protected variables. In GitHub, also limit the
environments that hold `MIGRUNE_URI` to those tags, so a branch's copy of
the workflow can't read it.

### Get the binary

- **Docker**: run `ghcr.io/joaopaulosr95/migrune:<version>` with
  `docker run`, as in the GitHub example. Mount the repository and pass
  variables with `-e NAME` (no value), so they come from the job's
  environment.
- **Job image**: CI systems that run job scripts inside the image, such as
  GitLab CI, need a shell, which the default image lacks. Use
  `ghcr.io/joaopaulosr95/migrune:<version>-debug`, which adds a busybox
  shell, and clear its entrypoint (`entrypoint: [""]`), as in the GitLab
  example.
- **Go**: `go install github.com/joaopaulosr95/migrune/cmd/migrune@<version>`.

In CI, use the same verified image-index digest for plan and apply. A tag
can move, including an exact version tag. Get the index digest from
`docker buildx imagetools inspect ghcr.io/joaopaulosr95/migrune:<version>`
and pin it together with the version tag. After a failure, see
[troubleshooting](troubleshooting.md).

```txt
ghcr.io/joaopaulosr95/migrune:<version>@sha256:<digest>
```

Images, default and `-debug`, have a GitHub build-provenance
attestation, signed by migrune's Release workflow, that records the
repository, workflow run, and source commit. Get the expected commit from
the Git release tag (`git rev-parse 'v<version>^{commit}'` in a trusted
checkout). Verify the digest with the GitHub CLI before pinning it, using that
commit as `<release-commit>`. For the debug image, use its own index digest.

```sh
gh attestation verify \
  oci://ghcr.io/joaopaulosr95/migrune:<version>@sha256:<digest> \
  --repo joaopaulosr95/migrune \
  --signer-workflow joaopaulosr95/migrune/.github/workflows/release.yml \
  --source-ref refs/heads/main \
  --source-digest <release-commit>
```

## Large collections

plan isn't a cheap preview: it runs the migrations against the target
database, the production one included, as apply would, without committing.
On large collections, both put the load of the migrations themselves on the
primary, while holding the migration lock:

- **Data migrations** run twice in their transaction, in plan and in apply,
  and plan then aborts it. Before it, each `updateMany` and `deleteMany`
  step's matching documents are counted, up to 10,001. A step matching more
  than 10,000 documents fails, and the transaction is bounded by the
  server's transaction lifetime, 60 seconds by default, which is why they
  [stay small](writing-migrations.md#data-migrations). A
  [batched](writing-migrations.md#batched-data-migrations) migration runs
  each batch that way instead: plan counts every matching document and
  runs the first batch; apply reads the matching `_id`s with one cursor and
  runs every batch, a transaction each, so its run time grows with the
  collection while each transaction stays small.
- **Merge migrations** run each step's pipeline in plan without `$merge`
  to compare its output with the target. A successful single-step apply normally
  runs the pipeline three times, once before the write, once with `$merge`, and
  once after the write. Each comparison can add a [read to validate BSON scopes
  in catalog IDs](writing-migrations.md#catalog-upserts), bringing that apply
  to five runs if both comparisons need it. After the last write in a file
  containing an insert step, apply also [rechecks every earlier step](writing-migrations.md#how-the-comparison-works).
  Grouping insert candidates for duplicate detection uses memory proportional
  to the distinct output keys.
- **Index migrations**: plan reads the indexes, counts the documents a new
  or lowered TTL would delete, and checks a `collModIndex` that sets `unique`
  for duplicates. apply builds each index with
  `createIndexes` and waits for it: migrune sets no `commitQuorum`, so the
  server's default applies (every data-bearing voting member), and no time
  limit.

migrune sets no `maxTimeMS` on any of these, and no overall deadline: a
merge over a large collection, or an index build, runs as long as the server
takes. Bound the job instead, with your CI system's job timeout
(`timeout-minutes` in GitHub Actions, `timeout` in GitLab CI), set above the
longest run you expect. A timed-out job is cancelled like any other: see
[a run was interrupted](troubleshooting.md#a-run-was-interrupted). An index
build in progress is left to finish on the server.

As a rough guide only, not a benchmark: a merge step over 1 million
documents took 39 seconds in plan and 1 minute 48 seconds in apply, on the
local 3-node test replica set (`test/replicaset/`, three `mongod` processes
on one machine). Your timings depend on the documents, the pipeline, the
indexes and the hardware: time plan against a copy of production, or read
the durations in its report, before you choose the timeout.

## Detecting index drift

Indexes change outside migrations: someone drops a slow-to-build index during
an incident, adds one by hand from the shell, or changes a TTL with `collMod`.
`drift` finds these changes. It replays the migrations applied in the
namespace and compares the result with `listIndexes` on every collection:

```sh
migrune drift --dir migrations --db app
```

An applied index that is missing or has other options is an error, and drift
exits 1. An index no applied migration creates, or one under another name, is
a warning. Findings print as lint's do; see [index drift](reference/drift.md)
for what is compared and each finding.

Run it on a schedule, such as a nightly CI job per environment, with a user
with `read` and a checkout that has every applied migration file: the expected
indexes come from the changelog, but a missing or edited file is an error. It
takes no lock, so it can run while apply does; its findings may then show a
half-applied run, so run it again before acting on them. Fix a finding with a migration, which also
reviews the change, rather than by hand.

What it can't see:

- pending migrations: until apply runs them, their indexes aren't expected;
- `collModCollection` validation changes, and data;
- where an extra index comes from: another namespace, the application or a
  person;
- a collation field set by hand that the migration never gave (see
  [collation](reference/drift.md#what-is-compared)).

## Inspecting migrations and the lock

`status` reads one namespace's changelog and lock without taking the lock or
writing anything. Give it `--dir` to compare the files with the changelog:

```sh
migrune status --dir migrations --db app --namespace billing
migrune status --db app --namespace billing -o json > status.json
```

Without `--dir`, it shows only applied and failed migrations. It also shows
the holder's runner id, current migration, token and heartbeat, and when
unlock is allowed. A successful read exits 0 whatever the migration or lock
state. The states, JSON fields and handling of stored error messages are
in [status output](reference/status.md).

## Removing a dead holder's lock

Inspect the lock with status, then pass its token to unlock in the same
database and namespace:

```sh
migrune status --db app --namespace billing
migrune unlock --db app --namespace billing --token '<token from status>'
```

`--token` can also come from `MIGRUNE_TOKEN`. unlock removes only that
acquisition's lock, once its committed heartbeat is old enough; it has no
`--force` option. See [the lock is held](troubleshooting.md#the-lock-is-held)
for refusals and what to do next, and the
[unlock guard](reference/changelog-and-lock.md#unlock) for the threshold and
server-parameter fallback. After removal, run plan and apply again as after
an [interrupted run](troubleshooting.md#a-run-was-interrupted).

## Credentials

Pass the connection string as `MIGRUNE_URI`, from a secret or protected
variable, never as `--uri`: process arguments show up in `ps` and in job
logs. A `--uri` given on the command line logs a warning saying so, without
its value. migrune never logs or prints the URI, not even in error messages: it
logs the hosts it connects to, as the driver parsed them (resolved, for
`mongodb+srv://`), and the database. It sends `appName=migrune`, so its
operations show under that name in `currentOp`, the server logs and the
profiler, unless the URI sets its own `appName`.

The URI's `readPreference` and `readConcernLevel` are ignored: migrune
always [reads from the primary](design.md#guarantees), so an application's
URI with `secondaryPreferred` or `nearest` can be used as it is. A
`readPreference` other than `primary` logs a warning.

The database user needs `readWrite` on the target database: it covers data
changes, `createIndex` and `dropIndex`, `$merge`, and the
`migrations_changelog` and `migrations_lock` collections. For a custom role,
catalog upserts need read access to the inputs and target, metadata access
for the collection and index checks, and insert and update on the target.
Upgrade both the plan and apply binaries before deploying an insert file.
`collModIndex`
and `collModCollection` also need the `collMod` action, which `readWrite`
lacks: apply runs `collMod`, and plan runs it with `dryRun` for a
`collModIndex` that sets `unique`. plan checks the user has it, from
`connectionStatus`, and fails a step that would change its index or
validation otherwise. Grant it with a
role of its own, for example:

```js
db.getSiblingDB("app").createRole({
  role: "migruneCollMod",
  privileges: [{ resource: { db: "app", collection: "" }, actions: ["collMod"] }],
  roles: [],
})
db.getSiblingDB("app").grantRolesToUser("<user>", ["migruneCollMod"])
```

For lint in pull requests, drift and status, use another user with `read`.
unlock needs permission to read the changelog and lock and delete from
`migrations_lock`; `readWrite` on the target database covers it. status and unlock also try to
read the server's transaction lifetime with `getParameter` on `admin`; see
the [fallback and its warning](reference/changelog-and-lock.md#unlock).

### AWS IAM

The MongoDB Go driver supports the `MONGODB-AWS` mechanism, on deployments
that offer it such as MongoDB Atlas, so there is no password to store. Create
the database user for the IAM role ARN that runs migrune, then choose the
mechanism in the URI, with no user name or password:

```sh
export MIGRUNE_URI='mongodb+srv://cluster0.example.net/?authSource=%24external&authMechanism=MONGODB-AWS'
migrune lint --dir migrations --db app   # fails on bad auth, changes nothing
```

`authSource=%24external` is `$external`, where these users live. The driver
then looks for credentials, in order:

1. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and, for temporary
   credentials, `AWS_SESSION_TOKEN`;
2. a web identity token: `AWS_WEB_IDENTITY_TOKEN_FILE` and `AWS_ROLE_ARN`
   (optionally `AWS_ROLE_SESSION_NAME`), as set by Kubernetes service account
   roles and CI OIDC integrations;
3. the ECS container credentials endpoint
   (`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`);
4. the EC2 instance metadata service.

On a runner or pod that already assumes the role, the URI is all you need.
Elsewhere, assume the role and export the temporary credentials, for example
with `eval "$(aws configure export-credentials --format env)"`. migrune reads
no AWS variable itself.

You can also put the credentials in the URI, URL-encoded: the access key id as
user name, the secret key as password, and the session token in
`authMechanismProperties=AWS_SESSION_TOKEN:<session-token>`. Prefer the
environment: temporary credentials expire, and a URI with secrets is one more
thing to keep out of logs.

## Locally, with `--mutable`

While you iterate on a migration against your own database, `apply --mutable`
re-runs `Finished` migrations whose file changed instead of failing. It runs
them again in id order with the pending ones, and writes a new `Finished`
entry, whose checksum later edits are compared with. It logs a warning for
each one.

```sh
migrune apply --dir migrations --db app --no-digest --mutable
```

- It only exists on apply. plan, and lint with a connection, still fail on
  an edited migration, so no plan digest can cover it: apply refuses
  `--mutable` without `--no-digest` as a usage error (exit 2).
- The migration must still be idempotent: the re-run applies it on top of its
  earlier effect.
- A migration whose file was deleted still stops the run.
- Never use it against a shared environment: other databases would keep the
  old version. Once a migration has reached a shared environment, write a new
  one instead.
