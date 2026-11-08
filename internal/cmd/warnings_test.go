package cmd

import (
	"bytes"
	"context"
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
		{name: "apply flag", args: []string{"apply", "--dir", missing, "--uri", uri}, want: true},
		{name: "lint flag", args: []string{"lint", "--dir", passDir, "--uri", uri}, want: true},
		{name: "flag over env", args: []string{"plan", "--dir", missing, "--uri", uri}, env: uri, want: true},
		{name: "plan env", args: []string{"plan", "--dir", missing}, env: uri},
		{name: "apply env", args: []string{"apply", "--dir", missing}, env: uri},
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

// TestNoWarnings runs the approval flow as CI does, with MIGRUNE_URI and an
// approver, on a replica set: neither plan nor apply logs the --uri warning.
func TestNoWarnings(t *testing.T) {
	uri, db := testTarget(t)
	dir := migrationsDir(t, setFlag)
	t.Setenv("MIGRUNE_URI", uri)
	t.Setenv("MIGRUNE_DB", db.Name())
	for _, args := range [][]string{
		{"plan", "--dir", dir},
		{"apply", "--dir", dir, "--approved-by-name", "Ada Lovelace"},
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
