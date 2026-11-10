package cmd

import (
	"bytes"
	"context"
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

// Index migration fixtures for drift, in testdata/drift.
const (
	driftIndexes = "2026-11-01-001-indexes"
	driftPending = "2026-11-02-001-pending"
	// In testdata/drift-wildcard.
	driftWildcard = "2026-11-01-002-wildcard"
)

func TestDriftUsage(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		message string
	}{
		{name: "missing dir", args: []string{"drift", "--uri", "mongodb://localhost", "--db", "app"}, message: "--dir is required"},
		{name: "missing uri", args: []string{"drift", "--dir", "testdata/drift", "--db", "app"}, message: "--uri is required"},
		{name: "missing db", args: []string{"drift", "--dir", "testdata/drift"}, env: map[string]string{"MIGRUNE_URI": "mongodb://localhost"},
			message: "--db is required"},
		{name: "bad output", args: []string{"drift", "--dir", "testdata/drift", "--uri", "mongodb://localhost", "--db", "app", "-o", "table2"},
			message: `invalid --output "table2"`},
		{name: "bad output from env", args: []string{"drift", "--dir", "testdata/drift", "--uri", "mongodb://localhost", "--db", "app"},
			env: map[string]string{"MIGRUNE_OUTPUT": "yaml"}, message: `invalid --output "yaml"`},
		{name: "invalid namespace", args: []string{"drift", "--dir", "testdata/drift", "--uri", "mongodb://localhost", "--db", "app", "--namespace", "API"},
			message: `--namespace: invalid namespace "API"`},
		{name: "invalid uri", args: []string{"drift", "--dir", "testdata/drift", "--uri", "postgres://user:secret@localhost", "--db", "app"},
			message: "--uri is not a valid MongoDB connection string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MIGRUNE_URI", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			_, err := Execute(context.Background(), tc.args, &stdout, &stderr)
			if !errors.As(err, new(*UsageError)) {
				t.Fatalf("got error %v, want a *UsageError", err)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q doesn't contain %q", err, tc.message)
			}
			if strings.Contains(stderr.String(), "secret") {
				t.Errorf("stderr shows the URI's password:\n%s", stderr.String())
			}
		})
	}
}

// TestDrift applies the index migration fixture to a fresh database, changes
// the database by hand, and checks what drift finds and how it exits.
func TestDrift(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// setup runs before apply, change after it.
		setup, change func(t *testing.T, db *mongo.Database)
		// edit changes the migrations directory after apply.
		edit func(t *testing.T, dir string)
		// wildcard adds the wildcard index migration of
		// testdata/drift-wildcard before apply.
		wildcard  bool
		namespace string // drift's --namespace; apply always uses the default
		failed    bool   // drift must fail, exit 1; otherwise succeed, exit 0
		want      []string
	}{
		// The collation index matches although listIndexes expands its
		// collation; views, which have no indexes, and migrune's own
		// collections are left out.
		{name: "clean",
			change: func(t *testing.T, db *mongo.Database) {
				if err := db.CreateView(ctx, "active_users", "users", mongo.Pipeline{}); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "index dropped by hand", failed: true,
			change: func(t *testing.T, db *mongo.Database) { dropIndex(t, db, "users", "email_1") },
			want:   []string{`users: error: index-missing: index email_1, created by ` + driftIndexes + ` with keys {"email": 1} options {"unique": true}, is not on the server`}},
		{name: "index created by hand",
			change: func(t *testing.T, db *mongo.Database) {
				createIndex(t, db, "accounts", bson.D{{Key: "x", Value: 1}}, "x_1")
			},
			want: []string{`accounts: warning: index-extra: index x_1, keys {"x": 1} options {}, is on the server but no applied migration of this namespace creates it`}},
		{name: "TTL changed by hand", failed: true,
			change: func(t *testing.T, db *mongo.Database) {
				runCommand(t, db, bson.D{{Key: "collMod", Value: "sessions"},
					{Key: "index", Value: bson.D{{Key: "name", Value: "expires_1"}, {Key: "expireAfterSeconds", Value: 60}}}})
			},
			want: []string{`sessions: error: index-options: index expires_1 has keys {"expiresAt": 1} with options {"expireAfterSeconds": 60} on the server, ` +
				`but the applied migrations leave index expires_1, created by ` + driftIndexes + `, with options {"expireAfterSeconds": 3600}`}},
		// The audit's case: a projection changed by hand, which drift
		// used to ignore.
		{name: "wildcard projection changed by hand", wildcard: true, failed: true,
			change: func(t *testing.T, db *mongo.Database) {
				dropIndex(t, db, "events", "payload_all")
				runCommand(t, db, bson.D{{Key: "createIndexes", Value: "events"}, {Key: "indexes", Value: bson.A{bson.D{
					{Key: "key", Value: bson.D{{Key: "$**", Value: 1}}}, {Key: "name", Value: "payload_all"},
					{Key: "wildcardProjection", Value: bson.D{{Key: "payload.id", Value: 1}}}}}}})
			},
			want: []string{`events: error: index-options: index payload_all has keys {"$**": 1} with options {"wildcardProjection": {"payload.id": 1}} on the server, ` +
				`but the applied migrations leave index payload_all, created by ` + driftWildcard + `, with options {"wildcardProjection": {"payload.id": 1, "payload.kind": 1}}`}},
		{name: "wildcard index unchanged", wildcard: true},
		{name: "renamed by hand",
			change: func(t *testing.T, db *mongo.Database) {
				dropIndex(t, db, "users", "email_1")
				createIndex(t, db, "users", bson.D{{Key: "email", Value: 1}}, "email_unique", true)
			},
			want: []string{`users: warning: index-renamed: index email_unique on the server matches index email_1, created by ` + driftIndexes + `, by definition, under a different name`}},
		{name: "pending migration not considered",
			edit: func(t *testing.T, dir string) { addMigration(t, dir, filepath.Join("testdata", "drift"), driftPending) }},
		{name: "another namespace's indexes are extra", namespace: "billing", want: []string{
			`accounts: warning: index-extra: index name_ci`,
			`labels: warning: index-extra: index label_1`,
			`sessions: warning: index-extra: index expires_1`,
			`users: warning: index-extra: index email_1`,
		}},
		// The changelog's copy is replayed, so the edit only fails the
		// checksum.
		{name: "applied file edited", failed: true,
			edit: func(t *testing.T, dir string) {
				path := filepath.Join(dir, driftIndexes+".json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, bytes.ReplaceAll(raw, []byte("3600"), []byte("60")), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{driftIndexes + ".json: error: checksum: the file changed after the migration was applied"}},
		{name: "collection default collation inherited",
			setup: func(t *testing.T, db *mongo.Database) {
				if err := db.CreateCollection(ctx, "labels", options.CreateCollection().
					SetCollation(&options.Collation{Locale: "fr", Strength: 1})); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "collation added by hand", failed: true,
			change: func(t *testing.T, db *mongo.Database) {
				dropIndex(t, db, "labels", "label_1")
				if _, err := db.Collection("labels").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "label", Value: 1}},
					Options: options.Index().SetName("label_1").SetCollation(&options.Collation{Locale: "fr", Strength: 1})}); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{`labels: error: index-options: index label_1 has keys {"label": 1} with options {"collation": {`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uri, db := testTarget(t)
			dir := t.TempDir()
			addMigration(t, dir, filepath.Join("testdata", "drift"), driftIndexes)
			if tc.wildcard {
				addMigration(t, dir, filepath.Join("testdata", "drift-wildcard"), driftWildcard)
			}
			if tc.setup != nil {
				tc.setup(t, db)
			}
			if _, err := migrune(t, uri, db, "apply", "--no-digest", "--dir", dir); err != nil {
				t.Fatal(err)
			}
			if tc.change != nil {
				tc.change(t, db)
			}
			if tc.edit != nil {
				tc.edit(t, dir)
			}
			before := entries(t, db)
			args := []string{"drift", "--dir", dir}
			if tc.namespace != "" {
				args = append(args, "--namespace", tc.namespace)
			}
			out, err := migrune(t, uri, db, args...)
			switch {
			case errors.As(err, new(*UsageError)):
				t.Fatalf("drift: %v, want no usage error", err)
			case tc.failed && err == nil:
				t.Fatal("drift succeeded, want a failure")
			case !tc.failed && err != nil:
				t.Fatalf("drift: %v, want success", err)
			}
			var got []string
			for line := range strings.Lines(out) {
				got = append(got, strings.TrimPrefix(strings.TrimSuffix(line, "\n"), dir+string(filepath.Separator)))
			}
			ok := len(got) == len(tc.want)
			for i := 0; ok && i < len(got); i++ {
				ok = strings.HasPrefix(got[i], tc.want[i])
			}
			if !ok {
				t.Errorf("findings:\n  %s\nwant lines starting:\n  %s", strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
			if after := entries(t, db); len(after) != len(before) {
				t.Errorf("drift wrote to the changelog: %d entries, had %d", len(after), len(before))
			}
			if n, err := db.Collection(runner.LockCollection).CountDocuments(ctx, bson.D{}); err != nil || n != 0 {
				t.Errorf("drift took the lock: %d documents, %v", n, err)
			}
		})
	}
}

// TestDriftNamespace checks drift reads only its namespace's changelog: the
// indexes another namespace applies don't count as applied, nor as missing.
func TestDriftNamespace(t *testing.T) {
	uri, db := testTarget(t)
	dir := t.TempDir()
	addMigration(t, dir, filepath.Join("testdata", "drift"), driftIndexes)
	if _, err := migrune(t, uri, db, "apply", "--no-digest", "--dir", dir, "--namespace", "billing"); err != nil {
		t.Fatal(err)
	}
	if out, err := migrune(t, uri, db, "drift", "--dir", dir, "--namespace", "billing", "-o", "json"); err != nil || out != "[]\n" {
		t.Fatalf("drift in the namespace: %v\n%s", err, out)
	}
	// The default namespace applied nothing: the file is pending, and every
	// index is extra.
	out, err := migrune(t, uri, db, "drift", "--dir", dir, "-o", "json")
	if err != nil {
		t.Fatalf("drift in the default namespace: %v", err)
	}
	if n := strings.Count(out, `"rule": "index-extra"`); n != 4 || !strings.Contains(out, `"collection": "users"`) {
		t.Errorf("findings:\n%s\nwant 4 index-extra findings, by collection", out)
	}
}

func dropIndex(t *testing.T, db *mongo.Database, coll, name string) {
	t.Helper()
	if err := db.Collection(coll).Indexes().DropOne(context.Background(), name); err != nil {
		t.Fatal(err)
	}
}

func createIndex(t *testing.T, db *mongo.Database, coll string, keys bson.D, name string, unique ...bool) {
	t.Helper()
	opts := options.Index().SetName(name)
	if len(unique) > 0 {
		opts.SetUnique(unique[0])
	}
	if _, err := db.Collection(coll).Indexes().CreateOne(context.Background(), mongo.IndexModel{Keys: keys, Options: opts}); err != nil {
		t.Fatal(err)
	}
}

func runCommand(t *testing.T, db *mongo.Database, cmd bson.D) {
	t.Helper()
	if err := db.RunCommand(context.Background(), cmd).Err(); err != nil {
		t.Fatal(err)
	}
}
