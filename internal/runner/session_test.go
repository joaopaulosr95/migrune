package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestTransientTransactionRetried checks a data migration whose transaction
// fails with a transient write conflict runs again, in a new transaction, and
// gives up after maxTransactionRetries new attempts (T2).
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

			rep, err := execute(t, ctx, db, load(t, fixtureDir(t, "data", setFlag)), Options{}, Config{Mode: ModeApply})
			if n := failCommandOff(t, client) - fired; n != int64(tc.conflicts) {
				t.Errorf("fail point fired %d times, want %d", n, tc.conflicts)
			}
			var wantErr []string
			if tc.wantErr {
				wantErr = []string{"WriteConflict", "112"}
			}
			checkSetFlagOutcome(t, ctx, db, rep, err, wantErr...)
		})
	}
}

// TestCommitOutcomeUnknown checks a commit that fails with
// UnknownTransactionCommitResult is sent again, and when the outcome stays
// unknown, the changelog entry decides it: there, the migration is applied;
// missing, it errored (T2).
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

			rep, err := execute(t, ctx, db, load(t, fixtureDir(t, "data", setFlag)), Options{}, Config{Mode: ModeApply})
			if n := failCommandOff(t, client) - fired; n < 1 {
				t.Errorf("fail point fired %d times, want at least once", n)
			}
			var wantErr []string
			if !tc.applied {
				wantErr = []string{"didn't commit"}
			}
			checkSetFlagOutcome(t, ctx, db, rep, err, wantErr...)
		})
	}
}

// checkSetFlagOutcome checks the outcome of an apply of setFlag on the
// items seedItems inserted. With no wantErr, the migration is applied: one
// Finished entry, and both items without a flag flagged. Otherwise err
// contains one of wantErr and the migration errored: one Errored entry, and
// no item flagged.
func checkSetFlagOutcome(t *testing.T, ctx context.Context, db *mongo.Database, rep *Report, err error, wantErr ...string) {
	t.Helper()
	entries := entryStatuses(changelog(t, ctx, db))
	flagged := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "flag", Value: false}})
	if len(wantErr) > 0 {
		if err == nil || !slices.ContainsFunc(wantErr, func(w string) bool { return strings.Contains(err.Error(), w) }) {
			t.Fatalf("apply: %v, want an error containing one of %q", err, wantErr)
		}
		if got := statuses(rep); !slices.Equal(got, []string{setFlag + ":errored"}) {
			t.Errorf("rows %v", got)
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
		t.Fatalf("apply: %v", err)
	}
	if got := statuses(rep); !slices.Equal(got, []string{setFlag + ":applied"}) {
		t.Errorf("rows %v", got)
	}
	if !slices.Equal(entries, []string{setFlag + ":Finished"}) {
		t.Errorf("changelog %v, want exactly one Finished entry", entries)
	}
	if flagged != 2 {
		t.Errorf("%d items flagged, want 2", flagged)
	}
}

// TestLockStolenBetweenMigrations checks a runner whose lock is taken over
// after a migration stops the run at its next write to the lock, whichever it
// is, and writes no changelog entry: the zombie neither applies the next
// migration nor records it as Errored (correctness #5).
func TestLockStolenBetweenMigrations(t *testing.T) {
	cases := []struct {
		name string
		// skip is how many of the runner's updates of the lock document go
		// through before the next one is held, while the lock is taken
		// over. The first migration makes three: current, the heartbeat
		// before its transaction, the fence.
		skip int
	}{
		{name: "current of the next migration", skip: 3},
		{name: "heartbeat before its transaction", skip: 4},
		{name: "fence of its transaction", skip: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			app := "migrune-zombie-" + rand.Text()[:8]
			client := appClient(t, app)
			db := freshDB(t, client)
			seedItems(t, ctx, db)
			fired := failCommand(t, ctx, client, bson.D{{Key: "skip", Value: tc.skip}}, bson.D{
				{Key: "failCommands", Value: bson.A{"update"}},
				{Key: "namespace", Value: db.Name() + "." + LockCollection},
				{Key: "appName", Value: app},
				{Key: "blockConnection", Value: true},
				{Key: "blockTimeMS", Value: 1500},
			})

			// A long interval: no timed heartbeat during the test.
			o := Options{RunnerID: "zombie", HeartbeatInterval: time.Minute, Logger: testLogger(t)}
			type result struct {
				rep *Report
				err error
			}
			ms := load(t, fixtureDir(t, "data", setFlag, dropStale))
			done := make(chan result, 1)
			go func() {
				rep, err := Execute(ctx, db, ms, o, Config{Mode: ModeApply, Init: true})
				done <- result{rep, err}
			}()
			waitFailPoint(t, ctx, client, fired+1)
			takeOver(t, ctx, testClient(t).Database(db.Name()), "next")
			res := <-done

			if !errors.Is(res.err, ErrLockLost) {
				t.Fatalf("apply: %v, want ErrLockLost", res.err)
			}
			want := []string{setFlag + ":applied", dropStale + ":errored"}
			if got := statuses(res.rep); !slices.Equal(got, want) {
				t.Errorf("rows %v, want %v", got, want)
			}
			if row := res.rep.row(dropStale); !strings.Contains(row.Error, "lock lost") {
				t.Errorf("row error %q, want the lock lost", row.Error)
			}
			if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{setFlag + ":Finished"}) {
				t.Errorf("changelog %v, want only the first migration's Finished entry", got)
			}
			if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "stale", Value: true}}); n != 1 {
				t.Errorf("%d stale items, want 1: the zombie applied the next migration", n)
			}
			if doc := readLock(t, ctx, db); doc == nil || doc.Owner != "next" {
				t.Errorf("lock %+v, want still the new holder's", doc)
			}
		})
	}
}

// TestLeaseExpiredStopsIndexStep checks a runner whose heartbeats are held by
// the server for longer than the lease margin doesn't send its next index
// command: the lock may expire on the server before the command lands (T12).
func TestLeaseExpiredStopsIndexStep(t *testing.T) {
	ctx := testCtx(t)
	app := "migrune-lease-" + rand.Text()[:8]
	client := appClient(t, app)
	db := freshDB(t, client)
	seedItems(t, ctx, db)
	// The lock expires 4s after each write, and the lease 2s after its send
	// time. Every write of this runner to the lock takes 3s.
	o := Options{RunnerID: "slow", HeartbeatInterval: time.Second, LockTTL: 4 * time.Second, Logger: testLogger(t)}
	fired := failCommand(t, ctx, client, "alwaysOn", bson.D{
		{Key: "failCommands", Value: bson.A{"update"}},
		{Key: "namespace", Value: db.Name() + "." + LockCollection},
		{Key: "appName", Value: app},
		{Key: "blockConnection", Value: true},
		{Key: "blockTimeMS", Value: 3000},
	})

	rep, err := Execute(ctx, db, load(t, fixtureDir(t, "index", createName)), o, Config{Mode: ModeApply, Init: true})
	if n := failCommandOff(t, client) - fired; n < 1 {
		t.Errorf("fail point fired %d times, want at least once", n)
	}
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("apply: %v, want ErrLockLost", err)
	}
	if got := statuses(rep); !slices.Equal(got, []string{createName + ":errored"}) {
		t.Errorf("rows %v", got)
	}
	if got := indexNames(t, ctx, db.Collection("items")); !slices.Equal(got, []string{"_id_"}) {
		t.Errorf("indexes %v: the index command was sent after the lease expired", got)
	}
	if n := countDocs(t, ctx, db.Collection(ChangelogCollection), bson.D{}); n != 0 {
		t.Errorf("%d changelog entries, want none", n)
	}
}
