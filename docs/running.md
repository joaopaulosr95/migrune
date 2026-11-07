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
migrune plan  --dir migrations --db app
migrune apply --dir migrations --db app
```

Every flag is also an environment variable (`--db` is `MIGRUNE_DB`); see
[environment variables](reference/env-and-exit-codes.md). The report goes to
stdout and logs to stderr, so redirect stdout to keep the report; `-o json`
gives a [machine-readable report](reference/report.md).

## In CI, with an approval gate

Run two jobs per environment: **plan**, which runs on its own and publishes
the plan, and **apply**, which waits for someone to approve it and applies
the migrations. Make the deploy job depend on apply, so a failed
migration blocks the deploy. Ready-to-adapt files:
[GitHub Actions](../examples/ci/github-actions.yaml) and
[GitLab CI](../examples/ci/gitlab-ci.yml).

1. **plan** runs `migrune plan`. Keep its report as a job artifact.
2. A person reads the report and approves the apply job.
3. **apply** runs `migrune apply`, against the same database. It applies the
   migrations pending when it starts, checking each one again as it runs it.

Both jobs need the same migrune version and the same commit, so apply runs
the files plan ran. The approval is your CI's: who may start the apply job
(see [design](design.md#what-migrune-doesnt-guarantee)).

plan isn't read-only: it takes the lock (a write to `migrations_lock`) and
opens transactions it aborts. Give it the same credentials as apply, and run
it only in pipelines you trust with them, such as tag or default-branch
pipelines, never in pull request pipelines that run the branch's own CI file.

Run lint before plan in every pipeline that plans: in pull requests, and in
the release or tag pipeline too, as the first step of the plan job, as both
examples do. plan and apply don't run lint's rules, such as `server-js`, and
a tag can point at a commit no pull request checked.
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
- **Go**: `go install github.com/joaopaulosr95/migrune/cmd/migrune@<version>`,
  as in the GitLab example. The image is distroless, with no shell, so it
  can't be a job image in CI systems that run scripts inside it.

Pin an exact version and upgrade on purpose. After a failure, see
[troubleshooting](troubleshooting.md).

## Large collections

plan isn't a cheap preview: it runs the migrations against the target
database, the production one included, as apply would, without committing.
On large collections, both put the load of the migrations themselves on the
primary, while holding the migration lock:

- **Data migrations** run twice in their transaction, in plan and in apply,
  and plan then aborts it. They are bounded by the server's transaction
  lifetime, 60 seconds by default, which is why they
  [stay small](writing-migrations.md#data-migrations).
- **Index migrations**: plan reads the indexes and counts the documents a
  new TTL index would delete. apply builds each index with
  `createIndexes` and waits for it: migrune sets no `commitQuorum`, so the
  server's default applies (every data-bearing voting member), and no time
  limit.

migrune sets no `maxTimeMS` on any of these, and no overall deadline: an
index build runs as long as the server takes. Bound the job instead, with
your CI system's job timeout (`timeout-minutes` in GitHub Actions, `timeout`
in GitLab CI), set above the longest run you expect. A timed-out job is cancelled like any other: see
[a run was interrupted](troubleshooting.md#a-run-was-interrupted). An index
build in progress is left to finish on the server.

## Credentials

Pass the connection string as `MIGRUNE_URI`, from a secret or protected
variable, never as `--uri`: process arguments show up in `ps` and in job
logs. migrune never logs or prints the URI, not even in error messages: it
logs the database it connects to.

The database user needs `readWrite` on the target database: it covers data
changes, `createIndex` and `dropIndex`, and the
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
migrune apply --dir migrations --db app --mutable
```

- It only exists on apply. plan, and lint with a connection, still fail on
  an edited migration.
- The migration must still be idempotent: the re-run applies it on top of its
  earlier effect.
- A migration whose file was deleted still stops the run.
- Never use it against a shared environment: other databases would keep the
  old version. Once a migration has reached a shared environment, write a new
  one instead.
