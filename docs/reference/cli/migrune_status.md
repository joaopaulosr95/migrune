## migrune status

Show a namespace's migrations and lock

### Synopsis

Show one namespace's migrations and lock, read-only: it takes no lock and
writes nothing. --uri and --db are required; --dir is optional.

STATE    MEANING
applied  Finished and the checksum matches, or no --dir
failed   Errored with no later Finished entry
pending  File with no Finished entry
changed  Finished, but the file's checksum differs
missing  Finished, but no file

Without --dir, only applied and failed migrations are shown. A failed
migration's stored error message is hidden unless --sample-values is set:
it may contain document values. -o json prints one JSON object.

The lock's heartbeat age uses the server's clock. A holder may be inside a
transaction until the server's transaction lifetime, counted as at least 60
seconds, plus two heartbeat intervals have passed without a committed
heartbeat. status exits 0 whatever the state, 1 on a read failure, and 2 on a
usage or configuration error.

Status fields: docs/reference/status.md, in
https://github.com/joaopaulosr95/migrune.

```
migrune status [flags]
```

### Options

```
      --db string                    target database name
      --dir string                   directory holding the migration files
  -h, --help[=true|false]            help for status
      --namespace string             set of migrations this directory holds in the database, for several apps sharing one database: scopes the changelog reads, the lock and the digest; 1 to 64 lowercase letters, digits, _ or -; empty is the default namespace. lint checks it always but uses it only with --uri
  -o, --output string                report format: table or json (default "table")
      --sample-values[=true|false]   show document values: those of the fields that differ in merge samples, each cut to 200 characters, server errors' messages, which can quote them, and the duplicate keys of a unique conversion, in the logs, the report and the changelog; by default samples show only the on fields, field names, BSON types and nested paths, and server errors only their code names, codes and _ids, so data stays out of CI logs
      --uri string                   MongoDB connection string; prefer MIGRUNE_URI to keep credentials out of process arguments
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune](migrune.md)	 - Run MongoDB migrations

