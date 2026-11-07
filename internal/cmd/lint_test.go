package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestLint(t *testing.T) {
	fixtures := filepath.Join("..", "lint", "testdata")
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		usage   bool   // Execute must return a *UsageError
		failed  bool   // Execute must return another error
		stdout  string // substring of stdout; "" means stdout must be empty
		message string // substring of the error
	}{
		{name: "clean", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "nested-out-merge", "pass")}},
		{name: "errors", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "nested-out-merge", "fail")},
			failed: true, message: "lint failed: 1 errors, 0 warnings",
			stdout: "2026-11-01-004-in-update-pipeline.json: error: nested-out-merge: /steps/0/update/0/$facet/x/0: $out is not allowed inside a $facet pipeline"},
		{name: "warnings only", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "empty-filter", "fail")},
			stdout: "warning: empty-filter:"},
		{name: "dir from env", env: map[string]string{"MIGRUNE_DIR": filepath.Join(fixtures, "rules", "nested-out-merge", "fail")},
			args: []string{"lint"}, failed: true, stdout: "nested-out-merge"},
		{name: "missing dir flag", args: []string{"lint"}, usage: true, message: "--dir is required"},
		{name: "unreadable dir", args: []string{"lint", "--dir", filepath.Join(fixtures, "does-not-exist")}, failed: true, message: "read migrations directory"},
		{name: "uri without db", args: []string{"lint", "--dir", fixtures, "--uri", "mongodb://localhost"},
			usage: true, message: "--db is required with --uri"},
		{name: "uri from env without db", args: []string{"lint", "--dir", fixtures}, env: map[string]string{"MIGRUNE_URI": "mongodb://localhost"},
			usage: true, message: "--db is required with --uri"},
		{name: "db alone stays offline", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "nested-out-merge", "pass"), "--db", "app"}},
		{name: "invalid uri", args: []string{"lint", "--dir", fixtures, "--uri", "postgres://user:secret@localhost", "--db", "app"},
			usage: true, message: "--uri is not a valid MongoDB connection string"},
		{name: "json clean", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "nested-out-merge", "pass"), "-o", "json"},
			stdout: "[]\n"},
		{name: "json errors", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "nested-out-merge", "fail"), "--output", "json"},
			failed: true, message: "lint failed: 1 errors, 0 warnings",
			stdout: `fail/2026-11-01-004-in-update-pipeline.json",
    "severity": "error",
    "rule": "nested-out-merge",
    "message": "/steps/0/update/0/$facet/x/0: $out is not allowed inside a $facet pipeline`},
		{name: "json warnings only", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "empty-filter", "fail"), "-o", "json"},
			stdout: `"severity": "warning",
    "rule": "empty-filter",`},
		{name: "output from env", env: map[string]string{"MIGRUNE_OUTPUT": "json"},
			args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "nested-out-merge", "pass")}, stdout: "[]\n"},
		{name: "table output is text", env: map[string]string{"MIGRUNE_OUTPUT": "table"},
			args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "nested-out-merge", "fail")}, failed: true,
			stdout: "2026-11-01-004-in-update-pipeline.json: error: nested-out-merge:"},
		{name: "bad output", args: []string{"lint", "--dir", fixtures, "-o", "yaml"}, usage: true, message: `invalid --output "yaml"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			_, err := Execute(context.Background(), tc.args, &stdout, &stderr)
			var usage *UsageError
			isUsage := errors.As(err, &usage)
			switch {
			case tc.usage && !isUsage:
				t.Fatalf("got error %v, want a *UsageError", err)
			case tc.failed && (err == nil || isUsage):
				t.Fatalf("got error %v, want a lint failure", err)
			case !tc.usage && !tc.failed && err != nil:
				t.Fatalf("got error %v, want nil", err)
			}
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q doesn't contain %q", err, tc.message)
			}
			if tc.stdout == "" && stdout.Len() != 0 || !strings.Contains(stdout.String(), tc.stdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tc.stdout)
			}
		})
	}
}
