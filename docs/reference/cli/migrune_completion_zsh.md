## migrune completion zsh

Generate the autocompletion script for zsh

### Synopsis

Generate the autocompletion script for the zsh shell.

If shell completion is not already enabled in your environment you will need
to enable it.  You can execute the following once:

	echo "autoload -U compinit; compinit" >> ~/.zshrc

To load completions in your current shell session:

	source <(migrune completion zsh)

To load completions for every new session, execute once:

#### Linux:

	migrune completion zsh > "${fpath[1]}/_migrune"

#### macOS:

	migrune completion zsh > $(brew --prefix)/share/zsh/site-functions/_migrune

You will need to start a new shell for this setup to take effect.


```
migrune completion zsh [flags]
```

### Options

```
  -h, --help[=true|false]              help for zsh
      --no-descriptions[=true|false]   disable completion descriptions
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune completion](migrune_completion.md)	 - Generate the autocompletion script for the specified shell

