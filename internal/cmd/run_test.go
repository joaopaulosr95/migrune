package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
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

// Data migration fixtures shared with the runner tests.
const (
	setFlag    = "2026-11-01-001-set-flag"
	incCounter = "2026-11-01-002-inc-counter"
	seed       = "2026-11-01-003-seed-settings"
	dropStale  = "2026-11-01-004-drop-stale"
)

func TestPlanApplyUsage(t *testing.T) {
	dir := migrationsDir(t, setFlag)
	conn := []string{"--dir", dir, "--uri", "mongodb://127.0.0.1:1", "--db", "app"}
	cases := []struct {
		name    string
		args    []string
		message string
	}{
		{name: "plan without dir", args: []string{"plan", "--uri", "mongodb://x", "--db", "app"}, message: "--dir is required"},
		{name: "plan without uri", args: []string{"plan", "--dir", dir, "--db", "app"}, message: "--uri is required"},
		{name: "apply without db", args: []string{"apply", "--dir", dir, "--uri", "mongodb://x"}, message: "--db is required"},
		{name: "bad output", args: append([]string{"plan", "-o", "yaml"}, conn...), message: `invalid --output "yaml"`},
		{name: "zero lock ttl", args: append([]string{"plan", "--lock-ttl", "0s"}, conn...), message: "invalid --lock-ttl 0s"},
		{name: "negative lock ttl", args: append([]string{"apply", "--lock-ttl", "-90s"}, conn...), message: "invalid --lock-ttl -1m30s"},
		{name: "fractional lock ttl", args: append([]string{"apply", "--lock-ttl", "1500ms"}, conn...), message: "invalid --lock-ttl 1.5s"},
		{name: "lock ttl at the transaction limit", args: append([]string{"plan", "--lock-ttl", "60s"}, conn...), message: "invalid --lock-ttl 1m0s: want a whole number of seconds, at least 61s"},
		{name: "negative lock wait", args: append([]string{"apply", "--lock-wait", "-1s"}, conn...), message: "invalid --lock-wait -1s"},
		{name: "unparsable lock wait", args: append([]string{"plan", "--lock-wait", "forever"}, conn...), message: "lock-wait"},
		{name: "unparsable lock ttl", args: append([]string{"plan", "--lock-ttl", "soon"}, conn...), message: "lock-ttl"},
		{name: "invalid uri", args: []string{"apply", "--dir", dir, "--uri", "mongodb://ada:s3cret@host/?authMechanismProperties=AWS_SESSION_TOKEN:tok3n&connectTimeoutMS=soon", "--db", "app"},
			message: "--uri is not a valid MongoDB connection string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			_, err := Execute(context.Background(), tc.args, &stdout, &stderr)
			var usage *UsageError
			if !errors.As(err, &usage) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("got %v, want a usage error containing %q", err, tc.message)
			}
			for _, secret := range []string{"s3cret", "tok3n"} {
				if strings.Contains(err.Error()+stderr.String()+stdout.String(), secret) {
					t.Errorf("output leaks %q: %v\n%s", secret, err, stderr.String())
				}
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want no report", stdout.String())
			}
		})
	}
}

// TestReservedCollection checks plan and apply refuse, before connecting, a
// migration that names a reserved collection, so skipping lint can't bypass
// the check.
func TestReservedCollection(t *testing.T) {
	for _, file := range []string{"2026-11-01-001-changelog-data", "2026-11-01-009-facet-lookup"} {
		for _, command := range []string{"plan", "apply"} {
			t.Run(command+" "+file, func(t *testing.T) {
				dir := t.TempDir()
				raw, err := os.ReadFile(filepath.Join("..", "migration", "testdata", "reserved", file+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, file+".json"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				_, err = Execute(context.Background(), []string{command, "--dir", dir, "--uri", "mongodb://127.0.0.1:1", "--db", "app"}, &stdout, &stderr)
				if err == nil || !strings.Contains(err.Error(), "reserved") || errors.As(err, new(*UsageError)) {
					t.Fatalf("got %v, want a failure naming the reserved collection", err)
				}
				if stdout.Len() != 0 || strings.Contains(stderr.String(), "connecting") {
					t.Errorf("ran past loading:\nstdout %s\nstderr %s", stdout.String(), stderr.String())
				}
			})
		}
	}
}

var updateGolden = flag.Bool("update", false, "rewrite the report golden files with the current output")

func TestWriteReport(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	rep := &runner.Report{
		Command: "apply",
		Runner:  "ci-42",
		Migrations: []runner.Row{
			{ID: setFlag, Kind: "data", Status: runner.RowApplied, DurationMs: 12, Steps: []runner.DataStep{
				{Index: 0, Operation: "updateMany", Collection: "items", Matched: n(2), Modified: n(2), Upserted: n(0)},
			}},
			{ID: incCounter, Kind: "data", Status: runner.RowErrored, DurationMs: 1500, Error: "/steps/0 (updateMany on items, second run): not idempotent",
				Steps: []runner.DataStep{{Index: 0, Operation: "updateMany", Collection: "items", Matched: n(3), Modified: n(3), Upserted: n(0)}}},
			{ID: seed, Kind: "data", Status: runner.RowSkipped},
		},
		AppliedButMissing: []migration.Applied{},
		Totals:            runner.Totals{Migrations: 3, Applied: 1, Errored: 1, Skipped: 1, DurationMs: 1530},
		Error:             "migration 2026-11-01-002-inc-counter failed: not idempotent",
		ApprovedBy:        &runner.ApprovedBy{Name: "Ada Lovelace", Email: "ada@example.com"},
	}
	at := time.Date(2026, 1, 4, 10, 12, 0, 0, time.UTC)
	gone1 := migration.Applied{ID: "2026-10-01-001-gone", Checksum: "not printed", Description: "Backfill the flag", AppliedAt: at, Runner: "ci-7"}
	gone2 := migration.Applied{ID: "2026-10-02-001-gone", Description: `Rename "old" to "new"`,
		AppliedAt: at.Add(26*time.Hour + 1500*time.Millisecond), Runner: "laptop-81-x7"}
	missing := &runner.Report{
		Command: "plan",
		Runner:  "ci-42",
		Migrations: []runner.Row{
			{ID: setFlag, Kind: "data", Status: runner.RowSkipped},
			{ID: seed, Kind: "data", Status: runner.RowSkipped},
		},
		AppliedButMissing: []migration.Applied{gone1, gone2},
		Totals:            runner.Totals{Migrations: 2, Skipped: 2, DurationMs: 40},
		Error:             (&runner.AppliedMissingError{Migrations: []migration.Applied{gone1, gone2}}).Error(),
	}
	checks := &runner.Report{
		Command: "apply",
		Runner:  "ci-42",
		Migrations: []runner.Row{
			{ID: setFlag, Kind: "data", Status: runner.RowErrored, Error: "the file changed since the migration was applied"},
			{ID: seed, Kind: "data", Status: runner.RowSkipped},
		},
		AppliedButMissing: []migration.Applied{gone1},
		Totals:            runner.Totals{Migrations: 2, Errored: 1, Skipped: 1, DurationMs: 40},
		Error: errors.Join(&runner.AppliedMissingError{Migrations: []migration.Applied{gone1}},
			&runner.ChecksumError{IDs: []string{setFlag}}).Error(),
	}
	empty := &runner.Report{Command: "plan", Runner: "ci-42", Migrations: []runner.Row{}, AppliedButMissing: []migration.Applied{}}
	dataPlan := &runner.Report{
		Command: "plan",
		Runner:  "ci-42",
		Migrations: []runner.Row{
			{ID: seed, Kind: "data", Status: runner.RowApplied, DurationMs: 9, Steps: []runner.DataStep{
				{Index: 0, Operation: "updateOne", Collection: "settings", Matched: n(0), Modified: n(0), Upserted: n(1)},
			}},
			{ID: dropStale, Kind: "data", Status: runner.RowApplied, DurationMs: 14, Steps: []runner.DataStep{
				{Index: 0, Operation: "deleteMany", Collection: "items", Deleted: n(1)},
				{Index: 1, Operation: "updateMany", Collection: "items", Matched: n(2), Modified: n(2), Upserted: n(0)},
			}},
		},
		AppliedButMissing: []migration.Applied{},
		Totals:            runner.Totals{Migrations: 2, Applied: 2, DurationMs: 30},
	}
	// Written by hand: no runner, no createdAt.
	byHand := migration.Applied{ID: "2026-10-03-001-by-hand", Description: "Fix by hand"}
	unknown := &runner.Report{
		Command:           "apply",
		Runner:            "ci-42",
		Migrations:        []runner.Row{{ID: seed, Kind: "data", Status: runner.RowSkipped}},
		AppliedButMissing: []migration.Applied{byHand},
		Totals:            runner.Totals{Migrations: 1, Skipped: 1, DurationMs: 40},
		Error:             (&runner.AppliedMissingError{Migrations: []migration.Applied{byHand}}).Error(),
	}
	// A plan against a new database.
	newDB := &runner.Report{
		Command:           "plan",
		Runner:            "ci-42",
		Database:          "appp",
		NewDatabase:       true,
		Migrations:        []runner.Row{{ID: setFlag, Kind: "data", Status: runner.RowApplied, DurationMs: 5}},
		AppliedButMissing: []migration.Applied{},
		Totals:            runner.Totals{Migrations: 1, Applied: 1, DurationMs: 9},
	}
	cases := []struct {
		name   string
		rep    *runner.Report
		output string
		golden string
	}{
		{name: "table", rep: rep, output: "table", golden: "apply.txt"},
		{name: "new database table", rep: newDB, output: "table", golden: "plan-new-database.txt"},
		{name: "new database json", rep: newDB, output: "json", golden: "plan-new-database.json"},
		{name: "json", rep: rep, output: "json", golden: "apply.json"},
		{name: "empty plan table", rep: empty, output: "table", golden: "plan-empty.txt"},
		{name: "empty plan json", rep: empty, output: "json", golden: "plan-empty.json"},
		{name: "data plan table", rep: dataPlan, output: "table", golden: "plan-data.txt"},
		{name: "data plan json", rep: dataPlan, output: "json", golden: "plan-data.json"},
		{name: "applied but missing table", rep: missing, output: "table", golden: "plan-missing.txt"},
		{name: "applied but missing json", rep: missing, output: "json", golden: "plan-missing.json"},
		{name: "two failed checks table", rep: checks, output: "table", golden: "apply-checks.txt"},
		{name: "unknown runner and date table", rep: unknown, output: "table", golden: "apply-unknown.txt"},
		{name: "unknown runner and date json", rep: unknown, output: "json", golden: "apply-unknown.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The metadata every report has.
			if tc.rep.Version == "" {
				tc.rep.Version = "0.1.0"
				tc.rep.StartedAt = time.Date(2026, 11, 8, 10, 30, 0, 125e6, time.UTC)
			}
			if tc.rep.Database == "" {
				tc.rep.Database = "app"
			}
			var out bytes.Buffer
			if err := writeReport(&out, tc.rep, tc.output); err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "report", tc.golden)
			if *updateGolden {
				if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != string(want) {
				t.Errorf("report:\n%s\nwant:\n%s", out.String(), want)
			}
		})
	}
}

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

// runnerFixtures is the directory of the runner's fixtures testdata/<sub>.
func runnerFixtures(sub string) string { return filepath.Join("..", "runner", "testdata", sub) }

// migrationsDir copies the runner's data migration fixtures ids into a new
// directory.
func migrationsDir(t *testing.T, ids ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, id := range ids {
		addMigration(t, dir, runnerFixtures("data"), id)
	}
	return dir
}

// addMigration copies the fixture <from>/<id>.json into dir.
func addMigration(t *testing.T, dir, from, id string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(from, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
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

// hasRow reports whether a table report has a data migration row for id with
// status.
func hasRow(table, id, status string) bool {
	for line := range strings.Lines(table) {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == id && f[1] == "data" && f[2] == status {
			return true
		}
	}
	return false
}

// seedItems gives the non-idempotent migration something to change.
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

// TestPlanThenApply runs the approval flow: plan, then apply, which records
// the approver and runner.
func TestPlanThenApply(t *testing.T) {
	uri, db := testTarget(t)
	dir := migrationsDir(t, setFlag, seed)

	out, err := migrune(t, uri, db, "plan", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Nothing was committed") || !hasRow(out, setFlag, "applied") {
		t.Errorf("plan table:\n%s", out)
	}
	if n := len(entries(t, db)); n != 0 {
		t.Fatalf("plan wrote %d changelog entries", n)
	}

	t.Setenv("MIGRUNE_RUNNER_ID", "ci-job-7")
	t.Setenv("MIGRUNE_APPROVED_BY_EMAIL", "ada@example.com")
	_, err = migrune(t, uri, db, "apply", "--dir", dir, "--approved-by-name", "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	es := entries(t, db)
	if len(es) != 2 {
		t.Fatalf("%d changelog entries, want 2", len(es))
	}
	for _, e := range es {
		if e.Status != runner.StatusFinished || e.Runner != "ci-job-7" ||
			e.ApprovedBy != (runner.ApprovedBy{Name: "Ada Lovelace", Email: "ada@example.com"}) {
			t.Errorf("entry %s: status %s runner %q approvedBy %+v", e.MigrationID, e.Status, e.Runner, e.ApprovedBy)
		}
	}
}

// TestAppliedButMissing checks plan fails before running anything when a
// Finished migration's file is gone: the JSON report skips every row and
// names the ids, the changelog is left alone, and the
// error isn't a usage error. The runner's TestAppliedButMissing covers apply
// and --mutable.
func TestAppliedButMissing(t *testing.T) {
	uri, db := testTarget(t)
	dir := migrationsDir(t, setFlag)
	if _, err := migrune(t, uri, db, "apply", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, setFlag+".json")); err != nil {
		t.Fatal(err)
	}
	addMigration(t, dir, runnerFixtures("data"), seed)
	before := entries(t, db)
	want := appliedContext(t, before, setFlag)

	out, err := migrune(t, uri, db, "plan", "--dir", dir, "-o", "json")
	var missing *runner.AppliedMissingError
	if !errors.As(err, &missing) || errors.As(err, new(*UsageError)) {
		t.Fatalf("plan: %v, want an applied-but-missing failure naming %s", err, setFlag)
	}
	var rep runner.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("report isn't JSON: %v\n%s", err, out)
	}
	if len(rep.Migrations) != 1 || rep.Migrations[0].ID != seed || rep.Migrations[0].Status != runner.RowSkipped {
		t.Errorf("rows %+v, want %s skipped", rep.Migrations, seed)
	}
	if len(rep.AppliedButMissing) != 1 || !sameApplied(rep.AppliedButMissing[0], want) || rep.Error != err.Error() {
		t.Errorf("appliedButMissing %+v error %q, want [%+v]", rep.AppliedButMissing, rep.Error, want)
	}
	if after := entries(t, db); len(after) != len(before) {
		t.Errorf("%d changelog entries, had %d", len(after), len(before))
	}
	if n, err := db.Collection("settings").CountDocuments(context.Background(), bson.D{}); err != nil || n != 0 {
		t.Errorf("seed ran: %d settings, %v", n, err)
	}
}

// appliedContext returns what the changelog in entries stored about the
// Finished migration id, as plan, apply and lint report it.
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

// sameApplied compares a and b, times as instants, ignoring the checksum.
func sameApplied(a, b migration.Applied) bool {
	return a.ID == b.ID && a.Description == b.Description && a.Runner == b.Runner && a.AppliedAt.Equal(b.AppliedAt)
}

// TestApplyDatedAhead checks a file's date doesn't affect whether it runs: a
// file dated far after today is pending in plan and runs in apply.
func TestApplyDatedAhead(t *testing.T) {
	const later = "2999-01-01-001-later"
	uri, db := testTarget(t)
	seedItems(t, db)
	dir := migrationsDir(t, seed)
	addMigration(t, dir, filepath.Join("testdata", "dated-ahead"), later)

	for _, command := range []string{"plan", "apply"} {
		out, err := migrune(t, uri, db, command, "--dir", dir, "-o", "json")
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		var rep runner.Report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("report isn't JSON: %v\n%s", err, out)
		}
		if len(rep.Migrations) != 2 || rep.Migrations[1].ID != later || rep.Migrations[1].Status != runner.RowApplied ||
			len(rep.AppliedButMissing) != 0 {
			t.Errorf("%s: report rows %+v, want %s pending and run", command, rep.Migrations, later)
		}
		if command == "plan" && !slices.Equal(
			[]string{rep.Migrations[0].ID, rep.Migrations[1].ID}, []string{seed, later}) {
			t.Errorf("plan rows %+v", rep.Migrations)
		}
	}
}

// TestReportJSON checks the JSON report's shape, on a failed run.
func TestReportJSON(t *testing.T) {
	uri, db := testTarget(t)
	seedItems(t, db)
	dir := migrationsDir(t, setFlag, incCounter, seed)
	out, err := migrune(t, uri, db, "apply", "--dir", dir, "-o", "json")
	if err == nil {
		t.Fatal("apply succeeded, want incCounter to fail")
	}
	var rep map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("report isn't JSON: %v\n%s", err, out)
	}
	if got := sortedKeys(rep); !slices.Equal(got, []string{"appliedButMissing", "command", "database", "error",
		"migrations", "newDatabase", "runner", "startedAt", "totals", "version"}) {
		t.Errorf("report keys %v", got)
	}
	var rows []map[string]any
	if err := json.Unmarshal(rep["migrations"], &rows); err != nil || len(rows) != 3 {
		t.Fatalf("migrations: %v %s", err, rep["migrations"])
	}
	want := []struct {
		status string
		keys   []string
	}{
		{"applied", []string{"durationMs", "id", "kind", "status", "steps"}},
		{"errored", []string{"durationMs", "error", "id", "kind", "status", "steps"}},
		{"skipped", []string{"durationMs", "id", "kind", "status"}},
	}
	for i, row := range rows {
		if row["status"] != want[i].status || row["kind"] != "data" || !slices.Equal(sortedKeys(row), want[i].keys) {
			t.Errorf("row %d: %v, want status %s and keys %v", i, row, want[i].status, want[i].keys)
		}
		if _, ok := row["durationMs"].(float64); !ok {
			t.Errorf("row %d durationMs %v is not a number", i, row["durationMs"])
		}
	}
	var totals map[string]any
	if err := json.Unmarshal(rep["totals"], &totals); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(totals); !slices.Equal(got, []string{"applied", "durationMs", "errored", "interrupted", "migrations", "skipped"}) {
		t.Errorf("totals keys %v", got)
	}
	if totals["migrations"] != 3.0 || totals["applied"] != 1.0 || totals["errored"] != 1.0 || totals["skipped"] != 1.0 {
		t.Errorf("totals %v", totals)
	}
	if string(rep["command"]) != `"apply"` || string(rep["appliedButMissing"]) != "[]" {
		t.Errorf("command %s appliedButMissing %s", rep["command"], rep["appliedButMissing"])
	}
}

func sortedKeys[V any](m map[string]V) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TestOnlineLint checks lint --uri --db compares checksums with the
// changelog and reports applied migrations whose file is missing, read-only.
func TestOnlineLint(t *testing.T) {
	uri, db := testTarget(t)
	dir := migrationsDir(t, setFlag, seed)
	if _, err := migrune(t, uri, db, "apply", "--dir", dir); err != nil {
		t.Fatal(err)
	}
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

// TestLockHeld checks apply fails with a lock held error, which exits 3, when
// another runner holds the lock, even under the same --runner-id, and that
// --lock-wait waits for the holder to release it. The runner's TestLockWait
// covers giving up after the wait.
func TestLockHeld(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		release time.Duration // when the holder releases the lock; 0: never
		wantErr bool
	}{
		{name: "same runner id", args: []string{"--runner-id", "deploy"}, wantErr: true},
		{name: "released within the wait", args: []string{"--lock-wait", "30s"}, release: 1500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uri, db := testTarget(t)
			seedItems(t, db)
			holder, err := runner.Start(context.Background(), db, runner.Options{RunnerID: "deploy"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Close() }()
			if tc.release > 0 {
				time.AfterFunc(tc.release, func() { _ = holder.Close() })
			}

			start := time.Now()
			out, err := migrune(t, uri, db, append([]string{"apply", "--dir", migrationsDir(t, setFlag)}, tc.args...)...)
			if !tc.wantErr {
				if err != nil || !hasRow(out, setFlag, "applied") {
					t.Fatalf("apply: %v\n%s", err, out)
				}
				if time.Since(start) < tc.release {
					t.Errorf("apply finished before the holder released the lock")
				}
				return
			}
			var held *runner.LockHeldError
			if !errors.As(err, &held) || held.Owner != "deploy" || errors.As(err, new(*UsageError)) {
				t.Fatalf("apply: %v, want the lock held by deploy", err)
			}
			if out != "" {
				t.Errorf("report %q, want none", out)
			}
			if n := len(entries(t, db)); n != 0 {
				t.Errorf("%d changelog entries", n)
			}
		})
	}
}
