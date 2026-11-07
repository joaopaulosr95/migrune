package runner

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// ChangelogCollection holds one entry per migration outcome.
const ChangelogCollection = migration.ChangelogCollection

// Status is the outcome a changelog entry records. There is no running status:
// what is running lives in the lock document.
type Status string

const (
	StatusFinished Status = "Finished"
	StatusErrored  Status = "Errored"
)

// Entry is a changelog document.
type Entry struct {
	ID bson.ObjectID `bson:"_id"`
	// V is the entry's format version, EntryVersion.
	V           int    `bson:"v"`
	MigrationID string `bson:"migrationId"`
	Checksum    string `bson:"checksum"`
	// CreatedAt is server time ($$NOW) when the entry was written.
	CreatedAt  time.Time  `bson:"createdAt"`
	Runner     string     `bson:"runner"`
	ApprovedBy ApprovedBy `bson:"approvedBy"`
	Status     Status     `bson:"status"`
	// Migration is the full migration as applied.
	Migration bson.Raw `bson:"migration"`
	// Results has one item per step, in file order. Finished entries only.
	Results []StepResult `bson:"results,omitempty"`
	// Errors is set on Errored entries only.
	Errors []StepError `bson:"errors,omitempty"`
}

// StepResult is what MongoDB returned for one step. Only the fields that apply
// to the step's operation are set.
type StepResult struct {
	Collection    string `bson:"collection"`
	Operation     string `bson:"operation"`
	MatchedCount  *int64 `bson:"matchedCount,omitempty"`
	ModifiedCount *int64 `bson:"modifiedCount,omitempty"`
	UpsertedCount *int64 `bson:"upsertedCount,omitempty"`
	DeletedCount  *int64 `bson:"deletedCount,omitempty"`
}

// StepError is one error of an Errored entry. Step is the index of the failed
// step in file order, or nil when the error isn't tied to a step.
type StepError struct {
	Step    *int   `bson:"step,omitempty"`
	Message string `bson:"message"`
}

func changelogCollection(db *mongo.Database) *mongo.Collection {
	return db.Collection(ChangelogCollection, options.Collection().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Majority()))
}

// snapshot is the migration file as applied, kept in every entry so the audit
// survives the file being deleted.
func snapshot(m migration.Migration) (bson.Raw, error) {
	raw, err := bson.Marshal(struct {
		ID          string           `bson:"id"`
		Description string           `bson:"description"`
		Steps       []migration.Step `bson:"steps"`
	}{m.ID, m.Description, m.Steps})
	if err != nil {
		return nil, fmt.Errorf("snapshot migration %s: %w", m.ID, err)
	}
	return raw, nil
}

// EntryVersion is the format version every changelog entry is written with,
// in its v field. An entry without v reads as 1. CheckFormats refuses a
// changelog holding a higher one; when to raise it is in CONTRIBUTING.md.
const EntryVersion = 1

// WriteFinished records m as Finished, with the results of its steps, as the
// changelog entry with _id id. Call it inside the fenced transaction the
// entry commits with: the data migration's.
func (r *Run) WriteFinished(ctx context.Context, id bson.ObjectID, m migration.Migration, results []StepResult) error {
	if results == nil {
		results = []StepResult{}
	}
	return r.writeEntry(ctx, id, m, StatusFinished, bson.E{Key: "results", Value: results})
}

// WriteErrored records m as Errored with errs, after it failed, in a fenced
// transaction of its own. It writes nothing when the run was interrupted or
// lost its lock: an interruption is not a failure and the migration stays
// pending, and a runner that lost its lock writes nothing. When the
// fence finds the lock lost, the run is cancelled, nothing is written and
// the error says so.
func (r *Run) WriteErrored(m migration.Migration, errs []StepError) error {
	if r.Err() != nil {
		return nil
	}
	if errs == nil {
		errs = []StepError{}
	}
	id := bson.NewObjectID()
	err := r.transaction(func(ctx context.Context) error {
		return r.writeEntry(ctx, id, m, StatusErrored, bson.E{Key: "errors", Value: errs})
	}, true, &id)
	if err != nil {
		return fmt.Errorf("no Errored changelog entry written for %s: %w", m.ID, err)
	}
	return nil
}

// writeEntry upserts the entry with _id id with an update pipeline, so
// createdAt comes from the server's $$NOW (see entrySet).
func (r *Run) writeEntry(ctx context.Context, id bson.ObjectID, m migration.Migration, status Status, outcome bson.E) error {
	set, err := r.entrySet(m, status, outcome)
	if err != nil {
		return err
	}
	_, err = r.changelog.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		mongo.Pipeline{{{Key: "$set", Value: set}}},
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("write %s changelog entry for %s: %w", status, m.ID, err)
	}
	r.log.Info("changelog entry written", "migration", m.ID, "status", string(status))
	return nil
}

// entrySet is the $set stage writeEntry writes an entry with: every field
// but _id. Every client value goes through $literal, so operators in the
// migration snapshot aren't evaluated.
func (r *Run) entrySet(m migration.Migration, status Status, outcome bson.E) (bson.D, error) {
	snap, err := snapshot(m)
	if err != nil {
		return nil, err
	}
	lit := func(v any) bson.D { return bson.D{{Key: "$literal", Value: v}} }
	set := bson.D{
		{Key: "v", Value: lit(int32(EntryVersion))},
		{Key: "migrationId", Value: lit(m.ID)},
		{Key: "checksum", Value: lit(m.Checksum)},
		{Key: "createdAt", Value: "$$NOW"},
		{Key: "runner", Value: lit(r.runnerID)},
		{Key: "approvedBy", Value: lit(r.approvedBy)},
		{Key: "status", Value: lit(status)},
		{Key: "migration", Value: lit(snap)},
		{Key: outcome.Key, Value: lit(outcome.Value)},
	}
	return set, nil
}
