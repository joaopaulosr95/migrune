package runner

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

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
			if errs := stepErrors(err); len(errs) != 1 || *errs[0].Step != tc.step {
				t.Errorf("stepErrors = %+v, want step %d", errs, tc.step)
			}
		})
	}
}
