package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// Mode is what a run does with each pending migration.
type Mode string

const (
	// ModePlan runs each migration and aborts it: nothing is committed and
	// no changelog entry is written.
	ModePlan Mode = "plan"
	// ModeApply runs each migration and commits it with its changelog
	// entry.
	ModeApply Mode = "apply"
)

// Config configures what Execute does.
type Config struct {
	Mode Mode
	// Mutable re-runs Finished migrations whose file changed instead of
	// failing. Meant for local use only.
	Mutable bool
	// Digest, when set, is the plan's digest input (ReadDigestFile): the
	// run's own must give the same digest, or Execute runs nothing and
	// returns a *DigestMismatchError.
	Digest *DigestInput
	// ReplicaSet names the deployment in the digest: the replica set name
	// hello reports.
	ReplicaSet string
	// SampleValues puts the before and after values of the differing
	// fields in merge samples, in the logs and the report, each cut to
	// MaxSampleValueChars characters, and servers' messages in error text
	// (errorText). Without it, samples show only field names, BSON types
	// and nested paths, and errors only servers' code names and codes, so
	// production data stays out of CI logs. It isn't part of the digest.
	SampleValues bool
}

// MigrationError reports the migration that stopped the run.
type MigrationError struct {
	ID  string
	Err error
}

func (e *MigrationError) Error() string { return fmt.Sprintf("migration %s failed: %v", e.ID, e.Err) }

func (e *MigrationError) Unwrap() error { return e.Err }

// Execute takes the lock on db and runs the pending migrations in id order,
// stopping at the first error. migrations must come from
// migration.Load.
//
// It returns a report whenever it got as far as holding the lock, failure or
// interruption included; the error tells the outcome. An interrupted run
// returns an *InterruptedError and writes no changelog entry.
// Without c.SampleValues, the error's text, the report's and the logs'
// leave out servers' messages (errorText).
func Execute(ctx context.Context, db *mongo.Database, migrations []migration.Migration, o Options, c Config) (*Report, error) {
	if c.Mode != ModePlan && c.Mode != ModeApply {
		return nil, fmt.Errorf("unknown mode %q", c.Mode)
	}
	start := time.Now()
	// Read before the lock, the same way by plan and apply.
	onlyOwn, deployment, err := readTarget(ctx, db)
	if err != nil {
		return nil, redact(interruption(ctx, err), c.SampleValues)
	}
	hasEntries, err := changelogHasEntries(ctx, db)
	if err != nil {
		return nil, redact(interruption(ctx, err), c.SampleValues)
	}
	// New: no collection but migrune's, and no changelog entry.
	newDB := onlyOwn && !hasEntries
	if newDB && o.Logger != nil {
		o.Logger.Warn("the target database has no collections: this run treats it as a new database; "+
			"check --db is the database you meant", "db", db.Name())
	}
	// The changelog collection's UUID is the digest's deployment, so it has
	// to exist from the first plan on: otherwise two deployments' first
	// plans, with the same names, would give the same digest.
	if deployment == DeploymentNone {
		if err := createChangelog(ctx, db); err != nil {
			return nil, redact(interruption(ctx, err), c.SampleValues)
		}
		if deployment, err = ChangelogDeployment(ctx, db); err != nil {
			return nil, redact(interruption(ctx, err), c.SampleValues)
		}
	}
	run, err := startWith(ctx, db, o, c.SampleValues)
	if err != nil {
		return nil, redact(err, c.SampleValues)
	}
	rep := &Report{
		Command:           string(c.Mode),
		Database:          db.Name(),
		Namespace:         o.Namespace,
		StartedAt:         start.UTC().Truncate(time.Millisecond),
		Runner:            run.RunnerID(),
		NewDatabase:       newDB,
		Migrations:        []Row{},
		AppliedButMissing: []migration.Applied{},
	}
	if c.Mode == ModeApply && o.ApprovedBy != (ApprovedBy{}) {
		rep.ApprovedBy = &o.ApprovedBy
	}
	err = run.execute(migrations, c, deployment, rep)
	// Errors leave the runner, for the report and the caller, without
	// servers' messages (errorText).
	err = redact(withRelease(err, run.Close(), run.log, c.SampleValues), c.SampleValues)
	rep.finish(time.Since(start), err)
	return rep, err
}

// withRelease combines a run's outcome err with the error of releasing its
// lock. After a successful run, a failed release is only logged: the lock
// expires on its own, with errorText for values.
func withRelease(err, release error, log *slog.Logger, values bool) error {
	switch {
	case release == nil:
		return err
	case err == nil:
		log.Warn("could not release the migration lock: it expires on its own", "error", errorText(release, values))
		return nil
	default:
		return errors.Join(err, release)
	}
}

func (r *Run) execute(migrations []migration.Migration, c Config, deployment string, rep *Report) error {
	finished, err := Finished(r.ctx, r.db, r.namespace)
	if err != nil {
		return r.cause(err)
	}
	set := classify(migrations, finished, c.Mutable)
	rep.AppliedButMissing = append(rep.AppliedButMissing, set.missing...)
	rep.DigestInput = NewDigestInput(r.db.Name(), c.ReplicaSet, deployment, set.pending)
	rep.DigestInput.Namespace = r.namespace
	rep.Digest = rep.DigestInput.Sum()

	for _, m := range set.pending {
		rep.Migrations = append(rep.Migrations, Row{ID: m.ID, Kind: m.Kind, Status: RowSkipped})
	}
	if !c.Mutable {
		for _, m := range set.changed {
			rep.Migrations = append(rep.Migrations, Row{ID: m.ID, Kind: m.Kind, Status: RowSkipped})
		}
	}
	slices.SortFunc(rep.Migrations, func(a, b Row) int { return strings.Compare(a.ID, b.ID) })

	if c.Digest != nil && c.Digest.Sum() != rep.Digest {
		return &DigestMismatchError{Plan: c.Digest, Run: rep.DigestInput}
	}
	// Only once the digest matched. plan leaves the changelog's indexes
	// alone.
	if c.Mode == ModeApply {
		if err := ensureChangelogIndex(r.ctx, r.db); err != nil {
			return r.cause(err)
		}
	}
	// Both checks run before any migration, so the report shows every
	// problem at once.
	var checks []error
	if len(set.missing) > 0 {
		for _, a := range set.missing {
			r.log.Error("applied migration is missing: the changelog has a Finished entry but there is no file",
				"migration", a.ID, "description", a.Description, "appliedAt", a.AppliedAtText(), "appliedBy", a.RunnerText())
		}
		checks = append(checks, &AppliedMissingError{Migrations: set.missing})
	}
	if !c.Mutable && len(set.changed) > 0 {
		cerr := &ChecksumError{}
		for _, m := range set.changed {
			cerr.IDs = append(cerr.IDs, m.ID)
			row := rep.row(m.ID)
			row.Status = RowErrored
			row.Error = fmt.Sprintf("the file changed since the migration was applied (changelog checksum %s, file checksum %s)",
				finished[m.ID].Checksum, m.Checksum)
			r.log.Error("applied migration was edited", "migration", m.ID,
				"changelogChecksum", finished[m.ID].Checksum, "fileChecksum", m.Checksum)
		}
		checks = append(checks, cerr)
	}
	if len(checks) > 0 {
		return errors.Join(checks...)
	}

	if c.Mutable {
		for _, m := range set.changed {
			r.log.Warn("RE-RUNNING AN APPLIED MIGRATION whose file changed (--mutable). "+
				"--mutable is meant for local use only: never use it against a shared environment",
				"migration", m.ID)
		}
	}
	r.log.Info("pending migrations", "count", len(set.pending), "mode", string(c.Mode))
	if err := r.warnSharedCollections(set.pending); err != nil {
		return r.cause(err)
	}
	for _, m := range set.pending {
		if err := r.Err(); err != nil {
			return err
		}
		if err := r.runMigration(m, c.Mode == ModeApply, rep.row(m.ID)); err != nil {
			return err
		}
	}
	return nil
}

// runMigration runs one migration and records its outcome in row.
func (r *Run) runMigration(m migration.Migration, apply bool, row *Row) error {
	log := r.log.With("migration", m.ID, "kind", string(m.Kind))
	log.Info("running migration")
	start := time.Now()
	var err error
	switch m.Kind {
	case migration.KindData:
		err = r.runData(m, apply, log, row)
	case migration.KindIndex:
		err = r.runIndex(m, apply, log, row)
	case migration.KindMerge:
		err = r.runMerge(m, apply, log, row)
	default:
		err = fmt.Errorf("unknown migration kind %q", m.Kind)
	}
	row.DurationMs = time.Since(start).Milliseconds()

	var ie *InterruptedError
	switch {
	case err == nil:
		row.Status = RowApplied
		if apply {
			log.Info("migration applied", "durationMs", row.DurationMs)
		} else {
			log.Info("migration planned: it would apply, nothing was committed", "durationMs", row.DurationMs)
		}
		return nil
	case errors.As(err, &ie):
		row.Status = RowInterrupted
		row.Error = r.errText(err)
		switch m.Kind {
		case migration.KindIndex:
			log.Warn("migration interrupted: it stays pending; an index command in progress is left to the server, " +
				"and the next run's identical step waits for it or does nothing")
		case migration.KindMerge:
			log.Warn("migration interrupted: it stays pending; the aggregate in progress was cancelled, " +
				"a half-done $merge is safe and the next run completes it")
		default:
			log.Warn("migration interrupted: its transaction was aborted and it stays pending")
		}
		return err
	default:
		row.Status = RowErrored
		row.Error = r.errText(err)
		log.Error("migration failed", "error", row.Error)
		return &MigrationError{ID: m.ID, Err: err}
	}
}
