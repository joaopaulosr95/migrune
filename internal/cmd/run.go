package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/migration"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

// ReportedError is a run error that the report on stdout already shows.
type ReportedError struct{ Err error }

func (e *ReportedError) Error() string { return e.Err.Error() }
func (e *ReportedError) Unwrap() error { return e.Err }

// runMigrations loads the migrations, runs them against the target database and
// prints the report, even when the run failed.
func (a *app) runMigrations(cmd *cobra.Command, conn connFlags, output string, o runner.Options, c runner.Config) error {
	dir, err := migration.LoadDir(conn.dir, a.log)
	if err != nil {
		return err
	}
	db, disconnect, err := a.connect(cmd.Context(), conn)
	if err != nil {
		return err
	}
	defer disconnect()
	o.Logger = a.log
	rep, err := runner.Execute(cmd.Context(), db, dir.Migrations, o, c)
	// The run is over: a signal from now on doesn't interrupt it.
	a.settle(cmd.Context())
	if rep != nil {
		rep.Version = Version()
		if werr := writeReport(cmd.OutOrStdout(), rep, output); werr != nil {
			err = errors.Join(err, fmt.Errorf("write report: %w", werr))
		} else if err != nil {
			err = &ReportedError{Err: err}
		}
	}
	return err
}
