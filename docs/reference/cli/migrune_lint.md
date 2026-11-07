## migrune lint

Check migration files

### Synopsis

Check migration files offline.

Each finding is printed to stdout on its own line as
"<file>: <severity>: <rule>: <message>", where severity is error or warning.
With -o json, the findings are printed as one JSON array of objects with
those four fields. lint exits with status 1 if there is any error; warnings
alone exit 0.

```
migrune lint [flags]
```

### Options

```
      --dir string          directory holding the migration files
  -h, --help[=true|false]   help for lint
  -o, --output string       findings format: text, one per line, or json, one array (default "text")
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune](migrune.md)	 - Run MongoDB migrations

