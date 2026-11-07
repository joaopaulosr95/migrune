package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// TestTransientTransactionRetried checks a transaction that fails with a
// transient write conflict runs again, in a new transaction, and gives up
// after maxTransactionRetries new attempts (T2).
func TestTransientTransactionRetried(t *testing.T) {
	cases := []struct {
		name      string
		conflicts int // first updates of items that fail
		wantErr   bool
	}{
		{name: "one write conflict", conflicts: 1},
		{name: "a write conflict on every attempt", conflicts: maxTransactionRetries + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			client := testClient(t)
			db := freshDB(t, client)
			seedItems(t, ctx, db)
			fired := failCommand(t, ctx, client, bson.D{{Key: "times", Value: tc.conflicts}}, bson.D{
				{Key: "failCommands", Value: bson.A{"update"}},
				{Key: "namespace", Value: db.Name() + ".items"},
				{Key: "errorCode", Value: 112},
				{Key: "errorLabels", Value: bson.A{labelTransient}},
			})

			err := flagItems(t, ctx, db)
			if n := failCommandOff(t, client) - fired; n != int64(tc.conflicts) {
				t.Errorf("fail point fired %d times, want %d", n, tc.conflicts)
			}
			var wantErr []string
			if tc.wantErr {
				wantErr = []string{"WriteConflict", "112"}
			}
			checkSetFlagOutcome(t, ctx, db, err, wantErr...)
		})
	}
}

// TestCommitOutcomeUnknown checks a commit that fails with
// UnknownTransactionCommitResult is sent again, and when the outcome stays
// unknown, the changelog entry decides it: there, the transaction committed;
// missing, it didn't (T2).
func TestCommitOutcomeUnknown(t *testing.T) {
	unknown := bson.D{{Key: "errorCode", Value: 50}, {Key: "errorLabels", Value: bson.A{labelUnknownCommit}}}
	cases := []struct {
		name string
		// times is how many commitTransaction commands fail.
		times int
		// fail is the failCommand data: the error the commits get.
		fail    bson.D
		applied bool
	}{
		// The commit fails before running; sent again, it commits.
		{name: "commit sent again", times: 1, fail: unknown, applied: true},
		// Every commit runs but reports a write concern timeout: the
		// outcome stays unknown, and the Finished entry is there.
		{name: "commit landed", times: 10, fail: bson.D{{Key: "writeConcernError", Value: bson.D{
			{Key: "code", Value: 64}, {Key: "codeName", Value: "WriteConcernFailed"},
			{Key: "errmsg", Value: "waiting for replication timed out"},
			{Key: "errInfo", Value: bson.D{{Key: "wtimeout", Value: true}}},
		}}}, applied: true},
		// The commit and its retries fail before running: the outcome
		// stays unknown and the Finished entry is missing. The Errored
		// entry's own commit, the next one, goes through.
		{name: "commit never landed", times: 1 + maxCommitRetries, fail: unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			app := "migrune-commit-" + rand.Text()[:8]
			client := appClient(t, app)
			db := freshDB(t, client)
			seedItems(t, ctx, db)
			data := append(bson.D{
				{Key: "failCommands", Value: bson.A{"commitTransaction"}},
				{Key: "appName", Value: app},
			}, tc.fail...)
			fired := failCommand(t, ctx, client, bson.D{{Key: "times", Value: tc.times}}, data)

			err := flagItems(t, ctx, db)
			if n := failCommandOff(t, client) - fired; n < 1 {
				t.Errorf("fail point fired %d times, want at least once", n)
			}
			var wantErr []string
			if !tc.applied {
				wantErr = []string{"didn't commit"}
			}
			checkSetFlagOutcome(t, ctx, db, err, wantErr...)
		})
	}
}

// setFlag is the migration flagItems records in the changelog.
const setFlag = "2026-11-05-001-add-archived-flag"

func seedItems(t *testing.T, ctx context.Context, db *mongo.Database) {
	t.Helper()
	_, err := db.Collection("items").InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "a"}},
		bson.D{{Key: "_id", Value: 2}, {Key: "name", Value: "b"}, {Key: "flag", Value: true}},
		bson.D{{Key: "_id", Value: 3}, {Key: "name", Value: "c"}, {Key: "stale", Value: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// flagItems starts a run on db and calls flagItemsIn, then releases the
// lock.
func flagItems(t *testing.T, ctx context.Context, db *mongo.Database) error {
	t.Helper()
	run, err := Start(ctx, db, Options{Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	err = flagItemsIn(run, fixture(t, setFlag))
	if cerr := run.Close(); cerr != nil {
		t.Error(cerr)
	}
	return err
}

// flagItemsIn stands in for applying m: in one fenced transaction it
// flags the items seedItems inserted without a flag and writes m's Finished
// entry; when that fails, it records m as Errored.
func flagItemsIn(run *Run, m migration.Migration) error {
	id := bson.NewObjectID()
	err := run.transaction(func(sctx context.Context) error {
		_, err := run.db.Collection("items").UpdateMany(sctx,
			bson.D{{Key: "flag", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "flag", Value: false}}}})
		if err != nil {
			return err
		}
		return run.WriteFinished(sctx, id, m, nil)
	}, true, &id)
	if err != nil {
		return errors.Join(err, run.WriteErrored(m, []StepError{{Message: err.Error()}}))
	}
	return nil
}

// changelog returns the changelog entries, oldest first.
func changelog(t *testing.T, ctx context.Context, db *mongo.Database) []Entry {
	t.Helper()
	cur, err := db.Collection(ChangelogCollection).Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var entries []Entry
	if err := cur.All(ctx, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

// entryStatuses renders entries as "id:status".
func entryStatuses(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.MigrationID+":"+string(e.Status))
	}
	return out
}

// checkSetFlagOutcome checks the outcome of flagItems on the items seedItems
// inserted. With no wantErr, the transaction committed: one Finished entry,
// and both items without a flag flagged. Otherwise err contains one of
// wantErr and it didn't: one Errored entry, and no item flagged.
func checkSetFlagOutcome(t *testing.T, ctx context.Context, db *mongo.Database, err error, wantErr ...string) {
	t.Helper()
	entries := entryStatuses(changelog(t, ctx, db))
	flagged := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "flag", Value: false}})
	if len(wantErr) > 0 {
		if err == nil || !slices.ContainsFunc(wantErr, func(w string) bool { return strings.Contains(err.Error(), w) }) {
			t.Fatalf("flagItems: %v, want an error containing one of %q", err, wantErr)
		}
		if !slices.Equal(entries, []string{setFlag + ":Errored"}) {
			t.Errorf("changelog %v, want one Errored entry", entries)
		}
		if flagged != 0 {
			t.Errorf("%d items flagged, want none", flagged)
		}
		return
	}
	if err != nil {
		t.Fatalf("flagItems: %v", err)
	}
	if !slices.Equal(entries, []string{setFlag + ":Finished"}) {
		t.Errorf("changelog %v, want exactly one Finished entry", entries)
	}
	if flagged != 2 {
		t.Errorf("%d items flagged, want 2", flagged)
	}
}
