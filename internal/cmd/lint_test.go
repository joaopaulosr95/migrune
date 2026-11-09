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
		{name: "clean", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "pass")}},
		{name: "errors", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "fail")},
			failed: true, message: "lint failed: 1 errors, 0 warnings",
			stdout: "2026-11-01-001-merge-first.json: error: merge-last: /steps/0/pipeline/0: $merge is stage 1 of 2"},
		{name: "warnings only", args: []string{"lint", "--dir", filepath.Join(fixtures, "expected", "migrations"),
			"--expected-indexes", filepath.Join(fixtures, "expected", "extra.json")},
			stdout: "warning: index-extra:"},
		{name: "dir from env", env: map[string]string{"MIGRUNE_DIR": filepath.Join(fixtures, "rules", "merge-last", "fail")},
			args: []string{"lint"}, failed: true, stdout: "merge-last"},
		{name: "missing dir flag", args: []string{"lint"}, usage: true, message: "--dir is required"},
		{name: "unreadable dir", args: []string{"lint", "--dir", filepath.Join(fixtures, "does-not-exist")}, failed: true, message: "read migrations directory"},
		{name: "uri without db", args: []string{"lint", "--dir", fixtures, "--uri", "mongodb://localhost"},
			usage: true, message: "--db is required with --uri"},
		{name: "uri from env without db", args: []string{"lint", "--dir", fixtures}, env: map[string]string{"MIGRUNE_URI": "mongodb://localhost"},
			usage: true, message: "--db is required with --uri"},
		{name: "db alone stays offline", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "pass"), "--db", "app"}},
		{name: "invalid namespace online", args: []string{"lint", "--dir", fixtures, "--uri", "mongodb://127.0.0.1:1", "--db", "app", "--namespace", "API"},
			usage: true, message: `--namespace: invalid namespace "API"`},
		{name: "invalid namespace offline", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "pass"), "--namespace", "API"},
			usage: true, message: `--namespace: invalid namespace "API"`},
		{name: "namespace unused offline", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "pass"), "--namespace", "api"}},
		{name: "invalid uri", args: []string{"lint", "--dir", fixtures, "--uri", "postgres://user:secret@localhost", "--db", "app"},
			usage: true, message: "--uri is not a valid MongoDB connection string"},
		{name: "json clean", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "pass"), "-o", "json"},
			stdout: "[]\n"},
		{name: "json errors", args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "fail"), "--output", "json"},
			failed: true, message: "lint failed: 1 errors, 0 warnings",
			stdout: `fail/2026-11-01-001-merge-first.json",
    "severity": "error",
    "rule": "merge-last",
    "message": "/steps/0/pipeline/0: $merge is stage 1 of 2`},
		{name: "json warnings only", args: []string{"lint", "--dir", filepath.Join(fixtures, "expected", "migrations"),
			"--expected-indexes", filepath.Join(fixtures, "expected", "extra.json"), "-o", "json"},
			stdout: `"severity": "warning",
    "rule": "index-extra",`},
		{name: "output from env", env: map[string]string{"MIGRUNE_OUTPUT": "json"},
			args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "pass")}, stdout: "[]\n"},
		{name: "table output is text", env: map[string]string{"MIGRUNE_OUTPUT": "table"},
			args: []string{"lint", "--dir", filepath.Join(fixtures, "rules", "merge-last", "fail")}, failed: true,
			stdout: "2026-11-01-001-merge-first.json: error: merge-last:"},
		{name: "bad output", args: []string{"lint", "--dir", fixtures, "-o", "yaml"}, usage: true, message: `invalid --output "yaml"`},
		{name: "invalid expected indexes", args: []string{"lint", "--dir", fixtures,
			"--expected-indexes", filepath.Join(fixtures, "expected-invalid", "unknown-field.json")},
			usage: true, message: `unknown field "version"`},
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
