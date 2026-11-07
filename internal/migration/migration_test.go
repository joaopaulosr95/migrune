package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// load loads the migrations in dir.
func load(dir string, logger *slog.Logger) ([]Migration, error) {
	d, err := LoadDir(dir, logger)
	return d.Migrations, err
}

// loadOne copies one fixture into an empty directory and loads it.
func loadOne(t *testing.T, fixture string) ([]Migration, error) {
	t.Helper()
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(fixture)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return load(dir, discard())
}

func TestLoadValid(t *testing.T) {
	cases := []struct {
		id         string
		kind       Kind
		operations []string
	}{
		{id: "2026-11-05-001-add-archived-flag", kind: KindData, operations: []string{"updateMany", "updateMany", "deleteMany"}},
		{id: "2026-11-05-002-seed-price-table-v1", kind: KindData, operations: []string{"updateOne"}},
		{id: "2026-11-05-003-account-name-ci-index", kind: KindIndex, operations: []string{"createIndex", "dropIndex"}},
		{id: "2026-11-05-004-denormalize-project-owner-name", kind: KindMerge, operations: []string{"aggregate"}},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			ms, err := loadOne(t, filepath.Join("testdata", "valid", tc.id+".json"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(ms) != 1 {
				t.Fatalf("got %d migrations, want 1", len(ms))
			}
			m := ms[0]
			if m.ID != tc.id || m.Kind != tc.kind || m.Description == "" {
				t.Errorf("got id %q kind %q description %q, want id %q kind %q", m.ID, m.Kind, m.Description, tc.id, tc.kind)
			}
			var ops []string
			for _, s := range m.Steps {
				if s.Collection == "" {
					t.Errorf("step %s has no collection", s.Operation)
				}
				ops = append(ops, s.Operation)
			}
			if !slices.Equal(ops, tc.operations) {
				t.Errorf("operations = %v, want %v", ops, tc.operations)
			}
		})
	}
}

// TestLoadExtendedJSON checks that step arguments keep their BSON types and
// key order after parsing.
func TestLoadExtendedJSON(t *testing.T) {
	ms, err := load(filepath.Join("testdata", "valid"), discard())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Migration{}
	for _, m := range ms {
		byID[m.ID] = m
	}

	data := byID["2026-11-05-001-add-archived-flag"]
	if got := data.Steps[0].Update.Type; got != bson.TypeEmbeddedDocument {
		t.Errorf("update document type = %v, want embedded document", got)
	}
	if got := data.Steps[1].Update.Type; got != bson.TypeArray {
		t.Errorf("update pipeline type = %v, want array", got)
	}
	if got := data.Steps[2].Filter.Lookup("_id").Type; got != bson.TypeObjectID {
		t.Errorf("$oid filter type = %v, want ObjectID", got)
	}
	if got := data.Steps[2].Filter.Lookup("createdAt", "$lt").Type; got != bson.TypeDateTime {
		t.Errorf("$date filter type = %v, want DateTime", got)
	}

	seed := byID["2026-11-05-002-seed-price-table-v1"]
	prices := seed.Steps[0].Update.Document().Lookup("$setOnInsert", "prices")
	if got := prices.Document().Lookup("basic").Type; got != bson.TypeDecimal128 {
		t.Errorf("$numberDecimal type = %v, want Decimal128", got)
	}
	if got := prices.Document().Lookup("pro").Type; got != bson.TypeInt64 {
		t.Errorf("$numberLong type = %v, want Int64", got)
	}
	if upsert, ok := seed.Steps[0].Options.Lookup("upsert").BooleanOK(); !ok || !upsert {
		t.Errorf("options.upsert = %v, want true", upsert)
	}

	index := byID["2026-11-05-003-account-name-ci-index"]
	keys, err := index.Steps[0].Keys.Elements()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, k := range keys {
		names = append(names, k.Key())
	}
	if !slices.Equal(names, []string{"name", "createdAt"}) {
		t.Errorf("index keys = %v, want [name createdAt] in file order", names)
	}
	if got := index.Steps[1].Name; got != "name_1" {
		t.Errorf("dropIndex name = %q, want name_1", got)
	}

	merge := byID["2026-11-05-004-denormalize-project-owner-name"]
	if got := len(merge.Steps[0].Pipeline); got != 4 {
		t.Fatalf("pipeline has %d stages, want 4", got)
	}
	if _, err := merge.Steps[0].Pipeline[3].LookupErr("$merge"); err != nil {
		t.Errorf("last stage is not $merge: %v", err)
	}
}

func TestLoadDirectory(t *testing.T) {
	ms, err := load(filepath.Join("testdata", "valid"), discard())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	// README.md is not a migration; results are sorted by id.
	want := []string{
		"2026-11-05-001-add-archived-flag",
		"2026-11-05-002-seed-price-table-v1",
		"2026-11-05-003-account-name-ci-index",
		"2026-11-05-004-denormalize-project-owner-name",
	}
	if !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

// TestLoadSortsByID checks the migrations come sorted by id, not by file
// name: "-" sorts before ".", so the file of 2026-11-05-001-a-b comes before
// the file of 2026-11-05-001-a, but the id 2026-11-05-001-a sorts first.
func TestLoadSortsByID(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"2026-11-05-001-a-b", "2026-11-05-001-a"} {
		body := `{"id":"` + id + `","description":"d","steps":[{"collection":"c","operation":"deleteMany","filter":{"x":1}}]}`
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 || entries[0].Name() != "2026-11-05-001-a-b.json" {
		t.Fatalf("directory order %v, %v: want the file of 2026-11-05-001-a-b first", entries, err)
	}
	ms, err := load(dir, discard())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	if want := []string{"2026-11-05-001-a", "2026-11-05-001-a-b"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := []struct {
		file    string
		pointer string // for schema errors: a violation must sit at this JSON pointer
		want    string // substring of that violation's message, or of the error otherwise
	}{
		// Not JSON, or not Extended JSON.
		{file: "2026-01-01-100-broken-json", want: "invalid JSON: unexpected EOF"},
		{file: "2026-01-01-101-trailing-data", want: "invalid JSON: invalid character after top-level value"},
		{file: "2026-01-01-102-bom", want: "invalid JSON: it starts with a UTF-8 byte order mark; save it as UTF-8 without one"},
		{file: "2026-01-01-045-extjson-bad-oid", want: "invalid Extended JSON: error decoding key steps.0.filter"},
		{file: "2026-01-01-046-extjson-bad-date", want: "invalid Extended JSON: error decoding key steps.0.filter"},
		{file: "2026-01-01-047-extjson-bad-number-long", want: "invalid Extended JSON: error decoding key steps.0.update"},
		// File names that don't carry a date.
		{file: "2026-13-01-001-bad-month", want: `file name must start with a valid YYYY-MM-DD date: "2026-13-01"`},
		{file: "add-flag", want: "file name must start with a YYYY-MM-DD date"},
		// The id field must match the file name.
		// A key twice in the same object: JSON parsers disagree on which
		// wins, and the schema saw the last into while the runner would
		// see the first.
		{file: "2026-01-01-054-duplicate-key", want: `duplicate key: /steps/0/pipeline/1/$merge/into: "into" appears twice in the same object`},
		{file: "2026-01-01-048-id-mismatch", want: `id "2026-01-01-048-another-name" doesn't match the file name; want "2026-01-01-048-id-mismatch"`},
		// Top-level shape.
		{file: "2026-01-01-001-not-an-object", pointer: "", want: "got array, want object"},
		{file: "2026-01-01-002-missing-id", pointer: "", want: "missing property 'id'"},
		{file: "2026-01-01-003-missing-description", pointer: "", want: "missing property 'description'"},
		{file: "2026-01-01-004-missing-steps", pointer: "", want: "missing property 'steps'"},
		{file: "2026-01-01-005-extra-property", pointer: "", want: "additional properties 'cutDate' not allowed"},
		{file: "2026-01-01-006-bad-id-pattern", pointer: "/id", want: "does not match pattern"},
		{file: "2026-01-01-007-empty-description", pointer: "/description", want: "minLength"},
		{file: "2026-01-01-008-empty-steps", pointer: "/steps", want: "minItems"},
		{file: "2026-01-01-009-steps-not-array", pointer: "/steps", want: "got object, want array"},
		// Unknown or undetectable kind.
		{file: "2026-01-01-010-mixed-kinds", pointer: "/steps/1/operation", want: "createIndex belongs to index migrations, but the file's first operation, updateMany, belongs to data migrations"},
		{file: "2026-01-01-011-insert-operation", pointer: "/steps/0/operation", want: "insertOne is not an operation migrune runs: use updateOne, updateMany or deleteMany (data), createIndex or dropIndex (index), or aggregate (merge); seed documents with updateOne, upsert: true and $setOnInsert"},
		{file: "2026-01-01-012-delete-one", pointer: "/steps/0/operation", want: "deleteOne is not an operation migrune runs"},
		{file: "2026-01-01-013-replace-one", pointer: "/steps/0/operation", want: "replaceOne is not an operation migrune runs"},
		{file: "2026-01-01-014-missing-operation", pointer: "/steps/0", want: "missing property 'operation'"},
		{file: "2026-01-01-015-missing-collection", pointer: "/steps/0", want: "missing property 'collection'"},
		{file: "2026-01-01-016-empty-collection", pointer: "/steps/0/collection", want: "minLength"},
		// Data steps.
		{file: "2026-01-01-017-data-missing-filter", pointer: "/steps/0", want: "missing property 'filter'"},
		{file: "2026-01-01-018-data-missing-update", pointer: "/steps/0", want: "missing property 'update'"},
		{file: "2026-01-01-019-data-delete-with-update", pointer: "/steps/0", want: "update is not allowed: deleteMany takes no update"},
		{file: "2026-01-01-020-data-empty-update-pipeline", pointer: "/steps/0/update", want: "minItems"},
		{file: "2026-01-01-021-data-update-string", pointer: "/steps/0/update", want: "got string, want array or object"},
		{file: "2026-01-01-022-data-filter-not-object", pointer: "/steps/0/filter", want: "got array, want object"},
		{file: "2026-01-01-023-data-options-not-object", pointer: "/steps/0/options", want: "got boolean, want object"},
		{file: "2026-01-01-024-data-extra-property", pointer: "/steps/0", want: "additional properties 'upsert' not allowed"},
		// Index steps.
		{file: "2026-01-01-025-index-create-missing-keys", pointer: "/steps/0", want: "missing property 'keys'"},
		{file: "2026-01-01-026-index-create-missing-options", pointer: "/steps/0", want: "missing property 'options'"},
		{file: "2026-01-01-027-index-create-unnamed", pointer: "/steps/0/options", want: "missing property 'name'"},
		{file: "2026-01-01-028-index-create-empty-keys", pointer: "/steps/0/keys", want: "minProperties"},
		{file: "2026-01-01-029-index-create-with-name", pointer: "/steps/0", want: "name is not allowed in createIndex: name the index in options.name"},
		{file: "2026-01-01-030-index-drop-missing-name", pointer: "/steps/0", want: "missing property 'name'"},
		{file: "2026-01-01-031-index-drop-with-keys", pointer: "/steps/0", want: "keys and options are not allowed in dropIndex"},
		{file: "2026-01-01-032-index-drop-with-options", pointer: "/steps/0", want: "keys and options are not allowed in dropIndex"},
		{file: "2026-01-01-033-index-extra-property", pointer: "/steps/0", want: "additional properties 'background' not allowed"},
		// Merge steps.
		{file: "2026-01-01-034-merge-missing-pipeline", pointer: "/steps/0", want: "missing property 'pipeline'"},
		{file: "2026-01-01-035-merge-empty-pipeline", pointer: "/steps/0/pipeline", want: "minItems"},
		{file: "2026-01-01-036-merge-no-merge-stage", pointer: "/steps/0/pipeline", want: "the pipeline has no $merge stage"},
		{file: "2026-01-01-037-merge-two-merge-stages", pointer: "/steps/0/pipeline", want: "the pipeline has 2 $merge stages, at indexes 1, 2"},
		{file: "2026-01-01-038-merge-out-stage", pointer: "/steps/0/pipeline/1", want: "$out is not allowed: it replaces the whole target collection"},
		{file: "2026-01-01-039-merge-when-matched-replace", pointer: "/steps/0/pipeline/1/$merge/whenMatched", want: `must be "merge", got "replace"`},
		{file: "2026-01-01-040-merge-when-not-matched-insert", pointer: "/steps/0/pipeline/1/$merge/whenNotMatched", want: `must be "discard", got "insert"`},
		{file: "2026-01-01-041-merge-missing-into", pointer: "/steps/0/pipeline/1/$merge", want: `missing property 'into': $merge takes into, on, whenMatched: "merge" and whenNotMatched: "discard"`},
		{file: "2026-01-01-042-merge-into-object", pointer: "/steps/0/pipeline/1/$merge/into", want: "got object, want string"},
		{file: "2026-01-01-043-merge-extra-option", pointer: "/steps/0/pipeline/1/$merge", want: "additional properties 'let' not allowed"},
		{file: "2026-01-01-044-merge-extra-property", pointer: "/steps/0", want: "additional properties 'options' not allowed"},
		{file: "2026-01-01-049-merge-on-empty-string", pointer: "/steps/0/pipeline/1/$merge/on", want: "minLength"},
		{file: "2026-01-01-050-merge-on-empty-array", pointer: "/steps/0/pipeline/1/$merge/on", want: "minItems"},
		{file: "2026-01-01-051-merge-on-repeated", pointer: "/steps/0/pipeline/1/$merge/on", want: "items at 0 and 1 are equal"},
		{file: "2026-01-01-052-merge-on-number", pointer: "/steps/0/pipeline/1/$merge/on", want: "got number, want string or array"},
		{file: "2026-01-01-053-merge-on-array-of-numbers", pointer: "/steps/0/pipeline/1/$merge/on/1", want: "got number, want string"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			ms, err := loadOne(t, filepath.Join("testdata", "invalid", tc.file+".json"))
			if err == nil {
				t.Fatalf("Load returned %d migrations and no error", len(ms))
			}
			if ms != nil {
				t.Errorf("Load returned migrations alongside an error")
			}
			if !strings.Contains(err.Error(), tc.file+".json") {
				t.Errorf("error doesn't name the file: %v", err)
			}
			var se *SchemaError
			isSchema := errors.As(err, &se)
			wantSchema := !strings.Contains(tc.want, "JSON") && !strings.Contains(tc.want, "file name") && !strings.Contains(tc.want, "duplicate key")
			if isSchema != wantSchema {
				t.Fatalf("schema error = %v, want %v: %v", isSchema, wantSchema, err)
			}
			if !isSchema {
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error %q doesn't contain %q", err, tc.want)
				}
				return
			}
			// Each fixture has one problem, reported once.
			if len(se.Violations) != 1 {
				t.Errorf("got %d violations, want 1:\n%v", len(se.Violations), err)
			}
			for _, v := range se.Violations {
				if v.Pointer == tc.pointer && strings.Contains(v.Message, tc.want) {
					return
				}
			}
			t.Errorf("no violation at %q containing %q in:\n%v", tc.pointer, tc.want, err)
		})
	}
}

func TestLoadFileParseErrors(t *testing.T) {
	cases := []struct {
		file string
		want error
	}{
		{file: "2026-01-01-100-broken-json", want: ErrInvalidJSON},
		{file: "2026-01-01-045-extjson-bad-oid", want: ErrInvalidExtendedJSON},
		{file: "2026-01-01-054-duplicate-key", want: ErrDuplicateKey},
		{file: "2026-01-01-102-bom", want: ErrBOM},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			_, err := LoadFile(filepath.Join("testdata", "invalid", tc.file+".json"))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.want)
			}
		})
	}
}

// TestLoadInvalidDirectory checks that every invalid file is reported, not
// just the first.
func TestLoadInvalidDirectory(t *testing.T) {
	_, err := load(filepath.Join("testdata", "invalid"), discard())
	if err == nil {
		t.Fatal("Load returned no error")
	}
	files, err2 := filepath.Glob(filepath.Join("testdata", "invalid", "*.json"))
	if err2 != nil {
		t.Fatal(err2)
	}
	for _, f := range files {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error doesn't report %s", f)
		}
	}
}

func TestLoadMissingDirectory(t *testing.T) {
	if _, err := load(filepath.Join("testdata", "does-not-exist"), discard()); err == nil {
		t.Fatal("Load returned no error")
	}
}

// TestLoadDatedAhead checks a file's date doesn't affect whether it loads:
// one dated far after today is loaded, without a warning.
func TestLoadDatedAhead(t *testing.T) {
	var logs bytes.Buffer
	ms, err := load(filepath.Join("testdata", "dated-ahead"), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != "2999-01-01-001-far-ahead" {
		t.Errorf("loaded %v, want 2999-01-01-001-far-ahead", ms)
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected logs: %s", logs.String())
	}
}

// TestChecksum checks a migration's checksum is the sha256sum of its file.
func TestChecksum(t *testing.T) {
	ms, err := loadOne(t, filepath.Join("testdata", "valid", "2026-11-05-001-add-archived-flag.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "359f225472152e72742f43dc867c94b0301c64daf3ad070d38cc3bccd1c3fa23"; ms[0].Checksum != want {
		t.Errorf("checksum = %s, want %s", ms[0].Checksum, want)
	}
}

// TestChecksumRawBytes checks the checksum covers the raw bytes, so a change
// that doesn't alter the parsed migration still changes it.
func TestChecksumRawBytes(t *testing.T) {
	fixture := filepath.Join("testdata", "valid", "2026-11-05-001-add-archived-flag.json")
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := loadOne(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(fixture)), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	edited, err := load(dir, discard())
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].Checksum == edited[0].Checksum {
		t.Error("checksum unchanged after adding a trailing newline")
	}
}

// TestLoadReserved checks every place a migration can name a collection
// refuses migrune's own collections and system.* ones: the schema for a
// step's collection and $merge.into, a check at load for the collections a
// pipeline reads or writes, at any depth.
func TestLoadReserved(t *testing.T) {
	cases := []struct {
		file    string
		schema  bool
		pointer string
		want    string
	}{
		{file: "2026-11-01-001-changelog-data", schema: true, pointer: "/steps/0/collection", want: "the collection is reserved"},
		{file: "2026-11-01-002-lock-index", schema: true, pointer: "/steps/0/collection", want: "the collection is reserved"},
		{file: "2026-11-01-003-system-collection", schema: true, pointer: "/steps/0/collection", want: "the collection is reserved"},
		{file: "2026-11-01-004-merge-into-changelog", schema: true, pointer: "/steps/0/pipeline/1/$merge/into", want: "the collection is reserved"},
		{file: "2026-11-01-005-lookup-from", pointer: "/steps/0/pipeline/0/$lookup/from", want: `"migrations_changelog" is reserved`},
		{file: "2026-11-01-006-graph-lookup-from", pointer: "/steps/0/pipeline/0/$graphLookup/from", want: `"system.users" is reserved`},
		{file: "2026-11-01-007-union-with-name", pointer: "/steps/0/pipeline/0/$unionWith", want: `"migrations_lock" is reserved`},
		{file: "2026-11-01-008-union-with-coll", pointer: "/steps/0/pipeline/0/$unionWith/coll", want: `"system.js" is reserved`},
		{file: "2026-11-01-009-facet-lookup", pointer: "/steps/0/pipeline/0/$facet/locks/0/$lookup/from", want: `"migrations_lock" is reserved`},
		{file: "2026-11-01-010-nested-union-with", pointer: "/steps/0/pipeline/0/$lookup/pipeline/0/$unionWith/pipeline/0/$unionWith", want: `"migrations_changelog" is reserved`},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			_, err := loadOne(t, filepath.Join("testdata", "reserved", tc.file+".json"))
			var violations []Violation
			var se *SchemaError
			var re *ReservedCollectionError
			switch {
			case tc.schema && errors.As(err, &se):
				violations = se.Violations
			case !tc.schema && errors.As(err, &re):
				violations = re.Violations
			default:
				t.Fatalf("Load: %v, want a schema error %v", err, tc.schema)
			}
			for _, v := range violations {
				if v.Pointer == tc.pointer && strings.Contains(v.Message, tc.want) && strings.Contains(v.Message, "system.*") {
					return
				}
			}
			t.Errorf("no violation at %q containing %q in:\n%v", tc.pointer, tc.want, err)
		})
	}
}

// TestLoadDirSymlink checks a symbolic link among the migrations fails the
// load instead of being followed.
func TestLoadDirSymlink(t *testing.T) {
	dir := t.TempDir()
	target, err := filepath.Abs(filepath.Join("testdata", "valid", "2026-11-05-001-add-archived-flag.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "2026-11-05-001-add-archived-flag.json")); err != nil {
		t.Skip("symbolic links not supported:", err)
	}
	if _, err := LoadDir(dir, discard()); !errors.Is(err, ErrNotRegular) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("LoadDir: %v, want %v", err, ErrNotRegular)
	}
}

// TestLoadIgnored checks a subdirectory and the files with an extension
// close to .json are skipped with a warning naming each, while other files
// are skipped silently.
func TestLoadIgnored(t *testing.T) {
	dir := filepath.Join("testdata", "ignored")
	var logs bytes.Buffer
	ms, err := load(dir, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != "2026-11-05-001-read" {
		t.Errorf("loaded %v, want only 2026-11-05-001-read", ms)
	}
	want := map[string]string{
		"2026-11-05-002-upper.JSON":      "its extension is .JSON",
		"2026-11-05-003-five.json5":      "its extension is .json5",
		"2026-11-05-004-commented.jsonc": "its extension is .jsonc",
		"archive":                        "it is a subdirectory",
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d log lines, want %d warnings:\n%s", len(lines), len(want), logs.String())
	}
	for _, line := range lines {
		var entry struct{ Level, Msg, Entry string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		reason, ok := want[filepath.Base(entry.Entry)]
		if !ok || entry.Level != "WARN" || !strings.Contains(entry.Msg, reason) {
			t.Errorf("unexpected log line: %s", line)
		}
		delete(want, filepath.Base(entry.Entry))
	}
}
