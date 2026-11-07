package lint

import (
	"bytes"
	"fmt"
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
		// Entries migrune skips although they may be meant as
		// migrations; README.md is skipped silently.
		{rule: RuleIgnoredEntry, want: []string{
			"2026-11-05-002-upper.JSON: warning: ignored-entry: not read by lint: its extension is .JSON, and only *.json files are read, as plain JSON",
			"2026-11-05-003-five.json5: warning: ignored-entry: not read by lint: its extension is .json5",
			"2026-11-05-004-commented.jsonc: warning: ignored-entry: not read by lint: its extension is .jsonc",
			"archive: warning: ignored-entry: not read by lint: it is a subdirectory, and only the files directly in the directory are read",
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
			"2026-11-01-003-out-in-update.json: error: schema: /steps/0/update/1: $out is not allowed: it replaces the whole target collection",
			"2026-11-01-008-insert-many.json: error: schema: /steps/0/operation: insertMany is not an operation migrune runs: use updateOne, updateMany or deleteMany; seed documents with updateOne, upsert: true and $setOnInsert",
			"2026-11-01-009-merge-in-update.json: error: schema: /steps/0/update/1: $merge is not allowed: it writes the pipeline's output into a collection",
		}},
		{rule: RuleID, want: []string{
			`2026-11-01-001-file-name.json: error: id: /id: "2026-11-01-001-other-id" doesn't match the file name; want "2026-11-01-001-file-name"`,
			// An id that also breaks the id pattern is reported once.
			`2026-11-01-002-bad-id.json: error: id: /id: "2026-11-01-002-Bad_Id" doesn't match the file name; want "2026-11-01-002-bad-id"`,
		}},
		{rule: RuleDuplicateKey, want: []string{
			`2026-11-01-001-duplicate-collection.json: error: duplicate-key: duplicate key: /steps/0/collection: "collection" appears twice in the same object; keep one`,
		}},
		{rule: RuleDuplicatePrefix, want: []string{
			"2026-11-01-001-first.json: error: duplicate-prefix: prefix 2026-11-01-001 is also used by 2026-11-01-001-second.json",
			"2026-11-01-001-second.json: error: duplicate-prefix: prefix 2026-11-01-001 is also used by 2026-11-01-001-first.json",
		}},
		{rule: RuleNested, want: []string{
			"2026-11-01-004-in-update-pipeline.json: error: nested-out-merge: /steps/0/update/0/$facet/x/0: $out is not allowed inside a $facet pipeline",
		}},
		{rule: RuleServerJS, want: []string{
			"2026-11-01-001-where-filter.json: error: server-js: /steps/0/filter/$where: $where runs JavaScript on the server: migrune can't check what it does, it is slow, often disabled, and deprecated from MongoDB 8.0; use aggregation operators instead",
			"2026-11-01-002-function-update.json: error: server-js: /steps/0/update/0/$set/slug/$function: $function runs JavaScript on the server",
			"2026-11-01-003-accumulator-and-where.json: error: server-js: /steps/0/filter/$and/0/$where: $where runs JavaScript on the server",
			"2026-11-01-003-accumulator-and-where.json: error: server-js: /steps/0/update/0/$set/n/$accumulator: $accumulator runs JavaScript on the server",
		}},
		{rule: RuleEmptyFilter, want: []string{
			"2026-11-01-001-delete-all.json: warning: empty-filter: /steps/0/filter: {} matches every document in sessions: make sure this deleteMany is meant to touch them all",
			"2026-11-01-002-update-all.json: warning: empty-filter: /steps/1/filter: {} matches every document in items: make sure this updateMany is meant to touch them all",
		}},
		// The schema refuses a reserved step collection; this rule covers
		// the collections pipelines read or write.
		{rule: RuleReserved, want: []string{
			`2026-11-01-005-lookup-from.json: error: reserved-collection: /steps/0/update/0/$lookup/from: "migrations_changelog" is reserved: migrations must not name migrune's own collections (migrations_changelog, migrations_lock) or MongoDB's system.* collections`,
			`2026-11-01-006-graph-lookup-from.json: error: reserved-collection: /steps/0/update/0/$graphLookup/from: "system.users" is reserved`,
			`2026-11-01-007-union-with-name.json: error: reserved-collection: /steps/0/update/0/$unionWith: "migrations_lock" is reserved`,
			`2026-11-01-008-union-with-coll.json: error: reserved-collection: /steps/0/update/0/$unionWith/coll: "system.js" is reserved`,
			`2026-11-01-009-facet-lookup.json: error: reserved-collection: /steps/0/update/0/$facet/locks/0/$lookup/from: "migrations_lock" is reserved`,
			`2026-11-01-010-nested-union-with.json: error: reserved-collection: /steps/0/update/0/$lookup/pipeline/0/$unionWith/pipeline/0/$unionWith: "migrations_changelog" is reserved`,
		}},
	}
	for _, tc := range cases {
		dir := filepath.Join("testdata", "rules", tc.rule)
		t.Run(tc.rule+"/pass", func(t *testing.T) {
			got, err := Run(filepath.Join(dir, "pass"), nil)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got), nil)
		})
		t.Run(tc.rule+"/fail", func(t *testing.T) {
			got, err := Run(filepath.Join(dir, "fail"), nil)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got), tc.want)
		})
	}
}

// TestValidMigrations lints the valid fixtures of the migration package.
func TestValidMigrations(t *testing.T) {
	got, err := Run(filepath.Join("..", "migration", "testdata", "valid"), nil)
	if err != nil {
		t.Fatal(err)
	}
	match(t, render(got), nil)
}

// TestExampleMigrations keeps the sample migrations in examples/ lint-clean.
func TestExampleMigrations(t *testing.T) {
	got, err := Run(filepath.Join("..", "..", "examples", "migrations"), nil)
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
	got, err := Run(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	match(t, render(got), []string{
		"2026-11-01-001-linked.json: error: regular-file: not a regular file: it is a symbolic link; migrune reads only regular files",
		"2026-11-01-002-big.json: error: file-size: file too large: 1048639 bytes, over the 1048576-byte limit; split the migration",
	})
}

func TestRunMissingDirectory(t *testing.T) {
	if _, err := Run(filepath.Join("testdata", "does-not-exist"), nil); err == nil {
		t.Fatal("Run returned no error")
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
			"Restore the file"
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
			got, err := Run(dir, tc.applied)
			if err != nil {
				t.Fatal(err)
			}
			match(t, render(got), tc.want)
		})
	}
}
