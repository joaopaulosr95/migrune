package lint

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// TestDrift compares the replay of the applied migrations in
// testdata/drift/migrations with server index lists in testdata/drift/server,
// written as expected-indexes files. 2026-11-03-001-pending is never
// applied.
func TestDrift(t *testing.T) {
	const (
		indexes = "2026-11-01-001-indexes"
		ttl     = "2026-11-02-001-ttl"
	)
	dir := filepath.Join("testdata", "drift")
	// applied returns the Finished entries of the migrations in
	// testdata/drift/migrations named by ids, as runner.FinishedSnapshots
	// does: their checksum and steps as applied.
	applied := func(ids ...string) map[string]migration.Applied {
		out := map[string]migration.Applied{}
		for _, id := range ids {
			m, err := migration.LoadFile(filepath.Join(dir, "migrations", id+".json"))
			if err != nil {
				t.Fatal(err)
			}
			out[id] = migration.Applied{ID: id, Checksum: m.Checksum, Description: m.Description, Steps: m.Steps}
		}
		return out
	}
	accountsCI := bson.D{{Key: "locale", Value: "en"}, {Key: "strength", Value: int32(2)}, {Key: "version", Value: "57.1"}}
	cases := []struct {
		name       string
		migrations string // directory under testdata/drift; default migrations
		server     string
		applied    map[string]migration.Applied
		collations map[string]bson.D
		pending    int
		want       []string
	}{
		{name: "clean", server: "clean", pending: 1},
		{name: "nothing applied", server: "clean", applied: map[string]migration.Applied{}, pending: 3, want: []string{
			`accounts: warning: index-extra: index name_ci, keys {"name": 1} options {"collation": {`,
			`labels: warning: index-extra: index label_1, keys {"label": 1} options {}, is on the server but no applied migration of this namespace creates it`,
			`sessions: warning: index-extra: index expires_1`,
			`users: warning: index-extra: index email_1`,
		}},
		// Only the first migration applied: the TTL is still 3600.
		{name: "earlier migrations only", server: "clean", applied: applied(indexes), pending: 2, want: []string{
			`projects: error: index-missing: index tmp_1, created by ` + indexes + ` with keys {"tmp": 1} options {}, is not on the server`,
			`sessions: error: index-options: index expires_1 has keys {"expiresAt": 1} with options {"expireAfterSeconds": 7200} on the server, ` +
				`but the applied migrations leave index expires_1, created by ` + indexes + `, with options {"expireAfterSeconds": 3600}`,
		}},
		{name: "missing", server: "missing", pending: 1, want: []string{
			`users: error: index-missing: index email_1, created by ` + indexes + ` with keys {"email": 1} options {"unique": true}, is not on the server`,
		}},
		{name: "options", server: "options", pending: 1, want: []string{
			`sessions: error: index-options: index expires_1 has keys {"expiresAt": 1} with options {"expireAfterSeconds": 3600} on the server, ` +
				`but the applied migrations leave index expires_1, created by ` + indexes + `, with options {"expireAfterSeconds": 7200}`,
		}},
		// The pending migration creates ownerId_1, but it isn't applied.
		{name: "extra", server: "extra", pending: 1, want: []string{
			`projects: warning: index-extra: index ownerId_1, keys {"ownerId": 1} options {}, is on the server but no applied migration of this namespace creates it: ` +
				"another namespace, the application or a person may have",
		}},
		{name: "renamed", server: "renamed", pending: 1, want: []string{
			`users: warning: index-renamed: index email_unique on the server matches index email_1, created by ` + indexes + `, by definition, under a different name`,
		}},
		{name: "collation field differs", server: "collation-field", pending: 1, want: []string{
			`accounts: error: index-options: index name_ci has keys {"name": 1} with options {"collation": {`,
		}},
		// The fields the migration gives match; the server's others count
		// as defaults.
		{name: "collation fields the migration doesn't give", server: "collation-subset", pending: 1},
		{name: "collation the migration doesn't give", server: "collation-added", pending: 1, want: []string{
			`labels: error: index-options: index label_1 has keys {"label": 1} with options {"collation": {`,
		}},
		{name: "collation inherited from the collection", server: "collation-added", pending: 1,
			collations: map[string]bson.D{"labels": collationOf(t, "collation-added", "labels")}},
		{name: "another collation than the collection's", server: "collation-added", pending: 1,
			collations: map[string]bson.D{"labels": accountsCI}, want: []string{
				`labels: error: index-options: index label_1 has keys {"label": 1} with options {"collation": {`,
			}},
		{name: "simple collation", server: "collation-simple", pending: 1},
		// The snapshot is replayed: the server has the applied TTL, 7200,
		// not the edited file's 9999.
		{name: "edited after apply", migrations: "edited", server: "clean", pending: 1, want: []string{
			"2026-11-02-001-ttl.json: error: checksum: the file changed after the migration was applied",
		}},
		{name: "applied but missing", migrations: "missing", server: "clean", pending: 1, want: []string{
			`2026-11-02-001-ttl.json: error: applied-missing: Finished unknown by unknown: "test fixture"; the file is not in --dir.`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.migrations == "" {
				tc.migrations = "migrations"
			}
			if tc.applied == nil {
				tc.applied = applied(indexes, ttl)
			}
			res, err := Drift(filepath.Join(dir, tc.migrations), tc.applied, serverIndexes(t, tc.server), tc.collations)
			if err != nil {
				t.Fatal(err)
			}
			if res.Pending != tc.pending || res.Replayed != len(tc.applied) {
				t.Errorf("pending, replayed = %d, %d; want %d, %d", res.Pending, res.Replayed, tc.pending, len(tc.applied))
			}
			var got []string
			for _, f := range res.Findings {
				if (f.File == "") == (f.Collection == "") {
					t.Errorf("finding %v: want exactly one of file and collection", f)
				}
				got = append(got, fmt.Sprintf("%s: %s: %s: %s", filepath.Base(f.Subject()), f.Severity, f.Rule, f.Message))
			}
			match(t, got, tc.want)
		})
	}
}

// TestDriftOptions compares the applied index of each type in
// testdata/expected-options/migrations with server index lists in
// testdata/drift-options, as listIndexes shows them on every supported
// MongoDB version: every option counts but those MongoDB fills in or
// ignores.
func TestDriftOptions(t *testing.T) {
	const id = "2026-11-01-001-indexes"
	dir := filepath.Join("testdata", "expected-options", "migrations")
	m, err := migration.LoadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	applied := map[string]migration.Applied{id: {ID: id, Checksum: m.Checksum, Description: m.Description, Steps: m.Steps}}
	cases := []struct {
		server string
		want   []string
	}{
		// text's language fields and weights, and collation, filled in.
		{server: "same"},
		// 2dsphereIndexVersion, textIndexVersion and background.
		{server: "ignored"},
		// The audit's case: a wildcardProjection changed by hand.
		{server: "wildcard", want: []string{
			`events: error: index-options: index all has keys {"$**": 1} with options {"wildcardProjection": {"payload.id": 1}} on the server, ` +
				`but the applied migrations leave index all, created by ` + id + `, with options {"wildcardProjection": {"payload.id": 1, "payload.kind": 1}}`,
		}},
		{server: "language-override", want: []string{
			`articles: error: index-options: index text has keys {"_fts": "text", "_ftsx": 1} with options {"weights": {"body": 1, "title": 10}, "default_language": "english", "language_override": "language"} on the server`,
		}},
		{server: "bits", want: []string{
			`places: error: index-options: index loc_2d has keys {"loc": "2d"} with options {} on the server, ` +
				`but the applied migrations leave index loc_2d, created by ` + id + `, with options {"bits": 20, "max": 1000, "min": -1000}`,
		}},
		{server: "storage", want: []string{
			`logs: error: index-options: index at_1 has keys {"at": 1} with options {} on the server`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.server, func(t *testing.T) {
			ex, err := ReadExpectedIndexes(filepath.Join("testdata", "drift-options", tc.server+".json"))
			if err != nil {
				t.Fatal(err)
			}
			server := make([]migration.Index, len(ex.Indexes))
			for i, ix := range ex.Indexes {
				server[i] = ix.Index
			}
			res, err := Drift(dir, applied, server, nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range res.Findings {
				got = append(got, fmt.Sprintf("%s: %s: %s: %s", filepath.Base(f.Subject()), f.Severity, f.Rule, f.Message))
			}
			match(t, got, tc.want)
		})
	}
}

// serverIndexes reads testdata/drift/server/<name>.json, an expected-indexes
// file standing for what listIndexes returns.
func serverIndexes(t *testing.T, name string) []migration.Index {
	t.Helper()
	ex, err := ReadExpectedIndexes(filepath.Join("testdata", "drift", "server", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]migration.Index, len(ex.Indexes))
	for i, ix := range ex.Indexes {
		out[i] = ix.Index
	}
	return out
}

// collationOf returns the collation of the first index on coll in the server
// fixture name.
func collationOf(t *testing.T, name, coll string) bson.D {
	t.Helper()
	for _, ix := range serverIndexes(t, name) {
		if ix.Collection == coll {
			return ix.Collation()
		}
	}
	t.Fatalf("no index on %s in %s", coll, name)
	return nil
}

// TestDriftUnreadableDir checks Drift fails when the directory can't be read.
func TestDriftUnreadableDir(t *testing.T) {
	_, err := Drift(filepath.Join("testdata", "does-not-exist"), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "read migrations directory") {
		t.Fatalf("err = %v, want a read error", err)
	}
}
