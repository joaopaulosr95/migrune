package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// Index migration fixtures in testdata/index.
const (
	createName  = "2026-11-02-001-name-index"
	dropMissing = "2026-11-02-002-drop-missing"
	nameCI      = "2026-11-02-003-name-ci"
	conflict    = "2026-11-02-004-conflict"
	emailIndex  = "2026-11-02-005-email-index"
	dropName    = "2026-11-02-006-drop-name-index"
	nameDesc    = "2026-11-02-007-name-index-descending"
	sessionsTTL = "2026-11-02-008-sessions-ttl"
)

// indexNames returns the names of coll's indexes, sorted.
func indexNames(t *testing.T, ctx context.Context, coll *mongo.Collection) []string {
	t.Helper()
	ixs, err := listIndexes(ctx, coll)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ix := range ixs {
		names = append(names, ix.Name)
	}
	slices.Sort(names)
	return names
}

func TestPlanCreate(t *testing.T) {
	d := func(kv ...any) bson.D {
		var out bson.D
		for i := 0; i < len(kv); i += 2 {
			out = append(out, bson.E{Key: kv[i].(string), Value: kv[i+1]})
		}
		return out
	}
	ix := func(name string, keys bson.D, opts ...any) migration.Index {
		return migration.Index{Collection: "items", Name: name, Keys: keys, Options: d(opts...)}
	}
	// As listIndexes returns them: numbers as given, collations expanded,
	// text indexes with internal keys.
	live := []migration.Index{
		ix("_id_", d("_id", int32(1))),
		ix("name_1", d("name", int32(1))),
		ix("email_1", d("email", int64(1)), "unique", true, "sparse", false),
		ix("label_ci", d("label", 1.0), "collation", d(
			"locale", "en", "caseLevel", false, "caseFirst", "off", "strength", int32(2),
			"numericOrdering", false, "alternate", "non-ignorable", "maxVariable", "punct",
			"normalization", false, "backwards", false, "version", "57.1")),
		ix("body_text", d("_fts", "text", "_ftsx", int32(1)),
			"weights", d("body", int32(1)), "default_language", "english", "language_override", "language", "textIndexVersion", int32(3)),
		ix("hidden_1", d("hidden", int32(1)), "hidden", true),
		ix("all", d("$**", int32(1)), "wildcardProjection", d("payload", d("id", int32(1)))),
		ix("loc_2d", d("loc", "2d"), "bits", int32(20), "min", int32(-1000), "max", int32(1000)),
		ix("area_2dsphere", d("area", "2dsphere"), "2dsphereIndexVersion", int32(4)),
		ix("at_1", d("at", int32(1)), "storageEngine", d("wiredTiger", d("configString", "block_compressor=zlib"))),
	}
	cases := []struct {
		name string
		want migration.Index
		out  IndexAction
		// warn and err are substrings of the warning and the error.
		warn, err string
		// before8 plans against a server that ignores hidden for an index
		// of the same name.
		before8 bool
	}{
		{name: "new name and keys", want: ix("age_1", d("age", int32(1))), out: IndexCreate},
		{name: "same definition", want: ix("name_1", d("name", 1.0)), out: IndexPresent},
		{name: "same definition, options spelled differently", want: ix("email_1", d("email", int32(1)), "unique", true), out: IndexPresent},
		{name: "a collation that isn't the existing one in full can't be compared",
			want: ix("label_ci", d("label", int32(1)), "collation", d("locale", "en", "strength", int32(2))),
			out:  IndexDiffers, warn: "plan can't tell whether MongoDB sees them as the same, so apply may fail (IndexKeySpecsConflict)"},
		{name: "another locale is another collation",
			want: ix("label_ci", d("label", int32(1)), "collation", d("locale", "fr")),
			err:  `already exists with a different definition (collation {"locale": "fr"}`},
		{name: "another strength is another collation",
			want: ix("label_ci", d("label", int32(1)), "collation", d("locale", "en", "strength", int32(1))),
			err:  "IndexKeySpecsConflict"},
		{name: "the simple collation is no collation",
			want: ix("label_ci", d("label", int32(1)), "collation", d("locale", "simple")),
			err:  "(collation none, existing {"},
		{name: "a text index with the default weights and language", want: ix("body_text", d("body", "text")), out: IndexPresent},
		{name: "a text index with the same weights spelled out",
			want: ix("body_text", d("body", "text"), "weights", d("body", 1.0), "default_language", "english"), out: IndexPresent},
		{name: "a text index with other weights", want: ix("body_text", d("body", "text"), "weights", d("body", int32(5))),
			err: `(weights {"body": 5}, existing {"body": 1})`},
		{name: "a text index with another default language", want: ix("body_text", d("body", "text"), "default_language", "french"),
			err: `(default_language "french", existing "english")`},
		{name: "a text index with another language_override", want: ix("body_text", d("body", "text"), "language_override", "lang"),
			err: `(language_override "lang", existing "language")`},
		{name: "a wildcard index with the same projection, spelled differently",
			want: ix("all", d("$**", int32(1)), "wildcardProjection", d("payload.id", true, "_id", int32(0))), out: IndexPresent},
		{name: "a wildcard index with another projection", want: ix("all", d("$**", int32(1)), "wildcardProjection", d("payload.kind", int32(1))),
			err: `(wildcardProjection {"payload.kind": 1}, existing {"payload.id": 1})`},
		{name: "a wildcard index without the projection", want: ix("all", d("$**", int32(1))),
			err: `(wildcardProjection none, existing {"payload.id": 1})`},
		{name: "a wildcard index under another name, another projection",
			want: ix("all_kind", d("$**", int32(1)), "wildcardProjection", d("payload.kind", int32(1))), out: IndexCreate},
		{name: "a wildcard index under another name, the same projection",
			want: ix("all_again", d("$**", int32(1)), "wildcardProjection", d("payload", d("id", 1.0))), err: "IndexOptionsConflict"},
		{name: "a 2d index without its bits, min and max", want: ix("loc_2d", d("loc", "2d")), err: "(bits none, existing 20)"},
		{name: "a 2d index with the same bounds as other numbers",
			want: ix("loc_2d", d("loc", "2d"), "bits", int64(20), "min", -1000.0, "max", int64(1000)), out: IndexPresent},
		{name: "a 2d index under another name, other bits", want: ix("loc_2d_fine", d("loc", "2d"), "bits", int32(26)), err: "IndexOptionsConflict"},
		{name: "a 2dsphere index of another version", want: ix("area_2dsphere", d("area", "2dsphere"), "2dsphereIndexVersion", int32(2)), out: IndexPresent},
		{name: "an index without its storageEngine", want: ix("at_1", d("at", int32(1)), "background", true),
			err: "(storageEngine none, existing {"},
		{name: "a text index on other fields", want: ix("body_text", d("title", "text")),
			err: `(weights {"title": 1}, existing {"body": 1})`},
		{name: "name taken by other keys", want: ix("name_1", d("name", int32(-1))),
			err: "index name_1 already exists with a different definition (keys"},
		{name: "name taken by other options", want: ix("name_1", d("name", int32(1)), "unique", true),
			err: "(unique true, existing none)"},
		{name: "same keys and options under another name", want: ix("by_name", d("name", int32(1))),
			err: "same keys and options as the existing index name_1"},
		{name: "same keys under another name with a distinct option",
			want: ix("name_unique", d("name", int32(1)), "unique", true), out: IndexCreate},
		{name: "same keys under another name and a different TTL",
			want: ix("name_ttl", d("name", int32(1)), "expireAfterSeconds", int32(60)), err: "IndexOptionsConflict"},
		{name: "same keys under another name and a collation plan can't compare",
			want: ix("label_ci_2", d("label", int32(1)), "collation", d("locale", "en", "strength", int32(2))),
			out:  IndexCreate, warn: "if MongoDB sees them as the same, apply fails (IndexOptionsConflict)"},
		{name: "same keys under another name and another strength",
			want: ix("label_ci_1", d("label", int32(1)), "collation", d("locale", "en", "strength", int32(1))), out: IndexCreate},
		{name: "same keys under another name without a collation", want: ix("label_1", d("label", int32(1))), out: IndexCreate},
		{name: "name taken by a hidden index", want: ix("hidden_1", d("hidden", int32(1))),
			err: "(hidden none, existing true)"},
		{name: "name taken by a hidden index, before 8.0", want: ix("hidden_1", d("hidden", int32(1))), out: IndexPresent, before8: true},
		{name: "a hidden index on a visible one's name, before 8.0", want: ix("name_1", d("name", int32(1)), "hidden", true), out: IndexPresent, before8: true},
		{name: "same keys under another name, hidden, before 8.0", want: ix("by_name", d("name", int32(1)), "hidden", true),
			err: "same keys and options as the existing index name_1", before8: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, warn, err := planCreate(tc.want, live, !tc.before8)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("planCreate = %q, %v, want an error containing %q", out, err, tc.err)
				}
				return
			}
			if err != nil || out != tc.out || !strings.Contains(warn, tc.warn) || (tc.warn == "") != (warn == "") {
				t.Fatalf("planCreate = %q, %q, %v, want %q with a warning containing %q", out, warn, err, tc.out, tc.warn)
			}
		})
	}
}

// TestIndexCreateThenRerun applies an index migration, then runs it again:
// once Finished it isn't pending, and with its changelog entry gone (a lost
// entry, as after a crash) the identical createIndex is a no-op.
func TestIndexCreateThenRerun(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	ms := load(t, fixtureDir(t, "index", createName))

	rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(rep); !slices.Equal(got, []string{createName + ":applied"}) || rep.Migrations[0].Kind != "index" {
		t.Errorf("rows %+v", rep.Migrations)
	}
	if got := indexNames(t, ctx, db.Collection("items")); !slices.Equal(got, []string{"_id_", "name_1"}) {
		t.Errorf("indexes %v", got)
	}
	entries := changelog(t, ctx, db)
	if len(entries) != 1 || entries[0].Status != StatusFinished {
		t.Fatalf("changelog %v", entryStatuses(entries))
	}
	want := []StepResult{{Collection: "items", Operation: "createIndex", IndexName: "name_1"}}
	if !reflect.DeepEqual(entries[0].Results, want) {
		t.Errorf("results %+v, want %+v", entries[0].Results, want)
	}

	rep, err = execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
	if err != nil || len(rep.Migrations) != 0 {
		t.Fatalf("second apply: %v, rows %v, want nothing pending", err, statuses(rep))
	}

	if _, err := db.Collection(ChangelogCollection).DeleteMany(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	rep, err = execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
	if err != nil || !slices.Equal(statuses(rep), []string{createName + ":applied"}) {
		t.Fatalf("re-run: %v, rows %v", err, statuses(rep))
	}
	if got := indexNames(t, ctx, db.Collection("items")); !slices.Equal(got, []string{"_id_", "name_1"}) {
		t.Errorf("indexes after the re-run %v", got)
	}
	if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{createName + ":Finished"}) {
		t.Errorf("changelog after the re-run %v", got)
	}
}

// TestIndexDropMissing checks dropping an index that doesn't exist, on an
// existing collection and on a missing one, counts as success, in plan and
// apply.
func TestIndexDropMissing(t *testing.T) {
	for _, mode := range []Mode{ModePlan, ModeApply} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			seedItems(t, ctx, db)
			ms := load(t, fixtureDir(t, "index", dropMissing))
			rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: mode})
			if err != nil || !slices.Equal(statuses(rep), []string{dropMissing + ":applied"}) {
				t.Fatalf("%s: %v, rows %v", mode, err, statuses(rep))
			}
			var want []string
			if mode == ModeApply {
				want = []string{dropMissing + ":Finished"}
			}
			if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, want) {
				t.Errorf("changelog %v, want %v", got, want)
			}
		})
	}
}

// indexLog is one "index ..." log line of a plan or apply.
type indexLog struct {
	Msg   string `json:"msg"`
	Step  int    `json:"step"`
	Index string `json:"index"`
}

// indexLogs runs Execute with a JSON logger and returns its index log lines.
func indexLogs(t *testing.T, ctx context.Context, db *mongo.Database, ms []string, c Config) (*Report, []string, error) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	rep, err := execute(t, ctx, db, load(t, fixtureDir(t, "index", ms...)), Options{Logger: logger}, c)
	t.Logf("logs:\n%s", buf.String())
	var out []string
	for line := range strings.Lines(buf.String()) {
		var l indexLog
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if strings.HasPrefix(l.Msg, "index ") {
			out = append(out, l.Msg+": "+l.Index)
		}
	}
	return rep, out, err
}

// reportedIndexes renders the index changes of rep's row id as
// "action: collection.name keys options [conflict]".
func reportedIndexes(rep *Report, id string) []string {
	row := rep.row(id)
	if row == nil {
		return nil
	}
	var out []string
	for _, c := range row.Indexes {
		line := string(c.Action) + ": " + c.Collection + "." + c.Name
		for _, part := range []string{string(c.Keys), string(c.Options), c.Conflict} {
			if part != "" {
				line += " " + part
			}
		}
		out = append(out, line)
	}
	return out
}

// TestIndexPlanDiff checks plan logs and reports the index diff step by step
// against listIndexes, writes nothing, and fails a step MongoDB would reject.
func TestIndexPlanDiff(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	items := db.Collection("items")
	if _, err := execute(t, ctx, db, load(t, fixtureDir(t, "index", createName)), Options{}, Config{Mode: ModeApply}); err != nil {
		t.Fatal(err)
	}

	rep, diff, err := indexLogs(t, ctx, db, []string{createName, nameCI}, Config{Mode: ModePlan})
	if err != nil || !slices.Equal(statuses(rep), []string{nameCI + ":applied"}) {
		t.Fatalf("plan: %v, rows %v", err, statuses(rep))
	}
	want := []string{
		"index already present: name_1",
		"index to create: name_1_ci",
		"index to drop: name_1",
		"index already absent: stale_1",
	}
	if !slices.Equal(diff, want) {
		t.Errorf("plan diff:\n  %s\nwant:\n  %s", strings.Join(diff, "\n  "), strings.Join(want, "\n  "))
	}
	ciOptions := `{"unique":true,"collation":{"locale":"en","strength":2}}`
	wantReport := []string{
		`present: items.name_1 {"name":1}`,
		`create: items.name_1_ci {"name":1} ` + ciOptions,
		`drop: items.name_1 {"name":1}`,
		"absent: items.stale_1",
	}
	if got := reportedIndexes(rep, nameCI); !slices.Equal(got, wantReport) {
		t.Errorf("plan report:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(wantReport, "\n  "))
	}
	if got := indexNames(t, ctx, items); !slices.Equal(got, []string{"_id_", "name_1"}) {
		t.Errorf("plan changed the indexes: %v", got)
	}
	if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{createName + ":Finished"}) {
		t.Errorf("plan wrote to the changelog: %v", got)
	}

	// apply logs and reports what it did, step by step.
	rep, diff, err = indexLogs(t, ctx, db, []string{createName, nameCI}, Config{Mode: ModeApply})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"index already present: name_1",
		"index created: name_1_ci",
		"index dropped: name_1",
		"index already absent: stale_1",
	}
	if !slices.Equal(diff, want) {
		t.Errorf("apply:\n  %s\nwant:\n  %s", strings.Join(diff, "\n  "), strings.Join(want, "\n  "))
	}
	if got := reportedIndexes(rep, nameCI); !slices.Equal(got, wantReport) {
		t.Errorf("apply report:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(wantReport, "\n  "))
	}
	if got := indexNames(t, ctx, items); !slices.Equal(got, []string{"_id_", "name_1_ci"}) {
		t.Errorf("indexes after apply: %v", got)
	}

	// Planned again, the server's expanded collation isn't the one the
	// file gives field for field: plan can't tell they are the same, and
	// warns instead of guessing the defaults.
	if _, err := db.Collection(ChangelogCollection).DeleteMany(ctx, bson.D{{Key: "migrationId", Value: nameCI}}); err != nil {
		t.Fatal(err)
	}
	_, diff, err = indexLogs(t, ctx, db, []string{createName, nameCI}, Config{Mode: ModePlan})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"index to create: name_1",
		"index already present with a collation plan can't compare: apply may fail: name_1_ci",
		"index to drop: name_1",
		"index already absent: stale_1",
	}
	if !slices.Equal(diff, want) {
		t.Errorf("second plan:\n  %s\nwant:\n  %s", strings.Join(diff, "\n  "), strings.Join(want, "\n  "))
	}

	// A step MongoDB would reject fails the plan, and nothing is built.
	if _, err := items.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "name", Value: 1}}}); err != nil {
		t.Fatal(err)
	}
	rep, _, err = indexLogs(t, ctx, db, []string{createName, conflict}, Config{Mode: ModePlan})
	if err == nil || !strings.Contains(err.Error(), "IndexKeySpecsConflict") {
		t.Fatalf("plan of a conflicting index: %v", err)
	}
	if got := statuses(rep); !slices.Equal(got, []string{conflict + ":errored"}) ||
		!strings.Contains(rep.Migrations[0].Error, "/steps/1 (createIndex on items)") {
		t.Errorf("rows %+v", rep.Migrations)
	}
	got := reportedIndexes(rep, conflict)
	if len(got) != 2 || got[0] != `create: items.createdAt_-1 {"createdAt":-1}` ||
		!strings.HasPrefix(got[1], `create: items.name_1 {"name":-1} index name_1 already exists with a different definition`) {
		t.Errorf("conflict report:\n  %s", strings.Join(got, "\n  "))
	}
	if got := indexNames(t, ctx, items); !slices.Equal(got, []string{"_id_", "name_1", "name_1_ci"}) {
		t.Errorf("plan built an index: %v", got)
	}
}

// TestIndexPlanCarriesState checks plan carries the indexes across pending
// migrations: one dropping name_1 and a later one recreating it with other
// keys plan without a false conflict, as apply then runs them.
func TestIndexPlanCarriesState(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	if _, err := execute(t, ctx, db, load(t, fixtureDir(t, "index", createName)), Options{}, Config{Mode: ModeApply}); err != nil {
		t.Fatal(err)
	}
	ids := []string{createName, dropName, nameDesc}

	rep, diff, err := indexLogs(t, ctx, db, ids, Config{Mode: ModePlan})
	if err != nil || !slices.Equal(statuses(rep), []string{dropName + ":applied", nameDesc + ":applied"}) {
		t.Fatalf("plan: %v, rows %v", err, statuses(rep))
	}
	want := []string{"index to drop: name_1", "index to create: name_1", "index to create: label_1"}
	if !slices.Equal(diff, want) {
		t.Errorf("plan:\n  %s\nwant:\n  %s", strings.Join(diff, "\n  "), strings.Join(want, "\n  "))
	}
	if got := indexNames(t, ctx, db.Collection("items")); !slices.Equal(got, []string{"_id_", "name_1"}) {
		t.Errorf("plan changed the indexes: %v", got)
	}

	if _, err := execute(t, ctx, db, load(t, fixtureDir(t, "index", ids...)), Options{}, Config{Mode: ModeApply}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := indexNames(t, ctx, db.Collection("items")); !slices.Equal(got, []string{"_id_", "label_1", "name_1"}) {
		t.Errorf("indexes after apply: %v", got)
	}
}

// TestIndexWouldExpire checks plan counts, for each TTL index it would
// create, the documents the index would delete as soon as it is built, and
// apply doesn't.
func TestIndexWouldExpire(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	old, recent := time.Now().Add(-2*time.Hour), time.Now().Add(-time.Minute)
	insert(t, ctx, db.Collection("sessions"),
		bson.D{{Key: "lastUsedAt", Value: old}},
		bson.D{{Key: "lastUsedAt", Value: old}},
		bson.D{{Key: "lastUsedAt", Value: recent}},
		bson.D{{Key: "lastUsedAt", Value: "yesterday"}},
		bson.D{{Key: "user", Value: "no date"}})
	insert(t, ctx, db.Collection("carts"),
		bson.D{{Key: "updatedAt", Value: old}, {Key: "guest", Value: true}},
		bson.D{{Key: "updatedAt", Value: old}, {Key: "guest", Value: false}},
		bson.D{{Key: "updatedAt", Value: recent}, {Key: "guest", Value: true}})
	ms := load(t, fixtureDir(t, "index", sessionsTTL))
	wouldExpire := func(rep *Report) []string {
		var out []string
		for _, c := range rep.row(sessionsTTL).Indexes {
			n := "-"
			if c.WouldExpire != nil {
				n = strconv.FormatInt(*c.WouldExpire, 10)
			}
			out = append(out, string(c.Action)+" "+c.Name+": "+n)
		}
		return out
	}

	rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"create lastUsedAt_ttl: 2", "create guest_updatedAt_ttl: 1", "create updatedAt_-1: -"}
	if got := wouldExpire(rep); !slices.Equal(got, want) {
		t.Errorf("plan: %v, want %v", got, want)
	}
	rep, err = execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"create lastUsedAt_ttl: -", "create guest_updatedAt_ttl: -", "create updatedAt_-1: -"}
	if got := wouldExpire(rep); !slices.Equal(got, want) {
		t.Errorf("apply: %v, want %v", got, want)
	}
}

// TestIndexChangelogAfterSteps checks an index migration whose step fails
// gets an Errored entry and no Finished one, and stops the run.
// The steps before the failure stay done: a re-run is harmless.
func TestIndexChangelogAfterSteps(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	ms := load(t, fixtureDir(t, "index", createName, conflict, emailIndex))

	rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
	var me *MigrationError
	if !errors.As(err, &me) || me.ID != conflict || !strings.Contains(err.Error(), "IndexKeySpecsConflict") {
		t.Fatalf("apply: %v, want %s to fail", err, conflict)
	}
	want := []string{createName + ":applied", conflict + ":errored", emailIndex + ":skipped"}
	if got := statuses(rep); !slices.Equal(got, want) {
		t.Errorf("rows %v, want %v", got, want)
	}
	entries := changelog(t, ctx, db)
	if got := entryStatuses(entries); !slices.Equal(got, []string{createName + ":Finished", conflict + ":Errored"}) {
		t.Fatalf("changelog %v", got)
	}
	errored := entries[1]
	if len(errored.Errors) != 1 || errored.Errors[0].Step == nil || *errored.Errors[0].Step != 1 {
		t.Errorf("errors %+v, want one on step 1", errored.Errors)
	}
	if got := indexNames(t, ctx, db.Collection("items")); !slices.Equal(got, []string{"_id_", "createdAt_-1", "name_1"}) {
		t.Errorf("indexes %v", got)
	}
}

// holdIndexBuilds makes the primary's index builds hang while they scan a
// document with this marker, until release is called. It needs the server's
// test commands, which test/replicaset enables. Builds of other collections
// aren't affected.
func holdIndexBuilds(t *testing.T, ctx context.Context, client *mongo.Client, marker string) (release func()) {
	t.Helper()
	const fp = "hangIndexBuildDuringCollectionScanPhaseAfterInsertion"
	err := client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "configureFailPoint", Value: fp},
		{Key: "mode", Value: "alwaysOn"},
		{Key: "data", Value: bson.D{{Key: "fieldsToMatch", Value: bson.D{{Key: "marker", Value: marker}}}}},
	}).Err()
	if err != nil {
		t.Fatalf("configure fail point: %v", err)
	}
	release = func() {
		err := client.Database("admin").RunCommand(context.Background(), bson.D{
			{Key: "configureFailPoint", Value: fp}, {Key: "mode", Value: "off"},
		}).Err()
		if err != nil {
			t.Errorf("turn fail point off: %v", err)
		}
	}
	// Runs before the database is dropped, so the drop doesn't wait on a
	// held build.
	t.Cleanup(release)
	return release
}

// waitIndexBuild waits until a createIndexes on db.coll is in progress.
func waitIndexBuild(t *testing.T, ctx context.Context, client *mongo.Client, db, coll string) {
	t.Helper()
	eventually(t, 30*time.Second, "an index build", func() bool {
		cur, err := client.Database("admin").Aggregate(ctx, mongo.Pipeline{
			{{Key: "$currentOp", Value: bson.D{}}},
			{{Key: "$match", Value: bson.D{{Key: "command.createIndexes", Value: coll}, {Key: "command.$db", Value: db}}}},
		})
		if err != nil {
			t.Fatalf("currentOp: %v", err)
		}
		return cur.Next(ctx)
	})
}

// TestIndexBuildKeepsLock checks the heartbeat keeps running during an index
// build outside a transaction: a build longer than the lock's expiry keeps
// the lock, no other runner can take it, and the Finished entry is written
// only once the build is done.
func TestIndexBuildKeepsLock(t *testing.T) {
	ctx := testCtx(t)
	client := testClient(t)
	db := freshDB(t, client)
	marker := rand.Text()
	if _, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "name", Value: "a"}, {Key: "marker", Value: marker}}); err != nil {
		t.Fatal(err)
	}
	release := holdIndexBuilds(t, ctx, client, marker)
	ms := load(t, fixtureDir(t, "index", createName))

	// The lock expires 3s after its last heartbeat.
	o := Options{RunnerID: "builder", HeartbeatInterval: time.Second, LockTTL: time.Second}
	done := make(chan error, 1)
	go func() {
		_, err := execute(t, ctx, db, ms, o, Config{Mode: ModeApply})
		done <- err
	}()
	waitIndexBuild(t, ctx, client, db.Name(), "items")

	first := readLock(t, ctx, db)
	time.Sleep(5 * time.Second)
	last := readLock(t, ctx, db)
	if first == nil || last == nil || last.Owner != "builder" || !last.LastHeartbeat.After(first.LastHeartbeat) {
		t.Fatalf("lock during the build: %+v then %+v, want builder's heartbeat to advance", first, last)
	}
	if _, err := Start(ctx, db, Options{RunnerID: "other", Logger: testLogger(t)}); !errors.Is(err, ErrLockHeld) {
		t.Errorf("another runner during the build: %v, want ErrLockHeld", err)
	}
	if n := countDocs(t, ctx, db.Collection(ChangelogCollection), bson.D{}); n != 0 {
		t.Errorf("%d changelog entries while the build runs, want none", n)
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{createName + ":Finished"}) {
		t.Errorf("changelog %v", got)
	}
}

// TestIndexConflicts checks plan predicts what apply does for a createIndex
// next to existing indexes, on the server under test: each case of
// testdata/index-conflicts/cases.json creates its existing indexes, plans
// the step (create, present, differs or error, with or without a warning),
// then applies it and checks whether MongoDB accepted it. CI runs it on every
// supported major version.
func TestIndexConflicts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "index-conflicts", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Name     string   `bson:"name"`
			Existing []bson.D `bson:"existing"`
			Step     bson.Raw `bson:"step"`
			Plan     string   `bson:"plan"`
			Warning  bool     `bson:"warning"`
			Apply    bool     `bson:"apply"`
			// Before8 replaces plan and apply on MongoDB 7.0.
			Before8 *struct {
				Plan  string `bson:"plan"`
				Apply bool   `bson:"apply"`
			} `bson:"before8"`
		} `bson:"cases"`
	}
	if err := bson.UnmarshalExtJSON(append(append([]byte(`{"cases":`), raw...), '}'), false, &file); err != nil {
		t.Fatal(err)
	}
	const id = "2026-11-02-001-conflict-case"
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			if err := db.RunCommand(ctx, bson.D{{Key: "createIndexes", Value: "items"}, {Key: "indexes", Value: tc.Existing}}).Err(); err != nil {
				t.Fatalf("create the existing indexes: %v", err)
			}
			step := bson.D{{Key: "collection", Value: "items"}, {Key: "operation", Value: "createIndex"},
				{Key: "keys", Value: tc.Step.Lookup("keys")}, {Key: "options", Value: tc.Step.Lookup("options")}}
			body, err := bson.MarshalExtJSON(bson.D{{Key: "id", Value: id}, {Key: "description", Value: tc.Name}, {Key: "steps", Value: bson.A{step}}}, true, false)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, id+".json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			ms := load(t, dir)
			if tc.Before8 != nil && !comparesHidden(serverVersion(t, ctx, db)) {
				tc.Plan, tc.Apply = tc.Before8.Plan, tc.Before8.Apply
			}

			rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan})
			row := rep.row(id)
			if row == nil || len(row.Indexes) != 1 {
				t.Fatalf("plan: %v, row %+v", err, row)
			}
			c := row.Indexes[0]
			got := string(c.Action)
			if c.Conflict != "" {
				got = "error"
			}
			if got != tc.Plan || (err != nil) != (tc.Plan == "error") || (c.Warning != "") != (tc.Warning || tc.Plan == "differs") {
				t.Errorf("plan: %s, warning %q, conflict %q, error %v; want %s, warning %v", c.Action, c.Warning, c.Conflict, err, tc.Plan, tc.Warning)
			}

			_, err = execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
			if (err == nil) != tc.Apply {
				t.Errorf("apply: %v, want it to succeed: %v", err, tc.Apply)
			}
		})
	}
}

func TestComparesHidden(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{version: "7.0.35", want: false},
		{version: "8.0.0", want: true},
		{version: "8.3.13", want: true},
		{version: "9.0.4", want: true},
		{version: "unknown", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			if got := comparesHidden(tc.version); got != tc.want {
				t.Errorf("comparesHidden(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

// serverVersion returns the version of the server db is on.
func serverVersion(t *testing.T, ctx context.Context, db *mongo.Database) string {
	t.Helper()
	var info struct {
		Version string `bson:"version"`
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		t.Fatal(err)
	}
	return info.Version
}

func TestSameCollationSpec(t *testing.T) {
	full := `{"alternate": "non-ignorable", "locale": "en", "strength": 2, "version": "57.1"}`
	cases := []struct {
		a, b       string
		same, sure bool
	}{
		{a: "", b: "", same: true, sure: true},
		{a: full, b: full, same: true, sure: true},
		{a: "", b: full, same: false, sure: true},
		{a: full, b: "", same: false, sure: true},
		// A collation from a file lists only what it sets.
		{a: `{"locale": "en", "strength": 2}`, b: full, same: true, sure: false},
		{a: `{"locale": "en", "strength": 1}`, b: full, same: false, sure: true},
		{a: `{"locale": "fr"}`, b: full, same: false, sure: true},
	}
	for _, tc := range cases {
		t.Run(tc.a+" "+tc.b, func(t *testing.T) {
			if same, sure := sameCollationSpec(tc.a, tc.b); same != tc.same || sure != tc.sure {
				t.Errorf("sameCollationSpec = %v, %v; want %v, %v", same, sure, tc.same, tc.sure)
			}
		})
	}
}
