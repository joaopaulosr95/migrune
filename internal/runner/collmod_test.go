package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// collModIndex migration fixtures in testdata/collmod.
const (
	expireEvents  = "2026-11-06-001-expire-events"
	hideName      = "2026-11-06-002-hide-name"
	unhideName    = "2026-11-06-003-unhide-name"
	prepareEmail  = "2026-11-06-004-prepare-email"
	dedupeEmails  = "2026-11-06-005-dedupe-emails"
	uniqueEmail   = "2026-11-06-006-unique-email"
	modifyMissing = "2026-11-06-007-modify-missing"
	indexThenTTL  = "2026-11-06-008-index-then-ttl"
)

// collModRun plans or applies the collmod fixtures ids on db.
func collModRun(t *testing.T, ctx context.Context, db *mongo.Database, mode Mode, ids ...string) (*Report, error) {
	t.Helper()
	return execute(t, ctx, db, load(t, fixtureDir(t, "collmod", ids...)), Options{}, Config{Mode: mode})
}

// indexChanges renders the index changes of rep's row id as "action name
// set before", followed by "wouldExpire N", "warning" and "conflict" when
// they are set.
func indexChanges(rep *Report, id string) []string {
	row := rep.row(id)
	if row == nil {
		return nil
	}
	var out []string
	for _, c := range row.Indexes {
		line := string(c.Action) + " " + c.Name + " " + string(c.Set) + " " + string(c.Before)
		if c.WouldExpire != nil {
			line += " wouldExpire " + strconv.FormatInt(*c.WouldExpire, 10)
		}
		if c.Warning != "" {
			line += " warning"
		}
		if c.Conflict != "" {
			line += " conflict"
		}
		out = append(out, strings.TrimSpace(line))
	}
	return out
}

// indexOptions returns the options listIndexes shows for coll's index name,
// for comparison, or "absent".
func indexOptions(t *testing.T, ctx context.Context, coll *mongo.Collection, name string) string {
	t.Helper()
	ixs, err := listIndexes(ctx, coll)
	if err != nil {
		t.Fatal(err)
	}
	for _, ix := range ixs {
		if ix.Name == name {
			return migration.Canonical(ix.Options)
		}
	}
	return "absent"
}

func createIndex(t *testing.T, ctx context.Context, coll *mongo.Collection, name string, keys bson.D) {
	t.Helper()
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys, Options: options.Index().SetName(name)}); err != nil {
		t.Fatal(err)
	}
}

// TestCollModTTL adds a TTL to a plain date index: plan reports modify and
// how many documents the TTL would delete at once, apply sets it and records
// the step, and once set, the step is present.
func TestCollModTTL(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	events := db.Collection("events")
	old, recent := time.Now().Add(-2*time.Hour), time.Now().Add(-time.Minute)
	insert(t, ctx, events,
		bson.D{{Key: "createdAt", Value: old}},
		bson.D{{Key: "createdAt", Value: old}},
		bson.D{{Key: "createdAt", Value: recent}})
	createIndex(t, ctx, events, "createdAt_1", bson.D{{Key: "createdAt", Value: 1}})

	rep, err := collModRun(t, ctx, db, ModePlan, expireEvents)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`modify createdAt_1 {"expireAfterSeconds":3600} {"expireAfterSeconds":null} wouldExpire 2`}
	if got := indexChanges(rep, expireEvents); !slices.Equal(got, want) {
		t.Errorf("plan: %v, want %v", got, want)
	}
	if got := indexOptions(t, ctx, events, "createdAt_1"); got != "{}" {
		t.Errorf("plan changed the index: %s", got)
	}

	rep, err = collModRun(t, ctx, db, ModeApply, expireEvents)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{`modify createdAt_1 {"expireAfterSeconds":3600} {"expireAfterSeconds":null}`}
	if got := indexChanges(rep, expireEvents); !slices.Equal(got, want) {
		t.Errorf("apply: %v, want %v", got, want)
	}
	if got := indexOptions(t, ctx, events, "createdAt_1"); got != `{"expireAfterSeconds": 3600}` {
		t.Errorf("index after apply: %s", got)
	}
	entries := changelog(t, ctx, db)
	if len(entries) != 1 || entries[0].Status != StatusFinished || len(entries[0].Results) != 1 {
		t.Fatalf("changelog %v", entryStatuses(entries))
	}
	res := entries[0].Results[0]
	if res.Operation != "collModIndex" || res.Collection != "events" || res.IndexName != "createdAt_1" ||
		migration.Canonical(rawDoc(t, res.Set)) != `{"expireAfterSeconds": 3600}` {
		t.Errorf("result %+v, set %s", res, res.Set)
	}

	// Finished, it isn't pending; with its entry gone, it is present.
	if rep, err := collModRun(t, ctx, db, ModeApply, expireEvents); err != nil || len(rep.Migrations) != 0 {
		t.Fatalf("second apply: %v, rows %v, want nothing pending", err, statuses(rep))
	}
	if _, err := db.Collection(ChangelogCollection).DeleteMany(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{ModePlan, ModeApply} {
		rep, err := collModRun(t, ctx, db, mode, expireEvents)
		want := []string{`present createdAt_1 {"expireAfterSeconds":3600} {"expireAfterSeconds":3600}`}
		if got := indexChanges(rep, expireEvents); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s again: %v, %v, want %v", mode, err, got, want)
		}
	}
}

// TestCollModPlanCarriesState checks plan applies a collModIndex to the index
// an earlier step of the same file creates: the TTL counts the documents it
// would delete.
func TestCollModPlanCarriesState(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	insert(t, ctx, db.Collection("sessions"),
		bson.D{{Key: "lastUsedAt", Value: time.Now().Add(-time.Hour)}},
		bson.D{{Key: "lastUsedAt", Value: time.Now()}})
	rep, err := collModRun(t, ctx, db, ModePlan, indexThenTTL)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`create lastUsedAt_1`, `modify lastUsedAt_1 {"expireAfterSeconds":60} {"expireAfterSeconds":null} wouldExpire 1`}
	if got := indexChanges(rep, indexThenTTL); !slices.Equal(got, want) {
		t.Errorf("plan: %v, want %v", got, want)
	}
	if _, err := collModRun(t, ctx, db, ModeApply, indexThenTTL); err != nil {
		t.Fatal(err)
	}
	if got := indexOptions(t, ctx, db.Collection("sessions"), "lastUsedAt_1"); got != `{"expireAfterSeconds": 60}` {
		t.Errorf("index after apply: %s", got)
	}
}

// TestCollModHidden hides an index, then unhides it.
func TestCollModHidden(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	items := db.Collection("items")
	createIndex(t, ctx, items, "name_1", bson.D{{Key: "name", Value: 1}})

	rep, err := collModRun(t, ctx, db, ModePlan, hideName, unhideName)
	if err != nil {
		t.Fatal(err)
	}
	// The second sees what the first would do.
	if got, want := indexChanges(rep, hideName), []string{`modify name_1 {"hidden":true} {"hidden":false}`}; !slices.Equal(got, want) {
		t.Errorf("plan hide: %v, want %v", got, want)
	}
	if got, want := indexChanges(rep, unhideName), []string{`modify name_1 {"hidden":false} {"hidden":true}`}; !slices.Equal(got, want) {
		t.Errorf("plan unhide: %v, want %v", got, want)
	}

	if _, err := collModRun(t, ctx, db, ModeApply, hideName); err != nil {
		t.Fatal(err)
	}
	if got := indexOptions(t, ctx, items, "name_1"); got != `{"hidden": true}` {
		t.Errorf("index after hiding: %s", got)
	}
	rep, err = collModRun(t, ctx, db, ModeApply, hideName, unhideName)
	if err != nil || !slices.Equal(statuses(rep), []string{unhideName + ":applied"}) {
		t.Fatalf("unhide: %v, rows %v", err, statuses(rep))
	}
	if got := indexOptions(t, ctx, items, "name_1"); got != "{}" {
		t.Errorf("index after unhiding: %s", got)
	}
}

// TestCollModUnique converts an index with duplicates to unique, in three
// migrations: prepareUnique, a data migration removing the duplicates, then
// unique. plan fails unique on an index that isn't prepareUnique, and on
// duplicates, with their count and keys.
func TestCollModUnique(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	users := db.Collection("users")
	insert(t, ctx, users,
		bson.D{{Key: "email", Value: "a@example.com"}},
		bson.D{{Key: "email", Value: "a@example.com"}, {Key: "duplicate", Value: true}},
		bson.D{{Key: "email", Value: "b@example.com"}},
		bson.D{{Key: "email", Value: "b@example.com"}, {Key: "duplicate", Value: true}},
		bson.D{{Key: "email", Value: "c@example.com"}})
	createIndex(t, ctx, users, "email_1", bson.D{{Key: "email", Value: 1}})

	// Without prepareUnique, MongoDB refuses the conversion.
	rep, err := collModRun(t, ctx, db, ModePlan, uniqueEmail)
	if err == nil || !strings.Contains(err.Error(), "index email_1 isn't prepareUnique") {
		t.Fatalf("plan of unique alone: %v", err)
	}
	if got := statuses(rep); !slices.Equal(got, []string{uniqueEmail + ":errored"}) {
		t.Errorf("rows %v", got)
	}
	_, err = collModRun(t, ctx, db, ModeApply, uniqueEmail)
	if err == nil || !strings.Contains(err.Error(), "InvalidOptions") {
		t.Errorf("apply of unique alone: %v, want MongoDB to refuse it", err)
	}
	if got := indexOptions(t, ctx, users, "email_1"); got != "{}" {
		t.Errorf("index after a refused unique: %s", got)
	}

	// With prepareUnique pending, plan can't check for duplicates yet.
	rep, err = collModRun(t, ctx, db, ModePlan, prepareEmail, uniqueEmail)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := indexChanges(rep, prepareEmail), []string{`modify email_1 {"prepareUnique":true} {"prepareUnique":false} warning`}; !slices.Equal(got, want) {
		t.Errorf("plan prepareUnique: %v, want %v", got, want)
	}
	if c := rep.row(uniqueEmail).Indexes; len(c) != 1 || c[0].Action != IndexModify || !strings.Contains(c[0].Warning, "plan can't check index email_1 for duplicate keys") {
		t.Errorf("plan unique: %+v", c)
	}

	if _, err := collModRun(t, ctx, db, ModeApply, prepareEmail); err != nil {
		t.Fatal(err)
	}
	if got := indexOptions(t, ctx, users, "email_1"); got != `{"prepareUnique": true}` {
		t.Errorf("index after prepareUnique: %s", got)
	}
	_, err = users.InsertOne(ctx, bson.D{{Key: "email", Value: "c@example.com"}})
	if !mongo.IsDuplicateKeyError(err) {
		t.Errorf("insert of a duplicate after prepareUnique: %v, want a duplicate key error", err)
	}

	// The duplicates fail the plan, with their count and keys.
	rep, err = collModRun(t, ctx, db, ModePlan, prepareEmail, uniqueEmail)
	want := `index email_1 has 2 duplicate keys, shared by 4 documents, such as {"email":"a@example.com"}, {"email":"b@example.com"}: ` +
		"MongoDB would refuse to make it unique (CannotConvertIndexToUnique)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("plan with duplicates: %v, want %q", err, want)
	}
	if got := indexChanges(rep, uniqueEmail); !slices.Equal(got, []string{`modify email_1 {"unique":true} {"unique":false} conflict`}) {
		t.Errorf("plan with duplicates: %v", got)
	}

	if _, err := collModRun(t, ctx, db, ModeApply, prepareEmail, dedupeEmails); err != nil {
		t.Fatal(err)
	}
	rep, err = collModRun(t, ctx, db, ModePlan, prepareEmail, dedupeEmails, uniqueEmail)
	if got := indexChanges(rep, uniqueEmail); err != nil || !slices.Equal(got, []string{`modify email_1 {"unique":true} {"unique":false}`}) {
		t.Fatalf("plan without duplicates: %v, %v", err, got)
	}
	if _, err := collModRun(t, ctx, db, ModeApply, prepareEmail, dedupeEmails, uniqueEmail); err != nil {
		t.Fatal(err)
	}
	if got := indexOptions(t, ctx, users, "email_1"); got != `{"unique": true}` {
		t.Errorf("index after unique: %s", got)
	}
	want2 := []string{prepareEmail + ":Finished", dedupeEmails + ":Finished", uniqueEmail + ":Finished"}
	entries := changelog(t, ctx, db)
	var finished []string
	for _, s := range entryStatuses(entries) {
		if strings.HasSuffix(s, ":Finished") {
			finished = append(finished, s)
		}
	}
	if !slices.Equal(finished, want2) {
		t.Errorf("changelog %v", entryStatuses(entries))
	}
}

// TestCollModPrivilege checks plan fails a collModIndex step for a user with
// readWrite only, which lacks the collMod action, and passes once a role
// grants it on the database.
func TestCollModPrivilege(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	seedItems(t, ctx, db)
	createIndex(t, ctx, db.Collection("items"), "name_1", bson.D{{Key: "name", Value: 1}})
	t.Cleanup(func() {
		_ = db.RunCommand(context.Background(), bson.D{{Key: "dropAllUsersFromDatabase", Value: 1}}).Err()
		_ = db.RunCommand(context.Background(), bson.D{{Key: "dropAllRolesFromDatabase", Value: 1}}).Err()
	})
	run := func(cmd bson.D) {
		t.Helper()
		if err := db.RunCommand(ctx, cmd).Err(); err != nil {
			t.Fatal(err)
		}
	}
	password := rand.Text()
	run(bson.D{{Key: "createUser", Value: "migrator"}, {Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: db.Name()}}}}})
	client, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("MIGRUNE_TEST_URI")).
		SetAuth(options.Credential{Username: "migrator", Password: password, AuthSource: db.Name()}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	asUser := client.Database(db.Name())

	_, err = collModRun(t, ctx, asUser, ModePlan, hideName)
	if err == nil || !strings.Contains(err.Error(), "lacks the collMod action on "+db.Name()+".items") {
		t.Fatalf("plan with readWrite only: %v", err)
	}
	run(bson.D{{Key: "createRole", Value: "collMod"}, {Key: "roles", Value: bson.A{}},
		{Key: "privileges", Value: bson.A{bson.D{
			{Key: "resource", Value: bson.D{{Key: "db", Value: db.Name()}, {Key: "collection", Value: ""}}},
			{Key: "actions", Value: bson.A{"collMod"}}}}}})
	run(bson.D{{Key: "grantRolesToUser", Value: "migrator"}, {Key: "roles", Value: bson.A{"collMod"}}})
	if _, err := collModRun(t, ctx, asUser, ModePlan, hideName); err != nil {
		t.Fatalf("plan with the collMod role: %v", err)
	}
}

// TestCollModMissing checks a collModIndex of an index or a collection that
// doesn't exist fails, in plan and apply.
func TestCollModMissing(t *testing.T) {
	for _, mode := range []Mode{ModePlan, ModeApply} {
		for _, seed := range []bool{true, false} {
			t.Run(string(mode)+"/collection exists "+strconv.FormatBool(seed), func(t *testing.T) {
				ctx := testCtx(t)
				db := testDB(t)
				if seed {
					seedItems(t, ctx, db)
				}
				rep, err := collModRun(t, ctx, db, mode, modifyMissing)
				if err == nil || !strings.Contains(err.Error(), "index missing_1 doesn't exist on items") {
					t.Fatalf("%s: %v", mode, err)
				}
				if got := statuses(rep); !slices.Equal(got, []string{modifyMissing + ":errored"}) {
					t.Errorf("rows %v", got)
				}
				var want []string
				if mode == ModeApply {
					want = []string{modifyMissing + ":Errored"}
				}
				if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, want) {
					t.Errorf("changelog %v, want %v", got, want)
				}
			})
		}
	}
}

// TestCollModInterrupted interrupts apply, as SIGTERM does, while the server
// holds its collMod: no changelog entry is written, and the next run settles
// the migration.
func TestCollModInterrupted(t *testing.T) {
	parent := testCtx(t)
	app := "migrune-collmod-" + rand.Text()[:8]
	client := appClient(t, app)
	db := freshDB(t, client)
	seedItems(t, parent, db)
	items := db.Collection("items")
	createIndex(t, parent, items, "name_1", bson.D{{Key: "name", Value: 1}})
	fired := failCommand(t, parent, client, "alwaysOn", bson.D{
		{Key: "failCommands", Value: bson.A{"collMod"}},
		{Key: "appName", Value: app},
		{Key: "blockConnection", Value: true},
		{Key: "blockTimeMS", Value: 3000},
	})

	ctx, interrupt := context.WithCancelCause(parent)
	defer interrupt(nil)
	signal := errors.New("terminated signal received")
	type result struct {
		rep *Report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := Execute(ctx, db, load(t, fixtureDir(t, "collmod", hideName)), Options{Logger: testLogger(t)}, Config{Mode: ModeApply})
		done <- result{rep, err}
	}()
	waitFailPoint(t, parent, client, fired+1)
	interrupt(signal)
	res := <-done
	var ie *InterruptedError
	if !errors.As(res.err, &ie) || !errors.Is(res.err, signal) {
		t.Fatalf("apply: %v, want an InterruptedError caused by the signal", res.err)
	}
	if got := statuses(res.rep); !slices.Equal(got, []string{hideName + ":interrupted"}) {
		t.Errorf("rows %v", got)
	}
	if n := countDocs(t, parent, db.Collection(ChangelogCollection), bson.D{}); n != 0 {
		t.Errorf("interrupted run wrote %d changelog entries", n)
	}
	failCommandOff(t, client)

	// The server may or may not have run the held collMod: either way the
	// next run settles it.
	rep, err := execute(t, parent, db, load(t, fixtureDir(t, "collmod", hideName)), Options{}, Config{Mode: ModeApply})
	if err != nil || !slices.Equal(statuses(rep), []string{hideName + ":applied"}) {
		t.Fatalf("next run: %v, rows %v", err, statuses(rep))
	}
	if got := indexOptions(t, parent, items, "name_1"); got != `{"hidden": true}` {
		t.Errorf("index after the next run: %s", got)
	}
	if got := entryStatuses(changelog(t, parent, db)); !slices.Equal(got, []string{hideName + ":Finished"}) {
		t.Errorf("changelog %v", got)
	}
}

func rawDoc(t *testing.T, raw bson.Raw) bson.D {
	t.Helper()
	var d bson.D
	if err := bson.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestCollModCases checks plan predicts what apply does for a collModIndex,
// on the server under test: each case of testdata/collmod-cases/cases.json
// creates its existing indexes on a collection with one document, plans the
// step (modify, present or error), then applies it and checks whether
// MongoDB accepted it. test:int:all runs it on every tested version.
func TestCollModCases(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "collmod-cases", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Name     string   `bson:"name"`
			Existing []bson.D `bson:"existing"`
			Index    string   `bson:"index"`
			Set      bson.Raw `bson:"set"`
			Plan     string   `bson:"plan"`
			Apply    bool     `bson:"apply"`
		} `bson:"cases"`
	}
	if err := bson.UnmarshalExtJSON(append(append([]byte(`{"cases":`), raw...), '}'), false, &file); err != nil {
		t.Fatal(err)
	}
	const id = "2026-11-06-001-collmod-case"
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t)
			insert(t, ctx, db.Collection("items"), bson.D{{Key: "at", Value: time.Now()}, {Key: "kind", Value: "a"}})
			if len(tc.Existing) > 0 {
				if err := db.RunCommand(ctx, bson.D{{Key: "createIndexes", Value: "items"}, {Key: "indexes", Value: tc.Existing}}).Err(); err != nil {
					t.Fatalf("create the existing indexes: %v", err)
				}
			}
			step := bson.D{{Key: "collection", Value: "items"}, {Key: "operation", Value: "collModIndex"},
				{Key: "name", Value: tc.Index}, {Key: "set", Value: tc.Set}}
			body, err := bson.MarshalExtJSON(bson.D{{Key: "id", Value: id}, {Key: "description", Value: tc.Name}, {Key: "steps", Value: bson.A{step}}}, false, false)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, id+".json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			ms := load(t, dir)

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
			if got != tc.Plan || (err != nil) != (tc.Plan == "error") {
				t.Errorf("plan: %s, conflict %q, error %v; want %s", c.Action, c.Conflict, err, tc.Plan)
			}

			_, err = execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply})
			if (err == nil) != tc.Apply {
				t.Errorf("apply: %v, want it to succeed: %v", err, tc.Apply)
			}
		})
	}
}
