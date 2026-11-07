## migrune completion bash

Generate the autocompletion script for bash

### Synopsis

Generate the autocompletion script for the bash shell.

This script depends on the 'bash-completion' package.
If it is not installed already, you can install it via your OS's package manager.

To load completions in your current shell session:

	source <(migrune completion bash)

To load completions for every new session, execute once:

#### Linux:

	migrune completion bash > /etc/bash_completion.d/migrune

#### macOS:

	migrune completion bash > $(brew --prefix)/etc/bash_completion.d/migrune

You will need to start a new shell for this setup to take effect.


```
migrune completion bash
```

### Options

```
  -h, --help[=true|false]              help for bash
      --no-descriptions[=true|false]   disable completion descriptions
```

### Options inherited from parent commands

```
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn or error (default "info")
```

### SEE ALSO

* [migrune completion](migrune_completion.md)	 - Generate the autocompletion script for the specified shell

