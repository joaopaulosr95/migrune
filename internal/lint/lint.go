// Package lint checks migration files offline.
//
// Every file in the migrations directory is checked on its own (file name,
// schema, Extended JSON, pipeline rules), then the files are checked together:
// prefix uniqueness and the index replay, optionally compared with a list of
// expected indexes.
package lint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// Severity tells whether a finding fails the lint.
type Severity string

const (
	// Error fails the lint.
	Error Severity = "error"
	// Warning is reported but doesn't fail the lint.
	Warning Severity = "warning"
)

// Rules reported by lint. docs/reference/lint-rules.md describes each one.
const (
	RuleFileName        = "file-name"
	RuleIgnoredEntry    = "ignored-entry"
	RuleJSON            = "json"
	RuleLineEndings     = "line-endings"
	RuleExtendedJSON    = "extended-json"
	RuleSchema          = "schema"
	RuleID              = "id"
	RuleDuplicatePrefix = "duplicate-prefix"
	RuleDuplicateKey    = "duplicate-key"
	RuleRegularFile     = "regular-file"
	RuleFileSize        = "file-size"
	RuleEmptyFilter     = "empty-filter"
	RuleMergeLast       = "merge-last"
	RuleNested          = "nested-out-merge"
	RuleVolatileMerge   = "volatile-merge"
	RuleServerJS        = "server-js"
	RuleReserved        = "reserved-collection"
	RuleIndexNameReused = "index-name-reused"
	RuleIndexConflict   = "index-conflict"
	RuleDropThenCreate  = "drop-then-create"
	RuleTTLIndex        = "ttl-index"
	RuleIndexMissing    = "index-missing"
	RuleIndexOptions    = "index-options"
	RuleIndexExtra      = "index-extra"
	RuleIndexRenamed    = "index-renamed"
	RuleChecksum        = "checksum"
	RuleAppliedMissing  = "applied-missing"
)

// Finding is one problem found by lint.
type Finding struct {
	File     string   `json:"file"`
	Severity Severity `json:"severity"`
	Rule     string   `json:"rule"`
	Message  string   `json:"message"`
}

// String renders the finding as "<file>: <severity>: <rule>: <message>".
func (f Finding) String() string {
	return fmt.Sprintf("%s: %s: %s: %s", f.File, f.Severity, f.Rule, f.Message)
}

// Run lints every *.json file in dir (subdirectories are not read), and warns
// on the entries plan and apply would ignore although they may be meant as
// migrations (migration.IgnoredEntry).
// When expected is not nil, the indexes the migrations create are compared
// with it. When applied is not nil, it maps the id of every migration Finished
// in the target database to what its latest Finished entry recorded, and a
// file whose checksum differs is an error, as is an applied id with no
// file in dir. Findings are sorted by
// file. Run returns an error only when dir or a file in it can't be read.
func Run(dir string, expected *ExpectedIndexes, applied map[string]migration.Applied) ([]Finding, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}
	var (
		findings   []Finding
		migrations []migration.Migration
		prefixes   = map[string][]string{}
		stems      = map[string]bool{}
	)
	for _, e := range entries {
		if reason := migration.IgnoredEntry(e); reason != "" {
			findings = append(findings, Finding{File: filepath.Join(dir, e.Name()), Severity: Warning, Rule: RuleIgnoredEntry, Message: "not read by lint, plan or apply: " + reason})
			continue
		}
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		stem := strings.TrimSuffix(e.Name(), ".json")
		stems[stem] = true
		_, err := migration.FileDate(stem)
		ok := err == nil
		badName := checkFileName(stem, ok)
		if badName != "" {
			findings = append(findings, Finding{File: path, Severity: Error, Rule: RuleFileName, Message: badName})
		}
		if migration.IDPattern.MatchString(stem) {
			p := stem[:len("YYYY-MM-DD-NNN")]
			prefixes[p] = append(prefixes[p], path)
		}
		raw, err := migration.ReadFile(path)
		if errors.Is(err, migration.ErrNotRegular) || errors.Is(err, migration.ErrTooLarge) {
			findings = append(findings, loadFindings(path, err)...)
			continue
		}
		if err != nil {
			return nil, err
		}
		fileFindings := checkFile(path, stem, raw)
		findings = append(findings, fileFindings...)
		m, err := migration.LoadFile(path)
		if err != nil {
			fs := loadFindings(path, err)
			if badName != "" || slices.ContainsFunc(fileFindings, func(f Finding) bool { return f.Rule == RuleID }) {
				// The file-name or id finding already says what the id must
				// be; the schema's id pattern finding would say it twice.
				fs = slices.DeleteFunc(fs, func(f Finding) bool { return f.Rule == RuleSchema && strings.HasPrefix(f.Message, "/id: '") })
			}
			findings = append(findings, fs...)
			continue
		}
		migrations = append(migrations, m)
	}
	findings = append(findings, duplicatePrefixes(prefixes)...)

	slices.SortFunc(migrations, func(a, b migration.Migration) int { return strings.Compare(a.ID, b.ID) })
	indexes, fs := replayIndexes(migrations)
	findings = append(findings, fs...)
	if expected != nil {
		findings = append(findings, compareIndexes(indexes, expected)...)
	}
	findings = append(findings, compareChecksums(migrations, applied)...)
	findings = append(findings, appliedMissing(dir, stems, applied)...)
	slices.SortStableFunc(findings, func(a, b Finding) int { return strings.Compare(a.File, b.File) })
	return findings, nil
}

func checkFileName(stem string, validDate bool) string {
	if !migration.IDPattern.MatchString(stem) {
		return "file name must be YYYY-MM-DD-NNN-slug.json, with a slug of lowercase letters and digits in dash-separated words"
	}
	if !validDate {
		return fmt.Sprintf("file name date %s is not a valid date", stem[:len(migration.DateLayout)])
	}
	return ""
}

// loadFindings turns an error from migration.LoadFile into findings.
func loadFindings(path string, err error) []Finding {
	var se *migration.SchemaError
	if errors.As(err, &se) {
		var out []Finding
		for _, v := range se.Violations {
			ptr := v.Pointer
			if ptr == "" {
				ptr = "(root)"
			}
			out = append(out, Finding{File: path, Severity: Error, Rule: RuleSchema, Message: ptr + ": " + v.Message})
		}
		return out
	}
	var re *migration.ReservedCollectionError
	if errors.As(err, &re) {
		var out []Finding
		for _, v := range re.Violations {
			out = append(out, Finding{File: path, Severity: Error, Rule: RuleReserved, Message: v.Pointer + ": " + v.Message})
		}
		return out
	}
	rule := RuleSchema
	switch {
	case errors.Is(err, migration.ErrBOM):
		return []Finding{{File: path, Severity: Error, Rule: RuleLineEndings, Message: "the file starts with a UTF-8 byte order mark (BOM): " + checksumAdvice}}
	case errors.Is(err, migration.ErrInvalidJSON):
		rule = RuleJSON
	case errors.Is(err, migration.ErrInvalidExtendedJSON):
		rule = RuleExtendedJSON
	case errors.Is(err, migration.ErrDuplicateKey):
		rule = RuleDuplicateKey
	case errors.Is(err, migration.ErrNotRegular):
		rule = RuleRegularFile
	case errors.Is(err, migration.ErrTooLarge):
		rule = RuleFileSize
	}
	return []Finding{{File: path, Severity: Error, Rule: rule, Message: strings.TrimPrefix(err.Error(), path+": ")}}
}

func duplicatePrefixes(prefixes map[string][]string) []Finding {
	var out []Finding
	for p, paths := range prefixes {
		if len(paths) < 2 {
			continue
		}
		for _, path := range paths {
			var others []string
			for _, o := range paths {
				if o != path {
					others = append(others, filepath.Base(o))
				}
			}
			out = append(out, Finding{File: path, Severity: Error, Rule: RuleDuplicatePrefix, Message: fmt.Sprintf(
				"prefix %s is also used by %s; rename the migration that was merged last", p, strings.Join(others, ", "))})
		}
	}
	return out
}

// compareChecksums reports the migrations whose file changed after they were
// applied. applied maps a migration id to its latest Finished changelog
// entry; migrations with only Errored entries aren't in it and may be edited.
func compareChecksums(migrations []migration.Migration, applied map[string]migration.Applied) []Finding {
	var out []Finding
	for _, m := range migrations {
		a, ok := applied[m.ID]
		if !ok || a.Checksum == m.Checksum {
			continue
		}
		out = append(out, Finding{File: m.Path, Severity: Error, Rule: RuleChecksum, Message: fmt.Sprintf(
			"the file changed after the migration was applied: the changelog has checksum %s, the file %s. "+
				"Applied migrations are immutable: revert the edit and write a new migration", a.Checksum, m.Checksum)})
	}
	return out
}

// appliedMissing reports the migrations Finished in applied with no file in
// dir, stems being the names of the files found there without .json. Each
// finding is on the file the migration would have, and says what the
// changelog stored about it.
func appliedMissing(dir string, stems map[string]bool, applied map[string]migration.Applied) []Finding {
	var out []Finding
	for id, a := range applied {
		if stems[id] {
			continue
		}
		out = append(out, Finding{File: filepath.Join(dir, id+".json"), Severity: Error, Rule: RuleAppliedMissing, Message: a.MissingContext() + ". plan and apply fail until the file is restored: removing a file doesn't undo its migration; " +
			"to retire a migration on purpose, delete its changelog entries by hand"})
	}
	slices.SortFunc(out, func(a, b Finding) int { return strings.Compare(a.File, b.File) })
	return out
}
