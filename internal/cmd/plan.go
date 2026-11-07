package cmd

import (
	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

type planOptions struct {
	conn   connFlags
	output string
	lock   lockFlags
	values bool
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
first migration that fails.

What plan checks and reports: docs/reference/report.md, in
https://github.com/joaopaulosr95/migrune.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.plan(cmd, o) }),
	}
	fs := c.Flags()
	o.conn.register(fs)
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
	return a.runMigrations(cmd, o.conn, o.output, runner.Options{LockTTL: o.lock.ttl, LockWait: o.lock.wait}, runner.Config{Mode: runner.ModePlan, SampleValues: o.values})
}
