## migrune completion powershell

Generate the autocompletion script for powershell

### Synopsis

Generate the autocompletion script for powershell.

To load completions in your current shell session:

	migrune completion powershell | Out-String | Invoke-Expression

To load completions for every new session, add the output of the above command
to your powershell profile.


```
migrune completion powershell [flags]
```

### Options

```
  -h, --help[=true|false]              help for powershell
      --no-descriptions[=true|false]   disable completion descriptions
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune completion](migrune_completion.md)	 - Generate the autocompletion script for the specified shell

