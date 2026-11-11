package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// Batched data migration fixtures in testdata/batch.
const (
	flagAll       = "2026-11-09-001-flag-all"
	flagMixed     = "2026-11-09-002-flag-mixed"
	markSeen      = "2026-11-09-003-mark-seen"
	parseN        = "2026-11-09-004-parse-n"
	deleteStale   = "2026-11-09-005-delete-stale"
	flagOneByOne  = "2026-11-09-006-flag-one-by-one"
	batchFixtures = "batch"
)

// TestBatched checks a data migration with a batchSize: plan runs its first
// batch and writes nothing, reporting how many documents match and how many
// it checked; apply runs every batch and writes one Finished entry with the
// summed counts and the number of batches.
func TestBatched(t *testing.T) {
	ints := func(n int, extra bson.D) []any {
		docs := make([]any, n)
		for i := range docs {
			docs[i] = append(bson.D{{Key: "_id", Value: i}}, extra...)
		}
		return docs
	}
	cases := []struct {
		name string
		id   string
		docs []any
		// matched is how many documents the filter matches; checked how
		// many plan's first batch holds.
		matched, checked int64
		batches          int64
		counts           string
		// check checks the data after apply.
		check func(t *testing.T, ctx context.Context, items *mongo.Collection)
	}{
		{
			// Over MaxTransactionDocs: the size guard doesn't apply.
			name: "25,000 documents in batches of 10,000", id: flagAll, docs: ints(25000, nil),
			matched: 25000, checked: 10000, batches: 3, counts: "matched 25000, modified 25000, upserted 0",
			check: func(t *testing.T, ctx context.Context, items *mongo.Collection) {
				if n := countDocs(t, ctx, items, bson.D{{Key: "flag", Value: false}}); n != 25000 {
					t.Errorf("%d items flagged, want 25000", n)
				}
			},
		},
		{
			// A $gt cursor on _id would visit one BSON type only.
			name: "mixed _id types", id: flagMixed,
			docs: []any{
				bson.D{{Key: "_id", Value: bson.NewObjectID()}}, bson.D{{Key: "_id", Value: bson.NewObjectID()}},
				bson.D{{Key: "_id", Value: bson.NewObjectID()}}, bson.D{{Key: "_id", Value: bson.NewObjectID()}},
				bson.D{{Key: "_id", Value: "a"}}, bson.D{{Key: "_id", Value: "b"}},
				bson.D{{Key: "_id", Value: "c"}}, bson.D{{Key: "_id", Value: "d"}},
				bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "_id", Value: 2.5}},
				bson.D{{Key: "_id", Value: bson.D{{Key: "k", Value: 1}}}},
				bson.D{{Key: "_id", Value: "e"}, {Key: "flag", Value: true}},
			},
			matched: 11, checked: 3, batches: 4, counts: "matched 11, modified 11, upserted 0",
			check: func(t *testing.T, ctx context.Context, items *mongo.Collection) {
				if n := countDocs(t, ctx, items, bson.D{{Key: "flag", Value: false}}); n != 11 {
					t.Errorf("%d items flagged, want 11", n)
				}
			},
		},
		{
			// The documents still match the filter once changed: each is
			// visited once, and the run ends.
			name: "documents still match after the update", id: markSeen,
			docs: []any{
				bson.D{{Key: "_id", Value: 1}, {Key: "kind", Value: "a"}}, bson.D{{Key: "_id", Value: 2}, {Key: "kind", Value: "b"}},
				bson.D{{Key: "_id", Value: 3}, {Key: "kind", Value: "a"}}, bson.D{{Key: "_id", Value: 4}, {Key: "kind", Value: "a"}},
				bson.D{{Key: "_id", Value: 5}, {Key: "kind", Value: "a"}}, bson.D{{Key: "_id", Value: 6}, {Key: "kind", Value: "b"}},
				bson.D{{Key: "_id", Value: 7}, {Key: "kind", Value: "a"}},
			},
			matched: 5, checked: 2, batches: 3, counts: "matched 5, modified 5, upserted 0",
			check: func(t *testing.T, ctx context.Context, items *mongo.Collection) {
				if n := countDocs(t, ctx, items, bson.D{{Key: "kind", Value: "a"}, {Key: "seen", Value: true}}); n != 5 {
					t.Errorf("%d items of kind a seen, want 5", n)
				}
				if n := countDocs(t, ctx, items, bson.D{{Key: "seen", Value: true}}); n != 5 {
					t.Errorf("%d items seen, want 5", n)
				}
			},
		},
		{
			name: "deleteMany", id: deleteStale, docs: append(ints(10, bson.D{{Key: "stale", Value: true}}),
				bson.D{{Key: "_id", Value: "kept1"}}, bson.D{{Key: "_id", Value: "kept2"}, {Key: "stale", Value: false}}),
			matched: 10, checked: 4, batches: 3, counts: "deleted 10",
			check: func(t *testing.T, ctx context.Context, items *mongo.Collection) {
				if n := countDocs(t, ctx, items, bson.D{}); n != 2 {
					t.Errorf("%d items left, want 2", n)
				}
			},
		},
		{
			name: "nothing matches", id: flagAll, docs: []any{bson.D{{Key: "_id", Value: 1}, {Key: "flag", Value: true}}},
			matched: 0, checked: 0, batches: 1, counts: "matched 0, modified 0, upserted 0",
			check: func(*testing.T, context.Context, *mongo.Collection) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			items := db.Collection("items")
			if _, err := items.InsertMany(ctx, tc.docs); err != nil {
				t.Fatal(err)
			}
			ms := load(t, fixtureDir(t, batchFixtures, tc.id))
			before := dump(t, ctx, items)

			rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if !slices.EqualFunc(dump(t, ctx, items), before, func(a, b bson.Raw) bool { return bytes.Equal(a, b) }) {
				t.Error("plan changed the items")
			}
			if entries := changelog(t, ctx, db); len(entries) != 0 {
				t.Errorf("plan wrote changelog entries: %v", entryStatuses(entries))
			}
			row := rep.Migrations[0]
			if row.Status != RowApplied || row.BatchSize != ms[0].BatchSize || row.Matched == nil || *row.Matched != tc.matched ||
				row.Checked == nil || *row.Checked != tc.checked || row.Batches != nil {
				t.Errorf("plan row %+v, want applied, batchSize %d, matched %d, checked %d", row, ms[0].BatchSize, tc.matched, tc.checked)
			}

			rep, err = execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			row = rep.Migrations[0]
			if row.Status != RowApplied || row.BatchSize != ms[0].BatchSize || row.Batches == nil || *row.Batches != tc.batches ||
				row.Matched != nil || row.Checked != nil {
				t.Errorf("apply row %+v, want applied, batchSize %d, batches %d", row, ms[0].BatchSize, tc.batches)
			}
			if len(row.Steps) != 1 || row.Steps[0].Counts() != tc.counts {
				t.Errorf("apply row steps %v, want %s", dataSteps(row), tc.counts)
			}
			entries := changelog(t, ctx, db)
			if got := entryStatuses(entries); !slices.Equal(got, []string{tc.id + ":Finished"}) {
				t.Fatalf("changelog %v, want one Finished entry", got)
			}
			e := entries[0]
			if e.Batches != int(tc.batches) || len(e.Results) != 1 || resultCounts(e.Results[0]) != tc.counts {
				t.Errorf("entry batches %d, results %+v, want %d batches and %s", e.Batches, e.Results, tc.batches, tc.counts)
			}
			if got := e.Migration.Lookup("batchSize").AsInt64(); got != int64(ms[0].BatchSize) {
				t.Errorf("entry snapshot batchSize %d, want %d", got, ms[0].BatchSize)
			}
			tc.check(t, ctx, items)
		})
	}
}

// TestBatchedFailure checks a batched migration that fails in its second
// batch: the first batch stays applied, the Errored entry and the report say
// how many batches committed, and a rerun of the fixed file finishes.
func TestBatchedFailure(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	items := db.Collection("items")
	docs := make([]any, 30)
	for i := range docs {
		v := string(rune('0' + i%10))
		if i == 15 {
			v = "x"
		}
		docs[i] = bson.D{{Key: "_id", Value: i}, {Key: "v", Value: v}}
	}
	if _, err := items.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}

	rep, err := execute(t, ctx, db, load(t, fixtureDir(t, batchFixtures, parseN)), Options{}, Config{Mode: ModeApply})
	const want = "; committed 1 of 3 batches"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("apply: %v, want it to contain %q", err, want)
	}
	row := rep.Migrations[0]
	if row.Status != RowErrored || !strings.Contains(row.Error, want) || !strings.Contains(row.Error, serverMessageHidden) ||
		row.Batches == nil || *row.Batches != 1 {
		t.Errorf("row %+v, want errored, with %q and the server's message hidden, 1 batch", row, want)
	}
	if got := dataSteps(row); !slices.Equal(got, []string{"0 updateMany items: matched 10, modified 10, upserted 0"}) {
		t.Errorf("row steps %v, want the first batch's counts", got)
	}
	entries := changelog(t, ctx, db)
	if got := entryStatuses(entries); !slices.Equal(got, []string{parseN + ":Errored"}) {
		t.Fatalf("changelog %v, want one Errored entry", got)
	}
	if errs := entries[0].Errors; len(errs) != 1 || errs[0].Step == nil || *errs[0].Step != 0 || !strings.Contains(errs[0].Message, want) {
		t.Errorf("errors %+v, want step 0 with %q", errs, want)
	}
	// The first batch, _ids 0 to 9, committed; the others didn't.
	for _, c := range []struct {
		filter bson.D
		want   int64
	}{
		{bson.D{{Key: "n", Value: bson.D{{Key: "$exists", Value: true}}}, {Key: "_id", Value: bson.D{{Key: "$lt", Value: 10}}}}, 10},
		{bson.D{{Key: "n", Value: bson.D{{Key: "$exists", Value: true}}}}, 10},
	} {
		if n := countDocs(t, ctx, items, c.filter); n != c.want {
			t.Errorf("%d items with n matching %v, want %d", n, c.filter, c.want)
		}
	}

	rep, err = execute(t, ctx, db, load(t, fixtureDir(t, "batch-fixed", parseN)), Options{}, Config{Mode: ModeApply})
	if err != nil {
		t.Fatalf("apply the fixed file: %v", err)
	}
	if row := rep.Migrations[0]; row.Batches == nil || *row.Batches != 2 {
		t.Errorf("row %+v, want 2 batches", row)
	}
	if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{parseN + ":Errored", parseN + ":Finished"}) {
		t.Errorf("changelog %v, want Errored then Finished", got)
	}
	if n := countDocs(t, ctx, items, bson.D{{Key: "n", Value: bson.D{{Key: "$exists", Value: true}}}}); n != 30 {
		t.Errorf("%d items with n, want 30", n)
	}
	if n := countDocs(t, ctx, items, bson.D{{Key: "_id", Value: 15}, {Key: "n", Value: -1}}); n != 1 {
		t.Error("item 15 doesn't have n -1")
	}
}

// TestBatchedFailureOneBatch checks the error of a batched migration that
// fails in its only batch says "batch", not "batches".
func TestBatchedFailureOneBatch(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	docs := []any{bson.D{{Key: "_id", Value: 0}, {Key: "v", Value: "0"}}, bson.D{{Key: "_id", Value: 1}, {Key: "v", Value: "x"}}}
	if _, err := db.Collection("items").InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	_, err := execute(t, ctx, db, load(t, fixtureDir(t, batchFixtures, parseN)), Options{}, Config{Mode: ModeApply})
	if err == nil || !strings.Contains(err.Error(), "; committed 0 of 1 batch;") {
		t.Fatalf("apply: %v, want it to contain %q", err, "; committed 0 of 1 batch;")
	}
}

// TestBatchedCommitUnknown checks a batch that isn't the last, whose commit
// outcome stays unknown, runs again, and the migration finishes.
func TestBatchedCommitUnknown(t *testing.T) {
	ctx := testCtx(t)
	app := "migrune-batch-" + rand.Text()[:8]
	client := appClient(t, app)
	db := freshDB(t, client)
	items := db.Collection("items")
	if _, err := items.InsertMany(ctx, []any{bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "_id", Value: 2}}, bson.D{{Key: "_id", Value: 3}}}); err != nil {
		t.Fatal(err)
	}
	// The first batch's commit and its retries fail before running.
	times := 1 + maxCommitRetries
	fired := failCommand(t, ctx, client, bson.D{{Key: "times", Value: times}}, bson.D{
		{Key: "failCommands", Value: bson.A{"commitTransaction"}},
		{Key: "appName", Value: app},
		{Key: "errorCode", Value: 50},
		{Key: "errorLabels", Value: bson.A{labelUnknownCommit}},
	})

	rep, err := execute(t, ctx, db, load(t, fixtureDir(t, batchFixtures, flagOneByOne)), Options{}, Config{Mode: ModeApply})
	if n := failCommandOff(t, client) - fired; n != int64(times) {
		t.Errorf("fail point fired %d times, want %d", n, times)
	}
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if row := rep.Migrations[0]; row.Batches == nil || *row.Batches != 3 {
		t.Errorf("row %+v, want 3 batches", row)
	}
	if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{flagOneByOne + ":Finished"}) {
		t.Errorf("changelog %v, want one Finished entry", got)
	}
	if n := countDocs(t, ctx, items, bson.D{{Key: "flag", Value: false}}); n != 3 {
		t.Errorf("%d items flagged, want 3", n)
	}
}

// TestBatchStep checks a batch's step: the filter restricted to the batch's
// _ids, and no hint.
func TestBatchStep(t *testing.T) {
	ms := load(t, fixtureDir(t, batchFixtures, flagMixed))
	ids := []bson.RawValue{}
	for _, v := range []any{"a", int32(1)} {
		typ, data, err := bson.MarshalValue(v)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, bson.RawValue{Type: typ, Value: data})
	}
	s, err := batchStep(ms[0].Steps[0], ids)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := migration.Canonical(s.Filter), `{"$and":[{"flag":{"$exists":false}},{"_id":{"$in":["a",1]}}]}`; got != want {
		t.Errorf("filter %s, want %s", got, want)
	}
	if got := migration.Canonical(s.Options); got != `{}` {
		t.Errorf("options %s, want {}", got)
	}
	if got := migration.Canonical(ms[0].Steps[0].Options); got != `{"hint":"_id_"}` {
		t.Errorf("the step's own options changed: %s", got)
	}
}

// dump returns every document of coll, in _id order.
func dump(t *testing.T, ctx context.Context, coll *mongo.Collection) []bson.Raw {
	t.Helper()
	cur, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var docs []bson.Raw
	if err := cur.All(ctx, &docs); err != nil {
		t.Fatal(err)
	}
	return docs
}

// resultCounts renders a changelog step result's counts as DataStep.Counts
// does.
func resultCounts(r StepResult) string {
	return DataStep{Matched: r.MatchedCount, Modified: r.ModifiedCount, Upserted: r.UpsertedCount, Deleted: r.DeletedCount}.Counts()
}
