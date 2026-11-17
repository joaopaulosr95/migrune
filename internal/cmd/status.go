package cmd

import (
	"slices"
	"strings"
	"time"

	"github.com/joaopaulosr95/migrune/internal/migration"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

type migrationState struct {
	ID         string             `json:"id"`
	Status     string             `json:"status"` // applied|failed|pending|changed|missing
	Checksum   string             `json:"checksum,omitempty"`
	AppliedAt  *time.Time         `json:"appliedAt,omitempty"` // the latest entry's createdAt: Finished, or Errored for failed
	Runner     string             `json:"runner,omitempty"`
	FailedStep *int               `json:"failedStep,omitempty"`
	Error      string             `json:"error,omitempty"` // set only with --sample-values
	errors     []runner.StepError // for the table's detail lines
}

// migrationStates classifies, sorted by id. files is nil without --dir.
func migrationStates(files []migration.Migration, finished map[string]migration.Applied,
	errored map[string]runner.ErroredEntry, sampleValues bool) []migrationState {
	states := make(map[string]migrationState, len(files)+len(finished)+len(errored))
	for _, m := range files {
		states[m.ID] = migrationState{ID: m.ID, Status: "pending", Checksum: m.Checksum}
	}
	for id, a := range finished {
		status := "applied"
		if files != nil {
			if file, ok := states[id]; !ok {
				status = "missing"
			} else if file.Checksum != a.Checksum {
				status = "changed"
			}
		}
		states[id] = migrationState{
			ID: id, Status: status, Checksum: a.Checksum, AppliedAt: &a.AppliedAt, Runner: a.Runner,
		}
	}
	for id, e := range errored {
		if a, ok := finished[id]; ok && !e.CreatedAt.After(a.AppliedAt) {
			continue
		}
		state := migrationState{
			ID: id, Status: "failed", Checksum: e.Checksum, AppliedAt: &e.CreatedAt, Runner: e.Runner,
		}
		for i, failure := range e.Errors {
			if state.FailedStep == nil && failure.Step != nil {
				state.FailedStep = failure.Step
			}
			if !sampleValues {
				failure.Message = ""
			} else if i == 0 {
				state.Error = failure.Message
			}
			state.errors = append(state.errors, failure)
		}
		states[id] = state
	}
	out := make([]migrationState, 0, len(states))
	for _, state := range states {
		out = append(out, state)
	}
	slices.SortFunc(out, func(a, b migrationState) int { return strings.Compare(a.ID, b.ID) })
	return out
}
