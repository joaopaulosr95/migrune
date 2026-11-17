## migrune unlock

Remove a dead holder's migration lock by token

### Synopsis

Remove one namespace's migration lock, only if its token matches --token
and its last committed heartbeat is older than the server's transaction
lifetime, counted as at least 60 seconds, plus two heartbeat intervals (80s
with defaults), by the server's clock. Inspect the lock with status first.
--uri, --db and --token are required.

If the server's transactionLifetimeLimitSeconds can't be read, unlock assumes
60s and warns: a raised limit makes removing a live holder's lock possible.
The delete may wait behind an open transaction, for at most the transaction
lifetime plus 10s; after 5s it logs that it is waiting.

A matching token with a recent heartbeat is refused with exit 3: its holder
may be inside a transaction; retry after the time shown. A token belonging to
another namespace exits 1 and shows which --namespace to use. A lock now held
by another acquisition is refused with exit 3: run status again. If no lock
has the token and the namespace has none, unlock exits 0: it was already
released or expired and removed, or --db names the wrong database; the log
line names the database and namespace it checked. A removed lock is logged at warn; usage
errors exit 2 and other failures exit 1.

The namespace is then as a killed run leaves it: run plan and apply again.

```
migrune unlock [flags]
```

### Options

```
      --db string           target database name
  -h, --help[=true|false]   help for unlock
      --namespace string    set of migrations this directory holds in the database, for several apps sharing one database: scopes the changelog reads, the lock and the digest; 1 to 64 lowercase letters, digits, _ or -; empty is the default namespace. lint checks it always but uses it only with --uri
      --token string        token of the lock to remove, as status shows it
      --uri string          MongoDB connection string; prefer MIGRUNE_URI to keep credentials out of process arguments
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune](migrune.md)	 - Run MongoDB migrations

