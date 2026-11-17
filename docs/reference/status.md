# Status output

`migrune status` reads one namespace's migrations and lock. It takes no lock
and writes nothing. `--uri` and `--db` are required; `--dir` is optional.
The output goes to stdout, logs to stderr:

```sh
migrune status --dir migrations --db app --namespace billing -o json > status.json
```

`--output` (`-o`) is `table` (the default) or `json`. A successful read exits
0 whatever the state, including failed migrations or a held lock. Failures
exit 1 and usage or configuration errors exit 2; see
[exit codes](env-and-exit-codes.md#exit-codes).

## Migration states

Rows are sorted by migration id. Only the selected namespace's changelog
entries count:

| State | Meaning | Needs `--dir` |
| ----- | ------- | ------------- |
| `applied` | A `Finished` entry exists and its checksum matches the file, or no `--dir` was supplied. | no |
| `failed` | An `Errored` entry exists with no `Finished` entry at or after its time. | no |
| `pending` | A file has no `Finished` or `Errored` entry. | yes |
| `changed` | A `Finished` entry exists, but the file's checksum differs. | yes |
| `missing` | A `Finished` entry exists, but its file is absent. | yes |

`failed` takes precedence over the file comparison, including for a migration
that was applied and later failed when re-run with `--mutable`. A later
`Finished` entry clears the failed state. Without `--dir`, only applied and
failed migrations are shown; an empty directory supplied with `--dir` still
allows missing files to be detected.

An `Errored` entry's stored messages may contain document values. By default,
the table shows the failed step and `error recorded (rerun with
--sample-values to show it)`, and JSON omits `error`. `--sample-values`
(`MIGRUNE_SAMPLE_VALUES=true`) reveals the messages already stored in the
changelog; it cannot recover server text omitted when the entry was written.
Use it where output is as protected as the data.

## JSON

`-o json` prints one object. Its fields are part of the 1.x compatibility
promise: fields may be added, but aren't renamed, removed or changed in
meaning within 1.x. The table's wording and layout aren't stable; parse JSON
and exit codes (see [compatibility](compatibility.md)).

| Field | Type | Description |
| ----- | ---- | ----------- |
| `version` | string | The migrune version. |
| `database` | string | The target database, `--db`. |
| `namespace` | string | The selected namespace; empty for the default namespace. |
| `dir` | string, optional | The migration directory, as supplied. Absent without `--dir`. |
| `migrations` | array | One row per migration considered, in id order; `[]` when there are none. |
| `migrations[].id` | string | The migration id: the file name without `.json`. |
| `migrations[].status` | string | One of the [migration states](#migration-states) above. |
| `migrations[].checksum` | string, optional | SHA-256, lowercase hex: the latest `Finished` entry's file checksum for applied, changed or missing rows; the latest `Errored` entry's for failed rows; the current file's for pending rows. Absent when empty. |
| `migrations[].appliedAt` | string, optional | The latest `Finished` entry's `createdAt`, or the latest `Errored` entry's for a failed row. RFC 3339 in UTC. Absent for pending rows. |
| `migrations[].runner` | string, optional | The runner from that entry. Absent when empty, including for pending rows. |
| `migrations[].failedStep` | integer, optional | The first recorded error's non-null step index, skipping errors without a step; zero-based. Absent when no error has a step. |
| `migrations[].error` | string, optional | The first stored error message of a failed migration, only with `--sample-values`. Absent otherwise, or when that message is empty. |
| `lock` | object or null | The namespace's lock, or `null` when it has none. Fields below are all present when it is an object. |
| `lock.owner` | string | The holder's runner id, a label for people. |
| `lock.current` | string | The migration the holder is running; empty between migrations or when the lock document has no current migration. |
| `lock.token` | string | The acquisition token to pass to unlock; a new acquisition has a new token, even with the same runner id. |
| `lock.acquiredAt` | string | When the lock was acquired, RFC 3339 in UTC. |
| `lock.lastHeartbeat` | string | The last committed heartbeat, RFC 3339 in UTC. |
| `lock.expiresAt` | string | When another runner may take over, RFC 3339 in UTC. |
| `lock.heartbeatAgeMs` | integer | Milliseconds since the last committed heartbeat, using the server's clock (`$$NOW`) from the same lock read. |
| `lock.expired` | boolean | Whether `expiresAt` is before that server time. A lock past expiry can still be present. |
| `lock.unlockAfter` | string | `lastHeartbeat` plus the [silence threshold](changelog-and-lock.md#unlock), RFC 3339 in UTC. unlock requires a heartbeat older than that threshold. |

For a lock document written by hand without a timestamp, that JSON field
is the zero time, `0001-01-01T00:00:00Z`; the table shows `-`.

```json
{
  "version": "1.1.0",
  "database": "app",
  "namespace": "billing",
  "dir": "migrations",
  "migrations": [
    {
      "id": "2026-11-03-001-copy-currency",
      "status": "failed",
      "checksum": "5f0c…",
      "appliedAt": "2026-11-03T09:12:44Z",
      "runner": "ci-42-1-AB12CD34",
      "failedStep": 0
    }
  ],
  "lock": {
    "owner": "ci-42-1-AB12CD34",
    "current": "2026-11-05-001-set-trial-days",
    "token": "9f86d081884c7d659a2feaa0c55ad015",
    "acquiredAt": "2026-11-05T10:10:02Z",
    "lastHeartbeat": "2026-11-05T10:12:00Z",
    "expiresAt": "2026-11-05T10:13:30Z",
    "heartbeatAgeMs": 47000,
    "expired": false,
    "unlockAfter": "2026-11-05T10:13:20Z"
  }
}
```

This page describes the shape of the migrune version it ships with.

## Table

The table starts with `Database: <name>` and, for a named namespace,
`Namespace: <name>`. Its columns are `MIGRATION`, `STATUS`, `APPLIED AT` and
`RUNNER`. `APPLIED AT` uses the same entry timestamp as JSON, including the
error's timestamp for a failed row. Failed rows have indented detail lines,
one per stored error, with the step index when present.

Without `--dir`, it prints `No --dir: pending, changed and missing migrations
aren't shown.` before the rows.

The lock section says `Lock: not held` when there is no document. Otherwise,
it shows the runner, current migration when present, token and timestamps.
The heartbeat line says `may be inside a transaction` until its age exceeds
the [silence threshold](changelog-and-lock.md#unlock), then `holder stopped:
unlock allowed`. Expiry is shown separately. For recovery, see
[the lock is held](../troubleshooting.md#the-lock-is-held).
