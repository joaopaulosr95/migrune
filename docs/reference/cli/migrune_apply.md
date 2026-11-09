## migrune apply

Apply pending migrations

### Synopsis

Apply the pending migrations in id order, under the migration lock.

Each data migration runs twice in one transaction, which commits with its
Finished changelog entry only if the second run changed nothing. Index and
merge migrations run outside a transaction; merges are checked before and
after writing. apply stops at the first migration that fails. It first checks
that its run matches the plan's digest (--digest-in); --no-digest skips that
check, for local runs.

What apply checks and reports: docs/reference/report.md, in
https://github.com/joaopaulosr95/migrune.

```
migrune apply [flags]
```

### Options

```
      --approved-by-email string     email of the person who approved the run, recorded in the changelog
      --approved-by-name string      name of the person who approved the run, recorded in the changelog
      --db string                    target database name
      --digest-in string             plan digest file (plan --digest-out) the run must match; required unless --no-digest
      --dir string                   directory holding the migration files
  -h, --help[=true|false]            help for apply
      --lock-ttl duration            shortest time the migration lock lives without a heartbeat, in whole seconds, at least 61s; it expires after the longer of this and 3 heartbeat intervals (30s). Keep it above the server's transactionLifetimeLimitSeconds (60s by default), so the lock can't expire while a migration's transaction is open (default 1m30s)
      --lock-wait duration           how long to keep trying, with backoff, to take a migration lock held by another runner; 0 fails at once. A lock still held after the wait exits with code 3
      --mutable[=true|false]         re-run finished migrations whose file changed instead of failing (local use only; needs --no-digest)
      --namespace string             set of migrations this directory holds in the database, for several apps sharing one database: scopes the changelog reads, the lock and the digest; 1 to 64 lowercase letters, digits, _ or -; empty is the default namespace. lint checks it always but uses it only with --uri
      --no-digest[=true|false]       run without a plan digest (local use only)
  -o, --output string                report format: table or json (default "table")
      --runner-id string             runner id used for the lock and logs (default: hostname, PID and a random suffix)
      --sample-values[=true|false]   show the values of the fields that differ in merge samples, in the logs and the report, each cut to 200 characters; by default samples show only the on fields, field names, BSON types and nested paths, so data stays out of CI logs
      --uri string                   MongoDB connection string; prefer MIGRUNE_URI to keep credentials out of process arguments
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune](migrune.md)	 - Run MongoDB migrations

