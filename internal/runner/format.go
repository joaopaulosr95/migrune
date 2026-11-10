package runner

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// FormatError reports a changelog entry or lock document written in a format
// newer than this migrune understands, by a newer migrune. Downgrading
// across a format change isn't supported.
type FormatError struct {
	// What is "changelog" or "migration lock".
	What string
	// Found is the newer format version, Known the one this migrune writes.
	Found, Known int
}

func (e *FormatError) Error() string {
	return fmt.Sprintf("the %s has format v%d, newer than this migrune understands (v%d): upgrade migrune",
		e.What, e.Found, e.Known)
}

// CheckFormats fails with a *FormatError when the changelog holds an entry,
// in any namespace, with a v above EntryVersion, or the lock collection a
// document with a v above LockVersion. A missing v reads as 1. Every command
// that connects runs it once, before any other read or write, so no later
// read can forget it. It only reads.
//
// Neither query has an index on v, and neither needs one. The lock collection
// holds one document per namespace. The changelog holds one entry per
// migration outcome, so it grows by a few entries per release, and every
// plan, apply and online lint already reads its Finished entries (Finished):
// the scan costs about as much as that read. An index would only help once
// apply had created it, and plan, lint and drift don't write.
func CheckFormats(ctx context.Context, db *mongo.Database) error {
	if err := checkFormat(ctx, changelogCollection(db), "changelog", EntryVersion); err != nil {
		return err
	}
	return checkFormat(ctx, lockCollection(db), "migration lock", LockVersion)
}

// checkFormat finds the document of coll with the highest v above known.
func checkFormat(ctx context.Context, coll *mongo.Collection, what string, known int) error {
	var doc struct {
		V int `bson:"v"`
	}
	err := coll.FindOne(ctx, bson.D{{Key: "v", Value: bson.D{{Key: "$gt", Value: known}}}},
		options.FindOne().
			SetSort(bson.D{{Key: "v", Value: -1}}).
			SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "v", Value: 1}})).
		Decode(&doc)
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return nil
	case err != nil:
		return fmt.Errorf("check the %s format version: %w", what, err)
	}
	return &FormatError{What: what, Found: doc.V, Known: known}
}
