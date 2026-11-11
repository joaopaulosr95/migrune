package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/lint"
	"github.com/joaopaulosr95/migrune/internal/migration"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

// ReportedError is a run error that the report on stdout already shows.
type ReportedError struct{ Err error }

func (e *ReportedError) Error() string { return e.Err.Error() }
func (e *ReportedError) Unwrap() error { return e.Err }

// lintOffline runs lint's offline rules on dir, before any connection, so a
// pipeline that skips `migrune lint` still can't run what lint rejects. It
// logs the warnings, except ignored-entry, which LoadDir has just logged, and
// returns the error findings, for lintApplied once connected. dir is read a
// second time, after LoadDir: one load would mean reshaping lint.Run for no
// gain.
func (a *app) lintOffline(dir string) (errs []lint.Finding, warnings int, err error) {
	findings, err := lint.Run(dir, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range findings {
		switch {
		case f.Severity == lint.Error:
			errs = append(errs, f)
		case f.Rule != lint.RuleIgnoredEntry:
			a.log.Warn("lint", "finding", f.String())
		}
	}
	_, warnings = countFindings(findings)
	return errs, warnings, nil
}

// lintApplied fails the run on the error findings errs of lintOffline, except
// those in a migration the namespace has already applied: one whose file has
// the id and checksum of a Finished entry, applied. That file never runs
// again (under --mutable, an edited one does, but its checksum differs), so
// its findings are logged as warnings. The remaining errors are printed like
// lint prints them, before the lock and any write.
func (a *app) lintApplied(cmd *cobra.Command, errs []lint.Finding, warnings int, migrations []migration.Migration,
	applied map[string]migration.Applied, output string) error {
	done := map[string]bool{}
	for _, m := range migrations {
		if e, ok := applied[m.ID]; ok && e.Checksum == m.Checksum {
			done[m.Path] = true
		}
	}
	var failing []lint.Finding
	for _, f := range errs {
		if !done[f.File] {
			failing = append(failing, f)
			continue
		}
		f.Severity = lint.Warning
		warnings++
		a.log.Warn("lint finding in an already-applied migration, which doesn't run again", "finding", f.String())
	}
	if len(failing) == 0 {
		return nil
	}
	if err := writeFindings(cmd.OutOrStdout(), failing, output); err != nil {
		return fmt.Errorf("write findings: %w", err)
	}
	return fmt.Errorf("lint failed: %d errors, %d warnings", len(failing), warnings)
}

// runMigrations loads the migrations, runs them against the target database and
// prints the report, even when the run failed.
func (a *app) runMigrations(cmd *cobra.Command, conn connFlags, output string, o runner.Options, c runner.Config) (*runner.Report, error) {
	dir, err := migration.LoadDir(conn.dir, a.log)
	if err != nil {
		return nil, err
	}
	lintErrs, warnings, err := a.lintOffline(conn.dir)
	if err != nil {
		return nil, err
	}
	db, srv, disconnect, err := a.connect(cmd.Context(), conn, c.SampleValues)
	if err != nil {
		return nil, err
	}
	defer disconnect()
	// Only when lint found errors: the run reads the changelog once more,
	// after connect's checks and before Execute's reads and lock.
	if len(lintErrs) > 0 {
		applied, err := runner.Finished(cmd.Context(), db, o.Namespace)
		if err != nil {
			return nil, runner.RedactError(err, c.SampleValues)
		}
		if err := a.lintApplied(cmd, lintErrs, warnings, dir.Migrations, applied, output); err != nil {
			return nil, err
		}
	}
	o.Logger = a.log
	c.ReplicaSet = srv.SetName
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
	return rep, err
}
