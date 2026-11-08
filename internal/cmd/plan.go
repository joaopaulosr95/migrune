package cmd

import (
	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

type planOptions struct {
	conn      connFlags
	digestOut string
	output    string
	lock      lockFlags
	values    bool
}

func (a *app) newPlanCmd() *cobra.Command {
	var o planOptions
	c := &cobra.Command{
		Use:   "plan",
		Short: "Show what apply would do",
		Long: `Show which migrations are pending and what applying them would do, without
changing the database.

Data migrations run twice in a transaction that is then aborted. Index steps
are compared with the indexes listIndexes returns. Merge pipelines run without
their $merge, and their output is compared with the target. plan stops at the
first migration that fails. With --digest-out, it writes the plan digest for
apply --digest-in.

What plan checks and reports: docs/reference/report.md, in
https://github.com/joaopaulosr95/migrune.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.plan(cmd, o) }),
	}
	fs := c.Flags()
	o.conn.register(fs)
	fs.StringVar(&o.digestOut, "digest-out", "", "file to write the plan digest to")
	outputFlag(fs, &o.output)
	o.lock.register(fs)
	sampleValuesFlag(fs, &o.values)
	return c
}

func (a *app) plan(cmd *cobra.Command, o planOptions) error {
	if err := o.conn.requireAll(); err != nil {
		return err
	}
	if err := checkOutput(o.output); err != nil {
		return err
	}
	if err := o.lock.check(); err != nil {
		return err
	}
	rep, err := a.runMigrations(cmd, o.conn, o.output, runner.Options{LockTTL: o.lock.ttl, LockWait: o.lock.wait}, runner.Config{Mode: runner.ModePlan, SampleValues: o.values})
	if err != nil {
		return err
	}
	// A run that returned no error but saw a signal at its very end is
	// still interrupted: no digest. A signal after the run doesn't
	// count.
	if o.digestOut != "" && !a.interrupted {
		if err := runner.WriteDigestFile(o.digestOut, rep.DigestInput); err != nil {
			return err
		}
		a.log.Info("plan digest written", "file", o.digestOut, "digest", rep.Digest)
	}
	return nil
}
