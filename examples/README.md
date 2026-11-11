# Examples

- [`docker-compose.yaml`](docker-compose.yaml): a single-node MongoDB replica
  set and a one-off `migrune apply --no-digest --init` service (the database's first apply), built from the repository's
  `Dockerfile`. Run `docker compose up --build` here to apply the sample
  migrations, and `docker compose down -v` to discard everything. The
  [getting started guide](../docs/getting-started.md) walks through it.
- [`migrations/`](migrations/): one migration of each kind: a data migration
  seeding reference data, an index migration and a merge migration.
- [`.gitattributes`](.gitattributes): keeps git from converting the line
  endings of the migrations, which would change their checksums.
- [`ci/github-actions.yaml`](ci/github-actions.yaml) and
  [`ci/gitlab-ci.yml`](ci/gitlab-ci.yml): a lint job for pull or merge
  requests; on release tags, a plan job that publishes the plan digest, then
  an apply job behind a manual approval that consumes it. See
  [Running migrations](../docs/running.md#in-ci-with-an-approval-gate).
