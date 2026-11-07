package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/joaopaulosr95/migrune/internal/cmd"
)

func TestExitCode(t *testing.T) {
	usage := &cmd.UsageError{Err: errors.New("unknown flag: --nope")}
	failure := errors.New("migration 2026-01-01-001-example failed")
	cases := []struct {
		name string
		err  error
		sig  os.Signal
		want int
	}{
		{name: "success", want: 0},
		{name: "failure", err: failure, want: 1},
		{name: "usage error", err: usage, want: 2},
		{name: "wrapped usage error", err: fmt.Errorf("flags: %w", usage), want: 2},
		{name: "SIGINT", sig: syscall.SIGINT, want: 130},
		{name: "SIGTERM", sig: syscall.SIGTERM, want: 143},
		{name: "SIGTERM wins over failure", err: failure, sig: syscall.SIGTERM, want: 143},
		{name: "SIGINT wins over usage error", err: usage, sig: syscall.SIGINT, want: 130},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err, tc.sig); got != tc.want {
				t.Errorf("exitCode(%v, %v) = %d, want %d", tc.err, tc.sig, got, tc.want)
			}
		})
	}
}

// TestWatchSignals checks the second signal exits at once with the first
// signal's code.
func TestWatchSignals(t *testing.T) {
	cases := []struct {
		name          string
		first, second os.Signal
		want          int
	}{
		{name: "SIGTERM then SIGINT", first: syscall.SIGTERM, second: syscall.SIGINT, want: 143},
		{name: "SIGINT then SIGTERM", first: syscall.SIGINT, second: syscall.SIGTERM, want: 130},
		{name: "SIGINT twice", first: syscall.SIGINT, second: syscall.SIGINT, want: 130},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sigs := make(chan os.Signal, 2)
			exited := make(chan int, 1)
			first := watchSignals(sigs, func(code int) { exited <- code })
			sigs <- tc.first
			if got := <-first; got != tc.first {
				t.Errorf("first signal %v, want %v", got, tc.first)
			}
			select {
			case code := <-exited:
				t.Fatalf("exited with %d after one signal", code)
			case <-time.After(20 * time.Millisecond):
			}
			sigs <- tc.second
			select {
			case code := <-exited:
				if code != tc.want {
					t.Errorf("exit code %d, want %d", code, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("no exit after the second signal")
			}
		})
	}
}

func TestPrintError(t *testing.T) {
	usage := &cmd.UsageError{Err: errors.New("required flag(s) \"dir\" not set")}
	failure := errors.New("migration 2026-01-01-001-example failed:\nnot idempotent")
	cases := []struct {
		name     string
		err      error
		code     int
		jsonLogs bool
		want     string
	}{
		{name: "text", err: failure, code: 1,
			want: "Error: migration 2026-01-01-001-example failed:\nnot idempotent\n"},
		{name: "text usage error", err: usage, code: 2,
			want: "Error: required flag(s) \"dir\" not set\nRun 'migrune --help' for usage.\n"},
		{name: "json", err: failure, code: 1, jsonLogs: true,
			want: `"level":"ERROR","msg":"migration 2026-01-01-001-example failed:\nnot idempotent","exitCode":1}` + "\n"},
		{name: "json usage error", err: usage, code: 2, jsonLogs: true,
			want: `"level":"ERROR","msg":"required flag(s) \"dir\" not set","exitCode":2}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			printError(&out, tc.err, tc.code, tc.jsonLogs)
			got := out.String()
			if tc.jsonLogs {
				// The record starts with the time; compare the rest.
				_, rest, ok := strings.Cut(got, `",`)
				if !ok || !strings.HasPrefix(got, `{"time":"`) {
					t.Fatalf("got %q, want a JSON log record", got)
				}
				got = rest
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFinalError runs migrune and checks how it prints the error it ends
// with: an "Error:" line by default, one JSON log record with JSON logs, set
// by the flag or the environment, and nothing else on stderr.
func TestFinalError(t *testing.T) {
	bin := buildMigrune(t)
	cases := []struct {
		name string
		args []string
		env  string
		code int
		// msg is the error: the Error: line's text, or the JSON record's
		// msg.
		msg  string
		json bool
	}{
		{name: "text usage error", args: []string{"--nope"}, code: 2, msg: "unknown flag: --nope"},
		{name: "json usage error", args: []string{"--log-format", "json", "--nope"}, code: 2, msg: "unknown flag: --nope", json: true},
		{name: "json unknown flag", args: []string{"--nope"}, env: "json", code: 2, msg: "unknown flag: --nope", json: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			c := exec.Command(bin, tc.args...)
			c.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "MIGRUNE_") })
			if tc.env != "" {
				c.Env = append(c.Env, "MIGRUNE_LOG_FORMAT="+tc.env)
			}
			c.Stdout, c.Stderr = &stdout, &stderr
			err := c.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tc.code {
				t.Fatalf("exit: %v, want status %d\nstderr:\n%s", err, tc.code, stderr.String())
			}
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			last := lines[len(lines)-1]
			if !tc.json {
				i := len(lines) - 1
				if tc.code == 2 {
					if last != "Run 'migrune --help' for usage." {
						t.Errorf("last line %q, want the pointer to --help", last)
					}
					i--
				}
				if !strings.HasPrefix(lines[i], "Error: ") || !strings.Contains(lines[i], tc.msg) {
					t.Errorf("stderr:\n%s\nwant an Error: line with %q", stderr.String(), tc.msg)
				}
				return
			}
			for _, line := range lines {
				if !json.Valid([]byte(line)) {
					t.Errorf("stderr line %q isn't JSON", line)
				}
			}
			var rec struct {
				Level    string `json:"level"`
				Msg      string `json:"msg"`
				ExitCode *int   `json:"exitCode"`
			}
			if err := json.Unmarshal([]byte(last), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Level != "ERROR" || !strings.Contains(rec.Msg, tc.msg) || rec.ExitCode == nil || *rec.ExitCode != tc.code {
				t.Errorf("last record %s, want level ERROR, msg with %q and exitCode %d", last, tc.msg, tc.code)
			}
		})
	}
}

var (
	buildOnce sync.Once
	// binDir holds the migrune binary the tests build; TestMain removes
	// it.
	binDir   string
	buildErr error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// buildMigrune returns the migrune binary, built once for all the tests.
func buildMigrune(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		if binDir, buildErr = os.MkdirTemp("", "migrune-test-"); buildErr != nil {
			return
		}
		if out, err := exec.Command("go", "build", "-o", filepath.Join(binDir, "migrune"), ".").CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build migrune: %v", buildErr)
	}
	return filepath.Join(binDir, "migrune")
}
