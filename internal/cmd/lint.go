package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/joaopaulosr95/migrune/internal/lint"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type lintOptions struct {
	dir    string
	output string
}

func (a *app) newLintCmd() *cobra.Command {
	var o lintOptions
	c := &cobra.Command{
		Use:   "lint",
		Short: "Check migration files",
		Long: `Check migration files offline.

Each finding is printed to stdout on its own line as
"<file>: <severity>: <rule>: <message>", where severity is error or warning.
With -o json, the findings are printed as one JSON array of objects with
those four fields. lint exits with status 1 if there is any error; warnings
alone exit 0.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.lint(cmd, o) }),
	}
	fs := c.Flags()
	fs.StringVar(&o.dir, "dir", "", "directory holding the migration files")
	findingsOutputFlag(fs, &o.output)
	return c
}

func (a *app) lint(cmd *cobra.Command, o lintOptions) error {
	if o.dir == "" {
		return &UsageError{Err: errors.New("--dir is required")}
	}
	if err := checkFindingsOutput(o.output); err != nil {
		return err
	}
	findings, err := lint.Run(o.dir)
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
	fs.StringVarP(p, "output", "o", "text", "findings format: text, one per line, or json, one array")
}

func checkFindingsOutput(output string) error {
	if output != "text" && output != "json" {
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
