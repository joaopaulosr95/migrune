package runner

import (
	"context"
	"fmt"
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
