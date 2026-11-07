package lint

import (
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// TestMatchIndexesCollations checks two indexes on the same keys, told apart
// only by their collation, each match the expected index meant for them,
// whatever the order: an index whose collation gives only the locale must
// not take the expected strength 2 that the other index gives in full.
func TestMatchIndexesCollations(t *testing.T) {
	ix := func(name string, collation bson.D) Index {
		return Index{Index: migration.Index{Collection: "users", Name: name,
			Keys: bson.D{{Key: "email", Value: int32(1)}}, Options: bson.D{{Key: "collation", Value: collation}}}}
	}
	// As listIndexes shows them, with every default and the version.
	expanded := func(name string, strength int32) Index {
		return ix(name, bson.D{
			{Key: "locale", Value: "en"}, {Key: "caseLevel", Value: false}, {Key: "caseFirst", Value: "off"},
			{Key: "strength", Value: strength}, {Key: "numericOrdering", Value: false},
			{Key: "alternate", Value: "non-ignorable"}, {Key: "maxVariable", Value: "punct"},
			{Key: "normalization", Value: false}, {Key: "backwards", Value: false}, {Key: "version", Value: "57.1"},
		})
	}
	replayed := []Index{
		ix("email_en", bson.D{{Key: "locale", Value: "en"}}),
		ix("email_ci", bson.D{{Key: "locale", Value: "en"}, {Key: "strength", Value: int32(2)}}),
	}
	ci, en := expanded("email_ci", 2), expanded("email_en", 3)
	for _, expected := range [][]Index{{ci, en}, {en, ci}} {
		for _, got := range [][]Index{replayed, {replayed[1], replayed[0]}} {
			name := fmt.Sprintf("expected %s,%s replayed %s,%s", expected[0].Name, expected[1].Name, got[0].Name, got[1].Name)
			t.Run(name, func(t *testing.T) {
				var rules []string
				for _, d := range matchIndexes(expected, got, sameExpected) {
					rules = append(rules, string(d.rule)+" "+d.want.Name+" "+d.got.Name)
				}
				if len(rules) != 0 {
					t.Errorf("findings %v, want none", rules)
				}
			})
		}
	}
}
