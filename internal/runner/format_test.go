package runner

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// TestFormatGolden pins the shape of the changelog entry and the lock
// document: a fully populated Entry as it reads, the $set stage writeEntry
// writes an entry with, and a lock document, as canonical Extended JSON. A
// mismatch means their format changed: decide whether v must go up (see
// "Changelog and lock formats" in CONTRIBUTING.md), then rerun with -update.
func TestFormatGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "format", "steps.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Steps []migration.Step `bson:"steps"`
	}
	if err := bson.UnmarshalExtJSON(raw, false, &file); err != nil {
		t.Fatal(err)
	}
	m := migration.Migration{
		ID:          "2026-11-05-001-every-step",
		Description: "One step of each operation",
		Steps:       file.Steps,
		Checksum:    "5f0c7e4a9d2b3c1e8f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e",
		// Only a one-step data migration has one: set, to pin it.
		BatchSize: 1000,
	}
	snap, err := snapshot(m)
	if err != nil {
		t.Fatal(err)
	}
	n := func(v int64) *int64 { return &v }
	step := func(i int) *int { return &i }
	set := func(ext string) bson.Raw {
		var r bson.Raw
		if err := bson.UnmarshalExtJSON([]byte(ext), false, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	results := []StepResult{
		{Collection: "projects", Operation: "updateOne", MatchedCount: n(0), ModifiedCount: n(0), UpsertedCount: n(1)},
		{Collection: "projects", Operation: "updateMany", MatchedCount: n(3), ModifiedCount: n(2), UpsertedCount: n(0)},
		{Collection: "projects", Operation: "deleteMany", DeletedCount: n(4)},
		{Collection: "accounts", Operation: "createIndex", IndexName: "name_1_ci"},
		{Collection: "accounts", Operation: "dropIndex", IndexName: "name_1"},
		{Collection: "events", Operation: "collModIndex", IndexName: "dedupeKey_1", Set: set(`{"unique": true}`)},
		{Collection: "events", Operation: "collModCollection", Set: set(`{"validationLevel": "moderate"}`)},
		{Collection: "accounts", Operation: "aggregate", MergeCounts: MergeCounts{
			DiffCountBefore: n(5), DiffCountAfter: n(0), UnmatchedCount: n(1), TypeChangedCount: n(2),
			DuplicateOnCount: n(0), NullOnCount: n(0), ArrayOnCount: n(0), IDMismatchCount: n(0),
			CollationOnlyMatchCount: n(1),
		}},
	}
	insert := StepResult{
		Collection: "legacy", Operation: "aggregate",
		MergeCounts: MergeCounts{DiffCountBefore: n(0), DiffCountAfter: n(0), TypeChangedCount: n(0),
			DuplicateOnCount: n(0), NullOnCount: n(0), ArrayOnCount: n(0), IDMismatchCount: n(0)},
		Insert: &InsertStep{Step: 1, Into: "catalog", TargetExistsBefore: new(false), PendingBefore: n(2),
			PendingAfter: n(0), DiffCountBefore: n(0), DiffCountAfter: n(0)},
	}
	errs := []StepError{
		{Step: step(1), Message: "/steps/1 (updateMany on projects, second run): not idempotent"},
		{Message: "no Errored changelog entry written: migration lock lost"},
	}
	at := time.Date(2026, 11, 8, 0, 11, 27, 10_000_000, time.UTC)
	approvedBy := ApprovedBy{Name: "Ada Lovelace", Email: "ada@example.com"}
	entry := Entry{
		ID:          bson.ObjectID{0x6a, 0xef, 0xbe, 0x2f, 0x08, 0xc4, 0x9a, 0x79, 0xb0, 0x6c, 0x46, 0x6b},
		V:           EntryVersion,
		Namespace:   "api",
		MigrationID: m.ID,
		Checksum:    m.Checksum,
		CreatedAt:   at,
		Runner:      "ci-42-1-AB12CD34",
		ApprovedBy:  approvedBy,
		Status:      StatusFinished,
		Migration:   snap,
		// Only one of the two is set in a real entry: both are, to pin both.
		Results: results,
		Errors:  errs,
		Batches: 3,
	}
	run := &Run{namespace: entry.Namespace, runnerID: entry.Runner, approvedBy: approvedBy}
	finished, err := run.entrySet(m, StatusFinished, bson.E{Key: "results", Value: results})
	if err != nil {
		t.Fatal(err)
	}
	batched, err := run.entrySet(m, StatusFinished, bson.E{Key: "results", Value: results}, bson.E{Key: "batches", Value: int32(3)})
	if err != nil {
		t.Fatal(err)
	}
	errored, err := run.entrySet(m, StatusErrored, bson.E{Key: "errors", Value: errs})
	if err != nil {
		t.Fatal(err)
	}
	current := "2026-11-03-001-copy-currency-symbol"
	lock := lockDoc{
		ID:            lockID("api"),
		V:             LockVersion,
		Token:         "9f86d081884c7d659a2feaa0c55ad015",
		Owner:         "ci-42-1-AB12CD34",
		Current:       &current,
		AcquiredAt:    at.Add(-85 * time.Second),
		LastHeartbeat: at,
		ExpiresAt:     at.Add(DefaultLockTTL),
	}

	cases := []struct {
		golden string
		doc    any
	}{
		{golden: "result-merge-insert.json", doc: insert},
		{golden: "entry.json", doc: entry},
		{golden: "entry-set-finished.json", doc: finished},
		{golden: "entry-set-finished-batched.json", doc: batched},
		{golden: "entry-set-errored.json", doc: errored},
		{golden: "lock.json", doc: lock},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := bson.MarshalExtJSONIndent(tc.doc, true, false, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := filepath.Join("testdata", "format", tc.golden)
			if *updateGolden {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("the changelog or lock format changed: decide whether v must go up "+
					"(see \"Changelog and lock formats\" in CONTRIBUTING.md), then rerun with -update.\n"+
					"got:\n%s\nwant (%s):\n%s", got, golden, want)
			}
		})
	}
}

// A v1 reader has no insert field on its step result. The BSON decoder
// must still read the entry and the original merge counters.
func TestV1ReaderWithInsertResult(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "format", "result-merge-insert.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result bson.Raw
	if err := bson.UnmarshalExtJSON(raw, true, &result); err != nil {
		t.Fatal(err)
	}
	entry, err := bson.Marshal(bson.D{
		{Key: "v", Value: EntryVersion}, {Key: "status", Value: StatusFinished},
		{Key: "results", Value: bson.A{result}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		V       int
		Status  Status
		Results []struct {
			Collection  string
			Operation   string
			MergeCounts `bson:",inline"`
		}
	}
	if err := bson.Unmarshal(entry, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.V != 1 || legacy.Status != StatusFinished || len(legacy.Results) != 1 {
		t.Fatalf("v1 entry: %+v", legacy)
	}
	r := legacy.Results[0]
	if r.Collection != "legacy" || r.Operation != "aggregate" || r.DiffCountAfter == nil || *r.DiffCountAfter != 0 || r.UnmatchedCount != nil {
		t.Fatalf("v1 result: %+v", r)
	}
}

// TestCheckFormats checks CheckFormats refuses a changelog entry, in any
// namespace, or a lock document with a v above the one this migrune writes,
// naming both versions, and reads a missing v as 1.
func TestCheckFormats(t *testing.T) {
	entry := func(v any, namespace string) bson.D {
		d := bson.D{{Key: "_id", Value: bson.NewObjectID()}}
		if v != nil {
			d = append(d, bson.E{Key: "v", Value: v})
		}
		if namespace != "" {
			d = append(d, bson.E{Key: "namespace", Value: namespace})
		}
		return append(d, bson.E{Key: "migrationId", Value: "2026-11-01-001-a"}, bson.E{Key: "status", Value: "Finished"})
	}
	lock := func(v any) bson.D {
		d := bson.D{{Key: "_id", Value: lockID("")}, {Key: "token", Value: newToken()}, {Key: "owner", Value: "other"}}
		if v != nil {
			d = append(d, bson.E{Key: "v", Value: v})
		}
		return d
	}
	cases := []struct {
		name      string
		changelog []bson.D
		locks     []bson.D
		want      string // the error, or "" for none
	}{
		{name: "empty database"},
		{name: "current entries and lock", changelog: []bson.D{entry(int32(1), ""), entry(int32(1), "api")}, locks: []bson.D{lock(int32(1))}},
		// Written by hand.
		{name: "no v", changelog: []bson.D{entry(nil, "")}, locks: []bson.D{lock(nil)}},
		{name: "newer entry", changelog: []bson.D{entry(int32(1), ""), entry(int32(2), "")},
			want: "the changelog has format v2, newer than this migrune understands (v1): upgrade migrune"},
		{name: "newer entry in another namespace", changelog: []bson.D{entry(int64(3), "web"), entry(2.0, "")},
			want: "the changelog has format v3, newer than this migrune understands (v1): upgrade migrune"},
		{name: "newer lock", locks: []bson.D{lock(int32(2))},
			want: "the migration lock has format v2, newer than this migrune understands (v1): upgrade migrune"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			ctx := testCtx(t)
			for coll, docs := range map[string][]bson.D{ChangelogCollection: tc.changelog, LockCollection: tc.locks} {
				for _, d := range docs {
					if _, err := db.Collection(coll).InsertOne(ctx, d); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := CheckFormats(ctx, db)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var format *FormatError
			if !errors.As(err, &format) || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// TestAcquireLockVersion checks the lock document carries v: 1 from every
// write, with the fields lock.json pins; that a lock without v is taken
// once expired; and that a newer one is
// never taken, even expired, and is left as it is.
func TestAcquireLockVersion(t *testing.T) {
	cases := []struct {
		name    string
		v       any // the existing lock's v; nil: no field
		wantErr bool
	}{
		{name: "no v", v: nil},
		{name: "v1", v: int32(1)},
		{name: "v2", v: int32(2), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			ctx := testCtx(t)
			old := bson.D{
				{Key: "_id", Value: lockID("")}, {Key: "token", Value: newToken()}, {Key: "owner", Value: "old"},
				{Key: "current", Value: nil}, {Key: "acquiredAt", Value: time.Now().Add(-time.Hour)},
				{Key: "lastHeartbeat", Value: time.Now().Add(-time.Hour)}, {Key: "expiresAt", Value: time.Now().Add(-time.Minute)},
			}
			if tc.v != nil {
				old = append(old, bson.E{Key: "v", Value: tc.v})
			}
			if _, err := db.Collection(LockCollection).InsertOne(ctx, old); err != nil {
				t.Fatal(err)
			}
			l := newTestLock(t, db, "runner-a", time.Second, 2*time.Second)
			err := l.Acquire(ctx, 0)
			if tc.wantErr {
				var format *FormatError
				if !errors.As(err, &format) || errors.Is(err, ErrLockHeld) || format.Found != 2 {
					t.Fatalf("acquire: %v, want a format error", err)
				}
				if doc := readLock(t, ctx, db); doc.Owner != "old" || doc.V != 2 {
					t.Errorf("lock %+v, want it left as it was", doc)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			rawLock := func(what string) {
				t.Helper()
				raw, err := db.Collection(LockCollection).FindOne(ctx, bson.D{}).Raw()
				if err != nil {
					t.Fatal(err)
				}
				elems, err := raw.Elements()
				if err != nil {
					t.Fatal(err)
				}
				want := docKeys(t, lockDoc{})
				if keys := elemKeys(elems); !slices.Equal(keys, want) {
					t.Errorf("after %s, lock fields %v, want %v (testdata/format/lock.json)", what, keys, want)
				}
				if v := raw.Lookup("v"); v.Type != bson.TypeInt32 || v.Int32() != LockVersion {
					t.Errorf("after %s, lock v %s, want %d", what, v, LockVersion)
				}
			}
			rawLock("acquire")
			// A heartbeat and SetCurrent write v too.
			if _, err := db.Collection(LockCollection).UpdateOne(ctx, bson.D{},
				bson.D{{Key: "$unset", Value: bson.D{{Key: "v", Value: ""}}}}); err != nil {
				t.Fatal(err)
			}
			if err := l.Heartbeat(ctx); err != nil {
				t.Fatal(err)
			}
			rawLock("a heartbeat")
			if _, err := db.Collection(LockCollection).UpdateOne(ctx, bson.D{},
				bson.D{{Key: "$unset", Value: bson.D{{Key: "v", Value: ""}}}}); err != nil {
				t.Fatal(err)
			}
			if err := l.SetCurrent(ctx, "2026-11-01-001-a"); err != nil {
				t.Fatal(err)
			}
			rawLock("SetCurrent")
			if ok, err := l.Release(ctx); err != nil || !ok {
				t.Fatalf("release: %v %v", ok, err)
			}
		})
	}
}

// docKeys returns the sorted field names doc marshals with.
func docKeys(t *testing.T, doc any) []string {
	t.Helper()
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	elems, err := bson.Raw(raw).Elements()
	if err != nil {
		t.Fatal(err)
	}
	return elemKeys(elems)
}

// elemKeys returns the sorted keys of elems.
func elemKeys(elems []bson.RawElement) []string {
	var keys []string
	for _, e := range elems {
		keys = append(keys, e.Key())
	}
	slices.Sort(keys)
	return keys
}
