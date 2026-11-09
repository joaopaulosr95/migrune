package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/joaopaulosr95/migrune/internal/migration"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RowStatus is the outcome of one migration in a report. For plan it
// is what apply would do.
type RowStatus string

const (
	RowApplied RowStatus = "applied"
	RowErrored RowStatus = "errored"
	// RowInterrupted is the migration in progress when the run was
	// interrupted. It stays pending.
	RowInterrupted RowStatus = "interrupted"
	// RowSkipped is a migration not reached because an earlier one errored
	// or the run was interrupted or failed a check.
	RowSkipped RowStatus = "skipped"
)

// Report is the outcome of a plan or apply run. docs/reference/report.md
// documents its JSON shape.
type Report struct {
	// Version is migrune's version, set by the caller.
	Version string `json:"version"`
	// Command is "plan" or "apply".
	Command string `json:"command"`
	// Database is the target database's name.
	Database string `json:"database"`
	// Namespace is the run's namespace; absent for the default one.
	Namespace string `json:"namespace,omitempty"`
	// StartedAt is when the run started, in UTC, to the millisecond.
	StartedAt time.Time `json:"startedAt"`
	Runner    string    `json:"runner"`
	// ApprovedBy is who approved an apply run, as given; absent in plan and
	// when neither name nor email was given.
	ApprovedBy *ApprovedBy `json:"approvedBy,omitempty"`
	// NewDatabase is set when the target database had no collections but
	// migrune's (an earlier plan leaves the lock and an empty changelog
	// collection) and no changelog entry when the run started: a mistyped
	// --db targets a new, empty database.
	NewDatabase bool `json:"newDatabase"`
	// Digest is the digest of DigestInput. plan writes both with
	// --digest-out. Absent when the run stopped before reading the
	// changelog.
	Digest      string       `json:"digest,omitempty"`
	DigestInput *DigestInput `json:"-"`
	// Migrations has one row per migration considered, in id order.
	Migrations []Row `json:"migrations"`
	// AppliedButMissing lists the migrations Finished in the changelog whose
	// file is gone, with what their latest Finished entry stored. Sorted by id.
	AppliedButMissing []migration.Applied `json:"appliedButMissing"`
	Totals            Totals              `json:"totals"`
	// Error is why the run failed, if it did.
	Error string `json:"error,omitempty"`
}

// Row is one migration in a report.
type Row struct {
	ID         string         `json:"id"`
	Kind       migration.Kind `json:"kind"`
	Status     RowStatus      `json:"status"`
	DurationMs int64          `json:"durationMs"`
	Error      string         `json:"error,omitempty"`
	// Steps has one item per step of a data migration whose first run
	// completed, with what MongoDB reported for it: in plan, what apply
	// would do to the data as it is now; in apply, what it did. Absent for
	// other kinds, for skipped rows, and when the first run failed.
	Steps []DataStep `json:"steps,omitempty"`
	// Indexes has one item per step of an index migration that ran: in
	// plan, what the step would do to the indexes as they are now, changed
	// by the pending index migrations planned before it; in
	// apply, what it did. A plan step predicted to fail is the last item,
	// with its Conflict. Absent for other kinds and for skipped rows.
	Indexes []IndexChange `json:"indexes,omitempty"`

	// MergeCounts are set for merge migrations only, summed over the steps
	// that ran.
	MergeCounts
	// Samples are a few documents that differed from their target before
	// the merge, those with a type change first: their on fields, and the
	// names, types and nested paths of the fields that differ, with their
	// values only with sample values on.
	Samples []MergeSample `json:"samples,omitempty"`
}

// MergeCounts are the counts of a merge migration: of one step in
// a changelog entry, summed over the steps that ran in a report row.
// DiffCountBefore counts the documents $merge would change, before it ran;
// DiffCountAfter those still differing after it ran (apply only; 0 when the
// migration settled). UnmatchedCount counts the output documents with no
// match in the target, which $merge discards. TypeChangedCount counts the
// documents where $merge would change the BSON type of an existing, non-null
// field, or of a non-null value nested in it (see MergeField.Path); above 0
// fails the migration. DuplicateOnCount counts the output documents whose on
// fields are equal to another's and match a target document. NullOnCount
// counts the output documents with a null or missing on field, ArrayOnCount
// those with an array as an on field, and IDMismatchCount those whose _id
// differs from their target's while the on fields don't include _id. Any of
// these four above 0 fails the migration.
type MergeCounts struct {
	DiffCountBefore  *int64 `json:"diffCountBefore,omitempty" bson:"diffCountBefore,omitempty"`
	DiffCountAfter   *int64 `json:"diffCountAfter,omitempty" bson:"diffCountAfter,omitempty"`
	UnmatchedCount   *int64 `json:"unmatchedCount,omitempty" bson:"unmatchedCount,omitempty"`
	TypeChangedCount *int64 `json:"typeChangedCount,omitempty" bson:"typeChangedCount,omitempty"`
	DuplicateOnCount *int64 `json:"duplicateOnCount,omitempty" bson:"duplicateOnCount,omitempty"`
	NullOnCount      *int64 `json:"nullOnCount,omitempty" bson:"nullOnCount,omitempty"`
	ArrayOnCount     *int64 `json:"arrayOnCount,omitempty" bson:"arrayOnCount,omitempty"`
	IDMismatchCount  *int64 `json:"idMismatchCount,omitempty" bson:"idMismatchCount,omitempty"`
	// CollationOnlyMatchCount is set only when the unique index $merge
	// matches the on fields through has a non-simple collation, such as a
	// case-insensitive one: it counts the output documents with no exact
	// match in the target (so also in UnmatchedCount) that match one
	// through that collation. MongoDB 8.3 and later merge them, rewriting
	// the target's on fields to the output's values; 7.0 to 8.2 discard them.
	CollationOnlyMatchCount *int64 `json:"collationOnlyMatchCount,omitempty" bson:"collationOnlyMatchCount,omitempty"`
}

// Counts renders the counts that are set, in field order, e.g.
// "diffCountBefore 3, unmatchedCount 0".
func (c MergeCounts) Counts() string {
	var parts []string
	for _, f := range c.fields() {
		if *f.p != nil {
			parts = append(parts, fmt.Sprintf("%s %d", f.name, **f.p))
		}
	}
	return strings.Join(parts, ", ")
}

// add adds o's counts to c's, leaving those o lacks.
func (c *MergeCounts) add(o MergeCounts) {
	from := o.fields()
	for i, f := range c.fields() {
		n := *from[i].p
		if n == nil {
			continue
		}
		if *f.p == nil {
			*f.p = new(int64)
		}
		**f.p += *n
	}
}

type mergeCount struct {
	name string
	p    **int64
}

// fields returns c's counts with their JSON names, in field order.
func (c *MergeCounts) fields() []mergeCount {
	return []mergeCount{
		{"diffCountBefore", &c.DiffCountBefore},
		{"diffCountAfter", &c.DiffCountAfter},
		{"unmatchedCount", &c.UnmatchedCount},
		{"typeChangedCount", &c.TypeChangedCount},
		{"duplicateOnCount", &c.DuplicateOnCount},
		{"nullOnCount", &c.NullOnCount},
		{"arrayOnCount", &c.ArrayOnCount},
		{"idMismatchCount", &c.IDMismatchCount},
		{"collationOnlyMatchCount", &c.CollationOnlyMatchCount},
	}
}

// DataStep is what one step of a data migration matched and changed, in the
// first of its two runs. The counts that don't apply to the operation are
// absent: deleteMany has only Deleted, the updates the others.
type DataStep struct {
	// Index is the step's index in file order.
	Index      int    `json:"index"`
	Operation  string `json:"operation"`
	Collection string `json:"collection"`
	Matched    *int64 `json:"matched,omitempty"`
	Modified   *int64 `json:"modified,omitempty"`
	Upserted   *int64 `json:"upserted,omitempty"`
	Deleted    *int64 `json:"deleted,omitempty"`
}

// Counts renders the counts that apply, e.g. "matched 3, modified 3,
// upserted 0".
func (s DataStep) Counts() string {
	var parts []string
	for _, c := range s.counts() {
		parts = append(parts, fmt.Sprintf("%s %d", c.name, c.n))
	}
	return strings.Join(parts, ", ")
}

type count struct {
	name string
	n    int64
}

// counts returns the counts that apply, in a fixed order.
func (s DataStep) counts() []count {
	var out []count
	for _, c := range []struct {
		name string
		n    *int64
	}{{"matched", s.Matched}, {"modified", s.Modified}, {"upserted", s.Upserted}, {"deleted", s.Deleted}} {
		if c.n != nil {
			out = append(out, count{c.name, *c.n})
		}
	}
	return out
}

func (s DataStep) logAttrs() []any {
	attrs := []any{"step", s.Index, "operation", s.Operation, "collection", s.Collection}
	for _, c := range s.counts() {
		attrs = append(attrs, c.name, c.n)
	}
	return attrs
}

// MergeSample is an output document of a merge step that differs from its
// target document.
type MergeSample struct {
	// Step is the step's index in file order.
	Step int `json:"step"`
	// Key holds the output document's $merge "on" fields, as relaxed
	// Extended JSON.
	Key json.RawMessage `json:"key"`
	// Fields are the output fields that differ, in output order.
	Fields []MergeField `json:"fields"`
}

// MergeField is one output field that differs from the target's.
type MergeField struct {
	Field string `json:"field"`
	// BeforeType and AfterType are the BSON types ($type names) of the
	// target's value ("missing" when the target lacks the field) and of
	// the output's, or, with Path, of the nested values at Path.
	BeforeType string `json:"beforeType"`
	AfterType  string `json:"afterType"`
	// Path is set when a nested value changes BSON type: the dotted path of
	// the shallowest such value, from the field, e.g. "price.amount" or
	// "lines.0.qty" (array elements by index).
	Path string `json:"path,omitempty"`
	// TypeChanged is set when the field exists in the target, not null,
	// with another BSON type, or when Path is set.
	TypeChanged bool `json:"typeChanged,omitempty"`
	// Before and After are the target's and the output's values, as
	// relaxed Extended JSON, only with sample values on (see
	// Config.SampleValues). Before is absent when the target lacks the
	// field. A value longer than MaxSampleValueChars characters is cut:
	// it becomes a JSON string of its first MaxSampleValueChars
	// characters followed by "…", and Truncated is set.
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// MaxSampleValueChars is how many characters of a value's Extended JSON a
// merge sample shows, with sample values on.
const MaxSampleValueChars = 200

// Describe renders the change on one line. Without values: `missing ->
// string`, `int -> double, type changed` or `type changed at c.n (long ->
// double)`. With values: `absent -> "a"`, `1 (int) -> 1.0 (double), type
// changed` or `{"n":5} -> {"n":5.0}, type changed at c.n (long -> double)`.
func (f MergeField) Describe() string {
	nested := "type changed at " + f.Path + " (" + f.BeforeType + " -> " + f.AfterType + ")"
	if f.After == nil {
		if f.Path != "" {
			return nested
		}
		out := f.BeforeType + " -> " + f.AfterType
		if f.TypeChanged {
			out += ", type changed"
		}
		return out
	}
	before := "absent"
	if f.Before != nil {
		before = string(f.Before)
	}
	after := string(f.After)
	if f.Path != "" {
		return before + " -> " + after + ", " + nested
	}
	if f.Before != nil && f.BeforeType != f.AfterType {
		before += " (" + f.BeforeType + ")"
		after += " (" + f.AfterType + ")"
	}
	out := before + " -> " + after
	if f.TypeChanged {
		out += ", type changed"
	}
	return out
}

// IndexAction is what an index step does, or would do, to its index.
type IndexAction string

const (
	IndexCreate IndexAction = "create"
	IndexDrop   IndexAction = "drop"
	// IndexPresent is a createIndex whose index already exists with the
	// same definition: a no-op.
	IndexPresent IndexAction = "present"
	// IndexAbsent is a dropIndex whose index doesn't exist: a no-op.
	IndexAbsent IndexAction = "absent"
	// IndexDiffers is a createIndex, in plan, whose index exists with a
	// collation plan can't compare with the step's: apply may do nothing
	// or fail.
	IndexDiffers IndexAction = "differs"
	// IndexModify is a collModIndex that changes at least one option of its
	// index. A collModIndex whose index already has every value it sets is
	// IndexPresent.
	IndexModify IndexAction = "modify"
)

// IndexChange is one index step in a report.
type IndexChange struct {
	Action     IndexAction `json:"action"`
	Collection string      `json:"collection"`
	Name       string      `json:"name"`
	// Keys is the key pattern, as relaxed Extended JSON: the step's for a
	// createIndex, the existing index's for a drop or a collModIndex.
	// Absent when the index is already absent, or missing.
	Keys json.RawMessage `json:"keys,omitempty"`
	// Options are the index options but its name, as relaxed Extended
	// JSON, for a createIndex or a drop, in the same way as Keys. Absent
	// when there are none, and for a collModIndex.
	Options json.RawMessage `json:"options,omitempty"`
	// Set is, for a collModIndex, the options the step sets: the values
	// after it. As relaxed Extended JSON.
	Set json.RawMessage `json:"set,omitempty"`
	// Before is, for a collModIndex whose index exists, the index's value
	// of each option in Set before the step: null for no
	// expireAfterSeconds, false for an absent boolean, and prepareUnique
	// true on a unique index.
	Before json.RawMessage `json:"before,omitempty"`
	// Conflict is why MongoDB would reject the step. plan only.
	Conflict string `json:"conflict,omitempty"`
	// Warning is why apply may fail although plan found no conflict. plan
	// only.
	Warning string `json:"warning,omitempty"`
	// WouldExpire is, for a TTL index plan would create, or a collModIndex
	// that adds a TTL to an index or lowers it, how many documents the
	// index would delete as soon as it is built or modified: those whose
	// indexed date is older than now less expireAfterSeconds. plan only.
	WouldExpire *int64 `json:"wouldExpire,omitempty"`
}

// Totals counts the rows of a report by status.
type Totals struct {
	Migrations  int `json:"migrations"`
	Applied     int `json:"applied"`
	Errored     int `json:"errored"`
	Interrupted int `json:"interrupted"`
	Skipped     int `json:"skipped"`
	// DurationMs is the duration of the whole run.
	DurationMs int64 `json:"durationMs"`
}

// row returns the row for id, or nil.
func (r *Report) row(id string) *Row {
	for i := range r.Migrations {
		if r.Migrations[i].ID == id {
			return &r.Migrations[i]
		}
	}
	return nil
}

// finish fills the totals and the run's error.
func (r *Report) finish(d time.Duration, err error) {
	t := Totals{Migrations: len(r.Migrations), DurationMs: d.Milliseconds()}
	for _, row := range r.Migrations {
		switch row.Status {
		case RowApplied:
			t.Applied++
		case RowErrored:
			t.Errored++
		case RowInterrupted:
			t.Interrupted++
		case RowSkipped:
			t.Skipped++
		}
	}
	r.Totals = t
	if err != nil {
		r.Error = err.Error()
	}
}

// rawExtJSON renders a document as relaxed Extended JSON, to embed in the
// JSON report.
func rawExtJSON(d bson.D) json.RawMessage {
	b, err := bson.MarshalExtJSON(d, false, false)
	if err != nil {
		b, _ = json.Marshal(err.Error())
	}
	return b
}

// extJSON renders a value as relaxed Extended JSON for logs and messages.
func extJSON(v any) string {
	if v == nil {
		return "none"
	}
	s, err := migration.ExtJSON(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return s
}
