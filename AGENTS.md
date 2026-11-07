# AGENTS

migrune is a generic, public Go CLI that runs MongoDB migrations. Keep code,
docs and examples free of company names, internal paths and provider-specific
assumptions.

## Layout

```txt
cmd/migrune/          package main: signal handling, exit codes, the final error (text or a JSON log record)
internal/cmd/          cobra commands, env binding, slog setup, connection
internal/cmd/gendocs/  CLI reference generator (not part of the binary)
internal/inttest/      keeps integration test packages from sharing the replica set (fails them without -p 1)
internal/migration/    load, validate (embedded schema.json) and parse migration files
internal/lint/         offline lint: per-file rules, index replay, expected indexes
internal/runner/       plan/apply: pending set, data migrations (double-run), index migrations, report;
                       lock, heartbeat and fence, changelog, format versions (v), graceful shutdown
test/replicaset/       3-node replica set for integration tests (test commands on, for fail points; image from MONGO_IMAGE), smoke test
docs/                  getting-started, writing-migrations, running, troubleshooting, design;
                       reference/ (migration file, lint rules, report, changelog and lock, env and exit codes; cli/ is generated)
examples/              docker-compose.yaml (1-node replica set + migrune apply), migrations/ (one per kind),
                       ci/ (GitHub Actions and GitLab CI plan/approve/apply snippets)
.github/workflows/     ci.yml (PRs, reusable), release.yml (main)
```

## Tasks

- `task setup`: install pinned tools (mise).
- `task build`, `task dev -- <args>`.
- `task test`: unit tests (`go test -short`).
- `task test:int`: starts `test/replicaset/` (default `mongo:9`), runs all tests with
  `MIGRUNE_TEST_URI`. `MONGO_IMAGE=mongo:7.0 task test:int` runs them on another image;
  switch back with `task replicaset:down` or a plain `task test:int`.
- `task test:int:all`: `test:int` on every tested image in turn (README "Requirements"),
  stops at the first failure, leaves `mongo:9` running. CI runs a subset
  (see the matrix in `.github/workflows/ci.yml`).
- `task lint`: go vet, staticcheck, gofmt, actionlint on the workflows and the GitHub Actions example, CLI docs up to date.
- `task docs`: regenerate `docs/reference/cli/`.

Done means `task lint test test:int:all` passes.

## Conventions

- Dependencies: cobra/pflag, mongo-driver v2, jsonschema v6, stdlib. Ask before adding any other.
- Every flag is also `MIGRUNE_<FLAG>`; precedence is flag > env > default. No viper.
- Logs: `log/slog` to stderr. Reports go to stdout. Never log or echo the URI.
- Exit codes: 0 ok, 1 failure, 2 usage/config (`*cmd.UsageError`), 3 lock held
  (`runner.ErrLockHeld`), 128+n when a signal interrupted the run (not one after it).
- Tests: table tests, fixtures under `testdata/`, no mocks of the mongo driver.
  Integration tests skip under `-short` or without `MIGRUNE_TEST_URI`, and use a fresh database each.
- Commits and PR titles: Conventional Commits.
- A breaking change to a stable interface (commands, flags, exit codes, migration schema,
  changelog/lock formats, JSON output, images) is a major release.
- Docs describe the code as built. When behaviour, a flag or the report changes, update
  `docs/reference/` and the guide that covers it (and `task docs` for the CLI pages) in the
  same change. Explain each fact in one place and link to it; don't repeat it across pages.
- `examples/migrations/` must pass `migrune lint`, and `docs/getting-started.md`'s commands
  must still produce the output it shows: run them against `examples/docker-compose.yaml`
  (project `migrune-example`, port 27117, separate from `test/replicaset/`), then
  `docker compose -f examples/docker-compose.yaml down -v`.
- `examples/ci/github-actions.yaml` must pass actionlint (part of `task lint`).
- Changelog and lock formats: `TestFormatGolden` (`internal/runner/testdata/format/`) pins them.
  An optional field older runners can ignore keeps `v`; a changed meaning, a newly required
  field or a changed read filter raises `v`, which is a major release (CONTRIBUTING,
  "Changelog and lock formats"). Existing fixtures in `internal/migration/testdata/valid/`
  are never edited, only added to.
