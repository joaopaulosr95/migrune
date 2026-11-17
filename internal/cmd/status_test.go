package cmd

import (
	"reflect"
	"testing"
	"time"

	"github.com/joaopaulosr95/migrune/internal/migration"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

func TestMigrationStates(t *testing.T) {
	at := time.Date(2026, 1, 4, 10, 12, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	step0, step1 := 0, 1
	errors := []runner.StepError{
		{Message: "document value in first message"},
		{Step: &step0, Message: "step zero failure"},
		{Step: &step1, Message: "step one failure"},
	}
	hidden := []runner.StepError{{}, {Step: &step0}, {Step: &step1}}
	finished := map[string]migration.Applied{
		"a": {ID: "a", Checksum: "applied", AppliedAt: at, Runner: "runner-a"},
	}
	errored := map[string]runner.ErroredEntry{
		"a": {ID: "a", Checksum: "retry", CreatedAt: later, Runner: "runner-b", Errors: errors},
	}
	failed := migrationState{ID: "a", Status: "failed", Checksum: "retry", AppliedAt: &later,
		Runner: "runner-b", FailedStep: &step0, errors: hidden}
	cases := []struct {
		name         string
		files        []migration.Migration
		finished     map[string]migration.Applied
		errored      map[string]runner.ErroredEntry
		sampleValues bool
		want         []migrationState
	}{
		{
			name: "applied", files: []migration.Migration{{ID: "a", Checksum: "applied"}}, finished: finished,
			want: []migrationState{{ID: "a", Status: "applied", Checksum: "applied", AppliedAt: &at, Runner: "runner-a"}},
		},
		{
			name: "changed", files: []migration.Migration{{ID: "a", Checksum: "edited"}}, finished: finished,
			want: []migrationState{{ID: "a", Status: "changed", Checksum: "applied", AppliedAt: &at, Runner: "runner-a"}},
		},
		{
			name: "missing", files: []migration.Migration{}, finished: finished,
			want: []migrationState{{ID: "a", Status: "missing", Checksum: "applied", AppliedAt: &at, Runner: "runner-a"}},
		},
		{
			name: "pending", files: []migration.Migration{{ID: "a", Checksum: "new"}},
			want: []migrationState{{ID: "a", Status: "pending", Checksum: "new"}},
		},
		{
			name: "failed instead of pending", files: []migration.Migration{{ID: "a", Checksum: "edited"}}, errored: errored,
			want: []migrationState{failed},
		},
		{
			name: "failed mutable rerun", files: []migration.Migration{{ID: "a", Checksum: "edited"}}, finished: finished, errored: errored,
			want: []migrationState{failed},
		},
		{
			name: "failed despite missing file", files: []migration.Migration{}, finished: finished, errored: errored,
			want: []migrationState{failed},
		},
		{
			name: "finished after error", files: []migration.Migration{{ID: "a", Checksum: "applied"}},
			finished: map[string]migration.Applied{"a": {ID: "a", Checksum: "applied", AppliedAt: later, Runner: "runner-a"}},
			errored:  map[string]runner.ErroredEntry{"a": {ID: "a", Checksum: "retry", CreatedAt: at, Runner: "runner-b", Errors: errors}},
			want:     []migrationState{{ID: "a", Status: "applied", Checksum: "applied", AppliedAt: &later, Runner: "runner-a"}},
		},
		{
			name: "equal timestamps keep finished", files: []migration.Migration{{ID: "a", Checksum: "applied"}}, finished: finished,
			errored: map[string]runner.ErroredEntry{"a": {ID: "a", CreatedAt: at, Errors: errors}},
			want:    []migrationState{{ID: "a", Status: "applied", Checksum: "applied", AppliedAt: &at, Runner: "runner-a"}},
		},
		{
			name: "without dir only applied and failed", errored: errored,
			finished: map[string]migration.Applied{
				"a": {ID: "a", Checksum: "applied", AppliedAt: at, Runner: "runner-a"},
				"b": {ID: "b", Checksum: "no-file", AppliedAt: at, Runner: "runner-c"},
			},
			want: []migrationState{failed, {ID: "b", Status: "applied", Checksum: "no-file", AppliedAt: &at, Runner: "runner-c"}},
		},
		{
			name: "sample values include first message and first step", errored: errored, sampleValues: true,
			want: []migrationState{{ID: "a", Status: "failed", Checksum: "retry", AppliedAt: &later, Runner: "runner-b",
				FailedStep: &step0, Error: "document value in first message", errors: errors}},
		},
		{
			name:    "failed without step errors",
			errored: map[string]runner.ErroredEntry{"a": {ID: "a", Checksum: "retry", CreatedAt: later, Runner: "runner-b"}},
			want:    []migrationState{{ID: "a", Status: "failed", Checksum: "retry", AppliedAt: &later, Runner: "runner-b"}},
		},
		{
			name: "sorted union", files: []migration.Migration{{ID: "c", Checksum: "c"}, {ID: "b", Checksum: "b"}},
			finished: map[string]migration.Applied{"d": {ID: "d", Checksum: "d", AppliedAt: at}}, errored: errored,
			want: []migrationState{failed, {ID: "b", Status: "pending", Checksum: "b"}, {ID: "c", Status: "pending", Checksum: "c"},
				{ID: "d", Status: "missing", Checksum: "d", AppliedAt: &at}},
		},
		{name: "empty", want: []migrationState{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := migrationStates(tc.files, tc.finished, tc.errored, tc.sampleValues)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("migrationStates = %+v, want %+v", got, tc.want)
			}
		})
	}
}
