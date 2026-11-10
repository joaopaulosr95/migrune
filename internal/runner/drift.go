package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// ServerIndexes are the indexes in a database, as drift compares them.
type ServerIndexes struct {
	// Indexes are every collection's indexes, as listIndexes returns them,
	// by collection name then in listIndexes order.
	Indexes []migration.Index
	// Collations are the default collations of the collections that have
	// one, which their indexes inherit unless given another.
	Collations map[string]bson.D
}

// ListServerIndexes reads the indexes of every collection in db with
// listCollections and listIndexes, leaving out views, which have none, and
// the reserved collections (migration.IsReservedCollection). It only reads.
func ListServerIndexes(ctx context.Context, db *mongo.Database) (ServerIndexes, error) {
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{{Key: "type", Value: bson.D{{Key: "$ne", Value: "view"}}}})
	if err != nil {
		return ServerIndexes{}, fmt.Errorf("list collections: %w", err)
	}
	slices.SortFunc(specs, func(a, b mongo.CollectionSpecification) int { return strings.Compare(a.Name, b.Name) })
	out := ServerIndexes{Collations: map[string]bson.D{}}
	for _, spec := range specs {
		if migration.IsReservedCollection(spec.Name) {
			continue
		}
		var opts struct {
			Collation bson.D `bson:"collation"`
		}
		if spec.Options != nil {
			if err := bson.Unmarshal(spec.Options, &opts); err != nil {
				return ServerIndexes{}, fmt.Errorf("read the options of collection %s: %w", spec.Name, err)
			}
		}
		if opts.Collation != nil {
			out.Collations[spec.Name] = opts.Collation
		}
		indexes, err := listIndexes(ctx, db.Collection(spec.Name))
		if err != nil {
			return ServerIndexes{}, fmt.Errorf("list the indexes of collection %s: %w", spec.Name, err)
		}
		out.Indexes = append(out.Indexes, indexes...)
	}
	return out, nil
}
