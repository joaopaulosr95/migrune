package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// codeMergeNoUniqueIndex is the error $merge fails with when the target has
// no unique index it can match the on fields through.
const codeMergeNoUniqueIndex = 51183

// TestMergeUniqueIndex checks plan fails, before writing, a merge on fields
// other than _id whose target has no unique index MongoDB accepts, and that
// the server agrees: running the same $merge directly fails with error 51183
// exactly when plan fails.
func TestMergeUniqueIndex(t *testing.T) {
	ci := &options.Collation{Locale: "en", Strength: 2}
	unique := func(keys bson.D, o *options.IndexOptionsBuilder) func(t *testing.T, ctx context.Context, db *mongo.Database) {
		return func(t *testing.T, ctx context.Context, db *mongo.Database) {
			if _, err := db.Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys, Options: o}); err != nil {
				t.Fatal(err)
			}
		}
	}
	sku := bson.D{{Key: "sku", Value: 1}}
	cases := []struct {
		name string
		id   string
		// sourceCollation is the default collation of sources.
		sourceCollation *options.Collation
		// noTarget leaves items uncreated.
		noTarget bool
		index    func(t *testing.T, ctx context.Context, db *mongo.Database)
		// err is part of plan's error; empty when plan passes.
		err string
	}{
		{name: "no target collection", id: priceBySKU, noTarget: true,
			err: `$merge on sku needs a unique, non-partial index on exactly {"sku": 1} in items, with the simple collation, ` +
				"which the pipeline runs with as the default collation of sources; there is none, so $merge would fail (error 51183)"},
		{name: "no index", id: priceBySKU, err: "there is none, so $merge would fail (error 51183). Create it in an earlier index migration"},
		{name: "not unique", id: priceBySKU, index: unique(sku, options.Index()), err: "on these fields, index sku_1 isn't unique"},
		{name: "unique", id: priceBySKU, index: unique(sku, options.Index().SetUnique(true))},
		{name: "unique, descending", id: priceBySKU, index: unique(bson.D{{Key: "sku", Value: -1}}, options.Index().SetUnique(true))},
		{name: "unique and sparse", id: priceBySKU, index: unique(sku, options.Index().SetUnique(true).SetSparse(true))},
		{name: "unique and hidden", id: priceBySKU, index: unique(sku, options.Index().SetUnique(true).SetHidden(true))},
		{name: "unique and partial", id: priceBySKU,
			index: unique(sku, options.Index().SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "sku", Value: bson.D{{Key: "$exists", Value: true}}}})),
			err:   "index sku_1 is partial"},
		{name: "unique with another key", id: priceBySKU,
			index: unique(bson.D{{Key: "sku", Value: 1}, {Key: "region", Value: 1}}, options.Index().SetUnique(true)), err: "there is none"},
		{name: "two on fields, keys in another order", id: onTwoFields,
			index: unique(bson.D{{Key: "region", Value: 1}, {Key: "sku", Value: 1}}, options.Index().SetUnique(true))},
		{name: "two on fields, one key", id: onTwoFields, index: unique(sku, options.Index().SetUnique(true)),
			err: `needs a unique, non-partial index on exactly {"sku": 1, "region": 1} in items`},
		{name: "case-insensitive index, simple pipeline", id: priceBySKU, index: unique(sku, options.Index().SetUnique(true).SetCollation(ci)),
			err: `with the simple collation, which the pipeline runs with as the default collation of sources; there is none, so $merge would fail (error 51183). ` +
				`Create it in an earlier index migration; on these fields, index sku_1 has the collation {"alternate": "non-ignorable"`},
		{name: "simple index, case-insensitive pipeline", id: priceBySKU, sourceCollation: ci, index: unique(sku, options.Index().SetUnique(true)),
			err: `with the collation {"alternate": "non-ignorable", "backwards": false, "caseFirst": "off", "caseLevel": false, "locale": "en", ` +
				`"maxVariable": "punct", "normalization": false, "numericOrdering": false, "strength": 2, "version": `},
		{name: "case-insensitive index and pipeline", id: priceBySKU, sourceCollation: ci, index: unique(sku, options.Index().SetUnique(true).SetCollation(ci))},
		{name: "index and pipeline collations of another strength", id: priceBySKU, sourceCollation: &options.Collation{Locale: "en"},
			index: unique(sku, options.Index().SetUnique(true).SetCollation(ci)), err: "index sku_1 has the collation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			if tc.sourceCollation != nil {
				if err := db.CreateCollection(ctx, "sources", options.CreateCollection().SetCollation(tc.sourceCollation)); err != nil {
					t.Fatal(err)
				}
			}
			insert(t, ctx, db.Collection("sources"), bson.D{{Key: "sku", Value: "a"}, {Key: "region", Value: "eu"}, {Key: "price", Value: int32(1)}})
			if !tc.noTarget {
				insert(t, ctx, db.Collection("items"), bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "a"}, {Key: "region", Value: "eu"}})
			}
			if tc.index != nil {
				tc.index(t, ctx, db)
			}
			ms := load(t, fixtureDir(t, "merge", tc.id))
			rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan})
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("plan: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("plan: %v, want an error containing %q", err, tc.err)
			}
			if tc.err != "" && (rep.Migrations[0].Status != RowErrored || rep.Migrations[0].DiffCountBefore != nil) {
				t.Errorf("row %s with diffCountBefore %v, want errored before the comparison", rep.Migrations[0].Status, rep.Migrations[0].DiffCountBefore)
			}

			// The server's own verdict on the same $merge.
			s := ms[0].Steps[0]
			_, merr := db.Collection(s.Collection).Aggregate(ctx, toPipeline(s.Pipeline))
			var se mongo.ServerError
			rejected := errors.As(merr, &se) && se.HasErrorCode(codeMergeNoUniqueIndex)
			if merr != nil && !rejected {
				t.Fatalf("$merge: %v", merr)
			}
			if rejected != (tc.err != "") {
				t.Errorf("the server rejects the $merge: %v; plan fails: %v", rejected, tc.err != "")
			}
		})
	}
}

// TestMergeUniqueIndexPlanned checks plan sees the unique index a pending
// index migration creates before the merge.
func TestMergeUniqueIndexPlanned(t *testing.T) {
	cases := []struct {
		name            string
		index           string
		sourceCollation *options.Collation
		err             string
		// warn is part of a warning logged.
		warn string
	}{
		{name: "simple", index: "2026-11-02-001-items-sku-unique"},
		{name: "collation differs", index: "2026-11-02-002-items-sku-unique-ci", err: `index sku_1_ci has the collation {"locale": "en", "strength": 2}`},
		// The pending index's collation lists only what the file sets; the
		// source's has every default: plan can't be sure.
		{name: "collation can't be compared", index: "2026-11-02-002-items-sku-unique-ci", sourceCollation: &options.Collation{Locale: "en", Strength: 2},
			warn: "the unique index for $merge has a collation plan can't compare with the pipeline's: apply may fail (error 51183)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			if tc.sourceCollation != nil {
				if err := db.CreateCollection(ctx, "sources", options.CreateCollection().SetCollation(tc.sourceCollation)); err != nil {
					t.Fatal(err)
				}
			}
			insert(t, ctx, db.Collection("sources"), bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}})
			insert(t, ctx, db.Collection("items"), bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "a"}})
			dir := fixtureDir(t, "merge", priceBySKU)
			raw, err := os.ReadFile(filepath.Join("testdata", "merge-index", tc.index+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.index+".json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			o := Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			_, err = execute(t, ctx, db, load(t, dir), o, Config{Mode: ModePlan})
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("plan: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("plan: %v, want an error containing %q", err, tc.err)
			}
			if tc.warn != "" && !strings.Contains(logs.String(), tc.warn) {
				t.Errorf("no warning %q in:\n%s", tc.warn, logs.String())
			}
		})
	}
}

func TestSameCollationSpec(t *testing.T) {
	full := `{"alternate": "non-ignorable", "locale": "en", "strength": 2, "version": "57.1"}`
	cases := []struct {
		a, b       string
		same, sure bool
	}{
		{a: "", b: "", same: true, sure: true},
		{a: full, b: full, same: true, sure: true},
		{a: "", b: full, same: false, sure: true},
		{a: full, b: "", same: false, sure: true},
		// A collation from a file lists only what it sets.
		{a: `{"locale": "en", "strength": 2}`, b: full, same: true, sure: false},
		{a: `{"locale": "en", "strength": 1}`, b: full, same: false, sure: true},
		{a: `{"locale": "fr"}`, b: full, same: false, sure: true},
	}
	for _, tc := range cases {
		t.Run(tc.a+" "+tc.b, func(t *testing.T) {
			if same, sure := sameCollationSpec(tc.a, tc.b); same != tc.same || sure != tc.sure {
				t.Errorf("sameCollationSpec = %v, %v; want %v, %v", same, sure, tc.same, tc.sure)
			}
		})
	}
}

func TestMergesThroughCollation(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{version: "7.0.0", want: false},
		{version: "7.0.28", want: false},
		{version: "8.0.34", want: false},
		{version: "8.2.12", want: false},
		{version: "8.3.0", want: true},
		{version: "8.3.13", want: true},
		{version: "9.0.4", want: true},
		{version: "10.0.0-rc0", want: true},
		{version: "unknown", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			if got := mergesThroughCollation(tc.version); got != tc.want {
				t.Errorf("mergesThroughCollation(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

// TestMergeCollationOnlyMatches checks plan counts the output documents that
// match a target only through a case-insensitive unique index, warns about
// them according to the server's version, and that apply does with them
// what the warning says: MongoDB 8.3 and later merge them, 7.0 to 8.2 discard them.
func TestMergeCollationOnlyMatches(t *testing.T) {
	ci := &options.Collation{Locale: "en", Strength: 2}
	cases := []struct {
		name string
		// collation is the default collation of sources and of the unique
		// index on items.sku; nil for the simple collation.
		collation *options.Collation
		// count is the row's collationOnlyMatchCount, or -1 when absent.
		count int64
	}{
		{name: "case-insensitive", collation: ci, count: 1},
		{name: "simple", count: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			var info struct {
				Version string `bson:"version"`
			}
			if err := db.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
				t.Fatal(err)
			}
			index := options.Index().SetUnique(true)
			if tc.collation != nil {
				if err := db.CreateCollection(ctx, "sources", options.CreateCollection().SetCollation(tc.collation)); err != nil {
					t.Fatal(err)
				}
				index.SetCollation(tc.collation)
			}
			items := db.Collection("items")
			if _, err := items.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "sku", Value: 1}}, Options: index}); err != nil {
				t.Fatal(err)
			}
			insert(t, ctx, items,
				bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "abc"}},
				bson.D{{Key: "_id", Value: 2}, {Key: "sku", Value: "XYZ"}})
			insert(t, ctx, db.Collection("sources"),
				// Matches item 1 only through the collation.
				bson.D{{Key: "sku", Value: "ABC"}, {Key: "price", Value: int32(1)}},
				// Matches item 2 exactly.
				bson.D{{Key: "sku", Value: "XYZ"}, {Key: "price", Value: int32(2)}},
				// Matches nothing.
				bson.D{{Key: "sku", Value: "none"}, {Key: "price", Value: int32(3)}})
			ms := load(t, fixtureDir(t, "merge", priceBySKU))

			for _, mode := range []Mode{ModePlan, ModeApply} {
				var logs bytes.Buffer
				o := Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
				rep, err := execute(t, ctx, db, ms, o, Config{Mode: mode})
				if err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
				row := rep.Migrations[0]
				if got := counts(row.MergeCounts); !strings.HasPrefix(got, "before=1 ") || !strings.Contains(got, " unmatched=2 ") {
					t.Errorf("%s: counts %s, want 1 diff and 2 unmatched", mode, got)
				}
				got := int64(-1)
				if row.CollationOnlyMatchCount != nil {
					got = *row.CollationOnlyMatchCount
				}
				if got != tc.count {
					t.Errorf("%s: collationOnlyMatchCount %d, want %d", mode, got, tc.count)
				}
				warned := strings.Contains(logs.String(), "only through the collation of the unique index")
				if warned != (tc.count > 0) {
					t.Errorf("%s: warned %v, want %v:\n%s", mode, warned, tc.count > 0, logs.String())
				}
				effect := "discards them"
				if mergesThroughCollation(info.Version) {
					effect = "merges them, rewriting the target's on fields"
				}
				if warned && !strings.Contains(logs.String(), "MongoDB "+info.Version+" "+effect) {
					t.Errorf("%s: the warning doesn't say MongoDB %s %s:\n%s", mode, info.Version, effect, logs.String())
				}
			}

			// What apply did to item 1.
			var item1 struct {
				SKU   string `bson:"sku"`
				Price *int32 `bson:"price"`
			}
			if err := items.FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&item1); err != nil {
				t.Fatal(err)
			}
			merged := tc.count > 0 && mergesThroughCollation(info.Version)
			if got := item1.Price != nil; got != merged || (merged && item1.SKU != "ABC") {
				t.Errorf("MongoDB %s: item 1 is %+v; merged through the collation: %v", info.Version, item1, merged)
			}
		})
	}
}

// TestMergeCollationWarnings checks plan and apply warn once per merge step
// when the collection the pipeline reads has a non-simple default
// collation, listing the index's limits in the same warning when the unique
// index on the on fields has it too, and stay silent otherwise.
func TestMergeCollationWarnings(t *testing.T) {
	const (
		sourceWarning = "the collection the pipeline reads has a non-simple default collation"
		indexWarning  = "the unique index on the on fields has it too"
	)
	ci := &options.Collation{Locale: "en", Strength: 2}
	cases := []struct {
		name string
		id   string
		// source is the collection the steps read; target the one they
		// merge into.
		source, target string
		// sourceCollation is the default collation of source, nil for none;
		// noSource leaves it uncreated.
		sourceCollation *options.Collation
		noSource        bool
		// indexCollation is the collation of the unique index on
		// target.sku, nil for the simple one; with on _id there is none.
		indexCollation *options.Collation
		// sourceWarnings and indexWarnings are the warnings each run logs.
		sourceWarnings, indexWarnings int
	}{
		{name: "case-insensitive source and index", id: priceCurrency, source: "sources", target: "items",
			sourceCollation: ci, indexCollation: ci, sourceWarnings: 2, indexWarnings: 2},
		{name: "simple source and index", id: priceCurrency, source: "sources", target: "items"},
		{name: "explicitly simple source", id: priceCurrency, source: "sources", target: "items",
			sourceCollation: &options.Collation{Locale: "simple"}},
		{name: "missing source", id: priceCurrency, source: "sources", target: "items", noSource: true},
		{name: "case-insensitive source, on _id", id: copyLabel, source: "items", target: "items",
			sourceCollation: ci, sourceWarnings: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			if tc.sourceCollation != nil {
				if err := db.CreateCollection(ctx, tc.source, options.CreateCollection().SetCollation(tc.sourceCollation)); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.noSource && tc.source != tc.target {
				insert(t, ctx, db.Collection(tc.source), bson.D{{Key: "sku", Value: "a"}, {Key: "price", Value: int32(1)}, {Key: "currency", Value: "EUR"}})
			}
			insert(t, ctx, db.Collection(tc.target), bson.D{{Key: "_id", Value: 1}, {Key: "sku", Value: "a"}, {Key: "name", Value: "A"}})
			if tc.id != copyLabel {
				index := options.Index().SetUnique(true)
				if tc.indexCollation != nil {
					index.SetCollation(tc.indexCollation)
				}
				if _, err := db.Collection(tc.target).Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "sku", Value: 1}}, Options: index}); err != nil {
					t.Fatal(err)
				}
			}
			ms := load(t, fixtureDir(t, "merge", tc.id))
			for _, mode := range []Mode{ModePlan, ModeApply} {
				var logs bytes.Buffer
				o := Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
				if _, err := execute(t, ctx, db, ms, o, Config{Mode: mode}); err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
				if got := strings.Count(logs.String(), sourceWarning); got != tc.sourceWarnings {
					t.Errorf("%s: %d source collation warnings, want %d:\n%s", mode, got, tc.sourceWarnings, logs.String())
				}
				if got := strings.Count(logs.String(), indexWarning); got != tc.indexWarnings {
					t.Errorf("%s: %d index collation warnings, want %d:\n%s", mode, got, tc.indexWarnings, logs.String())
				}
			}
		})
	}
}
