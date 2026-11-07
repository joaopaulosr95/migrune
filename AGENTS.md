# AGENTS

migrune is a generic, public Go CLI that runs MongoDB migrations. Keep code,
docs and examples free of company names, internal paths and provider-specific
assumptions.

## Layout

```txt
cmd/migrune/          package main: signal handling, exit codes, the final error (text or a JSON log record)
internal/cmd/          cobra commands, env binding, slog setup
internal/cmd/gendocs/  CLI reference generator (not part of the binary)
internal/migration/    load, validate (embedded schema.json) and parse migration files
test/replicaset/       3-node replica set for integration tests (image from MONGO_IMAGE), smoke test
docs/                  writing-migrations, design;
                       reference/ (migration file, env and exit codes; cli/ is generated)
examples/              migrations/ (a data migration)
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
- `task lint`: go vet, staticcheck, gofmt, actionlint on the workflows, CLI docs up to date.
- `task docs`: regenerate `docs/reference/cli/`.

Done means `task lint test test:int:all` passes.

## Conventions

- Dependencies: cobra/pflag, mongo-driver v2, jsonschema v6, stdlib. Ask before adding any other.
- Every flag is also `MIGRUNE_<FLAG>`; precedence is flag > env > default. No viper.
- Logs: `log/slog` to stderr.
- Exit codes: 0 ok, 1 failure, 2 usage/config (`*cmd.UsageError`), 128+n when a signal
  interrupted the command (not one after it).
- Tests: table tests, fixtures under `testdata/`, no mocks of the mongo driver.
  Integration tests skip under `-short` or without `MIGRUNE_TEST_URI`, and use a fresh database each.
- Commits and PR titles: Conventional Commits.
- A breaking change to a stable interface (commands, flags, exit codes, migration schema,
  JSON output, images) is a major release.
- Docs describe the code as built. When behaviour, a flag or the output changes, update
  `docs/reference/` and the guide that covers it (and `task docs` for the CLI pages) in the
  same change. Explain each fact in one place and link to it; don't repeat it across pages.
