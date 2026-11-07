package runner

import (
	"fmt"
	"strings"
	"time"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// RowStatus is the outcome of one migration in a report. For plan it
// is what apply would do.
type RowStatus string

const (
	RowApplied RowStatus = "applied"
	RowErrored RowStatus = "errored"
	// RowInterrupted is the migration in progress when the run was
	// interrupted. It stays pending.
	RowInterrupted RowStatus = "interrupted"
	// RowSkipped is a migration not reached because an earlier one errored
	// or the run was interrupted or failed a check.
	RowSkipped RowStatus = "skipped"
)

// Report is the outcome of a plan or apply run. docs/reference/report.md
// documents its JSON shape.
type Report struct {
	// Version is migrune's version, set by the caller.
	Version string `json:"version"`
	// Command is "plan" or "apply".
	Command string `json:"command"`
	// Database is the target database's name.
	Database string `json:"database"`
	// StartedAt is when the run started, in UTC, to the millisecond.
	StartedAt time.Time `json:"startedAt"`
	Runner    string    `json:"runner"`
	// ApprovedBy is who approved an apply run, as given; absent in plan and
	// when neither name nor email was given.
	ApprovedBy *ApprovedBy `json:"approvedBy,omitempty"`
	// NewDatabase is set when the target database had no collections when
	// the run started: a mistyped --db targets a new, empty database.
	NewDatabase bool `json:"newDatabase"`
	// Migrations has one row per migration considered, in id order.
	Migrations []Row `json:"migrations"`
	// AppliedButMissing lists the migrations Finished in the changelog whose
	// file is gone, with what their latest Finished entry stored. Sorted by id.
	AppliedButMissing []migration.Applied `json:"appliedButMissing"`
	Totals            Totals              `json:"totals"`
	// Error is why the run failed, if it did.
	Error string `json:"error,omitempty"`
}

// Row is one migration in a report.
type Row struct {
	ID         string         `json:"id"`
	Kind       migration.Kind `json:"kind"`
	Status     RowStatus      `json:"status"`
	DurationMs int64          `json:"durationMs"`
	Error      string         `json:"error,omitempty"`
	// Steps has one item per step of a data migration whose first run
	// completed, with what MongoDB reported for it: in plan, what apply
	// would do to the data as it is now; in apply, what it did. Absent for
	// skipped rows and when the first run failed.
	Steps []DataStep `json:"steps,omitempty"`
}

// DataStep is what one step of a data migration matched and changed, in the
// first of its two runs. The counts that don't apply to the operation are
// absent: deleteMany has only Deleted, the updates the others.
type DataStep struct {
	// Index is the step's index in file order.
	Index      int    `json:"index"`
	Operation  string `json:"operation"`
	Collection string `json:"collection"`
	Matched    *int64 `json:"matched,omitempty"`
	Modified   *int64 `json:"modified,omitempty"`
	Upserted   *int64 `json:"upserted,omitempty"`
	Deleted    *int64 `json:"deleted,omitempty"`
}

// Counts renders the counts that apply, e.g. "matched 3, modified 3,
// upserted 0".
func (s DataStep) Counts() string {
	var parts []string
	for _, c := range s.counts() {
		parts = append(parts, fmt.Sprintf("%s %d", c.name, c.n))
	}
	return strings.Join(parts, ", ")
}

type count struct {
	name string
	n    int64
}

// counts returns the counts that apply, in a fixed order.
func (s DataStep) counts() []count {
	var out []count
	for _, c := range []struct {
		name string
		n    *int64
	}{{"matched", s.Matched}, {"modified", s.Modified}, {"upserted", s.Upserted}, {"deleted", s.Deleted}} {
		if c.n != nil {
			out = append(out, count{c.name, *c.n})
		}
	}
	return out
}

func (s DataStep) logAttrs() []any {
	attrs := []any{"step", s.Index, "operation", s.Operation, "collection", s.Collection}
	for _, c := range s.counts() {
		attrs = append(attrs, c.name, c.n)
	}
	return attrs
}

// Totals counts the rows of a report by status.
type Totals struct {
	Migrations  int `json:"migrations"`
	Applied     int `json:"applied"`
	Errored     int `json:"errored"`
	Interrupted int `json:"interrupted"`
	Skipped     int `json:"skipped"`
	// DurationMs is the duration of the whole run.
	DurationMs int64 `json:"durationMs"`
}

// row returns the row for id, or nil.
func (r *Report) row(id string) *Row {
	for i := range r.Migrations {
		if r.Migrations[i].ID == id {
			return &r.Migrations[i]
		}
	}
	return nil
}

// finish fills the totals and the run's error.
func (r *Report) finish(d time.Duration, err error) {
	t := Totals{Migrations: len(r.Migrations), DurationMs: d.Milliseconds()}
	for _, row := range r.Migrations {
		switch row.Status {
		case RowApplied:
			t.Applied++
		case RowErrored:
			t.Errored++
		case RowInterrupted:
			t.Interrupted++
		case RowSkipped:
			t.Skipped++
		}
	}
	r.Totals = t
	if err != nil {
		r.Error = err.Error()
	}
}
