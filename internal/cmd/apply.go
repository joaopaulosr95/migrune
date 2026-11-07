package cmd

import (
	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

type applyOptions struct {
	conn            connFlags
	mutable         bool
	approvedByName  string
	approvedByEmail string
	runnerID        string
	output          string
	lock            lockFlags
}

func (a *app) newApplyCmd() *cobra.Command {
	var o applyOptions
	c := &cobra.Command{
		Use:   "apply",
		Short: "Apply pending migrations",
		Long: `Apply the pending migrations in id order, under the migration lock.

Each data migration runs twice in one transaction, which commits with its
Finished changelog entry only if the second run changed nothing. apply stops
at the first migration that fails.

What apply checks and reports: docs/reference/report.md, in
https://github.com/joaopaulosr95/migrune.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.apply(cmd, o) }),
	}
	fs := c.Flags()
	o.conn.register(fs)
	fs.BoolVar(&o.mutable, "mutable", false, "re-run finished migrations whose file changed instead of failing (local use only)")
	fs.StringVar(&o.approvedByName, "approved-by-name", "", "name of the person who approved the run, recorded in the changelog")
	fs.StringVar(&o.approvedByEmail, "approved-by-email", "", "email of the person who approved the run, recorded in the changelog")
	fs.StringVar(&o.runnerID, "runner-id", "", "runner id used for the lock and logs (default: hostname, PID and a random suffix)")
	outputFlag(fs, &o.output)
	o.lock.register(fs)
	return c
}

func (a *app) apply(cmd *cobra.Command, o applyOptions) error {
	if err := o.conn.requireAll(); err != nil {
		return err
	}
	if err := checkOutput(o.output); err != nil {
		return err
	}
	if err := o.lock.check(); err != nil {
		return err
	}
	if o.mutable {
		a.log.Warn("--mutable is set: applied migrations whose file changed are run again. " +
			"This is meant for local use only; never use it against a shared environment")
	}
	return a.runMigrations(cmd, o.conn, o.output,
		runner.Options{
			RunnerID:   o.runnerID,
			LockTTL:    o.lock.ttl,
			LockWait:   o.lock.wait,
			ApprovedBy: runner.ApprovedBy{Name: o.approvedByName, Email: o.approvedByEmail},
		},
		runner.Config{Mode: runner.ModeApply, Mutable: o.mutable})
}
