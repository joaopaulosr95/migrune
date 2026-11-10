package runner

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// collModCollection migration fixtures in testdata/validation.
const (
	requireName       = "2026-11-07-001-require-name"
	moderateUsers     = "2026-11-07-002-moderate"
	warnUsers         = "2026-11-07-003-warn"
	removeValidator   = "2026-11-07-004-remove-validator"
	validateMissing   = "2026-11-07-005-validate-missing"
	indexThenValidate = "2026-11-07-006-index-then-validate"
)

// validationRun plans or applies the validation fixtures ids on db.
func validationRun(t *testing.T, ctx context.Context, db *mongo.Database, mode Mode, ids ...string) (*Report, error) {
	t.Helper()
	return execute(t, ctx, db, load(t, fixtureDir(t, "validation", ids...)), Options{}, Config{Mode: mode})
}

// validationChanges renders the changes of rep's row id as "action set
// before", followed by "nonMatching N", "warning <level or action it names>"
// and "conflict" when they are set.
func validationChanges(rep *Report, id string) []string {
	row := rep.row(id)
	if row == nil {
		return nil
	}
	var out []string
	for _, c := range row.Indexes {
		line := string(c.Action) + " " + c.Name + " " + string(c.Set) + " " + string(c.Before)
		if c.NonMatching != nil {
			line += " nonMatching " + strconv.FormatInt(*c.NonMatching, 10)
		}
		for _, w := range []string{"validationAction warn", "validationLevel moderate it lets", "rejects every update"} {
			if strings.Contains(c.Warning, w) {
				line += " warning " + w
			}
		}
		if c.Conflict != "" {
			line += " conflict"
		}
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return out
}

// collOptions returns the options listCollections shows for coll, for
// comparison, or "absent".
func collOptions(t *testing.T, ctx context.Context, db *mongo.Database, coll string) string {
	t.Helper()
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{{Key: "name", Value: coll}})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) == 0 {
		return "absent"
	}
	var opts bson.D
	if err := bson.Unmarshal(specs[0].Options, &opts); err != nil {
		t.Fatal(err)
	}
	return migration.Canonical(opts)
}

// seedUsers inserts one user that matches requireName's validator and two
// that don't.
func seedUsers(t *testing.T, ctx context.Context, db *mongo.Database) *mongo.Collection {
	t.Helper()
	users := db.Collection("users")
	insert(t, ctx, users,
		bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "a"}},
		bson.D{{Key: "_id", Value: 2}},
		bson.D{{Key: "_id", Value: 3}, {Key: "name", Value: 3}})
	return users
}

const (
	requireNameValidator = `{"$jsonSchema":{"required":["name"],"properties":{"name":{"bsonType":"string"}}}}`
	requireNameSet       = `{"validator":` + requireNameValidator + `,"validationLevel":"strict"}`
)

// TestCollModCollection adds a validator: plan reports modify, how many
// documents don't match and what becomes of them; apply sets it and records
// the step; once set, the step is present.
func TestCollModCollection(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	users := seedUsers(t, ctx, db)

	rep, err := validationRun(t, ctx, db, ModePlan, requireName)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`modify ` + requireNameSet + ` {"validator":{},"validationLevel":"strict"} nonMatching 2 warning rejects every update`}
	if got := validationChanges(rep, requireName); !slices.Equal(got, want) {
		t.Errorf("plan: %v, want %v", got, want)
	}
	if got := collOptions(t, ctx, db, "users"); got != "{}" {
		t.Errorf("plan changed the collection: %s", got)
	}

	rep, err = validationRun(t, ctx, db, ModeApply, requireName)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{`modify ` + requireNameSet + ` {"validator":{},"validationLevel":"strict"}`}
	if got := validationChanges(rep, requireName); !slices.Equal(got, want) {
		t.Errorf("apply: %v, want %v", got, want)
	}
	if got := collOptions(t, ctx, db, "users"); !strings.Contains(got, `"validationLevel": "strict"`) || !strings.Contains(got, `"required": ["name"]`) {
		t.Errorf("collection after apply: %s", got)
	}
	entries := changelog(t, ctx, db)
	if len(entries) != 1 || entries[0].Status != StatusFinished || len(entries[0].Results) != 1 {
		t.Fatalf("changelog %v", entryStatuses(entries))
	}
	res := entries[0].Results[0]
	if res.Operation != "collModCollection" || res.Collection != "users" || res.IndexName != "" ||
		!strings.Contains(migration.Canonical(rawDoc(t, res.Set)), `"validationLevel": "strict"`) {
		t.Errorf("result %+v, set %s", res, res.Set)
	}
	// Under strict, an update that leaves a document invalid fails.
	if _, err := users.UpdateOne(ctx, bson.D{{Key: "_id", Value: 2}}, bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: 1}}}}); err == nil {
		t.Error("an update of an invalid document passed under strict")
	}

	if _, err := db.Collection(ChangelogCollection).DeleteMany(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{ModePlan, ModeApply} {
		rep, err := validationRun(t, ctx, db, mode, requireName)
		want := []string{`present ` + requireNameSet + " " + requireNameSet}
		if got := validationChanges(rep, requireName); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s again: %v, %v, want %v", mode, err, got, want)
		}
	}
}

// TestCollModCollectionPlanCarriesState plans a validator, then moderate,
// then warn: each step sees what the earlier ones would do, and its warning
// follows the level and action. Then it checks, on the server, what the
// moderate warning says, and removes the validator.
func TestCollModCollectionPlanCarriesState(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	users := seedUsers(t, ctx, db)

	rep, err := validationRun(t, ctx, db, ModePlan, requireName, moderateUsers, warnUsers, removeValidator)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		requireName:     `modify ` + requireNameSet + ` {"validator":{},"validationLevel":"strict"} nonMatching 2 warning rejects every update`,
		moderateUsers:   `modify {"validationLevel":"moderate"} {"validationLevel":"strict"} nonMatching 2 warning validationLevel moderate it lets`,
		warnUsers:       `modify {"validationAction":"warn"} {"validationAction":"error"} nonMatching 2 warning validationAction warn`,
		removeValidator: `modify {"validator":{},"validationAction":"error"} {"validator":` + requireNameSet[len(`{"validator":`):strings.Index(requireNameSet, `,"validationLevel"`)] + `,"validationAction":"warn"}`,
	} {
		if got := validationChanges(rep, id); !slices.Equal(got, []string{want}) {
			t.Errorf("plan %s: %v, want %s", id, got, want)
		}
	}

	if _, err := validationRun(t, ctx, db, ModeApply, requireName, moderateUsers); err != nil {
		t.Fatal(err)
	}
	// Under moderate, an update to a document that doesn't match passes,
	// and one that makes a matching document invalid fails.
	if _, err := users.UpdateOne(ctx, bson.D{{Key: "_id", Value: 2}}, bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: 1}}}}); err != nil {
		t.Errorf("an update of an invalid document under moderate: %v", err)
	}
	if _, err := users.UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$unset", Value: bson.D{{Key: "name", Value: 1}}}}); err == nil {
		t.Error("an update making a valid document invalid passed under moderate")
	}

	if _, err := validationRun(t, ctx, db, ModeApply, requireName, moderateUsers, warnUsers, removeValidator); err != nil {
		t.Fatal(err)
	}
	if got := collOptions(t, ctx, db, "users"); got != `{"validationAction": "error", "validationLevel": "moderate"}` {
		t.Errorf("collection after removing the validator: %s", got)
	}
}

// TestCollModCollectionMissing checks a collModCollection of a collection
// that doesn't exist, or of a view, fails in plan and apply.
func TestCollModCollectionMissing(t *testing.T) {
	for _, mode := range []Mode{ModePlan, ModeApply} {
		for _, view := range []bool{false, true} {
			t.Run(string(mode)+"/view "+strconv.FormatBool(view), func(t *testing.T) {
				ctx := testCtx(t)
				db := testDB(t)
				seedItems(t, ctx, db)
				want := "collection nope doesn't exist"
				if view {
					if err := db.CreateView(ctx, "nope", "items", bson.A{}); err != nil {
						t.Fatal(err)
					}
					want = "nope is a view"
				}
				rep, err := validationRun(t, ctx, db, mode, validateMissing)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s: %v, want %q", mode, err, want)
				}
				if got := statuses(rep); !slices.Equal(got, []string{validateMissing + ":errored"}) {
					t.Errorf("rows %v", got)
				}
				if mode == ModePlan {
					if got := validationChanges(rep, validateMissing); !slices.Equal(got, []string{`modify {"validationLevel":"moderate"} conflict`}) {
						t.Errorf("plan: %v", got)
					}
				}
			})
		}
	}
}

// TestCollModCollectionAfterCreate checks plan sees the collection an
// earlier createIndex of the same run creates.
func TestCollModCollectionAfterCreate(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	rep, err := validationRun(t, ctx, db, ModePlan, indexThenValidate)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`create at_1`, `modify {"validator":{"action":{"$exists":true}}} {"validator":{}} nonMatching 0`}
	if got := validationChanges(rep, indexThenValidate); !slices.Equal(got, want) {
		t.Errorf("plan: %v, want %v", got, want)
	}
	if _, err := validationRun(t, ctx, db, ModeApply, indexThenValidate); err != nil {
		t.Fatal(err)
	}
	if got := collOptions(t, ctx, db, "audit"); !strings.Contains(got, `"validator": {"action": {"$exists": true}}`) {
		t.Errorf("collection after apply: %s", got)
	}
}
