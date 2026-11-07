package runner

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

func TestClassify(t *testing.T) {
	m := func(id, sum string) migration.Migration { return migration.Migration{ID: id, Checksum: sum} }
	files := []migration.Migration{m("1-a", "a"), m("2-b", "b"), m("3-c", "c")}
	cases := []struct {
		name     string
		finished map[string]string
		mutable  bool
		pending  []string
		changed  []string
		missing  []string
	}{
		{name: "nothing applied", pending: []string{"1-a", "2-b", "3-c"}},
		{name: "some applied", finished: map[string]string{"1-a": "a"}, pending: []string{"2-b", "3-c"}},
		{name: "all applied", finished: map[string]string{"1-a": "a", "2-b": "b", "3-c": "c"}},
		{name: "applied but missing", finished: map[string]string{"0-gone": "x", "1-a": "a", "0-also": "y"},
			pending: []string{"2-b", "3-c"}, missing: []string{"0-also", "0-gone"}},
		{name: "changed", finished: map[string]string{"1-a": "a", "2-b": "old"},
			pending: []string{"3-c"}, changed: []string{"2-b"}},
		{name: "changed and mutable", finished: map[string]string{"1-a": "a", "2-b": "old"}, mutable: true,
			pending: []string{"2-b", "3-c"}, changed: []string{"2-b"}},
	}
	ids := func(ms []migration.Migration) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finished := make(map[string]migration.Applied, len(tc.finished))
			for id, sum := range tc.finished {
				finished[id] = migration.Applied{ID: id, Checksum: sum}
			}
			s := classify(files, finished, tc.mutable)
			var missing []string
			for _, a := range s.missing {
				missing = append(missing, a.ID)
			}
			if !slices.Equal(ids(s.pending), tc.pending) || !slices.Equal(ids(s.changed), tc.changed) || !slices.Equal(missing, tc.missing) {
				t.Errorf("pending %v changed %v missing %v, want %v %v %v",
					ids(s.pending), ids(s.changed), missing, tc.pending, tc.changed, tc.missing)
			}
		})
	}
}

func TestChecksumError(t *testing.T) {
	err := &ChecksumError{IDs: []string{"2026-11-01-001-a", "2026-11-01-002-b"}}
	if !strings.Contains(err.Error(), "2026-11-01-001-a, 2026-11-01-002-b changed") {
		t.Errorf("Error() = %q, want both ids", err.Error())
	}
}

// TestAppliedMissingError checks the error is one line naming every id.
func TestAppliedMissingError(t *testing.T) {
	err := &AppliedMissingError{Migrations: []migration.Applied{{ID: "2026-01-04-001-a"}, {ID: "2026-01-05-001-b", Description: "Line one\nline two"}}}
	if got := err.Error(); strings.Contains(got, "\n") || !strings.Contains(got, "the file of 2026-01-04-001-a, 2026-01-05-001-b is missing") {
		t.Errorf("Error() = %q, want one line with both ids", got)
	}
}

// TestClassifyDatedAhead checks a file's date doesn't affect whether it is
// pending: one dated next year is pending like any other.
func TestClassifyDatedAhead(t *testing.T) {
	next := time.Now().UTC().AddDate(1, 0, 0).Format(migration.DateLayout) + "-001-next-year"
	s := classify([]migration.Migration{{ID: "2026-01-01-001-a", Checksum: "a"}, {ID: next, Checksum: "n"}},
		map[string]migration.Applied{"2026-01-01-001-a": {ID: "2026-01-01-001-a", Checksum: "a"}}, false)
	if len(s.pending) != 1 || s.pending[0].ID != next || len(s.missing) != 0 {
		t.Errorf("pending %v missing %v, want only %s pending", s.pending, s.missing, next)
	}
}
