package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// passDir holds migrations that lint without findings.
var passDir = filepath.Join("..", "lint", "testdata", "rules", "nested-out-merge", "pass")

func TestEnvPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		env   map[string]string
		flag  string
		want  string
		usage bool // whether Execute must return a *UsageError
	}{
		{name: "default", args: []string{"lint"}, flag: "output", want: "text"},
		{name: "env over default", args: []string{"lint"}, env: map[string]string{"MIGRUNE_OUTPUT": "json"}, flag: "output", want: "json"},
		{name: "flag over env", args: []string{"lint", "--output", "text"}, env: map[string]string{"MIGRUNE_OUTPUT": "json"}, flag: "output", want: "text"},
		{name: "shorthand flag over env", args: []string{"lint", "-o", "text"}, env: map[string]string{"MIGRUNE_OUTPUT": "json"}, flag: "output", want: "text"},
		{name: "subcommand flag from env", args: []string{"lint"}, env: map[string]string{"MIGRUNE_DB": "app"}, flag: "db", want: "app"},
		{name: "subcommand flag over env", args: []string{"lint", "--db", "billing"}, env: map[string]string{"MIGRUNE_DB": "app"}, flag: "db", want: "billing"},
		{name: "uri from env", args: []string{"lint"}, env: map[string]string{"MIGRUNE_URI": "mongodb://localhost"}, flag: "uri", want: "mongodb://localhost"},
		{name: "inherited flag from env", args: []string{"lint"}, env: map[string]string{"MIGRUNE_LOG_LEVEL": "debug"}, flag: "log-level", want: "debug"},
		{name: "inherited flag over env", args: []string{"lint", "--log-level", "warn"}, env: map[string]string{"MIGRUNE_LOG_LEVEL": "debug"}, flag: "log-level", want: "warn"},
		{name: "inherited flag default", args: []string{"lint"}, flag: "log-format", want: "text"},
		{name: "invalid log level from env", args: []string{"lint"}, env: map[string]string{"MIGRUNE_LOG_LEVEL": "loud"}, usage: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			a := newApp(&bytes.Buffer{}, &bytes.Buffer{})
			// Only flag parsing and env binding are under test: replace
			// lint with a command that does nothing.
			c, _, err := a.root.Find([]string{"lint"})
			if err != nil {
				t.Fatal(err)
			}
			c.RunE = func(*cobra.Command, []string) error { return nil }
			err = a.execute(context.Background(), tc.args)
			var usage *UsageError
			if tc.usage {
				if !errors.As(err, &usage) {
					t.Fatalf("got error %v, want a *UsageError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got := c.Flags().Lookup(tc.flag).Value.String(); got != tc.want {
				t.Errorf("--%s = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}
}

func TestExecuteErrors(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		usage bool
	}{
		{name: "help", args: []string{"--help"}},
		{name: "no command", args: nil},
		{name: "command", args: []string{"lint", "--dir", passDir}},
		{name: "missing flag", args: []string{"lint"}, usage: true},
		{name: "unknown command", args: []string{"migrate"}, usage: true},
		{name: "unknown flag", args: []string{"lint", "--nope"}, usage: true},
		{name: "unexpected argument", args: []string{"lint", "extra"}, usage: true},
		{name: "invalid log format", args: []string{"lint", "--log-format", "xml"}, usage: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Execute(context.Background(), tc.args, &bytes.Buffer{}, &bytes.Buffer{})
			var usage *UsageError
			switch {
			case tc.usage && !errors.As(err, &usage):
				t.Errorf("got error %v, want a *UsageError", err)
			case !tc.usage && err != nil:
				t.Errorf("got error %v, want nil", err)
			}
		})
	}
}

// TestExecuteInterrupted checks Execute reports an interruption when its
// context was cancelled before the command's outcome was decided, whatever
// the outcome.
func TestExecuteInterrupted(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		cancelled bool
		usage     bool
	}{
		{name: "success", args: []string{"lint", "--dir", passDir}},
		{name: "success, cancelled", args: []string{"lint", "--dir", passDir}, cancelled: true},
		{name: "usage error", args: []string{"lint"}, usage: true},
		{name: "usage error, cancelled", args: []string{"lint"}, cancelled: true, usage: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			out, err := Execute(ctx, tc.args, &bytes.Buffer{}, &bytes.Buffer{})
			var usage *UsageError
			if tc.usage != errors.As(err, &usage) || !tc.usage && err != nil {
				t.Errorf("error %v, want a usage error: %v", err, tc.usage)
			}
			if out.Interrupted != tc.cancelled {
				t.Errorf("interrupted = %v, want %v", out.Interrupted, tc.cancelled)
			}
		})
	}
}

func TestLogFormat(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "text", args: []string{"lint", "--dir", passDir}, want: `level=INFO msg="lint passed" warnings=0`},
		{name: "json", args: []string{"lint", "--dir", passDir, "--log-format", "json"}, want: `"level":"INFO","msg":"lint passed","warnings":0`},
		{name: "level filters", args: []string{"lint", "--dir", passDir, "--log-level", "warn"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if _, err := Execute(context.Background(), tc.args, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want logs on stderr only", stdout.String())
			}
			got := stderr.String()
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestExecuteJSONLogs checks Outcome.JSONLogs follows --log-format, then
// MIGRUNE_LOG_FORMAT, also when the run stops before the logger is set up.
func TestExecuteJSONLogs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{name: "default", args: []string{"lint", "--dir", passDir}},
		{name: "flag", args: []string{"lint", "--dir", passDir, "--log-format", "json"}, want: true},
		{name: "env", args: []string{"lint", "--dir", passDir}, env: "json", want: true},
		{name: "flag wins over env", args: []string{"lint", "--dir", passDir, "--log-format", "text"}, env: "json"},
		{name: "usage error", args: []string{"lint", "--log-format", "json"}, want: true},
		{name: "unknown flag, env", args: []string{"lint", "--nope"}, env: "json", want: true},
		{name: "unknown flag after the flag", args: []string{"lint", "--log-format", "json", "--nope"}, want: true},
		{name: "invalid log level, env", args: []string{"lint", "--dir", passDir, "--log-level", "loud"}, env: "json", want: true},
		{name: "invalid log format", args: []string{"lint", "--dir", passDir, "--log-format", "xml"}},
		{name: "invalid log format in env", args: []string{"lint", "--dir", passDir}, env: "xml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MIGRUNE_LOG_FORMAT", tc.env)
			if tc.env == "" {
				os.Unsetenv("MIGRUNE_LOG_FORMAT")
			}
			out, _ := Execute(context.Background(), tc.args, &bytes.Buffer{}, &bytes.Buffer{})
			if out.JSONLogs != tc.want {
				t.Errorf("JSONLogs = %v, want %v", out.JSONLogs, tc.want)
			}
		})
	}
}

func TestVersionFlag(t *testing.T) {
	var out bytes.Buffer
	if _, err := Execute(context.Background(), []string{"--version"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if want := "migrune version " + Version() + "\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}
