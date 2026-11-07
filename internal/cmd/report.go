package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// writeReport renders a plan or apply report as a table or as JSON.
func writeReport(w io.Writer, rep *runner.Report, output string) error {
	if output == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeTable(w, rep)
}

func writeTable(w io.Writer, rep *runner.Report) error {
	ew := &errWriter{w: w}
	if rep.Command == string(runner.ModePlan) {
		fmt.Fprintln(ew, "Plan: each status is what apply would do. Nothing was committed.")
	}
	if rep.NewDatabase {
		fmt.Fprintf(ew, "Database: %s (new: it had no collections)\n", rep.Database)
	} else {
		fmt.Fprintf(ew, "Database: %s\n", rep.Database)
	}
	fmt.Fprintln(ew)
	if len(rep.Migrations) == 0 {
		fmt.Fprintln(ew, "No pending migrations.")
	} else {
		var table bytes.Buffer
		tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "MIGRATION\tKIND\tSTATUS\tDURATION\tERROR")
		for _, row := range rep.Migrations {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", row.ID, row.Kind, row.Status, duration(row.DurationMs), row.Error)
		}
		_ = tw.Flush()
		// The first line is the header, then one per row. Rows without an
		// error end in padding. A row's details go under it, outside the
		// tabwriter so they don't widen the columns.
		i := -1
		for line := range strings.Lines(table.String()) {
			fmt.Fprintln(ew, strings.TrimRight(line, " \n"))
			if i >= 0 {
				writeRowDetails(ew, rep.Command, rep.Migrations[i])
			}
			i++
		}
	}
	if len(rep.AppliedButMissing) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintln(ew, "Applied but missing (Finished in the changelog, no file):")
		for _, a := range rep.AppliedButMissing {
			fmt.Fprintf(ew, "  %s: %s\n", a.ID, a.MissingContext())
		}
	}
	t := rep.Totals
	fmt.Fprintln(ew)
	fmt.Fprintf(ew, "Total: %d %s: %d applied, %d errored, %d interrupted, %d skipped in %s\n",
		t.Migrations, plural(t.Migrations, "migration"), t.Applied, t.Errored, t.Interrupted, t.Skipped, duration(t.DurationMs))
	if rep.Error != "" {
		// A run can fail several checks at once, one per line.
		fmt.Fprintln(ew)
		prefix := "Error: "
		for _, line := range strings.Split(rep.Error, "\n") {
			fmt.Fprintln(ew, prefix+line)
			prefix = "  "
		}
	}
	return ew.err
}

// writeRowDetails writes the lines under a row: for a data migration, one
// per step with its counts; for an index migration, one per index step; for a
// merge migration, its counts and samples.
func writeRowDetails(w io.Writer, command string, row runner.Row) {
	for _, s := range row.Steps {
		fmt.Fprintf(w, "  /steps/%d %s on %s: %s\n", s.Index, s.Operation, s.Collection, s.Counts())
	}
	if counts := row.Counts(); counts != "" {
		fmt.Fprintln(w, "  merge: "+counts)
	}
	for _, s := range row.Samples {
		var fields []string
		for _, f := range s.Fields {
			fields = append(fields, f.Field+": "+f.Describe())
		}
		fmt.Fprintf(w, "  differs at /steps/%d %s: %s\n", s.Step, s.Key, strings.Join(fields, "; "))
	}
	for _, c := range row.Indexes {
		line := fmt.Sprintf("  index %s: %s on %s", indexLabel(command, c), c.Name, c.Collection)
		if c.Keys != nil {
			line += ", keys " + string(c.Keys)
		}
		if c.Options != nil {
			line += ", options " + string(c.Options)
		}
		if c.WouldExpire != nil {
			line += fmt.Sprintf(", wouldExpire %d", *c.WouldExpire)
		}
		if c.Conflict != "" {
			line += ": " + c.Conflict
		}
		if c.Warning != "" {
			line += ": warning: " + c.Warning
		}
		fmt.Fprintln(w, line)
	}
}

// indexLabel says what an index step would do (plan) or did (apply), in the
// words of the logs.
func indexLabel(command string, c runner.IndexChange) string {
	plan := command == string(runner.ModePlan)
	switch {
	case c.Conflict != "":
		return "to " + string(c.Action) + ", would fail"
	case c.Action == runner.IndexPresent:
		return "already present"
	case c.Action == runner.IndexAbsent:
		return "already absent"
	case c.Action == runner.IndexDiffers:
		return "already present, collation not comparable"
	case plan:
		return "to " + string(c.Action)
	case c.Action == runner.IndexCreate:
		return "created"
	case c.Action == runner.IndexDrop:
		return "dropped"
	}
	return string(c.Action)
}

func duration(ms int64) string { return (time.Duration(ms) * time.Millisecond).String() }

// errWriter keeps the first write error, so the table can be written without
// checking every line.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
