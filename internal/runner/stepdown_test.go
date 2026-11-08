package runner

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// primary returns the host of the current primary, or "" when there is none.
func primary(ctx context.Context, client *mongo.Client) string {
	var hello struct {
		Primary           string `bson:"primary"`
		IsWritablePrimary bool   `bson:"isWritablePrimary"`
	}
	err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
	if err != nil {
		return ""
	}
	return hello.Primary
}

// TestDataMigrationSurvivesStepDown checks a primary step-down while a data
// migration's transaction is open aborts that attempt, and the transaction
// runs again on the new primary: the migration ends Finished, once (T2). The
// lock (written with w: majority) survives the step-down: another runner
// can't take it on the new primary. The step-down affects every test using
// the replica set, so the test gives the original primary time to take back
// its role before it ends.
func TestDataMigrationSurvivesStepDown(t *testing.T) {
	client := testClient(t)
	db := freshDB(t, client)
	ctx := testCtx(t)
	seedItems(t, ctx, db)
	old := primary(ctx, client)
	if old == "" {
		t.Fatal("no primary")
	}
	t.Cleanup(func() { waitOriginalPrimary(t, client, old) })

	// The migration's first update is held on the primary while the
	// transaction is open; the step-down happens meanwhile. Only that
	// node has the fail point, and only once.
	fired := failCommand(t, ctx, client, bson.D{{Key: "times", Value: 1}}, bson.D{
		{Key: "failCommands", Value: bson.A{"update"}},
		{Key: "namespace", Value: db.Name() + ".items"},
		{Key: "blockConnection", Value: true},
		{Key: "blockTimeMS", Value: 3000},
	})
	ms := load(t, fixtureDir(t, "data", setFlag))
	// The run is started here, not by Execute, so it still holds the lock
	// once the migration is done.
	run, err := Start(ctx, db, Options{RunnerID: "runner-a", Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	rep := &Report{}
	done := make(chan error, 1)
	go func() { done <- run.execute(ms, Config{Mode: ModeApply}, DeploymentNone, rep) }()
	waitFailPoint(t, ctx, client, fired+1)
	// The step-down closes the connection, so the command may fail even
	// though it worked.
	err = client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "replSetStepDown", Value: 10},
		{Key: "secondaryCatchUpPeriodSecs", Value: 5},
	}).Err()
	if err != nil {
		t.Logf("replSetStepDown: %v", err)
	}
	err = <-done
	t.Logf("primary moved from %s to %s", old, primary(ctx, client))

	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Still held: another runner can't take it.
	other := newTestLock(t, db, "runner-b", time.Second, time.Second)
	if err := other.Acquire(ctx, 0); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("runner-b acquire after step-down: %v, want ErrLockHeld", err)
	}
	if err := run.Err(); err != nil {
		t.Fatalf("run cancelled by the step-down: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if doc := readLock(t, ctx, db); doc != nil {
		t.Errorf("lock %+v not released after the step-down", doc)
	}
	if got := statuses(rep); !slices.Equal(got, []string{setFlag + ":applied"}) {
		t.Errorf("rows %v", got)
	}
	if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{setFlag + ":Finished"}) {
		t.Errorf("changelog %v, want exactly one Finished entry", got)
	}
	if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "flag", Value: false}}); n != 2 {
		t.Errorf("%d items flagged, want 2", n)
	}
}

// waitOriginalPrimary leaves the replica set as found: the original primary
// has the highest priority and takes over again once its step-down period
// ends.
func waitOriginalPrimary(t *testing.T, client *mongo.Client, old string) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		p := primary(pctx, client)
		cancel()
		if p == old {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("original primary %s did not take its role back within 60s", old)
}
