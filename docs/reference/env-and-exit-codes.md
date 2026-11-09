# Environment variables and exit codes

The [CLI reference](cli/migrune.md) describes each command and flag; it is
generated from the code with `task docs`.

## Environment variables

Every flag can also be set through an environment variable: `MIGRUNE_`
followed by the flag name in upper case, with dashes replaced by
underscores. `--approved-by-name` is `MIGRUNE_APPROVED_BY_NAME`.

- **Precedence**: a flag on the command line wins over its environment
  variable, which wins over the default.
- A variable applies to every command that has the flag: `MIGRUNE_DB` sets
  `--db` for lint, plan and apply. Commands without the flag ignore it.
- Values are parsed like the flag's: a duration such as `90s` or `2m` for
  `MIGRUNE_LOCK_TTL` or `MIGRUNE_LOCK_WAIT`, `true` or `false` (also `1`, `0`, `t`, `f`) for
  `MIGRUNE_MUTABLE`. An invalid value is a usage error (exit code 2); the
  message names the variable but never repeats its value, which could be a
  credential.
- migrune reads no other variable itself: map your CI's variables onto
  these (see [running in CI](../running.md#record-who-approved)). The MongoDB
  driver reads the standard `AWS_*` variables for `MONGODB-AWS`
  authentication (see [AWS IAM](../running.md#aws-iam)).

Pass the connection string as `MIGRUNE_URI`, not `--uri`: process arguments
show up in `ps` and in CI logs.

| Variable                     | Flag                  | Commands          | Default                  | Description                                                                                         |
| ---------------------------- | --------------------- | ----------------- | ------------------------ | --------------------------------------------------------------------------------------------------- |
| `MIGRUNE_LOG_FORMAT`        | `--log-format`        | all               | `text`                   | Log format on stderr: `text` or `json`.                                                             |
| `MIGRUNE_LOG_LEVEL`         | `--log-level`         | all               | `info`                   | `debug`, `info`, `warn` or `error`.                                                                 |
| `MIGRUNE_DIR`               | `--dir`               | lint, plan, apply | none, required           | Directory holding the migration files.                                                              |
| `MIGRUNE_URI`               | `--uri`               | lint, plan, apply | none; required for plan and apply | MongoDB connection string. With lint, enables the online checks (with `--db`). Its `readPreference` and `readConcernLevel` are ignored: see [reads come from the primary](../design.md#guarantees). |
| `MIGRUNE_DB`                | `--db`                | lint, plan, apply | none; required for plan and apply, and for lint with `--uri` | Target database name.                                        |
| `MIGRUNE_NAMESPACE`         | `--namespace`         | lint, plan, apply | empty, the default namespace | The set of migrations `--dir` holds in the database: scopes the changelog reads, the lock and the digest. 1 to 64 lowercase letters, digits, `_` or `-`. lint checks it always but uses it only with `--uri`. See [several apps on one database](../running.md#several-apps-on-one-database). |
| `MIGRUNE_EXPECTED_INDEXES`  | `--expected-indexes`  | lint              | none                     | [Expected-indexes file](lint-rules.md#expected-indexes) to compare the index migrations with.                  |
| `MIGRUNE_OUTPUT`            | `--output`, `-o`      | lint, plan, apply | `table`; `text` for lint | Report format on stdout: `table` or `json`. See [the report](report.md). lint's findings: `text` (or `table`) or `json`; see [lint output](lint-rules.md#output). |
| `MIGRUNE_LOCK_TTL`          | `--lock-ttl`          | plan, apply       | `1m30s`                  | Shortest time the lock lives without a heartbeat, in whole seconds, at least `61s`. The lock expires after the longer of this and 3 heartbeat intervals (30s). Keep it above the server's `transactionLifetimeLimitSeconds`. See [the lock](changelog-and-lock.md#lock-migrations_lock). |
| `MIGRUNE_LOCK_WAIT`         | `--lock-wait`         | plan, apply       | `0s`                     | How long to keep trying, with backoff, to take a lock another runner holds. `0s` fails at once. A lock still held after the wait exits 3. |
| `MIGRUNE_SAMPLE_VALUES`     | `--sample-values`     | plan, apply       | `false`                  | Show the values of the fields that differ in merge samples, in the logs and the report, each cut to 200 characters. Without it, samples show field names, BSON types and nested paths only. See [merge samples](report.md#merge-samples). |
| `MIGRUNE_DIGEST_OUT`        | `--digest-out`        | plan              | none                     | File to write the plan digest to, once every migration passed.                                     |
| `MIGRUNE_DIGEST_IN`         | `--digest-in`         | apply             | none; required unless `--no-digest` | Plan digest file the run must match; apply runs nothing otherwise. See [plan digest](report.md#plan-digest). |
| `MIGRUNE_NO_DIGEST`         | `--no-digest`         | apply             | `false`                  | Run without a plan digest. Local use only; not with `--digest-in`.                                  |
| `MIGRUNE_MUTABLE`           | `--mutable`           | apply             | `false`                  | Re-run applied migrations whose file changed instead of failing. Local use only, with `--no-digest`; see [`--mutable`](../running.md#locally-with---mutable). |
| `MIGRUNE_APPROVED_BY_NAME`  | `--approved-by-name`  | apply             | empty                    | Name of the person who approved the run, recorded in `Finished` changelog entries.                  |
| `MIGRUNE_APPROVED_BY_EMAIL` | `--approved-by-email` | apply             | empty                    | Email of that person, recorded the same way.                                                        |
| `MIGRUNE_RUNNER_ID`         | `--runner-id`         | apply             | `<hostname>-<pid>-<random>` | Runner id for the lock, the logs and the changelog. plan always uses the default.               |

## Exit codes

| Code      | Meaning                                                                                                                         |
| --------- | ------------------------------------------------------------------------------------------------------------------------------- |
| 0         | Success. lint found no errors (warnings are allowed); plan found that every pending migration would apply; apply applied them. |
| 1         | Failure. lint found an error; a migration failed in plan or apply; an applied migration was edited or is missing; apply's `--digest-in` didn't match; the lock was lost; the migrations directory or a migration file is invalid; the server can't be reached, its `mongodb+srv://` DNS lookup fails, or it refuses the credentials (the message has the driver's cause); the server runs a MongoDB version older than 7.0, or is a standalone mongod or a mongos rather than a replica set member; the changelog or lock has a [newer format](changelog-and-lock.md#format-versions). |
| 2         | Usage or configuration error, found before any work: an unknown command or flag, a missing required flag, an invalid flag or `MIGRUNE_*` value, a `--uri` that isn't a MongoDB connection string (the message never repeats it), an unreadable `--expected-indexes` or `--digest-in` file or one of another digest version, apply with neither `--digest-in` nor `--no-digest`, or with both, `apply --mutable` without `--no-digest`. |
| 3         | The migration lock is held by another runner, after `--lock-wait` if set. Nothing ran and no report is printed. Retry later, or raise `--lock-wait`. See [the lock is held](../troubleshooting.md#the-lock-is-held). |
| 128 + *n* | Interrupted by signal *n*: 130 for SIGINT (Ctrl+C), 143 for SIGTERM. A second signal exits at once with the first signal's code: 143 for a Ctrl+C after a SIGTERM. See [interrupted runs](../troubleshooting.md#a-run-was-interrupted) and [when a signal counts](#when-a-signal-counts). |

On any error, migrune prints `Error: <message>` to stderr; for exit code 2
it adds `Run 'migrune --help' for usage.` With `--log-format json`, it
writes one JSON log record instead, like the other logs, with the message as
`msg` and the exit code as `exitCode`:

```json
{"time":"2026-11-10T13:43:56.0145-03:00","level":"ERROR","msg":"--dir is required","exitCode":2}
```

The format comes from `--log-format`, or `MIGRUNE_LOG_FORMAT`, even when
the error is another flag's, such as an unknown flag; an invalid
`--log-format` is reported as text. When the [report](report.md) already
shows the error and stdout and stderr are the same file, as in a terminal,
the error is left out of stderr, in either format.

plan and apply print their report to stdout whenever the run got as far as
holding the migration lock, failures and interruptions included, so the exit
code and the report always agree. lint's findings go to stdout too (see
[lint rules](lint-rules.md#output)).

### When a signal counts

A signal sets the exit code only when it arrives before the run is over: for
plan and apply, before the run has released the migration lock (or failed
before taking it); for lint, before the command has finished. A
run that a signal reaches exits 128 + *n* even if it then finishes its work,
for example when a data migration's commit was already sent and lands. A
signal that arrives after that, while the report is printed or the
connection is closed, doesn't change the outcome: the exit code is 0 after
a run that succeeded (and plan still writes its digest), or the failure's
code after one that failed.
