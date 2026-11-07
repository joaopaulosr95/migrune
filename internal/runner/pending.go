package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// Finished reads the changelog in db and returns, for every migration id with
// a Finished entry, what its latest one recorded. It only reads, and projects
// out the stored steps.
func Finished(ctx context.Context, db *mongo.Database) (map[string]migration.Applied, error) {
	cur, err := changelogCollection(db).Find(ctx,
		bson.D{{Key: "status", Value: StatusFinished}},
		options.Find().
			SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}).
			SetProjection(bson.D{
				{Key: "migrationId", Value: 1},
				{Key: "checksum", Value: 1},
				{Key: "createdAt", Value: 1},
				{Key: "runner", Value: 1},
				{Key: "migration.description", Value: 1},
			}))
	if err != nil {
		return nil, fmt.Errorf("read changelog: %w", err)
	}
	var entries []struct {
		MigrationID string    `bson:"migrationId"`
		Checksum    string    `bson:"checksum"`
		CreatedAt   time.Time `bson:"createdAt"`
		Runner      string    `bson:"runner"`
		Migration   struct {
			Description string `bson:"description"`
		} `bson:"migration"`
	}
	if err := cur.All(ctx, &entries); err != nil {
		return nil, fmt.Errorf("read changelog: %w", err)
	}
	finished := make(map[string]migration.Applied, len(entries))
	for _, e := range entries {
		finished[e.MigrationID] = migration.Applied{
			ID:          e.MigrationID,
			Checksum:    e.Checksum,
			Description: e.Migration.Description,
			AppliedAt:   e.CreatedAt.UTC(),
			Runner:      e.Runner,
		}
	}
	return finished, nil
}

// pendingSet is how the migration files compare with the changelog.
type pendingSet struct {
	// pending have no Finished entry, plus, with mutable, the changed
	// ones. Sorted by id.
	pending []migration.Migration
	// changed are Finished, but their file's checksum differs from the
	// latest Finished entry. Sorted by id.
	changed []migration.Migration
	// missing are the migrations Finished in the changelog with no file in
	// the directory: applied but missing, e.g. after a revert. They fail the
	// run. Sorted by id.
	missing []migration.Applied
}

// classify compares migrations (sorted by id) with finished, as returned by
// Finished. With mutable, changed migrations are pending again.
func classify(migrations []migration.Migration, finished map[string]migration.Applied, mutable bool) pendingSet {
	var s pendingSet
	files := make(map[string]bool, len(migrations))
	for _, m := range migrations {
		files[m.ID] = true
		applied, done := finished[m.ID]
		switch {
		case !done:
			s.pending = append(s.pending, m)
		case applied.Checksum != m.Checksum:
			s.changed = append(s.changed, m)
			if mutable {
				s.pending = append(s.pending, m)
			}
		}
	}
	for id, applied := range finished {
		if !files[id] {
			s.missing = append(s.missing, applied)
		}
	}
	slices.SortFunc(s.missing, func(a, b migration.Applied) int { return strings.Compare(a.ID, b.ID) })
	return s
}

// ChecksumError reports Finished migrations whose file was edited.
type ChecksumError struct {
	IDs []string
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("applied migrations are immutable, but the file of %s changed since it was applied; "+
		"revert the edit and write a new migration (--mutable re-runs edited migrations, for local use only)",
		strings.Join(e.IDs, ", "))
}

// AppliedMissingError reports Finished migrations whose file is no longer in
// the migrations directory, with what their changelog entry stored.
type AppliedMissingError struct {
	// Migrations are sorted by id.
	Migrations []migration.Applied
}

// Error names the migrations on one line. The report's applied-but-missing
// section says what the changelog stored about each.
func (e *AppliedMissingError) Error() string {
	ids := make([]string, len(e.Migrations))
	for i, a := range e.Migrations {
		ids[i] = a.ID
	}
	return fmt.Sprintf("applied migrations must keep their file, but the file of %s is missing from the migrations directory; "+
		"restore it (removing a file doesn't undo its migration), or, to retire a migration on purpose, delete its changelog entries by hand",
		strings.Join(ids, ", "))
}
