package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
)

// envPrefix is the prefix of the environment variable that backs each flag.
const envPrefix = "MIGRUNE_"

// envName returns the environment variable for a flag: --approved-by-name
// becomes MIGRUNE_APPROVED_BY_NAME.
func envName(flag string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

// bindEnv sets every flag the user did not set on the command line from its
// environment variable, if that variable is set. The precedence is
// flag > environment > default. fs must hold both the command's own flags and
// the persistent flags it inherits, which cobra merges into Command.Flags.
func bindEnv(fs *pflag.FlagSet, lookup func(string) (string, bool)) error {
	var err error
	fs.VisitAll(func(f *pflag.Flag) {
		if err != nil || f.Changed || f.Name == "help" {
			return
		}
		name := envName(f.Name)
		v, ok := lookup(name)
		if !ok {
			return
		}
		// The value is left out of the message: it may be a credential.
		if setErr := fs.Set(f.Name, v); setErr != nil {
			err = fmt.Errorf("%s: invalid value for --%s", name, f.Name)
		}
	})
	return err
}
