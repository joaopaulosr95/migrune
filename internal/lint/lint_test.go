package lint

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// render formats findings as "<file base>: <severity>: <rule>: <message>",
// leaving out the rules in skip.
func render(findings []Finding, skip ...string) []string {
	var out []string
	for _, f := range findings {
		if slices.Contains(skip, f.Rule) {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s: %s: %s", filepath.Base(f.File), f.Severity, f.Rule, f.Message))
	}
	return out
}

// match checks got against want line by line: each want line is
// "<file base>: <severity>: <rule>: <message substring>".
func match(t *testing.T, got, want []string) {
	t.Helper()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		head, sub, _ := cutMessage(want[i])
		ghead, gmsg, _ := cutMessage(got[i])
		ok = head == ghead && strings.Contains(gmsg, sub)
	}
	if !ok {
		t.Errorf("findings:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// cutMessage splits a rendered finding after its third ": ".
func cutMessage(s string) (head, msg string, ok bool) {
	parts := strings.SplitN(s, ": ", 4)
	if len(parts) < 4 {
		return s, "", false
	}
	return strings.Join(parts[:3], ": "), parts[3], true
}

func TestRules(t *testing.T) {
	cases := []struct {
		rule string
		// want lists every finding in testdata/rules/<rule>/fail, so a
		// problem reported twice fails the test. The pass directory must
		// have no findings at all.
		want []string
	}{
		// The id field, equal to the file name, breaks the schema's
		// pattern too, but a bad file name is reported once.
		{rule: RuleFileName, want: []string{
			"2026-11-01-001-Upper-Case.json: error: file-name: file name must be YYYY-MM-DD-NNN-slug.json",
			"2026-11-01-002-under_score.json: error: file-name: file name must be YYYY-MM-DD-NNN-slug.json",
			"2026-11-01-1-short-sequence.json: error: file-name: file name must be YYYY-MM-DD-NNN-slug.json",
			"2026-13-01-001-bad-month.json: error: file-name: file name date 2026-13-01 is not a valid date",
		}},
		// Entries plan and apply skip although they may be meant as
		// migrations; README.md is skipped silently.
		{rule: RuleIgnoredEntry, want: []string{
			"2026-11-05-002-upper.JSON: warning: ignored-entry: not read by lint, plan or apply: its extension is .JSON, and only *.json files are read, as plain JSON",
			"2026-11-05-003-five.json5: warning: ignored-entry: not read by lint, plan or apply: its extension is .json5",
			"2026-11-05-004-commented.jsonc: warning: ignored-entry: not read by lint, plan or apply: its extension is .jsonc",
			"archive: warning: ignored-entry: not read by lint, plan or apply: it is a subdirectory, and only the files directly in the directory are read",
		}},
		// A file with both gets both findings; a BOM keeps it from
		// loading, which isn't reported again as a json finding.
		{rule: RuleLineEndings, want: []string{
			"2026-11-01-001-crlf.json: error: line-endings: line 1 ends with a carriage return (CRLF): the checksum covers the file's bytes",
			"2026-11-01-002-bom.json: error: line-endings: the file starts with a UTF-8 byte order mark (BOM): the checksum covers the file's bytes",
			"2026-11-01-003-crlf-and-bom.json: error: line-endings: line 1 ends with a carriage return (CRLF)",
			"2026-11-01-003-crlf-and-bom.json: error: line-endings: the file starts with a UTF-8 byte order mark (BOM)",
		}},
		{rule: RuleJSON, want: []string{
			"2026-11-01-001-broken.json: error: json: invalid JSON: unexpected EOF",
		}},
		{rule: RuleExtendedJSON, want: []string{
			"2026-11-01-001-bad-oid.json: error: extended-json: invalid Extended JSON: error decoding key steps.0.filter",
		}},
		// The schema owns what it can express, and each problem gives one
		// finding, with a message that says what to do.
		{rule: RuleSchema, want: []string{
			"2026-11-01-001-no-description.json: error: schema: (root): missing property 'description'",
			`2026-11-01-002-out-in-merge.json: error: schema: /steps/0/pipeline/1: $out is not allowed: it replaces the whole target collection. Use $merge with whenMatched: "merge" and whenNotMatched: "discard"`,
			"2026-11-01-003-out-in-update.json: error: schema: /steps/0/update/1: $out is not allowed: it replaces the whole target collection",
			`2026-11-01-004-replace-insert.json: error: schema: /steps/0/pipeline/0/$merge/whenMatched: must be "merge", got "replace"`,
			`2026-11-01-004-replace-insert.json: error: schema: /steps/0/pipeline/0/$merge/whenNotMatched: must be "discard", got "insert"`,
			`2026-11-01-005-defaults-and-let.json: error: schema: /steps/0/pipeline/0/$merge: missing properties 'whenMatched', 'whenNotMatched': $merge takes into, on, whenMatched: "merge" and whenNotMatched: "discard"`,
			`2026-11-01-005-defaults-and-let.json: error: schema: /steps/0/pipeline/0/$merge: additional properties 'let' not allowed: $merge takes into`,
			`2026-11-01-006-string-form.json: error: schema: /steps/0/pipeline/0/$merge: got string, want object: $merge takes into`,
			"2026-11-01-007-data-then-index.json: error: schema: /steps/1/operation: createIndex belongs to index migrations, but the file's first operation, deleteMany, belongs to data migrations; a file holds one kind of migration",
			"2026-11-01-007-data-then-index.json: error: schema: /steps/2/operation: aggregate belongs to merge migrations",
			"2026-11-01-008-insert-many.json: error: schema: /steps/0/operation: insertMany is not an operation migrune runs: use updateOne, updateMany or deleteMany (data), createIndex or dropIndex (index), or aggregate (merge); seed documents with updateOne, upsert: true and $setOnInsert",
			"2026-11-01-009-merge-in-update.json: error: schema: /steps/0/update/1: $merge is only allowed as the last stage of an aggregate step in a merge migration",
			// Two problems, two findings.
			"2026-11-01-010-out-and-replace.json: error: schema: /steps/0/pipeline/1: $out is not allowed",
			`2026-11-01-010-out-and-replace.json: error: schema: /steps/0/pipeline/2/$merge/whenMatched: must be "merge", got "replace"`,
		}},
		{rule: RuleID, want: []string{
			`2026-11-01-001-file-name.json: error: id: /id: "2026-11-01-001-other-id" doesn't match the file name; want "2026-11-01-001-file-name"`,
			// An id that also breaks the id pattern is reported once.
			`2026-11-01-002-bad-id.json: error: id: /id: "2026-11-01-002-Bad_Id" doesn't match the file name; want "2026-11-01-002-bad-id"`,
		}},
		{rule: RuleDuplicateKey, want: []string{
			`2026-11-01-001-duplicate-into.json: error: duplicate-key: duplicate key: /steps/0/pipeline/1/$merge/into: "into" appears twice in the same object; keep one`,
		}},
		{rule: RuleDuplicatePrefix, want: []string{
			"2026-11-01-001-first.json: error: duplicate-prefix: prefix 2026-11-01-001 is also used by 2026-11-01-001-second.json",
			"2026-11-01-001-second.json: error: duplicate-prefix: prefix 2026-11-01-001 is also used by 2026-11-01-001-first.json",
		}},
		{rule: RuleMergeLast, want: []string{
			"2026-11-01-001-merge-first.json: error: merge-last: /steps/0/pipeline/0: $merge is stage 1 of 2; it must be the last stage",
		}},
		{rule: RuleNested, want: []string{
			"2026-11-01-001-lookup-out.json: error: nested-out-merge: /steps/0/pipeline/0/$lookup/pipeline/0: $out is not allowed inside a $lookup pipeline",
			"2026-11-01-002-facet-merge.json: error: nested-out-merge: /steps/0/pipeline/0/$facet/copy/1: $merge is not allowed inside a $facet pipeline",
			"2026-11-01-003-deep-union-with.json: error: nested-out-merge: /steps/0/pipeline/0/$unionWith/pipeline/0/$lookup/pipeline/0/$facet/deep/0: $out is not allowed inside a $facet pipeline",
			"2026-11-01-004-in-update-pipeline.json: error: nested-out-merge: /steps/0/update/0/$facet/x/0: $out is not allowed inside a $facet pipeline",
		}},
		{rule: RuleVolatileMerge, want: []string{
			"2026-11-01-001-rand.json: error: volatile-merge: /steps/0/pipeline/0/$project/score/$multiply/0/$rand: $rand returns a new random number on each run: a merge must give the same output when run again, computed from fields it doesn't write",
			"2026-11-01-002-now-in-lookup.json: error: volatile-merge: /steps/0/pipeline/0/$lookup/pipeline/0/$set/seenAt: $$NOW changes on each run",
			"2026-11-01-003-cluster-time-in-facet.json: error: volatile-merge: /steps/0/pipeline/0/$facet/stamped/0/$project/at: $$CLUSTER_TIME changes on each run",
			"2026-11-01-004-function.json: error: volatile-merge: /steps/0/pipeline/0/$project/slug/$function: $function runs JavaScript migrune can't check",
			"2026-11-01-005-accumulator.json: error: volatile-merge: /steps/0/pipeline/0/$group/itemCount/$accumulator: $accumulator runs JavaScript migrune can't check",
		}},
		// In a merge step, $function and $accumulator are volatile-merge
		// findings only.
		{rule: RuleServerJS, want: []string{
			"2026-11-01-001-where-filter.json: error: server-js: /steps/0/filter/$where: $where runs JavaScript on the server: migrune can't check what it does, it is slow, often disabled, and deprecated from MongoDB 8.0; use aggregation operators instead",
			"2026-11-01-002-function-update.json: error: server-js: /steps/0/update/0/$set/slug/$function: $function runs JavaScript on the server",
			"2026-11-01-003-accumulator-and-where.json: error: server-js: /steps/0/filter/$and/0/$where: $where runs JavaScript on the server",
			"2026-11-01-003-accumulator-and-where.json: error: server-js: /steps/0/update/0/$set/n/$accumulator: $accumulator runs JavaScript on the server",
			"2026-11-01-004-merge.json: error: server-js: /steps/0/pipeline/0/$match/$where: $where runs JavaScript on the server",
			"2026-11-01-004-merge.json: error: volatile-merge: /steps/0/pipeline/1/$project/slug/$function: $function runs JavaScript migrune can't check",
		}},
		{rule: RuleTTLIndex, want: []string{
			"2026-11-01-001-expire-sessions.json: warning: ttl-index: /steps/0/options/expireAfterSeconds: a TTL index deletes the documents of sessions whose lastUsedAt is more than 3600 seconds old, from the moment it is built and from then on: check the wouldExpire count plan reports for it",
		}},
		{rule: RuleDropThenCreate, want: []string{
			`2026-11-01-002-same-keys.json: error: drop-then-create: /steps/1: index owner_unique on projects is created after /steps/0 drops index owner_1, with the same keys {"ownerId": 1}: if the createIndex fails, such as a unique index on duplicates, the collection is left without either. Create the new index under a new name, and drop the old one in a later migration`,
			"2026-11-01-003-same-name.json: error: drop-then-create: /steps/1: index email_1 on users is created after /steps/0 drops index email_1, with the same name",
		}},
		{rule: RuleEmptyFilter, want: []string{
			"2026-11-01-001-delete-all.json: warning: empty-filter: /steps/0/filter: {} matches every document in sessions: make sure this deleteMany is meant to touch them all",
			"2026-11-01-002-update-all.json: warning: empty-filter: /steps/1/filter: {} matches every document in items: make sure this updateMany is meant to touch them all",
		}},
		// The schema refuses a reserved step collection or $merge.into;
		// this rule covers the collections pipelines read or write.
		{rule: RuleReserved, want: []string{
			`2026-11-01-005-lookup-from.json: error: reserved-collection: /steps/0/pipeline/0/$lookup/from: "migrations_changelog" is reserved: migrations must not name migrune's own collections (migrations_changelog, migrations_lock) or MongoDB's system.* collections`,
			`2026-11-01-006-graph-lookup-from.json: error: reserved-collection: /steps/0/pipeline/0/$graphLookup/from: "system.users" is reserved`,
			`2026-11-01-007-union-with-name.json: error: reserved-collection: /steps/0/pipeline/0/$unionWith: "migrations_lock" is reserved`,
			`2026-11-01-008-union-with-coll.json: error: reserved-collection: /steps/0/pipeline/0/$unionWith/coll: "system.js" is reserved`,
			`2026-11-01-009-facet-lookup.json: error: reserved-collection: /steps/0/pipeline/0/$facet/locks/0/$lookup/from: "migrations_lock" is reserved`,
			`2026-11-01-010-nested-union-with.json: error: reserved-collection: /steps/0/pipeline/0/$lookup/pipeline/0/$unionWith/pipeline/0/$unionWith: "migrations_changelog" is reserved`,
		}},
	}
	for _, tc := range cases {
		dir := filepath.Join("testdata", "rules", tc.rule)
		t.Run(tc.rule+"/pass", func(t *testing.T) {
			got, err := Run(filepath.Join(dir, "pass"), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got), nil)
		})
		t.Run(tc.rule+"/fail", func(t *testing.T) {
			got, err := Run(filepath.Join(dir, "fail"), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got), tc.want)
		})
	}
}

// TestValidMigrations lints the valid fixtures of the migration package.
func TestValidMigrations(t *testing.T) {
	got, err := Run(filepath.Join("..", "migration", "testdata", "valid"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	match(t, render(got), nil)
}

// TestExampleMigrations keeps the sample migrations in examples/ lint-clean.
func TestExampleMigrations(t *testing.T) {
	got, err := Run(filepath.Join("..", "..", "examples", "migrations"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	match(t, render(got), nil)
}

// TestFileType checks lint reports, without reading them, a symbolic link
// and a file over migration.MaxFileSize bytes.
func TestFileType(t *testing.T) {
	valid, err := filepath.Abs(filepath.Join("testdata", "rules", "schema", "pass", "2026-11-01-001-complete.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(valid, filepath.Join(dir, "2026-11-01-001-linked.json")); err != nil {
		t.Skip("symbolic links not supported:", err)
	}
	big := append([]byte(`{"id": "2026-11-01-002-big", "description": "big", "steps": []}`), bytes.Repeat([]byte(" "), migration.MaxFileSize)...)
	if err := os.WriteFile(filepath.Join(dir, "2026-11-01-002-big.json"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Run(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	match(t, render(got), []string{
		"2026-11-01-001-linked.json: error: regular-file: not a regular file: it is a symbolic link; migrune reads only regular files",
		"2026-11-01-002-big.json: error: file-size: file too large: 1048639 bytes, over the 1048576-byte limit; split the migration",
	})
}

func TestRunMissingDirectory(t *testing.T) {
	if _, err := Run(filepath.Join("testdata", "does-not-exist"), nil, nil); err == nil {
		t.Fatal("Run returned no error")
	}
}

func TestReplay(t *testing.T) {
	cases := []struct {
		dir  string
		want []string
	}{
		// Dropping a name frees it for a new definition.
		{dir: "drop-recreate"},
		// Creating the same definition again is a no-op: numbers compare
		// by value, false booleans equal absent ones, and options that
		// don't change behaviour are ignored.
		{dir: "same-definition"},
		// Names are scoped to a collection.
		{dir: "other-collection"},
		{dir: "name-reused", want: []string{
			`2026-11-01-002-reuse.json: error: index-name-reused: /steps/0: index owner_1 on projects already exists with a different definition, created by 2026-11-01-001-create.json: keys {"ownerId": 1} options {"unique": true} here, keys {"ownerId": 1} options {} there`,
		}},
		// Key order is part of the definition.
		{dir: "name-reused-keys", want: []string{
			`2026-11-01-001-create.json: error: index-name-reused: /steps/1: index owner_1 on projects already exists with a different definition`,
		}},
		// The same definition under another name is rejected by MongoDB.
		{dir: "conflict-same-definition", want: []string{
			`2026-11-01-002-rename.json: error: index-conflict: /steps/0: index ownerId_1 on projects has the same definition as index owner_1, created by 2026-11-01-001-create.json: keys {"ownerId": 1}`,
		}},
		// So are the same keys with other options, unless collation,
		// partialFilterExpression, unique, sparse or wildcardProjection
		// differ. The simple collation is no collation, a false sparse is no
		// sparse, and a wildcardProjection compares as MongoDB normalizes it.
		{dir: "conflict-options", want: []string{
			`2026-11-01-001-create.json: error: index-conflict: /steps/1: index email_simple on users has the same definition as index email_1`,
			`2026-11-01-001-create.json: error: index-conflict: /steps/3: index expiresAt_ttl on sessions has the same keys and different options ({"expireAfterSeconds": 60} here, {"expireAfterSeconds": 3600} there) as index expiresAt_1`,
			`2026-11-01-001-create.json: error: index-conflict: /steps/5: index email_unique_again on users has the same definition as index email_unique`,
			`2026-11-01-001-create.json: error: index-conflict: /steps/7: index all_a_again on events has the same definition as index all_a`,
			`2026-11-01-001-create.json: error: index-conflict: /steps/9: index loc_2d_bits on places has the same keys and different options ({"bits": 20} here, {} there) as index loc_2d`,
		}},
		// A different collation, partialFilterExpression, unique, sparse or
		// wildcardProjection, or a different key pattern, lets indexes
		// coexist.
		{dir: "conflict-distinct"},
		// A dropped index doesn't conflict.
		{dir: "conflict-drop-rename"},
		// A text index's weights and default_language take their defaults
		// when left out, as listIndexes shows them.
		{dir: "text-same-definition"},
		{dir: "text-name-reused", want: []string{
			`2026-11-01-002-weights.json: error: index-name-reused: /steps/0: index text on articles already exists with a different definition, created by 2026-11-01-001-create.json: keys {"_fts": "text", "_ftsx": 1} options {"weights": {"body": 1, "title": 10}, "default_language": "english", "language_override": "language"} here, keys {"_fts": "text", "_ftsx": 1} options {"weights": {"body": 1, "title": 1}, "default_language": "english", "language_override": "language"} there`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			got, err := Run(filepath.Join("testdata", "replay", tc.dir), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got, RuleTTLIndex), tc.want)
		})
	}
}

func TestReplayResult(t *testing.T) {
	cases := []struct {
		dir  string
		want []string // collection.name keys options
	}{
		{dir: "drop-recreate", want: []string{`projects.owner_1 {"ownerId": 1, "createdAt": -1} {"unique": true}`}},
		{dir: "same-definition", want: []string{`projects.owner_1 {"ownerId": 1} {}`}},
		{dir: "other-collection", want: []string{`projects.owner_1 {"ownerId": 1} {}`, `owners.owner_1 {"name": 1} {}`}},
		{dir: "name-reused", want: []string{`projects.owner_1 {"ownerId": 1} {}`}},
		{dir: "conflict-same-definition", want: []string{`projects.owner_1 {"ownerId": 1} {}`}},
		{dir: "conflict-options", want: []string{`users.email_1 {"email": 1} {}`, `sessions.expiresAt_1 {"expiresAt": 1} {"expireAfterSeconds": 3600}`, `users.email_unique {"email": 1} {"unique": true}`,
			`events.all_a {"$**": 1} {"wildcardProjection": {"a.b": 1}}`, `places.loc_2d {"loc": "2d"} {}`}},
		{dir: "conflict-distinct", want: []string{
			`accounts.name_1 {"name": 1} {}`,
			`accounts.name_1_ci {"name": 1} {"collation": {"locale": "en", "strength": 2}}`,
			`projects.status_1 {"status": 1} {}`,
			`projects.status_1_active {"status": 1} {"partialFilterExpression": {"archived": false}}`,
			`projects.status_1_unique {"status": 1} {"unique": true}`,
			`projects.status_1_sparse {"status": 1} {"sparse": true}`,
			`projects.status_1_unique_sparse {"status": 1} {"unique": true, "sparse": true}`,
			`projects.status_1_ownerId_1 {"status": 1, "ownerId": 1} {}`,
			`events.all {"$**": 1} {}`,
			`events.all_a {"$**": 1} {"wildcardProjection": {"a": 1}}`,
			`events.all_b {"$**": 1} {"wildcardProjection": {"b": 1}}`,
		}},
		{dir: "conflict-drop-rename", want: []string{`projects.ownerId_1 {"ownerId": 1} {"unique": true}`}},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			ms, err := loadAll(filepath.Join("testdata", "replay", tc.dir))
			if err != nil {
				t.Fatal(err)
			}
			indexes, _ := replayIndexes(ms)
			var got []string
			for _, ix := range indexes {
				got = append(got, fmt.Sprintf("%s.%s %s %s", ix.Collection, ix.Name, ix.KeysString(), ix.OptionsString()))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("indexes:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}
}

func loadAll(dir string) ([]migration.Migration, error) {
	d, err := migration.LoadDir(dir, slog.New(slog.DiscardHandler))
	return d.Migrations, err
}

func TestExpectedIndexes(t *testing.T) {
	const migrations = "2026-11-01-001-indexes.json"
	cases := []struct {
		file string
		want []string
	}{
		// Matches although the expected file orders options differently,
		// writes 1.0 for 1, NumberLong for an int, sparse: false, adds
		// background and lists _id_.
		{file: "match"},
		// Without names, nothing can be reported as renamed.
		{file: "unnamed"},
		{file: "missing", want: []string{
			`missing.json: error: index-missing: /6: no migration creates the expected index status_1 on projects: keys {"status": 1} options {}`,
		}},
		// Same fields in a different order is a different index.
		{file: "missing-key-order", want: []string{
			migrations + `: warning: index-extra: /steps/1: index owner_1 on projects is not in missing-key-order.json`,
			`missing-key-order.json: error: index-missing: /1: no migration creates the expected index owner_1 on projects: keys {"createdAt": -1, "ownerId": 1}`,
		}},
		{file: "options", want: []string{
			migrations + `: error: index-options: /steps/3: index email_1 on users has keys {"email": 1} with options {"unique": true, "partialFilterExpression": {"email": {"$exists": true}}}, but the expected index email_1 has options {"unique": true}`,
		}},
		{file: "extra", want: []string{
			"2026-11-02-001-drop-and-tmp.json: warning: index-extra: /steps/0: index tmp_1 on projects is not in extra.json",
		}},
		{file: "renamed", want: []string{
			migrations + ": warning: index-renamed: /steps/1: index owner_1 on projects matches the expected index ownerId_1_createdAt_-1 by definition, under a different name",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			expected, err := ReadExpectedIndexes(filepath.Join("testdata", "expected", tc.file+".json"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Run(filepath.Join("testdata", "expected", "migrations"), expected, nil)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got, RuleTTLIndex), tc.want)
		})
	}
}

// TestExpectedIndexesOptions checks every option counts, for one index of
// each type: those the expected file spells differently, or gives as
// listIndexes shows them, match; those it leaves out or changes don't.
func TestExpectedIndexesOptions(t *testing.T) {
	const migrations = "2026-11-01-001-indexes.json"
	cases := []struct {
		file string
		want []string
	}{
		// A wildcardProjection nested or dotted, with true for 1 and _id: 0;
		// a text index with its keys, default_language and textIndexVersion
		// as listIndexes shows them; numbers of other types; and the ignored
		// options 2dsphereIndexVersion, background and prepareUnique.
		{file: "match"},
		// The collation as listIndexes shows it, with every default and its
		// version: every field the migration gives must match, the others
		// count as defaults, so caseLevel set on the server alone goes
		// unnoticed.
		{file: "collation-listindexes"},
		{file: "collation-listindexes-blind-spot"},
		{file: "collation-listindexes-differ", want: []string{
			migrations + `: error: index-options: /steps/7: index name_ci on accounts has keys {"name": 1} with options {"collation": {"locale": "en", "strength": 2}}, but the expected index name_ci has options {"collation": {"alternate": "non-ignorable", "backwards": false, "caseFirst": "off", "caseLevel": false, "locale": "en", "maxVariable": "punct", "normalization": false, "numericOrdering": false, "strength": 1, "version": "57.1"}}`,
		}},
		{file: "differ", want: []string{
			migrations + `: error: index-options: /steps/0: index all on events has keys {"$**": 1} with options {"wildcardProjection": {"payload.id": 1, "payload.kind": 1}}, but the expected index all has options {}`,
			migrations + `: error: index-options: /steps/1: index text on articles has keys {"_fts": "text", "_ftsx": 1} with options {"weights": {"body": 1, "title": 10}, "default_language": "english", "language_override": "lang"}, but the expected index text has options {"weights": {"body": 1, "title": 10}, "default_language": "english", "language_override": "language"}`,
			migrations + `: error: index-options: /steps/2: index loc_2d on places has keys {"loc": "2d"} with options {"bits": 20, "max": 1000, "min": -1000}, but the expected index loc_2d has options {}`,
			migrations + `: error: index-options: /steps/3: index area_2dsphere on places has keys {"area": "2dsphere"} with options {}, but the expected index area_2dsphere has options {"sparse": true}`,
			migrations + `: error: index-options: /steps/5: index expires_1 on sessions has keys {"expiresAt": 1} with options {"expireAfterSeconds": 3600}, but the expected index expires_1 has options {"expireAfterSeconds": 60}`,
			migrations + `: error: index-options: /steps/6: index email_1 on users has keys {"email": 1} with options {"unique": true, "partialFilterExpression": {"email": {"$exists": true}}}, but the expected index email_1 has options {"unique": true, "partialFilterExpression": {"email": {"$type": "string"}}}`,
			migrations + `: error: index-options: /steps/7: index name_ci on accounts has keys {"name": 1} with options {"collation": {"locale": "en", "strength": 2}}, but the expected index name_ci has options {"collation": {"locale": "en", "strength": 1}}`,
			migrations + `: error: index-options: /steps/8: index at_1 on logs has keys {"at": 1} with options {"storageEngine": {"wiredTiger": {"configString": "block_compressor=zlib"}}}, but the expected index at_1 has options {}`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			expected, err := ReadExpectedIndexes(filepath.Join("testdata", "expected-options", tc.file+".json"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Run(filepath.Join("testdata", "expected-options", "migrations"), expected, nil)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got, RuleTTLIndex), tc.want)
		})
	}
}

func TestReadExpectedIndexesInvalid(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{file: "not-json", want: "invalid JSON"},
		{file: "not-array", want: "want a JSON array of indexes"},
		{file: "entry-not-object", want: "invalid Extended JSON"},
		{file: "bad-extended-json", want: "invalid Extended JSON"},
		{file: "unknown-field", want: `/0: unknown field "version"`},
		{file: "missing-collection", want: "/0: missing collection"},
		{file: "empty-keys", want: "/0: invalid keys"},
		{file: "keys-not-object", want: "/0: invalid keys"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			_, err := ReadExpectedIndexes(filepath.Join("testdata", "expected-invalid", tc.file+".json"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestChecksums checks the online rules: a file whose checksum differs from
// its latest Finished changelog entry is an error, and so is a Finished
// migration with no file.
func TestChecksums(t *testing.T) {
	dir := filepath.Join("testdata", "checksum")
	sum := func(id string) string {
		m, err := migration.LoadFile(filepath.Join(dir, id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		return m.Checksum
	}
	const (
		flag  = "2026-11-01-001-set-flag"
		stale = "2026-11-02-001-drop-stale"
	)
	edited := strings.Repeat("0", 64)
	at := time.Date(2026, 1, 4, 10, 12, 0, 0, time.UTC)
	applied := func(id, sum string) migration.Applied {
		return migration.Applied{ID: id, Checksum: sum, Description: "Retired " + id, AppliedAt: at, Runner: "ci-7"}
	}
	set := func(as ...migration.Applied) map[string]migration.Applied {
		m := map[string]migration.Applied{}
		for _, a := range as {
			m[a.ID] = a
		}
		return m
	}
	gone := func(id string) string {
		return id + `.json: error: applied-missing: Finished 2026-01-04T10:12:00Z by ci-7: "Retired ` + id + `"; ` +
			`the file is not in --dir. The full migration is stored in migrations_changelog (migrationId: "` + id + `"). ` +
			"plan and apply fail until the file is restored"
	}
	cases := []struct {
		name    string
		applied map[string]migration.Applied
		want    []string
	}{
		{name: "offline", applied: nil},
		{name: "nothing applied", applied: set()},
		{name: "unchanged", applied: set(applied(flag, sum(flag)), applied(stale, sum(stale)))},
		{name: "applied but missing", applied: set(applied("2026-10-02-001-gone", edited), applied(flag, sum(flag)), applied("2026-10-01-001-gone", edited)),
			want: []string{gone("2026-10-01-001-gone"), gone("2026-10-02-001-gone")}},
		{name: "written by hand without runner or date",
			applied: set(applied(flag, sum(flag)), migration.Applied{ID: "2026-10-03-001-hand", Checksum: edited, Description: "By hand"}),
			want:    []string{`2026-10-03-001-hand.json: error: applied-missing: Finished unknown by unknown: "By hand"; the file is not in --dir.`}},
		{name: "edited after apply", applied: set(applied(flag, edited), applied(stale, sum(stale))),
			want: []string{flag + ".json: error: checksum: the file changed after the migration was applied: the changelog has checksum " + edited}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Run(dir, nil, tc.applied)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got), tc.want)
		})
	}
}
