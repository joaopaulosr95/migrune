package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

type applyOptions struct {
	conn            connFlags
	namespace       string
	digestIn        string
	noDigest        bool
	init            bool
	mutable         bool
	approvedByName  string
	approvedByEmail string
	runnerID        string
	output          string
	lock            lockFlags
	values          bool
}

func (a *app) newApplyCmd() *cobra.Command {
	var o applyOptions
	c := &cobra.Command{
		Use:   "apply",
		Short: "Apply pending migrations",
		Long: `Apply the pending migrations in id order, under the migration lock.

Each data migration runs twice in one transaction, which commits with its
Finished changelog entry only if the second run changed nothing. Index and
merge migrations run outside a transaction; merges are checked before and
after writing. apply stops at the first migration that fails. It first checks
that its run matches the plan's digest (--digest-in); --no-digest skips that
check, for local runs. A namespace's first apply, when the changelog has
no entries for it, needs --init, so a mistyped --db or --namespace can't run
every migration.

What apply checks and reports: docs/reference/report.md, in
https://github.com/joaopaulosr95/migrune.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.apply(cmd, o) }),
	}
	fs := c.Flags()
	o.conn.register(fs)
	namespaceFlag(fs, &o.namespace)
	fs.StringVar(&o.digestIn, "digest-in", "", "plan digest file (plan --digest-out) the run must match; required unless --no-digest")
	fs.BoolVar(&o.noDigest, "no-digest", false, "run without a plan digest (local use only)")
	fs.BoolVar(&o.init, "init", false, "allow the namespace's first apply, when the changelog has no entries for it; refused once it has")
	fs.BoolVar(&o.mutable, "mutable", false, "re-run finished migrations whose file changed instead of failing (local use only; needs --no-digest)")
	fs.StringVar(&o.approvedByName, "approved-by-name", "", "name of the person who approved the run, recorded in the changelog")
	fs.StringVar(&o.approvedByEmail, "approved-by-email", "", "email of the person who approved the run, recorded in the changelog")
	fs.StringVar(&o.runnerID, "runner-id", "", "runner id used for the lock and logs (default: hostname, PID and a random suffix)")
	outputFlag(fs, &o.output)
	o.lock.register(fs)
	sampleValuesFlag(fs, &o.values)
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
	if err := checkNamespace(o.namespace); err != nil {
		return err
	}
	switch {
	case o.noDigest && o.digestIn != "":
		return &UsageError{Err: errors.New("--no-digest can't be used with --digest-in")}
	case o.mutable && !o.noDigest:
		return &UsageError{Err: errors.New("--mutable needs --no-digest: plan has no --mutable, " +
			"so no plan digest covers re-running an edited migration; --mutable is for local use only")}
	case !o.noDigest && o.digestIn == "":
		return &UsageError{Err: errors.New("apply needs --digest-in (the file plan --digest-out wrote); use --no-digest for local runs")}
	}
	var digest *runner.DigestInput
	if o.digestIn != "" {
		var err error
		if digest, err = runner.ReadDigestFile(o.digestIn); err != nil {
			return &UsageError{Err: fmt.Errorf("--digest-in: %w", err)}
		}
	}
	if o.mutable {
		a.log.Warn("--mutable is set: applied migrations whose file changed are run again. " +
			"This is meant for local use only; never use it against a shared environment")
	}
	_, err := a.runMigrations(cmd, o.conn, o.output,
		runner.Options{
			RunnerID:   o.runnerID,
			LockTTL:    o.lock.ttl,
			LockWait:   o.lock.wait,
			Namespace:  o.namespace,
			ApprovedBy: runner.ApprovedBy{Name: o.approvedByName, Email: o.approvedByEmail},
		},
		runner.Config{Mode: runner.ModeApply, Init: o.init, Mutable: o.mutable, Digest: digest, SampleValues: o.values})
	return err
}
