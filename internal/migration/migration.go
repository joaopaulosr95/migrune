// Package migration loads migration files from a directory.
//
// Each *.json file is validated against the embedded migration file schema,
// parsed as MongoDB Extended JSON, and classified as a data, index or merge
// migration.
package migration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Kind is the kind of a migration. A file never mixes kinds.
type Kind string

const (
	KindData  Kind = "data"
	KindIndex Kind = "index"
	KindMerge Kind = "merge"
)

// kinds maps each allowed operation to the kind of migration it belongs to.
var kinds = map[string]Kind{
	"updateOne":    KindData,
	"updateMany":   KindData,
	"deleteMany":   KindData,
	"createIndex":  KindIndex,
	"dropIndex":    KindIndex,
	"collModIndex": KindIndex,
	"aggregate":    KindMerge,
}

// Errors wrapped by LoadFile and LoadDir when a file can't be read or parsed.
var (
	ErrInvalidJSON         = errors.New("invalid JSON")
	ErrInvalidExtendedJSON = errors.New("invalid Extended JSON")
	// ErrDuplicateKey is a key that appears twice in the same object:
	// JSON parsers disagree on which value wins, so the schema and the
	// runner could each see a different one.
	ErrDuplicateKey = errors.New("duplicate key")
)

// Migration is one parsed migration file.
type Migration struct {
	// ID is the file name stem, YYYY-MM-DD-NNN-slug.
	ID string
	// Path is the file the migration was loaded from.
	Path        string
	Description string
	Kind        Kind
	Steps       []Step
	// Checksum is the hex SHA256 of the raw file bytes.
	Checksum string
}

// Step is one operation of a migration. Which fields are set depends on the
// operation; the schema guarantees the combination is valid.
type Step struct {
	Collection string `bson:"collection"`
	Operation  string `bson:"operation"`
	// Data steps.
	Filter bson.Raw `bson:"filter,omitempty"`
	// Update is an update document or an update pipeline.
	Update  bson.RawValue `bson:"update,omitempty"`
	Options bson.Raw      `bson:"options,omitempty"`
	// Index steps: createIndex uses Keys and Options, dropIndex uses Name,
	// collModIndex uses Name and Set.
	Keys bson.Raw `bson:"keys,omitempty"`
	Name string   `bson:"name,omitempty"`
	// Set holds the index options a collModIndex step changes.
	Set bson.Raw `bson:"set,omitempty"`
	// Merge steps.
	Pipeline []bson.Raw `bson:"pipeline,omitempty"`
}

type file struct {
	ID          string `bson:"id"`
	Description string `bson:"description"`
	Steps       []Step `bson:"steps"`
}

// DateLayout is the layout of the date a migration id starts with.
const DateLayout = "2006-01-02"

// IDPattern is the migration file schema's pattern for ids, which are also
// file name stems.
var IDPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{3}-[a-z0-9]+(-[a-z0-9]+)*$`)

// Dir is what LoadDir found in a migrations directory.
type Dir struct {
	// Migrations are the files loaded, sorted by id.
	Migrations []Migration
}

// LoadDir reads every *.json file in dir (subdirectories are not read) and
// returns the migrations sorted by id. A subdirectory, or a file whose
// extension is close to .json (see IgnoredEntry), is skipped with a warning
// on logger. A file's date doesn't affect whether it is loaded. A file whose
// id field differs from its file name stem is invalid.
// If any file is invalid, LoadDir returns an empty Dir and an error
// naming every invalid file.
func LoadDir(dir string, logger *slog.Logger) (Dir, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Dir{}, fmt.Errorf("read migrations directory: %w", err)
	}
	var (
		migrations []Migration
		errs       []error
	)
	for _, e := range entries {
		if reason := IgnoredEntry(e); reason != "" {
			logger.Warn("ignoring an entry of the migrations directory: "+reason, "entry", filepath.Join(dir, e.Name()))
			continue
		}
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		id := strings.TrimSuffix(e.Name(), ".json")
		_, err := FileDate(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		m, declared, err := loadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if declared != m.ID {
			errs = append(errs, fmt.Errorf("%s: id %q doesn't match the file name; want %q", path, declared, m.ID))
			continue
		}
		migrations = append(migrations, m)
	}
	if len(errs) > 0 {
		return Dir{}, errors.Join(errs...)
	}
	slices.SortFunc(migrations, func(a, b Migration) int { return strings.Compare(a.ID, b.ID) })
	return Dir{Migrations: migrations}, nil
}

// nearMissExtensions are file extensions, lower-cased, that look like a
// migration's but aren't read.
var nearMissExtensions = []string{".json", ".json5", ".jsonc"}

// IgnoredEntry returns why migrune doesn't read e, an entry of the migrations
// directory that may have been meant as a migration: a subdirectory, or a
// file with an extension close to .json, such as .JSON or .jsonc. It returns
// "" for a *.json file, which is read, and for any other file, which is
// silently ignored.
func IgnoredEntry(e os.DirEntry) string {
	if e.IsDir() {
		return "it is a subdirectory, and only the files directly in the directory are read"
	}
	ext := filepath.Ext(e.Name())
	if ext != ".json" && slices.Contains(nearMissExtensions, strings.ToLower(ext)) {
		return fmt.Sprintf("its extension is %s, and only *.json files are read, as plain JSON", ext)
	}
	return ""
}

// FileDate returns the YYYY-MM-DD prefix of a migration id after checking it
// is a real date.
func FileDate(id string) (string, error) {
	if len(id) < len(DateLayout) {
		return "", fmt.Errorf("file name must start with a YYYY-MM-DD date")
	}
	date := id[:len(DateLayout)]
	if _, err := time.Parse(DateLayout, date); err != nil {
		return "", fmt.Errorf("file name must start with a valid YYYY-MM-DD date: %q", date)
	}
	return date, nil
}

// LoadFile reads, validates and parses one migration file. The migration id
// is the file name stem; LoadFile doesn't check the file name itself. A file
// ReadFile refuses returns an error wrapping ErrNotRegular or ErrTooLarge. A
// file that doesn't match the schema returns a *SchemaError; one that isn't
// JSON or Extended JSON returns an error wrapping ErrInvalidJSON or
// ErrInvalidExtendedJSON, and one with a key twice in the same object an
// error wrapping ErrDuplicateKey; one that names a reserved collection in a
// pipeline returns a *ReservedCollectionError.
func LoadFile(path string) (Migration, error) {
	m, _, err := loadFile(path)
	return m, err
}

// loadFile is LoadFile that also returns the id declared in the file.
func loadFile(path string) (Migration, string, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".json")
	raw, err := ReadFile(path)
	if err != nil {
		return Migration{}, "", err
	}
	if bytes.HasPrefix(raw, bom) {
		return Migration{}, "", fmt.Errorf("%s: %w: it starts with a %w; save it as UTF-8 without one", path, ErrInvalidJSON, ErrBOM)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return Migration{}, "", fmt.Errorf("%s: %w: %w", path, ErrInvalidJSON, err)
	}
	if ptr, key := duplicateKey(raw); ptr != "" {
		return Migration{}, "", fmt.Errorf("%s: %w: %s: %q appears twice in the same object; keep one", path, ErrDuplicateKey, ptr, key)
	}
	if err := schema.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return Migration{}, "", fmt.Errorf("%s: %w", path, err)
		}
		return Migration{}, "", &SchemaError{File: path, Violations: violations(ve, inst)}
	}
	var f file
	if err := bson.UnmarshalExtJSON(raw, false, &f); err != nil {
		return Migration{}, "", fmt.Errorf("%s: %w: %w", path, ErrInvalidExtendedJSON, err)
	}
	k := detectKind(f.Steps)
	if vs := ReservedCollections(f.Steps); len(vs) > 0 {
		return Migration{}, "", &ReservedCollectionError{File: path, Violations: vs}
	}
	sum := sha256.Sum256(raw)
	return Migration{
		ID:          id,
		Path:        path,
		Description: f.Description,
		Kind:        k,
		Steps:       f.Steps,
		Checksum:    hex.EncodeToString(sum[:]),
	}, f.ID, nil
}

// detectKind returns the kind of a migration's steps: the schema allows
// only known operations, at least one step, and one kind per file.
func detectKind(steps []Step) Kind {
	return kinds[steps[0].Operation]
}
