package runner

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// testDeployment is a changelog collection UUID for digest inputs.
const testDeployment = "0f8fad5b-d9cb-469f-a165-70867728950e"

var updateGolden = flag.Bool("update", false, "rewrite the golden files (digests, changelog and lock formats) with the current output")

// TestDigestInput checks the digest file plan writes for each input: the
// digest, then the input it hashes. The golden files pin the text byte for
// byte; the default namespace adds no namespace line. The digest on the first line is the
// SHA-256 of the lines after it, e.g. for valid.digest:
//
//	tail -n +2 testdata/digest/one.digest | shasum -a 256
func TestDigestInput(t *testing.T) {
	a := migration.Migration{ID: "2026-11-01-001-a", Checksum: "aaaa"}
	b := migration.Migration{ID: "2026-11-01-002-b", Checksum: "bbbb"}
	cases := []struct {
		golden     string
		database   string
		replicaSet string
		deployment string
		namespace  string
		pending    []migration.Migration
	}{
		{golden: "nothing-pending.digest", database: "app", replicaSet: "rs0", deployment: testDeployment},
		{golden: "one.digest", database: "app", replicaSet: "rs0", deployment: testDeployment, pending: []migration.Migration{a}},
		{golden: "sorted.digest", database: "app", replicaSet: "rs0", deployment: testDeployment, pending: []migration.Migration{b, a}},
		{golden: "namespace.digest", database: "app", replicaSet: "rs0", deployment: testDeployment, namespace: "api", pending: []migration.Migration{a}},
		{golden: "namespace-nothing-pending.digest", database: "app", replicaSet: "rs0", deployment: testDeployment, namespace: "api"},
		{golden: "deployment-none.digest", database: "app", replicaSet: "rs0", deployment: DeploymentNone, pending: []migration.Migration{a}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			d := NewDigestInput(tc.database, tc.replicaSet, tc.deployment, tc.pending)
			d.Namespace = tc.namespace
			path := filepath.Join(t.TempDir(), "plan.digest")
			if err := WriteDigestFile(path, d); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "digest", tc.golden)
			if *updateGolden {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("digest file:\n%s\nwant:\n%s", got, want)
			}
			back, err := ReadDigestFile(golden)
			if err != nil || back.Sum() != d.Sum() || back.Namespace != tc.namespace {
				t.Errorf("ReadDigestFile: %+v, %v; want %+v", back, err, d)
			}
		})
	}
	// Every part of the input changes the digest.
	base := NewDigestInput("app", "rs0", testDeployment, []migration.Migration{a, b})
	edited := b
	edited.Checksum = "cccc"
	namespaced := NewDigestInput("app", "rs0", testDeployment, []migration.Migration{a, b})
	namespaced.Namespace = "api"
	for name, d := range map[string]*DigestInput{
		"namespace":    namespaced,
		"database":     NewDigestInput("app-staging", "rs0", testDeployment, []migration.Migration{a, b}),
		"replica set":  NewDigestInput("app", "rs1", testDeployment, []migration.Migration{a, b}),
		"deployment":   NewDigestInput("app", "rs0", "7c9e6679-7425-40de-944b-e07fc1f90ae7", []migration.Migration{a, b}),
		"no changelog": NewDigestInput("app", "rs0", DeploymentNone, []migration.Migration{a, b}),
		"checksum":     NewDigestInput("app", "rs0", testDeployment, []migration.Migration{a, edited}),
		"pending":      NewDigestInput("app", "rs0", testDeployment, []migration.Migration{a}),
	} {
		if d.Sum() == base.Sum() {
			t.Errorf("another %s gives the same digest", name)
		}
	}
}

func TestDigestFile(t *testing.T) {
	d := NewDigestInput("app", "rs0", testDeployment, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.digest")
	// Overwrites an existing file.
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteDigestFile(path, d); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDigestFile(path)
	if err != nil || got.Sum() != d.Sum() {
		t.Errorf("ReadDigestFile = %+v, %v, want %+v", got, err, d)
	}

	if err := WriteDigestFile(filepath.Join(dir, "missing", "plan.digest"), d); err == nil {
		t.Error("WriteDigestFile into a missing directory succeeded")
	}
}

func TestReadDigestFile(t *testing.T) {
	cases := []struct {
		file string
		err  string // "" means valid
	}{
		{file: "one.digest"},
		{file: "sorted.digest"},
		{file: "namespace.digest"},
		{file: "deployment-none.digest"},
		{file: "namespace-invalid.digest", err: "line 5: want namespace <name>"},
		{file: "namespace-empty.digest", err: "line 5: want namespace <name>"},
		{file: "namespace-misplaced.digest", err: `line 6: unexpected "namespace api"`},
		{file: "deployment-invalid.digest", err: "line 4: want deployment <uuid> or deployment none"},
		{file: "deployment-missing.digest", err: `line 4: unexpected "migration`},
		{file: "uppercase.digest", err: "not a plan digest file"},
		{file: "short.digest", err: "not a plan digest file"},
		{file: "empty.digest", err: "not a plan digest file"},
		{file: "old-format.digest", err: "not a plan digest file"},
		{file: "torn.digest", err: "its digest doesn't match its contents"},
		{file: "tampered.digest", err: "its digest doesn't match its contents"},
		{file: "unknown-version.digest", err: `unknown digest version "migrune-digest-v0"`},
		{file: "does-not-exist.digest", err: "no such file"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			got, err := ReadDigestFile(filepath.Join("testdata", "digest", tc.file))
			if tc.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("ReadDigestFile = %+v, %v, want an error containing %q", got, err, tc.err)
			}
		})
	}
}

func TestDigestMismatchError(t *testing.T) {
	m := func(id, sum string) migration.Migration { return migration.Migration{ID: id, Checksum: sum} }
	plan := NewDigestInput("app", "rs0", testDeployment, []migration.Migration{m("1-a", "a"), m("2-b", "b"), m("3-c", "c")})
	cases := []struct {
		name string
		run  *DigestInput
		want []string
	}{
		{name: "database", run: NewDigestInput("app-prod", "rs0", testDeployment, []migration.Migration{m("1-a", "a"), m("2-b", "b"), m("3-c", "c")}),
			want: []string{`the database is "app-prod", the plan's was "app"`}},
		{name: "replica set", run: NewDigestInput("app", "prod", testDeployment, []migration.Migration{m("1-a", "a"), m("2-b", "b"), m("3-c", "c")}),
			want: []string{`the replica set is "prod", the plan's was "rs0"`}},
		{name: "deployment", run: NewDigestInput("app-prod", "rs0", DeploymentNone, []migration.Migration{m("1-a", "a"), m("2-b", "b"), m("3-c", "c")}),
			want: []string{"the plan was made against another deployment (this run's migrations_changelog collection is none, the plan's was " + testDeployment + ")",
				`the database is "app-prod", the plan's was "app"`}},
		{name: "namespace", run: withNamespace(NewDigestInput("app", "rs0", testDeployment, []migration.Migration{m("1-a", "a"), m("2-b", "b"), m("3-c", "c")}), "api"),
			want: []string{`the namespace is "api", the plan's was the default`}},
		{name: "migrations", run: NewDigestInput("app", "rs0", testDeployment, []migration.Migration{m("2-b", "edited"), m("3-c", "c"), m("4-d", "d")}),
			want: []string{"pending since the plan: 4-d", "edited since the plan: 2-b", "no longer pending (applied, or removed, since the plan): 1-a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &DigestMismatchError{Plan: plan, Run: tc.run}
			if got := e.Differences(); !slices.Equal(got, tc.want) {
				t.Errorf("Differences() = %q, want %q", got, tc.want)
			}
			if !strings.Contains(e.Error(), plan.Sum()) || !strings.Contains(e.Error(), tc.run.Sum()) {
				t.Errorf("Error() = %q, want both digests", e.Error())
			}
		})
	}
}

func withNamespace(d *DigestInput, namespace string) *DigestInput {
	d.Namespace = namespace
	return d
}
