package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

func catalogBSON(t *testing.T, document string) bson.Raw {
	t.Helper()
	var raw bson.Raw
	if err := bson.UnmarshalExtJSON([]byte(document), false, &raw); err != nil {
		t.Fatalf("parse %s: %v", document, err)
	}
	return raw
}

// Construct steps directly so runtime guards are exercised even for streams
// which file validation also rejects.
func catalogStep(t *testing.T, source, target, action string, stages ...string) migration.Step {
	t.Helper()
	s := migration.Step{Operation: "aggregate", Collection: source}
	for _, stage := range stages {
		s.Pipeline = append(s.Pipeline, catalogBSON(t, stage))
	}
	merge := fmt.Sprintf(`{"$merge":{"into":%q,"on":"_id","whenMatched":"merge","whenNotMatched":%q}}`, target, action)
	s.Pipeline = append(s.Pipeline, catalogBSON(t, merge))
	return s
}

func runCatalog(t *testing.T, ctx context.Context, db *mongo.Database, mode Mode, logger *slog.Logger, failedStep int, steps ...migration.Step) Row {
	t.Helper()
	m := migration.Migration{ID: "2026-11-25-001-catalog", Kind: migration.KindMerge, Steps: steps}
	report, err := execute(t, ctx, db, []migration.Migration{m}, Options{Logger: logger}, Config{Mode: mode})
	if report == nil || len(report.Migrations) != 1 {
		t.Fatalf("%s: report %+v, error %v", mode, report, err)
	}
	row := report.Migrations[0]
	if failedStep < 0 {
		if err != nil || row.Status != RowApplied {
			t.Fatalf("%s: row %s, error %v", mode, row.Status, err)
		}
	} else {
		var failure *StepFailure
		if !errors.As(err, &failure) || failure.Step != failedStep || row.Status != RowErrored {
			t.Fatalf("%s: row %s, error %v, want failure on step %d", mode, row.Status, err, failedStep)
		}
	}
	if mode == ModeApply {
		entries := changelog(t, ctx, db)
		if len(entries) == 0 {
			t.Fatalf("%s: changelog is empty", mode)
		}
		entry := entries[len(entries)-1]
		if failedStep < 0 {
			if entry.Status != StatusFinished {
				t.Fatalf("entry %s, want Finished", entry.Status)
			}
		} else if entry.Status != StatusErrored || len(entry.Errors) != 1 || entry.Errors[0].Step == nil || *entry.Errors[0].Step != failedStep {
			t.Fatalf("entry %s, errors %+v, want Errored on step %d", entry.Status, entry.Errors, failedStep)
		}
	}
	return row
}

func catalogCount(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		if want >= 0 {
			t.Errorf("%s absent, want %d", name, want)
		}
	} else if *got != want || want < 0 {
		t.Errorf("%s = %d, want %d (-1 means absent)", name, *got, want)
	}
}

func checkCatalogStep(t *testing.T, row Row, step int, target string, exists bool, pendingBefore, diffBefore, pendingAfter, diffAfter int64) {
	t.Helper()
	for _, s := range row.InsertSteps {
		if s.Step != step {
			continue
		}
		if s.Into != target || s.TargetExistsBefore == nil || *s.TargetExistsBefore != exists {
			t.Errorf("step %d target %+v, want %s with targetExistsBefore %t", step, s, target, exists)
		}
		catalogCount(t, "pendingBefore", s.PendingBefore, pendingBefore)
		catalogCount(t, "diffCountBefore", s.DiffCountBefore, diffBefore)
		catalogCount(t, "pendingAfter", s.PendingAfter, pendingAfter)
		catalogCount(t, "diffCountAfter", s.DiffCountAfter, diffAfter)
		return
	}
	t.Fatalf("step %d missing in %+v", step, row.InsertSteps)
}

func catalogExists(t *testing.T, ctx context.Context, db *mongo.Database) bool {
	t.Helper()
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: "catalog"}})
	if err != nil {
		t.Fatal(err)
	}
	return len(names) != 0
}

func catalogCommand(t *testing.T, ctx context.Context, db *mongo.Database, command string) {
	t.Helper()
	if err := db.RunCommand(ctx, catalogBSON(t, command)).Err(); err != nil {
		t.Fatalf("%s: %v", command, err)
	}
}

func TestCatalogInsertCollectionPreflight(t *testing.T) {
	const legacy = `{"create":"legacy"}`
	const catalog = `{"create":"catalog"}`
	const oldRow = `{"insert":"catalog","documents":[{"_id":"existing","local":true}]}`
	const sourceRow = `{"insert":"legacy","documents":[{"_id":"source"}]}`
	const alias = `{"create":"alias","viewOn":"catalog","pipeline":[]}`
	const twoHops = `{"create":"outer","viewOn":"alias","pipeline":[]}`
	cases := []struct {
		name, source string
		setup        []string
		stream       []string
	}{
		{"missing source", "legacy", nil, nil},
		{"same source and target", "catalog", []string{catalog, oldRow}, nil},
		{"primary source through two views", "outer", []string{catalog, oldRow, alias, twoHops}, nil},
		{"source has non-simple collation", "legacy", []string{`{"create":"legacy","collation":{"locale":"en","strength":2}}`, sourceRow}, nil},
		{"target has non-simple collation", "legacy", []string{legacy, sourceRow, `{"create":"catalog","collation":{"locale":"en","strength":2}}`, oldRow}, nil},
		{"target is a full capped collection", "legacy", []string{legacy, sourceRow, `{"create":"catalog","capped":true,"size":4096,"max":1}`, oldRow}, nil},
		{"target is a view", "legacy", []string{legacy, sourceRow, `{"create":"catalog","viewOn":"legacy","pipeline":[]}`}, nil},
		{"target is time series", "legacy", []string{legacy, sourceRow, `{"create":"catalog","timeseries":{"timeField":"at"}}`}, nil},
		{"union with target", "legacy", []string{legacy, sourceRow, catalog, oldRow}, []string{`{"$unionWith":"catalog"}`}},
		{"union with a view", "legacy", []string{legacy, sourceRow, catalog, oldRow, alias}, []string{`{"$unionWith":{"coll":"alias","pipeline":[]}}`}},
		{"nested union with missing source", "legacy", []string{legacy, sourceRow, `{"create":"other"}`}, []string{`{"$unionWith":{"coll":"other","pipeline":[{"$unionWith":"typo"}]}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, db := testCtx(t), testDB(t)
			for _, command := range tc.setup {
				catalogCommand(t, ctx, db, command)
			}
			// Bound self-reading cases even if a guard regresses.
			stages := append(slices.Clone(tc.stream), `{"$limit":1}`, `{"$project":{"_id":{"$literal":"candidate"}}}`)
			step := catalogStep(t, tc.source, "catalog", "insert", stages...)
			exists := catalogExists(t, ctx, db)
			original := dump(t, ctx, db.Collection("catalog"))
			for _, mode := range []Mode{ModePlan, ModeApply} {
				row := runCatalog(t, ctx, db, mode, nil, 0, step)
				if row.DiffCountBefore != nil || len(row.InsertSteps) != 1 || row.InsertSteps[0].Into != "catalog" || row.InsertSteps[0].PendingBefore != nil {
					t.Errorf("%s compared despite invalid metadata: %+v", mode, row)
				}
				if catalogExists(t, ctx, db) != exists || !slices.EqualFunc(original, dump(t, ctx, db.Collection("catalog")), sameCatalogDocument) {
					t.Fatalf("%s changed the catalog", mode)
				}
			}
			if tc.name == "missing source" {
				catalogCommand(t, ctx, db, legacy)
				plan := runCatalog(t, ctx, db, ModePlan, nil, -1, step)
				checkCatalogStep(t, plan, 0, "catalog", false, 0, 0, -1, -1)
			}
		})
	}
}

func TestCatalogInsertTargets(t *testing.T) {
	for _, tc := range []struct {
		name                string
		createTarget, empty bool
	}{
		{"empty source and absent target", false, true},
		{"source rows and absent target", false, false},
		{"source rows and existing empty target", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, db := testCtx(t), testDB(t)
			catalogCommand(t, ctx, db, `{"create":"legacy"}`)
			catalogCommand(t, ctx, db, `{"create":"other"}`)
			if tc.createTarget {
				catalogCommand(t, ctx, db, `{"create":"catalog","validator":{"label":{"$type":"string"}}}`)
				catalogCommand(t, ctx, db, `{"createIndexes":"catalog","indexes":[{"key":{"label":1},"name":"by_label"}]}`)
			}
			// Keep a legal nested DBRef, literal dotted key and nested regex
			// intact across comparison, grouping and the native write.
			id := catalogBSON(t, `{"region":"us","a.b":{"$ref":"items","$id":{"safe":1}},"r":{"$regularExpression":{"pattern":"x","options":""}}}`)
			var want int64
			if !tc.empty {
				catalogCommand(t, ctx, db, `{"insert":"legacy","documents":[{"_id":"X","details":{"label":"upper"}},{"_id":"x","details":{"label":"lower"}}]}`)
				insert(t, ctx, db.Collection("other"), bson.D{{Key: "_id", Value: id}, {Key: "details", Value: bson.D{{Key: "label", Value: "document"}}}})
				want = 3
			}
			step := catalogStep(t, "legacy", "catalog", "insert", `{"$unionWith":{"coll":"other","pipeline":[]}}`, `{"$project":{"label":"$details.label"}}`)
			for _, mode := range []Mode{ModePlan, ModeApply} {
				row := runCatalog(t, ctx, db, mode, nil, -1, step)
				after := int64(-1)
				if mode == ModeApply {
					after = 0
				}
				checkCatalogStep(t, row, 0, "catalog", tc.createTarget, want, 0, after, after)
				catalogCount(t, "unmatchedCount", row.UnmatchedCount, -1)
				if mode == ModePlan && (catalogExists(t, ctx, db) != tc.createTarget || len(dump(t, ctx, db.Collection("catalog"))) != 0) {
					t.Fatal("plan changed the catalog")
				}
			}
			col := db.Collection("catalog")
			if got := countDocs(t, ctx, col, bson.D{}); got != want {
				t.Fatalf("%d target documents, want %d", got, want)
			}
			if !tc.empty {
				for _, doc := range []string{`{"_id":"X","label":"upper"}`, `{"_id":"x","label":"lower"}`} {
					if countDocs(t, ctx, col, catalogBSON(t, doc)) != 1 {
						t.Error("missing target document", doc)
					}
				}
				got, err := col.FindOne(ctx, bson.D{{Key: "label", Value: "document"}}).Raw()
				storedID, _ := got.Lookup("_id").DocumentOK()
				if err != nil || !bytes.Equal(storedID, id) {
					t.Fatalf("document id after merge: %s, error %v", got, err)
				}
			}
			if tc.createTarget {
				specs, err := col.Indexes().ListSpecifications(ctx)
				if err != nil || len(specs) != 2 {
					t.Fatalf("target indexes %v, error %v", specs, err)
				}
				_, err = col.InsertOne(ctx, bson.D{{Key: "_id", Value: "bad"}})
				var server mongo.ServerError
				if !errors.As(err, &server) || !server.HasErrorCode(121) {
					t.Fatalf("validator after apply: %v, want document validation failure", err)
				}
			}
		})
	}
}

func TestCatalogInsertValidScopes(t *testing.T) {
	ctx, db := testCtx(t), testDB(t)
	ids := []bson.RawValue{
		// A code-with-scope scalar is legal as the whole _id. MongoDB only
		// applies the embedded name rules to object-valued IDs.
		catalogBSON(t, `{"_id":{"$code":"example","$scope":{"$opaque":1}}}`).Lookup("_id"),
		catalogBSON(t, `{"_id":{"v":[{"$code":"example","$scope":{"ref":{"$ref":"items","$id":{"$opaque":1},"$db":"db"},"a.b":true}}]}}`).Lookup("_id"),
	}
	for _, id := range ids {
		insert(t, ctx, db.Collection("legacy"), bson.D{{Key: "_id", Value: id}})
	}
	step := catalogStep(t, "legacy", "catalog", "insert")
	row := runCatalog(t, ctx, db, ModeApply, nil, -1, step)
	checkCatalogStep(t, row, 0, "catalog", false, 2, 0, 0, 0)
	for _, id := range ids {
		if n := countDocs(t, ctx, db.Collection("catalog"), bson.D{{Key: "_id", Value: id}}); n != 1 {
			t.Errorf("stored %d documents with %s _id, want 1", n, id.Type)
		}
	}
}

func TestCatalogInsertOutputPreflight(t *testing.T) {
	const scoped = `{"_id":{"v":{"$code":"example","$scope":{"label":"ok"}}}}`
	const invalidScoped = `{"_id":{"v":{"$code":"example","$scope":{"$bad":"value"}}}}`
	cases := []struct {
		name               string
		outputs            []string
		target, diagnostic string
	}{
		{"missing id", []string{`{"label":"catalog"}`}, "", "nullOnCount is 1"},
		{"null id", []string{`{"_id":null}`}, "", "nullOnCount is 1"},
		{"array id", []string{`{"_id":[1]}`}, "", "arrayOnCount is 1"},
		{"regex id", []string{`{"_id":{"$regularExpression":{"pattern":"x","options":""}}}`}, "", "invalidIDCount is 1"},
		{"undefined id", []string{`{"_id":{"$undefined":true}}`}, "", "invalidIDCount is 1"},
		{"document id with nested dollar name", []string{`{"_id":{"outer":[{"$bad":1}]}}`}, "", "invalidIDCount is 1"},
		{"document id with invalid DBRef", []string{`{"_id":{"nested":{"$ref":"items","$id":1,"$db":2}}}`}, "", "invalidIDCount is 1"},
		{"DBRef with invalid id payload", []string{`{"_id":{"r":{"$ref":"items","$id":[{"$bad":1}]}}}`}, "", "invalidIDCount is 1"},
		{"scope alongside invalid DBRef payload", []string{`{"_id":{"code":{"$code":"example","$scope":{}},"r":{"$ref":"items","$id":{"$bad":1}}}}`}, "", "invalidIDCount is 1"},
		{"nested scope in a document id", []string{`{"_id":{"value":[{"$code":"example","$scope":{"child":{"$code":"nested","$scope":{"$bad":1}}}}]}}`}, "", "invalidIDCount is 1"},
		{"empty name on equal row", []string{`{"_id":1,"":7}`}, `{"_id":1,"":7}`, "invalidFieldNameCount is 1"},
		{"empty name on missing row", []string{`{"_id":2,"":7}`}, "", "invalidFieldNameCount is 1"},
		{"dotted name on equal row", []string{`{"_id":1,"a.b":"catalog"}`}, `{"_id":1,"a.b":"catalog","a":{"b":"local"}}`, "invalidFieldNameCount is 1"},
		{"dotted name on missing row", []string{`{"_id":2,"a.b":"catalog"}`}, `{"_id":1,"a":{"b":"local"}}`, "invalidFieldNameCount is 1"},
		{"dollar name on equal row", []string{`{"_id":1,"$price":7}`}, `{"_id":1,"$price":7,"local":true}`, "invalidFieldNameCount is 1"},
		{"dollar name on missing row", []string{`{"_id":2,"$price":7}`}, "", "invalidFieldNameCount is 1"},
		{"matched duplicate", []string{`{"_id":1,"label":"same"}`, `{"_id":1,"label":"same"}`}, `{"_id":1,"label":"same"}`, "duplicateOnCount is 2"},
		{"missing duplicate", []string{`{"_id":1,"label":"same"}`, `{"_id":{"$numberLong":"1"},"label":"same"}`}, "", "duplicateMissingIDCount is 2"},
		{"matched scoped duplicate", []string{scoped, scoped}, scoped, "duplicateOnCount is 2"},
		{"missing scoped duplicate", []string{scoped, scoped}, "", "duplicateMissingIDCount is 2"},
		{"missing invalid scoped duplicate", []string{invalidScoped, invalidScoped}, "", "invalidIDCount is 2"},
		{"decimal precision", []string{`{"_id":{"$numberDecimal":"1.0"}}`}, `{"_id":{"$numberDecimal":"1.00"}}`, "idMismatchCount is 1"},
		{"nested decimal precision", []string{`{"_id":{"v":[{"$numberDecimal":"1.0"}]}}`}, `{"_id":{"v":[{"$numberDecimal":"1.00"}]}}`, "idMismatchCount is 1"},
		{"signed zero", []string{`{"_id":{"$numberDouble":"-0.0"}}`}, `{"_id":{"$numberDouble":"0.0"}}`, "idMismatchCount is 1"},
		{"forbidden matched type change", []string{`{"_id":1,"price":"1"}`}, `{"_id":1,"price":1}`, "typeChangedCount is 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, db := testCtx(t), testDB(t)
			// A valid candidate before each invalid row exposes partial writes.
			source := db.Collection("legacy")
			insert(t, ctx, source, bson.D{{Key: "_id", Value: 0}, {Key: "out", Value: catalogBSON(t, `{"_id":"safe"}`)}})
			for i, output := range tc.outputs {
				insert(t, ctx, source, bson.D{{Key: "_id", Value: i + 1}, {Key: "out", Value: catalogBSON(t, output)}})
			}
			target := db.Collection("catalog")
			insert(t, ctx, target, bson.D{{Key: "_id", Value: "keeper"}})
			if tc.target != "" {
				insert(t, ctx, target, rawDoc(t, catalogBSON(t, tc.target)))
			}
			original := dump(t, ctx, target)
			step := catalogStep(t, "legacy", "catalog", "insert", `{"$sort":{"_id":1}}`, `{"$replaceWith":"$out"}`)
			for _, mode := range []Mode{ModePlan, ModeApply} {
				row := runCatalog(t, ctx, db, mode, nil, 0, step)
				if len(row.InsertSteps) != 1 || row.InsertSteps[0].PendingBefore == nil || row.InsertSteps[0].PendingAfter != nil || row.DiffCountAfter != nil {
					t.Errorf("%s expected a completed preflight only: %+v", mode, row)
				}
				catalogCount(t, "unmatchedCount", row.UnmatchedCount, -1)
				if !strings.Contains(row.Error, tc.diagnostic) {
					t.Errorf("%s error %q, want %q", mode, row.Error, tc.diagnostic)
				}
				switch tc.name {
				case "matched duplicate", "matched scoped duplicate":
					catalogCount(t, "duplicateOnCount", row.DuplicateOnCount, 2)
				case "missing duplicate", "missing scoped duplicate", "missing invalid scoped duplicate":
					catalogCount(t, "duplicateOnCount", row.DuplicateOnCount, 0)
				case "decimal precision", "nested decimal precision", "signed zero":
					catalogCount(t, "idMismatchCount", row.IDMismatchCount, 1)
					catalogCount(t, "diffCountBefore", row.DiffCountBefore, 1)
					if len(row.Samples) != 1 || row.Samples[0].Fields[0].Field != "_id" {
						t.Errorf("missing ID mismatch sample: %+v", row.Samples)
					}
				case "forbidden matched type change":
					catalogCount(t, "typeChangedCount", row.TypeChangedCount, 1)
				}
				if !slices.EqualFunc(original, dump(t, ctx, target), sameCatalogDocument) {
					t.Fatalf("%s changed the target: %s", mode, dump(t, ctx, target))
				}
			}
		})
	}
}

// A view supplies a matching target ID to the read-only comparator even on
// servers that reject storing that ID. The native-write path uses ordinary
// targets in TestCatalogInsertOutputPreflight.
func TestCatalogComparisonMatchedInvalidScopedDuplicate(t *testing.T) {
	ctx := testCtx(t)
	run := startRun(t, ctx, Options{})
	defer run.Close()
	output := catalogBSON(t, `{"_id":{"v":{"$code":"example","$scope":{"$bad":"value"}}}}`)
	for i := 1; i <= 2; i++ {
		insert(t, ctx, run.db.Collection("legacy"), bson.D{{Key: "_id", Value: i}, {Key: "out", Value: output}})
	}
	catalogCommand(t, ctx, run.db, `{"create":"catalog","viewOn":"legacy","pipeline":[{"$match":{"_id":1}},{"$replaceWith":"$out"}]}`)

	c, err := run.compareMerge("legacy", []bson.Raw{catalogBSON(t, `{"$replaceWith":"$out"}`)},
		mergeTarget{Into: "catalog", On: []string{"_id"}, Insert: true}, run.log)
	if err != nil {
		t.Fatal(err)
	}
	if c.InvalidID != 2 || c.Unmatched != 0 || c.DuplicateOn != 0 || c.DuplicateMissing != 0 {
		t.Errorf("comparison %+v, want two invalid matches and no duplicates", c)
	}
}

func sameCatalogDocument(a, b bson.Raw) bool { return bytes.Equal(a, b) }

func TestCatalogInsertFileVerification(t *testing.T) {
	for _, tc := range []struct {
		name, firstMode, secondMode, secondLabel string
	}{
		{"discard invalidates insert", "insert", "discard", "B"},
		{"insert invalidates discard", "discard", "insert", "B"},
		{"mixed file converges", "discard", "insert", "A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, db := testCtx(t), testDB(t)
			insert(t, ctx, db.Collection("catalog"), bson.D{{Key: "_id", Value: 1}, {Key: "label", Value: "old"}})
			for _, s := range []struct{ source, label, mode string }{
				{"first", "A", tc.firstMode}, {"second", tc.secondLabel, tc.secondMode},
			} {
				insert(t, ctx, db.Collection(s.source), bson.D{{Key: "_id", Value: 1}, {Key: "label", Value: s.label}})
				if s.mode == "discard" {
					insert(t, ctx, db.Collection(s.source), bson.D{{Key: "_id", Value: "orphan"}})
				}
			}
			first := catalogStep(t, "first", "catalog", tc.firstMode)
			second := catalogStep(t, "second", "catalog", tc.secondMode)
			plan := runCatalog(t, ctx, db, ModePlan, nil, -1, first, second)
			catalogCount(t, "plan diffCountBefore", plan.DiffCountBefore, 2)
			catalogCount(t, "plan unmatchedCount", plan.UnmatchedCount, 1)

			failed, before, after := 0, int64(2), int64(1)
			if tc.secondLabel == "A" {
				failed, before, after = -1, 1, 0
			}
			row := runCatalog(t, ctx, db, ModeApply, nil, failed, first, second)
			catalogCount(t, "diffCountBefore", row.DiffCountBefore, before)
			catalogCount(t, "diffCountAfter", row.DiffCountAfter, after)
			catalogCount(t, "unmatchedCount", row.UnmatchedCount, 1)
			if tc.firstMode == "insert" {
				checkCatalogStep(t, row, 0, "catalog", true, 0, 1, 0, after)
			} else {
				checkCatalogStep(t, row, 1, "catalog", true, 0, before-1, 0, 0)
			}
			if len(row.InsertSteps) != 1 {
				t.Fatalf("insert observations %+v", row.InsertSteps)
			}
			col := db.Collection("catalog")
			if countDocs(t, ctx, col, bson.D{}) != 1 || countDocs(t, ctx, col, bson.D{{Key: "_id", Value: 1}, {Key: "label", Value: tc.secondLabel}}) != 1 {
				t.Fatalf("catalog after writes: %s", dump(t, ctx, col))
			}
		})
	}
}

// The discard writes the marker that makes its lookup emit two identical rows.
// Both rows now agree with the target, so diffCountAfter alone cannot catch them.
func TestCatalogFinalDiscardDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withInsert bool
		failedStep int
	}{
		{"mixed file", true, 1},
		{"discard-only file", false, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, db := testCtx(t), testDB(t)
			insert(t, ctx, db.Collection("legacy"), bson.D{{Key: "_id", Value: 1}})
			insert(t, ctx, db.Collection("catalog"), bson.D{{Key: "_id", Value: 1}, {Key: "marked", Value: false}})
			var steps []migration.Step
			if tc.withInsert {
				steps = append(steps, catalogStep(t, "legacy", "other", "insert"))
			}
			steps = append(steps, catalogStep(t, "legacy", "catalog", "discard",
				`{"$lookup":{"from":"catalog","localField":"_id","foreignField":"_id","as":"existing"}}`,
				`{"$set":{"copies":{"$cond":[{"$arrayElemAt":["$existing.marked",0]},[1,2],[1]]}}}`,
				`{"$unwind":"$copies"}`,
				`{"$project":{"_id":1,"marked":{"$literal":true}}}`,
			))
			row := runCatalog(t, ctx, db, ModeApply, nil, tc.failedStep, steps...)
			catalogCount(t, "diffCountAfter", row.DiffCountAfter, 0)
			if tc.withInsert && !strings.Contains(row.Error, "duplicateOnCount is 2") {
				t.Errorf("error %q, want two duplicate matches", row.Error)
			}
			if n := countDocs(t, ctx, db.Collection("catalog"), bson.D{{Key: "marked", Value: true}}); n != 1 {
				t.Errorf("discard wrote %d markers, want 1", n)
			}
		})
	}
}

func TestCatalogInsertPerTargetCounts(t *testing.T) {
	ctx, db := testCtx(t), testDB(t)
	var source, old []bson.D
	for i := range 20 {
		source = append(source, bson.D{{Key: "_id", Value: i}, {Key: "label", Value: "new"}})
		old = append(old, bson.D{{Key: "_id", Value: i}, {Key: "label", Value: "old"}})
	}
	insert(t, ctx, db.Collection("legacy"), source...)
	insert(t, ctx, db.Collection("first"), old...)
	insert(t, ctx, db.Collection("second"), source...)
	first := catalogStep(t, "legacy", "first", "insert")
	second := catalogStep(t, "legacy", "second", "insert")
	plan := runCatalog(t, ctx, db, ModePlan, nil, -1, first, second)
	checkCatalogStep(t, plan, 0, "first", true, 0, 20, -1, -1)
	checkCatalogStep(t, plan, 1, "second", true, 0, 0, -1, -1)
	catalogCount(t, "plan diffCountBefore", plan.DiffCountBefore, 20)

	// The second target changes after plan. Its validator now rejects restoring
	// the new label, so only the first step has a completed after-comparison.
	catalogCommand(t, ctx, db, `{"update":"second","updates":[{"q":{"_id":0},"u":{"$set":{"label":"old"}}}]}`)
	catalogCommand(t, ctx, db, `{"collMod":"second","validator":{"label":"old"}}`)
	row := runCatalog(t, ctx, db, ModeApply, nil, 1, first, second)
	checkCatalogStep(t, row, 0, "first", true, 0, 20, 0, 0)
	checkCatalogStep(t, row, 1, "second", true, 0, 1, -1, -1)
	catalogCount(t, "diffCountBefore", row.DiffCountBefore, 21)
	catalogCount(t, "diffCountAfter", row.DiffCountAfter, 0)
	if n := countDocs(t, ctx, db.Collection("first"), bson.D{{Key: "label", Value: "new"}}); n != 20 {
		t.Errorf("first target: %d labels changed, want 20", n)
	}
}

// The log handler runs synchronously between the native aggregate and its
// verification. This makes the deletion deterministic without a timed race.
type onCatalogWrite func()

func (f onCatalogWrite) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"msg":"merge done"`)) {
		f()
	}
	return len(p), nil
}

func TestCatalogInsertPostWriteVerification(t *testing.T) {
	for _, name := range []string{"lookup changes output", "target row disappears"} {
		t.Run(name, func(t *testing.T) {
			ctx, db := testCtx(t), testDB(t)
			target := db.Collection("catalog")
			insert(t, ctx, db.Collection("legacy"), bson.D{{Key: "_id", Value: "first"}})
			var stages []string
			var logger *slog.Logger
			if name == "lookup changes output" {
				stages = []string{
					`{"$lookup":{"from":"catalog","localField":"_id","foreignField":"_id","as":"existing"}}`,
					`{"$project":{"_id":{"$cond":[{"$eq":[{"$size":"$existing"},0]},"first","next"]}}}`,
				}
			} else {
				logger = slog.New(slog.NewJSONHandler(onCatalogWrite(func() {
					r, err := target.DeleteOne(ctx, bson.D{{Key: "_id", Value: "first"}})
					if err != nil || r.DeletedCount != 1 {
						t.Errorf("delete after write: result %v, error %v", r, err)
					}
				}), nil))
			}
			step := catalogStep(t, "legacy", "catalog", "insert", stages...)
			row := runCatalog(t, ctx, db, ModeApply, logger, 0, step)
			checkCatalogStep(t, row, 0, "catalog", false, 1, 0, 1, 0)
			first := int64(0)
			if name == "lookup changes output" {
				first = 1
			}
			if countDocs(t, ctx, target, bson.D{{Key: "_id", Value: "first"}}) != first || countDocs(t, ctx, target, bson.D{}) != first {
				t.Fatalf("target after failed verification: %s", dump(t, ctx, target))
			}

			plan := runCatalog(t, ctx, db, ModePlan, nil, -1, step)
			checkCatalogStep(t, plan, 0, "catalog", true, 1, 0, -1, -1)
			retry := runCatalog(t, ctx, db, ModeApply, nil, -1, step)
			checkCatalogStep(t, retry, 0, "catalog", true, 1, 0, 0, 0)
			if got := countDocs(t, ctx, target, bson.D{}); got != first+1 {
				t.Errorf("%d target documents after retry, want %d", got, first+1)
			}
		})
	}
}

// The duplicate group spans the initial 101-document cursor batch. Samples
// must still prefer the later type changes over ordinary differences.
func TestCatalogDuplicateAcrossCursorBatches(t *testing.T) {
	ctx, db := testCtx(t), testDB(t)
	var docs []bson.D
	for i := 0; i < 110; i++ {
		docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "catalogID", Value: "A"}, {Key: "v", Value: 1}})
	}
	for i := 0; i < MaxMergeSamples; i++ {
		id := fmt.Sprintf("B%d", i)
		docs = append(docs, bson.D{{Key: "_id", Value: 110 + i}, {Key: "catalogID", Value: id}, {Key: "v", Value: "changed"}})
		insert(t, ctx, db.Collection("catalog"), bson.D{{Key: "_id", Value: id}, {Key: "v", Value: 1}})
	}
	insert(t, ctx, db.Collection("legacy"), docs...)
	insert(t, ctx, db.Collection("catalog"), bson.D{{Key: "_id", Value: "A"}, {Key: "v", Value: 0}})
	step := catalogStep(t, "legacy", "catalog", "insert", `{"$project":{"_id":"$catalogID","v":1}}`)
	row := runCatalog(t, ctx, db, ModePlan, nil, 0, step)
	catalogCount(t, "duplicateOnCount", row.DuplicateOnCount, 110)
	catalogCount(t, "diffCountBefore", row.DiffCountBefore, int64(len(docs)))
	if len(row.Samples) != MaxMergeSamples {
		t.Fatalf("got %d samples", len(row.Samples))
	}
	for _, sample := range row.Samples {
		if len(sample.Fields) != 1 || !sample.Fields[0].TypeChanged || sample.Fields[0].Before != nil || sample.Fields[0].After != nil {
			t.Errorf("unexpected sample %+v", sample)
		}
	}
}

func TestCatalogLargeMatchedIDs(t *testing.T) {
	for _, kind := range []string{"string", "document"} {
		t.Run(kind, func(t *testing.T) {
			ctx := testCtx(t)
			run := startRun(t, ctx, Options{})
			defer run.Close()
			var id any = strings.Repeat("x", 9<<20)
			if kind == "document" {
				id = bson.D{{Key: "large", Value: id}}
			}
			// Exercise the comparison cursor at a size that MongoDB 7 cannot
			// seed as a real _id because its oplog repeats the key.
			insert(t, ctx, run.db.Collection("legacy"), bson.D{{Key: "_id", Value: 1}, {Key: "largeID", Value: id}})
			rows := []any{catalogBSON(t, `{"$replaceWith":{"key":["$largeID"],"targetID":"$largeID","matched":true,"diff":false,"typeChanged":false,"nullOn":false,"arrayOn":false,"invalidFieldName":false,"diffs":[]}}`)}
			c, err := run.compareInsertRows("legacy", rows, mergeTarget{Into: "catalog", On: []string{"_id"}, Insert: true}, discard(), options.Aggregate())
			if err != nil {
				t.Fatal(err)
			}
			if c.Diff != 0 || c.Unmatched != 0 || c.InvalidID != 0 || c.IDMismatch != 0 {
				t.Fatalf("unexpected counts %+v", c)
			}
		})
	}
}

func TestCatalogIDMismatchSampleOrder(t *testing.T) {
	ctx := testCtx(t)
	run := startRun(t, ctx, Options{})
	run.sampleValues = true
	defer run.Close()
	source := catalogBSON(t, `{"label":"new","_id":{"$numberDecimal":"1.0"},"price":2}`)
	target := catalogBSON(t, `{"_id":{"$numberDecimal":"1.00"},"label":"old","price":1}`)
	insert(t, ctx, run.db.Collection("legacy"), bson.D{{Key: "out", Value: source}})
	insert(t, ctx, run.db.Collection("catalog"), rawDoc(t, target))
	c, err := run.compareMerge("legacy", []bson.Raw{catalogBSON(t, `{"$replaceWith":"$out"}`)},
		mergeTarget{Into: "catalog", On: []string{"_id"}, Insert: true}, discard())
	if err != nil {
		t.Fatal(err)
	}
	if c.Diff != 1 || c.IDMismatch != 1 || len(c.Samples) != 1 {
		t.Fatalf("unexpected comparison %+v", c)
	}
	fields := c.Samples[0].Fields
	if len(fields) != 3 || fields[0].Field != "label" || fields[1].Field != "_id" || fields[2].Field != "price" ||
		!strings.Contains(string(fields[1].Before), "1.00") || !strings.Contains(string(fields[1].After), "1.0") {
		t.Fatalf("wrong field order or ID representations: %+v", fields)
	}
}

// The first post-write batch contains two identical IDs. A subsequent source
// read would find one matched ID and one missing ID instead.
func TestCatalogVerificationWithConcurrentScopedIDChange(t *testing.T) {
	ctx := testCtx(t)
	writer := testDB(t)
	source := writer.Collection("legacy")
	outputA := catalogBSON(t, `{"_id":{"code":{"$code":"example","$scope":{"key":"A"}}}}`)
	insert(t, ctx, source, bson.D{{Key: "_id", Value: 1}, {Key: "out", Value: outputA}})
	var afterWrite atomic.Bool
	var changed atomic.Bool

	logger := slog.New(slog.NewJSONHandler(onCatalogWrite(func() {
		insert(t, ctx, source, bson.D{{Key: "_id", Value: 2}, {Key: "out", Value: outputA}})
		afterWrite.Store(true)
	}), nil))
	monitor := &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if !afterWrite.Load() || e.CommandName != "aggregate" {
			return
		}
		ns, _ := e.Reply.Lookup("cursor", "ns").StringValueOK()
		if ns != writer.Name()+".legacy" || !changed.CompareAndSwap(false, true) {
			return
		}
		// Change the source after the server has produced the first comparison batch.
		result, err := source.UpdateByID(ctx, 2, catalogBSON(t, `{"$set":{"out._id":"B"}}`))
		if err != nil || result.ModifiedCount != 1 {
			t.Fatalf("writer update returned %v, %v", result, err)
		}
	}}
	client, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("MIGRUNE_TEST_URI")).SetMonitor(monitor))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(ctx)

	step := catalogStep(t, "legacy", "catalog", "insert", `{"$replaceWith":"$out"}`)
	row := runCatalog(t, ctx, client.Database(writer.Name()), ModeApply, logger, 0, step)
	if !changed.Load() || !strings.Contains(row.Error, "duplicateOnCount is 2") {
		t.Fatalf("writer ran %v, verification error %q", changed.Load(), row.Error)
	}
}
