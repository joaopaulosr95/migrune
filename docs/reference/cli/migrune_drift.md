## migrune drift

Compare the indexes with the applied migrations

### Synopsis

Compare the indexes on the server with the indexes the applied migrations
leave, read-only: it takes no lock and writes nothing.

The migrations Finished in the namespace's changelog are replayed in id order,
from the copy each changelog entry stores, and the indexes left are compared
with listIndexes on every collection of the database. An applied index missing
on the server, or there with other options, is an error; an index the applied
migrations don't create, or one under another name, is a warning. A Finished
migration whose file changed or is missing is an error too. Pending migrations
are left out.

Findings are printed as lint prints them, one per line as
"<collection or file>: <severity>: <rule>: <message>", or with -o json as one
JSON array. drift exits with status 1 if there is any error; warnings alone
exit 0.

What drift compares: docs/reference/drift.md, in
https://github.com/joaopaulosr95/migrune.

```
migrune drift [flags]
```

### Options

```
      --db string           target database name
      --dir string          directory holding the migration files
  -h, --help[=true|false]   help for drift
      --namespace string    set of migrations this directory holds in the database, for several apps sharing one database: scopes the changelog reads, the lock and the digest; 1 to 64 lowercase letters, digits, _ or -; empty is the default namespace. lint checks it always but uses it only with --uri
  -o, --output string       findings format: text, one per line, or json, one array; table, the plan and apply default, means text, so one MIGRUNE_OUTPUT serves every command (default "text")
      --uri string          MongoDB connection string; prefer MIGRUNE_URI to keep credentials out of process arguments
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune](migrune.md)	 - Run MongoDB migrations

