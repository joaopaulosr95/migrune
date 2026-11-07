package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

func startRun(t *testing.T, ctx context.Context, o Options) *Run {
	t.Helper()
	if o.Logger == nil {
		o.Logger = testLogger(t)
	}
	run, err := Start(ctx, testDB(t), o)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestChangelogFinished(t *testing.T) {
	cases := []struct {
		name        string
		commit      bool
		wantData    int64
		wantEntries int64
	}{
		{name: "commit writes data and entry together", commit: true, wantData: 1, wantEntries: 1},
		{name: "abort discards both", commit: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			approver := ApprovedBy{Name: "Ada Lovelace", Email: "ada@example.com"}
			run := startRun(t, ctx, Options{RunnerID: "runner-a", ApprovedBy: approver})
			db := run.db
			m := fixture(t, "2026-11-05-001-add-archived-flag")
			data := db.Collection("projects")
			one := int64(1)
			before := time.Now()

			err := run.transaction(func(sctx context.Context) error {
				res, err := data.UpdateOne(sctx, bson.D{{Key: "_id", Value: 1}},
					bson.D{{Key: "$set", Value: bson.D{{Key: "archived", Value: false}}}},
					options.UpdateOne().SetUpsert(true))
				if err != nil {
					return err
				}
				return run.WriteFinished(sctx, bson.NewObjectID(), m, []StepResult{{
					Collection: "projects", Operation: "updateOne",
					MatchedCount: &res.MatchedCount, ModifiedCount: &res.ModifiedCount, UpsertedCount: &one,
				}})
			}, tc.commit, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}

			if n := countDocs(t, ctx, data, bson.D{}); n != tc.wantData {
				t.Errorf("%d data documents, want %d", n, tc.wantData)
			}
			entries := db.Collection(ChangelogCollection)
			if n := countDocs(t, ctx, entries, bson.D{}); n != tc.wantEntries {
				t.Fatalf("%d changelog entries, want %d", n, tc.wantEntries)
			}
			if readLock(t, ctx, db) != nil {
				t.Error("lock not released")
			}
			if !tc.commit {
				return
			}
			var raw bson.Raw
			if err := entries.FindOne(ctx, bson.D{}).Decode(&raw); err != nil {
				t.Fatal(err)
			}
			if v, ok := raw.Lookup("v").Int32OK(); !ok || v != EntryVersion {
				t.Errorf("entry v = %v, want int32 %d", raw.Lookup("v"), EntryVersion)
			}
			var e Entry
			if err := bson.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			if e.MigrationID != m.ID || e.Checksum != m.Checksum || e.Status != StatusFinished ||
				e.Runner != "runner-a" || e.ApprovedBy != approver || e.ID.IsZero() {
				t.Errorf("entry %+v does not match the migration, runner and approver", e)
			}
			if d := e.CreatedAt.Sub(before); d < -time.Minute || d > time.Minute {
				t.Errorf("createdAt %s is not server time near %s", e.CreatedAt, before)
			}
			if len(e.Results) != 1 || *e.Results[0].UpsertedCount != 1 || e.Results[0].DeletedCount != nil {
				t.Errorf("results %+v, want one upsert result", e.Results)
			}
			if e.Errors != nil {
				t.Errorf("Finished entry has errors %v", e.Errors)
			}
			// The snapshot keeps operators as data, not evaluated.
			filter := e.Migration.Lookup("steps", "0", "filter", "archived", "$exists")
			if b, ok := filter.BooleanOK(); !ok || b {
				t.Errorf("snapshot steps.0.filter.archived.$exists = %v, want false", filter)
			}
			if id := e.Migration.Lookup("id").StringValue(); id != m.ID {
				t.Errorf("snapshot id %q, want %q", id, m.ID)
			}
		})
	}
}

func TestChangelogErrored(t *testing.T) {
	ctx := testCtx(t)
	run := startRun(t, ctx, Options{RunnerID: "runner-a"})
	db := run.db
	m := fixture(t, "2026-11-05-001-add-archived-flag")

	failure := errors.New("step 2: second pass modified 3 documents")
	err := run.transaction(func(sctx context.Context) error {
		if _, err := db.Collection("projects").InsertOne(sctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
			return err
		}
		return failure
	}, true, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("transaction: %v, want %v", err, failure)
	}
	step := 1
	if err := run.WriteErrored(m, []StepError{{Step: &step, Message: err.Error()}}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	if n := countDocs(t, ctx, db.Collection("projects"), bson.D{}); n != 0 {
		t.Errorf("aborted transaction left %d documents", n)
	}
	var e Entry
	if err := db.Collection(ChangelogCollection).FindOne(ctx, bson.D{}).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.Status != StatusErrored || e.V != EntryVersion || e.MigrationID != m.ID || e.Runner != "runner-a" || e.Results != nil {
		t.Errorf("entry %+v, want an Errored entry for %s without results", e, m.ID)
	}
	if len(e.Errors) != 1 || *e.Errors[0].Step != 1 || e.Errors[0].Message != failure.Error() {
		t.Errorf("errors %+v, want the step failure", e.Errors)
	}
}

// TestInterruptMidTransaction checks graceful shutdown: cancelling the run's context while
// a transaction is open aborts it, writes no changelog entry and releases the
// lock.
func TestInterruptMidTransaction(t *testing.T) {
	parent := testCtx(t)
	ctx, interrupt := context.WithCancelCause(parent)
	defer interrupt(nil)
	run := startRun(t, ctx, Options{RunnerID: "runner-a"})
	db := run.db
	m := fixture(t, "2026-11-05-001-add-archived-flag")
	data := db.Collection("projects")
	signal := errors.New("terminated signal received")

	err := run.transaction(func(sctx context.Context) error {
		if _, err := data.InsertOne(sctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
			return err
		}
		interrupt(signal)
		// The next operation fails: the context is done.
		return run.WriteFinished(sctx, bson.NewObjectID(), m, nil)
	}, true, nil)
	var ie *InterruptedError
	if !errors.As(err, &ie) || !errors.Is(err, signal) {
		t.Fatalf("transaction: %v, want an InterruptedError caused by the signal", err)
	}
	if err := run.WriteErrored(m, []StepError{{Message: err.Error()}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > CleanupTimeout {
		t.Errorf("cleanup took %s, over %s", d, CleanupTimeout)
	}

	if n := countDocs(t, parent, data, bson.D{}); n != 0 {
		t.Errorf("interrupted transaction left %d documents", n)
	}
	if n := countDocs(t, parent, db.Collection(ChangelogCollection), bson.D{}); n != 0 {
		t.Errorf("interrupted run wrote %d changelog entries", n)
	}
	if doc := readLock(t, parent, db); doc != nil {
		t.Errorf("lock not released: %+v", doc)
	}
}

// TestFinished checks Finished keeps, per migration, what its latest Finished
// entry stored, ignoring Errored entries.
func TestFinished(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	at := time.Date(2026, 1, 4, 10, 12, 0, 0, time.UTC)
	entry := func(id, sum string, status Status, createdAt time.Time, runner, description string) bson.D {
		return bson.D{
			{Key: "migrationId", Value: id},
			{Key: "checksum", Value: sum},
			{Key: "createdAt", Value: createdAt},
			{Key: "runner", Value: runner},
			{Key: "status", Value: status},
			{Key: "migration", Value: bson.D{
				{Key: "id", Value: id},
				{Key: "description", Value: description},
				{Key: "steps", Value: bson.A{bson.D{{Key: "collection", Value: "items"}}}},
			}},
		}
	}
	_, err := db.Collection(ChangelogCollection).InsertMany(ctx, []any{
		entry("2026-01-04-001-a", "new", StatusFinished, at.Add(time.Hour), "runner-b", "Second apply"),
		entry("2026-01-04-001-a", "old", StatusFinished, at, "runner-a", "First apply"),
		entry("2026-01-04-001-a", "bad", StatusErrored, at.Add(2*time.Hour), "runner-c", "Errored"),
		entry("2026-01-05-001-b", "b", StatusFinished, at, "runner-a", "Only apply"),
		entry("2026-01-06-001-c", "c", StatusErrored, at, "runner-a", "Never finished"),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := Finished(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]migration.Applied{
		"2026-01-04-001-a": {ID: "2026-01-04-001-a", Checksum: "new", Description: "Second apply", AppliedAt: at.Add(time.Hour), Runner: "runner-b"},
		"2026-01-05-001-b": {ID: "2026-01-05-001-b", Checksum: "b", Description: "Only apply", AppliedAt: at, Runner: "runner-a"},
	}
	if len(got) != len(want) {
		t.Fatalf("Finished = %+v, want %+v", got, want)
	}
	for id, w := range want {
		g := got[id]
		if g.ID != w.ID || g.Checksum != w.Checksum || g.Description != w.Description || g.Runner != w.Runner ||
			!g.AppliedAt.Equal(w.AppliedAt) || g.AppliedAt.Location() != time.UTC {
			t.Errorf("%s: %+v, want %+v", id, g, w)
		}
	}
}

// TestSnapshot checks the changelog's migration snapshot keeps the file's id,
// description and steps, and only the fields each step sets.
func TestSnapshot(t *testing.T) {
	cases := []struct {
		id     string
		fields [][]string // per step, the keys in order
	}{
		{
			id: "2026-11-05-001-add-archived-flag",
			fields: [][]string{
				{"collection", "operation", "filter", "update"},
				{"collection", "operation", "filter", "update"},
				{"collection", "operation", "filter"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			m := fixture(t, tc.id)
			raw, err := snapshot(m)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ID          string     `bson:"id"`
				Description string     `bson:"description"`
				Steps       []bson.Raw `bson:"steps"`
			}
			if err := bson.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.ID != m.ID || got.Description != m.Description {
				t.Errorf("snapshot id %q description %q, want %q %q", got.ID, got.Description, m.ID, m.Description)
			}
			if len(got.Steps) != len(tc.fields) {
				t.Fatalf("snapshot has %d steps, want %d", len(got.Steps), len(tc.fields))
			}
			for i, step := range got.Steps {
				elems, err := step.Elements()
				if err != nil {
					t.Fatal(err)
				}
				var keys []string
				for _, e := range elems {
					keys = append(keys, e.Key())
				}
				if strings.Join(keys, ",") != strings.Join(tc.fields[i], ",") {
					t.Errorf("step %d keys %v, want %v", i, keys, tc.fields[i])
				}
			}
		})
	}
}
