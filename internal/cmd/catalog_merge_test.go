package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

const catalogBackfill = "2026-11-25-001-backfill-catalog"

func catalogFiles(t *testing.T) (dir, digest string) {
	t.Helper()
	dir = t.TempDir()
	addMigration(t, dir, filepath.Join("testdata", "catalog"), catalogBackfill)
	return dir, filepath.Join(t.TempDir(), "plan.digest")
}

// checkCatalogCounts also checks the distinction between a measured zero and an
// absent measurement in a plan or a failed write.
func checkCatalogCounts(t *testing.T, rep runner.Report, exists bool, missing, differing int64, verified bool) {
	t.Helper()
	if len(rep.Migrations) != 1 || len(rep.Migrations[0].InsertSteps) != 1 {
		t.Fatalf("want one reached insert step, got %+v", rep.Migrations)
	}
	row := rep.Migrations[0]
	s := row.InsertSteps[0]
	if s.Step != 0 || s.Into != "catalog" || s.TargetExistsBefore == nil || *s.TargetExistsBefore != exists ||
		s.PendingBefore == nil || *s.PendingBefore != missing || s.DiffCountBefore == nil || *s.DiffCountBefore != differing ||
		row.UnmatchedCount != nil || row.DiffCountBefore == nil || *row.DiffCountBefore != differing {
		t.Fatalf("unexpected catalog observation: %+v, row %+v", s, row)
	}
	for _, n := range []*int64{s.PendingAfter, s.DiffCountAfter, row.DiffCountAfter} {
		if verified && (n == nil || *n != 0) || !verified && n != nil {
			t.Fatalf("unexpected post-write measurement %v (verification completed: %t): %+v", n, verified, s)
		}
	}
}

func catalogApply(t *testing.T, uri string, db *mongo.Database, dir, digest string, args ...string) (runner.Report, error) {
	t.Helper()
	out, err := migrune(t, uri, db, append([]string{"apply", "--dir", dir, "--digest-in", digest, "-o", "json"}, args...)...)
	var rep runner.Report
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("apply: %v, report: %v\n%s", err, jerr, out)
	}
	return rep, err
}

func TestCatalogMergeRefresh(t *testing.T) {
	uri, db := testTarget(t)
	dir, digest := catalogFiles(t)
	ctx := t.Context()
	source, target := db.Collection("legacyCatalog"), db.Collection("catalog")
	first, second, third := bson.NewObjectID(), bson.NewObjectID(), bson.NewObjectID()
	if _, err := source.InsertMany(ctx, []any{bson.M{"_id": first, "label": "A"}, bson.M{"_id": second, "label": "B"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := target.InsertOne(ctx, bson.M{"_id": second, "label": "old", "local": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := migrune(t, uri, db, "lint", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	plan := func(args ...string) runner.Report {
		t.Helper()
		rep, err := planJSON(t, uri, db, append([]string{"--dir", dir, "--digest-out", digest}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	checkCatalogCounts(t, plan(), true, 1, 1, false)
	if count, err := target.CountDocuments(ctx, bson.M{"_id": first}); err != nil || count != 0 {
		t.Fatalf("planned insert visible: %d, %v", count, err)
	}
	firstApply, err := catalogApply(t, uri, db, dir, digest, "--init")
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, firstApply, true, 1, 1, true)
	checkCatalogCounts(t, plan("--namespace", "observer"), true, 0, 0, false)

	if _, err := source.UpdateByID(ctx, second, bson.M{"$set": bson.M{"label": "B2"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.InsertOne(ctx, bson.M{"_id": third, "label": "C"}); err != nil {
		t.Fatal(err)
	}
	if rep := plan(); len(rep.Migrations) != 0 {
		t.Fatalf("finished catalog was scheduled again: %+v", rep.Migrations)
	}
	const refresh = "2026-11-26-001-refresh-catalog"
	raw, err := os.ReadFile(filepath.Join(dir, catalogBackfill+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, refresh+".json"), []byte(strings.Replace(string(raw), catalogBackfill, refresh, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, plan(), true, 1, 1, false)
	refreshed, err := catalogApply(t, uri, db, dir, digest)
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, refreshed, true, 1, 1, true)
	for _, want := range []bson.M{{"_id": first, "label": "A"}, {"_id": second, "label": "B2", "local": true}, {"_id": third, "label": "C"}} {
		if n, err := target.CountDocuments(ctx, want); err != nil || n != 1 {
			t.Errorf("catalog match %v: count %d, error %v", want, n, err)
		}
	}
	es := entries(t, db)
	if len(es) != 2 || es[0].MigrationID != catalogBackfill || es[1].MigrationID != refresh ||
		es[0].Status != runner.StatusFinished || es[1].Status != runner.StatusFinished {
		t.Fatalf("unexpected history: %+v", es)
	}
}

func TestCatalogMergeWriterAfterPlan(t *testing.T) {
	uri, db := testTarget(t)
	dir, digest := catalogFiles(t)
	ctx := t.Context()
	if _, err := db.Collection("legacyCatalog").InsertOne(ctx, bson.M{"_id": 1, "label": "catalog"}); err != nil {
		t.Fatal(err)
	}
	planned, err := planJSON(t, uri, db, "--dir", dir, "--digest-out", digest)
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, planned, false, 1, 0, false)
	names, err := db.ListCollectionNames(ctx, bson.M{"name": "catalog"})
	if err != nil || len(names) != 0 {
		t.Fatalf("target after plan: %v, %v", names, err)
	}
	target := db.Collection("catalog")
	if _, err := target.InsertOne(ctx, bson.M{"_id": 1, "label": "writer", "local": true}); err != nil {
		t.Fatal(err)
	}
	applied, err := catalogApply(t, uri, db, dir, digest, "--init")
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, applied, true, 0, 1, true)
	if n, err := target.CountDocuments(ctx, bson.M{"_id": 1, "label": "catalog", "local": true}); err != nil || n != 1 {
		t.Fatalf("writer's document after apply: count %d, error %v", n, err)
	}
}

// A rejected catalog document and a rejected Finished entry leave different
// evidence. The fresh plan decides whether the retry needs --init.
func TestCatalogMergeRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                string
		failCollection      string
		validator           bson.M
		missingAfterFailure int64
		firstApply          bool
	}{
		{"partial data write", "catalog", bson.M{"label": bson.M{"$type": "string"}}, 1, false},
		{"completion write", runner.ChangelogCollection, bson.M{"status": bson.M{"$ne": runner.StatusFinished}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri, db := testTarget(t)
			dir, digest := catalogFiles(t)
			ctx := t.Context()
			if err := db.CreateCollection(ctx, tc.failCollection, options.CreateCollection().SetValidator(tc.validator)); err != nil {
				t.Fatal(err)
			}
			second := bson.M{"_id": 2, "label": "B"}
			if tc.failCollection == "catalog" {
				second["label"] = 42
			}
			source := db.Collection("legacyCatalog")
			if _, err := source.InsertMany(ctx, []any{bson.M{"_id": 1, "label": "A"}, second}); err != nil {
				t.Fatal(err)
			}
			planned, err := planJSON(t, uri, db, "--dir", dir, "--digest-out", digest)
			if err != nil || !planned.FirstApply {
				t.Fatalf("first plan: firstApply %v, error %v", planned.FirstApply, err)
			}
			failed, err := catalogApply(t, uri, db, dir, digest, "--init")
			if err == nil || !strings.Contains(err.Error(), "121") {
				t.Fatalf("apply error %v, want validation error 121", err)
			}
			checkCatalogCounts(t, failed, tc.failCollection == "catalog", 2, 0, tc.firstApply)
			if n, err := db.Collection("catalog").CountDocuments(ctx, bson.M{}); err != nil || n != 2-tc.missingAfterFailure {
				t.Fatalf("surviving catalog size %d, error %v", n, err)
			}
			es := entries(t, db)
			if tc.firstApply && len(es) != 0 || !tc.firstApply && (len(es) != 1 || es[0].Status != runner.StatusErrored) {
				t.Fatalf("unexpected history after failure: %+v", es)
			}
			if tc.failCollection == "catalog" {
				if n, err := db.Collection("catalog").CountDocuments(ctx, bson.M{"_id": 1, "label": "A"}); err != nil || n != 1 {
					t.Fatalf("first catalog document after partial write: count %d, error %v", n, err)
				}
				if _, err := source.UpdateOne(ctx, bson.M{"_id": 2}, bson.M{"$set": bson.M{"label": "B"}}); err != nil {
					t.Fatal(err)
				}
			} else if err := db.RunCommand(ctx, bson.D{{Key: "collMod", Value: tc.failCollection}, {Key: "validator", Value: bson.M{}}}).Err(); err != nil {
				t.Fatal(err)
			}
			if !tc.firstApply {
				_, err := migrune(t, uri, db, "apply", "--dir", dir, "--digest-in", digest)
				if !errors.As(err, new(*runner.DigestMismatchError)) {
					t.Fatalf("stale first-apply digest: %v", err)
				}
			}
			fresh, err := planJSON(t, uri, db, "--dir", dir, "--digest-out", digest)
			if err != nil || fresh.FirstApply != tc.firstApply {
				t.Fatalf("replan: firstApply %v, error %v", fresh.FirstApply, err)
			}
			checkCatalogCounts(t, fresh, true, tc.missingAfterFailure, 0, false)
			var flags []string
			if fresh.FirstApply {
				if _, err := migrune(t, uri, db, "apply", "--dir", dir, "--digest-in", digest); !errors.As(err, new(*runner.InitError)) {
					t.Fatalf("retry on empty changelog without --init: %v", err)
				}
				flags = []string{"--init"}
			}
			retried, err := catalogApply(t, uri, db, dir, digest, flags...)
			if err != nil {
				t.Fatal(err)
			}
			checkCatalogCounts(t, retried, true, tc.missingAfterFailure, 0, true)
			es = entries(t, db)
			wantEntries := 1
			if !tc.firstApply {
				wantEntries++
			}
			if len(es) != wantEntries || es[len(es)-1].Status != runner.StatusFinished {
				t.Fatalf("unexpected retry history: %+v", es)
			}
		})
	}
}

func TestCatalogDBPointerIDs(t *testing.T) {
	uri, db := testTarget(t)
	dir, digest := catalogFiles(t)
	ctx := t.Context()
	oid := bson.NewObjectID()
	ids := []bson.DBPointer{{DB: "aa", Pointer: oid}, {DB: "bb", Pointer: oid}}
	for _, id := range ids {
		if _, err := db.Collection("legacyCatalog").InsertOne(ctx, bson.M{"_id": id, "label": id.DB}); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := planJSON(t, uri, db, "--dir", dir, "--digest-out", digest)
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, plan, false, 2, 0, false)
	applied, err := catalogApply(t, uri, db, dir, digest, "--init")
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, applied, false, 2, 0, true)
	catalog := db.Collection("catalog")
	if n, err := catalog.CountDocuments(ctx, bson.D{}); err != nil || n != 2 {
		t.Fatalf("catalog size %d, error %v", n, err)
	}
	for _, id := range ids {
		var doc struct {
			ID    bson.DBPointer `bson:"_id"`
			Label string
		}
		if err := catalog.FindOne(ctx, bson.M{"_id": id}).Decode(&doc); err != nil || doc.ID != id || doc.Label != id.DB {
			t.Fatalf("lookup for %v returned %+v, error %v", id, doc, err)
		}
	}
	if es := entries(t, db); len(es) != 1 || es[0].MigrationID != catalogBackfill || es[0].Status != runner.StatusFinished {
		t.Fatalf("unexpected history: %+v", es)
	}
	observed, err := planJSON(t, uri, db, "--dir", dir, "--namespace", "observer")
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, observed, true, 0, 0, false)
	if n := observed.Migrations[0].DuplicateOnCount; n == nil || *n != 0 {
		t.Fatalf("observer duplicateOnCount = %v, want a measured zero", n)
	}

	// A sample must keep the full DBPointer key alongside its field values.
	if _, err := db.Collection("legacyCatalog").UpdateByID(ctx, ids[0], bson.M{"$set": bson.M{"label": "updated"}}); err != nil {
		t.Fatal(err)
	}
	changed, err := planJSON(t, uri, db, "--dir", dir, "--namespace", "observer", "--sample-values")
	if err != nil {
		t.Fatal(err)
	}
	checkCatalogCounts(t, changed, true, 0, 1, false)
	samples := changed.Migrations[0].Samples
	if len(samples) != 1 || len(samples[0].Fields) != 1 {
		t.Fatalf("expected one changed field in one sample, got %+v", samples)
	}
	var key struct {
		ID bson.DBPointer `bson:"_id"`
	}
	if err := bson.UnmarshalExtJSON(samples[0].Key, false, &key); err != nil || key.ID != ids[0] {
		t.Fatalf("sample key %s, want %v, error %v", samples[0].Key, ids[0], err)
	}
	f := samples[0].Fields[0]
	if f.Field != "label" || string(f.Before) != `"aa"` || string(f.After) != `"updated"` {
		t.Fatalf("unexpected sampled change: %+v", f)
	}
}
