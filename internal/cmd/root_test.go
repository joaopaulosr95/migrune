package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestEnvPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		env   map[string]string
		flag  string
		want  string
		usage bool // whether preRun must return a *UsageError
	}{
		{name: "default", flag: "log-level", want: "info"},
		{name: "env over default", env: map[string]string{"MIGRUNE_LOG_FORMAT": "json"}, flag: "log-format", want: "json"},
		{name: "flag over env", args: []string{"--log-format", "text"}, env: map[string]string{"MIGRUNE_LOG_FORMAT": "json"}, flag: "log-format", want: "text"},
		{name: "flag from env", env: map[string]string{"MIGRUNE_LOG_LEVEL": "debug"}, flag: "log-level", want: "debug"},
		{name: "log level flag over env", args: []string{"--log-level", "warn"}, env: map[string]string{"MIGRUNE_LOG_LEVEL": "debug"}, flag: "log-level", want: "warn"},
		{name: "flag default", flag: "log-format", want: "text"},
		{name: "invalid log level from env", env: map[string]string{"MIGRUNE_LOG_LEVEL": "loud"}, usage: true},
		{name: "invalid log format from env", env: map[string]string{"MIGRUNE_LOG_FORMAT": "xml"}, usage: true},
		{name: "invalid log level", args: []string{"--log-level", "loud"}, usage: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			a := newApp(&bytes.Buffer{}, &bytes.Buffer{})
			// Only flag parsing, env binding and the logger setup are under
			// test: parse the flags and run the pre-run setup.
			c := a.root
			if err := c.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			err := a.preRun(c, nil)
			var usage *UsageError
			if tc.usage {
				if !errors.As(err, &usage) {
					t.Fatalf("got error %v, want a *UsageError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("preRun: %v", err)
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
		{name: "version", args: []string{"--version"}},
		{name: "unknown flag", args: []string{"--nope"}, usage: true},
		{name: "missing flag value", args: []string{"--log-level"}, usage: true},
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
		{name: "success", args: []string{"--version"}},
		{name: "success, cancelled", args: []string{"--version"}, cancelled: true},
		{name: "usage error", args: []string{"--nope"}, usage: true},
		{name: "usage error, cancelled", args: []string{"--nope"}, cancelled: true, usage: true},
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
		{name: "text", want: `level=INFO msg=ready count=1`},
		{name: "json", args: []string{"--log-format", "json"}, want: `"level":"INFO","msg":"ready","count":1`},
		{name: "level filters", args: []string{"--log-level", "warn"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			a := newApp(&stdout, &stderr)
			if err := a.root.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			if err := a.preRun(a.root, nil); err != nil {
				t.Fatal(err)
			}
			a.log.Info("ready", "count", 1)
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
		{name: "default", args: []string{"--version"}},
		{name: "flag", args: []string{"--version", "--log-format", "json"}, want: true},
		{name: "env", args: []string{"--version"}, env: "json", want: true},
		{name: "flag wins over env", args: []string{"--version", "--log-format", "text"}, env: "json"},
		{name: "unknown flag, env", args: []string{"--nope"}, env: "json", want: true},
		{name: "unknown flag after the flag", args: []string{"--log-format", "json", "--nope"}, want: true},
		{name: "invalid log format", args: []string{"--version", "--log-format", "xml"}},
		{name: "invalid log format in env", args: []string{"--version"}, env: "xml"},
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
