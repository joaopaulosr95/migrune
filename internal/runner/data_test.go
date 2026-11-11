package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

func TestParseDataOptions(t *testing.T) {
	no := false
	cases := []struct {
		file  string
		check func(t *testing.T, o dataOptions)
		err   string // substring of the error; "" means no error
	}{
		{
			file: "2026-11-01-001-all-update-options",
			check: func(t *testing.T, o dataOptions) {
				if o.Upsert == nil || *o.Upsert != no {
					t.Errorf("upsert = %v, want false", o.Upsert)
				}
				if len(o.ArrayFilters) != 1 || o.ArrayFilters[0].(bson.Raw).Lookup("t").StringValue() != "y" {
					t.Errorf("arrayFilters = %v", o.ArrayFilters)
				}
				want := options.Collation{Locale: "en", Strength: 2, CaseLevel: true}
				if o.Collation == nil || *o.Collation != want {
					t.Errorf("collation = %+v, want %+v", o.Collation, want)
				}
				if o.Hint != "name_1" {
					t.Errorf("hint = %v, want name_1", o.Hint)
				}
				if o.Let.(bson.Raw).Lookup("target").StringValue() != "a" {
					t.Errorf("let = %v", o.Let)
				}
			},
		},
		{
			file: "2026-11-01-002-delete-options",
			check: func(t *testing.T, o dataOptions) {
				if h, ok := o.Hint.(bson.Raw); !ok || h.Lookup("stale").Int32() != 1 {
					t.Errorf("hint = %v, want { stale: 1 }", o.Hint)
				}
				if o.Collation == nil || o.Collation.Locale != "fr" || !o.Collation.Backwards {
					t.Errorf("collation = %+v", o.Collation)
				}
			},
		},
		{file: "2026-11-01-003-upsert-on-delete", err: `option "upsert" is not supported for deleteMany; supported options: collation, hint, let`},
		{file: "2026-11-01-004-unknown-option", err: `option "bypassDocumentValidation" is not supported for updateOne`},
		{file: "2026-11-01-005-upsert-not-bool", err: "option upsert must be a boolean"},
		{file: "2026-11-01-006-hint-number", err: "option hint must be an index name or a key pattern"},
		{
			file: "2026-11-01-007-no-options",
			check: func(t *testing.T, o dataOptions) {
				if o.Upsert != nil || o.ArrayFilters != nil || o.Collation != nil || o.Hint != nil || o.Let != nil {
					t.Errorf("options = %+v, want none", o)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			m, err := migration.LoadFile(filepath.Join("testdata", "options", tc.file+".json"))
			if err != nil {
				t.Fatal(err)
			}
			o, err := parseDataOptions(m.Steps[0])
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %v, want it to contain %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, o)
		})
	}
}

func TestCheckIdempotent(t *testing.T) {
	m := fixture(t, "2026-11-05-001-add-archived-flag")
	n := func(v int64) *int64 { return &v }
	cases := []struct {
		name   string
		second []StepResult
		step   int // -1: idempotent
		want   string
	}{
		{name: "nothing changed", step: -1, second: []StepResult{
			{MatchedCount: n(4), ModifiedCount: n(0), UpsertedCount: n(0)},
			{MatchedCount: n(0), ModifiedCount: n(0), UpsertedCount: n(0)},
			{DeletedCount: n(0)},
		}},
		{name: "modified", step: 0, want: "modified 3", second: []StepResult{
			{MatchedCount: n(3), ModifiedCount: n(3), UpsertedCount: n(0)},
		}},
		{name: "upserted", step: 1, want: "upserted 1", second: []StepResult{
			{MatchedCount: n(0), ModifiedCount: n(0), UpsertedCount: n(0)},
			{MatchedCount: n(0), ModifiedCount: n(0), UpsertedCount: n(1)},
		}},
		{name: "deleted", step: 2, want: "deleted 2", second: []StepResult{
			{ModifiedCount: n(0)}, {ModifiedCount: n(0)}, {DeletedCount: n(2)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkIdempotent(m, tc.second)
			if tc.step < 0 {
				if err != nil {
					t.Fatalf("checkIdempotent: %v", err)
				}
				return
			}
			var sf *StepFailure
			if !errors.As(err, &sf) || sf.Step != tc.step || !sf.Second {
				t.Fatalf("checkIdempotent: %v, want a second-run failure of step %d", err, tc.step)
			}
			if !strings.Contains(err.Error(), "not idempotent") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want %q", err, tc.want)
			}
			if errs := (&Run{}).stepErrors(err); len(errs) != 1 || *errs[0].Step != tc.step {
				t.Errorf("stepErrors = %+v, want step %d", errs, tc.step)
			}
		})
	}
}

// Fixtures of TestTransactionSize and TestTransactionLifetime, in
// testdata/data.
const (
	seedAndFlagAll = "2026-11-01-007-seed-and-flag-all"
	deleteAllStale = "2026-11-01-008-delete-all-stale"
	slowCount      = "2026-11-01-009-slow-count"
)

// TestTransactionSize checks a data migration whose updateMany or
// deleteMany step matches more than MaxTransactionDocs documents fails
// before its transaction: nothing is written, and apply records Errored.
// updateOne steps aren't counted.
func TestTransactionSize(t *testing.T) {
	cases := []struct {
		name string
		id   string
		docs int
		// step is the step refused, -1 for none.
		step int
		want string
	}{
		{name: "updateMany at the limit", id: seedAndFlagAll, docs: MaxTransactionDocs, step: -1},
		{name: "updateMany over the limit", id: seedAndFlagAll, docs: MaxTransactionDocs + 1, step: 1,
			want: "/steps/1 (updateMany on items) matches more than 10,000 documents: one transaction would hold them all; " +
				"set batchSize (1 to 10,000) to run it in batches"},
		{name: "deleteMany at the limit", id: deleteAllStale, docs: MaxTransactionDocs, step: -1},
		{name: "deleteMany over the limit", id: deleteAllStale, docs: MaxTransactionDocs + 1, step: 0,
			want: "/steps/0 (deleteMany on items) matches more than 10,000 documents: one transaction would hold them all; " +
				"set batchSize (1 to 10,000) to run it in batches"},
	}
	for _, tc := range cases {
		for _, mode := range []Mode{ModePlan, ModeApply} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				ctx := testCtx(t)
				db := testDB(t)
				docs := make([]any, tc.docs)
				for i := range docs {
					docs[i] = bson.D{{Key: "_id", Value: i}, {Key: "stale", Value: true}}
				}
				items := db.Collection("items")
				if _, err := items.InsertMany(ctx, docs); err != nil {
					t.Fatal(err)
				}
				ms := load(t, fixtureDir(t, "data", tc.id))

				rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: mode})
				written := mode == ModeApply && tc.step < 0
				remaining := map[bool]int64{false: int64(tc.docs), true: 0}[written]
				if n := countDocs(t, ctx, items, bson.D{{Key: "flag", Value: bson.D{{Key: "$exists", Value: false}}}}); tc.id == seedAndFlagAll && n != remaining {
					t.Errorf("%d items without a flag, want %d", n, remaining)
				}
				if n := countDocs(t, ctx, items, bson.D{}); tc.id == deleteAllStale && n != remaining {
					t.Errorf("%d items, want %d", n, remaining)
				}
				if n := countDocs(t, ctx, db.Collection("settings"), bson.D{}); tc.id == seedAndFlagAll && n != map[bool]int64{false: 0, true: 1}[written] {
					t.Errorf("%d settings, want the updateOne written only by a passing apply", n)
				}
				entries := entryStatuses(changelog(t, ctx, db))
				if tc.step < 0 {
					if err != nil {
						t.Fatalf("Execute: %v", err)
					}
					if want := map[Mode][]string{ModeApply: {tc.id + ":Finished"}}[mode]; !slices.Equal(entries, want) {
						t.Errorf("changelog %v, want %v", entries, want)
					}
					return
				}
				var se *TransactionSizeError
				if !errors.As(err, &se) || se.Step != tc.step || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Execute: %v, want a TransactionSizeError %q", err, tc.want)
				}
				if row := rep.Migrations[0]; row.Status != RowErrored || row.Error != tc.want || len(row.Steps) != 0 {
					t.Errorf("row %+v, want errored with %q and no step counts", row, tc.want)
				}
				if want := map[Mode][]string{ModeApply: {tc.id + ":Errored"}}[mode]; !slices.Equal(entries, want) {
					t.Fatalf("changelog %v, want %v", entries, want)
				}
				if mode == ModeApply {
					e := changelog(t, ctx, db)[0]
					if len(e.Errors) != 1 || e.Errors[0].Step == nil || *e.Errors[0].Step != tc.step || e.Errors[0].Message != tc.want {
						t.Errorf("errors %+v, want step %d: %q", e.Errors, tc.step, tc.want)
					}
				}
			})
		}
	}
}

// TestTransactionLifetime checks a data migration whose transaction outlives
// transactionLifetimeLimitSeconds fails with a hint to batch it, whether or
// not the server's message is shown.
func TestTransactionLifetime(t *testing.T) {
	client := testClient(t)
	ctx := testCtx(t)
	// The limit is the primary's, where transactions run. It is restored
	// however the test ends.
	admin := client.Database("admin")
	primary := options.RunCmd().SetReadPreference(readpref.Primary())
	var was struct {
		Was int32 `bson:"was"`
	}
	err := admin.RunCommand(ctx, bson.D{{Key: "setParameter", Value: 1}, {Key: "transactionLifetimeLimitSeconds", Value: 1}}, primary).Decode(&was)
	if err != nil {
		t.Fatalf("lower transactionLifetimeLimitSeconds: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := admin.RunCommand(ctx, bson.D{{Key: "setParameter", Value: 1}, {Key: "transactionLifetimeLimitSeconds", Value: was.Was}}, primary).Err()
		if err != nil {
			t.Errorf("restore transactionLifetimeLimitSeconds to %d: %v", was.Was, err)
		}
	})

	// Part of the server's message, the same on every tested version.
	const server = "transactionLifetimeLimitSeconds"
	for _, values := range []bool{false, true} {
		t.Run(fmt.Sprintf("sampleValues=%t", values), func(t *testing.T) {
			db := freshDB(t, client)
			docs := make([]any, 2000)
			for i := range docs {
				docs[i] = bson.D{{Key: "_id", Value: i}, {Key: "slow", Value: true}}
			}
			if _, err := db.Collection("items").InsertMany(ctx, docs); err != nil {
				t.Fatal(err)
			}
			ms := load(t, fixtureDir(t, "data", slowCount))

			rep, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModeApply, SampleValues: values})
			if !isCode(err, codeTransactionExceededLifetimeLimitSeconds) {
				t.Fatalf("Execute: %v, want TransactionExceededLifetimeLimitSeconds", err)
			}
			want := "(TransactionExceededLifetimeLimitSeconds, code 290); lower batchSize, or set it; " + serverMessageHidden
			if values {
				want = server + "'; lower batchSize, or set it"
			}
			entries := changelog(t, ctx, db)
			if len(entries) != 1 || len(entries[0].Errors) != 1 {
				t.Fatalf("changelog %+v, want one Errored entry", entries)
			}
			for what, text := range map[string]string{"error": err.Error(), "report": rep.Migrations[0].Error,
				"changelog": entries[0].Errors[0].Message} {
				if !strings.Contains(text, want) {
					t.Errorf("%s %q, want it to contain %q", what, text, want)
				}
				if !values && strings.Contains(text, server) || values && !strings.Contains(text, "interrupted") {
					t.Errorf("%s %q: want the server's message with values only", what, text)
				}
			}
			if n := countDocs(t, ctx, db.Collection("items"), bson.D{{Key: "n", Value: bson.D{{Key: "$exists", Value: true}}}}); n != 0 {
				t.Errorf("%d items written, want none", n)
			}
		})
	}
}

// TestWithLifetimeHint checks the hint follows only the lifetime error, and
// shows with and without the server's message.
func TestWithLifetimeHint(t *testing.T) {
	const msg = "operation was interrupted because the transaction exceeded the configured 'transactionLifetimeLimitSeconds'"
	lifetime := &StepFailure{Step: 0, Operation: "updateMany", Collection: "items", Second: true,
		Err: mongo.CommandError{Code: 290, Name: "TransactionExceededLifetimeLimitSeconds", Message: msg}}
	other := &StepFailure{Step: 0, Operation: "updateMany", Collection: "items",
		Err: mongo.CommandError{Code: 251, Name: "NoSuchTransaction", Message: "Transaction with { txnNumber: 5 } has been aborted."}}
	cases := []struct {
		name   string
		err    error
		values bool
		want   string
	}{
		{name: "lifetime, hidden", err: lifetime,
			want: "/steps/0 (updateMany on items, second run): (TransactionExceededLifetimeLimitSeconds, code 290); " +
				"lower batchSize, or set it; " + serverMessageHidden},
		{name: "lifetime, shown", err: lifetime, values: true,
			want: "/steps/0 (updateMany on items, second run): (TransactionExceededLifetimeLimitSeconds) " + msg + "; lower batchSize, or set it"},
		{name: "other server error", err: other,
			want: "/steps/0 (updateMany on items): (NoSuchTransaction, code 251); " + serverMessageHidden},
		{name: "not a server error", err: errors.New("boom"), want: "boom"},
		{name: "nil", err: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := withLifetimeHint(tc.err)
			if got := errorText(err, tc.values); got != tc.want {
				t.Errorf("errorText:\n got %s\nwant %s", got, tc.want)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("withLifetimeHint(%v) doesn't wrap it", tc.err)
			}
		})
	}
}
