package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestURIFlagWarning checks a --uri given on the command line logs a
// warning, without its value, and that MIGRUNE_URI or no URI logs none.
// Every case fails before connecting.
func TestURIFlagWarning(t *testing.T) {
	const uri = "mongodb://ada:s3cret@127.0.0.1:1/"
	missing := filepath.Join(t.TempDir(), "missing")
	cases := []struct {
		name string
		args []string
		env  string // MIGRUNE_URI
		want bool
	}{
		{name: "plan flag", args: []string{"plan", "--dir", missing, "--uri", uri}, want: true},
		{name: "apply flag", args: []string{"apply", "--no-digest", "--dir", missing, "--uri", uri}, want: true},
		{name: "drift flag", args: []string{"drift", "--dir", missing, "--uri", uri}, want: true},
		{name: "lint flag", args: []string{"lint", "--dir", passDir, "--uri", uri}, want: true},
		{name: "flag over env", args: []string{"plan", "--dir", missing, "--uri", uri}, env: uri, want: true},
		{name: "plan env", args: []string{"plan", "--dir", missing}, env: uri},
		{name: "apply env", args: []string{"apply", "--no-digest", "--dir", missing}, env: uri},
		{name: "lint env", args: []string{"lint", "--dir", passDir}, env: uri},
		{name: "lint offline", args: []string{"lint", "--dir", passDir}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MIGRUNE_URI", tc.env)
			t.Setenv("MIGRUNE_DB", "")
			var stdout, stderr bytes.Buffer
			_, err := Execute(context.Background(), tc.args, &stdout, &stderr)
			t.Logf("err %v\nstderr:\n%s", err, stderr.String())
			if got := strings.Count(stderr.String(), uriFlagWarning); got != b2i(tc.want) {
				t.Errorf("logged the --uri warning %d times, want %d", got, b2i(tc.want))
			}
			if strings.Contains(stderr.String()+stdout.String(), "s3cret") {
				t.Error("the output quotes the URI")
			}
		})
	}
}

// TestApplyDigest checks apply needs --digest-in unless --no-digest is set,
// and --mutable needs --no-digest, from flags or the environment. The
// refusals are usage errors; a run that gets past them fails loading the
// missing directory, before connecting.
func TestApplyDigest(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	digest := filepath.Join(runnerFixtures("digest"), "one.digest")
	cases := []struct {
		name  string
		args  []string
		env   map[string]string
		usage string // "" means the run gets as far as loading the directory
	}{
		{name: "neither", args: []string{"apply"},
			usage: "apply needs --digest-in (the file plan --digest-out wrote); use --no-digest for local runs"},
		{name: "approver only", args: []string{"apply", "--approved-by-name", "Ada Lovelace"}, usage: "apply needs --digest-in"},
		{name: "digest", args: []string{"apply", "--digest-in", digest}},
		{name: "digest from env", args: []string{"apply"}, env: map[string]string{"MIGRUNE_DIGEST_IN": digest}},
		{name: "no digest", args: []string{"apply", "--no-digest"}},
		{name: "no digest from env", args: []string{"apply"}, env: map[string]string{"MIGRUNE_NO_DIGEST": "true"}},
		{name: "both", args: []string{"apply", "--no-digest", "--digest-in", digest}, usage: "--no-digest can't be used with --digest-in"},
		{name: "mutable", args: []string{"apply", "--mutable"}, usage: "--mutable needs --no-digest"},
		{name: "mutable with a digest", args: []string{"apply", "--mutable", "--digest-in", digest}, usage: "--mutable needs --no-digest"},
		{name: "mutable with a digest from env", args: []string{"apply"},
			env: map[string]string{"MIGRUNE_MUTABLE": "true", "MIGRUNE_DIGEST_IN": digest}, usage: "--mutable needs --no-digest"},
		{name: "mutable without a digest", args: []string{"apply", "--mutable", "--no-digest"}},
		{name: "plan", args: []string{"plan"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MIGRUNE_URI", "mongodb://127.0.0.1:1/")
			t.Setenv("MIGRUNE_DB", "app")
			t.Setenv("MIGRUNE_DIR", missing)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			_, err := Execute(context.Background(), tc.args, &stdout, &stderr)
			t.Logf("err %v\nstderr:\n%s", err, stderr.String())
			isUsage := errors.As(err, new(*UsageError))
			if tc.usage != "" {
				if !isUsage || !strings.Contains(err.Error(), tc.usage) {
					t.Fatalf("got %v, want a usage error containing %q", err, tc.usage)
				}
				return
			}
			if err == nil || isUsage || !strings.Contains(err.Error(), "migrations directory") || strings.Contains(stderr.String(), "connecting") {
				t.Fatalf("got %v, want a failure loading the directory", err)
			}
		})
	}
}

// TestNoWarnings runs the approval flow as CI does, with MIGRUNE_URI, a
// digest and an approver, on a replica set: neither plan nor apply logs the
// --uri warning.
func TestNoWarnings(t *testing.T) {
	uri, db := testTarget(t)
	dir := migrationsDir(t, setFlag)
	digest := filepath.Join(t.TempDir(), "plan.digest")
	t.Setenv("MIGRUNE_URI", uri)
	t.Setenv("MIGRUNE_DB", db.Name())
	for _, args := range [][]string{
		{"plan", "--dir", dir, "--digest-out", digest},
		{"apply", "--init", "--dir", dir, "--digest-in", digest, "--approved-by-name", "Ada Lovelace"},
	} {
		var stdout, stderr bytes.Buffer
		if _, err := Execute(context.Background(), args, &stdout, &stderr); err != nil {
			t.Fatalf("%s: %v\nstderr:\n%s", args[0], err, stderr.String())
		}
		for _, w := range []string{uriFlagWarning} {
			if strings.Contains(stderr.String(), w) {
				t.Errorf("%s logged %q:\n%s", args[0], w, stderr.String())
			}
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
