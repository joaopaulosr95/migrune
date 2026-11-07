## migrune

Run MongoDB migrations

### Synopsis

migrune runs MongoDB migrations written as Extended JSON files.

Every flag can also be set through an environment variable named
MIGRUNE_<FLAG>, in upper case with dashes replaced by underscores
(for example MIGRUNE_URI or MIGRUNE_LOG_LEVEL). A flag given on the
command line wins over the environment variable, which wins over the default.

### Options

```
  -h, --help[=true|false]      help for migrune
      --log-format string      log format: text or json (default "text")
      --log-level string       log level: debug, info, warn or error (default "info")
  -v, --version[=true|false]   version for migrune
```

### SEE ALSO

* [migrune completion](migrune_completion.md)	 - Generate the autocompletion script for the specified shell
* [migrune lint](migrune_lint.md)	 - Check migration files

