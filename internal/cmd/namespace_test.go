package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// planJSON runs plan -o json with args and returns its report.
func planJSON(t *testing.T, uri string, db *mongo.Database, args ...string) (runner.Report, error) {
	t.Helper()
	out, err := migrune(t, uri, db, append([]string{"plan", "-o", "json"}, args...)...)
	var rep runner.Report
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("report isn't JSON: %v\n%s", jerr, out)
	}
	return rep, err
}

// rowIDs returns the ids of rep's rows with status.
func rowIDs(rep runner.Report, status runner.RowStatus) []string {
	var ids []string
	for _, r := range rep.Migrations {
		if r.Status == status {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// TestNamespaces checks two namespaces in one database, each with its own
// directory: each applies and sees only its own entries, neither reports the
// other's migrations as applied but missing, and their locks don't block
// each other.
func TestNamespaces(t *testing.T) {
	uri, db := testTarget(t)
	api := migrationsDir(t, setFlag)
	web := migrationsDir(t, seed)

	if _, err := migrune(t, uri, db, "apply", "--no-digest", "--init", "--dir", api, "--namespace", "api"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRUNE_NAMESPACE", "web")
	t.Setenv("MIGRUNE_INIT", "true")
	if _, err := migrune(t, uri, db, "apply", "--no-digest", "--dir", web); err != nil {
		t.Fatalf("apply from MIGRUNE_NAMESPACE and MIGRUNE_INIT: %v", err)
	}
	t.Setenv("MIGRUNE_NAMESPACE", "")
	os.Unsetenv("MIGRUNE_INIT")

	got := map[string]string{}
	for _, e := range entries(t, db) {
		if e.Status != runner.StatusFinished {
			t.Errorf("entry %s: status %s", e.MigrationID, e.Status)
		}
		got[e.MigrationID] = e.Namespace
	}
	if len(got) != 2 || got[setFlag] != "api" || got[seed] != "web" {
		t.Fatalf("entries by migration: %v, want %s in api and %s in web", got, setFlag, seed)
	}

	for _, tc := range []struct{ namespace, dir string }{{"api", api}, {"web", web}} {
		rep, err := planJSON(t, uri, db, "--dir", tc.dir, "--namespace", tc.namespace)
		if err != nil || len(rep.Migrations) != 0 || len(rep.AppliedButMissing) != 0 || rep.Namespace != tc.namespace {
			t.Errorf("plan in %s: %v, report %+v, want nothing pending or missing", tc.namespace, err, rep)
		}
	}

	// web's lock doesn't block api, and only web's lock document is held.
	holder, err := runner.Start(context.Background(), db, runner.Options{RunnerID: "web-deploy", Namespace: "web"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	var lock struct {
		Owner string `bson:"owner"`
	}
	if err := db.Collection(runner.LockCollection).FindOne(context.Background(), bson.D{{Key: "_id", Value: "migrations:web"}}).Decode(&lock); err != nil || lock.Owner != "web-deploy" {
		t.Fatalf("lock document migrations:web: %+v, %v", lock, err)
	}
	addMigration(t, api, runnerFixtures("data"), dropStale)
	if out, err := migrune(t, uri, db, "apply", "--no-digest", "--dir", api, "--namespace", "api"); err != nil || !hasRow(out, dropStale, "applied") {
		t.Fatalf("apply in api while web holds its lock: %v\n%s", err, out)
	}
	if _, err := migrune(t, uri, db, "apply", "--no-digest", "--dir", web, "--namespace", "web"); !errors.Is(err, runner.ErrLockHeld) {
		t.Errorf("apply in web while web's lock is held: %v, want the lock held", err)
	}
	// The default namespace's lock is another document too.
	if _, err := migrune(t, uri, db, "plan", "--dir", migrationsDir(t)); err != nil {
		t.Errorf("plan in the default namespace while web's lock is held: %v", err)
	}
}

// TestNamespaceErrored checks an Errored entry records its namespace too.
func TestNamespaceErrored(t *testing.T) {
	uri, db := testTarget(t)
	seedItems(t, db)
	if _, err := migrune(t, uri, db, "apply", "--no-digest", "--init", "--dir", migrationsDir(t, incCounter), "--namespace", "api"); err == nil {
		t.Fatal("apply of a non-idempotent migration succeeded")
	}
	es := entries(t, db)
	if len(es) != 1 || es[0].Status != runner.StatusErrored || es[0].Namespace != "api" {
		t.Fatalf("entries %+v, want one Errored entry in api", es)
	}
}

// TestDefaultNamespace checks the default namespace and the others never mix:
//   - the default reads an entry without a namespace field, and writes
//     entries without one;
//   - a default-namespace run against a database whose changelog holds only
//     another namespace's entries, for files its directory doesn't have,
//     reports nothing applied but missing and applies its own;
//   - a namespaced run never sees default entries: a file the default
//     namespace applied is pending in it, and a default entry whose file it
//     doesn't have isn't applied but missing.
func TestDefaultNamespace(t *testing.T) {
	uri, db := testTarget(t)
	raw, err := os.ReadFile(filepath.Join(runnerFixtures("data"), setFlag+".json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	// A default-namespace Finished entry, with no namespace field.
	_, err = db.Collection(runner.ChangelogCollection).InsertOne(context.Background(), bson.D{
		{Key: "_id", Value: bson.NewObjectID()},
		{Key: "v", Value: int32(1)},
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

	// The default namespace sees it as applied.
	rep, err := planJSON(t, uri, db, "--dir", migrationsDir(t, setFlag))
	if err != nil || len(rep.Migrations) != 0 || rep.Namespace != "" {
		t.Errorf("default plan: %v, report %+v, want nothing pending", err, rep)
	}
	// A namespaced run doesn't: the file is pending there.
	rep, err = planJSON(t, uri, db, "--dir", migrationsDir(t, setFlag), "--namespace", "api")
	if err != nil || strings.Join(rowIDs(rep, runner.RowApplied), ",") != setFlag {
		t.Errorf("plan in api: %v, report %+v, want %s pending", err, rep, setFlag)
	}
	// Nor is the default entry applied but missing for a directory without
	// its file.
	if _, err := migrune(t, uri, db, "apply", "--no-digest", "--init", "--dir", migrationsDir(t, seed), "--namespace", "api"); err != nil {
		t.Fatalf("apply in api without the default namespace's file: %v", err)
	}

	// A fresh database whose changelog holds only api's entries: the
	// default namespace's directory has none of their files, and runs.
	_, other := testTarget(t)
	if _, err := migrune(t, uri, other, "apply", "--no-digest", "--init", "--dir", migrationsDir(t, setFlag), "--namespace", "api"); err != nil {
		t.Fatal(err)
	}
	out, err := migrune(t, uri, other, "apply", "--no-digest", "--init", "--dir", migrationsDir(t, seed))
	if err != nil || !hasRow(out, seed, "applied") || strings.Contains(out, "Applied but missing") || strings.Contains(out, "Namespace:") {
		t.Fatalf("default apply next to api's entries: %v\n%s", err, out)
	}
	for _, e := range entries(t, other) {
		if want := map[string]string{setFlag: "api", seed: ""}[e.MigrationID]; e.Namespace != want {
			t.Errorf("entry %s in namespace %q, want %q", e.MigrationID, e.Namespace, want)
		}
	}
	// The default entry has no namespace field at all.
	n, err := other.Collection(runner.ChangelogCollection).CountDocuments(context.Background(),
		bson.D{{Key: "migrationId", Value: seed}, {Key: "namespace", Value: bson.D{{Key: "$exists", Value: false}}}})
	if err != nil || n != 1 {
		t.Errorf("default entries without a namespace field: %d, %v, want 1", n, err)
	}
}

// TestNamespaceDigest checks a plan's digest differs across namespaces, so a
// plan for one can't approve another's apply, and the default namespace's
// digest input has no namespace line.
func TestNamespaceDigest(t *testing.T) {
	uri, db := testTarget(t)
	dir := migrationsDir(t, setFlag)
	tmp := t.TempDir()
	digests := map[string]string{}
	for _, namespace := range []string{"", "api", "web"} {
		path := filepath.Join(tmp, "plan-"+namespace+".digest")
		if _, err := migrune(t, uri, db, "plan", "--dir", dir, "--namespace", namespace, "--digest-out", path); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		digests[namespace] = string(raw)
	}
	// Pinned: the version, database, replica set, deployment, first-apply
	// and migration lines, with no namespace line.
	lines := strings.Split(digests[""], "\n")
	if len(lines) != 8 || lines[1] != runner.DigestVersion || lines[2] != "database "+db.Name() ||
		!strings.HasPrefix(lines[3], "replicaSet ") || (!strings.HasPrefix(lines[4], "deployment ") || lines[4] == "deployment none") || lines[5] != "first-apply" ||
		!strings.HasPrefix(lines[6], "migration "+setFlag+" ") || lines[7] != "" {
		t.Errorf("default digest file:\n%s", digests[""])
	}
	d, err := runner.ReadDigestFile(filepath.Join(tmp, "plan-.digest"))
	if err != nil || d.Text() != strings.Join(lines[1:], "\n") || d.Sum() != lines[0] {
		t.Errorf("default digest: %+v, %v", d, err)
	}
	for _, namespace := range []string{"api", "web"} {
		want := strings.Join(lines[1:5], "\n") + "\nnamespace " + namespace + "\n" + strings.Join(lines[5:], "\n")
		if _, text, _ := strings.Cut(digests[namespace], "\n"); text != want {
			t.Errorf("%s digest input:\n%s\nwant:\n%s", namespace, text, want)
		}
	}
	if digests[""] == digests["api"] || digests["api"] == digests["web"] {
		t.Error("two namespaces have the same digest")
	}

	_, err = migrune(t, uri, db, "apply", "--init", "--dir", dir, "--namespace", "web", "--digest-in", filepath.Join(tmp, "plan-api.digest"))
	var mismatch *runner.DigestMismatchError
	if !errors.As(err, &mismatch) || !strings.Contains(err.Error(), `the namespace is "web", the plan's was "api"`) {
		t.Fatalf("apply in web with api's plan: %v, want a digest mismatch naming the namespace", err)
	}
	if _, err := migrune(t, uri, db, "apply", "--init", "--dir", dir, "--namespace", "api", "--digest-in", filepath.Join(tmp, "plan-api.digest")); err != nil {
		t.Errorf("apply in api with api's plan: %v", err)
	}
}

// TestNamespaceOnlineLint checks online lint reads only its namespace's
// entries.
func TestNamespaceOnlineLint(t *testing.T) {
	uri, db := testTarget(t)
	if _, err := migrune(t, uri, db, "apply", "--no-digest", "--init", "--dir", migrationsDir(t, setFlag), "--namespace", "api"); err != nil {
		t.Fatal(err)
	}
	dir := migrationsDir(t, seed)
	if out, err := migrune(t, uri, db, "lint", "--dir", dir); err != nil || out != "" {
		t.Errorf("lint in the default namespace: %v\n%s", err, out)
	}
	if out, err := migrune(t, uri, db, "lint", "--dir", dir, "--namespace", "web"); err != nil || out != "" {
		t.Errorf("lint in web: %v\n%s", err, out)
	}
	out, err := migrune(t, uri, db, "lint", "--dir", dir, "--namespace", "api")
	if err == nil || errors.As(err, new(*UsageError)) || !strings.Contains(out, setFlag+".json: error: applied-missing: ") {
		t.Errorf("lint in api without %s: %v\n%s", setFlag, err, out)
	}
}

// TestChangelogIndex checks apply creates the changelog index, plan doesn't,
// and apply accepts the index when it already exists.
func TestChangelogIndex(t *testing.T) {
	want := `{"namespace":1,"status":1,"createdAt":1,"_id":1}`
	for _, tc := range []struct {
		name     string
		existing bool
	}{{name: "new"}, {name: "existing", existing: true}} {
		t.Run(tc.name, func(t *testing.T) {
			uri, db := testTarget(t)
			coll := db.Collection(runner.ChangelogCollection)
			dir := migrationsDir(t, setFlag)
			if tc.existing {
				_, err := coll.Indexes().CreateOne(context.Background(), mongo.IndexModel{
					Keys:    runner.ChangelogIndexKeys,
					Options: options.Index().SetName(runner.ChangelogIndexName),
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := migrune(t, uri, db, "plan", "--dir", dir); err != nil {
					t.Fatal(err)
				}
				if keys := changelogIndexKeys(t, coll); keys != "" {
					t.Fatalf("plan created the changelog index: %s", keys)
				}
			}
			if _, err := migrune(t, uri, db, "apply", "--no-digest", "--init", "--dir", dir); err != nil {
				t.Fatal(err)
			}
			if keys := changelogIndexKeys(t, coll); keys != want {
				t.Errorf("changelog index %s: keys %s, want %s", runner.ChangelogIndexName, keys, want)
			}
		})
	}
}

// changelogIndexKeys returns the keys of the changelog index, as Extended
// JSON, or "" when it doesn't exist.
func changelogIndexKeys(t *testing.T, coll *mongo.Collection) string {
	t.Helper()
	specs, err := coll.Indexes().ListSpecifications(context.Background())
	if err != nil {
		var ce mongo.CommandError
		if errors.As(err, &ce) && ce.Name == "NamespaceNotFound" {
			return ""
		}
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.Name == runner.ChangelogIndexName {
			keys, err := bson.MarshalExtJSON(s.KeysDocument, false, false)
			if err != nil {
				t.Fatal(err)
			}
			return string(keys)
		}
	}
	return ""
}
