package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// Merge migration fixtures in testdata/merge.
const (
	copyLabel      = "2026-11-03-001-copy-label"
	amountToDouble = "2026-11-03-002-amount-to-double"
	priceBySKU     = "2026-11-03-003-price-by-sku"
	qtyToDouble    = "2026-11-03-004-qty-to-double"
	randomScore    = "2026-11-03-005-random-score"
	onTwoFields    = "2026-11-03-006-on-two-fields"
	amountToLong   = "2026-11-03-007-amount-to-long"
	defaultCurr    = "2026-11-03-008-default-currency"
	deepToDouble   = "2026-11-03-009-deep-to-double"
	lineQtyDouble  = "2026-11-03-010-line-qty-to-double"
	levelFive      = "2026-11-03-011-level-five-to-double"
	levelSix       = "2026-11-03-012-level-six-to-double"
	cityOnly       = "2026-11-03-013-address-city-only"
	priceWithID    = "2026-11-03-014-price-by-sku-with-id"
	priceCurrency  = "2026-11-03-015-price-and-currency-by-sku"
)

// counts renders merge counts; nil counts are "-".
func counts(c MergeCounts) string {
	n := func(p *int64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatInt(*p, 10)
	}
	return "before=" + n(c.DiffCountBefore) + " after=" + n(c.DiffCountAfter) +
		" unmatched=" + n(c.UnmatchedCount) + " typeChanged=" + n(c.TypeChangedCount) +
		" duplicateOn=" + n(c.DuplicateOnCount) + " nullOn=" + n(c.NullOnCount) +
		" arrayOn=" + n(c.ArrayOnCount) + " idMismatch=" + n(c.IDMismatchCount)
}

// samples renders a merge row's samples as "key field: change; ...".
func samples(row Row) []string {
	var out []string
	for _, s := range row.Samples {
		var fields []string
		for _, f := range s.Fields {
			fields = append(fields, f.Field+": "+f.Describe())
		}
		out = append(out, string(s.Key)+" "+strings.Join(fields, "; "))
	}
	return out
}

func insert(t *testing.T, ctx context.Context, coll *mongo.Collection, docs ...bson.D) {
	t.Helper()
	if _, err := coll.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
}

// TestMergeComparison checks how plan and apply count a merge's changes:
// top-level fields, nested values whole, type-strict, nested
// types checked down to mergeTypeDepth levels (changed arrays excepted).
func TestMergeComparison(t *testing.T) {
	cases := []struct {
		name string
		id   string
		mode Mode
		seed func(t *testing.T, ctx context.Context, db *mongo.Database)
		// status and counts of the row; err is part of the run's error.
		status  RowStatus
		counts  string
		samples []string
		err     string
		// check runs on the data afterwards.
		check func(t *testing.T, ctx context.Context, db *mongo.Database)
	}{
		{
			name: "adding a missing field is not a type change",
			id:   copyLabel, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("items"),
					bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "a"}},
					bson.D{{Key: "_id", Value: 2}, {Key: "name", Value: "b"}, {Key: "label", Value: "b"}})
			},
			status:  RowApplied,
			counts:  "before=1 after=- unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} label: absent -> "a"`},
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "label", Value: "a"}}); n != 0 {
					t.Errorf("plan wrote %d labels", n)
				}
			},
		},
		{
			name: "a null field getting a string is a diff, not a type change, and settles",
			id:   copyLabel, mode: ModeApply,
			seed:    seedNullLabel,
			status:  RowApplied,
			counts:  "before=1 after=0 unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} label: null (null) -> "a" (string)`},
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "_id", Value: 1}, {Key: "label", Value: "a"}}); n != 1 {
					t.Error("item 1 has no label a")
				}
			},
		},
		{
			name: "a nested int32 turned into a double is a type change",
			id:   amountToDouble, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("prices"),
					bson.D{{Key: "_id", Value: 1}, {Key: "price", Value: bson.D{{Key: "amount", Value: int32(10)}}}},
					bson.D{{Key: "_id", Value: 2}, {Key: "price", Value: bson.D{{Key: "amount", Value: 10.0}}}})
			},
			status:  RowErrored,
			counts:  "before=1 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} price: {"amount":10} -> {"amount":10.0}, type changed at price.amount (int -> double)`},
			err:     "typeChangedCount is 1",
		},
		{
			name: "a nested long turned into a double of equal value is a type change",
			id:   amountToDouble, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("prices"),
					bson.D{{Key: "_id", Value: 1}, {Key: "price", Value: bson.D{{Key: "amount", Value: int64(5)}}}},
					bson.D{{Key: "_id", Value: 2}, {Key: "price", Value: bson.D{{Key: "amount", Value: 5.0}}}})
			},
			status:  RowErrored,
			counts:  "before=1 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} price: {"amount":5} -> {"amount":5.0}, type changed at price.amount (long -> double)`},
			err:     "typeChangedCount is 1",
		},
		{
			name: "a nested int32 turned into a long is a type change",
			id:   amountToLong, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("prices"),
					bson.D{{Key: "_id", Value: 1}, {Key: "price", Value: bson.D{{Key: "amount", Value: int32(10)}}}})
			},
			status:  RowErrored,
			counts:  "before=1 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} price: {"amount":10} -> {"amount":10}, type changed at price.amount (int -> long)`},
			err:     "typeChangedCount is 1",
		},
		{
			name: "a nested type change along with another change in the document is a type change",
			id:   amountToLong, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("prices"),
					bson.D{{Key: "_id", Value: 1}, {Key: "price", Value: bson.D{{Key: "note", Value: "x"}, {Key: "amount", Value: int32(10)}}}})
			},
			status:  RowErrored,
			counts:  "before=1 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} price: {"note":"x","amount":10} -> {"amount":10}, type changed at price.amount (int -> long)`},
			err:     "typeChangedCount is 1",
		},
		{
			name: "a nested type change inside an array that changes otherwise is only a diff",
			id:   lineQtyDouble, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("orders"),
					bson.D{{Key: "_id", Value: 1}, {Key: "lines", Value: bson.A{bson.D{{Key: "qty", Value: int64(2)}, {Key: "sku", Value: "a"}}}}})
			},
			status:  RowApplied,
			counts:  "before=1 after=- unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} lines: [{"qty":2,"sku":"a"}] -> [{"qty":2.0}]`},
		},
		{
			name: "a nested null getting a value is a diff, not a type change, and settles",
			id:   defaultCurr, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("prices"),
					bson.D{{Key: "_id", Value: 1}, {Key: "price", Value: bson.D{{Key: "amount", Value: int64(5)}, {Key: "currency", Value: nil}}}},
					bson.D{{Key: "_id", Value: 2}, {Key: "price", Value: bson.D{{Key: "amount", Value: int64(7)}, {Key: "currency", Value: "USD"}}}})
			},
			status:  RowApplied,
			counts:  "before=1 after=0 unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} price: {"amount":5,"currency":null} -> {"amount":5,"currency":"EUR"}`},
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("prices"), bson.D{{Key: "_id", Value: 1}, {Key: "price.currency", Value: "EUR"}}); n != 1 {
					t.Error("price 1 has no currency EUR")
				}
			},
		},
		{
			name: "a nested array element turned into a double is a type change",
			id:   lineQtyDouble, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("orders"),
					bson.D{{Key: "_id", Value: 1}, {Key: "lines", Value: bson.A{bson.D{{Key: "qty", Value: 1.0}}, bson.D{{Key: "qty", Value: int64(2)}}}}})
			},
			status:  RowErrored,
			counts:  "before=1 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} lines: [{"qty":1.0},{"qty":2}] -> [{"qty":1.0},{"qty":2.0}], type changed at lines.1.qty (long -> double)`},
			err:     "typeChangedCount is 1",
		},
		{
			name: "a type change at level 3 is found",
			id:   deepToDouble, mode: ModePlan,
			seed:    seedDeepLong,
			status:  RowErrored,
			counts:  "before=1 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} a: {"b":{"c":{"d":7}}} -> {"b":{"c":{"d":7.0}}}, type changed at a.b.c.d (long -> double)`},
			err:     "typeChangedCount is 1",
		},
		{
			name: "a type change at level 5, the deepest checked, is found",
			id:   levelFive, mode: ModePlan,
			seed:    func(t *testing.T, ctx context.Context, db *mongo.Database) { seedNested(t, ctx, db, "abcdef") },
			status:  RowErrored,
			counts:  "before=1 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} a: {"b":{"c":{"d":{"e":{"f":7}}}}} -> {"b":{"c":{"d":{"e":{"f":7.0}}}}}, type changed at a.b.c.d.e.f (long -> double)`},
			err:     "typeChangedCount is 1",
		},
		{
			name: "a type change at level 6 falls back to $eq and size",
			id:   levelSix, mode: ModeApply,
			seed:   func(t *testing.T, ctx context.Context, db *mongo.Database) { seedNested(t, ctx, db, "abcdefg") },
			status: RowApplied,
			counts: "before=0 after=0 unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("shapes"), bson.D{{Key: "a.b.c.d.e.f.g", Value: bson.D{{Key: "$type", Value: "double"}}}}); n != 1 {
					t.Error("the long beyond the depth was not converted: the fallback should miss it")
				}
			},
		},
		{
			name: "dropping a non-null nested field is a type change to missing",
			id:   cityOnly, mode: ModePlan,
			seed:   seedAddresses,
			status: RowErrored,
			counts: "before=2 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{
				`{"_id":1} address: {"city":"Lisbon","street":"Rua Augusta"} -> {"city":"Lisbon"}, type changed at address.street (string -> missing)`,
				`{"_id":2} address: {"city":"Porto","street":null} -> {"city":"Porto"}`,
			},
			err: "typeChangedCount is 1: the merge would change the BSON type of existing fields in people, or drop nested ones",
		},
		{
			name: "a case-only change is a diff under a case-insensitive default collation",
			id:   copyLabel, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				err := db.CreateCollection(ctx, "items", options.CreateCollection().
					SetCollation(&options.Collation{Locale: "en", Strength: 2}))
				if err != nil {
					t.Fatal(err)
				}
				insert(t, ctx, db.Collection("items"), bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "ABC"}, {Key: "label", Value: "abc"}})
			},
			status:  RowApplied,
			counts:  "before=1 after=0 unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"_id":1} label: "abc" -> "ABC"`},
		},
		{
			name: "an on mismatch shows up in unmatchedCount",
			id:   priceBySKU, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				items := db.Collection("items")
				// $merge on fields other than _id need a unique index.
				if _, err := items.Indexes().CreateOne(ctx, mongo.IndexModel{
					Keys: bson.D{{Key: "sku", Value: 1}}, Options: options.Index().SetUnique(true)}); err != nil {
					t.Fatal(err)
				}
				insert(t, ctx, items,
					bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "a"}},
					bson.D{{Key: "_id", Value: 2}, {Key: "sku", Value: "b"}, {Key: "price", Value: int32(2)}})
				insert(t, ctx, db.Collection("sources"),
					bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}},
					bson.D{{Key: "sku", Value: "b"}, {Key: "price", Value: int32(2)}},
					bson.D{{Key: "sku", Value: "z"}, {Key: "price", Value: int32(9)}})
			},
			status:  RowApplied,
			counts:  "before=1 after=0 unmatched=1 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"sku":"a"} price: absent -> 1`},
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				items := db.Collection("items")
				if n := countDocs(t, ctx, items, bson.D{}); n != 2 {
					t.Errorf("%d items, want 2: the unmatched document must be discarded", n)
				}
				if n := countDocs(t, ctx, items, bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}}); n != 1 {
					t.Error("item a has no price")
				}
			},
		},
		{
			name: "output documents sharing an on value fail plan",
			id:   priceBySKU, mode: ModePlan,
			seed:   seedDuplicateSKU,
			status: RowErrored,
			counts: "before=2 after=- unmatched=0 typeChanged=0 duplicateOn=2 nullOn=0 arrayOn=0 idMismatch=0",
			err:    "duplicateOnCount is 2: that many output documents share their on fields (sku) with another",
		},
		{
			name: "output documents sharing an on value with no target pass, and $merge discards them",
			id:   priceBySKU, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedSKUs(t, ctx, db,
					bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}},
					bson.D{{Key: "sku", Value: "z"}, {Key: "price", Value: int32(2)}},
					bson.D{{Key: "sku", Value: "z"}, {Key: "price", Value: int32(3)}})
			},
			status:  RowApplied,
			counts:  "before=1 after=0 unmatched=2 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"sku":"a"} price: absent -> 1`},
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("items"), bson.D{}); n != 1 {
					t.Errorf("%d items, want 1: $merge must discard the unmatched duplicates", n)
				}
			},
		},
		{
			name: "only matched duplicates count",
			id:   priceBySKU, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedSKUs(t, ctx, db,
					bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}},
					bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(2)}},
					bson.D{{Key: "sku", Value: "z"}, {Key: "price", Value: int32(3)}},
					bson.D{{Key: "sku", Value: "z"}, {Key: "price", Value: int32(4)}})
			},
			status: RowErrored,
			counts: "before=2 after=- unmatched=2 typeChanged=0 duplicateOn=2 nullOn=0 arrayOn=0 idMismatch=0",
			err:    "duplicateOnCount is 2",
		},
		{
			name: "an array on value fails plan",
			id:   priceBySKU, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedSKUs(t, ctx, db,
					bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}},
					bson.D{{Key: "sku", Value: bson.A{"a", "b"}}, {Key: "price", Value: int32(2)}},
					bson.D{{Key: "sku", Value: bson.A{"x"}}, {Key: "price", Value: int32(3)}})
			},
			status: RowErrored,
			counts: "before=1 after=- unmatched=2 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=2 idMismatch=0",
			err:    "arrayOnCount is 2: that many output documents have an array as an on field (sku)",
		},
		{
			// Documented in writing-migrations.md: $merge runs with the
			// source collection's default collation and, from MongoDB 8.3,
			// matches through the target's index with that collation, while
			// the comparison compares on values exactly. MongoDB 7.0 to
			// 8.2 discard the document, as plan predicts.
			name: "an on value differing only in case is unmatched in plan, and merges in apply from 8.3 under a case-insensitive index",
			id:   priceBySKU, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				ci := &options.Collation{Locale: "en", Strength: 2}
				if err := db.CreateCollection(ctx, "sources", options.CreateCollection().SetCollation(ci)); err != nil {
					t.Fatal(err)
				}
				items := db.Collection("items")
				if _, err := items.Indexes().CreateOne(ctx, mongo.IndexModel{
					Keys: bson.D{{Key: "sku", Value: 1}}, Options: options.Index().SetUnique(true).SetCollation(ci)}); err != nil {
					t.Fatal(err)
				}
				insert(t, ctx, items, bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "abc"}})
				insert(t, ctx, db.Collection("sources"), bson.D{{Key: "sku", Value: "ABC"}, {Key: "price", Value: int32(1)}})
			},
			status: RowApplied,
			counts: "before=0 after=0 unmatched=1 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				var info struct {
					Version string `bson:"version"`
				}
				if err := db.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
					t.Fatal(err)
				}
				var want int64
				if mergesThroughCollation(info.Version) {
					want = 1
				}
				if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "sku", Value: "ABC"}, {Key: "price", Value: int32(1)}}); n != want {
					t.Errorf("MongoDB %s: %d items merged through the index's collation, want %d", info.Version, n, want)
				}
			},
		},
		{
			name: "an output _id other than the target's fails plan when on isn't _id",
			id:   priceWithID, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedSKUs(t, ctx, db, bson.D{{Key: "_id", Value: 100}, {Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}})
			},
			status: RowErrored,
			counts: "before=1 after=- unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=1",
			err:    "idMismatchCount is 1: that many output documents have an _id other than the _id of the document they match in items on sku",
		},
		{
			name: "an output _id equal to the target's passes when on isn't _id",
			id:   priceWithID, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedSKUs(t, ctx, db, bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}})
			},
			status: RowApplied,
			counts: "before=1 after=0 unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
		},
		{
			name: "an output document without its on field fails plan",
			id:   priceBySKU, mode: ModePlan,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedSKUs(t, ctx, db,
					bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}},
					bson.D{{Key: "price", Value: int32(2)}},
					bson.D{{Key: "price", Value: int32(3)}})
			},
			status: RowErrored,
			counts: "before=1 after=- unmatched=2 typeChanged=0 duplicateOn=0 nullOn=2 arrayOn=0 idMismatch=0",
			err:    "nullOnCount is 2: that many output documents have a null or missing on field (sku)",
		},
		{
			name: "an output document with a null on field fails apply before writing",
			id:   priceBySKU, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedSKUs(t, ctx, db,
					bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}},
					bson.D{{Key: "sku", Value: nil}, {Key: "price", Value: int32(2)}})
			},
			status: RowErrored,
			counts: "before=1 after=- unmatched=1 typeChanged=0 duplicateOn=0 nullOn=1 arrayOn=0 idMismatch=0",
			err:    "nullOnCount is 1: that many output documents have a null or missing on field (sku)",
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "price", Value: bson.D{{Key: "$exists", Value: true}}}}); n != 0 {
					t.Errorf("apply priced %d items", n)
				}
			},
		},
		{
			name: "every on field must match",
			id:   onTwoFields, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				items := db.Collection("items")
				if _, err := items.Indexes().CreateOne(ctx, mongo.IndexModel{
					Keys: bson.D{{Key: "sku", Value: 1}, {Key: "region", Value: 1}}, Options: options.Index().SetUnique(true)}); err != nil {
					t.Fatal(err)
				}
				insert(t, ctx, items,
					bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "a"}, {Key: "region", Value: "eu"}},
					bson.D{{Key: "_id", Value: 2}, {Key: "sku", Value: "a"}, {Key: "region", Value: "us"}})
				insert(t, ctx, db.Collection("sources"),
					bson.D{{Key: "sku", Value: "a"}, {Key: "region", Value: "us"}, {Key: "price", Value: int32(1)}},
					bson.D{{Key: "sku", Value: "a"}, {Key: "region", Value: "br"}, {Key: "price", Value: int32(2)}})
			},
			status:  RowApplied,
			counts:  "before=1 after=0 unmatched=1 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{`{"sku":"a","region":"us"} price: absent -> 1`},
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "price", Value: bson.D{{Key: "$exists", Value: true}}}}); n != 1 {
					t.Errorf("%d items priced, want only the us one", n)
				}
			},
		},
		{
			// Every check that fails plan fails apply before writing:
			// they all run before the $merge.
			name: "a type change fails apply before writing",
			id:   qtyToDouble, mode: ModeApply,
			seed:   seedCounters,
			status: RowErrored,
			counts: "before=2 after=- unmatched=0 typeChanged=1 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			samples: []string{
				`{"_id":1} qty: 1 (int) -> 1.0 (double), type changed`,
				`{"_id":2} qty: absent -> null`,
			},
			err: "typeChangedCount is 1: the merge would change the BSON type of existing fields in counters",
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("counters"), bson.D{{Key: "qty", Value: bson.D{{Key: "$type", Value: "double"}}}}); n != 0 {
					t.Errorf("apply converted %d counters", n)
				}
				if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{qtyToDouble + ":Errored"}) {
					t.Errorf("changelog %v", got)
				}
			},
		},
		{
			name: "diffCountAfter reaches 0 after apply",
			id:   copyLabel, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedItems(t, ctx, db)
			},
			status: RowApplied,
			counts: "before=3 after=0 unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "$expr", Value: bson.D{{Key: "$eq", Value: bson.A{"$label", "$name"}}}}}); n != 3 {
					t.Errorf("%d items labelled, want 3", n)
				}
				entries := changelog(t, ctx, db)
				if got := entryStatuses(entries); !slices.Equal(got, []string{copyLabel + ":Finished"}) {
					t.Fatalf("changelog %v", got)
				}
				three, zero := int64(3), int64(0)
				want := []StepResult{{Collection: "items", Operation: "aggregate",
					DiffCountBefore: &three, DiffCountAfter: &zero, UnmatchedCount: &zero, TypeChangedCount: &zero, DuplicateOnCount: &zero,
					NullOnCount: &zero, ArrayOnCount: &zero, IDMismatchCount: &zero}}
				if got := entries[0].Results; len(got) != 1 || counts(got[0].MergeCounts) != counts(want[0].MergeCounts) ||
					got[0].Collection != "items" || got[0].Operation != "aggregate" {
					t.Errorf("results %+v", got)
				}
			},
		},
		{
			name: "a merge that doesn't settle is errored",
			id:   randomScore, mode: ModeApply,
			seed: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				seedItems(t, ctx, db)
			},
			status: RowErrored,
			counts: "before=3 after=3 unmatched=0 typeChanged=0 duplicateOn=0 nullOn=0 arrayOn=0 idMismatch=0",
			err:    "diffCountAfter is 3: documents in items still differ from the output after the merge",
			check: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				entries := changelog(t, ctx, db)
				if got := entryStatuses(entries); !slices.Equal(got, []string{randomScore + ":Errored"}) {
					t.Fatalf("changelog %v", got)
				}
				if e := entries[0].Errors; len(e) != 1 || e[0].Step == nil || *e[0].Step != 0 {
					t.Errorf("errors %+v, want one on step 0", e)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			tc.seed(t, ctx, db)
			ms := load(t, fixtureDir(t, "merge", tc.id))
			rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: tc.mode, SampleValues: true})
			if tc.err == "" && err != nil {
				t.Fatalf("%s: %v", tc.mode, err)
			}
			if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("%s: %v, want an error containing %q", tc.mode, err, tc.err)
			}
			if len(rep.Migrations) != 1 {
				t.Fatalf("rows %v", statuses(rep))
			}
			row := rep.Migrations[0]
			if row.Kind != migration.KindMerge || row.Status != tc.status {
				t.Errorf("row %s %s, want merge %s", row.Kind, row.Status, tc.status)
			}
			if got := counts(row.MergeCounts); got != tc.counts {
				t.Errorf("counts %s, want %s", got, tc.counts)
			}
			if tc.samples != nil && !slices.Equal(samples(row), tc.samples) {
				t.Errorf("samples:\n  %s\nwant:\n  %s", strings.Join(samples(row), "\n  "), strings.Join(tc.samples, "\n  "))
			}
			if tc.mode == ModePlan {
				if n := countDocs(t, ctx, db.Collection(ChangelogCollection), bson.D{}); n != 0 {
					t.Errorf("plan wrote %d changelog entries", n)
				}
			}
			if tc.check != nil {
				tc.check(t, ctx, db)
			}
		})
	}
}

// seedNullLabel inserts an item whose label is null, which copyLabel would
// set to its name, and one already labelled.
func seedNullLabel(t *testing.T, ctx context.Context, db *mongo.Database) {
	insert(t, ctx, db.Collection("items"),
		bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "a"}, {Key: "label", Value: nil}},
		bson.D{{Key: "_id", Value: 2}, {Key: "name", Value: "b"}, {Key: "label", Value: "b"}})
}

// seedDeepLong inserts a shape with a long three levels below its top-level
// field a, which deepToDouble would turn into a double of the same value.
func seedDeepLong(t *testing.T, ctx context.Context, db *mongo.Database) {
	insert(t, ctx, db.Collection("shapes"),
		bson.D{{Key: "_id", Value: 1}, {Key: "a", Value: bson.D{{Key: "b", Value: bson.D{{Key: "c", Value: bson.D{{Key: "d", Value: int64(7)}}}}}}}})
}

// seedNested inserts a shape whose fields, named by path's letters, nest
// one in the other down to a long 7: "abc" gives {a: {b: {c: long 7}}}.
func seedNested(t *testing.T, ctx context.Context, db *mongo.Database, path string) {
	var v any = int64(7)
	for i := len(path) - 1; i > 0; i-- {
		v = bson.D{{Key: path[i : i+1], Value: v}}
	}
	insert(t, ctx, db.Collection("shapes"), bson.D{{Key: "_id", Value: 1}, {Key: path[:1], Value: v}})
}

// seedAddresses inserts people whose address cityOnly would rewrite: one
// with a street, which it would drop; one with a null street, which may go;
// one with only a city, which stays the same.
func seedAddresses(t *testing.T, ctx context.Context, db *mongo.Database) {
	insert(t, ctx, db.Collection("people"),
		bson.D{{Key: "_id", Value: 1}, {Key: "address", Value: bson.D{{Key: "city", Value: "Lisbon"}, {Key: "street", Value: "Rua Augusta"}}}},
		bson.D{{Key: "_id", Value: 2}, {Key: "address", Value: bson.D{{Key: "city", Value: "Porto"}, {Key: "street", Value: nil}}}},
		bson.D{{Key: "_id", Value: 3}, {Key: "address", Value: bson.D{{Key: "city", Value: "Faro"}}}})
}

// seedSKUs inserts an item with sku a, a unique index on sku, and sources.
func seedSKUs(t *testing.T, ctx context.Context, db *mongo.Database, sources ...bson.D) {
	items := db.Collection("items")
	if _, err := items.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "sku", Value: 1}}, Options: options.Index().SetUnique(true)}); err != nil {
		t.Fatal(err)
	}
	insert(t, ctx, items, bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "a"}})
	insert(t, ctx, db.Collection("sources"), sources...)
}

// seedDuplicateSKU inserts an item with sku a, and two sources with sku a
// and different prices, which priceBySKU would both merge into it.
func seedDuplicateSKU(t *testing.T, ctx context.Context, db *mongo.Database) {
	items := db.Collection("items")
	if _, err := items.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "sku", Value: 1}}, Options: options.Index().SetUnique(true)}); err != nil {
		t.Fatal(err)
	}
	insert(t, ctx, items, bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "a"}})
	insert(t, ctx, db.Collection("sources"),
		bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}},
		bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(2)}})
}

// seedCounters inserts a counter whose qty is an int, which qtyToDouble would
// turn into a double, and one without qty, which it would add as null.
func seedCounters(t *testing.T, ctx context.Context, db *mongo.Database) {
	insert(t, ctx, db.Collection("counters"),
		bson.D{{Key: "_id", Value: 1}, {Key: "qty", Value: int32(1)}},
		bson.D{{Key: "_id", Value: 2}, {Key: "label", Value: "x"}})
}

func TestSplitMerge(t *testing.T) {
	cases := []struct {
		dir, id string
		into    string
		on      []string
		stages  int
		err     string
	}{
		{dir: "merge", id: copyLabel, into: "items", on: []string{"_id"}, stages: 1},
		{dir: "merge", id: amountToDouble, into: "prices", on: []string{"_id"}, stages: 1},
		{dir: "merge", id: priceBySKU, into: "items", on: []string{"sku"}, stages: 1},
		{dir: "merge", id: onTwoFields, into: "items", on: []string{"sku", "region"}, stages: 1},
		{dir: "merge-not-last", id: "2026-11-03-001-merge-not-last", err: "the $merge stage must be the last stage"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			ms := load(t, fixtureDir(t, tc.dir, tc.id))
			pre, target, err := splitMerge(ms[0].Steps[0])
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("splitMerge: %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || target.Into != tc.into || !slices.Equal(target.On, tc.on) || len(pre) != tc.stages {
				t.Errorf("splitMerge = %d stages, %+v, %v; want %d stages into %s on %v", len(pre), target, err, tc.stages, tc.into, tc.on)
			}
		})
	}
}

// TestMergeSampleValues checks merge samples leave values out of the report
// and the logs by default, and show them, cut to MaxSampleValueChars
// characters, with SampleValues.
func TestMergeSampleValues(t *testing.T) {
	long := strings.Repeat("x", MaxSampleValueChars+50)
	cases := []struct {
		name   string
		values bool
		label  string
		// samples are the rendered samples, the type change first; absent
		// must appear in neither the report nor the logs; cut tells whether
		// the second sample's value is cut.
		samples []string
		absent  string
		cut     bool
	}{
		{name: "default", label: "secret-a", absent: "secret-a",
			samples: []string{`{"_id":2} label: int -> string, type changed`, `{"_id":1} label: missing -> string`}},
		{name: "values", values: true, label: "secret-a",
			samples: []string{`{"_id":2} label: 5 (int) -> "b" (string), type changed`, `{"_id":1} label: absent -> "secret-a"`}},
		{name: "long value", values: true, label: long, absent: long, cut: true,
			samples: []string{`{"_id":2} label: 5 (int) -> "b" (string), type changed`, `{"_id":1} label: absent -> "\"` + strings.Repeat("x", MaxSampleValueChars-1) + `…"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			insert(t, ctx, db.Collection("items"),
				bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: tc.label}},
				bson.D{{Key: "_id", Value: 2}, {Key: "name", Value: "b"}, {Key: "label", Value: int32(5)}})
			var logs bytes.Buffer
			o := Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			rep, _ := execute(t, ctx, db, load(t, fixtureDir(t, "merge", copyLabel)), o, Config{Mode: ModePlan, SampleValues: tc.values})
			row := rep.Migrations[0]
			if got := samples(row); !slices.Equal(got, tc.samples) {
				t.Errorf("samples:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(tc.samples, "\n  "))
			}
			if len(row.Samples) == 2 && row.Samples[1].Fields[0].Truncated != tc.cut {
				t.Errorf("truncated %v, want %v", row.Samples[1].Fields[0].Truncated, tc.cut)
			}
			raw, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			if tc.absent != "" && (strings.Contains(string(raw), tc.absent) || strings.Contains(logs.String(), tc.absent)) {
				t.Errorf("the value %.20s… is in the report or the logs:\n%s\n%s", tc.absent, raw, logs.String())
			}
			if !strings.Contains(logs.String(), `"msg":"merge sample: a document that differs from its target"`) {
				t.Errorf("no sample logged:\n%s", logs.String())
			}
		})
	}
}

// TestMergeArrayOnServerError checks the server behaviour the arrayOnCount
// check guards against: $merge fails with error 51185 on an output document
// whose on field is an array, matched or not, instead of skipping it.
func TestMergeArrayOnServerError(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedSKUs(t, ctx, db, bson.D{{Key: "sku", Value: bson.A{"x", "y"}}, {Key: "price", Value: int32(1)}})
	_, err := db.Collection("sources").Aggregate(ctx, bson.A{
		bson.D{{Key: "$project", Value: bson.D{{Key: "_id", Value: 0}, {Key: "sku", Value: 1}, {Key: "price", Value: 1}}}},
		bson.D{{Key: "$merge", Value: bson.D{{Key: "into", Value: "items"}, {Key: "on", Value: "sku"},
			{Key: "whenMatched", Value: "merge"}, {Key: "whenNotMatched", Value: "discard"}}}},
	})
	var se mongo.ServerError
	if !errors.As(err, &se) || !se.HasErrorCode(51185) {
		t.Fatalf("$merge with an array on value: %v, want server error 51185", err)
	}
}
