package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/lint"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

type driftOptions struct {
	conn      connFlags
	namespace string
	output    string
}

func (a *app) newDriftCmd() *cobra.Command {
	var o driftOptions
	c := &cobra.Command{
		Use:   "drift",
		Short: "Compare the indexes with the applied migrations",
		Long: `Compare the indexes on the server with the indexes the applied migrations
leave, read-only: it takes no lock and writes nothing.

The migrations Finished in the namespace's changelog are replayed in id order,
from the copy each changelog entry stores, and the indexes left are compared
with listIndexes on every collection of the database. An applied index missing
on the server, or there with other options, is an error; an index the applied
migrations don't create, or one under another name, is a warning. A Finished
migration whose file changed or is missing is an error too. Pending migrations
are left out.

Findings are printed as lint prints them, one per line as
"<collection or file>: <severity>: <rule>: <message>", or with -o json as one
JSON array. drift exits with status 1 if there is any error; warnings alone
exit 0.

What drift compares: docs/reference/drift.md, in
https://github.com/joaopaulosr95/migrune.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.drift(cmd, o) }),
	}
	fs := c.Flags()
	o.conn.register(fs)
	namespaceFlag(fs, &o.namespace)
	findingsOutputFlag(fs, &o.output)
	return c
}

func (a *app) drift(cmd *cobra.Command, o driftOptions) error {
	if err := o.conn.requireAll(); err != nil {
		return err
	}
	if err := checkFindingsOutput(o.output); err != nil {
		return err
	}
	if err := checkNamespace(o.namespace); err != nil {
		return err
	}
	db, _, disconnect, err := a.connect(cmd.Context(), o.conn)
	if err != nil {
		return err
	}
	defer disconnect()
	applied, err := runner.FinishedSnapshots(cmd.Context(), db, o.namespace)
	if err != nil {
		return err
	}
	server, err := runner.ListServerIndexes(cmd.Context(), db)
	if err != nil {
		return err
	}
	res, err := lint.Drift(o.conn.dir, applied, server.Indexes, server.Collations)
	if err != nil {
		return err
	}
	a.log.Info("replayed the applied migrations", "count", res.Replayed)
	if res.Pending > 0 {
		a.log.Info("left out the migration files with no Finished entry: they aren't applied yet", "count", res.Pending)
	}
	if err := writeFindings(cmd.OutOrStdout(), res.Findings, o.output); err != nil {
		return fmt.Errorf("write findings: %w", err)
	}
	errs, warnings := countFindings(res.Findings)
	if errs > 0 {
		return fmt.Errorf("drift found: %d errors, %d warnings", errs, warnings)
	}
	a.log.Info("no index drift", "warnings", warnings)
	return nil
}
