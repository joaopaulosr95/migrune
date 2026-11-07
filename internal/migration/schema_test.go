package migration

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaErrorBranch checks that schema violations come only from the step
// definition matching the file's first known operation.
func TestSchemaErrorBranch(t *testing.T) {
	cases := []struct {
		file    string
		absent  []string // no violation may contain these
		present []string // some violation must contain each of these
	}{
		{file: "2026-01-01-025-index-create-missing-keys", absent: []string{"'updateOne'", "'aggregate'"}, present: []string{"missing property 'keys'"}},
		{file: "2026-01-01-018-data-missing-update", absent: []string{"'createIndex'", "'aggregate'"}, present: []string{"missing property 'update'"}},
		{file: "2026-01-01-039-merge-when-matched-replace", absent: []string{"'updateOne'", "'createIndex'"}, present: []string{`must be "merge"`}},
		// A step of another kind, or with an unknown operation, gets one
		// violation on its operation, not every failure of its fields.
		{file: "2026-01-01-010-mixed-kinds", absent: []string{"'createIndex', 'dropIndex'", "'updateOne'", "keys"}, present: []string{"createIndex belongs to index migrations"}},
		{file: "2026-01-01-011-insert-operation", absent: []string{"'updateOne'", "'createIndex'", "'aggregate'", "'documents'"}, present: []string{"insertOne is not an operation migrune runs"}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			_, err := LoadFile(filepath.Join("testdata", "invalid", tc.file+".json"))
			var se *SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("got %v, want a schema error", err)
			}
			msg := se.Error()
			for _, s := range tc.absent {
				if strings.Contains(msg, s) {
					t.Errorf("violations mention %s:\n%s", s, msg)
				}
			}
			for _, s := range tc.present {
				if !strings.Contains(msg, s) {
					t.Errorf("violations don't mention %s:\n%s", s, msg)
				}
			}
		})
	}
}

func TestComparePointers(t *testing.T) {
	cases := []struct {
		a, b string
		want int // sign
	}{
		{"/steps/2", "/steps/10", -1},
		{"/steps/0/pipeline/0/$merge/whenMatched", "/steps/0/pipeline/0/$merge/whenNotMatched", -1},
		{"", "/steps/0", -1},
		{"/steps/1/operation", "/steps/1/operation", 0},
		{"/steps/1", "/steps/0/update", 1},
	}
	for _, tc := range cases {
		got := comparePointers(tc.a, tc.b)
		if (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) {
			t.Errorf("comparePointers(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}
