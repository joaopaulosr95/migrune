package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/joaopaulosr95/migrune/internal/lint"
	"github.com/joaopaulosr95/migrune/internal/migration"
	"github.com/joaopaulosr95/migrune/internal/runner"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type lintOptions struct {
	conn            connFlags
	expectedIndexes string
	output          string
}

func (a *app) newLintCmd() *cobra.Command {
	var o lintOptions
	c := &cobra.Command{
		Use:   "lint",
		Short: "Check migration files",
		Long: `Check migration files offline. With --uri and --db it also compares each
file's checksum with the changelog of the target database, read-only: a
migration whose file changed after it was applied is an error, and so is an
applied migration whose file is missing.

Each finding is printed to stdout on its own line as
"<file>: <severity>: <rule>: <message>", where severity is error or warning.
With -o json, the findings are printed as one JSON array of objects with
those four fields. lint exits with status 1 if there is any error; warnings
alone exit 0.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.lint(cmd, o) }),
	}
	fs := c.Flags()
	o.conn.register(fs)
	fs.StringVar(&o.expectedIndexes, "expected-indexes", "", "JSON file listing the indexes the migrations must produce")
	findingsOutputFlag(fs, &o.output)
	return c
}

func (a *app) lint(cmd *cobra.Command, o lintOptions) error {
	if o.conn.dir == "" {
		return &UsageError{Err: errors.New("--dir is required")}
	}
	if o.conn.uri != "" && o.conn.db == "" {
		return &UsageError{Err: errors.New("--db is required with --uri")}
	}
	if err := checkFindingsOutput(o.output); err != nil {
		return err
	}
	var expected *lint.ExpectedIndexes
	if o.expectedIndexes != "" {
		var err error
		if expected, err = lint.ReadExpectedIndexes(o.expectedIndexes); err != nil {
			return &UsageError{Err: fmt.Errorf("--expected-indexes: %w", err)}
		}
	}
	// The online check runs only with both --uri and --db.
	var applied map[string]migration.Applied
	if o.conn.uri != "" {
		db, disconnect, err := a.connect(cmd.Context(), o.conn)
		if err != nil {
			return err
		}
		defer disconnect()
		if applied, err = runner.Finished(cmd.Context(), db); err != nil {
			return err
		}
	}
	findings, err := lint.Run(o.conn.dir, expected, applied)
	if err != nil {
		return err
	}
	if err := writeFindings(cmd.OutOrStdout(), findings, o.output); err != nil {
		return fmt.Errorf("write findings: %w", err)
	}
	errs, warnings := countFindings(findings)
	if errs > 0 {
		return fmt.Errorf("lint failed: %d errors, %d warnings", errs, warnings)
	}
	a.log.Info("lint passed", "warnings", warnings)
	return nil
}

// findingsOutputFlag registers the findings format flag of lint.
func findingsOutputFlag(fs *pflag.FlagSet, p *string) {
	fs.StringVarP(p, "output", "o", "text",
		"findings format: text, one per line, or json, one array; table, the plan and apply default, means text, so one MIGRUNE_OUTPUT serves every command")
}

func checkFindingsOutput(output string) error {
	if output != "text" && output != "table" && output != "json" {
		return &UsageError{Err: fmt.Errorf("invalid --output %q: want text or json", output)}
	}
	return nil
}

// countFindings counts the errors and the warnings in findings.
func countFindings(findings []lint.Finding) (errs, warnings int) {
	for _, f := range findings {
		if f.Severity == lint.Error {
			errs++
		} else {
			warnings++
		}
	}
	return errs, warnings
}

// writeFindings prints findings one per line, or as one JSON array.
func writeFindings(w io.Writer, findings []lint.Finding, output string) error {
	if output == "json" {
		if findings == nil {
			findings = []lint.Finding{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(findings)
	}
	for _, f := range findings {
		if _, err := fmt.Fprintln(w, f); err != nil {
			return err
		}
	}
	return nil
}
