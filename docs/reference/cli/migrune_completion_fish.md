## migrune completion fish

Generate the autocompletion script for fish

### Synopsis

Generate the autocompletion script for the fish shell.

To load completions in your current shell session:

	migrune completion fish | source

To load completions for every new session, execute once:

	migrune completion fish > ~/.config/fish/completions/migrune.fish

You will need to start a new shell for this setup to take effect.


```
migrune completion fish [flags]
```

### Options

```
  -h, --help[=true|false]              help for fish
      --no-descriptions[=true|false]   disable completion descriptions
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune completion](migrune_completion.md)	 - Generate the autocompletion script for the specified shell

