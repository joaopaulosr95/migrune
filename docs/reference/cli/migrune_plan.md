## migrune plan

Show what apply would do

### Synopsis

Show which migrations are pending and what applying them would do, without
changing the database.

Data migrations run twice in a transaction that is then aborted. Index steps
are compared with the indexes listIndexes returns. Merge pipelines run without
their $merge, and their output is compared with the target. plan stops at the
first migration that fails. With --digest-out, it writes the plan digest for
apply --digest-in.

What plan checks and reports: docs/reference/report.md, in
https://github.com/joaopaulosr95/migrune.

```
migrune plan [flags]
```

### Options

```
      --db string                    target database name
      --digest-out string            file to write the plan digest to
      --dir string                   directory holding the migration files
  -h, --help[=true|false]            help for plan
      --lock-ttl duration            shortest time the migration lock lives without a heartbeat, in whole seconds, at least 61s; it expires after the longer of this and 3 heartbeat intervals (30s). Keep it above the server's transactionLifetimeLimitSeconds (60s by default), so the lock can't expire while a migration's transaction is open (default 1m30s)
      --lock-wait duration           how long to keep trying, with backoff, to take a migration lock held by another runner; 0 fails at once. A lock still held after the wait exits with code 3
      --namespace string             set of migrations this directory holds in the database, for several apps sharing one database: scopes the changelog reads, the lock and the digest; 1 to 64 lowercase letters, digits, _ or -; empty is the default namespace. lint checks it always but uses it only with --uri
  -o, --output string                report format: table or json (default "table")
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

