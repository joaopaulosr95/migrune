package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/joaopaulosr95/migrune/internal/inttest"
	"github.com/joaopaulosr95/migrune/internal/migration"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testLogger logs to the test output, so failures show the runner's logs.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(tWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type tWriter struct{ t *testing.T }

func (w tWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(p[:len(p)-1]))
	return len(p), nil
}

// fixtureDir copies the fixtures testdata/<sub>/<id>.json into a new
// directory.
func fixtureDir(t *testing.T, sub string, ids ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, id := range ids {
		addFixture(t, dir, sub, id)
	}
	return dir
}

// addFixture copies the fixture testdata/<sub>/<id>.json into dir.
func addFixture(t *testing.T, dir, sub, id string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", sub, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// load loads the migrations in dir.
func load(t *testing.T, dir string) []migration.Migration {
	t.Helper()
	d, err := migration.LoadDir(dir, discard())
	if err != nil {
		t.Fatal(err)
	}
	return d.Migrations
}

// fixture loads the migration testdata/<id>.json.
func fixture(t *testing.T, id string) migration.Migration {
	t.Helper()
	ms := load(t, fixtureDir(t, "", id))
	if len(ms) != 1 {
		t.Fatalf("load fixture %s: %d migrations", id, len(ms))
	}
	return ms[0]
}

// TestMain releases this test process's hold on the test replica set.
func TestMain(m *testing.M) {
	code := m.Run()
	inttest.Release()
	os.Exit(code)
}

// testClient connects to the replica set named by MIGRUNE_TEST_URI, or skips
// the test.
func testClient(t *testing.T) *mongo.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: skipped with -short")
	}
	uri := os.Getenv("MIGRUNE_TEST_URI")
	if uri == "" {
		t.Skip("integration test: MIGRUNE_TEST_URI is not set; run `task test:int`")
	}
	inttest.Exclusive(t, uri)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		t.Fatalf("ping primary: %v", err)
	}
	return client
}

// testDB returns a fresh database on the test replica set, dropped when the
// test ends.
func testDB(t *testing.T) *mongo.Database {
	t.Helper()
	return freshDB(t, testClient(t))
}

func freshDB(t *testing.T, client *mongo.Client) *mongo.Database {
	t.Helper()
	db := client.Database("migrune_test_"+rand.Text()[:12], options.Database().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Majority()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = db.Drop(ctx)
	})
	return db
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func newTestLock(t *testing.T, db *mongo.Database, owner string, interval, ttl time.Duration) *Lock {
	t.Helper()
	l, err := NewLock(db, owner, interval, ttl, testLogger(t).With("runner", owner))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// readLock returns the lock document, or nil when there is none.
func readLock(t *testing.T, ctx context.Context, db *mongo.Database) *lockDoc {
	t.Helper()
	var doc lockDoc
	err := db.Collection(LockCollection).FindOne(ctx, bson.D{{Key: "_id", Value: lockID}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	return &doc
}

// takeOver gives the lock a new token and owner, as a runner taking over an
// expired lock would.
func takeOver(t *testing.T, ctx context.Context, db *mongo.Database, owner string) {
	t.Helper()
	_, err := db.Collection(LockCollection).UpdateOne(ctx, bson.D{{Key: "_id", Value: lockID}},
		mongo.Pipeline{{{Key: "$set", Value: bson.D{
			{Key: "token", Value: newToken()},
			{Key: "owner", Value: owner},
			{Key: "lastHeartbeat", Value: "$$NOW"},
			{Key: "expiresAt", Value: bson.D{{Key: "$add", Value: bson.A{"$$NOW", int64(time.Minute / time.Millisecond)}}}},
		}}}})
	if err != nil {
		t.Fatalf("take over lock: %v", err)
	}
}

func countDocs(t *testing.T, ctx context.Context, coll *mongo.Collection, filter any) int64 {
	t.Helper()
	n, err := coll.CountDocuments(ctx, filter)
	if err != nil {
		t.Fatalf("count %s: %v", coll.Name(), err)
	}
	return n
}

// eventually polls cond every 100ms until it holds or timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// appClient connects to the test replica set with appName app, so a
// failCommand fail point can target this client's commands only.
func appClient(t *testing.T, app string) *mongo.Client {
	t.Helper()
	testClient(t) // skips when there is no replica set
	client, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("MIGRUNE_TEST_URI")).SetAppName(app))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return client
}

// failCommand configures the failCommand fail point on the primary, and turns
// it off when the test ends. It returns how many times the fail point had
// fired before, for waitFailPoint and failCommandOff. It needs the server's
// test commands, which test/replicaset enables.
//
// The fail point is server-wide: test packages must not run in parallel
// (task test:int runs them one at a time; inttest fails them otherwise), and
// data should name this test's database or app.
func failCommand(t *testing.T, ctx context.Context, client *mongo.Client, mode any, data bson.D) (fired int64) {
	t.Helper()
	var res struct {
		Count int64 `bson:"count"`
	}
	err := client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: mode}, {Key: "data", Value: data},
	}).Decode(&res)
	if err != nil {
		t.Fatalf("configure fail point: %v", err)
	}
	t.Cleanup(func() { failCommandOff(t, client) })
	return res.Count
}

// failCommandOff turns the failCommand fail point off and returns how many
// times it has fired in total.
func failCommandOff(t *testing.T, client *mongo.Client) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var res struct {
		Count int64 `bson:"count"`
	}
	err := client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"},
	}).Decode(&res)
	if err != nil {
		t.Errorf("turn fail point off: %v", err)
	}
	return res.Count
}

// waitFailPoint waits until the failCommand fail point has fired n times in
// total.
func waitFailPoint(t *testing.T, ctx context.Context, client *mongo.Client, n int64) {
	t.Helper()
	err := client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "waitForFailPoint", Value: "failCommand"}, {Key: "timesEntered", Value: n},
	}).Err()
	if err != nil {
		t.Fatalf("wait for the fail point: %v", err)
	}
}

func insert(t *testing.T, ctx context.Context, coll *mongo.Collection, docs ...bson.D) {
	t.Helper()
	if _, err := coll.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
}
