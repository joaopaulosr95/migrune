package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// TestNewerFormat checks plan, apply and online lint fail, exiting 1,
// before anything else happens and with nothing written, when the changelog
// holds an entry, in any namespace, or the lock collection a document, with a
// v above the one this migrune writes.
func TestNewerFormat(t *testing.T) {
	newerEntry := bson.D{
		{Key: "_id", Value: bson.NewObjectID()}, {Key: "v", Value: int32(2)}, {Key: "namespace", Value: "web"},
		{Key: "migrationId", Value: "2026-10-01-001-later-format"}, {Key: "status", Value: "Finished"},
	}
	newerLock := bson.D{
		{Key: "_id", Value: "migrations:web"}, {Key: "v", Value: int32(2)}, {Key: "token", Value: "t"},
		{Key: "owner", Value: "newer"}, {Key: "expiresAt", Value: time.Now().Add(-time.Hour)},
	}
	cases := []struct {
		name string
		coll string
		doc  bson.D
		want string
	}{
		{name: "entry", coll: runner.ChangelogCollection, doc: newerEntry,
			want: "the changelog has format v2, newer than this migrune understands (v1): upgrade migrune"},
		{name: "lock", coll: runner.LockCollection, doc: newerLock,
			want: "the migration lock has format v2, newer than this migrune understands (v1): upgrade migrune"},
	}
	for _, tc := range cases {
		for _, command := range []string{"plan", "apply", "lint"} {
			t.Run(tc.name+" "+command, func(t *testing.T) {
				uri, db := testTarget(t)
				seedItems(t, db)
				if _, err := db.Collection(tc.coll).InsertOne(context.Background(), tc.doc); err != nil {
					t.Fatal(err)
				}
				before := dumpDB(t, db)
				out, err := migrune(t, uri, db, append(localArgs(command), "--dir", migrationsDir(t, setFlag))...)
				var format *runner.FormatError
				if !errors.As(err, &format) || err.Error() != tc.want {
					t.Fatalf("%s: %v, want %q", command, err, tc.want)
				}
				// Neither a usage error (exit 2) nor a held lock (exit 3):
				// exit 1.
				if errors.As(err, new(*UsageError)) || errors.Is(err, runner.ErrLockHeld) {
					t.Errorf("%v exits 2 or 3, want 1", err)
				}
				if out != "" {
					t.Errorf("stdout %q, want nothing", out)
				}
				if after := dumpDB(t, db); after != before {
					t.Errorf("%s changed the database:\nbefore %s\nafter  %s", command, before, after)
				}
			})
		}
	}
}

// TestOlderFormat checks entries and a lock document without v read as v1: a
// Finished entry without v counts as applied, and an expired lock without v
// is taken over.
func TestOlderFormat(t *testing.T) {
	uri, db := testTarget(t)
	raw, err := os.ReadFile(filepath.Join(runnerFixtures("data"), setFlag+".json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	ctx := context.Background()
	_, err = db.Collection(runner.ChangelogCollection).InsertOne(ctx, bson.D{
		{Key: "_id", Value: bson.NewObjectID()},
		{Key: "migrationId", Value: setFlag},
		{Key: "checksum", Value: hex.EncodeToString(sum[:])},
		{Key: "createdAt", Value: time.Now()},
		{Key: "runner", Value: "v0-runner"},
		{Key: "approvedBy", Value: bson.D{{Key: "name", Value: ""}, {Key: "email", Value: ""}}},
		{Key: "status", Value: "Finished"},
		{Key: "migration", Value: bson.D{{Key: "id", Value: setFlag}, {Key: "description", Value: "Set a flag"}, {Key: "steps", Value: bson.A{}}}},
		{Key: "results", Value: bson.A{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Collection(runner.LockCollection).InsertOne(ctx, bson.D{
		{Key: "_id", Value: "migrations"}, {Key: "token", Value: "old"}, {Key: "owner", Value: "v0-runner"},
		{Key: "current", Value: nil}, {Key: "acquiredAt", Value: time.Now().Add(-time.Hour)},
		{Key: "lastHeartbeat", Value: time.Now().Add(-time.Hour)}, {Key: "expiresAt", Value: time.Now().Add(-time.Minute)},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := migrationsDir(t, setFlag, seed)
	if out, err := migrune(t, uri, db, "lint", "--dir", dir); err != nil {
		t.Errorf("lint: %v\n%s", err, out)
	}
	out, err := migrune(t, uri, db, "apply", "--no-digest", "--dir", dir)
	if err != nil || !hasRow(out, seed, "applied") || hasRow(out, setFlag, "applied") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
}

// dumpDB renders every collection of db, its documents in _id order and its
// index names, as Extended JSON.
func dumpDB(t *testing.T, db *mongo.Database) string {
	t.Helper()
	ctx := context.Background()
	names, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	slices.Sort(names)
	for _, name := range names {
		coll := db.Collection(name)
		cur, err := coll.Find(ctx, bson.D{})
		if err != nil {
			t.Fatal(err)
		}
		var docs []bson.Raw
		if err := cur.All(ctx, &docs); err != nil {
			t.Fatal(err)
		}
		specs, err := coll.Indexes().ListSpecifications(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(name + ": indexes")
		for _, s := range specs {
			b.WriteString(" " + s.Name)
		}
		b.WriteString("; documents")
		for _, d := range docs {
			b.WriteString(" " + d.String())
		}
		b.WriteString("\n")
	}
	return b.String()
}
