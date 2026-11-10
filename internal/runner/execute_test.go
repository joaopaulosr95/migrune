package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// Data migration fixtures in testdata/data.
const (
	setFlag     = "2026-11-01-001-set-flag"
	incCounter  = "2026-11-01-002-inc-counter"
	seed        = "2026-11-01-003-seed-settings"
	dropStale   = "2026-11-01-004-drop-stale"
	markChecked = "2026-11-01-006-mark-checked"
)

// execute runs Execute with a test logger and checks the lock is released.
func execute(t *testing.T, ctx context.Context, db *mongo.Database, ms []migration.Migration, o Options, c Config) (*Report, error) {
	t.Helper()
	if o.Logger == nil {
		o.Logger = testLogger(t)
	}
	rep, err := Execute(ctx, db, ms, o, c)
	if doc := readLock(t, ctx, db); doc != nil {
		t.Errorf("lock not released after %s: %+v", c.Mode, doc)
	}
	return rep, err
}

// statuses renders the report rows as "id:status".
func statuses(rep *Report) []string {
	var out []string
	for _, r := range rep.Migrations {
		out = append(out, r.ID+":"+string(r.Status))
	}
	return out
}

// changelog returns the changelog entries as "id:status", oldest first.
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

func entryStatuses(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.MigrationID+":"+string(e.Status))
	}
	return out
}

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

func TestExecuteDataMigrations(t *testing.T) {
	cases := []struct {
		mode Mode
		// wantChanged: the data shows the migrations' effects.
		wantChanged bool
		wantEntries []string
	}{
		{mode: ModePlan},
		{mode: ModeApply, wantChanged: true, wantEntries: []string{setFlag + ":Finished", seed + ":Finished", dropStale + ":Finished"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			seedItems(t, ctx, db)
			ms := load(t, fixtureDir(t, "data", setFlag, seed, dropStale))

			rep, err := execute(t, ctx, db, ms, Options{RunnerID: "runner-a"}, Config{Mode: tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{setFlag + ":applied", seed + ":applied", dropStale + ":applied"}
			if got := statuses(rep); !slices.Equal(got, want) {
				t.Errorf("rows %v, want %v", got, want)
			}
			if rep.Command != string(tc.mode) || rep.Runner != "runner-a" || rep.Error != "" ||
				rep.Totals != (Totals{Migrations: 3, Applied: 3, DurationMs: rep.Totals.DurationMs}) {
				t.Errorf("report %+v", rep)
			}
			// The changelog collection didn't exist before the run: the run
			// created it.
			created, err := ChangelogDeployment(ctx, db)
			if err != nil || created == DeploymentNone {
				t.Fatalf("deployment after the run: %q, %v", created, err)
			}
			if want := NewDigestInput(db.Name(), "", created, ms).Sum(); rep.Digest != want {
				t.Errorf("digest %s, want %s", rep.Digest, want)
			}
			// The first run's counts, the same whether committed or not.
			wantSteps := map[string][]string{
				setFlag:   {"0 updateMany items: matched 2, modified 2, upserted 0"},
				seed:      {"0 updateOne settings: matched 0, modified 0, upserted 1"},
				dropStale: {"0 deleteMany items: deleted 1", "1 updateMany items: matched 2, modified 2, upserted 0"},
			}
			for _, row := range rep.Migrations {
				if got := dataSteps(row); !slices.Equal(got, wantSteps[row.ID]) {
					t.Errorf("steps of %s: %v, want %v", row.ID, got, wantSteps[row.ID])
				}
			}

			items := db.Collection("items")
			changed := map[bool]int64{false: 0, true: 1}[tc.wantChanged]
			if n := countDocs(t, ctx, items, bson.D{{Key: "_id", Value: 1}, {Key: "flag", Value: false}}); n != changed {
				t.Errorf("flag set on %d documents, want %d", n, changed)
			}
			if n := countDocs(t, ctx, db.Collection("settings"), bson.D{}); n != changed {
				t.Errorf("%d settings documents, want %d", n, changed)
			}
			if n := countDocs(t, ctx, items, bson.D{{Key: "stale", Value: true}}); n != 1-changed {
				t.Errorf("%d stale items, want %d", n, 1-changed)
			}
			if n := countDocs(t, ctx, items, bson.D{{Key: "label", Value: "a"}}); n != changed {
				t.Errorf("update pipeline labelled %d documents, want %d", n, changed)
			}

			entries := changelog(t, ctx, db)
			if got := entryStatuses(entries); !slices.Equal(got, tc.wantEntries) {
				t.Fatalf("changelog %v, want %v", got, tc.wantEntries)
			}
			if tc.mode == ModeApply {
				// The results are the first run's: what the migration did.
				r := entries[2].Results
				if len(r) != 2 || *r[0].DeletedCount != 1 || *r[1].ModifiedCount != 2 || r[1].MatchedCount == nil {
					t.Errorf("results of %s: %+v", dropStale, r)
				}
				if r := entries[1].Results; len(r) != 1 || *r[0].UpsertedCount != 1 {
					t.Errorf("results of %s: %+v", seed, r)
				}
				// Nothing is pending any more.
				rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan})
				if err != nil || len(rep.Migrations) != 0 || rep.Digest != NewDigestInput(db.Name(), "", created, nil).Sum() {
					t.Errorf("plan after apply: %v, rows %v, digest %s", err, statuses(rep), rep.Digest)
				}
			}
		})
	}
}

// dataSteps renders a data row's steps as "index operation collection:
// counts".
func dataSteps(row Row) []string {
	var out []string
	for _, s := range row.Steps {
		out = append(out, fmt.Sprintf("%d %s %s: %s", s.Index, s.Operation, s.Collection, s.Counts()))
	}
	return out
}

// TestIdempotencyRejected checks the double-run: $inc changes the data
// again on the second run, so plan and apply both fail and nothing is
// committed.
func TestIdempotencyRejected(t *testing.T) {
	cases := []struct {
		mode        Mode
		wantEntries []string
	}{
		{mode: ModePlan},
		{mode: ModeApply, wantEntries: []string{incCounter + ":Errored"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			seedItems(t, ctx, db)
			ms := load(t, fixtureDir(t, "data", incCounter))

			rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: tc.mode})
			var me *MigrationError
			if !errors.As(err, &me) || me.ID != incCounter {
				t.Fatalf("Execute: %v, want a MigrationError for %s", err, incCounter)
			}
			row := rep.Migrations[0]
			if row.Status != RowErrored || !strings.Contains(row.Error, "/steps/0 (updateMany on items, second run): not idempotent") ||
				!strings.Contains(row.Error, "modified 3") {
				t.Errorf("row %+v, want errored as not idempotent", row)
			}
			if got := dataSteps(row); !slices.Equal(got, []string{"0 updateMany items: matched 3, modified 3, upserted 0"}) {
				t.Errorf("steps %v, want the first run's counts", got)
			}
			if rep.Error == "" || rep.Totals.Errored != 1 {
				t.Errorf("report error %q totals %+v", rep.Error, rep.Totals)
			}
			if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "counter", Value: bson.D{{Key: "$exists", Value: true}}}}); n != 0 {
				t.Errorf("%d documents have a counter, want none", n)
			}
			entries := changelog(t, ctx, db)
			if got := entryStatuses(entries); !slices.Equal(got, tc.wantEntries) {
				t.Fatalf("changelog %v, want %v", got, tc.wantEntries)
			}
			if tc.mode == ModeApply {
				e := entries[0]
				if len(e.Errors) != 1 || e.Errors[0].Step == nil || *e.Errors[0].Step != 0 || !strings.Contains(e.Errors[0].Message, "not idempotent") {
					t.Errorf("errors %+v, want step 0 not idempotent", e.Errors)
				}
			}
		})
	}
}

// TestAtomicChangelog checks a failure after the steps, while writing the
// Finished entry, leaves neither the data nor a Finished entry.
func TestAtomicChangelog(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	// The changelog refuses Finished entries: the steps succeed, then the
	// entry's write fails inside the transaction.
	err := db.CreateCollection(ctx, ChangelogCollection, options.CreateCollection().
		SetValidator(bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: StatusFinished}}}}))
	if err != nil {
		t.Fatal(err)
	}
	ms := load(t, fixtureDir(t, "data", setFlag))

	rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
	if !errors.As(err, new(*MigrationError)) || !strings.Contains(err.Error(), "(DocumentValidationFailure, code 121)") {
		t.Fatalf("Execute: %v, want the Finished entry's validation failure", err)
	}
	if got := statuses(rep); !slices.Equal(got, []string{setFlag + ":errored"}) {
		t.Errorf("rows %v", got)
	}
	if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "flag", Value: false}}); n != 0 {
		t.Errorf("the steps' changes were committed to %d documents", n)
	}
	entries := changelog(t, ctx, db)
	if got := entryStatuses(entries); !slices.Equal(got, []string{setFlag + ":Errored"}) {
		t.Fatalf("changelog %v, want only an Errored entry", got)
	}
	if e := entries[0]; len(e.Errors) != 1 || e.Errors[0].Step != nil {
		t.Errorf("errors %+v, want one error not tied to a step", e.Errors)
	}
}

// TestStopAtFirstError checks apply stops at the first failing migration:
// earlier ones stay committed, later ones are skipped.
func TestStopAtFirstError(t *testing.T) {
	cases := []struct {
		name        string
		ids         []string
		wantRows    []string
		wantEntries []string
		wantErr     string
	}{
		{
			name:        "not idempotent",
			ids:         []string{setFlag, incCounter, seed},
			wantRows:    []string{setFlag + ":applied", incCounter + ":errored", seed + ":skipped"},
			wantEntries: []string{setFlag + ":Finished", incCounter + ":Errored"},
			wantErr:     "not idempotent",
		},
		{
			name:        "not idempotent, then an update",
			ids:         []string{setFlag, incCounter, markChecked},
			wantRows:    []string{setFlag + ":applied", incCounter + ":errored", markChecked + ":skipped"},
			wantEntries: []string{setFlag + ":Finished", incCounter + ":Errored"},
			wantErr:     "not idempotent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			seedItems(t, ctx, db)
			ms := load(t, fixtureDir(t, "data", tc.ids...))

			rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute: %v, want %q", err, tc.wantErr)
			}
			if got := statuses(rep); !slices.Equal(got, tc.wantRows) {
				t.Errorf("rows %v, want %v", got, tc.wantRows)
			}
			if rep.Totals != (Totals{Migrations: 3, Applied: 1, Errored: 1, Skipped: 1, DurationMs: rep.Totals.DurationMs}) {
				t.Errorf("totals %+v", rep.Totals)
			}
			if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, tc.wantEntries) {
				t.Errorf("changelog %v, want %v", got, tc.wantEntries)
			}
			if n := countDocs(t, ctx, db.Collection("settings"), bson.D{}); n != 0 {
				t.Errorf("skipped migration wrote %d settings", n)
			}
			if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "checked", Value: true}}); n != 0 {
				t.Errorf("skipped migration checked %d items", n)
			}
		})
	}
}

// TestChecksumImmutable checks a Finished migration can't be edited, unless
// mutable re-runs it, and that one with only Errored entries can.
func TestChecksumImmutable(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	dir := fixtureDir(t, "data", setFlag, incCounter)

	// setFlag is Finished, incCounter Errored.
	if _, err := execute(t, ctx, db, load(t, dir), Options{}, Config{Mode: ModeApply}); err == nil {
		t.Fatal("first apply succeeded, want incCounter to fail")
	}
	edit := func(id, from, to string) {
		t.Helper()
		path := filepath.Join(dir, id+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(raw), from, to, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Fix the Errored one in place: allowed.
	edit(incCounter, `"$inc": { "counter": 1 }`, `"$set": { "counter": 1 }`)
	if _, err := execute(t, ctx, db, load(t, dir), Options{}, Config{Mode: ModeApply}); err != nil {
		t.Fatalf("apply after fixing the Errored migration: %v", err)
	}

	// Edit the Finished one: every run fails before running anything.
	edit(setFlag, "Default flag to false", "Default the flag to false")
	ms := load(t, dir)
	for _, mode := range []Mode{ModePlan, ModeApply} {
		rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: mode})
		var ce *ChecksumError
		if !errors.As(err, &ce) || !slices.Equal(ce.IDs, []string{setFlag}) {
			t.Fatalf("%s: %v, want a ChecksumError for %s", mode, err, setFlag)
		}
		if got := statuses(rep); !slices.Equal(got, []string{setFlag + ":errored"}) || !strings.Contains(rep.Migrations[0].Error, "the file changed") {
			t.Errorf("%s rows %v %+v", mode, got, rep.Migrations)
		}
	}
	before := len(changelog(t, ctx, db))

	// --mutable runs it again and records the new checksum.
	rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply, Mutable: true})
	if err != nil {
		t.Fatalf("mutable apply: %v", err)
	}
	if got := statuses(rep); !slices.Equal(got, []string{setFlag + ":applied"}) {
		t.Errorf("mutable rows %v", got)
	}
	entries := changelog(t, ctx, db)
	if len(entries) != before+1 {
		t.Fatalf("%d changelog entries, want %d", len(entries), before+1)
	}
	if last := entries[len(entries)-1]; last.MigrationID != setFlag || last.Status != StatusFinished || last.Checksum != ms[0].Checksum {
		t.Errorf("last entry %s %s %s, want Finished with checksum %s", last.MigrationID, last.Status, last.Checksum, ms[0].Checksum)
	}
	// The latest Finished entry now matches the file.
	rep, err = execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan})
	if err != nil || len(rep.Migrations) != 0 {
		t.Errorf("plan after the mutable apply: %v, rows %v", err, statuses(rep))
	}
}

// TestAppliedButMissing checks a Finished migration whose file is gone fails
// every run before anything runs, --mutable included, and that the error
// joins the checksum one when a file was also edited.
func TestAppliedButMissing(t *testing.T) {
	cases := []struct {
		name     string
		config   Config
		edit     bool
		wantRows []string
		checksum bool
	}{
		{name: "plan", config: Config{Mode: ModePlan}, wantRows: []string{dropStale + ":skipped"}},
		{name: "apply", config: Config{Mode: ModeApply}, wantRows: []string{dropStale + ":skipped"}},
		{name: "apply mutable", config: Config{Mode: ModeApply, Mutable: true}, edit: true,
			wantRows: []string{seed + ":skipped", dropStale + ":skipped"}},
		{name: "apply with an edited file", config: Config{Mode: ModeApply}, edit: true,
			wantRows: []string{seed + ":errored", dropStale + ":skipped"}, checksum: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			dir := fixtureDir(t, "data", setFlag, seed)
			if _, err := execute(t, ctx, db, load(t, dir), Options{}, Config{Mode: ModeApply}); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, setFlag+".json")); err != nil {
				t.Fatal(err)
			}
			if tc.edit {
				path := filepath.Join(dir, seed+".json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			addFixture(t, dir, "data", dropStale)
			if _, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "stale", Value: true}}); err != nil {
				t.Fatal(err)
			}
			entries := changelog(t, ctx, db)
			before := len(entries)
			var applied Entry
			for _, e := range entries {
				if e.MigrationID == setFlag {
					applied = e
				}
			}
			want := migration.Applied{ID: setFlag, Description: "Default flag to false on items that don't have it",
				AppliedAt: applied.CreatedAt, Runner: applied.Runner}

			rep, err := execute(t, ctx, db, load(t, dir), Options{}, tc.config)
			var me *AppliedMissingError
			if !errors.As(err, &me) {
				t.Fatalf("Execute: %v, want an AppliedMissingError for %s", err, setFlag)
			}
			if !sameApplied(me.Migrations, want) {
				t.Errorf("error migrations %+v, want %+v", me.Migrations, want)
			}
			if got := errors.As(err, new(*ChecksumError)); got != tc.checksum {
				t.Errorf("ChecksumError joined: %v, want %v", got, tc.checksum)
			}
			if !sameApplied(rep.AppliedButMissing, want) {
				t.Errorf("applied but missing %+v, want [%+v]", rep.AppliedButMissing, want)
			}
			if got := statuses(rep); !slices.Equal(got, tc.wantRows) {
				t.Errorf("rows %v, want %v", got, tc.wantRows)
			}
			if rep.Error != err.Error() {
				t.Errorf("report error %q, want %q", rep.Error, err)
			}
			if n := len(changelog(t, ctx, db)); n != before {
				t.Errorf("%d changelog entries, had %d", n, before)
			}
			if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "stale", Value: true}}); n != 1 {
				t.Errorf("%d stale items, want 1: drop-stale ran", n)
			}
		})
	}
}

// TestApprovedByAndRunner checks Finished entries record who approved the
// run and which runner applied it.
func TestApprovedByAndRunner(t *testing.T) {
	approver := ApprovedBy{Name: "Ada Lovelace", Email: "ada@example.com"}
	cases := []struct {
		name     string
		runnerID string
		want     *regexp.Regexp
	}{
		{name: "given", runnerID: "ci-job-42", want: regexp.MustCompile(`^ci-job-42$`)},
		{name: "default", want: regexp.MustCompile(`^.+-\d+-[A-Z2-7]{8}$`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			ms := load(t, fixtureDir(t, "data", seed))
			rep, err := execute(t, ctx, db, ms, Options{RunnerID: tc.runnerID, ApprovedBy: approver}, Config{Mode: ModeApply})
			if err != nil {
				t.Fatal(err)
			}
			entries := changelog(t, ctx, db)
			if len(entries) != 1 {
				t.Fatalf("%d entries, want 1", len(entries))
			}
			e := entries[0]
			if e.ApprovedBy != approver || !tc.want.MatchString(e.Runner) || e.Runner != rep.Runner {
				t.Errorf("entry approvedBy %+v runner %q, report runner %q", e.ApprovedBy, e.Runner, rep.Runner)
			}
			if rep.ApprovedBy == nil || *rep.ApprovedBy != approver {
				t.Errorf("report approvedBy %+v, want %+v", rep.ApprovedBy, approver)
			}
		})
	}
}

// TestNewDatabase checks a run against a database with no collections, as a
// mistyped --db would target, reports it as new, even after a plan left the
// lock collection, and one against a database with a collection doesn't.
func TestNewDatabase(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	ms := load(t, fixtureDir(t, "data", seed))
	for _, mode := range []Mode{ModePlan, ModeApply} {
		rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: mode})
		if err != nil || !rep.NewDatabase || rep.ApprovedBy != nil {
			t.Fatalf("%s on an empty database: %v, newDatabase %v, approvedBy %+v", mode, err, rep.NewDatabase, rep.ApprovedBy)
		}
	}
	// apply wrote the changelog and the seed.
	rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan})
	if err != nil || rep.NewDatabase {
		t.Fatalf("plan on a database with collections: %v, newDatabase %v", err, rep.NewDatabase)
	}
}

// TestDigestCheck checks apply runs nothing when the pending set doesn't match
// the plan's digest.
func TestDigestCheck(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	dir := fixtureDir(t, "data", setFlag)
	planned, err := execute(t, ctx, db, load(t, dir), Options{}, Config{Mode: ModePlan})
	if err != nil {
		t.Fatal(err)
	}
	addFixture(t, dir, "data", seed)
	ms := load(t, dir)

	rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply, Digest: planned.DigestInput})
	var dm *DigestMismatchError
	deployment := planned.DigestInput.Deployment
	if !errors.As(err, &dm) || dm.Plan != planned.DigestInput || dm.Run.Sum() != NewDigestInput(db.Name(), "", deployment, ms).Sum() ||
		!slices.Equal(dm.Differences(), []string{"pending since the plan: " + seed}) {
		t.Fatalf("apply: %v, want a DigestMismatchError naming %s", err, seed)
	}
	if got := statuses(rep); !slices.Equal(got, []string{setFlag + ":skipped", seed + ":skipped"}) {
		t.Errorf("rows %v, want all skipped", got)
	}
	if n := countDocs(t, ctx, db.Collection(ChangelogCollection), bson.D{}); n != 0 {
		t.Errorf("%d changelog entries, want none", n)
	}

	// The refused apply left the changelog collection plan created.
	if got, err := ChangelogDeployment(ctx, db); err != nil || got != deployment {
		t.Fatalf("deployment after the refused apply: %q, %v, want %s", got, err, deployment)
	}
	if _, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply, Digest: NewDigestInput(db.Name(), "", deployment, ms)}); err != nil {
		t.Fatalf("apply with the matching digest: %v", err)
	}
}

// TestDigestDeployment checks the digest names the deployment, the UUID of
// the changelog collection, which plan creates, so even the first plan has
// one. Two deployments' first plans, with the same database and replica set
// names (one deployment, whose database is dropped in between, stands for
// both here), get different digests, and apply refuses the other's,
// naming the deployment first.
func TestDigestDeployment(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	ms := load(t, fixtureDir(t, "data", setFlag))
	plan := func() *DigestInput {
		t.Helper()
		rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan, ReplicaSet: "rs0"})
		if err != nil {
			t.Fatal(err)
		}
		return rep.DigestInput
	}
	first := plan()
	if !deploymentPattern.MatchString(first.Deployment) || !strings.Contains(first.Text(), "\ndeployment "+first.Deployment+"\n") {
		t.Fatalf("first plan's digest:\n%s", first.Text())
	}
	if deployment, err := ChangelogDeployment(ctx, db); err != nil || deployment != first.Deployment {
		t.Fatalf("deployment after plan: %q, %v, want %s", deployment, err, first.Deployment)
	}
	if again := plan(); again.Sum() != first.Sum() {
		t.Errorf("the same deployment gives another digest:\n%s\nthen:\n%s", first.Text(), again.Text())
	}
	if err := db.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	second := plan()
	if second.Deployment == first.Deployment || second.Database != first.Database || second.ReplicaSet != first.ReplicaSet ||
		second.Sum() == first.Sum() {
		t.Fatalf("another deployment's first plan, same names: digest\n%s\nthen:\n%s", first.Text(), second.Text())
	}

	_, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply, ReplicaSet: "rs0", Digest: first})
	var dm *DigestMismatchError
	if !errors.As(err, &dm) || !slices.Equal(dm.Differences(), []string{"the plan was made against another deployment " +
		"(this run's migrations_changelog collection is " + second.Deployment + ", the plan's was " + first.Deployment + ")"}) {
		t.Fatalf("apply with the old plan's digest: %v, want a deployment mismatch", err)
	}
}

// sameApplied reports whether got is just want, comparing times as instants.
func sameApplied(got []migration.Applied, want migration.Applied) bool {
	if len(got) != 1 {
		return false
	}
	g := got[0]
	return g.ID == want.ID && g.Description == want.Description && g.Runner == want.Runner &&
		g.Checksum != "" && !want.AppliedAt.IsZero() && g.AppliedAt.Equal(want.AppliedAt)
}

// TestWithRelease checks a failed lock release fails the run only when the run
// failed anyway.
func TestWithRelease(t *testing.T) {
	runErr := errors.New("migration x failed")
	releaseErr := errors.New("release migration lock: connection refused")
	cases := []struct {
		name         string
		run, release error
		want         []error // errors the result must match; none: nil
	}{
		{name: "both succeed"},
		{name: "run fails", run: runErr, want: []error{runErr}},
		{name: "release fails after success", release: releaseErr},
		{name: "both fail", run: runErr, release: releaseErr, want: []error{runErr, releaseErr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withRelease(tc.run, tc.release, discard(), false)
			if (got == nil) != (len(tc.want) == 0) {
				t.Fatalf("withRelease = %v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !errors.Is(got, w) {
					t.Errorf("withRelease = %v, want it to match %v", got, w)
				}
			}
		})
	}
}

// TestSharedCollectionWarning checks a run warns about each pending
// migration writing a collection another namespace's migrations wrote, and
// only then.
func TestSharedCollectionWarning(t *testing.T) {
	const warning = "a pending migration writes a collection another namespace's migrations wrote"
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	if _, err := execute(t, ctx, db, load(t, fixtureDir(t, "data", setFlag)), Options{Namespace: "api"}, Config{Mode: ModeApply}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		namespace string
		ids       []string
		want      []string // the collection of each warning
	}{
		{name: "default namespace writes items", ids: []string{setFlag, seed}, want: []string{"items"}},
		{name: "other collection", ids: []string{seed}},
		{name: "same namespace", namespace: "api", ids: []string{setFlag, seed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			o := Options{Namespace: tc.namespace, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			if _, err := execute(t, ctx, db, load(t, fixtureDir(t, "data", tc.ids...)), o, Config{Mode: ModePlan}); err != nil {
				t.Fatal(err)
			}
			var got []string
			for line := range strings.Lines(logs.String()) {
				var rec struct{ Msg, Collection, OtherNamespaces string }
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(rec.Msg, warning) {
					got = append(got, rec.Collection)
					if rec.OtherNamespaces != "api" {
						t.Errorf("otherNamespaces %q, want api", rec.OtherNamespaces)
					}
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("warnings for %v, want %v:\n%s", got, tc.want, logs.String())
			}
		})
	}
}
