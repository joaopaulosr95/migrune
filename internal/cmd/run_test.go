package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/inttest"
	"github.com/joaopaulosr95/migrune/internal/migration"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

// Data migration fixtures shared with the migration tests.
const (
	setFlag = "2026-11-05-001-add-archived-flag"
	seed    = "2026-11-05-002-seed-price-table-v1"
)

// TestMain releases this test process's hold on the test replica set.
func TestMain(m *testing.M) {
	code := m.Run()
	inttest.Release()
	os.Exit(code)
}

// testTarget returns the test replica set's URI and a fresh database on it,
// dropped when the test ends, or skips the test.
func testTarget(t *testing.T) (string, *mongo.Database) {
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
		t.Fatal(err)
	}
	db := client.Database("migrune_test_" + rand.Text()[:12])
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	return uri, db
}

// validFixtures is the directory of the migration package's valid
// fixtures.
var validFixtures = filepath.Join("..", "migration", "testdata", "valid")

// migrationsDir copies the valid migration fixtures ids into a new
// directory.
func migrationsDir(t *testing.T, ids ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, id := range ids {
		raw, err := os.ReadFile(filepath.Join(validFixtures, id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// migrune runs the command line with MIGRUNE_URI and MIGRUNE_DB set
// for db, as CI would pass them.
func migrune(t *testing.T, uri string, db *mongo.Database, args ...string) (stdout string, err error) {
	t.Helper()
	t.Setenv("MIGRUNE_URI", uri)
	t.Setenv("MIGRUNE_DB", db.Name())
	var out, logs bytes.Buffer
	_, err = Execute(context.Background(), args, &out, &logs)
	t.Logf("migrune %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out.String(), logs.String())
	return out.String(), err
}

// seedItems gives the database a collection of its own.
func seedItems(t *testing.T, db *mongo.Database) {
	t.Helper()
	_, err := db.Collection("items").InsertMany(context.Background(), []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "a"}},
		bson.D{{Key: "_id", Value: 2}, {Key: "name", Value: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// finish records the migrations ids of dir as Finished in db's changelog,
// as the runner writes them.
func finish(t *testing.T, db *mongo.Database, dir string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		m, err := migration.LoadFile(filepath.Join(dir, id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Collection(runner.ChangelogCollection).InsertOne(context.Background(), bson.D{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "v", Value: int32(runner.EntryVersion)},
			{Key: "migrationId", Value: m.ID},
			{Key: "checksum", Value: m.Checksum},
			{Key: "createdAt", Value: time.Now()},
			{Key: "runner", Value: "ci-job-7"},
			{Key: "approvedBy", Value: bson.D{{Key: "name", Value: ""}, {Key: "email", Value: ""}}},
			{Key: "status", Value: string(runner.StatusFinished)},
			{Key: "migration", Value: bson.D{{Key: "id", Value: m.ID}, {Key: "description", Value: m.Description}, {Key: "steps", Value: bson.A{}}}},
			{Key: "results", Value: bson.A{}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func entries(t *testing.T, db *mongo.Database) []runner.Entry {
	t.Helper()
	cur, err := db.Collection(runner.ChangelogCollection).Find(context.Background(), bson.D{},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var out []runner.Entry
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// appliedContext returns what the changelog in entries stored about the
// Finished migration id, as lint reports it.
func appliedContext(t *testing.T, entries []runner.Entry, id string) migration.Applied {
	t.Helper()
	for _, e := range slices.Backward(entries) {
		if e.MigrationID == id && e.Status == runner.StatusFinished {
			a := migration.Applied{ID: id, AppliedAt: e.CreatedAt.UTC(), Runner: e.Runner}
			a.Description = e.Migration.Lookup("description").StringValue()
			if a.Runner == "" || a.Description == "" || a.AppliedAt.IsZero() {
				t.Fatalf("entry %+v lacks the context to report", e)
			}
			return a
		}
	}
	t.Fatalf("no Finished entry for %s", id)
	return migration.Applied{}
}

// TestOnlineLint checks lint --uri --db compares checksums with the
// changelog and reports applied migrations whose file is missing, read-only.
func TestOnlineLint(t *testing.T) {
	uri, db := testTarget(t)
	dir := migrationsDir(t, setFlag, seed)
	finish(t, db, dir, setFlag, seed)
	if out, err := migrune(t, uri, db, "lint", "--dir", dir); err != nil || out != "" {
		t.Fatalf("lint of unchanged files: %v\n%s", err, out)
	}

	path := filepath.Join(dir, setFlag+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	before := entries(t, db)
	out, err := migrune(t, uri, db, "lint", "--dir", dir)
	if err == nil || errors.As(err, new(*UsageError)) {
		t.Fatalf("lint of an edited applied migration: %v, want a failure", err)
	}
	if !strings.Contains(out, setFlag+".json: error: checksum: the file changed after the migration was applied") ||
		strings.Contains(out, seed) {
		t.Errorf("findings:\n%s", out)
	}
	if after := entries(t, db); len(after) != len(before) {
		t.Errorf("lint wrote to the changelog: %d entries, had %d", len(after), len(before))
	}
	if n, err := db.Collection(runner.LockCollection).CountDocuments(context.Background(), bson.D{}); err != nil || n != 0 {
		t.Errorf("lint took the lock: %d documents, %v", n, err)
	}

	// A Finished migration whose file is gone is an error too.
	if err := os.Remove(filepath.Join(dir, seed+".json")); err != nil {
		t.Fatal(err)
	}
	applied := appliedContext(t, before, seed)
	out, err = migrune(t, uri, db, "lint", "--dir", dir)
	if err == nil || errors.As(err, new(*UsageError)) {
		t.Fatalf("lint of a missing applied migration: %v, want a failure", err)
	}
	if want := "\n" + filepath.Join(dir, seed+".json") + ": error: applied-missing: " + applied.MissingContext() + ". "; !strings.Contains("\n"+out, want) {
		t.Errorf("findings:\n%s\nwant a line starting %q", out, want)
	}

	// Without --uri, lint stays offline.
	t.Setenv("MIGRUNE_URI", "")
	var stdout bytes.Buffer
	if _, err := Execute(context.Background(), []string{"lint", "--dir", dir}, &stdout, &bytes.Buffer{}); err != nil {
		t.Errorf("offline lint: %v\n%s", err, stdout.String())
	}
}
