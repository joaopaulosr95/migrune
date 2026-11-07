package runner

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// runMerge runs a merge migration. Each step is an aggregate pipeline
// ending in $merge, which MongoDB doesn't allow in a transaction, so the steps
// run one by one, in file order, outside one.
//
// For each step whose on fields aren't only _id, the target must first have
// the unique index $merge matches through, with the pipeline's collation
// (checkMergeIndex); without one, the step fails before anything is written.
// A non-simple collation on the source, and so on that index, logs a warning
// about what the comparison can't check (warnCollation).
// Then the pipeline runs without its $merge stage, followed by a comparison
// with the target collection: compareMerge says what it counts. Unmatched
// output documents are a warning; null, array or duplicate on fields, an _id
// mismatch or a type change fail the step. plan stops there and writes
// nothing. apply then runs the whole pipeline and compares again: any
// document still differing (diffCountAfter) fails the migration, since a
// settled merge changes nothing. Only apply can find a merge that doesn't
// settle, such as one computing a field from its own value, and only after
// writing.
//
// With apply, the Finished entry is written after every step succeeded, in a
// fenced transaction of its own; a failure is recorded as
// Errored, unless the run was interrupted, which cancels the
// aggregate in progress. Either way, the counts and samples go into row.
func (r *Run) runMerge(m migration.Migration, apply bool, log *slog.Logger, row *Row) error {
	if err := r.lock.SetCurrent(r.ctx, m.ID); err != nil {
		return r.lockErr(err)
	}
	results := make([]StepResult, len(m.Steps))
	for i, s := range m.Steps {
		res, err := r.runMergeStep(i, s, apply, log.With("step", i, "collection", s.Collection), row)
		row.add(res.MergeCounts)
		if err != nil {
			return r.stepFailed(m, i, s, err, apply)
		}
		results[i] = res
	}
	if !apply {
		return nil
	}
	return r.writeFinishedAlone(m, results)
}

// runMergeStep compares, and with apply merges and compares again, one step.
func (r *Run) runMergeStep(i int, s migration.Step, apply bool, log *slog.Logger, row *Row) (StepResult, error) {
	res := StepResult{Collection: s.Collection, Operation: s.Operation}
	pre, target, err := splitMerge(s)
	if err != nil {
		return res, err
	}
	interrupted := func(err error) error {
		if rerr := r.Err(); rerr != nil {
			return rerr
		}
		return err
	}
	collation, err := r.defaultCollation(s.Collection)
	if err != nil {
		return res, interrupted(fmt.Errorf("read the default collation of %s: %w", s.Collection, err))
	}
	index, err := r.checkMergeIndex(s.Collection, collation, target, apply, log)
	if err != nil {
		return res, interrupted(err)
	}
	warnCollation(log, s.Collection, collation, target, index)
	before, err := r.compareMerge(s.Collection, pre, target, log)
	if err != nil {
		return res, fmt.Errorf("compare with %s: %w", target.Into, err)
	}
	res.MergeCounts = before.counts()
	if index.Collation != "" {
		n, err := r.collationOnlyMatches(s.Collection, pre, target, index, log)
		if err != nil {
			return res, fmt.Errorf("count the documents matching %s only through the collation of index %s: %w", target.Into, index.Name, err)
		}
		res.CollationOnlyMatchCount = &n
	}
	for _, sm := range before.Samples {
		sm.Step = i
		row.Samples = append(row.Samples, sm)
		log.Info("merge sample: a document that differs from its target", sampleAttrs(sm)...)
	}
	log.Info("merge comparison", "into", target.Into, "on", strings.Join(target.On, ","),
		"diffCount", before.Diff, "unmatchedCount", before.Unmatched, "typeChangedCount", before.TypeChanged,
		"duplicateOnCount", before.DuplicateOn, "nullOnCount", before.NullOn, "arrayOnCount", before.ArrayOn,
		"idMismatchCount", before.IDMismatch)
	if before.Unmatched > 0 {
		log.Warn("output documents have no match in the target: $merge discards them; check the on fields",
			"into", target.Into, "on", strings.Join(target.On, ","), "unmatchedCount", before.Unmatched)
	}
	on := strings.Join(target.On, ",")
	if before.NullOn > 0 {
		return res, fmt.Errorf("nullOnCount is %d: that many output documents have a null or missing on field (%s): "+
			"$merge would match them with a target whose field is null or missing, or with none; "+
			"output only documents with every on field set", before.NullOn, on)
	}
	if before.ArrayOn > 0 {
		return res, fmt.Errorf("arrayOnCount is %d: that many output documents have an array as an on field (%s): "+
			"$merge would fail on them part way through (error 51185); output one value per on field", before.ArrayOn, on)
	}
	if before.IDMismatch > 0 {
		return res, fmt.Errorf("idMismatchCount is %d: that many output documents have an _id other than the _id of the document "+
			"they match in %s on %s: $merge can't change an _id and would fail part way through; "+
			"leave _id out of the output ({\"$project\": {\"_id\": 0, ...}})",
			before.IDMismatch, target.Into, on)
	}
	if before.TypeChanged > 0 {
		return res, fmt.Errorf("typeChangedCount is %d: the merge would change the BSON type of existing fields in %s, "+
			"or drop nested ones (type missing); neither is allowed in place: write the new type or shape to a new field instead",
			before.TypeChanged, target.Into)
	}
	if before.DuplicateOn > 0 {
		return res, fmt.Errorf("duplicateOnCount is %d: that many output documents share their on fields (%s) with another, "+
			"so $merge would write the same document in %s more than once and the last write would win; "+
			"make the pipeline output one document per on value", before.DuplicateOn, on, target.Into)
	}
	if !apply {
		return res, nil
	}

	// $merge writes outside any transaction, unfenced.
	if err := r.checkLease(); err != nil {
		return res, err
	}
	if err := r.aggregate(s.Collection, toPipeline(s.Pipeline), log, nil); err != nil {
		return res, fmt.Errorf("merge into %s: %w", target.Into, err)
	}
	log.Info("merge done", "into", target.Into)
	after, err := r.compareMerge(s.Collection, pre, target, log)
	if err != nil {
		return res, fmt.Errorf("compare with %s after the merge: %w", target.Into, err)
	}
	res.DiffCountAfter = &after.Diff
	log.Info("merge comparison after the merge", "into", target.Into, "diffCountAfter", after.Diff)
	if after.Diff > 0 {
		return res, fmt.Errorf("diffCountAfter is %d: documents in %s still differ from the output after the merge, "+
			"so it didn't settle and running it again would change them again", after.Diff, target.Into)
	}
	return res, nil
}

// mergeTarget is what a step's $merge stage writes to.
type mergeTarget struct {
	Into string
	// On are the fields matching output documents with target documents.
	On []string
}

// splitMerge returns a merge step's pipeline without its $merge stage, and
// that stage's target. The schema requires exactly one $merge, with into as a
// collection name and on, if set, as a field name or an array of distinct
// ones; lint requires it to be the last stage, which plan and apply check
// here.
func splitMerge(s migration.Step) ([]bson.Raw, mergeTarget, error) {
	var t mergeTarget
	n := len(s.Pipeline)
	stage, ok := s.Pipeline[n-1].Lookup("$merge").DocumentOK()
	if !ok {
		return nil, t, errors.New("the $merge stage must be the last stage of the pipeline")
	}
	t.Into, _ = stage.Lookup("into").StringValueOK()
	on, err := stage.LookupErr("on")
	switch {
	case err != nil:
		t.On = []string{"_id"}
	case on.Type == bson.TypeString:
		t.On = []string{on.StringValue()}
	default:
		vals, _ := on.Array().Values()
		for _, v := range vals {
			t.On = append(t.On, v.StringValue())
		}
	}
	return s.Pipeline[:n-1], t, nil
}

// toPipeline turns stages into a pipeline the driver accepts.
func toPipeline(stages []bson.Raw) []any {
	out := make([]any, len(stages))
	for i, st := range stages {
		out[i] = st
	}
	return out
}

// warnCollation logs one warning per merge step when source, the collection
// the step reads, has a non-simple default collation. $merge runs the
// pipeline under it, since an aggregate has a single collation and migrune
// sets none, while the comparison runs the same pipeline under the simple
// collation to compare strings byte by byte. Any expression comparing
// strings, in any stage, can then give another output, so the counts may not
// be what apply writes. The unique index $merge matches the on fields
// through, ix, has the same collation, and the warning then also lists what
// the comparison can't see through it.
func warnCollation(log *slog.Logger, source, collation string, t mergeTarget, ix mergeIndex) {
	if collation == "" {
		return
	}
	msg := "the collection the pipeline reads has a non-simple default collation: $merge runs the pipeline under it, " +
		"while plan's preview and the before and after counts run it under the simple collation, " +
		"so they may differ from what apply writes"
	attrs := []any{"source", source, "collation", collation}
	if ix.Collation != "" {
		msg += "; the unique index on the on fields has it too, and the comparison doesn't fully check it: " +
			"duplicateOnCount misses output documents whose on values are equal only under it; " +
			"documents that match a target only through it get no per-field diff or type check; " +
			"from MongoDB 8.3, $merge rewrites the target's stored on values to the output's; " +
			"collationOnlyMatchCount leaves out on values that are embedded documents"
		attrs = append(attrs, "into", t.Into, "on", strings.Join(t.On, ","), "index", ix.Name)
	}
	log.Warn(msg, attrs...)
}
