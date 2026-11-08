# Running migrations

migrune has three commands:

- `lint` checks the files offline; with a connection, it also checks that no
  applied migration was edited or deleted, read-only.
- `plan` takes the migration lock, runs every pending migration without
  committing anything and prints what apply would do.
- `apply` takes the lock and applies the pending migrations in id order,
  stopping at the first failure.

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
   `migrations_changelog` collection), the pending migration ids and their
   file checksums. Keep the report and the digest as job artifacts.
2. A person reads the report and approves the apply job.
3. **apply** runs `migrune apply --digest-in plan.digest`. It computes the
   digest of its own run first; if a migration was added, edited or applied
   in between, or apply targets another deployment, database or replica set
   than plan did, the digests differ, apply runs nothing and exits 1, naming
   what differs: plan again (see [the digest](reference/report.md#plan-digest)).

Both jobs need the same migrune version and the same commit. apply refuses
to run without `--digest-in` (exit 2); `--no-digest` (`MIGRUNE_NO_DIGEST`)
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

The plan report and logs are meant to be published, so merge samples show
field names and BSON types, never values. Leave `--sample-values` off in CI
unless its logs and artifacts are as protected as the database.

Run lint before plan in every pipeline that plans: in pull requests, and in
the release or tag pipeline too, as the first step of the plan job, as both
examples do. plan and apply don't run lint's rules, such as `server-js` or
`volatile-merge`, and a tag can point at a commit no pull request checked.
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

Pin an exact version and upgrade on purpose. After a failure, see
[troubleshooting](troubleshooting.md).

A tag can be moved; a digest can't. To pin the image itself, add the digest
of the version's image index, which
`docker buildx imagetools inspect ghcr.io/joaopaulosr95/migrune:<version>`
prints:

```txt
ghcr.io/joaopaulosr95/migrune:<version>@sha256:<digest>
```

Images, default and `-debug`, have a GitHub build-provenance
attestation, signed by migrune's Release workflow, that records the
repository, the workflow run and the commit it was built from. Check it with
the GitHub CLI before you pin a digest:

```sh
gh attestation verify \
  oci://ghcr.io/joaopaulosr95/migrune:<version>@sha256:<digest> \
  --repo joaopaulosr95/migrune
```

It exits non-zero unless an attestation from that repository matches the
digest.

## Large collections

plan isn't a cheap preview: it runs the migrations against the target
database, the production one included, as apply would, without committing.
On large collections, both put the load of the migrations themselves on the
primary, while holding the migration lock:

- **Data migrations** run twice in their transaction, in plan and in apply,
  and plan then aborts it. They are bounded by the server's transaction
  lifetime, 60 seconds by default, which is why they
  [stay small](writing-migrations.md#data-migrations).
- **Merge migrations**: plan runs each step's pipeline in full, without
  `$merge`, and compares every output document with its target. apply runs
  it up to three times: that comparison before, the pipeline with `$merge`,
  and the comparison again after (see
  [how the comparison works](writing-migrations.md#how-the-comparison-works)).
- **Index migrations**: plan reads the indexes and counts the documents a
  new TTL index would delete. apply builds each index with
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
`migrations_changelog` and `migrations_lock` collections. For lint in pull
requests, use another user with `read`.

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
