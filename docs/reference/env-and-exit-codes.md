# Environment variables and exit codes

The [CLI reference](cli/migrune.md) describes each command and flag; it is
generated from the code with `task docs`.

## Environment variables

Every flag can also be set through an environment variable: `MIGRUNE_`
followed by the flag name in upper case, with dashes replaced by
underscores. `--log-level` is `MIGRUNE_LOG_LEVEL`.

- **Precedence**: a flag on the command line wins over its environment
  variable, which wins over the default.
- A variable applies to every command that has the flag. Commands without
  the flag ignore it.
- Values are parsed like the flag's. An invalid value is a usage error (exit
  code 2); the message names the variable but never repeats its value, which
  could be a credential.
- migrune reads no other variable itself: map your CI's variables onto
  these.

| Variable                     | Flag                  | Commands          | Default                  | Description                                                                                         |
| ---------------------------- | --------------------- | ----------------- | ------------------------ | --------------------------------------------------------------------------------------------------- |
| `MIGRUNE_LOG_FORMAT`        | `--log-format`        | all               | `text`                   | Log format on stderr: `text` or `json`.                                                             |
| `MIGRUNE_LOG_LEVEL`         | `--log-level`         | all               | `info`                   | `debug`, `info`, `warn` or `error`.                                                                 |
| `MIGRUNE_DIR`               | `--dir`               | lint              | none, required           | Directory holding the migration files.                                                              |
| `MIGRUNE_OUTPUT`            | `--output`, `-o`      | lint              | `text`                   | Findings format on stdout: `text` or `json`; see [lint output](lint-rules.md#output).                |

## Exit codes

| Code      | Meaning                                                                                                                         |
| --------- | ------------------------------------------------------------------------------------------------------------------------------- |
| 0         | Success. lint found no errors (warnings are allowed).                                                                           |
| 1         | Failure. lint found an error, or the migrations directory can't be read.                                                       |
| 2         | Usage or configuration error, found before any work: an unknown command or flag, a missing required flag, an invalid flag or `MIGRUNE_*` value. |
| 128 + *n* | Interrupted by signal *n*: 130 for SIGINT (Ctrl+C), 143 for SIGTERM. A second signal exits at once with the first signal's code: 143 for a Ctrl+C after a SIGTERM. See [when a signal counts](#when-a-signal-counts). |

On any error, migrune prints `Error: <message>` to stderr; for exit code 2
it adds `Run 'migrune --help' for usage.` With `--log-format json`, it
writes one JSON log record instead, like the other logs, with the message as
`msg` and the exit code as `exitCode`:

```json
{"time":"2026-11-10T13:43:56.0145-03:00","level":"ERROR","msg":"--dir is required","exitCode":2}
```

The format comes from `--log-format`, or `MIGRUNE_LOG_FORMAT`, even when
the error is another flag's, such as an unknown flag; an invalid
`--log-format` is reported as text.

lint's findings go to stdout (see [lint rules](lint-rules.md#output)).

### When a signal counts

A signal sets the exit code only when it arrives before the command has
finished. A command that a signal reaches exits 128 + *n* even if it then
finishes its work. A signal that arrives after that doesn't change the
outcome: the exit code is 0 after a command that succeeded, or the failure's
code after one that failed.
