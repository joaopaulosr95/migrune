package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/pflag"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// connFlags are the flags every command uses to find migrations and the
// target database.
type connFlags struct {
	dir string
	uri string
	db  string
}

func (c *connFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&c.dir, "dir", "", "directory holding the migration files")
	fs.StringVar(&c.uri, "uri", "", "MongoDB connection string; prefer MIGRUNE_URI to keep credentials out of process arguments")
	fs.StringVar(&c.db, "db", "", "target database name")
}

// requireAll checks --dir, --uri and --db are all set.
func (c *connFlags) requireAll() error {
	for _, f := range []struct{ name, value string }{{"dir", c.dir}, {"uri", c.uri}, {"db", c.db}} {
		if f.value == "" {
			return &UsageError{Err: fmt.Errorf("--%s is required", f.name)}
		}
	}
	return nil
}

// namespaceFlag registers the namespace flag shared by lint, plan, apply and
// drift.
func namespaceFlag(fs *pflag.FlagSet, p *string) {
	fs.StringVar(p, "namespace", "",
		"set of migrations this directory holds in the database, for several apps sharing one database: "+
			"scopes the changelog reads, the lock and the digest; 1 to 64 lowercase letters, digits, _ or -; "+
			"empty is the default namespace. lint checks it always but uses it only with --uri")
}

// checkNamespace validates --namespace.
func checkNamespace(namespace string) error {
	if err := runner.CheckNamespace(namespace); err != nil {
		return &UsageError{Err: fmt.Errorf("--namespace: %w", err)}
	}
	return nil
}

// outputFlag registers the report format flag shared by plan and apply.
func outputFlag(fs *pflag.FlagSet, p *string) {
	fs.StringVarP(p, "output", "o", "table", "report format: table or json")
}

// lockFlags are the migration lock flags shared by plan and apply.
type lockFlags struct {
	ttl  time.Duration
	wait time.Duration
}

func (l *lockFlags) register(fs *pflag.FlagSet) {
	fs.DurationVar(&l.ttl, "lock-ttl", runner.DefaultLockTTL,
		fmt.Sprintf("shortest time the migration lock lives without a heartbeat, in whole seconds, at least %ds; "+
			"it expires after the longer of this and 3 heartbeat intervals (30s). Keep it above the server's "+
			"transactionLifetimeLimitSeconds (60s by default), so the lock can't expire while a migration's transaction is open",
			runner.MinLockTTL/time.Second))
	fs.DurationVar(&l.wait, "lock-wait", 0,
		"how long to keep trying, with backoff, to take a migration lock held by another runner; "+
			"0 fails at once. A lock still held after the wait exits with code 3")
}

func (l *lockFlags) check() error {
	if l.ttl < runner.MinLockTTL || l.ttl%time.Second != 0 {
		return &UsageError{Err: fmt.Errorf("invalid --lock-ttl %s: want a whole number of seconds, at least %ds, "+
			"above the server's 60s transaction lifetime limit", l.ttl, runner.MinLockTTL/time.Second)}
	}
	if l.wait < 0 {
		return &UsageError{Err: fmt.Errorf("invalid --lock-wait %s: want 0 or more", l.wait)}
	}
	return nil
}

// sampleValuesFlag registers the document values flag shared by plan and
// apply.
func sampleValuesFlag(fs *pflag.FlagSet, p *bool) {
	fs.BoolVar(p, "sample-values", false, fmt.Sprintf(
		"show document values: those of the fields that differ in merge samples, each cut to %d characters, "+
			"server errors' messages, which can quote them, and the duplicate keys of a unique conversion, in the logs, the report and the changelog; "+
			"by default samples show only the on fields, field names, BSON types and nested paths, and server errors only their code names, codes and _ids, "+
			"so data stays out of CI logs",
		runner.MaxSampleValueChars))
}

func checkOutput(output string) error {
	if output != "table" && output != "json" {
		return &UsageError{Err: fmt.Errorf("invalid --output %q: want table or json", output)}
	}
	return nil
}
